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

package core

import (
	"sync/atomic"

	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/params"
)

// statePrefetcher is a basic Prefetcher, which blindly executes a block on top
// of an arbitrary state with the goal of prefetching potentially useful state
// data from disk before the main block processor start executing.
// statePrefetcherは基本的なプリフェッチャーであり、メインブロックプロセッサが実行を開始する前に、
// 潜在的に有用な状態データをディスクからプリフェッチすることを目的として、
// 任意の状態の上にブロックを盲目的に実行します。
type statePrefetcher struct {
	config *params.ChainConfig // チェーン構成オプション // Chain configuration options
	bc     *BlockChain         // 正規のブロックチェーン // Canonical block chain
	engine consensus.Engine    // ブロック報酬に使用されるコンセンサスエンジン // Consensus engine used for block rewards
}

// newStatePrefetcher initialises a new statePrefetcher.
// newStatePrefetcherは、新しいstatePrefetcherを初期化します。
func newStatePrefetcher(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *statePrefetcher {
	return &statePrefetcher{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

// Prefetch processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb, but any changes are discarded. The
// only goal is to pre-cache transaction signatures and state trie nodes.
// プリフェッチは、statementedbを使用してトランザクションメッセージを実行することにより、
// イーサリアムルールに従って状態の変化を処理しますが、変更はすべて破棄されます。
// 唯一の目標は、トランザクション署名と状態トライノードを事前にキャッシュすることです。
func (p *statePrefetcher) Prefetch(block *types.Block, statedb *state.StateDB, cfg vm.Config, interrupt *uint32) {
	var (
		header       = block.Header()
		gaspool      = new(GasPool).AddGas(block.GasLimit())
		blockContext = NewEVMBlockContext(header, p.bc, nil)
		evm          = vm.NewEVM(blockContext, vm.TxContext{}, statedb, p.config, cfg)
		signer       = types.MakeSigner(p.config, header.Number)
	)
	// Iterate over and process the individual transactions
	// 個々のトランザクションを繰り返し処理します
	byzantium := p.config.IsByzantium(block.Number())
	for i, tx := range block.Transactions() {
		// If block precaching was interrupted, abort
		// ブロックの事前キャッシュが中断された場合は、中止します
		if interrupt != nil && atomic.LoadUint32(interrupt) == 1 {
			return
		}
		// Convert the transaction into an executable message and pre-cache its sender
		// トランザクションを実行可能メッセージに変換し、その送信者を事前にキャッシュします
		msg, err := tx.AsMessage(signer, header.BaseFee)
		if err != nil {
			return // 無効なブロック、ベイルアウト // Also invalid block, bail out
		}
		statedb.Prepare(tx.Hash(), i)
		if err := precacheTransaction(msg, p.config, gaspool, statedb, header, evm); err != nil {
			return //うーん、何かがひどく間違っていた、保釈// Ugh, something went horribly wrong, bail out
		}
		// If we're pre-byzantium, pre-load trie nodes for the intermediate root
		// ビザンチウム以前の場合、中間ルートのトライノードをプリロードします
		if !byzantium {
			statedb.IntermediateRoot(true)
		}
	}
	// If were post-byzantium, pre-load trie nodes for the final root hash
	// ポストビザンチウムの場合、最終ルートハッシュのトライノードをプリロードします
	if byzantium {
		statedb.IntermediateRoot(true)
	}
}

// precacheTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. The goal is not to execute
// the transaction successfully, rather to warm up touched data slots.

// precacheTransactionは、指定された状態データベースにトランザクションを適用しようとし、その環境の入力パラメーターを使用します。
// 目標は、トランザクションを正常に実行することではなく、タッチされたデータスロットをウォームアップすることです。
func precacheTransaction(msg types.Message, config *params.ChainConfig, gaspool *GasPool, statedb *state.StateDB, header *types.Header, evm *vm.EVM) error {
	// Update the evm with the new transaction context.
	// evmを新しいトランザクションコンテキストで更新します。
	evm.Reset(NewEVMTxContext(msg), statedb)
	// Add addresses to access list if applicable
	// 必要に応じて、アクセスリストにアドレスを追加します
	_, err := ApplyMessage(evm, msg, gaspool)
	return err
}
