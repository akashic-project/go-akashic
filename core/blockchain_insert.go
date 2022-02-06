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
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// insertStats tracks and reports on block insertion.
type insertStats struct {
	queued, processed, ignored int
	usedGas                    uint64
	lastIndex                  int
	startTime                  mclock.AbsTime
}

// statsReportLimit is the time limit during import and export after which we
// always print out progress. This avoids the user wondering what's going on.
// statsReportLimitは、インポートおよびエクスポート中の制限時間であり、その後は常に進行状況を出力します。
// これにより、ユーザーが何が起こっているのか不思議に思うのを防ぐことができます。
const statsReportLimit = 8 * time.Second

// report prints statistics if some number of blocks have been processed
// or more than a few seconds have passed since the last message.
// レポートは、最後のメッセージからいくつかのブロックが処理された場合、または数秒以上経過した場合に統計を出力します。
func (st *insertStats) report(chain []*types.Block, index int, dirty common.StorageSize) {
	// Fetch the timings for the batch
	// バッチのタイミングを取得します
	var (
		now     = mclock.Now()
		elapsed = now.Sub(st.startTime)
	)
	// If we're at the last block of the batch or report period reached, log
	// バッチまたはレポート期間の最後のブロックに達した場合は、ログに記録します
	if index == len(chain)-1 || elapsed >= statsReportLimit {
		// Count the number of transactions in this segment
		// このセグメントのトランザクション数をカウントします
		var txs int
		for _, block := range chain[st.lastIndex : index+1] {
			txs += len(block.Transactions())
		}
		end := chain[index]

		// Assemble the log context and send it to the logger
		// ログコンテキストをアセンブルし、ロガーに送信します
		context := []interface{}{
			"blocks", st.processed, "txs", txs, "mgas", float64(st.usedGas) / 1000000,
			"elapsed", common.PrettyDuration(elapsed), "mgasps", float64(st.usedGas) * 1000 / float64(elapsed),
			"number", end.Number(), "hash", end.Hash(),
		}
		if timestamp := time.Unix(int64(end.Time()), 0); time.Since(timestamp) > time.Minute {
			context = append(context, []interface{}{"age", common.PrettyAge(timestamp)}...)
		}
		context = append(context, []interface{}{"dirty", dirty}...)

		if st.queued > 0 {
			context = append(context, []interface{}{"queued", st.queued}...)
		}
		if st.ignored > 0 {
			context = append(context, []interface{}{"ignored", st.ignored}...)
		}
		log.Info("Imported new chain segment", context...)

		// Bump the stats reported to the next section
		// 次のセクションに報告された統計をバンプします
		*st = insertStats{startTime: now, lastIndex: index + 1}
	}
}

// insertIterator is a helper to assist during chain import.
// insertIteratorは、チェーンのインポート中に支援するヘルパーです。
type insertIterator struct {
	chain types.Blocks // 繰り返されるブロックのチェーン // Chain of blocks being iterated over

	results <-chan error // コンセンサスエンジンからの検証結果シンク // Verification result sink from the consensus engine
	errors  []error      // ブロックのヘッダー検証エラー            // Header verification errors for the blocks

	index     int       // イテレータの現在のオフセット // Current offset of the iterator
	validator Validator // 検証が成功した場合に実行するバリデーター // Validator to run if verification succeeds
}

// newInsertIterator creates a new iterator based on the given blocks, which are
// assumed to be a contiguous chain.
// newInsertIteratorは、指定されたブロックに基づいて新しいイテレータを作成します。
// これは、連続したチェーンであると見なされます。
func newInsertIterator(chain types.Blocks, results <-chan error, validator Validator) *insertIterator {
	return &insertIterator{
		chain:     chain,
		results:   results,
		errors:    make([]error, 0, len(chain)),
		index:     -1,
		validator: validator,
	}
}

// next returns the next block in the iterator, along with any potential validation
// error for that block. When the end is reached, it will return (nil, nil).
// nextは、イテレータ内の次のブロックと、そのブロックの潜在的な検証エラーを返します。
// 終わりに達すると、（nil、nil）に戻ります。
func (it *insertIterator) next() (*types.Block, error) {
	// If we reached the end of the chain, abort
	// チェーンの最後に到達した場合は、中止します
	if it.index+1 >= len(it.chain) {
		it.index = len(it.chain)
		return nil, nil
	}
	// Advance the iterator and wait for verification result if not yet done
	// イテレータを進め、まだ実行されていない場合は検証結果を待ちます
	it.index++
	if len(it.errors) <= it.index {
		it.errors = append(it.errors, <-it.results)
	}
	if it.errors[it.index] != nil {
		return it.chain[it.index], it.errors[it.index]
	}
	// Block header valid, run body validation and return
	// ヘッダーを有効にブロックし、本文の検証を実行して戻ります
	return it.chain[it.index], it.validator.ValidateBody(it.chain[it.index])
}

// peek returns the next block in the iterator, along with any potential validation
// error for that block, but does **not** advance the iterator.
//
// Both header and body validation errors (nil too) is cached into the iterator
// to avoid duplicating work on the following next() call.
// peekは、イテレータ内の次のブロックと、そのブロックの潜在的な検証エラーを返しますが、イテレータを**進めません**。
//
//ヘッダーと本文の両方の検証エラー（nilも）は、次のnext（）呼び出しでの作業の重複を避けるために、イテレーターにキャッシュされます。
func (it *insertIterator) peek() (*types.Block, error) {
	// If we reached the end of the chain, abort
	// チェーンの最後に到達した場合は、中止します
	if it.index+1 >= len(it.chain) {
		return nil, nil
	}
	// Wait for verification result if not yet done
	// まだ行われていない場合は、検証結果を待ちます
	if len(it.errors) <= it.index+1 {
		it.errors = append(it.errors, <-it.results)
	}
	if it.errors[it.index+1] != nil {
		return it.chain[it.index+1], it.errors[it.index+1]
	}
	// Block header valid, ignore body validation since we don't have a parent anyway
	// ヘッダーを有効にブロックします。とにかく親がないため、本文の検証を無視します
	return it.chain[it.index+1], nil
}

// previous returns the previous header that was being processed, or nil.
// previousは、処理されていた前のヘッダー、またはnilを返します。
func (it *insertIterator) previous() *types.Header {
	if it.index < 1 {
		return nil
	}
	return it.chain[it.index-1].Header()
}

// current returns the current header that is being processed, or nil.
// currentは、処理中の現在のヘッダー、またはnilを返します。
func (it *insertIterator) current() *types.Header {
	if it.index == -1 || it.index >= len(it.chain) {
		return nil
	}
	return it.chain[it.index].Header()
}

// first returns the first block in the it.
// 最初にその中の最初のブロックを返します。
func (it *insertIterator) first() *types.Block {
	return it.chain[0]
}

// remaining returns the number of remaining blocks.
// remainingは残りのブロックの数を返します。
func (it *insertIterator) remaining() int {
	return len(it.chain) - it.index
}

// processed returns the number of processed blocks.
// 処理済みは処理済みブロックの数を返します。
func (it *insertIterator) processed() int {
	return it.index + 1
}
