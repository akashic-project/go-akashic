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

package state

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// journalEntry is a modification entry in the state change journal that can be
// reverted on demand.
// journalEntryは、オンデマンドで元に戻すことができる状態変更ジャーナルの変更エントリです。
type journalEntry interface {
	// revert undoes the changes introduced by this journal entry.
	// 元に戻すと、このジャーナルエントリによって導入された変更が元に戻されます。
	revert(*StateDB)

	// dirtied returns the Ethereum address modified by this journal entry.
	// dirtiedは、このジャーナルエントリによって変更されたEthereumアドレスを返します。
	dirtied() *common.Address
}

// journal contains the list of state modifications applied since the last state
// commit. These are tracked to be able to be reverted in the case of an execution
// exception or request for reversal.
// ジャーナルには、最後の状態コミット以降に適用された状態変更のリストが含まれています。
// これらは、実行例外または取り消しの要求が発生した場合に元に戻すことができるように追跡されます。
type journal struct {
	entries []journalEntry         // ジャーナルによって追跡される現在の変更 // Current changes tracked by the journal
	dirties map[common.Address]int // ダーティアカウントと変更の数 // Dirty accounts and the number of changes
}

// newJournal creates a new initialized journal.
// newJournalは、新しい初期化されたジャーナルを作成します。
func newJournal() *journal {
	return &journal{
		dirties: make(map[common.Address]int),
	}
}

// append inserts a new modification entry to the end of the change journal.
// appendは、変更ジャーナルの最後に新しい変更エントリを挿入します。
func (j *journal) append(entry journalEntry) {
	j.entries = append(j.entries, entry)
	if addr := entry.dirtied(); addr != nil {
		j.dirties[*addr]++
	}
}

// revert undoes a batch of journalled modifications along with any reverted
// dirty handling too.
// 元に戻すと、ジャーナルに記録された変更のバッチが元に戻され、ダーティ処理も元に戻されます。
func (j *journal) revert(statedb *StateDB, snapshot int) {
	for i := len(j.entries) - 1; i >= snapshot; i-- {
		// Undo the changes made by the operation
		// 操作によって行われた変更を元に戻します
		j.entries[i].revert(statedb)

		// Drop any dirty tracking induced by the change
		// 変更によって引き起こされたダーティトラッキングを削除します
		if addr := j.entries[i].dirtied(); addr != nil {
			if j.dirties[*addr]--; j.dirties[*addr] == 0 {
				delete(j.dirties, *addr)
			}
		}
	}
	j.entries = j.entries[:snapshot]
}

// dirty explicitly sets an address to dirty, even if the change entries would
// otherwise suggest it as clean. This method is an ugly hack to handle the RIPEMD
// precompile consensus exception.
// ダーティは、変更エントリがクリーンであると示唆する場合でも、アドレスをダーティに明示的に設定します。
// このメソッドは、RIPEMDプリコンパイルコンセンサス例外を処理するための醜いハックです。
func (j *journal) dirty(addr common.Address) {
	j.dirties[addr]++
}

// length returns the current number of entries in the journal.
// lengthは、ジャーナルの現在のエントリ数を返します。
func (j *journal) length() int {
	return len(j.entries)
}

type (
	// Changes to the account trie.
	// アカウントトライへの変更。
	createObjectChange struct {
		account *common.Address
	}
	resetObjectChange struct {
		prev         *stateObject
		prevdestruct bool
	}
	suicideChange struct {
		account     *common.Address
		prev        bool // アカウントがすでに自殺したかどうか // whether account had already suicided
		prevbalance *big.Int
	}

	// Changes to individual accounts.
	// 個々のアカウントへの変更。
	balanceChange struct {
		account *common.Address
		prev    *big.Int
	}

	LastBlockNumberChange struct {
		account *common.Address
		prev    *big.Int
	}

	nonceChange struct {
		account *common.Address
		prev    uint64
	}
	storageChange struct {
		account       *common.Address
		key, prevalue common.Hash
	}
	codeChange struct {
		account            *common.Address
		prevcode, prevhash []byte
	}

	// Changes to other state values.
	// 他の状態値に変更します。
	refundChange struct {
		prev uint64
	}
	addLogChange struct {
		txhash common.Hash
	}
	addPreimageChange struct {
		hash common.Hash
	}
	touchChange struct {
		account *common.Address
	}
	// Changes to the access list
	// アクセスリストへの変更
	accessListAddAccountChange struct {
		address *common.Address
	}
	accessListAddSlotChange struct {
		address *common.Address
		slot    *common.Hash
	}
)

func (ch createObjectChange) revert(s *StateDB) {
	delete(s.stateObjects, *ch.account)
	delete(s.stateObjectsDirty, *ch.account)
}

func (ch createObjectChange) dirtied() *common.Address {
	return ch.account
}

func (ch resetObjectChange) revert(s *StateDB) {
	s.setStateObject(ch.prev)
	if !ch.prevdestruct && s.snap != nil {
		delete(s.snapDestructs, ch.prev.addrHash)
	}
}

func (ch resetObjectChange) dirtied() *common.Address {
	return nil
}

func (ch suicideChange) revert(s *StateDB) {
	obj := s.getStateObject(*ch.account)
	if obj != nil {
		obj.suicided = ch.prev
		obj.setBalance(ch.prevbalance)
	}
}

func (ch suicideChange) dirtied() *common.Address {
	return ch.account
}

var ripemd = common.HexToAddress("0000000000000000000000000000000000000003")

func (ch touchChange) revert(s *StateDB) {
}

func (ch touchChange) dirtied() *common.Address {
	return ch.account
}

func (ch balanceChange) revert(s *StateDB) {
	s.getStateObject(*ch.account).setBalance(ch.prev)
}

func (ch balanceChange) dirtied() *common.Address {
	return ch.account
}

func (ch LastBlockNumberChange) revert(s *StateDB) {
	s.getStateObject(*ch.account).setLastBlockNumber(ch.prev)
}

func (ch LastBlockNumberChange) dirtied() *common.Address {
	return ch.account
}

func (ch nonceChange) revert(s *StateDB) {
	s.getStateObject(*ch.account).setNonce(ch.prev)
}

func (ch nonceChange) dirtied() *common.Address {
	return ch.account
}

func (ch codeChange) revert(s *StateDB) {
	s.getStateObject(*ch.account).setCode(common.BytesToHash(ch.prevhash), ch.prevcode)
}

func (ch codeChange) dirtied() *common.Address {
	return ch.account
}

func (ch storageChange) revert(s *StateDB) {
	s.getStateObject(*ch.account).setState(ch.key, ch.prevalue)
}

func (ch storageChange) dirtied() *common.Address {
	return ch.account
}

func (ch refundChange) revert(s *StateDB) {
	s.refund = ch.prev
}

func (ch refundChange) dirtied() *common.Address {
	return nil
}

func (ch addLogChange) revert(s *StateDB) {
	logs := s.logs[ch.txhash]
	if len(logs) == 1 {
		delete(s.logs, ch.txhash)
	} else {
		s.logs[ch.txhash] = logs[:len(logs)-1]
	}
	s.logSize--
}

func (ch addLogChange) dirtied() *common.Address {
	return nil
}

func (ch addPreimageChange) revert(s *StateDB) {
	delete(s.preimages, ch.hash)
}

func (ch addPreimageChange) dirtied() *common.Address {
	return nil
}

func (ch accessListAddAccountChange) revert(s *StateDB) {
	/*
		One important invariant here, is that whenever a (addr, slot) is added, if the
		addr is not already present, the add causes two journal entries:
		- one for the address,
		- one for the (address,slot)
		Therefore, when unrolling the change, we can always blindly delete the
		(addr) at this point, since no storage adds can remain when come upon
		a single (addr) change.
	*/
	/*
			ここで重要な不変条件の1つは、（addr、slot）が追加されるたびに、
			addrがまだ存在しない場合、追加によって2つのジャーナルエントリが発生することです。
			 -アドレス用に1つ、
			 --（address、slot）用に1つ。したがって、変更を展開するときは、
		    この時点でいつでも盲目的に（addr）を削除できます。
			これは、単一の（addr）変更が発生したときにストレージの追加が残っていないためです。
	*/
	s.accessList.DeleteAddress(*ch.address)
}

func (ch accessListAddAccountChange) dirtied() *common.Address {
	return nil
}

func (ch accessListAddSlotChange) revert(s *StateDB) {
	s.accessList.DeleteSlot(*ch.address, *ch.slot)
}

func (ch accessListAddSlotChange) dirtied() *common.Address {
	return nil
}
