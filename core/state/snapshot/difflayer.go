// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package snapshot

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	bloomfilter "github.com/holiman/bloomfilter/v2"
)

var (
	// aggregatorMemoryLimit is the maximum size of the bottom-most diff layer
	// that aggregates the writes from above until it's flushed into the disk
	// layer.
	//
	// Note, bumping this up might drastically increase the size of the bloom
	// filters that's stored in every diff layer. Don't do that without fully
	// understanding all the implications.
	// aggregatorMemoryLimitは、ディスクレイヤーにフラッシュされるまで、
	// 上からの書き込みを集約する最下部のdiffレイヤーの最大サイズです。
	//
	// これを増やすと、すべてのdiffレイヤーに格納されるブルームフィルターのサイズが大幅に増加する可能性があることに注意してください。
	// すべての影響を完全に理解せずにそれを行わないでください。
	aggregatorMemoryLimit = uint64(4 * 1024 * 1024)

	// aggregatorItemLimit is an approximate number of items that will end up
	// in the agregator layer before it's flushed out to disk. A plain account
	// weighs around 14B (+hash), a storage slot 32B (+hash), a deleted slot
	// 0B (+hash). Slots are mostly set/unset in lockstep, so that average at
	// 16B (+hash). All in all, the average entry seems to be 15+32=47B. Use a
	// smaller number to be on the safe side.
	// aggregatorItemLimitは、ディスクにフラッシュされる前にアグリゲーターレイヤーに配置されるアイテムのおおよその数です。
	// プレーンアカウントの重みは約14B（+ハッシュ）、ストレージスロット32B（+ハッシュ）、
	// 削除されたスロット0B（+ハッシュ）です。
	// スロットはほとんどロックステップで設定/設定解除されるため、平均で16B（+ハッシュ）になります。
	// 全体として、平均エントリは15 + 32=47Bのようです。安全のため、小さい数字を使用してください。
	aggregatorItemLimit = aggregatorMemoryLimit / 42

	// bloomTargetError is the target false positive rate when the aggregator
	// layer is at its fullest. The actual value will probably move around up
	// and down from this number, it's mostly a ballpark figure.
	//
	// Note, dropping this down might drastically increase the size of the bloom
	// filters that's stored in every diff layer. Don't do that without fully
	// understanding all the implications.
	// BloomTargetErrorは、アグリゲーターレイヤーが最大になっているときのターゲットの偽陽性率です。
	// 実際の値はおそらくこの数値から上下に移動します。これはほとんどが球場の数値です。
	//
	// これをドロップダウンすると、すべてのdiffレイヤーに格納されるブルームフィルターのサイズが
	// 大幅に増加する可能性があることに注意してください。
	// すべての影響を完全に理解せずにそれを行わないでください。
	bloomTargetError = 0.02

	// bloomSize is the ideal bloom filter size given the maximum number of items
	// it's expected to hold and the target false positive error rate.
	// BloomSizeは、保持すると予想されるアイテムの最大数とターゲットの誤検知エラー率を
	// 考慮した場合の理想的なブルームフィルターサイズです。
	bloomSize = math.Ceil(float64(aggregatorItemLimit) * math.Log(bloomTargetError) / math.Log(1/math.Pow(2, math.Log(2))))

	// bloomFuncs is the ideal number of bits a single entry should set in the
	// bloom filter to keep its size to a minimum (given it's size and maximum
	// entry count).
	// BloomFuncsは、サイズを最小に保つために1つのエントリがブルームフィルターに設定する
	// 必要のある理想的なビット数です（サイズと最大エントリ数が与えられた場合）。
	bloomFuncs = math.Round((bloomSize / float64(aggregatorItemLimit)) * math.Log(2))

	// the bloom offsets are runtime constants which determines which part of the
	// the account/storage hash the hasher functions looks at, to determine the
	// bloom key for an account/slot. This is randomized at init(), so that the
	// global population of nodes do not all display the exact same behaviour with
	// regards to bloom content
	// ブルームオフセットは実行時定数であり、アカウント/スロットのブルームキーを決定するために、
	// ハッシャー関数がアカウント/ストレージハッシュのどの部分を参照するかを決定します。
	// これはinit（）でランダム化されるため、ノードのグローバルな母集団はすべて、
	// ブルームコンテンツに関してまったく同じ動作を表示するわけではありません。
	bloomDestructHasherOffset = 0
	bloomAccountHasherOffset  = 0
	bloomStorageHasherOffset  = 0
)

func init() {
	// Init the bloom offsets in the range [0:24] (requires 8 bytes)
	// [0:24]の範囲でブルームオフセットを初期化します（8バイトが必要です）
	bloomDestructHasherOffset = rand.Intn(25)
	bloomAccountHasherOffset = rand.Intn(25)
	bloomStorageHasherOffset = rand.Intn(25)

	// The destruct and account blooms must be different, as the storage slots
	// will check for destruction too for every bloom miss. It should not collide
	// with modified accounts.
	// ブルームが失敗するたびにストレージスロットも破壊をチェックするため、
	// 破棄とアカウントのブルームは異なっている必要があります。
	// 変更されたアカウントと衝突しないようにする必要があります。
	for bloomAccountHasherOffset == bloomDestructHasherOffset {
		bloomAccountHasherOffset = rand.Intn(25)
	}
}

// diffLayer represents a collection of modifications made to a state snapshot
// after running a block on top. It contains one sorted list for the account trie
// and one-one list for each storage tries.
//
// The goal of a diff layer is to act as a journal, tracking recent modifications
// made to the state, that have not yet graduated into a semi-immutable state.

// diffLayerは、ブロックを上で実行した後に状態スナップショットに加えられた変更のコレクションを表します。
// これには、アカウント試行用に1つのソートされたリストと、ストレージ試行ごとに1つのリストが含まれています。
//
// diffレイヤーの目標は、ジャーナルとして機能し、状態に対して行われた最近の変更を追跡することです。
// これらの変更は、まだ半不変の状態に移行していません。
type diffLayer struct {
	origin *diskLayer // ブルームミスで直接使用するベースディスクレイヤー    // Base disk layer to directly use on bloom misses
	parent snapshot   // これによって変更された親スナップショット、決してnil // Parent snapshot modified by this one, never nil
	memory uint64     // 使用するメモリの量に関するおおよその推測           // Approximate guess as to how much memory we use

	root  common.Hash // このスナップショット差分が属するルートハッシュ     // Root hash to which this snapshot diff belongs to
	stale uint32      // レイヤーが古くなったことを通知します（状態が進行しました） // Signals that the layer became stale (state progressed)

	// destructSet is a very special helper marker. If an account is marked as
	// deleted, then it's recorded in this set. However it's allowed that an account
	// is included here but still available in other sets(e.g. storageData). The
	// reason is the diff layer includes all the changes in a *block*. It can
	// happen that in the tx_1, account A is self-destructed while in the tx_2
	// it's recreated. But we still need this marker to indicate the "old" A is
	// deleted, all data in other set belongs to the "new" A.
	// destructSetは非常に特別なヘルパーマーカーです。
	// アカウントが削除済みとしてマークされている場合、
	// そのアカウントはこのセットに記録されます。
	// ただし、アカウントはここに含まれていても、
	// 他のセット（storageDataなど）で引き続き使用できます。
	// その理由は、diffレイヤーにすべての変更が*ブロック*に含まれているためです。
	//  tx_1では、アカウントAが自己破壊され、tx_2では再作成される可能性があります。
	// ただし、「古い」Aが削除されたことを示すためにこのマーカーが必要です。
	// 他のセットのすべてのデータは、「新しい」Aに属します。
	destructSet map[common.Hash]struct{}               // 削除された（および潜在的に）再作成されたアカウントのキー付きマーカー // Keyed markers for deleted (and potentially) recreated accounts
	accountList []common.Hash                          // 反復のアカウントのリスト。存在する場合はソートされ、存在しない場合はnilです。 // List of account for iteration. If it exists, it's sorted, otherwise it's nil
	accountData map[common.Hash][]byte                 // 直接取得用のキー付きアカウント（nilは削除されたことを意味します） // Keyed accounts for direct retrieval (nil means deleted)
	storageList map[common.Hash][]common.Hash          // アカウントごとに1つずつ、反復取得用のストレージスロットのリスト。 nil以外の場合、既存のリストは並べ替えられます // List of storage slots for iterated retrievals, one per account. Any existing lists are sorted if non-nil
	storageData map[common.Hash]map[common.Hash][]byte // 直接取得するためのキー付きストレージスロット。アカウントごとに1つ（nilは削除されたことを意味します） // Keyed storage slots for direct retrieval. one per account (nil means deleted)

	diffed *bloomfilter.Filter //ディスクレイヤーまでのすべての差分アイテムを追跡するブルームフィルター // Bloom filter tracking all the diffed items up to the disk layer

	lock sync.RWMutex
}

// destructBloomHasher is a wrapper around a common.Hash to satisfy the interface
// API requirements of the bloom library used. It's used to convert a destruct
// event into a 64 bit mini hash.
// destructBloomHasherは、使用されるブルームライブラリのインターフェースAPI要件を満たすためのcommon.Hashのラッパーです。
// これは、破棄イベントを64ビットのミニハッシュに変換するために使用されます。
type destructBloomHasher common.Hash

func (h destructBloomHasher) Write(p []byte) (n int, err error) { panic("not implemented") } // 実装されていません
func (h destructBloomHasher) Sum(b []byte) []byte               { panic("not implemented") } // 実装されていません
func (h destructBloomHasher) Reset()                            { panic("not implemented") } // 実装されていません
func (h destructBloomHasher) BlockSize() int                    { panic("not implemented") } // 実装されていません
func (h destructBloomHasher) Size() int                         { return 8 }
func (h destructBloomHasher) Sum64() uint64 {
	return binary.BigEndian.Uint64(h[bloomDestructHasherOffset : bloomDestructHasherOffset+8])
}

// accountBloomHasher is a wrapper around a common.Hash to satisfy the interface
// API requirements of the bloom library used. It's used to convert an account
// hash into a 64 bit mini hash.
// accountBloomHasherは、使用されるブルームライブラリのインターフェースAPI要件を満たすためのcommon.Hashのラッパーです。
// アカウントハッシュを64ビットミニハッシュに変換するために使用されます。
type accountBloomHasher common.Hash

func (h accountBloomHasher) Write(p []byte) (n int, err error) { panic("not implemented") } // 実装されていません
func (h accountBloomHasher) Sum(b []byte) []byte               { panic("not implemented") } // 実装されていません
func (h accountBloomHasher) Reset()                            { panic("not implemented") } // 実装されていません
func (h accountBloomHasher) BlockSize() int                    { panic("not implemented") } // 実装されていません
func (h accountBloomHasher) Size() int                         { return 8 }
func (h accountBloomHasher) Sum64() uint64 {
	return binary.BigEndian.Uint64(h[bloomAccountHasherOffset : bloomAccountHasherOffset+8])
}

// storageBloomHasher is a wrapper around a [2]common.Hash to satisfy the interface
// API requirements of the bloom library used. It's used to convert an account
// hash into a 64 bit mini hash.
// storageBloomHasherは、使用されるブルームライブラリのインターフェースAPI要件を満たすための[2]common.Hashのラッパーです。
// アカウントハッシュを64ビットミニハッシュに変換するために使用されます。
type storageBloomHasher [2]common.Hash

func (h storageBloomHasher) Write(p []byte) (n int, err error) { panic("not implemented") } // 実装されていません
func (h storageBloomHasher) Sum(b []byte) []byte               { panic("not implemented") } // 実装されていません
func (h storageBloomHasher) Reset()                            { panic("not implemented") } // 実装されていません
func (h storageBloomHasher) BlockSize() int                    { panic("not implemented") } // 実装されていません
func (h storageBloomHasher) Size() int                         { return 8 }
func (h storageBloomHasher) Sum64() uint64 {
	return binary.BigEndian.Uint64(h[0][bloomStorageHasherOffset:bloomStorageHasherOffset+8]) ^
		binary.BigEndian.Uint64(h[1][bloomStorageHasherOffset:bloomStorageHasherOffset+8])
}

// newDiffLayer creates a new diff on top of an existing snapshot, whether that's a low
// level persistent database or a hierarchical diff already.
// newDiffLayerは、既存のスナップショットの上に新しい差分を作成します。
// これが低レベルの永続データベースであるか、すでに階層型の差分であるかは関係ありません。
func newDiffLayer(parent snapshot, root common.Hash, destructs map[common.Hash]struct{}, accounts map[common.Hash][]byte, storage map[common.Hash]map[common.Hash][]byte) *diffLayer {
	// Create the new layer with some pre-allocated data segments
	// 事前に割り当てられたデータセグメントを使用して新しいレイヤーを作成します
	dl := &diffLayer{
		parent:      parent,
		root:        root,
		destructSet: destructs,
		accountData: accounts,
		storageData: storage,
		storageList: make(map[common.Hash][]common.Hash),
	}
	switch parent := parent.(type) {
	case *diskLayer:
		dl.rebloom(parent)
	case *diffLayer:
		dl.rebloom(parent.origin)
	default:
		panic("unknown parent type") // 不明な親タイプ
	}
	// Sanity check that accounts or storage slots are never nil
	// アカウントまたはストレージスロットがゼロにならないことを健全性チェック
	for accountHash, blob := range accounts {
		if blob == nil {
			panic(fmt.Sprintf("account %#x nil", accountHash))
		}
		// Determine memory size and track the dirty writes
		// メモリサイズを決定し、ダーティ書き込みを追跡します
		dl.memory += uint64(common.HashLength + len(blob))
		snapshotDirtyAccountWriteMeter.Mark(int64(len(blob)))
	}
	for accountHash, slots := range storage {
		if slots == nil {
			panic(fmt.Sprintf("storage %#x nil", accountHash))
		}
		// Determine memory size and track the dirty writes
		// メモリサイズを決定し、ダーティ書き込みを追跡します
		for _, data := range slots {
			dl.memory += uint64(common.HashLength + len(data))
			snapshotDirtyStorageWriteMeter.Mark(int64(len(data)))
		}
	}
	dl.memory += uint64(len(destructs) * common.HashLength)
	return dl
}

// rebloom discards the layer's current bloom and rebuilds it from scratch based
// on the parent's and the local diffs.
// rebloomはレイヤーの現在のブルームを破棄し、
// 親とローカルの差分に基づいて最初から再構築します。
func (dl *diffLayer) rebloom(origin *diskLayer) {
	dl.lock.Lock()
	defer dl.lock.Unlock()

	defer func(start time.Time) {
		snapshotBloomIndexTimer.Update(time.Since(start))
	}(time.Now())

	// Inject the new origin that triggered the rebloom
	// リブルームをトリガーした新しい原点を注入します
	dl.origin = origin

	// Retrieve the parent bloom or create a fresh empty one
	// 親ブルームを取得するか、新しい空のブルームを作成します
	if parent, ok := dl.parent.(*diffLayer); ok {
		parent.lock.RLock()
		dl.diffed, _ = parent.diffed.Copy()
		parent.lock.RUnlock()
	} else {
		dl.diffed, _ = bloomfilter.New(uint64(bloomSize), uint64(bloomFuncs))
	}
	// Iterate over all the accounts and storage slots and index them
	// すべてのアカウントとストレージスロットを繰り返し処理し、インデックスを作成します
	for hash := range dl.destructSet {
		dl.diffed.Add(destructBloomHasher(hash))
	}
	for hash := range dl.accountData {
		dl.diffed.Add(accountBloomHasher(hash))
	}
	for accountHash, slots := range dl.storageData {
		for storageHash := range slots {
			dl.diffed.Add(storageBloomHasher{accountHash, storageHash})
		}
	}
	// Calculate the current false positive rate and update the error rate meter.
	// This is a bit cheating because subsequent layers will overwrite it, but it
	// should be fine, we're only interested in ballpark figures.
	// 現在の偽陽性率を計算し、エラー率メーターを更新します。
	// 後続のレイヤーが上書きするため、これは少し不正行為ですが、問題はないはずです。
	// 私たちは球場の数字にのみ関心があります。
	k := float64(dl.diffed.K())
	n := float64(dl.diffed.N())
	m := float64(dl.diffed.M())
	snapshotBloomErrorGauge.Update(math.Pow(1.0-math.Exp((-k)*(n+0.5)/(m-1)), k))
}

// Root returns the root hash for which this snapshot was made.
// Rootは、このスナップショットが作成されたルートハッシュを返します。
func (dl *diffLayer) Root() common.Hash {
	return dl.root
}

// Parent returns the subsequent layer of a diff layer.
// 親はdiffレイヤーの後続のレイヤーを返します。
func (dl *diffLayer) Parent() snapshot {
	return dl.parent
}

// Stale return whether this layer has become stale (was flattened across) or if
// it's still live.
// Staleは、このレイヤーが古くなった（フラット化された）か、まだライブであるかを返します。
func (dl *diffLayer) Stale() bool {
	return atomic.LoadUint32(&dl.stale) != 0
}

// Account directly retrieves the account associated with a particular hash in
// the snapshot slim data format.
// アカウントは、スナップショットスリムデータ形式で特定のハッシュに関連付けられたアカウントを直接取得します。
func (dl *diffLayer) Account(hash common.Hash) (*Account, error) {
	data, err := dl.AccountRLP(hash)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 { // nilと[]byte{}の両方にすることができます // can be both nil and []byte{}
		return nil, nil
	}
	account := new(Account)
	if err := rlp.DecodeBytes(data, account); err != nil {
		panic(err)
	}
	return account, nil
}

// AccountRLP directly retrieves the account RLP associated with a particular
// hash in the snapshot slim data format.
//
// Note the returned account is not a copy, please don't modify it.
// AccountRLPは、スナップショットスリムデータ形式の特定のハッシュに関連付けられたアカウントRLPを直接取得します。
//
// 返されたアカウントはコピーではないことに注意してください。変更しないでください。
func (dl *diffLayer) AccountRLP(hash common.Hash) ([]byte, error) {
	// Check the bloom filter first whether there's even a point in reaching into
	// all the maps in all the layers below
	// 最初にブルームフィルターをチェックして、下のすべてのレイヤーのすべてのマップに到達するポイントがあるかどうかを確認します
	dl.lock.RLock()
	hit := dl.diffed.Contains(accountBloomHasher(hash))
	if !hit {
		hit = dl.diffed.Contains(destructBloomHasher(hash))
	}
	var origin *diskLayer
	if !hit {
		origin = dl.origin // ロックを保持しながら原点を抽出します // extract origin while holding the lock
	}
	dl.lock.RUnlock()

	// If the bloom filter misses, don't even bother with traversing the memory
	// diff layers, reach straight into the bottom persistent disk layer
	// ブルームフィルターが失敗した場合は、メモリ差分レイヤーをトラバースすることさえ気にせず、
	// 最下部の永続ディスクレイヤーに直接到達します
	if origin != nil {
		snapshotBloomAccountMissMeter.Mark(1)
		return origin.AccountRLP(hash)
	}
	// The bloom filter hit, start poking in the internal maps
	// ブルームフィルターがヒットし、内部マップを突っ込み始めます
	return dl.accountRLP(hash, 0)
}

// accountRLP is an internal version of AccountRLP that skips the bloom filter
// checks and uses the internal maps to try and retrieve the data. It's meant
// to be used if a higher layer's bloom filter hit already.
// accountRLPはAccountRLPの内部バージョンであり、ブルームフィルターのチェックをスキップし、
// 内部マップを使用してデータを取得しようとします。
// これは、上位層のブルームフィルターがすでにヒットしている場合に使用することを目的としています。
func (dl *diffLayer) accountRLP(hash common.Hash, depth int) ([]byte, error) {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	// If the layer was flattened into, consider it invalid (any live reference to
	// the original should be marked as unusable).
	// レイヤーがフラット化されている場合は、無効と見なします
	// （元のレイヤーへのライブ参照はすべて使用不可としてマークする必要があります）。
	if dl.Stale() {
		return nil, ErrSnapshotStale
	}
	// If the account is known locally, return it
	// アカウントがローカルで知られている場合は、それを返します
	if data, ok := dl.accountData[hash]; ok {
		snapshotDirtyAccountHitMeter.Mark(1)
		snapshotDirtyAccountHitDepthHist.Update(int64(depth))
		snapshotDirtyAccountReadMeter.Mark(int64(len(data)))
		snapshotBloomAccountTrueHitMeter.Mark(1)
		return data, nil
	}
	// If the account is known locally, but deleted, return it
	// アカウントはローカルで認識されているが削除されている場合は、アカウントを返します
	if _, ok := dl.destructSet[hash]; ok {
		snapshotDirtyAccountHitMeter.Mark(1)
		snapshotDirtyAccountHitDepthHist.Update(int64(depth))
		snapshotDirtyAccountInexMeter.Mark(1)
		snapshotBloomAccountTrueHitMeter.Mark(1)
		return nil, nil
	}
	// Account unknown to this diff, resolve from parent
	// この差分に不明なアカウント、親から解決
	if diff, ok := dl.parent.(*diffLayer); ok {
		return diff.accountRLP(hash, depth+1)
	}
	// Failed to resolve through diff layers, mark a bloom error and use the disk
	// 差分レイヤーを介した解決に失敗し、ブルームエラーをマークして、ディスクを使用しました
	snapshotBloomAccountFalseHitMeter.Mark(1)
	return dl.parent.AccountRLP(hash)
}

// Storage directly retrieves the storage data associated with a particular hash,
// within a particular account. If the slot is unknown to this diff, it's parent
// is consulted.
//
// Note the returned slot is not a copy, please don't modify it.
// ストレージは、特定のアカウント内の特定のハッシュに関連付けられたストレージデータを直接取得します。
// スロットがこのdiffに不明な場合は、その親が参照されます。
//
// 返されたスロットはコピーではないことに注意してください。変更しないでください。
func (dl *diffLayer) Storage(accountHash, storageHash common.Hash) ([]byte, error) {
	// Check the bloom filter first whether there's even a point in reaching into
	// all the maps in all the layers below
	// 最初にブルームフィルターをチェックして、
	// 下のすべてのレイヤーのすべてのマップに到達するポイントがあるかどうかを確認します
	dl.lock.RLock()
	hit := dl.diffed.Contains(storageBloomHasher{accountHash, storageHash})
	if !hit {
		hit = dl.diffed.Contains(destructBloomHasher(accountHash))
	}
	var origin *diskLayer
	if !hit {
		origin = dl.origin // ロックを保持しながら原点を抽出します // extract origin while holding the lock
	}
	dl.lock.RUnlock()

	// If the bloom filter misses, don't even bother with traversing the memory
	// diff layers, reach straight into the bottom persistent disk layer
	// ブルームフィルターが失敗した場合は、メモリ差分レイヤーをトラバースすることさえ気にせず、最下部の永続ディスクレイヤーに直接到達します
	if origin != nil {
		snapshotBloomStorageMissMeter.Mark(1)
		return origin.Storage(accountHash, storageHash)
	}
	// The bloom filter hit, start poking in the internal maps
	// ブルームフィルターがヒットし、内部マップを突っ込み始めます
	return dl.storage(accountHash, storageHash, 0)
}

// storage is an internal version of Storage that skips the bloom filter checks
// and uses the internal maps to try and retrieve the data. It's meant  to be
// used if a higher layer's bloom filter hit already.
// ストレージはストレージの内部バージョンであり、ブルームフィルターのチェックをスキップし、
// 内部マップを使用してデータを取得しようとします。これは、
// 上位層のブルームフィルターがすでにヒットしている場合に使用することを目的としています。
func (dl *diffLayer) storage(accountHash, storageHash common.Hash, depth int) ([]byte, error) {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	// If the layer was flattened into, consider it invalid (any live reference to
	// the original should be marked as unusable).
	// レイヤーがフラット化されている場合は、無効と見なします
	// （元のレイヤーへのライブ参照はすべて使用不可としてマークする必要があります）。
	if dl.Stale() {
		return nil, ErrSnapshotStale
	}
	// If the account is known locally, try to resolve the slot locally
	// アカウントがローカルでわかっている場合は、スロットをローカルで解決してみてください
	if storage, ok := dl.storageData[accountHash]; ok {
		if data, ok := storage[storageHash]; ok {
			snapshotDirtyStorageHitMeter.Mark(1)
			snapshotDirtyStorageHitDepthHist.Update(int64(depth))
			if n := len(data); n > 0 {
				snapshotDirtyStorageReadMeter.Mark(int64(n))
			} else {
				snapshotDirtyStorageInexMeter.Mark(1)
			}
			snapshotBloomStorageTrueHitMeter.Mark(1)
			return data, nil
		}
	}
	// If the account is known locally, but deleted, return an empty slot
	// アカウントはローカルで認識されているが削除されている場合は、空のスロットを返します
	if _, ok := dl.destructSet[accountHash]; ok {
		snapshotDirtyStorageHitMeter.Mark(1)
		snapshotDirtyStorageHitDepthHist.Update(int64(depth))
		snapshotDirtyStorageInexMeter.Mark(1)
		snapshotBloomStorageTrueHitMeter.Mark(1)
		return nil, nil
	}
	// Storage slot unknown to this diff, resolve from parent
	// この差分に不明なストレージスロット、親から解決
	if diff, ok := dl.parent.(*diffLayer); ok {
		return diff.storage(accountHash, storageHash, depth+1)
	}
	// Failed to resolve through diff layers, mark a bloom error and use the disk
	// 差分レイヤーを介した解決に失敗し、ブルームエラーをマークして、ディスクを使用しました
	snapshotBloomStorageFalseHitMeter.Mark(1)
	return dl.parent.Storage(accountHash, storageHash)
}

// Update creates a new layer on top of the existing snapshot diff tree with
// the specified data items.
// Updateは、指定されたデータ項目を使用して、
// 既存のスナップショット差分ツリーの上に新しいレイヤーを作成します。
func (dl *diffLayer) Update(blockRoot common.Hash, destructs map[common.Hash]struct{}, accounts map[common.Hash][]byte, storage map[common.Hash]map[common.Hash][]byte) *diffLayer {
	return newDiffLayer(dl, blockRoot, destructs, accounts, storage)
}

// flatten pushes all data from this point downwards, flattening everything into
// a single diff at the bottom. Since usually the lowermost diff is the largest,
// the flattening builds up from there in reverse.
// flattenは、このポイントからすべてのデータを下にプッシュし、
// すべてをフラット化して下部の単一の差分にします。
// 通常、最も低い差分が最大であるため、平坦化はそこから逆に蓄積されます。
func (dl *diffLayer) flatten() snapshot {
	// If the parent is not diff, we're the first in line, return unmodified
	// 親がdiffでない場合、私たちは最初の行であり、変更されていない状態を返します
	parent, ok := dl.parent.(*diffLayer)
	if !ok {
		return dl
	}
	// Parent is a diff, flatten it first (note, apart from weird corned cases,
	// flatten will realistically only ever merge 1 layer, so there's no need to
	// be smarter about grouping flattens together).
	// 親は差分であり、最初にフラット化します
	// （ただし、奇妙なコンビーフの場合を除いて、フラット化は実際には1つのレイヤーのみをマージするため、
	// フラット化をグループ化することについて賢くする必要はありません）。
	parent = parent.flatten().(*diffLayer)

	parent.lock.Lock()
	defer parent.lock.Unlock()

	// Before actually writing all our data to the parent, first ensure that the
	// parent hasn't been 'corrupted' by someone else already flattening into it
	// 実際にすべてのデータを親に書き込む前に、
	// まず、親がすでにフラット化されている他の誰かによって「破損」していないことを確認します
	if atomic.SwapUint32(&parent.stale, 1) != 0 {
		panic("parent diff layer is stale") // 2人の子供から同じ親にフラット化されました、ブー// we've flattened into the same parent from two children, boo
	}
	// Overwrite all the updated accounts blindly, merge the sorted list
	// 更新されたすべてのアカウントを盲目的に上書きし、並べ替えられたリストをマージします
	for hash := range dl.destructSet {
		parent.destructSet[hash] = struct{}{}
		delete(parent.accountData, hash)
		delete(parent.storageData, hash)
	}
	for hash, data := range dl.accountData {
		parent.accountData[hash] = data
	}
	// Overwrite all the updated storage slots (individually)
	// 更新されたすべてのストレージスロットを（個別に）上書きします
	for accountHash, storage := range dl.storageData {
		// If storage didn't exist (or was deleted) in the parent, overwrite blindly
		// ストレージが親に存在しなかった（または削除された）場合は、盲目的に上書きします
		if _, ok := parent.storageData[accountHash]; !ok {
			parent.storageData[accountHash] = storage
			continue
		}
		// Storage exists in both parent and child, merge the slots
		// ストレージは親と子の両方に存在し、スロットをマージします
		comboData := parent.storageData[accountHash]
		for storageHash, data := range storage {
			comboData[storageHash] = data
		}
		parent.storageData[accountHash] = comboData
	}
	// Return the combo parent
	// コンボの親を返します
	return &diffLayer{
		parent:      parent.parent,
		origin:      parent.origin,
		root:        dl.root,
		destructSet: parent.destructSet,
		accountData: parent.accountData,
		storageData: parent.storageData,
		storageList: make(map[common.Hash][]common.Hash),
		diffed:      dl.diffed,
		memory:      parent.memory + dl.memory,
	}
}

// AccountList returns a sorted list of all accounts in this diffLayer, including
// the deleted ones.
//
// Note, the returned slice is not a copy, so do not modify it.
// AccountListは、削除されたアカウントを含む、このdiffLayer内のすべてのアカウントのソートされたリストを返します。
//
// 返されるスライスはコピーではないため、変更しないでください。
func (dl *diffLayer) AccountList() []common.Hash {
	// If an old list already exists, return it
	// 古いリストがすでに存在する場合は、それを返します
	dl.lock.RLock()
	list := dl.accountList
	dl.lock.RUnlock()

	if list != nil {
		return list
	}
	// No old sorted account list exists, generate a new one
	// 古いソート済みアカウントリストは存在しません。新しいアカウントリストを生成します
	dl.lock.Lock()
	defer dl.lock.Unlock()

	dl.accountList = make([]common.Hash, 0, len(dl.destructSet)+len(dl.accountData))
	for hash := range dl.accountData {
		dl.accountList = append(dl.accountList, hash)
	}
	for hash := range dl.destructSet {
		if _, ok := dl.accountData[hash]; !ok {
			dl.accountList = append(dl.accountList, hash)
		}
	}
	sort.Sort(hashes(dl.accountList))
	dl.memory += uint64(len(dl.accountList) * common.HashLength)
	return dl.accountList
}

// StorageList returns a sorted list of all storage slot hashes in this diffLayer
// for the given account. If the whole storage is destructed in this layer, then
// an additional flag *destructed = true* will be returned, otherwise the flag is
// false. Besides, the returned list will include the hash of deleted storage slot.
// Note a special case is an account is deleted in a prior tx but is recreated in
// the following tx with some storage slots set. In this case the returned list is
// not empty but the flag is true.
//
// Note, the returned slice is not a copy, so do not modify it.

// StorageListは、指定されたアカウントのこのdiffLayer内のすべてのストレージスロットハッシュのソートされたリストを返します。
// ストレージ全体がこのレイヤーで破棄された場合、追加のフラグ* destroyd = true *が返されます。
// それ以外の場合、フラグはfalseになります。また、返されるリストには、削除されたストレージスロットのハッシュが含まれます。
// 特別な場合は、アカウントが前のtxで削除されたが、いくつかのストレージスロットが設定された次のtxで再作成されることに注意してください。
// この場合、返されるリストは空ではありませんが、フラグはtrueです。
//
// 返されるスライスはコピーではないため、変更しないでください。
func (dl *diffLayer) StorageList(accountHash common.Hash) ([]common.Hash, bool) {
	dl.lock.RLock()
	_, destructed := dl.destructSet[accountHash]
	if _, ok := dl.storageData[accountHash]; !ok {
		// Account not tracked by this layer
		// このレイヤーで追跡されていないアカウント
		dl.lock.RUnlock()
		return nil, destructed
	}
	// If an old list already exists, return it
	// 古いリストがすでに存在する場合は、それを返します
	if list, exist := dl.storageList[accountHash]; exist {
		dl.lock.RUnlock()
		return list, destructed // キャッシュされたリストをnilにすることはできません // the cached list can't be nil
	}
	dl.lock.RUnlock()

	// No old sorted account list exists, generate a new one
	// 古いソート済みアカウントリストは存在しません。新しいアカウントリストを生成します
	dl.lock.Lock()
	defer dl.lock.Unlock()

	storageMap := dl.storageData[accountHash]
	storageList := make([]common.Hash, 0, len(storageMap))
	for k := range storageMap {
		storageList = append(storageList, k)
	}
	sort.Sort(hashes(storageList))
	dl.storageList[accountHash] = storageList
	dl.memory += uint64(len(dl.storageList)*common.HashLength + common.HashLength)
	return storageList, destructed
}
