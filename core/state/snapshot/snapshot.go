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

// Package snapshot implements a journalled, dynamic state dump.
package snapshot

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

var (
	snapshotCleanAccountHitMeter   = metrics.NewRegisteredMeter("state/snapshot/clean/account/hit", nil)
	snapshotCleanAccountMissMeter  = metrics.NewRegisteredMeter("state/snapshot/clean/account/miss", nil)
	snapshotCleanAccountInexMeter  = metrics.NewRegisteredMeter("state/snapshot/clean/account/inex", nil)
	snapshotCleanAccountReadMeter  = metrics.NewRegisteredMeter("state/snapshot/clean/account/read", nil)
	snapshotCleanAccountWriteMeter = metrics.NewRegisteredMeter("state/snapshot/clean/account/write", nil)

	snapshotCleanStorageHitMeter   = metrics.NewRegisteredMeter("state/snapshot/clean/storage/hit", nil)
	snapshotCleanStorageMissMeter  = metrics.NewRegisteredMeter("state/snapshot/clean/storage/miss", nil)
	snapshotCleanStorageInexMeter  = metrics.NewRegisteredMeter("state/snapshot/clean/storage/inex", nil)
	snapshotCleanStorageReadMeter  = metrics.NewRegisteredMeter("state/snapshot/clean/storage/read", nil)
	snapshotCleanStorageWriteMeter = metrics.NewRegisteredMeter("state/snapshot/clean/storage/write", nil)

	snapshotDirtyAccountHitMeter   = metrics.NewRegisteredMeter("state/snapshot/dirty/account/hit", nil)
	snapshotDirtyAccountMissMeter  = metrics.NewRegisteredMeter("state/snapshot/dirty/account/miss", nil)
	snapshotDirtyAccountInexMeter  = metrics.NewRegisteredMeter("state/snapshot/dirty/account/inex", nil)
	snapshotDirtyAccountReadMeter  = metrics.NewRegisteredMeter("state/snapshot/dirty/account/read", nil)
	snapshotDirtyAccountWriteMeter = metrics.NewRegisteredMeter("state/snapshot/dirty/account/write", nil)

	snapshotDirtyStorageHitMeter   = metrics.NewRegisteredMeter("state/snapshot/dirty/storage/hit", nil)
	snapshotDirtyStorageMissMeter  = metrics.NewRegisteredMeter("state/snapshot/dirty/storage/miss", nil)
	snapshotDirtyStorageInexMeter  = metrics.NewRegisteredMeter("state/snapshot/dirty/storage/inex", nil)
	snapshotDirtyStorageReadMeter  = metrics.NewRegisteredMeter("state/snapshot/dirty/storage/read", nil)
	snapshotDirtyStorageWriteMeter = metrics.NewRegisteredMeter("state/snapshot/dirty/storage/write", nil)

	snapshotDirtyAccountHitDepthHist = metrics.NewRegisteredHistogram("state/snapshot/dirty/account/hit/depth", nil, metrics.NewExpDecaySample(1028, 0.015))
	snapshotDirtyStorageHitDepthHist = metrics.NewRegisteredHistogram("state/snapshot/dirty/storage/hit/depth", nil, metrics.NewExpDecaySample(1028, 0.015))

	snapshotFlushAccountItemMeter = metrics.NewRegisteredMeter("state/snapshot/flush/account/item", nil)
	snapshotFlushAccountSizeMeter = metrics.NewRegisteredMeter("state/snapshot/flush/account/size", nil)
	snapshotFlushStorageItemMeter = metrics.NewRegisteredMeter("state/snapshot/flush/storage/item", nil)
	snapshotFlushStorageSizeMeter = metrics.NewRegisteredMeter("state/snapshot/flush/storage/size", nil)

	snapshotBloomIndexTimer = metrics.NewRegisteredResettingTimer("state/snapshot/bloom/index", nil)
	snapshotBloomErrorGauge = metrics.NewRegisteredGaugeFloat64("state/snapshot/bloom/error", nil)

	snapshotBloomAccountTrueHitMeter  = metrics.NewRegisteredMeter("state/snapshot/bloom/account/truehit", nil)
	snapshotBloomAccountFalseHitMeter = metrics.NewRegisteredMeter("state/snapshot/bloom/account/falsehit", nil)
	snapshotBloomAccountMissMeter     = metrics.NewRegisteredMeter("state/snapshot/bloom/account/miss", nil)

	snapshotBloomStorageTrueHitMeter  = metrics.NewRegisteredMeter("state/snapshot/bloom/storage/truehit", nil)
	snapshotBloomStorageFalseHitMeter = metrics.NewRegisteredMeter("state/snapshot/bloom/storage/falsehit", nil)
	snapshotBloomStorageMissMeter     = metrics.NewRegisteredMeter("state/snapshot/bloom/storage/miss", nil)

	// ErrSnapshotStale is returned from data accessors if the underlying snapshot
	// layer had been invalidated due to the chain progressing forward far enough
	// to not maintain the layer's original state.
	// チェーンが十分に前進してレイヤーの元の状態を維持できないために、
	// 基になるスナップショットレイヤーが無効にされた場合、
	// ErrSnapshotStaleがデータアクセサーから返されます。
	ErrSnapshotStale = errors.New("snapshot stale")

	// ErrNotCoveredYet is returned from data accessors if the underlying snapshot
	// is being generated currently and the requested data item is not yet in the
	// range of accounts covered.
	// 基になるスナップショットが現在生成されており、
	// 要求されたデータ項目がまだカバーされているアカウントの範囲内にない場合、
	// ErrNotCoveredYetがデータアクセサーから返されます。
	ErrNotCoveredYet = errors.New("not covered yet")

	// ErrNotConstructed is returned if the callers want to iterate the snapshot
	// while the generation is not finished yet.
	// 生成がまだ終了していないときに呼び出し元がスナップショットを繰り返したい場合は、
	// ErrNotConstructedが返されます。
	ErrNotConstructed = errors.New("snapshot is not constructed")

	// errSnapshotCycle is returned if a snapshot is attempted to be inserted
	// that forms a cycle in the snapshot tree.
	// スナップショットツリーにサイクルを形成するスナップショットを挿入しようとすると、
	// errSnapshotCycleが返されます。
	errSnapshotCycle = errors.New("snapshot cycle")
)

// Snapshot represents the functionality supported by a snapshot storage layer.
//スナップショットは、スナップショットストレージレイヤーでサポートされる機能を表します。
type Snapshot interface {
	// Root returns the root hash for which this snapshot was made.
	// Rootは、このスナップショットが作成されたルートハッシュを返します。
	Root() common.Hash

	// Account directly retrieves the account associated with a particular hash in
	// the snapshot slim data format.
	// アカウントは、スナップショットスリムデータ形式で特定のハッシュに関連付けられたアカウントを直接取得します。
	Account(hash common.Hash) (*Account, error)

	// AccountRLP directly retrieves the account RLP associated with a particular
	// hash in the snapshot slim data format.
	// AccountRLPは、スナップショットスリムデータ形式の特定のハッシュに関連付けられたアカウントRLPを直接取得します。
	AccountRLP(hash common.Hash) ([]byte, error)

	// Storage directly retrieves the storage data associated with a particular hash,
	// within a particular account.
	// ストレージは、特定のアカウント内の特定のハッシュに関連付けられたストレージデータを直接取得します。
	Storage(accountHash, storageHash common.Hash) ([]byte, error)
}

// snapshot is the internal version of the snapshot data layer that supports some
// additional methods compared to the public API.
// スナップショットは、パブリックAPIと比較していくつかの追加メソッドをサポートするスナップショットデータレイヤーの内部バージョンです。
type snapshot interface {
	Snapshot

	// Parent returns the subsequent layer of a snapshot, or nil if the base was
	// reached.
	//
	// Note, the method is an internal helper to avoid type switching between the
	// disk and diff layers. There is no locking involved.
	// 親はスナップショットの後続のレイヤーを返します。ベースに達した場合はnilを返します。
	//
	// このメソッドは、ディスクレイヤーと差分レイヤーの間でタイプが切り替わらないようにするための内部ヘルパーであることに注意してください。
	// ロックは必要ありません。
	Parent() snapshot

	// Update creates a new layer on top of the existing snapshot diff tree with
	// the specified data items.
	//
	// Note, the maps are retained by the method to avoid copying everything.
	// Updateは、指定されたデータ項目を使用して、既存のスナップショット差分ツリーの上に新しいレイヤーを作成します。
	//
	// すべてをコピーしないように、マップはメソッドによって保持されることに注意してください。
	Update(blockRoot common.Hash, destructs map[common.Hash]struct{}, accounts map[common.Hash][]byte, storage map[common.Hash]map[common.Hash][]byte) *diffLayer

	// Journal commits an entire diff hierarchy to disk into a single journal entry.
	// This is meant to be used during shutdown to persist the snapshot without
	// flattening everything down (bad for reorgs).
	// Journalは、diff階層全体をディスクにコミットして単一のジャーナルエントリにします。
	// これは、シャットダウン中に、すべてをフラット化せずにスナップショットを永続化するために使用することを目的としています（再編成には適していません）。
	Journal(buffer *bytes.Buffer) (common.Hash, error)

	// Stale return whether this layer has become stale (was flattened across) or
	// if it's still live.
	// Staleは、このレイヤーが古くなった（フラット化された）か、まだライブであるかを返します。
	Stale() bool

	// AccountIterator creates an account iterator over an arbitrary layer.
	// AccountIteratorは、任意のレイヤー上にアカウントイテレーターを作成します。
	AccountIterator(seek common.Hash) AccountIterator

	// StorageIterator creates a storage iterator over an arbitrary layer.
	// StorageIteratorは、任意のレイヤー上にストレージイテレーターを作成します。
	StorageIterator(account common.Hash, seek common.Hash) (StorageIterator, bool)
}

// Tree is an Ethereum state snapshot tree. It consists of one persistent base
// layer backed by a key-value store, on top of which arbitrarily many in-memory
// diff layers are topped. The memory diffs can form a tree with branching, but
// the disk layer is singleton and common to all. If a reorg goes deeper than the
// disk layer, everything needs to be deleted.
//
// The goal of a state snapshot is twofold: to allow direct access to account and
// storage data to avoid expensive multi-level trie lookups; and to allow sorted,
// cheap iteration of the account/storage tries for sync aid.
// ツリーはイーサリアム状態のスナップショットツリーです。
// これは、Key-Valueストアに裏打ちされた1つの永続的なベースレイヤーで構成され、
// その上に任意の数のメモリ内差分レイヤーがトップになります。
// メモリ差分は分岐のあるツリーを形成できますが、ディスクレイヤーはシングルトンであり、すべてに共通です。
// 再編成がディスクレイヤーよりも深くなる場合は、すべてを削除する必要があります。
//
// 状態スナップショットの目標は2つあります。
// アカウントとストレージデータへの直接アクセスを許可して、
// コストのかかるマルチレベルのトライルックアップを回避することです。
// アカウント/ストレージのソートされた安価な反復で同期支援を試行できるようにします。
type Tree struct {
	diskdb ethdb.KeyValueStore      // スナップショットを保存する永続データベース // Persistent database to store the snapshot
	triedb *trie.Database           // トライを介してトライにアクセスするためのメモリ内キャッシュ // In-memory cache to access the trie through
	cache  int                      // 読み取りキャッシュに使用できるメガバイト // Megabytes permitted to use for read caches
	layers map[common.Hash]snapshot // すべての既知のレイヤーのコレクション // Collection of all known layers
	lock   sync.RWMutex

	// Test hooks
	// フックをテストします
	onFlatten func() // 最下部のdiffレイヤーがフラット化されたときに呼び出されるフック // Hook invoked when the bottom most diff layers are flattened
}

// New attempts to load an already existing snapshot from a persistent key-value
// store (with a number of memory layers from a journal), ensuring that the head
// of the snapshot matches the expected one.
//
// If the snapshot is missing or the disk layer is broken, the snapshot will be
// reconstructed using both the existing data and the state trie.
// The repair happens on a background thread.
//
// If the memory layers in the journal do not match the disk layer (e.g. there is
// a gap) or the journal is missing, there are two repair cases:
//
// - if the 'recovery' parameter is true, all memory diff-layers will be discarded.
//   This case happens when the snapshot is 'ahead' of the state trie.
// - otherwise, the entire snapshot is considered invalid and will be recreated on
//   a background thread.
// 新しいは、永続的なKey-Valueストアから（ジャーナルからのいくつかのメモリレイヤーを使用して）
// 既存のスナップショットをロードしようとし、スナップショットのヘッドが予想されるものと一致することを確認します。
//
// スナップショットが欠落しているか、ディスクレイヤーが壊れている場合、
// スナップショットは既存のデータと状態トライの両方を使用して再構築されます。
// 修復はバックグラウンドスレッドで行われます。
//
// ジャーナルのメモリレイヤーがディスクレイヤーと一致しない場合（ギャップがあるなど）、
// またはジャーナルが欠落している場合は、次の2つの修復ケースがあります。
//
// --'recovery'パラメータがtrueの場合、すべてのメモリ差分レイヤーが破棄されます。
//    このケースは、スナップショットが状態トライの「前方」にある場合に発生します。
// -それ以外の場合、スナップショット全体が無効と見なされ、バックグラウンドスレッドで再作成されます。
func New(diskdb ethdb.KeyValueStore, triedb *trie.Database, cache int, root common.Hash, async bool, rebuild bool, recovery bool) (*Tree, error) {
	// Create a new, empty snapshot tree
	// 新しい空のスナップショットツリーを作成します
	snap := &Tree{
		diskdb: diskdb,
		triedb: triedb,
		cache:  cache,
		layers: make(map[common.Hash]snapshot),
	}
	if !async {
		defer snap.waitBuild()
	}
	// Attempt to load a previously persisted snapshot and rebuild one if failed
	// 以前に永続化されたスナップショットをロードし、失敗した場合は再構築を試みます
	head, disabled, err := loadSnapshot(diskdb, triedb, cache, root, recovery)
	if disabled {
		log.Warn("Snapshot maintenance disabled (syncing)") // スナップショットのメンテナンスが無効（同期）
		return snap, nil
	}
	if err != nil {
		if rebuild {
			log.Warn("Failed to load snapshot, regenerating", "err", err) // スナップショットの読み込みに失敗し、再生成しました
			snap.Rebuild(root)
			return snap, nil
		}
		return nil, err // エラーを解決し、自動的に再構築しないでください。 // Bail out the error, don't rebuild automatically.
	}
	// Existing snapshot loaded, seed all the layers
	// 既存のスナップショットをロードし、すべてのレイヤーをシードします
	for head != nil {
		snap.layers[head.Root()] = head
		head = head.Parent()
	}
	return snap, nil
}

// waitBuild blocks until the snapshot finishes rebuilding. This method is meant
// to be used by tests to ensure we're testing what we believe we are.
// スナップショットの再構築が完了するまでwaitBuildブロック。
// このメソッドは、私たちが信じているものをテストしていることを確認するためのテストで使用することを目的としています。
func (t *Tree) waitBuild() {
	// Find the rebuild termination channel
	// 再構築終了チャネルを検索します
	var done chan struct{}

	t.lock.RLock()
	for _, layer := range t.layers {
		if layer, ok := layer.(*diskLayer); ok {
			done = layer.genPending
			break
		}
	}
	t.lock.RUnlock()

	// Wait until the snapshot is generated
	// スナップショットが生成されるまで待ちます
	if done != nil {
		<-done
	}
}

// Disable interrupts any pending snapshot generator, deletes all the snapshot
// layers in memory and marks snapshots disabled globally. In order to resume
// the snapshot functionality, the caller must invoke Rebuild.
// 無効にすると、保留中のスナップショットジェネレータの割り込みが発生し、
// メモリ内のすべてのスナップショットレイヤーが削除され、スナップショットがグローバルに無効になります。
// スナップショット機能を再開するには、呼び出し元はRebuildを呼び出す必要があります。
func (t *Tree) Disable() {
	// Interrupt any live snapshot layers
	// ライブスナップショットレイヤーを中断します
	t.lock.Lock()
	defer t.lock.Unlock()

	for _, layer := range t.layers {
		switch layer := layer.(type) {
		case *diskLayer:
			// If the base layer is generating, abort it
			// ベースレイヤーが生成されている場合は、中止します
			if layer.genAbort != nil {
				abort := make(chan *generatorStats)
				layer.genAbort <- abort
				<-abort
			}
			// Layer should be inactive now, mark it as stale
			// レイヤーは現在非アクティブになっているはずです。古いものとしてマークしてください
			layer.lock.Lock()
			layer.stale = true
			layer.lock.Unlock()

		case *diffLayer:
			// If the layer is a simple diff, simply mark as stale
			// レイヤーが単純な差分である場合は、単に古いものとしてマークします
			layer.lock.Lock()
			atomic.StoreUint32(&layer.stale, 1)
			layer.lock.Unlock()

		default:
			panic(fmt.Sprintf("unknown layer type: %T", layer))
		}
	}
	t.layers = map[common.Hash]snapshot{}

	// Delete all snapshot liveness information from the database
	// データベースからすべてのスナップショットの活性情報を削除します
	batch := t.diskdb.NewBatch()

	rawdb.WriteSnapshotDisabled(batch)
	rawdb.DeleteSnapshotRoot(batch)
	rawdb.DeleteSnapshotJournal(batch)
	rawdb.DeleteSnapshotGenerator(batch)
	rawdb.DeleteSnapshotRecoveryNumber(batch)
	// Note, we don't delete the sync progress
	// 同期の進行状況は削除しないことに注意してください

	if err := batch.Write(); err != nil {
		log.Crit("Failed to disable snapshots", "err", err)
	}
}

// Snapshot retrieves a snapshot belonging to the given block root, or nil if no
// snapshot is maintained for that block.

// スナップショットは、指定されたブロックルートに属するスナップショットを取得します。
// そのブロックのスナップショットが維持されていない場合はnilを取得します。
func (t *Tree) Snapshot(blockRoot common.Hash) Snapshot {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.layers[blockRoot]
}

// Snapshots returns all visited layers from the topmost layer with specific
// root and traverses downward. The layer amount is limited by the given number.
// If nodisk is set, then disk layer is excluded.
// スナップショットは、特定のルートを持つ最上位のレイヤーから訪問したすべてのレイヤーを返し、
// 下に移動します。レイヤーの量は、指定された数によって制限されます。
// nodiskが設定されている場合、ディスクレイヤーは除外されます。
func (t *Tree) Snapshots(root common.Hash, limits int, nodisk bool) []Snapshot {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if limits == 0 {
		return nil
	}
	layer := t.layers[root]
	if layer == nil {
		return nil
	}
	var ret []Snapshot
	for {
		if _, isdisk := layer.(*diskLayer); isdisk && nodisk {
			break
		}
		ret = append(ret, layer)
		limits -= 1
		if limits == 0 {
			break
		}
		parent := layer.Parent()
		if parent == nil {
			break
		}
		layer = parent
	}
	return ret
}

// Update adds a new snapshot into the tree, if that can be linked to an existing
// old parent. It is disallowed to insert a disk layer (the origin of all).
// Updateは、既存の古い親にリンクできる場合、ツリーに新しいスナップショットを追加します。
// ディスクレイヤー（すべての原点）を挿入することは許可されていません。
func (t *Tree) Update(blockRoot common.Hash, parentRoot common.Hash, destructs map[common.Hash]struct{}, accounts map[common.Hash][]byte, storage map[common.Hash]map[common.Hash][]byte) error {
	// Reject noop updates to avoid self-loops in the snapshot tree. This is a
	// special case that can only happen for Clique networks where empty blocks
	// don't modify the state (0 block subsidy).
	//
	// Although we could silently ignore this internally, it should be the caller's
	// responsibility to avoid even attempting to insert such a snapshot.
	// スナップショットツリーでの自己ループを回避するために、noopの更新を拒否します。
	// これは、空のブロックが状態を変更しないクリークネットワークでのみ発生する可能性がある特殊なケースです（0ブロックの補助金）。
	//
	//これを内部的に黙って無視することもできますが、そのようなスナップショットを挿入しようとさえしないようにするのは呼び出し元の責任です。
	if blockRoot == parentRoot {
		return errSnapshotCycle
	}
	// Generate a new snapshot on top of the parent
	// 親の上に新しいスナップショットを生成します
	parent := t.Snapshot(parentRoot)
	if parent == nil {
		return fmt.Errorf("parent [%#x] snapshot missing", parentRoot)
	}
	snap := parent.(snapshot).Update(blockRoot, destructs, accounts, storage)

	// Save the new snapshot for later
	// 後で使用するために新しいスナップショットを保存します
	t.lock.Lock()
	defer t.lock.Unlock()

	t.layers[snap.root] = snap
	return nil
}

// Cap traverses downwards the snapshot tree from a head block hash until the
// number of allowed layers are crossed. All layers beyond the permitted number
// are flattened downwards.
//
// Note, the final diff layer count in general will be one more than the amount
// requested. This happens because the bottom-most diff layer is the accumulator
// which may or may not overflow and cascade to disk. Since this last layer's
// survival is only known *after* capping, we need to omit it from the count if
// we want to ensure that *at least* the requested number of diff layers remain.
// キャップは、許可されたレイヤーの数を超えるまで、
// ヘッドブロックハッシュからスナップショットツリーを下方向にトラバースします。
// 許可された数を超えるすべてのレイヤーは、下向きに平坦化されます。
//
// 通常、最終的な差分レイヤー数は、要求された量より1つ多いことに注意してください。
// これは、最下部の差分レイヤーがアキュムレータであり、
// オーバーフローしてディスクにカスケードされる場合とされない場合があるために発生します。
// この最後のレイヤーの存続はキャッピング後*しかわからないため、
// *少なくとも*要求された数のdiffレイヤーが残るようにする場合は、カウントから除外する必要があります。
func (t *Tree) Cap(root common.Hash, layers int) error {
	// Retrieve the head snapshot to cap from
	// キャップするヘッドスナップショットを取得します
	snap := t.Snapshot(root)
	if snap == nil {
		return fmt.Errorf("snapshot [%#x] missing", root)
	}
	diff, ok := snap.(*diffLayer)
	if !ok {
		return fmt.Errorf("snapshot [%#x] is disk layer", root)
	}
	// If the generator is still running, use a more aggressive cap
	// ジェネレーターがまだ実行中の場合は、より積極的な上限を使用します
	diff.origin.lock.RLock()
	if diff.origin.genMarker != nil && layers > 8 {
		layers = 8
	}
	diff.origin.lock.RUnlock()

	// Run the internal capping and discard all stale layers
	// 内部キャッピングを実行し、古いレイヤーをすべて破棄します
	t.lock.Lock()
	defer t.lock.Unlock()

	// Flattening the bottom-most diff layer requires special casing since there's
	// no child to rewire to the grandparent. In that case we can fake a temporary
	// child for the capping and then remove it.
	// 祖父母に再配線する子がないため、最下部のdiffレイヤーをフラット化するには特別なケーシングが必要です。
	// その場合、キャッピングのために一時的な子を偽造してから削除することができます。
	if layers == 0 {
		// If full commit was requested, flatten the diffs and merge onto disk
		// フルコミットが要求された場合は、差分をフラット化してディスクにマージします
		diff.lock.RLock()
		base := diffToDisk(diff.flatten().(*diffLayer))
		diff.lock.RUnlock()

		// Replace the entire snapshot tree with the flat base
		// スナップショットツリー全体をフラットベースに置き換えます
		t.layers = map[common.Hash]snapshot{base.root: base}
		return nil
	}
	persisted := t.cap(diff, layers)

	// Remove any layer that is stale or links into a stale layer
	// 古くなっているレイヤーまたは古くなったレイヤーにリンクしているレイヤーを削除します
	children := make(map[common.Hash][]common.Hash)
	for root, snap := range t.layers {
		if diff, ok := snap.(*diffLayer); ok {
			parent := diff.parent.Root()
			children[parent] = append(children[parent], root)
		}
	}
	var remove func(root common.Hash)
	remove = func(root common.Hash) {
		delete(t.layers, root)
		for _, child := range children[root] {
			remove(child)
		}
		delete(children, root)
	}
	for root, snap := range t.layers {
		if snap.Stale() {
			remove(root)
		}
	}
	// If the disk layer was modified, regenerate all the cumulative blooms
	// ディスクレイヤーが変更された場合は、すべての累積ブルームを再生成します
	if persisted != nil {
		var rebloom func(root common.Hash)
		rebloom = func(root common.Hash) {
			if diff, ok := t.layers[root].(*diffLayer); ok {
				diff.rebloom(persisted)
			}
			for _, child := range children[root] {
				rebloom(child)
			}
		}
		rebloom(persisted.root)
	}
	return nil
}

// cap traverses downwards the diff tree until the number of allowed layers are
// crossed. All diffs beyond the permitted number are flattened downwards. If the
// layer limit is reached, memory cap is also enforced (but not before).
//
// The method returns the new disk layer if diffs were persisted into it.
//
// Note, the final diff layer count in general will be one more than the amount
// requested. This happens because the bottom-most diff layer is the accumulator
// which may or may not overflow and cascade to disk. Since this last layer's
// survival is only known *after* capping, we need to omit it from the count if
// we want to ensure that *at least* the requested number of diff layers remain.
// capは、許可されたレイヤーの数を超えるまで、diffツリーを下方向にトラバースします。
// 許可された数を超えるすべての差分は、下向きにフラット化されます。レイヤーの制限に達すると、
// メモリの上限も適用されます（以前は適用されません）。
//
// diffが永続化されている場合、メソッドは新しいディスクレイヤーを返します。
//
// 通常、最終的な差分レイヤー数は、要求された量より1つ多いことに注意してください。
// これは、最下部の差分レイヤーがアキュムレータであり、
// オーバーフローしてディスクにカスケードされる場合とされない場合があるために発生します。
// この最後のレイヤーの存続はキャッピング後*しかわからないため、
// *少なくとも*要求された数のdiffレイヤーが残るようにする場合は、カウントから除外する必要があります。
func (t *Tree) cap(diff *diffLayer, layers int) *diskLayer {
	// Dive until we run out of layers or reach the persistent database
	// レイヤーがなくなるか、永続データベースに到達するまで飛び込みます
	for i := 0; i < layers-1; i++ {
		// If we still have diff layers below, continue down
		// まだ下に差分レイヤーがある場合は、下に進みます
		if parent, ok := diff.parent.(*diffLayer); ok {
			diff = parent
		} else {
			// Diff stack too shallow, return without modifications
			// 差分スタックが浅すぎます。変更せずに返します
			return nil
		}
	}
	// We're out of layers, flatten anything below, stopping if it's the disk or if
	// the memory limit is not yet exceeded.
	// レイヤーが不足しているため、下にあるものをフラット化し、
	// ディスクの場合、またはメモリ制限をまだ超えていない場合は停止します。
	switch parent := diff.parent.(type) {
	case *diskLayer:
		return nil

	case *diffLayer:
		// Hold the write lock until the flattened parent is linked correctly.
		// Otherwise, the stale layer may be accessed by external reads in the
		// meantime.
		// フラット化された親が正しくリンクされるまで書き込みロックを保持します。
		// それ以外の場合、古いレイヤーは、その間に外部読み取りによってアクセスされる可能性があります。
		diff.lock.Lock()
		defer diff.lock.Unlock()

		// Flatten the parent into the grandparent. The flattening internally obtains a
		// write lock on grandparent.
		// 親を祖父母にフラット化します。
		// 平坦化は、祖父母のロックを内部的に取得します。
		flattened := parent.flatten().(*diffLayer)
		t.layers[flattened.root] = flattened

		// Invoke the hook if it's registered. Ugly hack.
		// 登録されている場合は、フックを呼び出します。醜いハック。
		if t.onFlatten != nil {
			t.onFlatten()
		}
		diff.parent = flattened
		if flattened.memory < aggregatorMemoryLimit {
			// Accumulator layer is smaller than the limit, so we can abort, unless
			// there's a snapshot being generated currently. In that case, the trie
			// will move from underneath the generator so we **must** merge all the
			// partial data down into the snapshot and restart the generation.
			// アキュムレータレイヤーが制限よりも小さいため、
			// 現在生成されているスナップショットがない限り、中止できます。
			// その場合、トライはジェネレーターの下から移動するため、
			// すべての部分データをスナップショットにマージして生成を再開する必要があります。
			if flattened.parent.(*diskLayer).genAbort == nil {
				return nil
			}
		}
	default:
		panic(fmt.Sprintf("unknown data layer: %T", parent))
	}
	// If the bottom-most layer is larger than our memory cap, persist to disk
	// 最下層がメモリの上限よりも大きい場合は、ディスクに保持します
	bottom := diff.parent.(*diffLayer)

	bottom.lock.RLock()
	base := diffToDisk(bottom)
	bottom.lock.RUnlock()

	t.layers[base.root] = base
	diff.parent = base
	return base
}

// diffToDisk merges a bottom-most diff into the persistent disk layer underneath
// it. The method will panic if called onto a non-bottom-most diff layer.
//
// The disk layer persistence should be operated in an atomic way. All updates should
// be discarded if the whole transition if not finished.
// diffToDiskは、最下部のdiffをその下の永続ディスクレイヤーにマージします。
// 最下部以外のdiffレイヤーに呼び出されると、メソッドはパニックになります。
//
// ディスク層の永続性はアトミックな方法で操作する必要があります。
// 移行全体が終了していない場合は、すべての更新を破棄する必要があります。
func diffToDisk(bottom *diffLayer) *diskLayer {
	var (
		base  = bottom.parent.(*diskLayer)
		batch = base.diskdb.NewBatch()
		stats *generatorStats
	)
	// If the disk layer is running a snapshot generator, abort it
	// ディスクレイヤーがスナップショットジェネレーターを実行している場合は、それを中止します
	if base.genAbort != nil {
		abort := make(chan *generatorStats)
		base.genAbort <- abort
		stats = <-abort
	}
	// Put the deletion in the batch writer, flush all updates in the final step.
	// 削除をバッチライターに入れ、最後のステップですべての更新をフラッシュします。
	rawdb.DeleteSnapshotRoot(batch)

	// Mark the original base as stale as we're going to create a new wrapper
	// 新しいラッパーを作成するため、元のベースを古いものとしてマークします
	base.lock.Lock()
	if base.stale {
		panic("parent disk layer is stale") // 親ディスクレイヤーが古くなっています // 2人の子供から同じベースにコミットしました、ブー // we've committed into the same base from two children, boo
	}
	base.stale = true
	base.lock.Unlock()

	// Destroy all the destructed accounts from the database
	// データベースから破壊されたアカウントをすべて破棄します
	for hash := range bottom.destructSet {
		// Skip any account not covered yet by the snapshot
		// スナップショットでまだカバーされていないアカウントをスキップします
		if base.genMarker != nil && bytes.Compare(hash[:], base.genMarker) > 0 {
			continue
		}
		// Remove all storage slots
		// すべてのストレージスロットを削除します
		rawdb.DeleteAccountSnapshot(batch, hash)
		base.cache.Set(hash[:], nil)

		it := rawdb.IterateStorageSnapshots(base.diskdb, hash)
		for it.Next() {
			if key := it.Key(); len(key) == 65 { // TODO（karalabe）：はい、これをイテレータに移動する必要があります// TODO(karalabe): Yuck, we should move this into the iterator
				batch.Delete(key)
				base.cache.Del(key[1:])
				snapshotFlushStorageItemMeter.Mark(1)

				// Ensure we don't delete too much data blindly (contract can be
				// huge). It's ok to flush, the root will go missing in case of a
				// crash and we'll detect and regenerate the snapshot.
				// あまりにも多くのデータを盲目的に削除しないようにします（契約は膨大になる可能性があります）。
				// フラッシュしても問題ありません。
				// クラッシュした場合はルートが失われ、スナップショットを検出して再生成します。
				if batch.ValueSize() > ethdb.IdealBatchSize {
					if err := batch.Write(); err != nil {
						log.Crit("Failed to write storage deletions", "err", err)
					}
					batch.Reset()
				}
			}
		}
		it.Release()
	}
	// Push all updated accounts into the database
	// 更新されたすべてのアカウントをデータベースにプッシュします
	for hash, data := range bottom.accountData {
		// Skip any account not covered yet by the snapshot
		// スナップショットでまだカバーされていないアカウントをスキップします
		if base.genMarker != nil && bytes.Compare(hash[:], base.genMarker) > 0 {
			continue
		}
		// Push the account to disk
		// アカウントをディスクにプッシュします
		rawdb.WriteAccountSnapshot(batch, hash, data)
		base.cache.Set(hash[:], data)
		snapshotCleanAccountWriteMeter.Mark(int64(len(data)))

		snapshotFlushAccountItemMeter.Mark(1)
		snapshotFlushAccountSizeMeter.Mark(int64(len(data)))

		// Ensure we don't write too much data blindly. It's ok to flush, the
		// root will go missing in case of a crash and we'll detect and regen
		// the snapshot.
		// 盲目的に大量のデータを書き込まないようにします。フラッシュしても問題ありません。
		// クラッシュした場合はルートが失われ、スナップショットを検出して再生成します。
		if batch.ValueSize() > ethdb.IdealBatchSize {
			if err := batch.Write(); err != nil {
				log.Crit("Failed to write storage deletions", "err", err) // ストレージ削除の書き込みに失敗しました
			}
			batch.Reset()
		}
	}
	// Push all the storage slots into the database
	// すべてのストレージスロットをデータベースにプッシュします
	for accountHash, storage := range bottom.storageData {
		// Skip any account not covered yet by the
		// スナップショットでまだカバーされていないアカウントをスキップします
		if base.genMarker != nil && bytes.Compare(accountHash[:], base.genMarker) > 0 {
			continue
		}
		// Generation might be mid-account, track that case too
		// 生成はアカウントの途中である可能性があり、その場合も追跡します
		midAccount := base.genMarker != nil && bytes.Equal(accountHash[:], base.genMarker[:common.HashLength])

		for storageHash, data := range storage {
			// Skip any slot not covered yet by the snapshot
			// スナップショットでまだカバーされていないスロットをスキップします
			if midAccount && bytes.Compare(storageHash[:], base.genMarker[common.HashLength:]) > 0 {
				continue
			}
			if len(data) > 0 {
				rawdb.WriteStorageSnapshot(batch, accountHash, storageHash, data)
				base.cache.Set(append(accountHash[:], storageHash[:]...), data)
				snapshotCleanStorageWriteMeter.Mark(int64(len(data)))
			} else {
				rawdb.DeleteStorageSnapshot(batch, accountHash, storageHash)
				base.cache.Set(append(accountHash[:], storageHash[:]...), nil)
			}
			snapshotFlushStorageItemMeter.Mark(1)
			snapshotFlushStorageSizeMeter.Mark(int64(len(data)))
		}
	}
	// Update the snapshot block marker and write any remainder data
	// スナップショットブロックマーカーを更新し、残りのデータを書き込みます
	rawdb.WriteSnapshotRoot(batch, bottom.root)

	// Write out the generator progress marker and report
	// ジェネレータの進行状況マーカーを書き出してレポートします
	journalProgress(batch, base.genMarker, stats)

	// Flush all the updates in the single db operation. Ensure the
	// disk layer transition is atomic.
	// 単一のdb操作ですべての更新をフラッシュします。
	// ディスク層の移行がアトミックであることを確認します。
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write leftover snapshot", "err", err) // 残りのスナップショットを書き込めませんでした
	}
	log.Debug("Journalled disk layer", "root", bottom.root, "complete", base.genMarker == nil) // ジャーナル化されたディスクレイヤー
	res := &diskLayer{
		root:       bottom.root,
		cache:      base.cache,
		diskdb:     base.diskdb,
		triedb:     base.triedb,
		genMarker:  base.genMarker,
		genPending: base.genPending,
	}
	// If snapshot generation hasn't finished yet, port over all the starts and
	// continue where the previous round left off.
	//
	// Note, the `base.genAbort` comparison is not used normally, it's checked
	// to allow the tests to play with the marker without triggering this path.
	// スナップショットの生成がまだ終了していない場合は、
	// すべての開始を移植し、前のラウンドが中断したところから続行します。
	//
	// `base.genAbort`比較は通常は使用されないことに注意してください。
	// このパスをトリガーせずに、テストがマーカーで再生できるようにチェックされています。
	if base.genMarker != nil && base.genAbort != nil {
		res.genMarker = base.genMarker
		res.genAbort = make(chan chan *generatorStats)
		go res.generate(stats)
	}
	return res
}

// Journal commits an entire diff hierarchy to disk into a single journal entry.
// This is meant to be used during shutdown to persist the snapshot without
// flattening everything down (bad for reorgs).
//
// The method returns the root hash of the base layer that needs to be persisted
// to disk as a trie too to allow continuing any pending generation op.

// Journalは、diff階層全体をディスクにコミットして単一のジャーナルエントリにします。
// これは、シャットダウン中に、すべてをフラット化せずにスナップショットを永続化するために使用することを目的としています（再編成には適していません）。
//
// このメソッドは、保留中の生成操作を続行できるようにするために、トライとしてディスクに永続化する必要があるベースレイヤーのルートハッシュを返します。
func (t *Tree) Journal(root common.Hash) (common.Hash, error) {
	// Retrieve the head snapshot to journal from var snap snapshot
	// varsnapsnapshotからジャーナルにヘッドスナップショットを取得します
	snap := t.Snapshot(root)
	if snap == nil {
		return common.Hash{}, fmt.Errorf("snapshot [%#x] missing", root)
	}
	// Run the journaling
	// ジャーナリングを実行します
	t.lock.Lock()
	defer t.lock.Unlock()

	// Firstly write out the metadata of journal
	// 最初にジャーナルのメタデータを書き出します
	journal := new(bytes.Buffer)
	if err := rlp.Encode(journal, journalVersion); err != nil {
		return common.Hash{}, err
	}
	diskroot := t.diskRoot()
	if diskroot == (common.Hash{}) {
		return common.Hash{}, errors.New("invalid disk root") // 無効なディスクルート
	}
	// Secondly write out the disk layer root, ensure the
	// diff journal is continuous with disk.
	// 次に、ディスクレイヤールートを書き出し、
	// diffジャーナルがディスクと連続していることを確認します。
	if err := rlp.Encode(journal, diskroot); err != nil {
		return common.Hash{}, err
	}
	// Finally write out the journal of each layer in reverse order.
	// 最後に、各レイヤーのジャーナルを逆の順序で書き出します。
	base, err := snap.(snapshot).Journal(journal)
	if err != nil {
		return common.Hash{}, err
	}
	// Store the journal into the database and return
	// ジャーナルをデータベースに保存し、
	rawdb.WriteSnapshotJournal(t.diskdb, journal.Bytes())
	return base, nil
}

// Rebuild wipes all available snapshot data from the persistent database and
// discard all caches and diff layers. Afterwards, it starts a new snapshot
// generator with the given root hash.
// Rebuildは、永続データベースから使用可能なすべてのスナップショットデータをワイプし、
// すべてのキャッシュと差分レイヤーを破棄します。
// その後、指定されたルートハッシュを使用して新しいスナップショットジェネレータを起動します。
func (t *Tree) Rebuild(root common.Hash) {
	t.lock.Lock()
	defer t.lock.Unlock()

	// Firstly delete any recovery flag in the database. Because now we are
	// building a brand new snapshot. Also reenable the snapshot feature.
	// まず、データベース内の回復フラグを削除します。
	// 今、私たちはまったく新しいスナップショットを作成しているからです。
	// また、スナップショット機能を再度有効にします。
	rawdb.DeleteSnapshotRecoveryNumber(t.diskdb)
	rawdb.DeleteSnapshotDisabled(t.diskdb)

	// Iterate over and mark all layers stale
	// 繰り返して、すべてのレイヤーを古くなったとマークします
	for _, layer := range t.layers {
		switch layer := layer.(type) {
		case *diskLayer:
			// If the base layer is generating, abort it and save
			// ベースレイヤーが生成されている場合は、それを中止して保存します
			if layer.genAbort != nil {
				abort := make(chan *generatorStats)
				layer.genAbort <- abort
				<-abort
			}
			// Layer should be inactive now, mark it as stale
			// レイヤーは現在非アクティブになっているはずです。古いものとしてマークしてください
			layer.lock.Lock()
			layer.stale = true
			layer.lock.Unlock()

		case *diffLayer:
			// If the layer is a simple diff, simply mark as stale
			// レイヤーが単純な差分である場合は、単に古いものとしてマークします
			layer.lock.Lock()
			atomic.StoreUint32(&layer.stale, 1)
			layer.lock.Unlock()

		default:
			panic(fmt.Sprintf("unknown layer type: %T", layer)) // 不明なレイヤータイプ：
		}
	}
	// Start generating a new snapshot from scratch on a background thread. The
	// generator will run a wiper first if there's not one running right now.
	// バックグラウンドスレッドで最初から新しいスナップショットの生成を開始します。
	// 現在実行中のワイパーがない場合、ジェネレーターは最初にワイパーを実行します。
	log.Info("Rebuilding state snapshot")
	t.layers = map[common.Hash]snapshot{
		root: generateSnapshot(t.diskdb, t.triedb, t.cache, root),
	}
}

// AccountIterator creates a new account iterator for the specified root hash and
// seeks to a starting account hash.
// AccountIteratorは、指定されたルートハッシュの新しいアカウントイテレータを作成し、開始アカウントハッシュを探します。
func (t *Tree) AccountIterator(root common.Hash, seek common.Hash) (AccountIterator, error) {
	ok, err := t.generating()
	if err != nil {
		return nil, err
	}
	if ok {
		return nil, ErrNotConstructed
	}
	return newFastAccountIterator(t, root, seek)
}

// StorageIterator creates a new storage iterator for the specified root hash and
// account. The iterator will be move to the specific start position.
// StorageIteratorは、指定されたルートハッシュとアカウントの新しいストレージイテレータを作成します。
// イテレータは特定の開始位置に移動します。
func (t *Tree) StorageIterator(root common.Hash, account common.Hash, seek common.Hash) (StorageIterator, error) {
	ok, err := t.generating()
	if err != nil {
		return nil, err
	}
	if ok {
		return nil, ErrNotConstructed
	}
	return newFastStorageIterator(t, root, account, seek)
}

// Verify iterates the whole state(all the accounts as well as the corresponding storages)
// with the specific root and compares the re-computed hash with the original one.
// 状態全体（すべてのアカウントと対応するストレージ）を特定のルートで反復し、
// 再計算されたハッシュを元のハッシュと比較することを確認します。
func (t *Tree) Verify(root common.Hash) error {
	acctIt, err := t.AccountIterator(root, common.Hash{})
	if err != nil {
		return err
	}
	defer acctIt.Release()

	got, err := generateTrieRoot(nil, acctIt, common.Hash{}, stackTrieGenerate, func(db ethdb.KeyValueWriter, accountHash, codeHash common.Hash, stat *generateStats) (common.Hash, error) {
		storageIt, err := t.StorageIterator(root, accountHash, common.Hash{})
		if err != nil {
			return common.Hash{}, err
		}
		defer storageIt.Release()

		hash, err := generateTrieRoot(nil, storageIt, accountHash, stackTrieGenerate, nil, stat, false)
		if err != nil {
			return common.Hash{}, err
		}
		return hash, nil
	}, newGenerateStats(), true)

	if err != nil {
		return err
	}
	if got != root {
		return fmt.Errorf("state root hash mismatch: got %x, want %x", got, root) // 状態ルートハッシュの不一致：％xを取得、％xが必要
	}
	return nil
}

// disklayer is an internal helper function to return the disk layer.
// The lock of snapTree is assumed to be held already.
// disklayerは、ディスクレイヤーを返すための内部ヘルパー関数です。
// snapTreeのロックはすでに保持されていると想定されます。
func (t *Tree) disklayer() *diskLayer {
	var snap snapshot
	for _, s := range t.layers {
		snap = s
		break
	}
	if snap == nil {
		return nil
	}
	switch layer := snap.(type) {
	case *diskLayer:
		return layer
	case *diffLayer:
		return layer.origin
	default:
		panic(fmt.Sprintf("%T: undefined layer", snap)) // ％T：未定義のレイヤー
	}
}

// diskRoot is a internal helper function to return the disk layer root.
// The lock of snapTree is assumed to be held already.
// diskRootは、ディスクレイヤーのルートを返す内部ヘルパー関数です。
// snapTreeのロックはすでに保持されていると想定されます。
func (t *Tree) diskRoot() common.Hash {
	disklayer := t.disklayer()
	if disklayer == nil {
		return common.Hash{}
	}
	return disklayer.Root()
}

// generating is an internal helper function which reports whether the snapshot
// is still under the construction.
// 生成は、スナップショットがまだ作成中であるかどうかを報告する内部ヘルパー関数です。
func (t *Tree) generating() (bool, error) {
	t.lock.Lock()
	defer t.lock.Unlock()

	layer := t.disklayer()
	if layer == nil {
		return false, errors.New("disk layer is missing") // ディスクレイヤーがありません
	}
	layer.lock.RLock()
	defer layer.lock.RUnlock()
	return layer.genMarker != nil, nil
}

// diskRoot is a external helper function to return the disk layer root.
// diskRootは、ディスクレイヤーのルートを返す外部ヘルパー関数です。
func (t *Tree) DiskRoot() common.Hash {
	t.lock.Lock()
	defer t.lock.Unlock()

	return t.diskRoot()
}
