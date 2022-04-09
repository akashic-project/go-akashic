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

package pruner

import (
	"encoding/binary"
	"errors"
	"os"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/log"
	bloomfilter "github.com/holiman/bloomfilter/v2"
)

// stateBloomHasher is a wrapper around a byte blob to satisfy the interface API
// requirements of the bloom library used. It's used to convert a trie hash or
// contract code hash into a 64 bit mini hash.
type stateBloomHasher []byte

func (f stateBloomHasher) Write(p []byte) (n int, err error) { panic("not implemented") }
func (f stateBloomHasher) Sum(b []byte) []byte               { panic("not implemented") }
func (f stateBloomHasher) Reset()                            { panic("not implemented") }
func (f stateBloomHasher) BlockSize() int                    { panic("not implemented") }
func (f stateBloomHasher) Size() int                         { return 8 }
func (f stateBloomHasher) Sum64() uint64                     { return binary.BigEndian.Uint64(f) }

// stateBloom is a bloom filter used during the state convesion(snapshot->state).
// The keys of all generated entries will be recorded here so that in the pruning
// stage the entries belong to the specific version can be avoided for deletion.
//
// The false-positive is allowed here. The "false-positive" entries means they
// actually don't belong to the specific version but they are not deleted in the
// pruning. The downside of the false-positive allowance is we may leave some "dangling"
// nodes in the disk. But in practice the it's very unlike the dangling node is
// state root. So in theory this pruned state shouldn't be visited anymore. Another
// potential issue is for fast sync. If we do another fast sync upon the pruned
// database, it's problematic which will stop the expansion during the syncing.
// TODO address it @rjl493456442 @holiman @karalabe.
//
// After the entire state is generated, the bloom filter should be persisted into
// the disk. It indicates the whole generation procedure is finished.
//状態全体が生成された後、ブルームフィルターをディスクに永続化する必要があります。
// 生成手順全体が終了したことを示します。
type stateBloom struct {
	bloom *bloomfilter.Filter
}

// newStateBloomWithSize creates a brand new state bloom for state generation.
// The bloom filter will be created by the passing bloom filter size. According
// to the https://hur.st/bloomfilter/?n=600000000&p=&m=2048MB&k=4, the parameters
// are picked so that the false-positive rate for mainnet is low enough.

// newStateBloomWithSizeは、状態生成用のまったく新しい状態ブルームを作成します。
// ブルームフィルターは、通過するブルームフィルターサイズによって作成されます。
//  https://hur.st/bloomfilter/?n=600000000&p3%m=2048MB&k=4
// によると、メインネットの誤検知率が十分に低くなるようにパラメーターが選択されます。
func newStateBloomWithSize(size uint64) (*stateBloom, error) {
	bloom, err := bloomfilter.New(size*1024*1024*8, 4)
	if err != nil {
		return nil, err
	}
	log.Info("Initialized state bloom", "size", common.StorageSize(float64(bloom.M()/8)))
	return &stateBloom{bloom: bloom}, nil
}

// NewStateBloomFromDisk loads the state bloom from the given file.
// In this case the assumption is held the bloom filter is complete.
// NewStateBloomFromDiskは、指定されたファイルから状態ブルームをロードします。
// この場合、仮定が保持され、ブルームフィルターが完了します。
func NewStateBloomFromDisk(filename string) (*stateBloom, error) {
	bloom, _, err := bloomfilter.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	return &stateBloom{bloom: bloom}, nil
}

// Commit flushes the bloom filter content into the disk and marks the bloom
// as complete.
// Commitは、ブルームフィルターのコンテンツをディスクにフラッシュし、ブルームを完了としてマークします。
func (bloom *stateBloom) Commit(filename, tempname string) error {
	// Write the bloom out into a temporary file
	// ブルームを一時ファイルに書き込みます
	_, err := bloom.bloom.WriteFile(tempname)
	if err != nil {
		return err
	}
	// Ensure the file is synced to disk
	// ファイルがディスクに同期されていることを確認します
	f, err := os.OpenFile(tempname, os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()

	// Move the teporary file into it's final location
	// 一時ファイルを最終的な場所に移動します
	return os.Rename(tempname, filename)
}

// Put implements the KeyValueWriter interface. But here only the key is needed.
// PutはKeyValueWriterインターフェイスを実装します。ただし、ここで必要なのはキーだけです。
func (bloom *stateBloom) Put(key []byte, value []byte) error {
	// If the key length is not 32bytes, ensure it's contract code
	// entry with new scheme.
	// キーの長さが32バイトでない場合は、新しいスキームを使用したコントラクトコードエントリであることを確認してください。
	if len(key) != common.HashLength {
		isCode, codeKey := rawdb.IsCodeKey(key)
		if !isCode {
			return errors.New("invalid entry") // 無効なエントリ
		}
		bloom.bloom.Add(stateBloomHasher(codeKey))
		return nil
	}
	bloom.bloom.Add(stateBloomHasher(key))
	return nil
}

// Delete removes the key from the key-value data store.
// Deleteは、Key-Valueデータストアからキーを削除します。
func (bloom *stateBloom) Delete(key []byte) error { panic("not supported") } // サポートされていません

// Contain is the wrapper of the underlying contains function which
// reports whether the key is contained.
// - If it says yes, the key may be contained
// - If it says no, the key is definitely not contained.
// Containは、キーが含まれているかどうかを報告する基になるcontains関数のラッパーです。
// -「はい」と表示されている場合、キーが含まれている可能性があります
// -「いいえ」と表示されている場合、キーは間違いなく含まれていません。
func (bloom *stateBloom) Contain(key []byte) (bool, error) {
	return bloom.bloom.Contains(stateBloomHasher(key)), nil
}
