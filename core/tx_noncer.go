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
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
)

// txNoncer is a tiny virtual state database to manage the executable nonces of
// accounts in the pool, falling back to reading from a real state database if
// an account is unknown.
// txNoncerは、プール内のアカウントの実行可能なナンスを管理するための小さな仮想状態データベースであり、
// アカウントが不明な場合は実際の状態データベースからの読み取りにフォールバックします。
type txNoncer struct {
	fallback *state.StateDB
	nonces   map[common.Address]uint64
	lock     sync.Mutex
}

// newTxNoncer creates a new virtual state database to track the pool nonces.
// newTxNoncerは、プールナンスを追跡するための新しい仮想状態データベースを作成します。
func newTxNoncer(statedb *state.StateDB) *txNoncer {
	return &txNoncer{
		fallback: statedb.Copy(),
		nonces:   make(map[common.Address]uint64),
	}
}

// get returns the current nonce of an account, falling back to a real state
// database if the account is unknown.
// getはアカウントの現在のナンスを返し、アカウントが不明な場合は実際の状態のデータベースにフォールバックします。
func (txn *txNoncer) get(addr common.Address) uint64 {
	// We use mutex for get operation is the underlying
	// state will mutate db even for read access.
	// get操作にミューテックスを使用します。基礎となる状態は、読み取りアクセスの場合でもdbをミューテーションします。
	txn.lock.Lock()
	defer txn.lock.Unlock()

	if _, ok := txn.nonces[addr]; !ok {
		txn.nonces[addr] = txn.fallback.GetNonce(addr)
	}
	return txn.nonces[addr]
}

// set inserts a new virtual nonce into the virtual state database to be returned
// whenever the pool requests it instead of reaching into the real state database.
// setは、実際の状態データベースに到達する代わりに、
// プールが要求するたびに返される仮想状態データベースに新しい仮想ナンスを挿入します。
func (txn *txNoncer) set(addr common.Address, nonce uint64) {
	txn.lock.Lock()
	defer txn.lock.Unlock()

	txn.nonces[addr] = nonce
}

// setIfLower updates a new virtual nonce into the virtual state database if the
// the new one is lower.
// setIfLowerは、新しい仮想ナンスが低い場合、新しい仮想ナンスを仮想状態データベースに更新します。
func (txn *txNoncer) setIfLower(addr common.Address, nonce uint64) {
	txn.lock.Lock()
	defer txn.lock.Unlock()

	if _, ok := txn.nonces[addr]; !ok {
		txn.nonces[addr] = txn.fallback.GetNonce(addr)
	}
	if txn.nonces[addr] <= nonce {
		return
	}
	txn.nonces[addr] = nonce
}

// setAll sets the nonces for all accounts to the given map.
// setAllは、すべてのアカウントのナンスを指定されたマップに設定します。
func (txn *txNoncer) setAll(all map[common.Address]uint64) {
	txn.lock.Lock()
	defer txn.lock.Unlock()

	txn.nonces = all
}
