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
)

// weightedIterator is a iterator with an assigned weight. It is used to prioritise
// which account or storage slot is the correct one if multiple iterators find the
// same one (modified in multiple consecutive blocks).
// weightedIteratorは、重みが割り当てられたイテレータです。
// 複数のイテレータが同じものを見つけた場合（複数の連続したブロックで変更された場合）、
// どのアカウントまたはストレージスロットが正しいかを優先するために使用されます。
type weightedIterator struct {
	it       Iterator
	priority int
}

// weightedIterators is a set of iterators implementing the sort.Interface.
// weightedIteratorsは、sort.Interfaceを実装するイテレータのセットです。
type weightedIterators []*weightedIterator

// Len implements sort.Interface, returning the number of active iterators.
// Lenはsort.Interfaceを実装し、アクティブなイテレータの数を返します。
func (its weightedIterators) Len() int { return len(its) }

// Less implements sort.Interface, returning which of two iterators in the stack
// is before the other.
// Lessはsort.Interfaceを実装し、スタック内の2つのイテレータのどちらが他のイテレータよりも前にあるかを返します。
func (its weightedIterators) Less(i, j int) bool {
	// Order the iterators primarily by the account hashes
	// 主にアカウントハッシュでイテレータを並べ替えます
	hashI := its[i].it.Hash()
	hashJ := its[j].it.Hash()

	switch bytes.Compare(hashI[:], hashJ[:]) {
	case -1:
		return true
	case 1:
		return false
	}
	// Same account/storage-slot in multiple layers, split by priority
	// 同じアカウント/ストレージ-複数のレイヤーのスロット、優先度で分割
	return its[i].priority < its[j].priority
}

// Swap implements sort.Interface, swapping two entries in the iterator stack.
// Swapはsort.Interfaceを実装し、イテレータスタックの2つのエントリを交換します。
func (its weightedIterators) Swap(i, j int) {
	its[i], its[j] = its[j], its[i]
}

// fastIterator is a more optimized multi-layer iterator which maintains a
// direct mapping of all iterators leading down to the bottom layer.
// fastIteratorは、より最適化されたマルチレイヤーイテレーターであり、最下層に至るすべてのイテレーターの直接マッピングを維持します。
type fastIterator struct {
	tree *Tree       // 古いサブイテレータを再初期化するスナップショットツリー // Snapshot tree to reinitialize stale sub-iterators with
	root common.Hash // ルートハッシュを使用して、古いサブイテレータを再初期化します。 // Root hash to reinitialize stale sub-iterators through

	curAccount []byte
	curSlot    []byte

	iterators weightedIterators
	initiated bool
	account   bool
	fail      error
}

// newFastIterator creates a new hierarchical account or storage iterator with one
// element per diff layer. The returned combo iterator can be used to walk over
// the entire snapshot diff stack simultaneously.

// newFastIteratorは、diffレイヤーごとに1つの要素を持つ新しい階層アカウントまたはストレージイテレーターを作成します。
// 返されたコンボイテレータを使用して、スナップショット差分スタック全体を同時にウォークオーバーできます。
func newFastIterator(tree *Tree, root common.Hash, account common.Hash, seek common.Hash, accountIterator bool) (*fastIterator, error) {
	snap := tree.Snapshot(root)
	if snap == nil {
		return nil, fmt.Errorf("unknown snapshot: %x", root) // 不明なスナップショット：％x
	}
	fi := &fastIterator{
		tree:    tree,
		root:    root,
		account: accountIterator,
	}
	current := snap.(snapshot)
	for depth := 0; current != nil; depth++ {
		if accountIterator {
			fi.iterators = append(fi.iterators, &weightedIterator{
				it:       current.AccountIterator(seek),
				priority: depth,
			})
		} else {
			// If the whole storage is destructed in this layer, don't
			// bother deeper layer anymore. But we should still keep
			// the iterator for this layer, since the iterator can contain
			// some valid slots which belongs to the re-created account.
			// ストレージ全体がこのレイヤーで破壊された場合、もう深いレイヤーを気にしないでください。
			// ただし、イテレーターには再作成されたアカウントに属するいくつかの有効なスロットを含めることができるため、
			// このレイヤーのイテレーターを保持する必要があります。
			it, destructed := current.StorageIterator(account, seek)
			fi.iterators = append(fi.iterators, &weightedIterator{
				it:       it,
				priority: depth,
			})
			if destructed {
				break
			}
		}
		current = current.Parent()
	}
	fi.init()
	return fi, nil
}

// init walks over all the iterators and resolves any clashes between them, after
// which it prepares the stack for step-by-step iteration.
// initはすべてのイテレータをウォークオーバーし、それらの間の衝突を解決します。
// その後、段階的な反復のためにスタックを準備します。
func (fi *fastIterator) init() {
	// Track which account hashes are iterators positioned on
	// どのアカウントハッシュがイテレータに配置されているかを追跡します
	var positioned = make(map[common.Hash]int)

	// Position all iterators and track how many remain live
	// すべてのイテレータを配置し、残りのイテレータの数を追跡します
	for i := 0; i < len(fi.iterators); i++ {
		// Retrieve the first element and if it clashes with a previous iterator,
		// advance either the current one or the old one. Repeat until nothing is
		// clashing any more.
		// 最初の要素を取得し、それが前のイテレータと衝突する場合は、
		// 現在のイテレータまたは古いイテレータのいずれかを進めます。
		// 何も衝突しなくなるまで繰り返します。
		it := fi.iterators[i]
		for {
			// If the iterator is exhausted, drop it off the end
			// イテレータが使い果たされた場合は、最後から削除します
			if !it.it.Next() {
				it.it.Release()
				last := len(fi.iterators) - 1

				fi.iterators[i] = fi.iterators[last]
				fi.iterators[last] = nil
				fi.iterators = fi.iterators[:last]

				i--
				break
			}
			// The iterator is still alive, check for collisions with previous ones
			// イテレータはまだ生きています、前のイテレータとの衝突をチェックしてください
			hash := it.it.Hash()
			if other, exist := positioned[hash]; !exist {
				positioned[hash] = i
				break
			} else {
				// Iterators collide, one needs to be progressed, use priority to
				// determine which.
				//
				// This whole else-block can be avoided, if we instead
				// do an initial priority-sort of the iterators. If we do that,
				// then we'll only wind up here if a lower-priority (preferred) iterator
				// has the same value, and then we will always just continue.
				// However, it costs an extra sort, so it's probably not better
				//イテレータが衝突し、進行する必要があります。優先度を使用してどちらを決定します。
				//
				// 代わりに初期優先順位を実行する場合、このelse-block全体を回避できます-イテレータのソート。
				// そうすると、優先度の低い（推奨される）イテレータの値が同じである場合にのみここに到達し、常に続行します。
				// ただし、追加の並べ替えが必要になるため、おそらく良くありません
				if fi.iterators[other].priority < it.priority {
					// The 'it' should be progressed
					//「それ」は進行する必要があります
					continue
				} else {
					// The 'other' should be progressed, swap them
					// 'other'は進行する必要があり、それらを交換します
					it = fi.iterators[other]
					fi.iterators[other], fi.iterators[i] = fi.iterators[i], fi.iterators[other]
					continue
				}
			}
		}
	}
	// Re-sort the entire list
	// リスト全体を並べ替えます
	sort.Sort(fi.iterators)
	fi.initiated = false
}

// Next steps the iterator forward one element, returning false if exhausted.
// 次のステップでは、イテレータが1つの要素を転送し、使い果たされた場合はfalseを返します。
func (fi *fastIterator) Next() bool {
	if len(fi.iterators) == 0 {
		return false
	}
	if !fi.initiated {
		// Don't forward first time -- we had to 'Next' once in order to
		// do the sorting already
		// 最初は転送しないでください-すでに並べ替えを行うには、「次へ」を1回実行する必要がありました
		fi.initiated = true
		if fi.account {
			fi.curAccount = fi.iterators[0].it.(AccountIterator).Account()
		} else {
			fi.curSlot = fi.iterators[0].it.(StorageIterator).Slot()
		}
		if innerErr := fi.iterators[0].it.Error(); innerErr != nil {
			fi.fail = innerErr
			return false
		}
		if fi.curAccount != nil || fi.curSlot != nil {
			return true
		}
		// Implicit else: we've hit a nil-account or nil-slot, and need to
		// fall through to the loop below to land on something non-nil
		// 暗黙的else：nil-accountまたはnil-slotにヒットし、nil以外のものに到達するには、
		// 以下のループにフォールスルーする必要があります
	}
	// If an account or a slot is deleted in one of the layers, the key will
	// still be there, but the actual value will be nil. However, the iterator
	// should not export nil-values (but instead simply omit the key), so we
	// need to loop here until we either
	//  - get a non-nil value,
	//  - hit an error,
	//  - or exhaust the iterator
	// アカウントまたはスロットがいずれかのレイヤーで削除された場合、キーは引き続き存在しますが、
	// 実際の値はnilになります。ただし、イテレータはnil値をエクスポートするべきではないため
	// （代わりに単にキーを省略します）、次のいずれかになるまでここでループする必要があります。
	// -nil以外の値を取得し、
	// -エラーが発生しました
	// -またはイテレータを使い果たします
	for {
		if !fi.next(0) {
			return false // 疲れ果てた // exhausted
		}
		if fi.account {
			fi.curAccount = fi.iterators[0].it.(AccountIterator).Account()
		} else {
			fi.curSlot = fi.iterators[0].it.(StorageIterator).Slot()
		}
		if innerErr := fi.iterators[0].it.Error(); innerErr != nil {
			fi.fail = innerErr
			return false // error
		}
		if fi.curAccount != nil || fi.curSlot != nil {
			break // nil以外の値が見つかりました // non-nil value found
		}
	}
	return true
}

// next handles the next operation internally and should be invoked when we know
// that two elements in the list may have the same value.
//
// For example, if the iterated hashes become [2,3,5,5,8,9,10], then we should
// invoke next(3), which will call Next on elem 3 (the second '5') and will
// cascade along the list, applying the same operation if needed.

// nextは次の操作を内部で処理し、リスト内の2つの要素が同じ値を持つ可能性があることがわかっている場合に呼び出す必要があります。
//
// たとえば、繰り返されるハッシュが[2,3,5,5,8,9,10]になった場合、next（3）を呼び出す必要があります。
// これにより、elem 3（2番目の「5」）でNextが呼び出されます。
// リストに沿ってカスケードし、必要に応じて同じ操作を適用します。
func (fi *fastIterator) next(idx int) bool {
	// If this particular iterator got exhausted, remove it and return true (the
	// next one is surely not exhausted yet, otherwise it would have been removed
	// already).
	// この特定のイテレータが使い果たされた場合は、それを削除してtrueを返します
	// （次のイテレータはまだ使い果たされていません。
	// そうでない場合は、すでに削除されています）。
	if it := fi.iterators[idx].it; !it.Next() {
		it.Release()

		fi.iterators = append(fi.iterators[:idx], fi.iterators[idx+1:]...)
		return len(fi.iterators) > 0
	}
	// If there's no one left to cascade into, return
	// カスケードする人が残っていない場合は、
	if idx == len(fi.iterators)-1 {
		return true
	}
	// We next-ed the iterator at 'idx', now we may have to re-sort that element
	// 次に'idx'でイテレータを編集しましたが、その要素を再ソートする必要があるかもしれません
	var (
		cur, next         = fi.iterators[idx], fi.iterators[idx+1]
		curHash, nextHash = cur.it.Hash(), next.it.Hash()
	)
	if diff := bytes.Compare(curHash[:], nextHash[:]); diff < 0 {
		// It is still in correct place
		// まだ正しい場所にあります
		return true
	} else if diff == 0 && cur.priority < next.priority {
		// So still in correct place, but we need to iterate on the next
		// まだ正しい場所にありますが、次の場所で繰り返す必要があります
		fi.next(idx + 1)
		return true
	}
	// At this point, the iterator is in the wrong location, but the remaining
	// list is sorted. Find out where to move the item.
	// この時点で、イテレータは間違った場所にありますが、残りのリストはソートされています。
	// アイテムを移動する場所を見つけます。
	clash := -1
	index := sort.Search(len(fi.iterators), func(n int) bool {
		// The iterator always advances forward, so anything before the old slot
		// is known to be behind us, so just skip them altogether. This actually
		// is an important clause since the sort order got invalidated.
		//イテレータは常に前方に進むので、古いスロットの前にあるものはすべて私たちの後ろにあることがわかっているので、
		// それらを完全にスキップします。
		// ソート順が無効になったため、これは実際には重要な句です。
		if n < idx {
			return false
		}
		if n == len(fi.iterators)-1 {
			// Can always place an elem last
			// 常に要素を最後に配置できます
			return true
		}
		nextHash := fi.iterators[n+1].it.Hash()
		if diff := bytes.Compare(curHash[:], nextHash[:]); diff < 0 {
			return true
		} else if diff > 0 {
			return false
		}
		// The elem we're placing it next to has the same value,
		// so whichever winds up on n+1 will need further iteraton
		// 隣に配置する要素は同じ値であるため、n+1で終わる方はさらに反復する必要があります
		clash = n + 1

		return cur.priority < fi.iterators[n+1].priority
	})
	fi.move(idx, index)
	if clash != -1 {
		fi.next(clash)
	}
	return true
}

// move advances an iterator to another position in the list.
// 移動すると、イテレータがリスト内の別の位置に進みます。
func (fi *fastIterator) move(index, newpos int) {
	elem := fi.iterators[index]
	copy(fi.iterators[index:], fi.iterators[index+1:newpos+1])
	fi.iterators[newpos] = elem
}

// Error returns any failure that occurred during iteration, which might have
// caused a premature iteration exit (e.g. snapshot stack becoming stale).
// エラーは、反復中に発生した障害を返します。
// これにより、反復が途中で終了する可能性があります（スナップショットスタックが古くなるなど）。
func (fi *fastIterator) Error() error {
	return fi.fail
}

// Hash returns the current key
// ハッシュは現在のキーを返します
func (fi *fastIterator) Hash() common.Hash {
	return fi.iterators[0].it.Hash()
}

// Account returns the current account blob.
// Note the returned account is not a copy, please don't modify it.
// アカウントは現在のアカウントBLOBを返します。
// 返されたアカウントはコピーではないことに注意してください。変更しないでください。
func (fi *fastIterator) Account() []byte {
	return fi.curAccount
}

// Slot returns the current storage slot.
// Note the returned slot is not a copy, please don't modify it.
// Slotは現在のストレージスロットを返します。
// 返されたスロットはコピーではないことに注意してください。変更しないでください。
func (fi *fastIterator) Slot() []byte {
	return fi.curSlot
}

// Release iterates over all the remaining live layer iterators and releases each
// of thme individually.
// Releaseは、残りのすべてのライブレイヤーイテレーターを反復処理し、各thmeを個別にリリースします。
func (fi *fastIterator) Release() {
	for _, it := range fi.iterators {
		it.it.Release()
	}
	fi.iterators = nil
}

// Debug is a convencience helper during testing
// デバッグはテスト中のコンビニエンスヘルパーです
func (fi *fastIterator) Debug() {
	for _, it := range fi.iterators {
		fmt.Printf("[p=%v v=%v] ", it.priority, it.it.Hash()[0])
	}
	fmt.Println()
}

// newFastAccountIterator creates a new hierarchical account iterator with one
// element per diff layer. The returned combo iterator can be used to walk over
// the entire snapshot diff stack simultaneously.
// newFastAccountIteratorは、diffレイヤーごとに1つの要素を持つ新しい階層アカウントイテレーターを作成します。
// 返されたコンボイテレータを使用して、スナップショット差分スタック全体を同時にウォークオーバーできます。
func newFastAccountIterator(tree *Tree, root common.Hash, seek common.Hash) (AccountIterator, error) {
	return newFastIterator(tree, root, common.Hash{}, seek, true)
}

// newFastStorageIterator creates a new hierarchical storage iterator with one
// element per diff layer. The returned combo iterator can be used to walk over
// the entire snapshot diff stack simultaneously.
// newFastStorageIteratorは、diffレイヤーごとに1つの要素を持つ新しい階層ストレージイテレーターを作成します。
// 返されたコンボイテレータを使用して、スナップショット差分スタック全体を同時にウォークオーバーできます。
func newFastStorageIterator(tree *Tree, root common.Hash, account common.Hash, seek common.Hash) (StorageIterator, error) {
	return newFastIterator(tree, root, account, seek, false)
}
