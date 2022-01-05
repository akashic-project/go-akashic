// Copyright 2016 The go-ethereum Authors
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

package miner

import (
	"container/ring"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
)

// chainRetriever is used by the unconfirmed block set to verify whether a previously
// mined block is part of the canonical chain or not.
// chainRetrieverは、以前にマイニングされたブロックが正規チェーンの一部であるかどうかを検証するために、未確認のブロックセットによって使用されます。
type chainRetriever interface {
	// GetHeaderByNumber retrieves the canonical header associated with a block number.
	// GetHeaderByNumberは、ブロック番号に関連付けられた正規ヘッダーを取得します。
	GetHeaderByNumber(number uint64) *types.Header

	// GetBlockByNumber retrieves the canonical block associated with a block number.
	// GetBlockByNumberは、ブロック番号に関連付けられた正規ブロックを取得します。
	GetBlockByNumber(number uint64) *types.Block
}

// unconfirmedBlock is a small collection of metadata about a locally mined block
// that is placed into a unconfirmed set for canonical chain inclusion tracking.
// unconfirmedBlockは、ローカルでマイニングされたブロックに関するメタデータの小さなコレクションであり、
// 正規のチェーン包含追跡のために未確認のセットに配置されます。
type unconfirmedBlock struct {
	index uint64
	hash  common.Hash
}

// unconfirmedBlocks implements a data structure to maintain locally mined blocks
// have not yet reached enough maturity to guarantee chain inclusion. It is
// used by the miner to provide logs to the user when a previously mined block
// has a high enough guarantee to not be reorged out of the canonical chain.

// unconfirmedBlocksは、ローカルでマイニングされたブロックがチェーンの包含を保証するのに十分な成熟度に達していないことを
// 維持するためのデータ構造を実装します。
// これは、以前にマイニングされたブロックが正規チェーンから再編成されないほど十分に高い保証がある場合に、
// マイナーがユーザーにログを提供するために使用されます。
type unconfirmedBlocks struct {
	chain  chainRetriever // 正規のステータスを確認するためのブロックチェーン // Blockchain to verify canonical status through
	depth  uint           // 前のブロックを破棄するまでの深さ               // Depth after which to discard previous blocks
	blocks *ring.Ring     // 情報をブロックして、正規のチェーンクロスチェックを許可します// Block infos to allow canonical chain cross checks
	lock   sync.Mutex     //フィールドを同時アクセスから保護します                     // Protects the fields from concurrent access
}

// newUnconfirmedBlocks returns new data structure to track currently unconfirmed blocks.
// newUnconfirmedBlocksは、現在未確認のブロックを追跡するための新しいデータ構造を返します。
func newUnconfirmedBlocks(chain chainRetriever, depth uint) *unconfirmedBlocks {
	return &unconfirmedBlocks{
		chain: chain,
		depth: depth,
	}
}

// Insert adds a new block to the set of unconfirmed ones.
// Insertは、未確認のブロックのセットに新しいブロックを追加します。
func (set *unconfirmedBlocks) Insert(index uint64, hash common.Hash) {
	// If a new block was mined locally, shift out any old enough blocks
	// 新しいブロックがローカルでマイニングされた場合は、十分に古いブロックをシフトアウトします
	set.Shift(index)

	// Create the new item as its own ring
	// 新しいアイテムを独自のリングとして作成します
	item := ring.New(1)
	item.Value = &unconfirmedBlock{
		index: index,
		hash:  hash,
	}
	// Set as the initial ring or append to the end
	// 最初のリングとして設定するか、最後に追加します
	set.lock.Lock()
	defer set.lock.Unlock()

	if set.blocks == nil {
		set.blocks = item
	} else {
		set.blocks.Move(-1).Link(item)
	}
	// Display a log for the user to notify of a new mined block unconfirmed
	// ユーザーが新しいマイニングされたブロックが未確認であることを通知するためのログを表示します
	log.Info("🔨 mined potential block", "number", index, "hash", hash)
}

// Shift drops all unconfirmed blocks from the set which exceed the unconfirmed sets depth
// allowance, checking them against the canonical chain for inclusion or staleness
// report.
// Shiftは、未確認のセットの深さの許容値を超えるすべての未確認のブロックをセットから削除し、包含または失効レポートの正規チェーンと照合します。
func (set *unconfirmedBlocks) Shift(height uint64) {
	set.lock.Lock()
	defer set.lock.Unlock()

	for set.blocks != nil {
		// Retrieve the next unconfirmed block and abort if too fresh
		// 次の未確認のブロックを取得し、新鮮すぎる場合は中止します
		next := set.blocks.Value.(*unconfirmedBlock)
		if next.index+uint64(set.depth) > height {
			break
		}
		// Block seems to exceed depth allowance, check for canonical status
		// ブロックが深度許容値を超えているようです。正規のステータスを確認してください
		header := set.chain.GetHeaderByNumber(next.index)
		switch {
		case header == nil:
			log.Warn("Failed to retrieve header of mined block", "number", next.index, "hash", next.hash)
		case header.Hash() == next.hash:
			log.Info("🔗 block reached canonical chain", "number", next.index, "hash", next.hash)
		default:
			// Block is not canonical, check whether we have an uncle or a lost block
			// ブロックは正規ではありません、叔父または失われたブロックがあるかどうかを確認してください
			included := false
			for number := next.index; !included && number < next.index+uint64(set.depth) && number <= height; number++ {
				if block := set.chain.GetBlockByNumber(number); block != nil {
					for _, uncle := range block.Uncles() {
						if uncle.Hash() == next.hash {
							included = true
							break
						}
					}
				}
			}
			if included {
				log.Info("⑂ block became an uncle", "number", next.index, "hash", next.hash)
			} else {
				log.Info("😱 block lost", "number", next.index, "hash", next.hash)
			}
		}
		// Drop the block out of the ring
		// ブロックをリングから外します
		if set.blocks.Value == set.blocks.Next().Value {
			set.blocks = nil
		} else {
			set.blocks = set.blocks.Move(-1)
			set.blocks.Unlink(1)
			set.blocks = set.blocks.Move(1)
		}
	}
}
