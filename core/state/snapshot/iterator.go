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
	"fmt"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
)

// Iterator is an iterator to step over all the accounts or the specific
// storage in a snapshot which may or may not be composed of multiple layers.

// イテレーターは、スナップショット内のすべてのアカウントまたは特定のストレージをステップオーバーするイテレーターであり、
// 複数のレイヤーで構成されている場合と構成されていない場合があります。
type Iterator interface {
	// Next steps the iterator forward one element, returning false if exhausted,
	// or an error if iteration failed for some reason (e.g. root being iterated
	// becomes stale and garbage collected).
	// 次のステップでは、イテレータが1つの要素を転送し、使い果たされた場合はfalseを返し、
	// 何らかの理由で反復が失敗した場合はエラーを返します
	// （たとえば、反復されるルートが古くなり、ガベージコレクションが発生します）。
	Next() bool

	// Error returns any failure that occurred during iteration, which might have
	// caused a premature iteration exit (e.g. snapshot stack becoming stale).
	// エラーは、反復中に発生した障害を返します。
	// これにより、反復が途中で終了する可能性があります（スナップショットスタックが古くなるなど）。
	Error() error

	// Hash returns the hash of the account or storage slot the iterator is
	// currently at.
	// ハッシュは、イテレータが現在存在するアカウントまたはストレージスロットのハッシュを返します。
	Hash() common.Hash

	// Release releases associated resources. Release should always succeed and
	// can be called multiple times without causing error.
	// リリース関連のリソースをリリースします。
	// リリースは常に成功する必要があり、エラーを発生させることなく複数回呼び出すことができます。
	Release()
}

// AccountIterator is an iterator to step over all the accounts in a snapshot,
// which may or may not be composed of multiple layers.
// AccountIteratorは、スナップショット内のすべてのアカウントをステップオーバーするイテレーターであり、
// 複数のレイヤーで構成されている場合と構成されていない場合があります。
type AccountIterator interface {
	Iterator

	// Account returns the RLP encoded slim account the iterator is currently at.
	// An error will be returned if the iterator becomes invalid
	// アカウントは、イテレータが現在使用しているRLPエンコードされたスリムアカウントを返します。
	// イテレータが無効になると、エラーが返されます
	Account() []byte
}

// StorageIterator is an iterator to step over the specific storage in a snapshot,
// which may or may not be composed of multiple layers.
// StorageIteratorは、スナップショット内の特定のストレージをステップオーバーするイテレーターであり、
// 複数のレイヤーで構成されている場合と構成されていない場合があります。
type StorageIterator interface {
	Iterator

	// Slot returns the storage slot the iterator is currently at. An error will
	// be returned if the iterator becomes invalid
	// Slotは、イテレータが現在存在するストレージスロットを返します。
	// イテレータが無効になるとエラーが返されます
	Slot() []byte
}

// diffAccountIterator is an account iterator that steps over the accounts (both
// live and deleted) contained within a single diff layer. Higher order iterators
// will use the deleted accounts to skip deeper iterators.
// diffAccountIteratorは、単一のdiffレイヤーに含まれるアカウント（ライブと削除の両方）
// をステップオーバーするアカウントイテレーターです。
// 高次のイテレータは、削除されたアカウントを使用して、より深いイテレータをスキップします。
type diffAccountIterator struct {
	// curHash is the current hash the iterator is positioned on. The field is
	// explicitly tracked since the referenced diff layer might go stale after
	// the iterator was positioned and we don't want to fail accessing the old
	// hash as long as the iterator is not touched any more.
	// curHashは、イテレータが配置されている現在のハッシュです。
	// イテレータが配置された後に参照されるdiffレイヤーが古くなる可能性があるため、
	// フィールドは明示的に追跡されます。
	// イテレータに触れない限り、古いハッシュへのアクセスに失敗することはありません。
	curHash common.Hash

	layer *diffLayer    // 値を取得するライブレイヤー // Live layer to retrieve values from
	keys  []common.Hash // 反復するためにレイヤーに残されたキー // Keys left in the layer to iterate
	fail  error         // 発生した障害（失効） // Any failures encountered (stale)
}

// AccountIterator creates an account iterator over a single diff layer.
// AccountIteratorは、単一のdiffレイヤー上にアカウントイテレーターを作成します。
func (dl *diffLayer) AccountIterator(seek common.Hash) AccountIterator {
	// Seek out the requested starting account
	// 要求された開始アカウントを探します
	hashes := dl.AccountList()
	index := sort.Search(len(hashes), func(i int) bool {
		return bytes.Compare(seek[:], hashes[i][:]) <= 0
	})
	// Assemble and returned the already seeked iterator
	// すでにシークされているイテレータをアセンブルして返します
	return &diffAccountIterator{
		layer: dl,
		keys:  hashes[index:],
	}
}

// Next steps the iterator forward one element, returning false if exhausted.
// 次のステップでは、イテレータが1つの要素を転送し、使い果たされた場合はfalseを返します。
func (it *diffAccountIterator) Next() bool {
	// If the iterator was already stale, consider it a programmer error. Although
	// we could just return false here, triggering this path would probably mean
	// somebody forgot to check for Error, so lets blow up instead of undefined
	// behavior that's hard to debug.
	//イテレータがすでに古くなっている場合は、プログラマーエラーと見なしてください。
	// ここでfalseを返すこともできますが、このパスをトリガーすると、
	// 誰かがエラーのチェックを忘れたことを意味する可能性があるため、
	// デバッグが難しい未定義の動作ではなく、爆発させましょう。
	if it.fail != nil {
		panic(fmt.Sprintf("called Next of failed iterator: %v", it.fail)) // 失敗したイテレータのNextと呼ばれる：％v
	}
	// Stop iterating if all keys were exhausted
	// すべてのキーが使い果たされた場合は反復を停止します
	if len(it.keys) == 0 {
		return false
	}
	if it.layer.Stale() {
		it.fail, it.keys = ErrSnapshotStale, nil
		return false
	}
	// Iterator seems to be still alive, retrieve and cache the live hash
	// イテレータはまだ生きているようです、ライブハッシュを取得してキャッシュします
	it.curHash = it.keys[0]
	// key cached, shift the iterator and notify the user of success
	// キーがキャッシュされ、イテレータをシフトして、成功をユーザーに通知します
	it.keys = it.keys[1:]
	return true
}

// Error returns any failure that occurred during iteration, which might have
// caused a premature iteration exit (e.g. snapshot stack becoming stale).
// エラーは、反復中に発生した障害を返します。
// これにより、反復が途中で終了する可能性があります（スナップショットスタックが古くなるなど）。
func (it *diffAccountIterator) Error() error {
	return it.fail
}

// Hash returns the hash of the account the iterator is currently at.
// Hashは、イテレータが現在存在するアカウントのハッシュを返します。
func (it *diffAccountIterator) Hash() common.Hash {
	return it.curHash
}

// Account returns the RLP encoded slim account the iterator is currently at.
// This method may _fail_, if the underlying layer has been flattened between
// the call to Next and Account. That type of error will set it.Err.
// This method assumes that flattening does not delete elements from
// the accountdata mapping (writing nil into it is fine though), and will panic
// if elements have been deleted.
//
// Note the returned account is not a copy, please don't modify it.
// アカウントは、イテレータが現在使用しているRLPエンコードされたスリムアカウントを返します。
// Nextの呼び出しとAccountの呼び出しの間で基礎となるレイヤーがフラット化されている場合、
// このメソッドは_失敗_する可能性があります。そのタイプのエラーはそれを設定します。
// このメソッドは、フラット化によってアカウントデータマッピングから要素が削除されないことを前提としています
// （ただし、nilを書き込むことは問題ありません）。
// 要素が削除されると、パニックになります。
//
// 返されたアカウントはコピーではないことに注意してください。変更しないでください。
func (it *diffAccountIterator) Account() []byte {
	it.layer.lock.RLock()
	blob, ok := it.layer.accountData[it.curHash]
	if !ok {
		if _, ok := it.layer.destructSet[it.curHash]; ok {
			it.layer.lock.RUnlock()
			return nil
		}
		panic(fmt.Sprintf("iterator referenced non-existent account: %x", it.curHash)) // イテレータが存在しないアカウントを参照しました：％x
	}
	it.layer.lock.RUnlock()
	if it.layer.Stale() {
		it.fail, it.keys = ErrSnapshotStale, nil
	}
	return blob
}

// Release is a noop for diff account iterators as there are no held resources.
// 保持されているリソースがないため、リリースはdiffアカウントイテレータにとっては重要ではありません。
func (it *diffAccountIterator) Release() {}

// diskAccountIterator is an account iterator that steps over the live accounts
// contained within a disk layer.
// diskAccountIteratorは、ディスクレイヤー内に含まれる
// ライブアカウントをステップオーバーするアカウントイテレーターです。
type diskAccountIterator struct {
	layer *diskLayer
	it    ethdb.Iterator
}

// AccountIterator creates an account iterator over a disk layer.
// AccountIteratorは、ディスクレイヤー上にアカウントイテレーターを作成します。
func (dl *diskLayer) AccountIterator(seek common.Hash) AccountIterator {
	pos := common.TrimRightZeroes(seek[:])
	return &diskAccountIterator{
		layer: dl,
		it:    dl.diskdb.NewIterator(rawdb.SnapshotAccountPrefix, pos),
	}
}

// Next steps the iterator forward one element, returning false if exhausted.
// 次のステップでは、イテレータが1つの要素を転送し、使い果たされた場合はfalseを返します。
func (it *diskAccountIterator) Next() bool {
	// If the iterator was already exhausted, don't bother
	// イテレータがすでに使い果たされている場合は、気にしないでください
	if it.it == nil {
		return false
	}
	// Try to advance the iterator and release it if we reached the end
	// イテレータを進めて、最後に達した場合は解放してみてください
	for {
		if !it.it.Next() {
			it.it.Release()
			it.it = nil
			return false
		}
		if len(it.it.Key()) == len(rawdb.SnapshotAccountPrefix)+common.HashLength {
			break
		}
	}
	return true
}

// Error returns any failure that occurred during iteration, which might have
// caused a premature iteration exit (e.g. snapshot stack becoming stale).
//
// A diff layer is immutable after creation content wise and can always be fully
// iterated without error, so this method always returns nil.
// エラーは、反復中に発生した障害を返します。
// これにより、反復が途中で終了する可能性があります（スナップショットスタックが古くなるなど）。
//
// diffレイヤーは、コンテンツを作成した後は不変であり、
// エラーなしで常に完全に繰り返すことができるため、このメソッドは常にnilを返します。
func (it *diskAccountIterator) Error() error {
	if it.it == nil {
		return nil // イテレータが使い果たされて解放されました // Iterator is exhausted and released
	}
	return it.it.Error()
}

// Hash returns the hash of the account the iterator is currently at.
// Hashは、イテレータが現在存在するアカウントのハッシュを返します。
func (it *diskAccountIterator) Hash() common.Hash {
	return common.BytesToHash(it.it.Key()) // プレフィックスは切り捨てられます // The prefix will be truncated
}

// Account returns the RLP encoded slim account the iterator is currently at.
// アカウントは、イテレータが現在使用しているRLPエンコードされたスリムアカウントを返します。
func (it *diskAccountIterator) Account() []byte {
	return it.it.Value()
}

// Release releases the database snapshot held during iteration.
// リリースは、反復中に保持されたデータベーススナップショットをリリースします。
func (it *diskAccountIterator) Release() {
	// The iterator is auto-released on exhaustion, so make sure it's still alive
	// イテレータは使い果たされると自動解放されるので、まだ生きていることを確認してください
	if it.it != nil {
		it.it.Release()
		it.it = nil
	}
}

// diffStorageIterator is a storage iterator that steps over the specific storage
// (both live and deleted) contained within a single diff layer. Higher order
// iterators will use the deleted slot to skip deeper iterators.
// diffStorageIteratorは、単一のdiffレイヤー内に含まれる特定のストレージ
// （ライブと削除の両方）をステップオーバーするストレージイテレーターです。
// 高次のイテレータは、削除されたスロットを使用して、より深いイテレータをスキップします。
type diffStorageIterator struct {
	// curHash is the current hash the iterator is positioned on. The field is
	// explicitly tracked since the referenced diff layer might go stale after
	// the iterator was positioned and we don't want to fail accessing the old
	// hash as long as the iterator is not touched any more.
	// curHashは、イテレータが配置されている現在のハッシュです。
	// イテレータが配置された後に参照されるdiffレイヤーが古くなる可能性があるため、
	// フィールドは明示的に追跡されます。
	// イテレータに触れない限り、古いハッシュへのアクセスに失敗することはありません。
	curHash common.Hash
	account common.Hash

	layer *diffLayer    // 値を取得するライブレイヤー // Live layer to retrieve values from
	keys  []common.Hash // 反復するためにレイヤーに残されたキー // Keys left in the layer to iterate
	fail  error         // 発生した障害（失効） // Any failures encountered (stale)
}

// StorageIterator creates a storage iterator over a single diff layer.
// Except the storage iterator is returned, there is an additional flag
// "destructed" returned. If it's true then it means the whole storage is
// destructed in this layer(maybe recreated too), don't bother deeper layer
// for storage retrieval.

// StorageIteratorは、単一のdiffレイヤー上にストレージイテレーターを作成します。
// ストレージイテレータが返されることを除いて、「破棄された」追加のフラグが返されます。
// それが本当なら、それはストレージ全体がこのレイヤーで破壊されていることを意味します（おそらく再作成されます）。
// ストレージの取得のために深いレイヤーを気にしないでください。
func (dl *diffLayer) StorageIterator(account common.Hash, seek common.Hash) (StorageIterator, bool) {
	// Create the storage for this account even it's marked
	// as destructed. The iterator is for the new one which
	// just has the same address as the deleted one.
	// 破棄済みとしてマークされている場合でも、このアカウントのストレージを作成します。
	// イテレータは、削除されたアドレスと同じアドレスを持つ新しいイテレータ用です。
	hashes, destructed := dl.StorageList(account)
	index := sort.Search(len(hashes), func(i int) bool {
		return bytes.Compare(seek[:], hashes[i][:]) <= 0
	})
	// Assemble and returned the already seeked iterator
	// すでにシークされているイテレータをアセンブルして返します
	return &diffStorageIterator{
		layer:   dl,
		account: account,
		keys:    hashes[index:],
	}, destructed
}

// Next steps the iterator forward one element, returning false if exhausted.
// 次のステップでは、イテレータが1つの要素を転送し、使い果たされた場合はfalseを返します。
func (it *diffStorageIterator) Next() bool {
	// If the iterator was already stale, consider it a programmer error. Although
	// we could just return false here, triggering this path would probably mean
	// somebody forgot to check for Error, so lets blow up instead of undefined
	// behavior that's hard to debug.
	// イテレータがすでに古くなっている場合は、プログラマーエラーと見なしてください。
	// ここでfalseを返すこともできますが、このパスをトリガーすると、
	// 誰かがエラーのチェックを忘れたことを意味する可能性があるため、
	// デバッグが難しい未定義の動作ではなく、爆発させましょう。
	if it.fail != nil {
		panic(fmt.Sprintf("called Next of failed iterator: %v", it.fail)) // 失敗したイテレータのNextと呼ばれる：％v
	}
	// Stop iterating if all keys were exhausted
	// すべてのキーが使い果たされた場合は反復を停止します
	if len(it.keys) == 0 {
		return false
	}
	if it.layer.Stale() {
		it.fail, it.keys = ErrSnapshotStale, nil
		return false
	}
	// Iterator seems to be still alive, retrieve and cache the live hash
	// イテレータはまだ生きているようです、ライブハッシュを取得してキャッシュします
	it.curHash = it.keys[0]
	// key cached, shift the iterator and notify the user of success
	// キーがキャッシュされ、イテレータをシフトして、成功をユーザーに通知します
	it.keys = it.keys[1:]
	return true
}

// Error returns any failure that occurred during iteration, which might have
// caused a premature iteration exit (e.g. snapshot stack becoming stale).
// エラーは、反復中に発生した障害を返します。
// これにより、反復が途中で終了する可能性があります（スナップショットスタックが古くなるなど）。
func (it *diffStorageIterator) Error() error {
	return it.fail
}

// Hash returns the hash of the storage slot the iterator is currently at.
// ハッシュは、イテレータが現在存在するストレージスロットのハッシュを返します。
func (it *diffStorageIterator) Hash() common.Hash {
	return it.curHash
}

// Slot returns the raw storage slot value the iterator is currently at.
// This method may _fail_, if the underlying layer has been flattened between
// the call to Next and Value. That type of error will set it.Err.
// This method assumes that flattening does not delete elements from
// the storage mapping (writing nil into it is fine though), and will panic
// if elements have been deleted.
//
// Note the returned slot is not a copy, please don't modify it.
// Slotは、イテレータが現在使用しているrawストレージスロット値を返します。
// Nextの呼び出しとValueの呼び出しの間で基礎となるレイヤーがフラット化されている場合、
// このメソッドは_失敗_する可能性があります。
// そのタイプのエラーはそれを設定します。
// このメソッドは、フラット化によってストレージマッピングから要素が削除されないことを前提としています
// （ただし、nilを書き込むことは問題ありません）。
// 要素が削除されると、パニックになります。
//
// 返されたスロットはコピーではないことに注意してください。変更しないでください。
func (it *diffStorageIterator) Slot() []byte {
	it.layer.lock.RLock()
	storage, ok := it.layer.storageData[it.account]
	if !ok {
		panic(fmt.Sprintf("iterator referenced non-existent account storage: %x", it.account)) // イテレータが存在しないアカウントストレージを参照しました：％x
	}
	// Storage slot might be nil(deleted), but it must exist
	// ストレージスロットはnil（削除済み）である可能性がありますが、存在している必要があります
	blob, ok := storage[it.curHash]
	if !ok {
		panic(fmt.Sprintf("iterator referenced non-existent storage slot: %x", it.curHash)) // イテレータが存在しないストレージスロットを参照しました：％x
	}
	it.layer.lock.RUnlock()
	if it.layer.Stale() {
		it.fail, it.keys = ErrSnapshotStale, nil
	}
	return blob
}

// Release is a noop for diff account iterators as there are no held resources.
// 保持されているリソースがないため、リリースはdiffアカウントイテレータにとっては重要ではありません。
func (it *diffStorageIterator) Release() {}

// diskStorageIterator is a storage iterator that steps over the live storage
// contained within a disk layer.
// diskStorageIteratorは、ディスクレイヤー内に含まれるライブストレージをステップオーバーするストレージイテレーターです。
type diskStorageIterator struct {
	layer   *diskLayer
	account common.Hash
	it      ethdb.Iterator
}

// StorageIterator creates a storage iterator over a disk layer.
// If the whole storage is destructed, then all entries in the disk
// layer are deleted already. So the "destructed" flag returned here
// is always false.
// StorageIteratorは、ディスクレイヤー上にストレージイテレーターを作成します。
// ストレージ全体が破壊された場合、ディスクレイヤーのすべてのエントリはすでに削除されています。
// したがって、ここで返される「破棄された」フラグは常にfalseです。
func (dl *diskLayer) StorageIterator(account common.Hash, seek common.Hash) (StorageIterator, bool) {
	pos := common.TrimRightZeroes(seek[:])
	return &diskStorageIterator{
		layer:   dl,
		account: account,
		it:      dl.diskdb.NewIterator(append(rawdb.SnapshotStoragePrefix, account.Bytes()...), pos),
	}, false
}

// Next steps the iterator forward one element, returning false if exhausted.
// 次のステップでは、イテレータが1つの要素を転送し、使い果たされた場合はfalseを返します。
func (it *diskStorageIterator) Next() bool {
	// If the iterator was already exhausted, don't bother
	// イテレータがすでに使い果たされている場合は、気にしないでください
	if it.it == nil {
		return false
	}
	// Try to advance the iterator and release it if we reached the end
	// イテレータを進めて、最後に達した場合は解放してみてください
	for {
		if !it.it.Next() {
			it.it.Release()
			it.it = nil
			return false
		}
		if len(it.it.Key()) == len(rawdb.SnapshotStoragePrefix)+common.HashLength+common.HashLength {
			break
		}
	}
	return true
}

// Error returns any failure that occurred during iteration, which might have
// caused a premature iteration exit (e.g. snapshot stack becoming stale).
//
// A diff layer is immutable after creation content wise and can always be fully
// iterated without error, so this method always returns nil.
// エラーは、反復中に発生した障害を返します。
// これにより、反復が途中で終了する可能性があります（スナップショットスタックが古くなるなど）。
//
// diffレイヤーは、コンテンツを作成した後は不変であり、
// エラーなしで常に完全に繰り返すことができるため、このメソッドは常にnilを返します。
func (it *diskStorageIterator) Error() error {
	if it.it == nil {
		return nil // イテレータが使い果たされて解放されました // Iterator is exhausted and released
	}
	return it.it.Error()
}

// Hash returns the hash of the storage slot the iterator is currently at.
// ハッシュは、イテレータが現在存在するストレージスロットのハッシュを返します。
func (it *diskStorageIterator) Hash() common.Hash {
	return common.BytesToHash(it.it.Key()) // プレフィックスは切り捨てられます // The prefix will be truncated
}

// Slot returns the raw storage slot content the iterator is currently at.
// Slotは、イテレータが現在使用しているrawストレージスロットのコンテンツを返します。
func (it *diskStorageIterator) Slot() []byte {
	return it.it.Value()
}

// Release releases the database snapshot held during iteration.
//リリースは、反復中に保持されたデータベーススナップショットをリリースします。
func (it *diskStorageIterator) Release() {
	// The iterator is auto-released on exhaustion, so make sure it's still alive
	// イテレータは使い果たされると自動解放されるので、まだ生きていることを確認してください
	if it.it != nil {
		it.it.Release()
		it.it = nil
	}
}
