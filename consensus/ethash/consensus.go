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

package ethash

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"time"

	mapset "github.com/deckarep/golang-set"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"golang.org/x/crypto/sha3"
)

// Ethash proof-of-work protocol constants.
// Ethashのプルーフオブワークプロトコル定数。
var (
	FrontierBlockReward, _        = big.NewInt(0).SetString("100000000000000000000", 10) // 100AKS // ブロックのマイニングに成功した場合のweiのブロック報酬                                // Block reward in wei for successfully mining a block
	ByzantiumBlockReward, _       = big.NewInt(0).SetString("100000000000000000000", 10) // 100AKS // ビザンチウムから上向きのブロックをうまく採掘したことに対するweiのブロック報酬          // Block reward in wei for successfully mining a block upward from Byzantium
	ConstantinopleBlockReward, _  = big.NewInt(0).SetString("100000000000000000000", 10) // 100AKS // コンスタンティノープルから上向きのブロックをうまく採掘したことに対するweiのブロック報酬 // Block reward in wei for successfully mining a block upward from Constantinople
	maxUncles                     = 2                                                    // 1つのブロックで許可される叔父の最大数                                               // Maximum number of uncles allowed in a single block
	allowedFutureBlockTimeSeconds = int64(15)                                            // ブロックに許可されている現在の時刻から、将来のブロックと見なされるまでの最大秒数        // Max seconds from current time allowed for blocks, before they're considered future blocks

	// calcDifficultyEip4345 is the difficulty adjustment algorithm as specified by EIP 4345.
	// It offsets the bomb a total of 10.7M blocks.
	// Specification EIP-4345: https://eips.ethereum.org/EIPS/eip-4345
	// calcDifficultyEip4345は、EIP4345で指定されている難易度調整アルゴリズムです。
	// 爆弾を合計10.7Mブロックオフセットします。
	// 仕様EIP-4345：https://eips.ethereum.org/EIPS/eip-4345
	calcDifficultyEip4345 = makeDifficultyCalculator(big.NewInt(10_700_000))

	// calcDifficultyEip3554 is the difficulty adjustment algorithm as specified by EIP 3554.
	// It offsets the bomb a total of 9.7M blocks.
	// Specification EIP-3554: https://eips.ethereum.org/EIPS/eip-3554
	// calcDifficultyEip3554は、EIP3554で指定されている難易度調整アルゴリズムです。
	// 爆弾を合計970万ブロックオフセットします。
	// 仕様EIP-3554： https://eips.ethereum.org/EIPS/eip-3554
	calcDifficultyEip3554 = makeDifficultyCalculator(big.NewInt(9700000))

	// calcDifficultyEip2384 is the difficulty adjustment algorithm as specified by EIP 2384.
	// It offsets the bomb 4M blocks from Constantinople, so in total 9M blocks.
	// Specification EIP-2384: https://eips.ethereum.org/EIPS/eip-2384
	// calcDifficultyEip2384は、EIP2384で指定されている難易度調整アルゴリズムです。
	//爆弾をコンスタンティノープルから4Mブロックオフセットするため、合計で9Mブロックになります。
	//仕様EIP-2384：https://eips.ethereum.org/EIPS/eip-2384
	calcDifficultyEip2384 = makeDifficultyCalculator(big.NewInt(9000000))

	// calcDifficultyConstantinople is the difficulty adjustment algorithm for Constantinople.
	// It returns the difficulty that a new block should have when created at time given the
	// parent block's time and difficulty. The calculation uses the Byzantium rules, but with
	// bomb offset 5M.
	// Specification EIP-1234: https://eips.ethereum.org/EIPS/eip-1234
	// calcDifficultyConstantinopleは、コンスタンティノープルの難易度調整アルゴリズムです。
	// 親ブロックの時間と難易度を考慮して、新しいブロックがその時点で作成されたときに持つべき難易度を返します。
	// 計算にはビザンチウムのルールが使用されていますが、爆弾のオフセットは5Mです。
	// 仕様EIP-1234：https://eips.ethereum.org/EIPS/eip-1234
	calcDifficultyConstantinople = makeDifficultyCalculator(big.NewInt(5000000))

	// calcDifficultyByzantium is the difficulty adjustment algorithm. It returns
	// the difficulty that a new block should have when created at time given the
	// parent block's time and difficulty. The calculation uses the Byzantium rules.
	// Specification EIP-649: https://eips.ethereum.org/EIPS/eip-649
	// calcDifficultyByzantiumは、難易度調整アルゴリズムです。
	// 親ブロックの時間と難易度を考慮して、新しいブロックがその時点で作成されたときに持つべき難易度を返します。
	// 計算にはビザンチウムのルールが使用されます。
	// 仕様EIP-649：https://eips.ethereum.org/EIPS/eip-649
	calcDifficultyByzantium = makeDifficultyCalculator(big.NewInt(3000000))
)

// Various error messages to mark blocks invalid. These should be private to
// prevent engine specific errors from being referenced in the remainder of the
// codebase, inherently breaking if the engine is swapped out. Please put common
// error types into the consensus package.
// ブロックを無効としてマークするためのさまざまなエラーメッセージ。
// これらは、エンジン固有のエラーがコードベースの残りの部分で参照されないようにするためにプライベートにする必要があります。
// これは、エンジンがスワップアウトされた場合に本質的に壊れます。
// 一般的なエラータイプをコンセンサスパッケージに入れてください。
var (
	errOlderBlockTime    = errors.New("timestamp older than parent")    // 親より古いタイムスタンプ
	errTooManyUncles     = errors.New("too many uncles")                // おじさんが多すぎる
	errDuplicateUncle    = errors.New("duplicate uncle")                // 重複した叔父
	errUncleIsAncestor   = errors.New("uncle is ancestor")              // おじは祖先です
	errDanglingUncle     = errors.New("uncle's parent is not ancestor") // 叔父の親は祖先ではありません
	errInvalidDifficulty = errors.New("non-positive difficulty")        // 非正の難しさ
	errInvalidMixDigest  = errors.New("invalid mix digest")             // 無効なミックスダイジェスト
	errInvalidPoW        = errors.New("invalid proof-of-work")          // 無効なプルーフオブワーク
)

// Author implements consensus.Engine, returning the header's coinbase as the
// proof-of-work verified author of the block.
// 作成者はconsensus.Engineを実装し、ヘッダーのコインベースをブロックのプルーフオブワーク検証済み作成者として返します。
func (ethash *Ethash) Author(header *types.Header) (common.Address, error) {
	return header.Coinbase, nil
}

// VerifyHeader checks whether a header conforms to the consensus rules of the
// stock Ethereum ethash engine.
// VerificationHeaderは、ヘッダーがストックのイーサリアムエタッシュエンジンのコンセンサスルールに準拠しているかどうかを確認します。
func (ethash *Ethash) VerifyHeader(chain consensus.ChainHeaderReader, header *types.Header, seal bool) error {
	// If we're running a full engine faking, accept any input as valid
	// フルエンジンの偽造を実行している場合は、すべての入力を有効なものとして受け入れます
	if ethash.config.PowMode == ModeFullFake {
		return nil
	}
	// Short circuit if the header is known, or its parent not
	// ヘッダーがわかっている場合、またはその親がわかっていない場合は短絡
	number := header.Number.Uint64()
	if chain.GetHeader(header.Hash(), number) != nil {
		return nil
	}
	parent := chain.GetHeader(header.ParentHash, number-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	// Sanity checks passed, do a proper verification
	// 健全性チェックに合格し、適切な検証を行う
	return ethash.verifyHeader(chain, header, parent, false, seal, time.Now().Unix())
}

// VerifyHeaders is similar to VerifyHeader, but verifies a batch of headers
// concurrently. The method returns a quit channel to abort the operations and
// a results channel to retrieve the async verifications.
// VerificationHeadersはVerifyHeaderに似ていますが、ヘッダーのバッチを同時に検証します。
// このメソッドは、操作を中止するための終了チャネルと、非同期検証を取得するための結果チャネルを返します。
func (ethash *Ethash) VerifyHeaders(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool) (chan<- struct{}, <-chan error) {
	// If we're running a full engine faking, accept any input as valid
	// フルエンジンの偽造を実行している場合は、すべての入力を有効なものとして受け入れます
	if ethash.config.PowMode == ModeFullFake || len(headers) == 0 {
		abort, results := make(chan struct{}), make(chan error, len(headers))
		for i := 0; i < len(headers); i++ {
			results <- nil
		}
		return abort, results
	}

	// Spawn as many workers as allowed threads
	// 許可されたスレッドと同じ数のワーカーを生成します
	workers := runtime.GOMAXPROCS(0)
	if len(headers) < workers {
		workers = len(headers)
	}

	// Create a task channel and spawn the verifiers
	// タスクチャネルを作成し、ベリファイアを生成します
	var (
		inputs  = make(chan int)
		done    = make(chan int, workers)
		errors  = make([]error, len(headers))
		abort   = make(chan struct{})
		unixNow = time.Now().Unix()
	)
	for i := 0; i < workers; i++ {
		go func() {
			for index := range inputs {
				errors[index] = ethash.verifyHeaderWorker(chain, headers, seals, index, unixNow)
				done <- index
			}
		}()
	}

	errorsOut := make(chan error, len(headers))
	go func() {
		defer close(inputs)
		var (
			in, out = 0, 0
			checked = make([]bool, len(headers))
			inputs  = inputs
		)
		for {
			select {
			case inputs <- in:
				if in++; in == len(headers) {
					// Reached end of headers. Stop sending to workers.
					// ヘッダーの終わりに到達しました。労働者への送信を停止します。
					inputs = nil
				}
			case index := <-done:
				for checked[index] = true; checked[out]; out++ {
					errorsOut <- errors[out]
					if out == len(headers)-1 {
						return
					}
				}
			case <-abort:
				return
			}
		}
	}()
	return abort, errorsOut
}

func (ethash *Ethash) verifyHeaderWorker(chain consensus.ChainHeaderReader, headers []*types.Header, seals []bool, index int, unixNow int64) error {
	var parent *types.Header
	if index == 0 {
		parent = chain.GetHeader(headers[0].ParentHash, headers[0].Number.Uint64()-1)
	} else if headers[index-1].Hash() == headers[index].ParentHash {
		parent = headers[index-1]
	}
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	return ethash.verifyHeader(chain, headers[index], parent, false, seals[index], unixNow)
}

// VerifyUncles verifies that the given block's uncles conform to the consensus
// rules of the stock Ethereum ethash engine.
// VerificationUnclesは、指定されたブロックの叔父がストックイーサリアムエタッシュエンジンのコンセンサスルールに準拠していることを確認します。
func (ethash *Ethash) VerifyUncles(chain consensus.ChainReader, block *types.Block) error {
	// If we're running a full engine faking, accept any input as valid
	// フルエンジンの偽造を実行している場合は、すべての入力を有効なものとして受け入れます
	if ethash.config.PowMode == ModeFullFake {
		return nil
	}
	// Verify that there are at most 2 uncles included in this block
	// このブロックに含まれる叔父が最大2人であることを確認します
	if len(block.Uncles()) > maxUncles {
		return errTooManyUncles
	}
	if len(block.Uncles()) == 0 {
		return nil
	}
	// Gather the set of past uncles and ancestors
	// 過去の叔父と祖先のセットを収集します
	uncles, ancestors := mapset.NewSet(), make(map[common.Hash]*types.Header)

	number, parent := block.NumberU64()-1, block.ParentHash()
	for i := 0; i < 7; i++ {
		ancestorHeader := chain.GetHeader(parent, number)
		if ancestorHeader == nil {
			break
		}
		ancestors[parent] = ancestorHeader
		// If the ancestor doesn't have any uncles, we don't have to iterate them
		// 祖先に叔父がいない場合は、それらを繰り返す必要はありません
		if ancestorHeader.UncleHash != types.EmptyUncleHash {
			// Need to add those uncles to the banned list too
			// それらの叔父も禁止リストに追加する必要があります
			ancestor := chain.GetBlock(parent, number)
			if ancestor == nil {
				break
			}
			for _, uncle := range ancestor.Uncles() {
				uncles.Add(uncle.Hash())
			}
		}
		parent, number = ancestorHeader.ParentHash, number-1
	}
	ancestors[block.Hash()] = block.Header()
	uncles.Add(block.Hash())

	// Verify each of the uncles that it's recent, but not an ancestor
	// それぞれの叔父が最近のものであるが、祖先ではないことを確認します
	for _, uncle := range block.Uncles() {
		// Make sure every uncle is rewarded only once
		// すべての叔父が一度だけ報われることを確認してください
		hash := uncle.Hash()
		if uncles.Contains(hash) {
			return errDuplicateUncle
		}
		uncles.Add(hash)

		// Make sure the uncle has a valid ancestry
		// おじが有効な祖先を持っていることを確認してください
		if ancestors[hash] != nil {
			return errUncleIsAncestor
		}
		if ancestors[uncle.ParentHash] == nil || uncle.ParentHash == block.ParentHash() {
			return errDanglingUncle
		}
		if err := ethash.verifyHeader(chain, uncle, ancestors[uncle.ParentHash], true, true, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// verifyHeader checks whether a header conforms to the consensus rules of the
// stock Ethereum ethash engine.
// See YP section 4.3.4. "Block Header Validity"
// verifyHeaderは、ヘッダーがストックのイーサリアムethashエンジンのコンセンサスルールに準拠しているかどうかを確認します。
// YPセクション4.3.4を参照してください。 「ブロックヘッダーの有効性」
func (ethash *Ethash) verifyHeader(chain consensus.ChainHeaderReader, header, parent *types.Header, uncle bool, seal bool, unixNow int64) error {
	// Ensure that the header's extra-data section is of a reasonable size
	// ヘッダーの追加データセクションが適切なサイズであることを確認します
	if uint64(len(header.Extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("extra-data too long: %d > %d", len(header.Extra), params.MaximumExtraDataSize)
	}
	// Verify the header's timestamp
	// ヘッダーのタイムスタンプを確認します
	if !uncle {
		if header.Time > uint64(unixNow+allowedFutureBlockTimeSeconds) {
			return consensus.ErrFutureBlock
		}
	}
	if header.Time <= parent.Time {
		return errOlderBlockTime
	}
	// Verify the block's difficulty based on its timestamp and parent's difficulty
	// タイムスタンプと親の難易度に基づいて、ブロックの難易度を確認します
	expected := ethash.CalcDifficulty(chain, header.Time, parent)

	if expected.Cmp(header.Difficulty) != 0 {
		return fmt.Errorf("invalid difficulty: have %v, want %v", header.Difficulty, expected)
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
	// Verify the block's gas usage and (if applicable) verify the base fee.
	// ブロックのガス使用量を確認し、（該当する場合）基本料金を確認します。
	if !chain.Config().IsLondon(header.Number) {
		// Verify BaseFee not present before EIP-1559 fork.
		// EIP-1559フォークの前にBaseFeeが存在しないことを確認します。
		if header.BaseFee != nil {
			return fmt.Errorf("invalid baseFee before fork: have %d, expected 'nil'", header.BaseFee)
		}
		if err := misc.VerifyGaslimit(parent.GasLimit, header.GasLimit); err != nil {
			return err
		}
	} else if err := misc.VerifyEip1559Header(chain.Config(), parent, header); err != nil {
		// Verify the header's EIP-1559 attributes.
		// ヘッダーのEIP-1559属性を確認します。
		return err
	}
	// Verify that the block number is parent's +1
	// ブロック番号が親の+1であることを確認します
	if diff := new(big.Int).Sub(header.Number, parent.Number); diff.Cmp(big.NewInt(1)) != 0 {
		return consensus.ErrInvalidNumber
	}
	// Verify the engine specific seal securing the block
	// ブロックを固定しているエンジン固有のシールを確認します
	if seal {
		if err := ethash.verifySeal(chain, header, false); err != nil {
			return err
		}
	}
	// If all checks passed, validate any special fields for hard forks
	// すべてのチェックに合格したら、ハードフォークの特別なフィールドを検証します
	if err := misc.VerifyDAOHeaderExtraData(chain.Config(), header); err != nil {
		return err
	}
	if err := misc.VerifyForkHashes(chain.Config(), header, uncle); err != nil {
		return err
	}
	return nil
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time
// given the parent block's time and difficulty.
// CalcDifficultyは、難易度調整アルゴリズムです。
// 親ブロックの時間と難易度を考慮して、新しいブロックがその時点で作成されたときに持つべき難易度を返します。
func (ethash *Ethash) CalcDifficulty(chain consensus.ChainHeaderReader, time uint64, parent *types.Header) *big.Int {
	return CalcDifficulty(chain.Config(), time, parent)
}

// CalcDifficulty is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time
// given the parent block's time and difficulty.
// CalcDifficultyは、難易度調整アルゴリズムです。
// 親ブロックの時間と難易度を考慮して、新しいブロックがその時点で作成されたときに持つべき難易度を返します。
func CalcDifficulty(config *params.ChainConfig, time uint64, parent *types.Header) *big.Int {
	next := new(big.Int).Add(parent.Number, big1)
	switch {
	case config.IsArrowGlacier(next):
		return calcDifficultyEip4345(time, parent)
	case config.IsLondon(next):
		return calcDifficultyEip3554(time, parent)
	case config.IsMuirGlacier(next):
		return calcDifficultyEip2384(time, parent)
	case config.IsConstantinople(next):
		return calcDifficultyConstantinople(time, parent)
	case config.IsByzantium(next):
		return calcDifficultyByzantium(time, parent)
	case config.IsHomestead(next):
		return calcDifficultyHomestead(time, parent)
	default:
		return calcDifficultyFrontier(time, parent)
	}
}

// Some weird constants to avoid constant memory allocs for them.
// それらに一定のメモリ割り当てを回避するためのいくつかの奇妙な定数。
var (
	expDiffPeriod = big.NewInt(100000)
	big1          = big.NewInt(1)
	big2          = big.NewInt(2)
	big9          = big.NewInt(9)
	big10         = big.NewInt(10)
	bigMinus99    = big.NewInt(-99)
)

// makeDifficultyCalculator creates a difficultyCalculator with the given bomb-delay.
// the difficulty is calculated with Byzantium rules, which differs from Homestead in
// how uncles affect the calculation
// makeDifficultyCalculatorは、指定された爆弾遅延を使用してdifferityCalculatorを作成します。
// 難易度はビザンチウムのルールで計算されます。
// これは、叔父が計算に与える影響がホームステッドとは異なります。
func makeDifficultyCalculator(bombDelay *big.Int) func(time uint64, parent *types.Header) *big.Int {
	// Note, the calculations below looks at the parent number, which is 1 below
	// the block number. Thus we remove one from the delay given
	// 以下の計算では、ブロック番号の1つ下にある親番号を調べていることに注意してください。
	// したがって、与えられた遅延から1つを削除します
	bombDelayFromParent := new(big.Int).Sub(bombDelay, big1)
	return func(time uint64, parent *types.Header) *big.Int {
		// https://github.com/ethereum/EIPs/issues/100.
		// algorithm:
		// アルゴリズム:
		// diff = (parent_diff +
		//         (parent_diff / 2048 * max((2 if len(parent.uncles) else 1) - ((timestamp - parent.timestamp) // 9), -99))
		//        ) + 2^(periodCount - 2)

		bigTime := new(big.Int).SetUint64(time)
		bigParentTime := new(big.Int).SetUint64(parent.Time)

		// holds intermediate values to make the algo easier to read & audit
		// アルゴを読みやすく監査しやすくするための中間値を保持します
		x := new(big.Int)
		y := new(big.Int)

		// (2 if len(parent_uncles) else 1) - (block_timestamp - parent_timestamp) // 9
		x.Sub(bigTime, bigParentTime)
		x.Div(x, big9)
		if parent.UncleHash == types.EmptyUncleHash {
			x.Sub(big1, x)
		} else {
			x.Sub(big2, x)
		}
		// max((2 if len(parent_uncles) else 1) - (block_timestamp - parent_timestamp) // 9, -99)
		if x.Cmp(bigMinus99) < 0 {
			x.Set(bigMinus99)
		}
		// parent_diff + (parent_diff / 2048 * max((2 if len(parent.uncles) else 1) - ((timestamp - parent.timestamp) // 9), -99))
		y.Div(parent.Difficulty, params.DifficultyBoundDivisor)
		x.Mul(y, x)
		x.Add(parent.Difficulty, x)

		// minimum difficulty can ever be (before exponential factor)
		// 最小の難易度は（指数因子の前に）あり得る
		if x.Cmp(params.MinimumDifficulty) < 0 {
			x.Set(params.MinimumDifficulty)
		}
		// calculate a fake block number for the ice-age delay
		// Specification: https://eips.ethereum.org/EIPS/eip-1234
		//氷河期の遅延の偽のブロック番号を計算します
		// 仕様：https://eips.ethereum.org/EIPS/eip-1234
		fakeBlockNumber := new(big.Int)
		if parent.Number.Cmp(bombDelayFromParent) >= 0 {
			fakeBlockNumber = fakeBlockNumber.Sub(parent.Number, bombDelayFromParent)
		}
		// for the exponential factor
		// 指数因子について
		periodCount := fakeBlockNumber
		periodCount.Div(periodCount, expDiffPeriod)

		// the exponential factor, commonly referred to as "the bomb"
		// 一般に「爆弾」と呼ばれる指数因子
		// diff = diff + 2^(periodCount - 2)
		if periodCount.Cmp(big1) > 0 {
			y.Sub(periodCount, big2)
			y.Exp(big2, y, nil)
			x.Add(x, y)
		}
		return x
	}
}

// calcDifficultyHomestead is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time given the
// parent block's time and difficulty. The calculation uses the Homestead rules.
// calcDifficultyHomesteadは、難易度調整アルゴリズムです。
// 親ブロックの時間と難易度を考慮して、新しいブロックがその時点で作成されたときに持つべき難易度を返します。
// 計算にはホームステッドルールが使用されます。
func calcDifficultyHomestead(time uint64, parent *types.Header) *big.Int {
	// https://github.com/ethereum/EIPs/blob/master/EIPS/eip-2.md
	// algorithm:
	// アルゴリズム：
	// diff = (parent_diff +
	//         (parent_diff / 2048 * max(1 - (block_timestamp - parent_timestamp) // 10, -99))
	//        ) + 2^(periodCount - 2)

	bigTime := new(big.Int).SetUint64(time)
	bigParentTime := new(big.Int).SetUint64(parent.Time)

	// holds intermediate values to make the algo easier to read & audit
	// アルゴを読みやすく監査しやすくするために中間値を保持します
	x := new(big.Int)
	y := new(big.Int)

	// 1 - (block_timestamp - parent_timestamp) // 10
	x.Sub(bigTime, bigParentTime)
	x.Div(x, big10)
	x.Sub(big1, x)

	// max(1 - (block_timestamp - parent_timestamp) // 10, -99)
	if x.Cmp(bigMinus99) < 0 {
		x.Set(bigMinus99)
	}
	// (parent_diff + parent_diff // 2048 * max(1 - (block_timestamp - parent_timestamp) // 10, -99))
	y.Div(parent.Difficulty, params.DifficultyBoundDivisor)
	x.Mul(y, x)
	x.Add(parent.Difficulty, x)

	// minimum difficulty can ever be (before exponential factor)
	// 最小の難易度は（指数因子の前に）あり得ます
	if x.Cmp(params.MinimumDifficulty) < 0 {
		x.Set(params.MinimumDifficulty)
	}
	// for the exponential factor
	// 指数因子について
	periodCount := new(big.Int).Add(parent.Number, big1)
	periodCount.Div(periodCount, expDiffPeriod)

	// the exponential factor, commonly referred to as "the bomb"
	// 一般に「爆弾」と呼ばれる指数因子
	// diff = diff + 2^(periodCount - 2)
	if periodCount.Cmp(big1) > 0 {
		y.Sub(periodCount, big2)
		y.Exp(big2, y, nil)
		x.Add(x, y)
	}
	return x
}

// calcDifficultyFrontier is the difficulty adjustment algorithm. It returns the
// difficulty that a new block should have when created at time given the parent
// block's time and difficulty. The calculation uses the Frontier rules.
// calcDifficultyFrontierは、難易度調整アルゴリズムです。
// 親ブロックの時間と難易度を考慮して、新しいブロックがその時点で作成されたときに持つべき難易度を返します。
// 計算にはフロンティアルールが使用されます。
func calcDifficultyFrontier(time uint64, parent *types.Header) *big.Int {
	diff := new(big.Int)
	adjust := new(big.Int).Div(parent.Difficulty, params.DifficultyBoundDivisor)
	bigTime := new(big.Int)
	bigParentTime := new(big.Int)

	bigTime.SetUint64(time)
	bigParentTime.SetUint64(parent.Time)

	if bigTime.Sub(bigTime, bigParentTime).Cmp(params.DurationLimit) < 0 {
		diff.Add(parent.Difficulty, adjust)
	} else {
		diff.Sub(parent.Difficulty, adjust)
	}
	if diff.Cmp(params.MinimumDifficulty) < 0 {
		diff.Set(params.MinimumDifficulty)
	}

	periodCount := new(big.Int).Add(parent.Number, big1)
	periodCount.Div(periodCount, expDiffPeriod)
	if periodCount.Cmp(big1) > 0 {
		// diff = diff + 2^(periodCount - 2)
		expDiff := periodCount.Sub(periodCount, big2)
		expDiff.Exp(big2, expDiff, nil)
		diff.Add(diff, expDiff)
		diff = math.BigMax(diff, params.MinimumDifficulty)
	}
	return diff
}

// Exported for fuzzing
// ファジング用にエクスポート
var FrontierDifficultyCalulator = calcDifficultyFrontier
var HomesteadDifficultyCalulator = calcDifficultyHomestead
var DynamicDifficultyCalculator = makeDifficultyCalculator

// verifySeal checks whether a block satisfies the PoW difficulty requirements,
// either using the usual ethash cache for it, or alternatively using a full DAG
// to make remote mining fast.
// verifySealは、ブロックがPoWの難易度の要件を満たしているかどうかをチェックします。
// 通常のethashキャッシュを使用するか、フルDAGを使用してリモートマイニングを高速化します。
func (ethash *Ethash) verifySeal(chain consensus.ChainHeaderReader, header *types.Header, fulldag bool) error {
	// If we're running a fake PoW, accept any seal as valid
	// 偽のPoWを実行している場合は、任意のシールを有効なものとして受け入れます
	if ethash.config.PowMode == ModeFake || ethash.config.PowMode == ModeFullFake {
		time.Sleep(ethash.fakeDelay)
		if ethash.fakeFail == header.Number.Uint64() {
			return errInvalidPoW
		}
		return nil
	}
	// If we're running a shared PoW, delegate verification to it
	// 共有PoWを実行している場合は、検証を委任します
	if ethash.shared != nil {
		return ethash.shared.verifySeal(chain, header, fulldag)
	}
	// Ensure that we have a valid difficulty for the block
	// ブロックに有効な難易度があることを確認してください
	if header.Difficulty.Sign() <= 0 {
		return errInvalidDifficulty
	}
	// Recompute the digest and PoW values
	// ダイジェスト値とPoW値を再計算します
	number := header.Number.Uint64()

	var (
		digest []byte
		result []byte
	)
	// If fast-but-heavy PoW verification was requested, use an ethash dataset
	// 高速だが重いPoW検証が要求された場合は、ethashデータセットを使用します
	if fulldag {
		dataset := ethash.dataset(number, true)
		if dataset.generated() {
			digest, result = hashimotoFull(dataset.dataset, ethash.SealHash(header).Bytes(), header.Nonce.Uint64())

			// Datasets are unmapped in a finalizer. Ensure that the dataset stays alive
			// until after the call to hashimotoFull so it's not unmapped while being used.
			// データセットはファイナライザーでマップ解除されます。
			// データセットがhashimotoFullの呼び出し後まで存続していることを確認して、使用中にマップが解除されないようにします。
			runtime.KeepAlive(dataset)
		} else {
			// Dataset not yet generated, don't hang, use a cache instead
			// データセットはまだ生成されていません。
			// ハングしないでください。代わりにキャッシュを使用してください
			fulldag = false
		}
	}
	// If slow-but-light PoW verification was requested (or DAG not yet ready), use an ethash cache
	// 遅いが軽いPoW検証が要求された場合（またはDAGの準備ができていない場合）、ethashキャッシュを使用します
	if !fulldag {
		cache := ethash.cache(number)

		size := datasetSize(number)
		if ethash.config.PowMode == ModeTest {
			size = 32 * 1024
		}
		digest, result = hashimotoLight(size, cache.cache, ethash.SealHash(header).Bytes(), header.Nonce.Uint64())

		// Caches are unmapped in a finalizer. Ensure that the cache stays alive
		// until after the call to hashimotoLight so it's not unmapped while being used.
		// キャッシュはファイナライザーでマップ解除されます。
		// 使用中にマップが解除されないように、hashimotoLightの呼び出しが完了するまでキャッシュが有効であることを確認してください。
		runtime.KeepAlive(cache)
	}
	// Verify the calculated values against the ones provided in the header
	// ヘッダーで提供されている値に対して計算値を確認します
	if !bytes.Equal(header.MixDigest[:], digest) {
		return errInvalidMixDigest
	}
	target := new(big.Int).Div(two256, header.Difficulty)
	if new(big.Int).SetBytes(result).Cmp(target) > 0 {
		return errInvalidPoW
	}
	return nil
}

// Prepare implements consensus.Engine, initializing the difficulty field of a
// header to conform to the ethash protocol. The changes are done inline.
// Prepareはconsensus.Engineを実装し、ヘッダーの難易度フィールドを初期化してethashプロトコルに準拠させます。
// 変更はインラインで行われます。
func (ethash *Ethash) Prepare(chain consensus.ChainHeaderReader, header *types.Header) error {
	parent := chain.GetHeader(header.ParentHash, header.Number.Uint64()-1)
	if parent == nil {
		return consensus.ErrUnknownAncestor
	}
	header.Difficulty = ethash.CalcDifficulty(chain, header.Time, parent)
	return nil
}

// Finalize implements consensus.Engine, accumulating the block and uncle rewards,
// setting the final state on the header
// Finalizeはconsensus.Engineを実装し、ブロックと叔父の報酬を蓄積し、ヘッダーに最終状態を設定します
func (ethash *Ethash) Finalize(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header) {
	// Accumulate any block and uncle rewards and commit the final state root
	// ブロックと叔父の報酬を蓄積し、最終的な状態のルートをコミットします
	accumulateRewards(chain.Config(), state, header, uncles)
	header.Root = state.IntermediateRoot(chain.Config().IsEIP158(header.Number))
}

// FinalizeAndAssemble implements consensus.Engine, accumulating the block and
// uncle rewards, setting the final state and assembling the block.
// FinalizeAndAssembleは、consensus.Engineを実装し、ブロックと叔父の報酬を蓄積し、最終状態を設定してブロックを組み立てます。
func (ethash *Ethash) FinalizeAndAssemble(chain consensus.ChainHeaderReader, header *types.Header, state *state.StateDB, txs []*types.Transaction, uncles []*types.Header, receipts []*types.Receipt) (*types.Block, error) {
	// Finalize block
	// ブロックをファイナライズ
	ethash.Finalize(chain, header, state, txs, uncles)

	// Header seems complete, assemble into a block and return
	// ヘッダーが完成しているようです。ブロックにアセンブルして戻ります
	return types.NewBlock(header, txs, uncles, receipts, trie.NewStackTrie(nil)), nil
}

// SealHash returns the hash of a block prior to it being sealed.
// SealHashは、ブロックがシールされる前のブロックのハッシュを返します。
func (ethash *Ethash) SealHash(header *types.Header) (hash common.Hash) {
	hasher := sha3.NewLegacyKeccak256()

	enc := []interface{}{
		header.ParentHash,
		header.UncleHash,
		header.Coinbase,
		header.Root,
		header.TxHash,
		header.ReceiptHash,
		header.Bloom,
		header.Difficulty,
		header.Number,
		header.GasLimit,
		header.GasUsed,
		header.Time,
		header.Extra,
	}
	if header.BaseFee != nil {
		enc = append(enc, header.BaseFee)
	}
	rlp.Encode(hasher, enc)
	hasher.Sum(hash[:0])
	return hash
}

// Some weird constants to avoid constant memory allocs for them.
// それらに一定のメモリ割り当てを回避するためのいくつかの奇妙な定数。
var (
	big8  = big.NewInt(8)
	big32 = big.NewInt(32)
)

// AccumulateRewards credits the coinbase of the given block with the mining
// reward. The total reward consists of the static block reward and rewards for
// included uncles. The coinbase of each uncle block is also rewarded.
// AccumulateRewardsは、指定されたブロックのコインベースにマイニング報酬をクレジットします。
// 合計報酬は、静的ブロック報酬と含まれている叔父の報酬で構成されます。
// 各叔父ブロックのコインベースも報われます。
func accumulateRewards(config *params.ChainConfig, state *state.StateDB, header *types.Header, uncles []*types.Header) {
	// Select the correct block reward based on chain progression
	// チェーンの進行に基づいて正しいブロック報酬を選択します
	blockReward := FrontierBlockReward
	if config.IsByzantium(header.Number) {
		blockReward = ByzantiumBlockReward
	}
	if config.IsConstantinople(header.Number) {
		blockReward = ConstantinopleBlockReward
	}
	// Accumulate the rewards for the miner and any included uncles
	// 鉱夫と含まれている叔父の報酬を累積します
	reward := new(big.Int).Set(blockReward)
	r := new(big.Int)
	for _, uncle := range uncles {
		r.Add(uncle.Number, big8)
		r.Sub(r, header.Number)
		r.Mul(r, blockReward)
		r.Div(r, big8)
		state.AddBalance(uncle.Coinbase, r)

		r.Div(blockReward, big32)
		reward.Add(reward, r)
	}
	state.AddBalance(header.Coinbase, reward)
}
