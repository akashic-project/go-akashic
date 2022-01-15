// Copyright 2021 The go-ethereum Authors
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

package beacon

import (
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
)

// Proof-of-stake protocol constants.
// プルーフオブステークプロトコル定数。
var (
	beaconDifficulty = common.Big0          // ビーコンコンセンサスのデフォルトのブロック難易度 // The default block difficulty in the beacon consensus
	beaconNonce      = types.EncodeNonce(0) // ビーコンコンセンサスのデフォルトのブロックナンス // The default block nonce in the beacon consensus
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
// ブロックを無効としてマークするためのさまざまなエラーメッセージ。
// これらは、エンジン固有のエラーがコードベースの残りの部分で参照されないようにするためにプライベートにする必要があります。
// これは、エンジンがスワップアウトされた場合に本質的に壊れます。 一般的なエラータイプをコンセンサスパッケージに入れてください。
var (
	errTooManyUncles    = errors.New("too many uncles")    // おじさんが多すぎる
	errInvalidMixDigest = errors.New("invalid mix digest") // 無効なミックスダイジェスト
	errInvalidNonce     = errors.New("invalid nonce")      // 無効なナンス
	errInvalidUncleHash = errors.New("invalid uncle hash") // 無効な叔父のハッシュ
)

// Beacon is a consensus engine that combines the eth1 consensus and proof-of-stake
// algorithm. There is a special flag inside to decide whether to use legacy consensus
// rules or new rules. The transition rule is described in the eth1/2 merge spec.
// https://github.com/ethereum/EIPs/blob/master/EIPS/eip-3675.md
//
// The beacon here is a half-functional consensus engine with partial functions which
// is only used for necessary consensus checks. The legacy consensus engine can be any
// engine implements the consensus interface (except the beacon itself).
// ビーコンは、eth1コンセンサスとプルーフオブステークアルゴリズムを組み合わせたコンセンサスエンジンです。
// 内部には、従来のコンセンサスルールを使用するか新しいルールを使用するかを決定するための特別なフラグがあります。
// 遷移ルールは、eth1 / 2マージ仕様で説明されています。 https://github.com/ethereum/EIPs/blob/master/EIPS/eip-3675.md
//
// ここでのビーコンは、必要なコンセンサスチェックにのみ使用される部分機能を備えた半機能コンセンサスエンジンです。
// 従来のコンセンサスエンジンは、コンセンサスインターフェイスを実装する任意のエンジンにすることができます（ビーコン自体を除く）。
type Beacon struct {
	ethone consensus.Engine // eth1で使用される元のコンセンサスエンジン。例： エタッシュまたはクリーク // Original consensus engine used in eth1, e.g. ethash or clique
}

// New creates a consensus engine with the given embedded eth1 engine.
// Newは、指定された埋め込みeth1エンジンを使用してコンセンサスエンジンを作成します。
func New(ethone consensus.Engine) *Beacon {
	if _, ok := ethone.(*Beacon); ok {
		panic("nested consensus engine")
	}
	return &Beacon{ethone: ethone}
}

// Author implements consensus.Engine, returning the verified author of the block.
// 作成者はconsensus.Engineを実装し、ブロックの検証済み作成者を返します。
func (beacon *Beacon) Author(header *types.Header) (common.Address, error) {
	if !beacon.IsPoSHeader(header) {
		return beacon.ethone.Author(header)
	}
	return header.Coinbase, nil
}

// VerifyHeader checks whether a header conforms to the consensus rules of the
// stock Ethereum consensus engine.
// VerificationHeaderは、ヘッダーがストックイーサリアムコンセンサスエンジンのコンセンサスルールに準拠しているかどうかを確認します。
func (beacon *Beacon) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header, seal bool) error {
	reached, _ := IsTTDReached(chain, header.ParentHash, header.Number.Uint64()-1)
	if !reached {
		return beacon.ethone.VerifyHeader(chain, header, seal)
	}
	// Short circuit if the parent is not known
	// 親が不明な場合の短絡
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	// Sanity checks passed, do a proper verification
	// 健全性チェックに合格し、適切な検証を行います
	return beacon.verifyHeader(chain, header, parent)
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications.
// VerifyHeaders expect the headers to be ordered and continuous.
// VerificationHeadersはVerifyHeaderに似ていますが、ヘッダーのバッチを同時に検証します。
// このメソッドは、操作を中止するための終了チャネルと、非同期検証を取得するための結果チャネルを返します。
// VerificationHeadersは、ヘッダーが順序付けられて連続していることを期待しています。
func (beacon *Beacon) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	if !beacon.IsPoSHeader(headers[len(headers)-1]) {
		return beacon.ethone.VerifyHeaders(chain, headers, seals)
	}
	var (
		preHeaders  []*types.Header
		postHeaders []*types.Header
		preSeals    []bool
	)
	for index, header := range headers {
		if beacon.IsPoSHeader(header) {
			preHeaders = headers[:index]
			postHeaders = headers[index:]
			preSeals = seals[:index]
			break
		}
	}
	// All the headers have passed the transition point, use new rules.
	// すべてのヘッダーが遷移点を通過しました。新しいルールを使用してください。
	if len(preHeaders) == 0 {
		return beacon.verifyHeaders(chain, headers, nil)
	}
	// The transition point exists in the middle, separate the headers
	// into two batches and apply different verification rules for them.
	// 遷移点は中央に存在し、ヘッダーを2つのバッチに分割し、それらに異なる検証ルールを適用します。
	var (
		abort   = make(chan struct{})
		results = make(chan error, len(headers))
	)
	go func() {
		var (
			old, new, out      = 0, len(preHeaders), 0
			errors             = make([]error, len(headers))
			done               = make([]bool, len(headers))
			oldDone, oldResult = beacon.ethone.VerifyHeaders(chain, preHeaders, preSeals)
			newDone, newResult = beacon.verifyHeaders(chain, postHeaders, preHeaders[len(preHeaders)-1])
		)
		for {
			for ; done[out]; out++ {
				results <- errors[out]
				if out == len(headers)-1 {
					return
				}
			}
			select {
			case err := <-oldResult:
				errors[old], done[old] = err, true
				old++
			case err := <-newResult:
				errors[new], done[new] = err, true
				new++
			case <-abort:
				close(oldDone)
				close(newDone)
				return
			}
		}
	}()
	return abort, results
}

// VerifyUncles verifies that the given block's uncles conform to the consensus
// rules of the Ethereum consensus engine.
// VerificationUnclesは、指定されたブロックの叔父がイーサリアムコンセンサスエンジンの
// コンセンサスルールに準拠していることを確認します。
func (beacon *Beacon) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	if !beacon.IsPoSHeader(block.Header()) {
		return beacon.ethone.VerifyUncles(chain, block)
	}
	// Verify that there is no uncle block. It's explicitly disabled in the beacon
	// おじさんのブロックがないことを確認します。 ビーコンで明示的に無効になっています
	if len(block.Uncles()) > 0 {
		return errTooManyUncles
	}
	return nil
}

// verifyHeader checks whether a header conforms to the consensus rules of the
// stock Ethereum consensus engine. The difference between the beacon and classic is
// (a) The following fields are expected to be constants:
//     - difficulty is expected to be 0
// 	   - nonce is expected to be 0
//     - unclehash is expected to be Hash(emptyHeader)
//     to be the desired constants
// (b) the timestamp is not verified anymore
// (c) the extradata is limited to 32 bytes
// verifyHeaderは、ヘッダーがストックイーサリアムコンセンサスエンジンのコンセンサスルールに準拠しているかどうかを確認します。 ビーコンとクラシックの違いは
//（a）次のフィールドは定数であると予想されます：
// -難易度は0と予想されます
// -nonceは0であると予想されます
// --unclehashはHash（emptyHeader）であると予想されます
// 目的の定数になる
//（b）タイムスタンプはもう検証されません
//（c）エクストラデータは32バイトに制限されています
func (beacon *Beacon) verifyHeader(chain consensus.ChainHeaderReader, header, parent *types.Header) error {
	// Ensure that the header's extra-data section is of a reasonable size
	// ヘッダーの追加データセクションが適切なサイズであることを確認します
	if len(header.Extra) > 32 {
		return fmt.Errorf("extra-data longer than 32 bytes (%d)", len(header.Extra))
	}
	// Verify the seal parts. Ensure the mixhash, nonce and uncle hash are the expected value.
	// シール部品を確認します。 mixhash、nonce、およびuncleハッシュが期待値であることを確認してください。
	if header.MixDigest != (common.Hash{}) {
		return errInvalidMixDigest
	}
	if header.Nonce != beaconNonce {
		return errInvalidNonce
	}
	if header.UncleHash != types.EmptyUncleHash {
		return errInvalidUncleHash
	}
	// Verify the block's difficulty to ensure it's the default constant
	// ブロックの難易度を確認して、デフォルトの定数であることを確認します
	if beaconDifficulty.Cmp(header.Difficulty) != 0 {
		return fmt.Errorf("invalid difficulty: have %v, want %v", header.Difficulty, beaconDifficulty)
	}
	// Verify that the gas limit is <= 2^63-1
	// ガス制限が<= 2 ^ 63-1であることを確認します
	if header.GasLimit > params.MaxGasLimit {
		return fmt.Errorf("invalid gasLimit: have %v, max %v", header.GasLimit, params.MaxGasLimit)
	}
	// Verify that the gasUsed is <= gasLimit
	// gasUsedが<= gasLimitであることを確認します
	if header.GasUsed > header.GasLimit {
		return fmt.Errorf("invalid gasUsed: have %d, gasLimit %d", header.GasUsed, header.GasLimit)
	}
	// Verify that the block number is parent's +1
	// ブロック番号が親の+1であることを確認します
	if diff := new(big.Int).Sub(header.Number, parent.Number); diff.Cmp(common.Big1) != 0 {
		return consensus.ErrInvalidNumber
	}
	// Verify the header's EIP-1559 attributes.
	// ヘッダーのEIP-1559属性を確認します。
	return misc.VerifyEip1559Header(chain.Config(), parent, header)
}

// verifyHeaders is similar to verifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications. An additional parent
// header will be passed if the relevant header is not in the database yet.
// verifyHeadersはverifyHeaderに似ていますが、ヘッダーのバッチを同時に検証します。
// このメソッドは、操作を中止するための終了チャネルと、非同期検証を取得するための結果チャネルを返します。
// 関連するヘッダーがまだデータベースにない場合は、追加の親ヘッダーが渡されます。
func (beacon *Beacon) verifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header, ancestor *types.Header) (chan<- struct{}, <-chan error) {
	var (
		abort   = make(chan struct{})
		results = make(chan error, len(headers))
	)
	go func() {
		for i, header := range headers {
			var parent *types.Header
			if i == 0 {
				if ancestor != nil {
					parent = ancestor
				} else {
					parent = chain.GetHeader(headers[0].ParentHash, headers[0].Number.Uint64()-1)
				}
			} else if headers[i-1].Hash() == headers[i].ParentHash {
				parent = headers[i-1]
			}
			if parent == nil {
				select {
				case <-abort:
					return
				case results <- consensus.ErrUnknownAncestor:
				}
				continue
			}
			err := beacon.verifyHeader(chain, header, parent)
			select {
			case <-abort:
				return
			case results <- err:
			}
		}
	}()
	return abort, results
}

// Prepare implements consensus.Engine, initializing the difficulty field of a
// header to conform to the beacon protocol. The changes are done inline.
// Prepareはconsensus.Engineを実装し、ビーコンプロトコルに準拠するようにヘッダーの難易度フィールドを初期化します。
// 変更はインラインで行われます。
func (beacon *Beacon) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	// Transition isn't triggered yet, use the legacy rules for preparation.
	// 移行はまだトリガーされていません。準備には、従来のルールを使用してください。
	reached, err := IsTTDReached(chain, header.ParentHash, header.Number.Uint64()-1)
	if err != nil {
		return err
	}
	if !reached {
		return beacon.ethone.Prepare(chain, header)
	}
	header.Difficulty = beaconDifficulty
	return nil
}

// Finalize implements consensus.Engine, setting the final state on the header
// Finalizeはconsensus.Engineを実装し、ヘッダーに最終状態を設定します
func (beacon *Beacon) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header) {
	// Finalize is different with Prepare, it can be used in both block generation
	// and verification. So determine the consensus rules by header type.
	// FinalizeはPrepareとは異なり、ブロックの生成と検証の両方で使用できます。
	// したがって、ヘッダータイプごとにコンセンサスルールを決定します。
	if !beacon.IsPoSHeader(header) {
		beacon.ethone.Finalize(chain, header, state, txs, uncles)
		return
	}
	// The block reward is no longer handled here. It's done by the
	// external consensus engine.
	// ブロック報酬はここでは処理されなくなりました。 これは、外部コンセンサスエンジンによって行われます。
	header.Root = state.IntermediateRoot(true)
}

// FinalizeAndAssemble implements consensus.Engine, setting the final state and
// assembling the block.
// FinalizeAndAssembleは、consensus.Engineを実装し、最終状態を設定してブロックをアセンブルします。
func (beacon *Beacon) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	// FinalizeAndAssemble is different with Prepare, it can be used in both block
	// generation and verification. So determine the consensus rules by header type.
	// FinalizeAndAssembleはPrepareとは異なり、ブロックの生成と検証の両方で使用できます。
	// したがって、ヘッダータイプごとにコンセンサスルールを決定します。
	if !beacon.IsPoSHeader(header) {
		return beacon.ethone.FinalizeAndAssemble(chain, header, state, txs, uncles, receipts)
	}
	// Finalize and assemble the block
	// ブロックを完成させて組み立てます
	beacon.Finalize(chain, header, state, txs, uncles)
	return types.NewBlock(header, txs, uncles, receipts, trie.NewStackTrie(nil)), nil
}

// Seal generates a new sealing request for the given input block and pushes
// the result into the given channel.
//
// Note, the method returns immediately and will send the result async. More
// than one result may also be returned depending on the consensus algorithm.
// シールは、指定された入力ブロックに対して新しいシール要求を生成し、結果を指定されたチャネルにプッシュします。
//
// メソッドはすぐに戻り、結果を非同期で送信することに注意してください。
// コンセンサスアルゴリズムによっては、複数の結果が返される場合もあります。
func (beacon *Beacon) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	if !beacon.IsPoSHeader(block.Header()) {
		return beacon.ethone.Seal(chain, block, results, stop)
	}
	// The seal verification is done by the external consensus engine,
	// return directly without pushing any block back. In another word
	// beacon won't return any result by `results` channel which may
	// blocks the receiver logic forever.
	// シールの検証は外部コンセンサスエンジンによって行われ、ブロックを押し戻すことなく直接戻ります。
	// 言い換えると、ビーコンは、レシーバーロジックを永久にブロックする可能性のある `results`チャネルによる結果を返しません。
	return nil
}

// SealHash returns the hash of a block prior to it being sealed.
// SealHashは、ブロックがシールされる前のブロックのハッシュを返します。
func (beacon *Beacon) SealHash(header *types.Header) common.Hash {
	return beacon.ethone.SealHash(header)
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time
// given the parent block's time and difficulty.
// CalcDifficultyは難易度調整アルゴリズムです。
// 親ブロックの時間と難易度を考慮して、新しいブロックがその時点で作成されたときに持つべき難易度を返します。
func (beacon *Beacon) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	// Transition isn't triggered yet, use the legacy rules for calculation
	// 遷移はまだトリガーされていません、計算にはレガシールールを使用してください
	if reached, _ := IsTTDReached(chain, parent.Hash(), parent.Number.Uint64()); !reached {
		return beacon.ethone.CalcDifficulty(chain, time, parent)
	}
	return beaconDifficulty
}

// APIs implements consensus.Engine, returning the user facing RPC APIs.
// APIはconsensus.Engineを実装し、RPCAPIに直面しているユーザーを返します。
func (beacon *Beacon) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	return beacon.ethone.APIs(chain)
}

// Close shutdowns the consensus engine
// コンセンサスエンジンを閉じてシャットダウンします
func (beacon *Beacon) Close() error {
	return beacon.ethone.Close()
}

// IsPoSHeader reports the header belongs to the PoS-stage with some special fields.
// This function is not suitable for a part of APIs like Prepare or CalcDifficulty
// because the header difficulty is not set yet.
// IsPoSHeaderは、ヘッダーがいくつかの特別なフィールドを持つPoSステージに属していることを報告します。
// この関数は、ヘッダーの難易度がまだ設定されていないため、PrepareやCalcDifficultyなどの一部のAPIには適していません。
func (beacon *Beacon) IsPoSHeader(header *types.Header) bool {
	if header.Difficulty == nil {
		panic("IsPoSHeader called with invalid difficulty")
	}
	return header.Difficulty.Cmp(beaconDifficulty) == 0
}

// InnerEngine returns the embedded eth1 consensus engine.
// InnerEngineは、埋め込まれたeth1コンセンサスエンジンを返します。
func (beacon *Beacon) InnerEngine() consensus.Engine {
	return beacon.ethone
}

// SetThreads updates the mining threads. Delegate the call
// to the eth1 engine if it's threaded.
// SetThreadsはマイニングスレッドを更新します。 スレッド化されている場合は、呼び出しをeth1エンジンに委任します。
func (beacon *Beacon) SetThreads(threads int) {
	type threaded interface {
		SetThreads(threads int)
	}
	if th, ok := beacon.ethone.(threaded); ok {
		th.SetThreads(threads)
	}
}

// IsTTDReached checks if the TotalTerminalDifficulty has been surpassed on the `parentHash` block.
// It depends on the parentHash already being stored in the database.
// If the parentHash is not stored in the database a UnknownAncestor error is returned.
// IsTTDReachedは、 `parentHash`ブロックでTotalTerminalDifficultyを超えているかどうかをチェックします。
// データベースにすでに保存されているparentHashに依存します。
// parentHashがデータベースに保存されていない場合、UnknownAncestorエラーが返されます。
func IsTTDReached(chain consensus.ChainHeaderReader, parentHash common.Hash, number uint64) (bool, error) {
	if chain.Config().TerminalTotalDifficulty == nil {
		return false, nil
	}
	td := chain.GetTd(parentHash, number)
	if td == nil {
		return false, consensus.ErrUnknownAncestor
	}
	return td.Cmp(chain.Config().TerminalTotalDifficulty) >= 0, nil
}
