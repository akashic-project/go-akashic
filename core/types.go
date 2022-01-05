// Copyright 2015 The go-ethereum Authors
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
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
)

// Validator is an interface which defines the standard for block validation. It
// is only responsible for validating block contents, as the header validation is
// done by the specific consensus engines.
// Validatorは、ブロック検証の標準を定義するインターフェースです。
// ヘッダーの検証は特定のコンセンサスエンジンによって行われるため、ブロックの内容の検証のみを担当します。
type Validator interface {
	// ValidateBody validates the given block's content.
	// ValidateBodyは、指定されたブロックのコンテンツを検証します。
	ValidateBody(block *types.Block) error

	// ValidateState validates the given statedb and optionally the receipts and
	// gas used.
	// ValidateStateは、指定されたstatedBと、オプションで使用されたレシートとガスを検証します。
	ValidateState(block *types.Block, state *state.StateDB, receipts types.Receipts, usedGas uint64) error
}

// Prefetcher is an interface for pre-caching transaction signatures and state.
// Prefetcherは、トランザクションの署名と状態を事前にキャッシュするためのインターフェースです。
type Prefetcher interface {
	// Prefetch processes the state changes according to the Ethereum rules by running
	// the transaction messages using the statedb, but any changes are discarded. The
	// only goal is to pre-cache transaction signatures and state trie nodes.
	// プリフェッチは、statementedbを使用してトランザクションメッセージを実行することにより、
	// イーサリアムルールに従って状態の変化を処理しますが、変更はすべて破棄されます。
	// 唯一の目標は、トランザクション署名と状態トライノードを事前にキャッシュすることです。
	Prefetch(block *types.Block, statedb *state.StateDB, cfg vm.Config, interrupt *uint32)
}

// Processor is an interface for processing blocks using a given initial state.
// プロセッサは、特定の初期状態を使用してブロックを処理するためのインターフェイスです。
type Processor interface {
	// Process processes the state changes according to the Ethereum rules by running
	// the transaction messages using the statedb and applying any rewards to both
	// the processor (coinbase) and any included uncles.
	// プロセスは、statementedbを使用してトランザクションメッセージを実行し、
	// プロセッサ（コインベース）と含まれている叔父の両方に報酬を適用することにより、
	// イーサリアムのルールに従って状態の変化を処理します。
	Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error)
}
