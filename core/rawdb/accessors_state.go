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

package rawdb

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

// ReadPreimage retrieves a single preimage of the provided hash.
// ReadPreimageは、提供されたハッシュの単一のプリイメージを取得します。
func ReadPreimage(db ethdb.KeyValueReader, hash common.Hash) []byte {
	data, _ := db.Get(preimageKey(hash))
	return data
}

// WritePreimages writes the provided set of preimages to the database.
// WritePreimagesは、提供された一連のプレイメージをデータベースに書き込みます。
func WritePreimages(db ethdb.KeyValueWriter, preimages map[common.Hash][]byte) {
	for hash, preimage := range preimages {
		if err := db.Put(preimageKey(hash), preimage); err != nil {
			log.Crit("Failed to store trie preimage", "err", err)
		}
	}
	preimageCounter.Inc(int64(len(preimages)))
	preimageHitCounter.Inc(int64(len(preimages)))
}

// ReadCode retrieves the contract code of the provided code hash.
// ReadCodeは、提供されたコードハッシュのコントラクトコードを取得します。
func ReadCode(db ethdb.KeyValueReader, hash common.Hash) []byte {
	// Try with the legacy code scheme first, if not then try with current
	// scheme. Since most of the code will be found with legacy scheme.
	//
	// todo(rjl493456442) change the order when we forcibly upgrade the code
	// scheme with snapshot.
	// 最初にレガシーコードスキームで試してください。そうでない場合は、現在のスキームで試してください。ほとんどのコードはレガシースキームで見つかるためです。
	//
	// todo（rjl493456442）スナップショットを使用してコードスキームを強制的にアップグレードする場合、順序を変更します。
	data, _ := db.Get(hash[:])
	if len(data) != 0 {
		return data
	}
	return ReadCodeWithPrefix(db, hash)
}

// ReadCodeWithPrefix retrieves the contract code of the provided code hash.
// The main difference between this function and ReadCode is this function
// will only check the existence with latest scheme(with prefix).
// ReadCodeWithPrefixは、提供されたコードハッシュのコントラクトコードを取得します。
// この関数とReadCodeの主な違いは、この関数は最新のスキーム（プレフィックス付き）でのみ存在をチェックすることです。
func ReadCodeWithPrefix(db ethdb.KeyValueReader, hash common.Hash) []byte {
	data, _ := db.Get(codeKey(hash))
	return data
}

// WriteCode writes the provided contract code database.
// WriteCodeは、提供されたコントラクトコードデータベースを書き込みます。
func WriteCode(db ethdb.KeyValueWriter, hash common.Hash, code []byte) {
	if err := db.Put(codeKey(hash), code); err != nil {
		log.Crit("Failed to store contract code", "err", err)
	}
}

// DeleteCode deletes the specified contract code from the database.
// DeleteCodeは、指定されたコントラクトコードをデータベースから削除します。
func DeleteCode(db ethdb.KeyValueWriter, hash common.Hash) {
	if err := db.Delete(codeKey(hash)); err != nil {
		log.Crit("Failed to delete contract code", "err", err)
	}
}

// ReadTrieNode retrieves the trie node of the provided hash.
// ReadTrieNodeは、提供されたハッシュのトライノードを取得します。
func ReadTrieNode(db ethdb.KeyValueReader, hash common.Hash) []byte {
	data, _ := db.Get(hash.Bytes())
	return data
}

// WriteTrieNode writes the provided trie node database.
// WriteTrieNodeは、提供されたトライノードデータベースを書き込みます。
func WriteTrieNode(db ethdb.KeyValueWriter, hash common.Hash, node []byte) {
	if err := db.Put(hash.Bytes(), node); err != nil {
		log.Crit("Failed to store trie node", "err", err)
	}
}

// DeleteTrieNode deletes the specified trie node from the database.
// DeleteTrieNodeは、指定されたトライノードをデータベースから削除します。
func DeleteTrieNode(db ethdb.KeyValueWriter, hash common.Hash) {
	if err := db.Delete(hash.Bytes()); err != nil {
		log.Crit("Failed to delete trie node", "err", err)
	}
}
