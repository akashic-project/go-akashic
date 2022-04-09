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

	"github.com/ethereum/go-ethereum/common"
)

// hashes is a helper to implement sort.Interface.
// ハッシュはsort.Interfaceを実装するためのヘルパーです。
type hashes []common.Hash

// Len is the number of elements in the collection.
// Lenは、コレクション内の要素の数です。
func (hs hashes) Len() int { return len(hs) }

// Less reports whether the element with index i should sort before the element
// with index j.
//インデックスiの要素がインデックスjの要素の前にソートする必要があるかどうかのレポートが少なくなります。
func (hs hashes) Less(i, j int) bool { return bytes.Compare(hs[i][:], hs[j][:]) < 0 }

// Swap swaps the elements with indexes i and j.
// スワップは、要素をインデックスiおよびjとスワップします。
func (hs hashes) Swap(i, j int) { hs[i], hs[j] = hs[j], hs[i] }
