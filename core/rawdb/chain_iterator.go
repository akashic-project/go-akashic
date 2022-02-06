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

package rawdb

import (
	"runtime"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// InitDatabaseFromFreezer reinitializes an empty database from a previous batch
// of frozen ancient blocks. The method iterates over all the frozen blocks and
// injects into the database the block hash->number mappings.
// InitDatabaseFromFreezerは、凍結された古代ブロックの前のバッチから空のデータベースを再初期化します。
// このメソッドは、凍結されたすべてのブロックを反復処理し、ブロックハッシュ->数値マッピングをデータベースに挿入します。
func InitDatabaseFromFreezer(db ethdb.Database) {
	// If we can't access the freezer or it's empty, abort
	// 冷凍庫にアクセスできない場合、または冷凍庫が空の場合は中止します
	frozen, err := db.Ancients()
	if err != nil || frozen == 0 {
		return
	}
	var (
		batch  = db.NewBatch()
		start  = time.Now()
		logged = start.Add(-7 * time.Second) //インポート中のインデックス解除は高速です。ログを二重にしないでください // Unindex during import is fast, don't double log
		hash   common.Hash
	)
	for i := uint64(0); i < frozen; {
		// We read 100K hashes at a time, for a total of 3.2M
		// 一度に100Kのハッシュを読み取り、合計で320万
		count := uint64(100_000)
		if i+count > frozen {
			count = frozen - i
		}
		data, err := db.AncientRange(freezerHashTable, i, count, 32*count)
		if err != nil {
			log.Crit("Failed to init database from freezer", "err", err)
		}
		for j, h := range data {
			number := i + uint64(j)
			hash = common.BytesToHash(h)
			WriteHeaderNumber(batch, hash, number)
			// If enough data was accumulated in memory or we're at the last block, dump to disk
			// 十分なデータがメモリに蓄積されているか、最後のブロックにいる場合は、ディスクにダンプします
			if batch.ValueSize() > ethdb.IdealBatchSize {
				if err := batch.Write(); err != nil {
					log.Crit("Failed to write data to db", "err", err)
				}
				batch.Reset()
			}
		}
		i += uint64(len(data))
		// If we've spent too much time already, notify the user of what we're doing
		// すでに時間がかかりすぎている場合は、ユーザーに何をしているかを通知します
		if time.Since(logged) > 8*time.Second {
			log.Info("Initializing database from freezer", "total", frozen, "number", i, "hash", hash, "elapsed", common.PrettyDuration(time.Since(start)))
			logged = time.Now()
		}
	}
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write data to db", "err", err)
	}
	batch.Reset()

	WriteHeadHeaderHash(db, hash)
	WriteHeadFastBlockHash(db, hash)
	log.Info("Initialized database from freezer", "blocks", frozen, "elapsed", common.PrettyDuration(time.Since(start)))
}

type blockTxHashes struct {
	number uint64
	hashes []common.Hash
}

// iterateTransactions iterates over all transactions in the (canon) block
// number(s) given, and yields the hashes on a channel. If there is a signal
// received from interrupt channel, the iteration will be aborted and result
// channel will be closed.
// iterateTransactionsは、指定された（canon）ブロック番号のすべてのトランザクションを反復処理し、チャネルにハッシュを生成します。
// 割り込みチャネルから信号を受信した場合、反復は中止され、結果チャネルは閉じられます。
func iterateTransactions(db ethdb.Database, from uint64, to uint64, reverse bool, interrupt chan struct{}) chan *blockTxHashes {
	// One thread sequentially reads data from db
	// 1つのスレッドがdbからデータを順次読み取ります
	type numberRlp struct {
		number uint64
		rlp    rlp.RawValue
	}
	if to == from {
		return nil
	}
	threads := to - from
	if cpus := runtime.NumCPU(); threads > uint64(cpus) {
		threads = uint64(cpus)
	}
	var (
		rlpCh    = make(chan *numberRlp, threads*2)     // このチャネルを介して生のrlpを送信します // we send raw rlp over this channel
		hashesCh = make(chan *blockTxHashes, threads*2) // hashesChを介してハッシュを送信します   // send hashes over hashesCh
	)
	// lookup runs in one instance
	// ルックアップは1つのインスタンスで実行されます
	lookup := func() {
		n, end := from, to
		if reverse {
			n, end = to-1, from-1
		}
		defer close(rlpCh)
		for n != end {
			data := ReadCanonicalBodyRLP(db, n)
			// Feed the block to the aggregator, or abort on interrupt
			// ブロックをアグリゲーターにフィードするか、割り込みで中止します
			select {
			case rlpCh <- &numberRlp{n, data}:
			case <-interrupt:
				return
			}
			if reverse {
				n--
			} else {
				n++
			}
		}
	}
	// process runs in parallel
	// プロセスは並行して実行されます
	nThreadsAlive := int32(threads)
	process := func() {
		defer func() {
			// Last processor closes the result channel
			// 最後のプロセッサが結果チャネルを閉じます
			if atomic.AddInt32(&nThreadsAlive, -1) == 0 {
				close(hashesCh)
			}
		}()
		for data := range rlpCh {
			var body types.Body
			if err := rlp.DecodeBytes(data.rlp, &body); err != nil {
				log.Warn("Failed to decode block body", "block", data.number, "error", err)
				return
			}
			var hashes []common.Hash
			for _, tx := range body.Transactions {
				hashes = append(hashes, tx.Hash())
			}
			result := &blockTxHashes{
				hashes: hashes,
				number: data.number,
			}
			// Feed the block to the aggregator, or abort on interrupt
			// ブロックをアグリゲーターにフィードするか、割り込みで中止します
			select {
			case hashesCh <- result:
			case <-interrupt:
				return
			}
		}
	}
	go lookup() // シーケンシャルデータベースアクセサーを開始します // start the sequential db accessor
	for i := 0; i < int(threads); i++ {
		go process()
	}
	return hashesCh
}

// indexTransactions creates txlookup indices of the specified block range.
//
// This function iterates canonical chain in reverse order, it has one main advantage:
// We can write tx index tail flag periodically even without the whole indexing
// procedure is finished. So that we can resume indexing procedure next time quickly.
//
// There is a passed channel, the whole procedure will be interrupted if any
// signal received.
// indexTransactionsは、指定されたブロック範囲のtxlookupインデックスを作成します。
//
// この関数は、正規チェーンを逆の順序で繰り返します。主な利点が1つあります。
// インデックス全体がなくても、定期的にtxインデックステールフラグを記述できます
// プロシージャは終了しました。次回はすぐにインデックス作成手順を再開できるようにします。
//
// 渡されたチャネルがあり、信号を受信するとプロシージャ全体が中断されます。
func indexTransactions(db ethdb.Database, from uint64, to uint64, interrupt chan struct{}, hook func(uint64) bool) {
	// short circuit for invalid range
	// 無効な範囲の短絡
	if from >= to {
		return
	}
	var (
		hashesCh = iterateTransactions(db, from, to, true, interrupt)
		batch    = db.NewBatch()
		start    = time.Now()
		logged   = start.Add(-7 * time.Second)
		// Since we iterate in reverse, we expect the first number to come
		// in to be [to-1]. Therefore, setting lastNum to means that the
		// prqueue gap-evaluation will work correctly
		// 逆に反復するため、最初の数値は[to-1]になると予想されます。したがって、lastNumをに設定すると、
		// prqueueギャップ-評価は正しく機能します
		lastNum = to
		queue   = prque.New(nil)
		// for stats reporting
		// 統計レポート用
		blocks, txs = 0, 0
	)
	for chanDelivery := range hashesCh {
		// Push the delivery into the queue and process contiguous ranges.
		// Since we iterate in reverse, so lower numbers have lower prio, and
		// we can use the number directly as prio marker
		// 配信をキューにプッシュし、連続する範囲を処理します。
		// 逆に繰り返すため、数値が小さいほどprioが低くなり、数値をprioマーカーとして直接使用できます
		queue.Push(chanDelivery, int64(chanDelivery.number))
		for !queue.Empty() {
			// If the next available item is gapped, return
			// 次に利用可能なアイテムにギャップがある場合は、
			if _, priority := queue.Peek(); priority != int64(lastNum-1) {
				break
			}
			// For testing
			// 検査用の
			if hook != nil && !hook(lastNum-1) {
				break
			}
			// Next block available, pop it off and index it
			// 利用可能な次のブロック、それをポップオフしてインデックスを作成します
			delivery := queue.PopItem().(*blockTxHashes)
			lastNum = delivery.number
			WriteTxLookupEntries(batch, delivery.number, delivery.hashes)
			blocks++
			txs += len(delivery.hashes)
			// If enough data was accumulated in memory or we're at the last block, dump to disk
			// 十分なデータがメモリに蓄積されているか、最後のブロックにいる場合は、ディスクにダンプします
			if batch.ValueSize() > ethdb.IdealBatchSize {
				WriteTxIndexTail(batch, lastNum) // Also write the tail here
				if err := batch.Write(); err != nil {
					log.Crit("Failed writing batch to db", "error", err)
					return
				}
				batch.Reset()
			}
			// If we've spent too much time already, notify the user of what we're doing
			// すでに時間がかかりすぎている場合は、ユーザーに何をしているかを通知します
			if time.Since(logged) > 8*time.Second {
				log.Info("Indexing transactions", "blocks", blocks, "txs", txs, "tail", lastNum, "total", to-from, "elapsed", common.PrettyDuration(time.Since(start)))
				logged = time.Now()
			}
		}
	}
	// Flush the new indexing tail and the last committed data. It can also happen
	// that the last batch is empty because nothing to index, but the tail has to
	// be flushed anyway.
	// 新しいインデックステールと最後にコミットされたデータをフラッシュします。
	// インデックスを作成するものがないために最後のバッチが空になることもありますが、
	// とにかくテールをフラッシュする必要があります。
	WriteTxIndexTail(batch, lastNum)
	if err := batch.Write(); err != nil {
		log.Crit("Failed writing batch to db", "error", err)
		return
	}
	select {
	case <-interrupt:
		log.Debug("Transaction indexing interrupted", "blocks", blocks, "txs", txs, "tail", lastNum, "elapsed", common.PrettyDuration(time.Since(start)))
	default:
		log.Info("Indexed transactions", "blocks", blocks, "txs", txs, "tail", lastNum, "elapsed", common.PrettyDuration(time.Since(start)))
	}
}

// IndexTransactions creates txlookup indices of the specified block range. The from
// is included while to is excluded.
//
// This function iterates canonical chain in reverse order, it has one main advantage:
// We can write tx index tail flag periodically even without the whole indexing
// procedure is finished. So that we can resume indexing procedure next time quickly.
//
// There is a passed channel, the whole procedure will be interrupted if any
// signal received.
// IndexTransactionsは、指定されたブロック範囲のtxlookupインデックスを作成します。 fromは含まれ、toは除外されます。
//
// この関数は、正規チェーンを逆の順序で繰り返します。主な利点が1つあります。
// インデックス作成手順全体が終了していなくても、txインデックステールフラグを定期的に書き込むことができます。
// 次回はすぐにインデックス作成手順を再開できるようにします。
//
// 渡されたチャネルがあり、信号を受信するとプロシージャ全体が中断されます。
func IndexTransactions(db ethdb.Database, from uint64, to uint64, interrupt chan struct{}) {
	indexTransactions(db, from, to, interrupt, nil)
}

// indexTransactionsForTesting is the internal debug version with an additional hook.
// indexTransactionsForTestingは、フックが追加された内部デバッグバージョンです。
func indexTransactionsForTesting(db ethdb.Database, from uint64, to uint64, interrupt chan struct{}, hook func(uint64) bool) {
	indexTransactions(db, from, to, interrupt, hook)
}

// unindexTransactions removes txlookup indices of the specified block range.
//
// There is a passed channel, the whole procedure will be interrupted if any
// signal received.
// unindexTransactionsは、指定されたブロック範囲のtxlookupインデックスを削除します。
//
// 渡されたチャネルがあり、信号を受信するとプロシージャ全体が中断されます。
func unindexTransactions(db ethdb.Database, from uint64, to uint64, interrupt chan struct{}, hook func(uint64) bool) {
	// short circuit for invalid range
	// 無効な範囲の短絡
	if from >= to {
		return
	}
	var (
		hashesCh = iterateTransactions(db, from, to, false, interrupt)
		batch    = db.NewBatch()
		start    = time.Now()
		logged   = start.Add(-7 * time.Second)
		// we expect the first number to come in to be [from]. Therefore, setting
		// nextNum to from means that the prqueue gap-evaluation will work correctly
		// 最初の番号が[from]になると予想します。
		// したがって、nextNumをfromに設定すると、prqueueギャップ評価が正しく機能することを意味します。
		nextNum = from
		queue   = prque.New(nil)
		// for stats reporting
		// 統計レポート用
		blocks, txs = 0, 0
	)
	// Otherwise spin up the concurrent iterator and unindexer
	// それ以外の場合は、同時イテレータとアンインデクサーを起動します
	for delivery := range hashesCh {
		// Push the delivery into the queue and process contiguous ranges.
		// 配信をキューにプッシュし、連続する範囲を処理します。
		queue.Push(delivery, -int64(delivery.number))
		for !queue.Empty() {
			// If the next available item is gapped, return
			// 次に利用可能なアイテムにギャップがある場合は、
			if _, priority := queue.Peek(); -priority != int64(nextNum) {
				break
			}
			// For testing
			// 検査用の
			if hook != nil && !hook(nextNum) {
				break
			}
			delivery := queue.PopItem().(*blockTxHashes)
			nextNum = delivery.number + 1
			DeleteTxLookupEntries(batch, delivery.hashes)
			txs += len(delivery.hashes)
			blocks++

			// If enough data was accumulated in memory or we're at the last block, dump to disk
			// A batch counts the size of deletion as '1', so we need to flush more
			// often than that.
			// 十分なデータがメモリに蓄積されているか、最後のブロックにいる場合は、
			// ディスクにダンプしますバッチは削除のサイズを「1」としてカウントするため、
			// それよりも頻繁にフラッシュする必要があります。
			if blocks%1000 == 0 {
				WriteTxIndexTail(batch, nextNum)
				if err := batch.Write(); err != nil {
					log.Crit("Failed writing batch to db", "error", err)
					return
				}
				batch.Reset()
			}
			// If we've spent too much time already, notify the user of what we're doing
			// すでに時間がかかりすぎている場合は、ユーザーに何をしているかを通知します
			if time.Since(logged) > 8*time.Second {
				log.Info("Unindexing transactions", "blocks", blocks, "txs", txs, "total", to-from, "elapsed", common.PrettyDuration(time.Since(start)))
				logged = time.Now()
			}
		}
	}
	// Flush the new indexing tail and the last committed data. It can also happen
	// that the last batch is empty because nothing to unindex, but the tail has to
	// be flushed anyway.
	// 新しいインデックステールと最後にコミットされたデータをフラッシュします。
	// インデックスを解除するものがないために最後のバッチが空になることもありますが、
	// とにかくテールをフラッシュする必要があります。
	WriteTxIndexTail(batch, nextNum)
	if err := batch.Write(); err != nil {
		log.Crit("Failed writing batch to db", "error", err)
		return
	}
	select {
	case <-interrupt:
		log.Debug("Transaction unindexing interrupted", "blocks", blocks, "txs", txs, "tail", to, "elapsed", common.PrettyDuration(time.Since(start)))
	default:
		log.Info("Unindexed transactions", "blocks", blocks, "txs", txs, "tail", to, "elapsed", common.PrettyDuration(time.Since(start)))
	}
}

// UnindexTransactions removes txlookup indices of the specified block range.
// The from is included while to is excluded.
//
// There is a passed channel, the whole procedure will be interrupted if any
// signal received.
// UnindexTransactionsは、指定されたブロック範囲のtxlookupインデックスを削除します。
// fromは含まれ、toは除外されます。
//
// 渡されたチャネルがあり、信号を受信するとプロシージャ全体が中断されます。
func UnindexTransactions(db ethdb.Database, from uint64, to uint64, interrupt chan struct{}) {
	unindexTransactions(db, from, to, interrupt, nil)
}

// unindexTransactionsForTesting is the internal debug version with an additional hook.
// unindexTransactionsForTestingは、フックが追加された内部デバッグバージョンです。
func unindexTransactionsForTesting(db ethdb.Database, from uint64, to uint64, interrupt chan struct{}, hook func(uint64) bool) {
	unindexTransactions(db, from, to, interrupt, hook)
}
