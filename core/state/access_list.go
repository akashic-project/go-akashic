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

package state

import (
	"github.com/ethereum/go-ethereum/common"
)

type accessList struct {
	addresses map[common.Address]int
	slots     []map[common.Hash]struct{}
}

// ContainsAddress returns true if the address is in the access list.
// アドレスがアクセスリストにある場合、ContainsAddressはtrueを返します。
func (al *accessList) ContainsAddress(address common.Address) bool {
	_, ok := al.addresses[address]
	return ok
}

// Contains checks if a slot within an account is present in the access list, returning
// separate flags for the presence of the account and the slot respectively.
//アカウント内のスロットがアクセスリストに存在するかどうかのチェックが含まれ、アカウントとスロットの存在に対してそれぞれ個別のフラグを返します。
func (al *accessList) Contains(address common.Address, slot common.Hash) (addressPresent bool, slotPresent bool) {
	idx, ok := al.addresses[address]
	if !ok {
		// no such address (and hence zero slots)
		// そのようなアドレスはありません（したがって、スロットはゼロです）
		return false, false
	}
	if idx == -1 {
		// address yes, but no slots
		// アドレスはい、ただしスロットはありません
		return true, false
	}
	_, slotPresent = al.slots[idx][slot]
	return true, slotPresent
}

// newAccessList creates a new accessList.
// newAccessListは新しいaccessListを作成します。
func newAccessList() *accessList {
	return &accessList{
		addresses: make(map[common.Address]int),
	}
}

// Copy creates an independent copy of an accessList.
// コピーはaccessListの独立したコピーを作成します。
func (a *accessList) Copy() *accessList {
	cp := newAccessList()
	for k, v := range a.addresses {
		cp.addresses[k] = v
	}
	cp.slots = make([]map[common.Hash]struct{}, len(a.slots))
	for i, slotMap := range a.slots {
		newSlotmap := make(map[common.Hash]struct{}, len(slotMap))
		for k := range slotMap {
			newSlotmap[k] = struct{}{}
		}
		cp.slots[i] = newSlotmap
	}
	return cp
}

// AddAddress adds an address to the access list, and returns 'true' if the operation
// caused a change (addr was not previously in the list).
// AddAddressはアクセスリストにアドレスを追加し、操作によって変更が発生した場合は「true」を返します
// （addrは以前はリストにありませんでした）。
func (al *accessList) AddAddress(address common.Address) bool {
	if _, present := al.addresses[address]; present {
		return false
	}
	al.addresses[address] = -1
	return true
}

// AddSlot adds the specified (addr, slot) combo to the access list.
// Return values are:
// - address added
// - slot added
// For any 'true' value returned, a corresponding journal entry must be made.
// AddSlotは、指定された（addr、slot）コンボをアクセスリストに追加します。
// 戻り値は次のとおりです。
//  -アドレスが追加されました
//  -スロットが追加されました
//  返される「true」の値については、対応するジャーナルエントリを作成する必要があります。
func (al *accessList) AddSlot(address common.Address, slot common.Hash) (addrChange bool, slotChange bool) {
	idx, addrPresent := al.addresses[address]
	if !addrPresent || idx == -1 {
		// Address not present, or addr present but no slots there
		// アドレスが存在しないか、アドレスは存在するがスロットがない
		al.addresses[address] = len(al.slots)
		slotmap := map[common.Hash]struct{}{slot: {}}
		al.slots = append(al.slots, slotmap)
		return !addrPresent, true
	}
	// There is already an (address,slot) mapping
	//（アドレス、スロット）マッピングがすでにあります
	slotmap := al.slots[idx]
	if _, ok := slotmap[slot]; !ok {
		slotmap[slot] = struct{}{}
		// Journal add slot change
		// ジャーナル追加スロット変更
		return false, true
	}
	// No changes required
	// 変更は必要ありません
	return false, false
}

// DeleteSlot removes an (address, slot)-tuple from the access list.
// This operation needs to be performed in the same order as the addition happened.
// This method is meant to be used  by the journal, which maintains ordering of
// operations.
// DeleteSlotは、アクセスリストから（アドレス、スロット）タプルを削除します。
// この操作は、加算が行われたのと同じ順序で実行する必要があります。
// このメソッドは、操作の順序を維持するジャーナルによって使用されることを意図しています。
func (al *accessList) DeleteSlot(address common.Address, slot common.Hash) {
	idx, addrOk := al.addresses[address]
	// There are two ways this can fail
	// これが失敗する可能性がある2つの方法があります
	if !addrOk {
		panic("reverting slot change, address not present in list") // スロットの変更を元に戻し、アドレスがリストに存在しません
	}
	slotmap := al.slots[idx]
	delete(slotmap, slot)
	// If that was the last (first) slot, remove it
	// Since additions and rollbacks are always performed in order,
	// we can delete the item without worrying about screwing up later indices
	// それが最後の（最初の）スロットだった場合は、
	// それを削除します追加とロールバックは常に順番に実行されるため、
	// 後のインデックスを台無しにすることを心配せずにアイテムを削除できます
	if len(slotmap) == 0 {
		al.slots = al.slots[:idx]
		al.addresses[address] = -1
	}
}

// DeleteAddress removes an address from the access list. This operation
// needs to be performed in the same order as the addition happened.
// This method is meant to be used  by the journal, which maintains ordering of
// operations.
// DeleteAddressは、アクセスリストからアドレスを削除します。
// この操作は、加算が行われたのと同じ順序で実行する必要があります。
// このメソッドは、操作の順序を維持するジャーナルによって使用されることを意図しています。
func (al *accessList) DeleteAddress(address common.Address) {
	delete(al.addresses, address)
}
