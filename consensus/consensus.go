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

// Package consensus implements different Ethereum consensus engines.
package consensus

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

// ChainHeaderReader defines a small collection of methods needed to access the local
// blockchain during header verification.
// ChainHeaderReaderは、ヘッダー検証中にローカルブロックチェーンにアクセスするために必要なメソッドの小さなコレクションを定義します。
type ChainHeaderReader interface {
	// Config retrieves the blockchain's chain configuration.
	// Configは、ブロックチェーンのチェーン構成を取得します。
	Config() *params.ChainConfig

	// CurrentHeader retrieves the current header from the local chain.
	// CurrentHeaderは、ローカルチェーンから現在のヘッダーを取得します。
	CurrentHeader() *types.Header

	// GetHeader retrieves a block header from the database by hash and number.
	// GetHeaderは、ハッシュと数値によってデータベースからブロックヘッダーを取得します。
	GetHeader(hash common.Hash, number uint64) *types.Header

	// GetHeaderByNumber retrieves a block header from the database by number.
	// GetHeaderByNumberは、データベースからブロックヘッダーを番号で取得します。
	GetHeaderByNumber(number uint64) *types.Header

	// GetHeaderByHash retrieves a block header from the database by its hash.
	// GetHeaderByHashは、ハッシュによってデータベースからブロックヘッダーを取得します。
	GetHeaderByHash(hash common.Hash) *types.Header

	// GetTd retrieves the total difficulty from the database by hash and number.
	// GetTdは、ハッシュと数値によってデータベースから合計難易度を取得します。
	GetTd(hash common.Hash, number uint64) *big.Int
}

// ChainReader defines a small collection of methods needed to access the local
// blockchain during header and/or uncle verification.
// ChainReaderは、ヘッダーや叔父の検証中にローカルブロックチェーンにアクセスするために必要なメソッドの小さなコレクションを定義します。
type ChainReader interface {
	ChainHeaderReader

	// GetBlock retrieves a block from the database by hash and number.
	// GetBlockは、ハッシュと数値によってデータベースからブロックを取得します。
	GetBlock(hash common.Hash, number uint64) *types.Block
}

// Engine is an algorithm agnostic consensus engine.
// エンジンは、アルゴリズムにとらわれないコンセンサスエンジンです。
type Engine interface {
	// Author retrieves the Ethereum address of the account that minted the given
	// block, which may be different from the header's coinbase if a consensus
	// engine is based on signatures.
	// 作成者は、指定されたブロックを作成したアカウントのイーサリアムアドレスを取得します。
	// これは、コンセンサスエンジンが署名に基づいている場合、ヘッダーのコインベースとは異なる場合があります。
	Author(header *types.Header) (common.Address, error)

	// VerifyHeader checks whether a header conforms to the consensus rules of a
	// given engine. Verifying the seal may be done optionally here, or explicitly
	// via the VerifySeal method.
	// VerificationHeaderは、ヘッダーが特定のエンジンのコンセンサスルールに準拠しているかどうかを確認します。
	// シールの検証は、オプションでここで実行することも、VerifySealメソッドを介して明示的に実行することもできます。
	VerifyHeader(chain ChainHeaderReader, header *types.Header, seal bool, db ethdb.Database) error

	// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
	// concurrently. The method returns a quit channel to abort the operations and
	// a results channel to retrieve the async verifications (the order is that of
	// the input slice).
	// VerificationHeadersはVerifyHeaderに似ていますが、ヘッダーのバッチを同時に検証します。
	// このメソッドは、操作を中止するための終了チャネルと、
	// 非同期検証を取得するための結果チャネルを返します（順序は入力スライスの順序です）。
	VerifyHeaders(chain ChainHeaderReader, headers []*types.Header, seals []bool, db ethdb.Database) (chan<- struct{}, <-chan error)

	// BlockMakeTimeはコインエイジからブロック作成間隔を決定する。
	BlockMakeTime(number *big.Int, difficulty *big.Int, coinbase common.Address, statedb *state.StateDB) uint64

	// VerifyUncles verifies that the given block's uncles conform to the consensus
	// rules of a given engine.
	// VerificationUnclesは、指定されたブロックの叔父が指定されたエンジンのコンセンサスルールに準拠していることを確認します。
	VerifyUncles(chain ChainReader, block *types.Block, db ethdb.Database) error

	// Prepare initializes the consensus fields of a block header according to the
	// rules of a particular engine. The changes are executed inline.
	// Prepareは、特定のエンジンのルールに従って、
	// ブロックヘッダーのコンセンサスフィールドを初期化します。変更はインラインで実行されます
	Prepare(chain ChainHeaderReader, header *types.Header) error

	// Finalize runs any post-transaction state modifications (e.g. block rewards)
	// but does not assemble the block.
	//
	// Note: The block header and state database might be updated to reflect any
	// consensus rules that happen at finalization (e.g. block rewards).
	// Finalizeは、トランザクション後の状態の変更（ブロック報酬など）を実行しますが、ブロックをアセンブルしません。
	//
	// 注：ブロックヘッダーと状態データベースは、ファイナライズ時に発生するコンセンサスルール（ブロック報酬など）を反映するように更新される場合があります。
	Finalize(chain ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction,
		uncles []*types.Header)

	// FinalizeAndAssemble runs any post-transaction state modifications (e.g. block
	// rewards) and assembles the final block.
	//
	// Note: The block header and state database might be updated to reflect any
	// consensus rules that happen at finalization (e.g. block rewards).
	// FinalizeAndAssembleは、トランザクション後の状態の変更（ブロック報酬など）を実行し、最終ブロックをアセンブルします。
	//
	// 注：ブロックヘッダーと状態データベースは、ファイナライズ時に発生するコンセンサスルール（ブロック報酬など）を反映するように更新される場合があります。
	FinalizeAndAssemble(chain ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction,
		uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error)

	// Seal generates a new sealing request for the given input block and pushes
	// the result into the given channel.
	//
	// Note, the method returns immediately and will send the result async. More
	// than one result may also be returned depending on the consensus algorithm.
	// シールは、指定された入力ブロックに対して新しいシール要求を生成し、結果を指定されたチャネルにプッシュします。
	//
	// メソッドはすぐに戻り、結果を非同期で送信することに注意してください。コンセンサスアルゴリズムによっては、複数の結果が返される場合もあります。
	Seal(chain ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error

	// SealHash returns the hash of a block prior to it being sealed.
	// SealHashは、ブロックがシールされる前のブロックのハッシュを返します。
	SealHash(header *types.Header) common.Hash

	// CalcDifficulty is the difficulty adjustment algorithm. It returns the difficulty
	// that a new block should have.
	// CalcDifficultyは難易度調整アルゴリズムです。新しいブロックが持つべき難易度を返します。
	CalcDifficulty(chain ChainHeaderReader, time uint64, parent *types.Header) *big.Int

	// APIs returns the RPC APIs this consensus engine provides.
	// APIは、このコンセンサスエンジンが提供するRPCAPIを返します。
	APIs(chain ChainHeaderReader) []rpc.API

	// Close terminates any background threads maintained by the consensus engine.
	// Closeは、コンセンサスエンジンによって維持されているバックグラウンドスレッドをすべて終了します。
	Close() error
}

// PoW is a consensus engine based on proof-of-work.
// PoWは、プルーフオブワークに基づくコンセンサスエンジンです。
type PoW interface {
	Engine

	// Hashrate returns the current mining hashrate of a PoW consensus engine.
	// Hashrateは、PoWコンセンサスエンジンの現在のマイニングハッシュレートを返します。
	Hashrate() float64
}
