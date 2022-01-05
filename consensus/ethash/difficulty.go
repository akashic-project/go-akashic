// Copyright 2020 The go-ethereum Authors
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
	"math/big"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

const (
	// frontierDurationLimit is for Frontier:
	// The decision boundary on the blocktime duration used to determine
	// whether difficulty should go up or down.
	frontierDurationLimit = 13
	// minimumDifficulty The minimum that the difficulty may ever be.
	minimumDifficulty = 131072
	// expDiffPeriod is the exponential difficulty period
	expDiffPeriodUint = 100000
	// difficultyBoundDivisorBitShift is the bound divisor of the difficulty (2048),
	// This constant is the right-shifts to use for the division.
	difficultyBoundDivisor = 11
)

// CalcDifficultyFrontierU256 is the difficulty adjustment algorithm. It returns the
// difficulty that a new block should have when created at time given the parent
// block's time and difficulty. The calculation uses the Frontier rules.
func CalcDifficultyFrontierU256(time uint64, parent *types.Header) *big.Int {
	/*
		Algorithm
		block_diff = pdiff + pdiff / 2048 * (1 if time - ptime < 13 else -1) + int(2^((num // 100000) - 2))

		Where:
		- pdiff  = parent.difficulty
		- ptime = parent.time
		- time = block.timestamp
		- num = block.number
	*/

	pDiff, _ := uint256.FromBig(parent.Difficulty) // pDiff: pdiff
	adjust := pDiff.Clone()
	adjust.Rsh(adjust, difficultyBoundDivisor) // adjust: pDiff / 2048

	if time-parent.Time < frontierDurationLimit {
		pDiff.Add(pDiff, adjust)
	} else {
		pDiff.Sub(pDiff, adjust)
	}
	if pDiff.LtUint64(minimumDifficulty) {
		pDiff.SetUint64(minimumDifficulty)
	}
	// 'pdiff' now contains:
	// pdiff + pdiff / 2048 * (1 if time - ptime < 13 else -1)

	if periodCount := (parent.Number.Uint64() + 1) / expDiffPeriodUint; periodCount > 1 {
		// diff = diff + 2^(periodCount - 2)
		expDiff := adjust.SetOne()
		expDiff.Lsh(expDiff, uint(periodCount-2)) // expdiff: 2 ^ (periodCount -2)
		pDiff.Add(pDiff, expDiff)
	}
	return pDiff.ToBig()
}

// CalcDifficultyHomesteadU256 is the difficulty adjustment algorithm. It returns
// the difficulty that a new block should have when created at time given the
// parent block's time and difficulty. The calculation uses the Homestead rules.

// CalcDifficultyHomesteadU256は難易度調整アルゴリズムです。
// 親ブロックの時間と難易度を考慮して、新しいブロックがその時点で作成されたときに持つべき難易度を返します。
// 計算にはホームステッドルールが使用されます。
func CalcDifficultyHomesteadU256(time uint64, parent *types.Header) *big.Int {
	/*
		https://github.com/ethereum/EIPs/blob/master/EIPS/eip-2.md
		Algorithm:
		block_diff = pdiff + pdiff / 2048 * max(1 - (time - ptime) / 10, -99) + 2 ^ int((num / 100000) - 2))

		Our modification, to use unsigned ints:
		block_diff = pdiff - pdiff / 2048 * max((time - ptime) / 10 - 1, 99) + 2 ^ int((num / 100000) - 2))

		Where:
		- pdiff  = parent.difficulty
		- ptime = parent.time
		- time = block.timestamp
		- num = block.number
	*/

	pDiff, _ := uint256.FromBig(parent.Difficulty) // pDiff: pdiff
	adjust := pDiff.Clone()
	adjust.Rsh(adjust, difficultyBoundDivisor) // adjust: pDiff / 2048

	x := (time - parent.Time) / 10 // (time - ptime) / 10)
	var neg = true
	if x == 0 {
		x = 1
		neg = false
	} else if x >= 100 {
		x = 99
	} else {
		x = x - 1
	}
	z := new(uint256.Int).SetUint64(x)
	adjust.Mul(adjust, z) // adjust: (pdiff / 2048) * max((time - ptime) / 10 - 1, 99)
	if neg {
		pDiff.Sub(pDiff, adjust) // pdiff - pdiff / 2048 * max((time - ptime) / 10 - 1, 99)
	} else {
		pDiff.Add(pDiff, adjust) // pdiff + pdiff / 2048 * max((time - ptime) / 10 - 1, 99)
	}
	if pDiff.LtUint64(minimumDifficulty) {
		pDiff.SetUint64(minimumDifficulty)
	}
	// for the exponential factor, a.k.a "the bomb"
	// 指数因子の場合、別名「爆弾」
	// diff = diff + 2^(periodCount - 2)
	if periodCount := (1 + parent.Number.Uint64()) / expDiffPeriodUint; periodCount > 1 {
		expFactor := adjust.Lsh(adjust.SetOne(), uint(periodCount-2))
		pDiff.Add(pDiff, expFactor)
	}
	return pDiff.ToBig()
}

// MakeDifficultyCalculatorU256 creates a difficultyCalculator with the given bomb-delay.
// the difficulty is calculated with Byzantium rules, which differs from Homestead in
// how uncles affect the calculation
// MakeDifficultyCalculatorU256は、指定された爆弾遅延で難易​​度計算機を作成します。
//難易度はビザンチウムのルールで計算されます。これは、叔父が計算に与える影響がホームステッドとは異なります。
func MakeDifficultyCalculatorU256(bombDelay *big.Int) func(time uint64, parent *types.Header) *big.Int {
	// Note, the calculations below looks at the parent number, which is 1 below
	// the block number. Thus we remove one from the delay given
	// 以下の計算では、ブロック番号の1つ下にある親番号を調べていることに注意してください。
	// したがって、与えられた遅延から1つを削除します
	bombDelayFromParent := bombDelay.Uint64() - 1
	return func(time uint64, parent *types.Header) *big.Int {
		/*
			https://github.com/ethereum/EIPs/issues/100
			pDiff = parent.difficulty
			BLOCK_DIFF_FACTOR = 9
			a = pDiff + (pDiff // BLOCK_DIFF_FACTOR) * adj_factor
			b = min(parent.difficulty, MIN_DIFF)
			child_diff = max(a,b )
		*/
		x := (time - parent.Time) / 9 // (block_timestamp - parent_timestamp) // 9
		c := uint64(1)                // parent.unclehash == emptyUncleHashHashの場合 // if parent.unclehash == emptyUncleHashHash
		if parent.UncleHash != types.EmptyUncleHash {
			c = 2
		}
		xNeg := x >= c
		if xNeg {
			// x is now _negative_ adjustment factor
			// xは_negative_調整係数になりました
			x = x - c // - ( (t-p)/p -( 2 or 1) )
		} else {
			x = c - x // (2 or 1) - (t-p)/9
		}
		if x > 99 {
			x = 99 // max(x, 99)
		}
		// parent_diff + (parent_diff / 2048 * max((2 if len(parent.uncles) else 1) - ((timestamp - parent.timestamp) // 9), -99))
		y := new(uint256.Int)
		y.SetFromBig(parent.Difficulty)    // y: p_diff
		pDiff := y.Clone()                 // pdiff: p_diff
		z := new(uint256.Int).SetUint64(x) //z : +-adj_factor (either pos or negative)
		y.Rsh(y, difficultyBoundDivisor)   // y: p__diff / 2048
		z.Mul(y, z)                        // z: (p_diff / 2048 ) * (+- adj_factor)

		if xNeg {
			y.Sub(pDiff, z) // y: parent_diff + parent_diff/2048 * adjustment_factor
		} else {
			y.Add(pDiff, z) // y: parent_diff + parent_diff/2048 * adjustment_factor
		}
		// minimum difficulty can ever be (before exponential factor)
		// 最小の難易度は（指数因子の前に）あり得る
		if y.LtUint64(minimumDifficulty) {
			y.SetUint64(minimumDifficulty)
		}
		// calculate a fake block number for the ice-age delay
		// 氷河期の遅延の偽のブロック番号を計算します
		// Specification: https://eips.ethereum.org/EIPS/eip-1234
		var pNum = parent.Number.Uint64()
		if pNum >= bombDelayFromParent {
			if fakeBlockNumber := pNum - bombDelayFromParent; fakeBlockNumber >= 2*expDiffPeriodUint {
				z.SetOne()
				z.Lsh(z, uint(fakeBlockNumber/expDiffPeriodUint-2))
				y.Add(z, y)
			}
		}
		return y.ToBig()
	}
}
