// Copyright 2017 The go-ethereum Authors
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
	"errors"
	"io"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// errNoActiveJournal is returned if a transaction is attempted to be inserted
// into the journal, but no such file is currently open.
//トランザクションをジャーナルに挿入しようとしたが、
// そのようなファイルが現在開いていない場合は、errNoActiveJournalが返されます。
var errNoActiveJournal = errors.New("no active journal")

// devNull is a WriteCloser that just discards anything written into it. Its
// goal is to allow the transaction journal to write into a fake journal when
// loading transactions on startup without printing warnings due to no file
// being read for write.
// devNullは、書き込まれたものをすべて破棄するWriteCloserです。
// その目的は、書き込み用に読み取られているファイルがないために警告を出力せずに、
// 起動時にトランザクションをロードするときにトランザクションジャーナルが偽のジャーナルに書き込めるようにすることです。
type devNull struct{}

func (*devNull) Write(p []byte) (n int, err error) { return len(p), nil }
func (*devNull) Close() error                      { return nil }

// txJournal is a rotating log of transactions with the aim of storing locally
// created transactions to allow non-executed ones to survive node restarts.
// txJournalは、ローカルで作成されたトランザクションを保存して、
// 実行されていないトランザクションがノードの再起動後も存続できるようにすることを目的とした、
// トランザクションのローテーションログです。
type txJournal struct {
	path   string         //トランザクションを保存するファイルシステムパス        // Filesystem path to store the transactions at
	writer io.WriteCloser // 新しいトランザクションを書き込むための出力ストリーム // Output stream to write new transactions into
}

// newTxJournal creates a new transaction journal to
// newTxJournalは、新しいトランザクションジャーナルを作成します
func newTxJournal(path string) *txJournal {
	return &txJournal{
		path: path,
	}
}

// load parses a transaction journal dump from disk, loading its contents into
// the specified pool.
// loadは、ディスクからトランザクションジャーナルダンプを解析し、
// その内容を指定されたプールにロードします。
func (journal *txJournal) load(add func([]*types.Transaction) []error) error {
	// Skip the parsing if the journal file doesn't exist at all
	// ジャーナルファイルがまったく存在しない場合は、解析をスキップします
	if _, err := os.Stat(journal.path); os.IsNotExist(err) {
		return nil
	}
	// Open the journal for loading any past transactions
	// 過去のトランザクションをロードするためにジャーナルを開きます
	input, err := os.Open(journal.path)
	if err != nil {
		return err
	}
	defer input.Close()

	// Temporarily discard any journal additions (don't double add on load)
	// ジャーナルの追加を一時的に破棄します（ロード時に二重に追加しないでください）
	journal.writer = new(devNull)
	defer func() { journal.writer = nil }()

	// Inject all transactions from the journal into the pool
	// ジャーナルからプールにすべてのトランザクションを挿入します
	stream := rlp.NewStream(input, 0)
	total, dropped := 0, 0

	// Create a method to load a limited batch of transactions and bump the
	// appropriate progress counters. Then use this method to load all the
	// journaled transactions in small-ish batches.
	// トランザクションの限定されたバッチをロードし、適切な進行状況カウンターをバンプするメソッドを作成します。
	// 次に、このメソッドを使用して、ジャーナル化されたすべてのトランザクションを小さなバッチでロードします。
	loadBatch := func(txs types.Transactions) {
		for _, err := range add(txs) {
			if err != nil {
				log.Debug("Failed to add journaled transaction", "err", err)
				dropped++
			}
		}
	}
	var (
		failure error
		batch   types.Transactions
	)
	for {
		// Parse the next transaction and terminate on error
		// 次のトランザクションを解析し、エラーで終了します
		tx := new(types.Transaction)
		if err = stream.Decode(tx); err != nil {
			if err != io.EOF {
				failure = err
			}
			if batch.Len() > 0 {
				loadBatch(batch)
			}
			break
		}
		// New transaction parsed, queue up for later, import if threshold is reached
		// 新しいトランザクションが解析され、後でキューに入れられ、
		// しきい値に達した場合はインポートされます
		total++

		if batch = append(batch, tx); batch.Len() > 1024 {
			loadBatch(batch)
			batch = batch[:0]
		}
	}
	log.Info("Loaded local transaction journal", "transactions", total, "dropped", dropped)

	return failure
}

// insert adds the specified transaction to the local disk journal.
// insertは、指定されたトランザクションをローカルディスクジャーナルに追加します。
func (journal *txJournal) insert(tx *types.Transaction) error {
	if journal.writer == nil {
		return errNoActiveJournal
	}
	if err := rlp.Encode(journal.writer, tx); err != nil {
		return err
	}
	return nil
}

// rotate regenerates the transaction journal based on the current contents of
// the transaction pool.
// rotateはトランザクションプールの現在の内容に基づいてトランザクションジャーナルを再生成します。
func (journal *txJournal) rotate(all map[common.Address]types.Transactions) error {
	// Close the current journal (if any is open)
	// 現在のジャーナルを閉じます（開いている場合）
	if journal.writer != nil {
		if err := journal.writer.Close(); err != nil {
			return err
		}
		journal.writer = nil
	}
	// Generate a new journal with the contents of the current pool
	// 現在のプールの内容で新しいジャーナルを生成します
	replacement, err := os.OpenFile(journal.path+".new", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	journaled := 0
	for _, txs := range all {
		for _, tx := range txs {
			if err = rlp.Encode(replacement, tx); err != nil {
				replacement.Close()
				return err
			}
		}
		journaled += len(txs)
	}
	replacement.Close()

	// Replace the live journal with the newly generated one
	// ライブジャーナルを新しく生成されたジャーナルに置き換えます
	if err = os.Rename(journal.path+".new", journal.path); err != nil {
		return err
	}
	sink, err := os.OpenFile(journal.path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	journal.writer = sink
	log.Info("Regenerated local transaction journal", "transactions", journaled, "accounts", len(all))
	// 再生成されたローカルトランザクションジャーナル
	return nil
}

// close flushes the transaction journal contents to disk and closes the file.
// closeは、トランザクションジャーナルの内容をディスクにフラッシュしてファイルを閉じます。
func (journal *txJournal) close() error {
	var err error

	if journal.writer != nil {
		err = journal.writer.Close()
		journal.writer = nil
	}
	return err
}
