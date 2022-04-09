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
	"bytes"
	"sync"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// diskLayer is a low level persistent snapshot built on top of a key-value store.
// diskLayerは、Key-Valueストアの上に構築された低レベルの永続スナップショットです。
type diskLayer struct {
	diskdb ethdb.KeyValueStore // ベーススナップショットを含むKey-Valueストア // Key-value store containing the base snapshot
	triedb *trie.Database      // 再構築の目的でノードキャッシュを試行します // Trie node cache for reconstruction purposes
	cache  *fastcache.Cache    // 直接アクセスのためにディスクにぶつからないようにキャッシュする // Cache to avoid hitting the disk for direct access

	root  common.Hash // ベーススナップショットのルートハッシュ                   // Root hash of the base snapshot
	stale bool        // レイヤーが古くなったことを通知します（状態が進行しました） // Signals that the layer became stale (state progressed)

	genMarker  []byte                    // 初期レイヤー生成中にインデックス付けされた状態のマーカー    // Marker for the state that's indexed during initial layer generation
	genPending chan struct{}             // 生成が完了したときの通知チャネル（同期性のテスト）          // Notification channel when generation is done (test synchronicity)
	genAbort   chan chan *generatorStats // このレイヤーでのスナップショットの生成を中止する通知チャネル // Notification channel to abort generating the snapshot in this layer

	lock sync.RWMutex
}

// Root returns  root hash for which this snapshot was made.
// Rootは、このスナップショットが作成されたルートハッシュを返します。
func (dl *diskLayer) Root() common.Hash {
	return dl.root
}

// Parent always returns nil as there's no layer below the disk.
// ディスクの下にレイヤーがないため、親は常にnilを返します。
func (dl *diskLayer) Parent() snapshot {
	return nil
}

// Stale return whether this layer has become stale (was flattened across) or if
// it's still live.
// Staleは、このレイヤーが古くなった（フラット化された）か、まだライブであるかを返します。
func (dl *diskLayer) Stale() bool {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	return dl.stale
}

// Account directly retrieves the account associated with a particular hash in
// the snapshot slim data format.
// アカウントは、スナップショットスリムデータ形式で特定のハッシュに関連付けられたアカウントを直接取得します。
func (dl *diskLayer) Account(hash common.Hash) (*Account, error) {
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
// AccountRLPは、スナップショットスリムデータ形式の特定のハッシュに関連付けられたアカウントRLPを直接取得します。
func (dl *diskLayer) AccountRLP(hash common.Hash) ([]byte, error) {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	// If the layer was flattened into, consider it invalid (any live reference to
	// the original should be marked as unusable).
	// レイヤーがフラット化されている場合は、無効と見なします
	//（元のレイヤーへのライブ参照はすべて使用不可としてマークする必要があります）。
	if dl.stale {
		return nil, ErrSnapshotStale
	}
	// If the layer is being generated, ensure the requested hash has already been
	// covered by the generator.
	// レイヤーが生成されている場合は、要求されたハッシュが既にジェネレーターによってカバーされていることを確認してください。
	if dl.genMarker != nil && bytes.Compare(hash[:], dl.genMarker) > 0 {
		return nil, ErrNotCoveredYet
	}
	// If we're in the disk layer, all diff layers missed
	// ディスクレイヤーにいる場合、すべての差分レイヤーが欠落しています
	snapshotDirtyAccountMissMeter.Mark(1)

	// Try to retrieve the account from the memory cache
	// メモリキャッシュからアカウントを取得しようとします
	if blob, found := dl.cache.HasGet(nil, hash[:]); found {
		snapshotCleanAccountHitMeter.Mark(1)
		snapshotCleanAccountReadMeter.Mark(int64(len(blob)))
		return blob, nil
	}
	// Cache doesn't contain account, pull from disk and cache for later
	// キャッシュにアカウントが含まれていない、ディスクからプルして後でキャッシュする
	blob := rawdb.ReadAccountSnapshot(dl.diskdb, hash)
	dl.cache.Set(hash[:], blob)

	snapshotCleanAccountMissMeter.Mark(1)
	if n := len(blob); n > 0 {
		snapshotCleanAccountWriteMeter.Mark(int64(n))
	} else {
		snapshotCleanAccountInexMeter.Mark(1)
	}
	return blob, nil
}

// Storage directly retrieves the storage data associated with a particular hash,
// within a particular account.
// ストレージは、特定のアカウント内の特定のハッシュに関連付けられたストレージデータを直接取得します。
func (dl *diskLayer) Storage(accountHash, storageHash common.Hash) ([]byte, error) {
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	// If the layer was flattened into, consider it invalid (any live reference to
	// the original should be marked as unusable).
	// レイヤーがフラット化されている場合は、無効と見なします
	// （元のレイヤーへのライブ参照はすべて使用不可としてマークする必要があります）。
	if dl.stale {
		return nil, ErrSnapshotStale
	}
	key := append(accountHash[:], storageHash[:]...)

	// If the layer is being generated, ensure the requested hash has already been
	// covered by the generator.
	// レイヤーが生成されている場合は、要求されたハッシュが既にジェネレーターによってカバーされていることを確認してください。
	if dl.genMarker != nil && bytes.Compare(key, dl.genMarker) > 0 {
		return nil, ErrNotCoveredYet
	}
	// If we're in the disk layer, all diff layers missed
	// ディスクレイヤーにいる場合、すべての差分レイヤーが欠落しています
	snapshotDirtyStorageMissMeter.Mark(1)

	// Try to retrieve the storage slot from the memory cache
	// メモリキャッシュからストレージスロットを取得しようとします
	if blob, found := dl.cache.HasGet(nil, key); found {
		snapshotCleanStorageHitMeter.Mark(1)
		snapshotCleanStorageReadMeter.Mark(int64(len(blob)))
		return blob, nil
	}
	// Cache doesn't contain storage slot, pull from disk and cache for later
	// キャッシュにストレージスロットが含まれていない、ディスクからプルして後でキャッシュする
	blob := rawdb.ReadStorageSnapshot(dl.diskdb, accountHash, storageHash)
	dl.cache.Set(key, blob)

	snapshotCleanStorageMissMeter.Mark(1)
	if n := len(blob); n > 0 {
		snapshotCleanStorageWriteMeter.Mark(int64(n))
	} else {
		snapshotCleanStorageInexMeter.Mark(1)
	}
	return blob, nil
}

// Update creates a new layer on top of the existing snapshot diff tree with
// the specified data items. Note, the maps are retained by the method to avoid
// copying everything.
// Updateは、指定されたデータ項目を使用して、既存のスナップショット差分ツリーの上に新しいレイヤーを作成します。
// すべてをコピーしないように、マップはメソッドによって保持されることに注意してください。
func (dl *diskLayer) Update(blockHash common.Hash, destructs map[common.Hash]struct{}, accounts map[common.Hash][]byte, storage map[common.Hash]map[common.Hash][]byte) *diffLayer {
	return newDiffLayer(dl, blockHash, destructs, accounts, storage)
}
