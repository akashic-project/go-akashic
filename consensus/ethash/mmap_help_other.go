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

//go:build !linux
// +build !linux

package ethash

import (
	"os"
)

// ensureSize expands the file to the given size. This is to prevent runtime
// errors later on, if the underlying file expands beyond the disk capacity,
// even though it ostensibly is already expanded, but due to being sparse
// does not actually occupy the full declared size on disk.

// sureSizeはファイルを指定されたサイズに拡張します。
// これは、基になるファイルがディスク容量を超えて拡張された場合に、実行時エラーを防ぐためです。
// 表面上はすでに拡張されていますが、スパースであるため、実際にはディスク上で宣言されたサイズ全体を占有しません。
func ensureSize(f *os.File, size int64) error {
	// On systems which do not support fallocate, we merely truncate it.
	// More robust alternatives  would be to
	// - Use posix_fallocate, or
	// - explicitly fill the file with zeroes.
	// fallocateをサポートしていないシステムでは、単に切り捨てます。
	// より堅牢な代替手段は
	//-posix_fallocate、またはを使用します
	//-ファイルを明示的にゼロで埋めます。
	return f.Truncate(size)
}
