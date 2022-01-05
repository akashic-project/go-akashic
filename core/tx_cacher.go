// Copyright 2018 The go-ethereum Authors
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
	"runtime"

	"github.com/ethereum/go-ethereum/core/types"
)

// senderCacher is a concurrent transaction sender recoverer and cacher.
// senderCacherは、同時トランザクションの送信者リカバリとキャッシャーです。
var senderCacher = newTxSenderCacher(runtime.NumCPU())

// txSenderCacherRequest is a request for recovering transaction senders with a
// specific signature scheme and caching it into the transactions themselves.
//
// The inc field defines the number of transactions to skip after each recovery,
// which is used to feed the same underlying input array to different threads but
// ensure they process the early transactions fast.
// txSenderCacherRequestは、特定の署名スキームを使用してトランザクション送信者を回復し、
// それをトランザクション自体にキャッシュするためのリクエストです。
//
// incフィールドは、各リカバリ後にスキップするトランザクションの数を定義します。
// これは、同じ基になる入力配列を異なるスレッドにフィードするために使用されますが、
// 初期のトランザクションを高速に処理します。
type txSenderCacherRequest struct {
	signer types.Signer
	txs    []*types.Transaction
	inc    int
}

// txSenderCacher is a helper structure to concurrently ecrecover transaction
// senders from digital signatures on background threads.
// txSenderCacherは、トランザクションを同時にecrecoverするためのヘルパー構造です
// バックグラウンドスレッドのデジタル署名からの送信者。
type txSenderCacher struct {
	threads int
	tasks   chan *txSenderCacherRequest
}

// newTxSenderCacher creates a new transaction sender background cacher and starts
// as many processing goroutines as allowed by the GOMAXPROCS on construction.
// newTxSenderCacherは、新しいトランザクション送信者のバックグラウンドキャッシャーを作成し、
// 構築時にGOMAXPROCSで許可されている数の処理ゴルーチンを開始します。
func newTxSenderCacher(threads int) *txSenderCacher {
	cacher := &txSenderCacher{
		tasks:   make(chan *txSenderCacherRequest, threads),
		threads: threads,
	}
	for i := 0; i < threads; i++ {
		go cacher.cache()
	}
	return cacher
}

// cache is an infinite loop, caching transaction senders from various forms of
// data structures.
//キャッシュは無限ループであり、さまざまな形式のデータ構造からトランザクション送信者をキャッシュします。
func (cacher *txSenderCacher) cache() {
	for task := range cacher.tasks {
		for i := 0; i < len(task.txs); i += task.inc {
			types.Sender(task.signer, task.txs[i])
		}
	}
}

// recover recovers the senders from a batch of transactions and caches them
// back into the same data structures. There is no validation being done, nor
// any reaction to invalid signatures. That is up to calling code later.
// restoreは、トランザクションのバッチから送信者を回復し、それらを同じデータ構造にキャッシュします。
// 検証は行われず、無効な署名に対する反応もありません。 それは後でコードを呼び出すことまでです。
func (cacher *txSenderCacher) recover(signer types.Signer, txs []*types.Transaction) {
	// If there's nothing to recover, abort
	// 回復するものがない場合は、中止します
	if len(txs) == 0 {
		return
	}
	// Ensure we have meaningful task sizes and schedule the recoveries
	// 意味のあるタスクサイズがあることを確認し、リカバリをスケジュールします
	tasks := cacher.threads
	if len(txs) < tasks*4 {
		tasks = (len(txs) + 3) / 4
	}
	for i := 0; i < tasks; i++ {
		cacher.tasks <- &txSenderCacherRequest{
			signer: signer,
			txs:    txs[i:],
			inc:    tasks,
		}
	}
}

// recoverFromBlocks recovers the senders from a batch of blocks and caches them
// back into the same data structures. There is no validation being done, nor
// any reaction to invalid signatures. That is up to calling code later.
// restoreFromBlocksは、ブロックのバッチから送信者を回復し、それらを同じデータ構造にキャッシュします。
// 検証は行われず、無効な署名に対する反応もありません。 それは後でコードを呼び出すことまでです。
func (cacher *txSenderCacher) recoverFromBlocks(signer types.Signer, blocks []*types.Block) {
	count := 0
	for _, block := range blocks {
		count += len(block.Transactions())
	}
	txs := make([]*types.Transaction, 0, count)
	for _, block := range blocks {
		txs = append(txs, block.Transactions()...)
	}
	cacher.recover(signer, txs)
}
