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

package types

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// StateAccount is the Ethereum consensus representation of accounts.
// These objects are stored in the main account trie.
// StateAccountは、アカウントのイーサリアムコンセンサス表現です。
// これらのオブジェクトはメインアカウントトライに保存されます。
type StateAccount struct {
	Nonce           uint64
	Balance         *big.Int
	LastBlockNumber *big.Int    //コインエイジ計算用　アカウントの最終更新ブロックナンバー
	Root            common.Hash //ストレージトライのマークルルート // merkle root of the storage trie
	CodeHash        []byte
}
