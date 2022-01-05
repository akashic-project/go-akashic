// Copyright 2016 The go-ethereum Authors
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

package core

import (
	"container/heap"
	"math"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// nonceHeap is a heap.Interface implementation over 64bit unsigned integers for
// retrieving sorted transactions from the possibly gapped future queue.
// nonceHeapは、ギャップのある可能性のある将来のキューからソートされたトランザクションを取得するための
// 64ビット符号なし整数を介したheap.Interface実装です。
type nonceHeap []uint64

func (h nonceHeap) Len() int           { return len(h) }
func (h nonceHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h nonceHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *nonceHeap) Push(x interface{}) {
	*h = append(*h, x.(uint64))
}

func (h *nonceHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// txSortedMap is a nonce->transaction hash map with a heap based index to allow
// iterating over the contents in a nonce-incrementing way.
// txSortedMapは、ヒープベースのインデックスを備えたnonce-> transactionハッシュマップであり、
// nonceインクリメント方式でコンテンツを反復処理できます。
type txSortedMap struct {
	items map[uint64]*types.Transaction // トランザクションデータを格納するハッシュマップ    // Hash map storing the transaction data
	index *nonceHeap                    // 保存されているすべてのトランザクションのナンスのヒープ（非厳密モード） // Heap of nonces of all the stored transactions (non-strict mode)
	cache types.Transactions            //すでに並べ替えられたトランザクションのキャッシュ   // Cache of the transactions already sorted
}

// newTxSortedMap creates a new nonce-sorted transaction map.
// newTxSortedMapは、新しいナンスソートされたトランザクションマップを作成します。
func newTxSortedMap() *txSortedMap {
	return &txSortedMap{
		items: make(map[uint64]*types.Transaction),
		index: new(nonceHeap),
	}
}

// Get retrieves the current transactions associated with the given nonce.
// Getは、指定されたナンスに関連付けられている現在のトランザクションを取得します。
func (m *txSortedMap) Get(nonce uint64) *types.Transaction {
	return m.items[nonce]
}

// Put inserts a new transaction into the map, also updating the map's nonce
// index. If a transaction already exists with the same nonce, it's overwritten.
// Putは新しいトランザクションをマップに挿入し、マップのナンスインデックスも更新します。
// 同じナンスを持つトランザクションがすでに存在する場合、それは上書きされます。
func (m *txSortedMap) Put(tx *types.Transaction) {
	nonce := tx.Nonce()
	if m.items[nonce] == nil {
		heap.Push(m.index, nonce)
	}
	m.items[nonce], m.cache = tx, nil
}

// Forward removes all transactions from the map with a nonce lower than the
// provided threshold. Every removed transaction is returned for any post-removal
// maintenance.
// Forwardは、指定されたしきい値よりも低いナンスを持つすべてのトランザクションをマップから削除します。
// 削除されたすべてのトランザクションは、削除後のメンテナンスのために返されます。
func (m *txSortedMap) Forward(threshold uint64) types.Transactions {
	var removed types.Transactions

	// Pop off heap items until the threshold is reached
	// しきい値に達するまでヒープアイテムをポップオフします
	for m.index.Len() > 0 && (*m.index)[0] < threshold {
		nonce := heap.Pop(m.index).(uint64)
		removed = append(removed, m.items[nonce])
		delete(m.items, nonce)
	}
	// If we had a cached order, shift the front
	// キャッシュされた注文がある場合は、前をシフトします
	if m.cache != nil {
		m.cache = m.cache[len(removed):]
	}
	return removed
}

// Filter iterates over the list of transactions and removes all of them for which
// the specified function evaluates to true.
// Filter, as opposed to 'filter', re-initialises the heap after the operation is done.
// If you want to do several consecutive filterings, it's therefore better to first
// do a .filter(func1) followed by .Filter(func2) or reheap()
// フィルタはトランザクションのリストを繰り返し処理し、指定された関数がtrueと評価したトランザクションをすべて削除します。
// 'filter'とは対照的に、Filterは、操作の完了後にヒープを再初期化します。
// 複数の連続したフィルタリングを実行する場合は、最初に.filter（func1）を実行し、
// 次に.Filter（func2）またはreheap（）を実行することをお勧めします。
func (m *txSortedMap) Filter(filter func(*types.Transaction) bool) types.Transactions {
	removed := m.filter(filter)
	// If transactions were removed, the heap and cache are ruined
	// トランザクションが削除された場合、ヒープとキャッシュは台無しになります
	if len(removed) > 0 {
		m.reheap()
	}
	return removed
}

func (m *txSortedMap) reheap() {
	*m.index = make([]uint64, 0, len(m.items))
	for nonce := range m.items {
		*m.index = append(*m.index, nonce)
	}
	heap.Init(m.index)
	m.cache = nil
}

// filter is identical to Filter, but **does not** regenerate the heap. This method
// should only be used if followed immediately by a call to Filter or reheap()
// filterはFilterと同じですが、ヒープを**再生成しません**。
// このメソッドは、直後にFilterまたはreheap（）を呼び出す場合にのみ使用してください。
func (m *txSortedMap) filter(filter func(*types.Transaction) bool) types.Transactions {
	var removed types.Transactions

	// Collect all the transactions to filter out
	// すべてのトランザクションを収集して除外します
	for nonce, tx := range m.items {
		if filter(tx) {
			removed = append(removed, tx)
			delete(m.items, nonce)
		}
	}
	if len(removed) > 0 {
		m.cache = nil
	}
	return removed
}

// Cap places a hard limit on the number of items, returning all transactions
// exceeding that limit.
// Capはアイテムの数に厳しい制限を課し、その制限を超えるすべてのトランザクションを返します。
func (m *txSortedMap) Cap(threshold int) types.Transactions {
	// Short circuit if the number of items is under the limit
	// アイテム数が制限を下回っている場合は短絡
	if len(m.items) <= threshold {
		return nil
	}
	// Otherwise gather and drop the highest nonce'd transactions
	// それ以外の場合は、最も高いナンストランザクションを収集して削除します
	var drops types.Transactions

	sort.Sort(*m.index)
	for size := len(m.items); size > threshold; size-- {
		drops = append(drops, m.items[(*m.index)[size-1]])
		delete(m.items, (*m.index)[size-1])
	}
	*m.index = (*m.index)[:threshold]
	heap.Init(m.index)

	// If we had a cache, shift the back
	// キャッシュがある場合は、後ろにシフトします
	if m.cache != nil {
		m.cache = m.cache[:len(m.cache)-len(drops)]
	}
	return drops
}

// Remove deletes a transaction from the maintained map, returning whether the
// transaction was found.
// 削除すると、維持されているマップからトランザクションが削除され、トランザクションが見つかったかどうかが返されます。
func (m *txSortedMap) Remove(nonce uint64) bool {
	// Short circuit if no transaction is present
	// トランザクションが存在しない場合の短絡
	_, ok := m.items[nonce]
	if !ok {
		return false
	}
	// Otherwise delete the transaction and fix the heap index
	// それ以外の場合は、トランザクションを削除してヒープインデックスを修正します
	for i := 0; i < m.index.Len(); i++ {
		if (*m.index)[i] == nonce {
			heap.Remove(m.index, i)
			break
		}
	}
	delete(m.items, nonce)
	m.cache = nil

	return true
}

// Ready retrieves a sequentially increasing list of transactions starting at the
// provided nonce that is ready for processing. The returned transactions will be
// removed from the list.
//
// Note, all transactions with nonces lower than start will also be returned to
// prevent getting into and invalid state. This is not something that should ever
// happen but better to be self correcting than failing!
// Readyは、処理の準備ができている、提供されたナンスから始まるトランザクションの順次増加するリストを取得します。
// 返されたトランザクションはリストから削除されます。
//
// 無効な状態になるのを防ぐために、開始よりも小さいナンスを持つすべてのトランザクションも返されることに注意してください。
// これは決して起こるべきことではありませんが、失敗するよりも自己修正する方が良いです！
func (m *txSortedMap) Ready(start uint64) types.Transactions {
	// Short circuit if no transactions are available
	// 利用可能なトランザクションがない場合は短絡
	if m.index.Len() == 0 || (*m.index)[0] > start {
		return nil
	}
	// Otherwise start accumulating incremental transactions
	// それ以外の場合は、増分トランザクションの蓄積を開始します
	var ready types.Transactions
	for next := (*m.index)[0]; m.index.Len() > 0 && (*m.index)[0] == next; next++ {
		ready = append(ready, m.items[next])
		delete(m.items, next)
		heap.Pop(m.index)
	}
	m.cache = nil

	return ready
}

// Len returns the length of the transaction map.
// Lenはトランザクションマップの長さを返します。
func (m *txSortedMap) Len() int {
	return len(m.items)
}

func (m *txSortedMap) flatten() types.Transactions {
	// If the sorting was not cached yet, create and cache it
	// 並べ替えがまだキャッシュされていない場合は、作成してキャッシュします
	if m.cache == nil {
		m.cache = make(types.Transactions, 0, len(m.items))
		for _, tx := range m.items {
			m.cache = append(m.cache, tx)
		}
		sort.Sort(types.TxByNonce(m.cache))
	}
	return m.cache
}

// Flatten creates a nonce-sorted slice of transactions based on the loosely
// sorted internal representation. The result of the sorting is cached in case
// it's requested again before any modifications are made to the contents.
// Flattenは、大まかにソートされた内部表現に基づいて、トランザクションのナンスソートされたスライスを作成します。
// ソートの結果は、万が一の場合に備えてキャッシュされます
// コンテンツに変更が加えられる前に、再度要求されます。
func (m *txSortedMap) Flatten() types.Transactions {
	// Copy the cache to prevent accidental modifications
	// 偶発的な変更を防ぐために、キャッシュをコピーします
	cache := m.flatten()
	txs := make(types.Transactions, len(cache))
	copy(txs, cache)
	return txs
}

// LastElement returns the last element of a flattened list, thus, the
// transaction with the highest nonce
// LastElementはフラット化されたリストの最後の要素を返すため、ナンスが最も高いトランザクション
func (m *txSortedMap) LastElement() *types.Transaction {
	cache := m.flatten()
	return cache[len(cache)-1]
}

// txList is a "list" of transactions belonging to an account, sorted by account
// nonce. The same type can be used both for storing contiguous transactions for
// the executable/pending queue; and for storing gapped transactions for the non-
// executable/future queue, with minor behavioral changes.
// txListは、アカウントに属するトランザクションの「リスト」であり、アカウントナンスでソートされています。
// 同じタイプを、実行可能/保留中のキューの連続したトランザクションの保存の両方に使用できます。
// 動作を少し変更して、実行不可能/将来のキューのギャップのあるトランザクションを保存します。
type txList struct {
	strict bool         // ナンスが厳密に連続であるかどうか                              // Whether nonces are strictly continuous or not
	txs    *txSortedMap // トランザクションのヒープインデックス付きソート済みハッシュマップ // Heap indexed sorted hash map of the transactions

	costcap *big.Int // 最もコストの高いトランザクションの価格（残高を超えた場合にのみリセット） // Price of the highest costing transaction (reset only if exceeds balance)
	gascap  uint64   //最も支出の多いトランザクションのガス制限（ブロック制限を超えた場合にのみリセット） // Gas limit of the highest spending transaction (reset only if exceeds block limit)
}

// newTxList create a new transaction list for maintaining nonce-indexable fast,
// gapped, sortable transaction lists.
// newTxListは、nonce-indexableの高速でギャップのある、
// ソート可能なトランザクションリストを維持するための新しいトランザクションリストを作成します。
func newTxList(strict bool) *txList {
	return &txList{
		strict:  strict,
		txs:     newTxSortedMap(),
		costcap: new(big.Int),
	}
}

// Overlaps returns whether the transaction specified has the same nonce as one
// already contained within the list.
// Overlapsは、指定されたトランザクションがすでにリストに含まれているものと同じナンスを持っているかどうかを返します。
func (l *txList) Overlaps(tx *types.Transaction) bool {
	return l.txs.Get(tx.Nonce()) != nil
}

// Add tries to insert a new transaction into the list, returning whether the
// transaction was accepted, and if yes, any previous transaction it replaced.
//
// If the new transaction is accepted into the list, the lists' cost and gas
// thresholds are also potentially updated.
// Addは、新しいトランザクションをリストに挿入しようとし、トランザクションが受け入れられたかどうかを返します。
// 受け入れられた場合は、以前のトランザクションが置き換えられました。
//
// 新しいトランザクションがリストに受け入れられると、リストのコストとガスのしきい値も更新される可能性があります。
func (l *txList) Add(tx *types.Transaction, priceBump uint64) (bool, *types.Transaction) {
	// If there's an older better transaction, abort
	// より古いより良いトランザクションがある場合は、中止します
	old := l.txs.Get(tx.Nonce())
	if old != nil {
		if old.GasFeeCapCmp(tx) >= 0 || old.GasTipCapCmp(tx) >= 0 {
			return false, nil
		}
		// thresholdFeeCap = oldFC  * (100 + priceBump) / 100
		a := big.NewInt(100 + int64(priceBump))
		aFeeCap := new(big.Int).Mul(a, old.GasFeeCap())
		aTip := a.Mul(a, old.GasTipCap())

		// thresholdTip    = oldTip * (100 + priceBump) / 100
		b := big.NewInt(100)
		thresholdFeeCap := aFeeCap.Div(aFeeCap, b)
		thresholdTip := aTip.Div(aTip, b)

		// We have to ensure that both the new fee cap and tip are higher than the
		// old ones as well as checking the percentage threshold to ensure that
		// this is accurate for low (Wei-level) gas price replacements.
		// 新しい料金上限とチップの両方が古いものよりも高いことを確認する必要があります。
		// また、パーセンテージのしきい値をチェックして、
		// これが低（Weiレベル）のガス価格の交換に対して正確であることを確認する必要があります。
		if tx.GasFeeCapIntCmp(thresholdFeeCap) < 0 || tx.GasTipCapIntCmp(thresholdTip) < 0 {
			return false, nil
		}
	}
	// Otherwise overwrite the old transaction with the current one
	// それ以外の場合は、古いトランザクションを現在のトランザクションで上書きします
	l.txs.Put(tx)
	if cost := tx.Cost(); l.costcap.Cmp(cost) < 0 {
		l.costcap = cost
	}
	if gas := tx.Gas(); l.gascap < gas {
		l.gascap = gas
	}
	return true, old
}

// Forward removes all transactions from the list with a nonce lower than the
// provided threshold. Every removed transaction is returned for any post-removal
// maintenance.
// Forwardは、指定されたしきい値よりも低いナンスを持つすべてのトランザクションをリストから削除します。
// 削除されたすべてのトランザクションは、削除後のメンテナンスのために返されます。
func (l *txList) Forward(threshold uint64) types.Transactions {
	return l.txs.Forward(threshold)
}

// Filter removes all transactions from the list with a cost or gas limit higher
// than the provided thresholds. Every removed transaction is returned for any
// post-removal maintenance. Strict-mode invalidated transactions are also
// returned.
//
// This method uses the cached costcap and gascap to quickly decide if there's even
// a point in calculating all the costs or if the balance covers all. If the threshold
// is lower than the costgas cap, the caps will be reset to a new high after removing
// the newly invalidated transactions.
// フィルタは、提供されたしきい値よりも高いコストまたはガス制限を持つすべてのトランザクションをリストから削除します。
// 削除されたすべてのトランザクションは、削除後のメンテナンスのために返されます。
// 厳密モードの無効化されたトランザクションも返されます。
//
// このメソッドは、キャッシュされたコストキャップとガスキャップを使用して、
// すべてのコストを計算することにポイントがあるかどうか、またはバランスがすべてをカバーするかどうかをすばやく判断します。
// しきい値がコストガスの上限よりも低い場合、
// 新しく無効にされたトランザクションを削除した後、上限は新しい上限にリセットされます。
func (l *txList) Filter(costLimit *big.Int, gasLimit uint64) (types.Transactions, types.Transactions) {
	// If all transactions are below the threshold, short circuit
	// すべてのトランザクションがしきい値を下回っている場合は、短絡します
	if l.costcap.Cmp(costLimit) <= 0 && l.gascap <= gasLimit {
		return nil, nil
	}
	l.costcap = new(big.Int).Set(costLimit) // 上限をしきい値まで下げます // Lower the caps to the thresholds
	l.gascap = gasLimit

	// Filter out all the transactions above the account's funds
	// アカウントの資金を超えるすべてのトランザクションを除外します
	removed := l.txs.Filter(func(tx *types.Transaction) bool {
		return tx.Gas() > gasLimit || tx.Cost().Cmp(costLimit) > 0
	})

	if len(removed) == 0 {
		return nil, nil
	}
	var invalids types.Transactions
	// If the list was strict, filter anything above the lowest nonce
	// リストが厳密な場合は、最も低いナンスより上のものをフィルタリングします
	if l.strict {
		lowest := uint64(math.MaxUint64)
		for _, tx := range removed {
			if nonce := tx.Nonce(); lowest > nonce {
				lowest = nonce
			}
		}
		invalids = l.txs.filter(func(tx *types.Transaction) bool { return tx.Nonce() > lowest })
	}
	l.txs.reheap()
	return removed, invalids
}

// Cap places a hard limit on the number of items, returning all transactions
// exceeding that limit.
// Capはアイテムの数に厳しい制限を課し、その制限を超えるすべてのトランザクションを返します。
func (l *txList) Cap(threshold int) types.Transactions {
	return l.txs.Cap(threshold)
}

// Remove deletes a transaction from the maintained list, returning whether the
// transaction was found, and also returning any transaction invalidated due to
// the deletion (strict mode only).
// 削除は、維持されているリストからトランザクションを削除し、トランザクションが見つかったかどうかを返します。
// また、削除のために無効になったトランザクションも返します（厳密モードのみ）。
func (l *txList) Remove(tx *types.Transaction) (bool, types.Transactions) {
	// Remove the transaction from the set
	// セットからトランザクションを削除します
	nonce := tx.Nonce()
	if removed := l.txs.Remove(nonce); !removed {
		return false, nil
	}
	// In strict mode, filter out non-executable transactions
	// 厳密モードでは、実行不可能なトランザクションを除外します
	if l.strict {
		return true, l.txs.Filter(func(tx *types.Transaction) bool { return tx.Nonce() > nonce })
	}
	return true, nil
}

// Ready retrieves a sequentially increasing list of transactions starting at the
// provided nonce that is ready for processing. The returned transactions will be
// removed from the list.
//
// Note, all transactions with nonces lower than start will also be returned to
// prevent getting into and invalid state. This is not something that should ever
// happen but better to be self correcting than failing!
// Readyは、処理の準備ができている、提供されたナンスから始まるトランザクションの順次増加するリストを取得します。
// 返されたトランザクションはリストから削除されます。
//
// 無効な状態になるのを防ぐために、開始よりも小さいナンスを持つすべてのトランザクションも返されることに注意してください。
// これは決して起こるべきことではありませんが、失敗するよりも自己修正する方が良いです！
func (l *txList) Ready(start uint64) types.Transactions {
	return l.txs.Ready(start)
}

// Len returns the length of the transaction list.
// Lenはトランザクションリストの長さを返します。
func (l *txList) Len() int {
	return l.txs.Len()
}

// Empty returns whether the list of transactions is empty or not.
// Emptyは、トランザクションのリストが空かどうかを返します。
func (l *txList) Empty() bool {
	return l.Len() == 0
}

// Flatten creates a nonce-sorted slice of transactions based on the loosely
// sorted internal representation. The result of the sorting is cached in case
// it's requested again before any modifications are made to the contents.
// Flattenは、大まかにソートされた内部表現に基づいて、トランザクションのナンスソートされたスライスを作成します。
// ソートの結果は、コンテンツに変更が加えられる前に再度要求された場合に備えてキャッシュされます。
func (l *txList) Flatten() types.Transactions {
	return l.txs.Flatten()
}

// LastElement returns the last element of a flattened list, thus, the
// transaction with the highest nonce
// LastElementはフラット化されたリストの最後の要素を返すため、ナンスが最も高いトランザクション
func (l *txList) LastElement() *types.Transaction {
	return l.txs.LastElement()
}

// priceHeap is a heap.Interface implementation over transactions for retrieving
// price-sorted transactions to discard when the pool fills up. If baseFee is set
// then the heap is sorted based on the effective tip based on the given base fee.
// If baseFee is nil then the sorting is based on gasFeeCap.
// priceHeapは、プールがいっぱいになったときに破棄する価格でソートされたトランザクションを取得するための、
// トランザクションに対するheap.Interface実装です。
// baseFeeが設定されている場合、ヒープは、指定された基本料金に基づく有効なチップに基づいてソートされます。
// baseFeeがnilの場合、並べ替えはgasFeeCapに基づいています。
type priceHeap struct {
	baseFee *big.Int // baseFeeが変更された後、ヒープは常に再ソートする必要があります // heap should always be re-sorted after baseFee is changed
	list    []*types.Transaction
}

func (h *priceHeap) Len() int      { return len(h.list) }
func (h *priceHeap) Swap(i, j int) { h.list[i], h.list[j] = h.list[j], h.list[i] }

func (h *priceHeap) Less(i, j int) bool {
	switch h.cmp(h.list[i], h.list[j]) {
	case -1:
		return true
	case 1:
		return false
	default:
		return h.list[i].Nonce() > h.list[j].Nonce()
	}
}

func (h *priceHeap) cmp(a, b *types.Transaction) int {
	if h.baseFee != nil {
		// baseFeeが指定されている場合、効果的なヒントを比較します // Compare effective tips if baseFee is specified
		if c := a.EffectiveGasTipCmp(b, h.baseFee); c != 0 {
			return c
		}
	}
	// Compare fee caps if baseFee is not specified or effective tips are equal
	// baseFeeが指定されていない場合、または有効なヒントが等しい場合は、料金の上限を比較します
	if c := a.GasFeeCapCmp(b); c != 0 {
		return c
	}
	// Compare tips if effective tips and fee caps are equal
	// 効果的なヒントと料金の上限が等しい場合はヒントを比較します
	return a.GasTipCapCmp(b)
}

func (h *priceHeap) Push(x interface{}) {
	tx := x.(*types.Transaction)
	h.list = append(h.list, tx)
}

func (h *priceHeap) Pop() interface{} {
	old := h.list
	n := len(old)
	x := old[n-1]
	old[n-1] = nil
	h.list = old[0 : n-1]
	return x
}

// txPricedList is a price-sorted heap to allow operating on transactions pool
// contents in a price-incrementing way. It's built opon the all transactions
// in txpool but only interested in the remote part. It means only remote transactions
// will be considered for tracking, sorting, eviction, etc.
//
// Two heaps are used for sorting: the urgent heap (based on effective tip in the next
// block) and the floating heap (based on gasFeeCap). Always the bigger heap is chosen for
// eviction. Transactions evicted from the urgent heap are first demoted into the floating heap.
// In some cases (during a congestion, when blocks are full) the urgent heap can provide
// better candidates for inclusion while in other cases (at the top of the baseFee peak)
// the floating heap is better. When baseFee is decreasing they behave similarly.
// txPricedListは価格でソートされたヒープであり、トランザクションプールのコンテンツを価格を上げて操作できるようにします。
// これは、txpool内のすべてのトランザクションで構築されていますが、リモート部分にのみ関心があります。
// これは、追跡、並べ替え、削除などの対象となるのはリモートトランザクションのみであることを意味します。
//
// 並べ替えには、緊急ヒープ（次のブロックの有効なチップに基づく）とフローティングヒープ（gasFeeCapに基づく）の
// 2つのヒープが使用されます。 常に、より大きなヒープが立ち退きのために選択されます。
// 緊急ヒープから削除されたトランザクションは、最初にフローティングヒープに降格されます。
// 場合によっては（混雑時、ブロックがいっぱいのとき）、緊急ヒープは、他の場合（baseFeeピークの上部）に
// 含めるためのより良い候補を提供できます。
// フローティングヒープの方が優れています。 baseFeeが減少しているときは、同じように動作します。
type txPricedList struct {
	// Number of stale price points to (re-heap trigger).
	// This field is accessed atomically, and must be the first field
	// to ensure it has correct alignment for atomic.AddInt64.
	// See https://golang.org/pkg/sync/atomic/#pkg-note-BUG.
	// 古い価格ポイントの数（再ヒープトリガー）。
	// このフィールドはアトミックにアクセスされ、最初のフィールドである必要があります
	// atomic.AddInt64に対して正しい配置になっていることを確認します。
	// See https://golang.org/pkg/sync/atomic/#pkg-note-BUG.
	stales int64

	all              *txLookup  // すべてのトランザクションのマップへのポインタ // Pointer to the map of all transactions
	urgent, floating priceHeap  // 保存されているすべての**リモート**トランザクションの価格のヒープ // Heaps of prices of all the stored **remote** transactions
	reheapMu         sync.Mutex // Mutexは、1つのルーチンのみがリストを再ヒープしていると主張します // Mutex asserts that only one routine is reheaping the list
}

const (
	// urgentRatio : floatingRatio is the capacity ratio of the two queues
	// urgentRatio：floatingRatioは、2つのキューの容量比です
	urgentRatio   = 4
	floatingRatio = 1
)

// newTxPricedList creates a new price-sorted transaction heap.
// newTxPricedListは、価格でソートされた新しいトランザクションヒープを作成します。
func newTxPricedList(all *txLookup) *txPricedList {
	return &txPricedList{
		all: all,
	}
}

// Put inserts a new transaction into the heap.
// Putは、新しいトランザクションをヒープに挿入します。
func (l *txPricedList) Put(tx *types.Transaction, local bool) {
	if local {
		return
	}
	// Insert every new transaction to the urgent heap first; Discard will balance the heaps
	// すべての新しいトランザクションを最初に緊急ヒープに挿入します。 破棄するとヒープのバランスがとれます
	heap.Push(&l.urgent, tx)
}

// Removed notifies the prices transaction list that an old transaction dropped
// from the pool. The list will just keep a counter of stale objects and update
// the heap if a large enough ratio of transactions go stale.
// 削除は、古いトランザクションがプールから削除されたことを価格トランザクションリストに通知します。
// リストは、古くなったオブジェクトのカウンターを保持し、トランザクションの十分な比率が古くなった場合にヒープを更新します。
func (l *txPricedList) Removed(count int) {
	// Bump the stale counter, but exit if still too low (< 25%)
	// 失効したカウンターをバンプしますが、それでも低すぎる場合は終了します（<25％）
	stales := atomic.AddInt64(&l.stales, int64(count))
	if int(stales) <= (len(l.urgent.list)+len(l.floating.list))/4 {
		return
	}
	// Seems we've reached a critical number of stale transactions,
	// 失効したトランザクションのクリティカル数に達したようです、再ヒープします
	l.Reheap()
}

// Underpriced checks whether a transaction is cheaper than (or as cheap as) the
// lowest priced (remote) transaction currently being tracked.
// 低価格は、トランザクションが現在追跡されている最低価格の
// （リモート）トランザクションよりも安い（または同じくらい安い）かどうかをチェックします。
func (l *txPricedList) Underpriced(tx *types.Transaction) bool {
	// Note: with two queues, being underpriced is defined as being worse than the worst item
	// in all non-empty queues if there is any. If both queues are empty then nothing is underpriced.
	// 注：2つのキューがある場合、低価格であるとは、空でないすべてのキューにある場合は、最悪のアイテムよりも悪いと定義されます。
	// 両方のキューが空の場合、低価格のものはありません。
	return (l.underpricedFor(&l.urgent, tx) || len(l.urgent.list) == 0) &&
		(l.underpricedFor(&l.floating, tx) || len(l.floating.list) == 0) &&
		(len(l.urgent.list) != 0 || len(l.floating.list) != 0)
}

// underpricedFor checks whether a transaction is cheaper than (or as cheap as) the
// lowest priced (remote) transaction in the given heap.
// underpricedForは、トランザクションが指定されたヒープ内の最低価格の
//（リモート）トランザクションよりも安い（または同じくらい安い）かどうかをチェックします。
func (l *txPricedList) underpricedFor(h *priceHeap, tx *types.Transaction) bool {
	// Discard stale price points if found at the heap start
	// ヒープの開始時に見つかった場合、古い価格ポイントを破棄します
	for len(h.list) > 0 {
		head := h.list[0]
		if l.all.GetRemote(head.Hash()) == nil { // 削除または移行 // Removed or migrated
			atomic.AddInt64(&l.stales, -1)
			heap.Pop(h)
			continue
		}
		break
	}
	// Check if the transaction is underpriced or not
	// トランザクションが低価格かどうかを確認します
	if len(h.list) == 0 {
		return false //リモートトランザクションはまったくありません。 // There is no remote transaction at all.
	}
	// If the remote transaction is even cheaper than the
	// cheapest one tracked locally, reject it.
	// リモートトランザクションがローカルで追跡された最も安いトランザクションよりもさらに安い場合は、それを拒否します。
	return h.cmp(h.list[0], tx) >= 0
}

// Discard finds a number of most underpriced transactions, removes them from the
// priced list and returns them for further removal from the entire pool.
//
// Note local transaction won't be considered for eviction.
// Discardは、最も低価格のトランザクションをいくつか見つけ、それらを価格リストから削除し、
// プール全体からさらに削除するためにそれらを返します。
//
// ローカルトランザクションは削除の対象とは見なされないことに注意してください。
func (l *txPricedList) Discard(slots int, force bool) (types.Transactions, bool) {
	drop := make(types.Transactions, 0, slots) // 削除するリモートの低価格トランザクション // Remote underpriced transactions to drop
	for slots > 0 {
		if len(l.urgent.list)*floatingRatio > len(l.floating.list)*urgentRatio || floatingRatio == 0 {
			// Discard stale transactions if found during cleanup
			// クリーンアップ中に見つかった場合、古いトランザクションを破棄します
			tx := heap.Pop(&l.urgent).(*types.Transaction)
			if l.all.GetRemote(tx.Hash()) == nil { //削除または移行 // Removed or migrated
				atomic.AddInt64(&l.stales, -1)
				continue
			}
			// Non stale transaction found, move to floating heap
			// 古くないトランザクションが見つかりました、フローティングヒープに移動します
			heap.Push(&l.floating, tx)
		} else {
			if len(l.floating.list) == 0 {
				// Stop if both heaps are empty
				// 両方のヒープが空の場合は停止します
				break
			}
			// Discard stale transactions if found during cleanup
			// クリーンアップ中に見つかった場合、古いトランザクションを破棄します
			tx := heap.Pop(&l.floating).(*types.Transaction)
			if l.all.GetRemote(tx.Hash()) == nil { // 削除または移行 // Removed or migrated
				atomic.AddInt64(&l.stales, -1)
				continue
			}
			// Non stale transaction found, discard it
			// 古くないトランザクションが見つかりました、破棄します
			drop = append(drop, tx)
			slots -= numSlots(tx)
		}
	}
	// If we still can't make enough room for the new transaction
	// それでも新しいトランザクションのための十分なスペースを確保できない場合
	if slots > 0 && !force {
		for _, tx := range drop {
			heap.Push(&l.urgent, tx)
		}
		return nil, false
	}
	return drop, true
}

// Reheap forcibly rebuilds the heap based on the current remote transaction set.
// Reheapは、現在のリモートトランザクションセットに基づいてヒープを強制的に再構築します。
func (l *txPricedList) Reheap() {
	l.reheapMu.Lock()
	defer l.reheapMu.Unlock()
	start := time.Now()
	atomic.StoreInt64(&l.stales, 0)
	l.urgent.list = make([]*types.Transaction, 0, l.all.RemoteCount())
	l.all.Range(func(hash common.Hash, tx *types.Transaction, local bool) bool {
		l.urgent.list = append(l.urgent.list, tx)
		return true
	}, false, true) // リモートのみを反復します // Only iterate remotes
	heap.Init(&l.urgent)

	// balance out the two heaps by moving the worse half of transactions into the
	// floating heap
	// Note: Discard would also do this before the first eviction but Reheap can do
	// is more efficiently. Also, Underpriced would work suboptimally the first time
	// if the floating queue was empty.
	// トランザクションの後半をフローティングヒープに移動して、2つのヒープのバランスを取ります
	// 注：Discardも最初の立ち退きの前にこれを行いますが、Reheapが行うことができるのはより効率的です。
	// また、フローティングキューが空の場合、Underpricedは最初は最適に機能しません。
	floatingCount := len(l.urgent.list) * floatingRatio / (urgentRatio + floatingRatio)
	l.floating.list = make([]*types.Transaction, floatingCount)
	for i := 0; i < floatingCount; i++ {
		l.floating.list[i] = heap.Pop(&l.urgent).(*types.Transaction)
	}
	heap.Init(&l.floating)
	reheapTimer.Update(time.Since(start))
}

// SetBaseFee updates the base fee and triggers a re-heap. Note that Removed is not
// necessary to call right before SetBaseFee when processing a new block.
// SetBaseFeeは基本料金を更新し、再ヒープをトリガーします。 削除されていないことに注意してください
// 新しいブロックを処理するときにSetBaseFeeの直前に呼び出す必要があります。
func (l *txPricedList) SetBaseFee(baseFee *big.Int) {
	l.urgent.baseFee = baseFee
	l.Reheap()
}
