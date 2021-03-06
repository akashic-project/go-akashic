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

package snapshot

import (
	"bytes"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

// Account is a modified version of a state.Account, where the root is replaced
// with a byte slice. This format can be used to represent full-consensus format
// or slim-snapshot format which replaces the empty root and code hash as nil
// byte slice.
// アカウントはstate.Accountの修正バージョンであり、ルートはバイトスライスに置き換えられます。
// この形式は、フルコンセンサス形式またはスリムスナップショット形式を表すために使用できます。
// これは、空のルートとコードハッシュをnilバイトスライスとして置き換えます。
type Account struct {
	Nonce           uint64
	Balance         *big.Int
	LastBlockNumber *big.Int
	Root            []byte
	CodeHash        []byte
}

// SlimAccount converts a state.Account content into a slim snapshot account
// SlimAccountはstate.Accountコンテンツをスリムスナップショットアカウントに変換します
func SlimAccount(nonce uint64, balance *big.Int, LastBlockNumber *big.Int, root common.Hash, codehash []byte) Account {
	slim := Account{
		Nonce:           nonce,
		Balance:         balance,
		LastBlockNumber: LastBlockNumber,
	}
	if root != emptyRoot {
		slim.Root = root[:]
	}
	if !bytes.Equal(codehash, emptyCode[:]) {
		slim.CodeHash = codehash
	}
	return slim
}

// SlimAccountRLP converts a state.Account content into a slim snapshot
// version RLP encoded.
// SlimAccountRLPは、state.AccountコンテンツをRLPでエンコードされたスリムスナップショットバージョンに変換します。
func SlimAccountRLP(nonce uint64, balance *big.Int, LastBlockNumber *big.Int, root common.Hash, codehash []byte) []byte {
	data, err := rlp.EncodeToBytes(SlimAccount(nonce, balance, LastBlockNumber, root, codehash))
	if err != nil {
		panic(err)
	}
	return data
}

// FullAccount decodes the data on the 'slim RLP' format and return
// the consensus format account.
// FullAccountは、「スリムRLP」形式でデータをデコードし、コンセンサス形式のアカウントを返します。
func FullAccount(data []byte) (Account, error) {
	var account Account
	if err := rlp.DecodeBytes(data, &account); err != nil {
		return Account{}, err
	}
	if len(account.Root) == 0 {
		account.Root = emptyRoot[:]
	}
	if len(account.CodeHash) == 0 {
		account.CodeHash = emptyCode[:]
	}
	return account, nil
}

// FullAccountRLP converts data on the 'slim RLP' format into the full RLP-format.
// FullAccountRLPは、「スリムRLP」形式のデータを完全なRLP形式に変換します。
func FullAccountRLP(data []byte) ([]byte, error) {
	account, err := FullAccount(data)
	if err != nil {
		return nil, err
	}
	return rlp.EncodeToBytes(account)
}
