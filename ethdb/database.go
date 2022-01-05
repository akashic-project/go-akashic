// Copyright 2014 The go-ethereum Authors
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

// Package ethdb defines the interfaces for an Ethereum data store.
// パッケージethdbは、Ethereumデータストアのインターフェイスを定義します。
package ethdb

import "io"

// KeyValueReader wraps the Has and Get method of a backing data store.
// KeyValueReaderは、バッキングデータストアのHasメソッドとGetメソッドをラップします。
type KeyValueReader interface {
	// Has retrieves if a key is present in the key-value data store.
	// キーがKey-Valueデータストアに存在するかどうかを取得します。
	Has(key []byte) (bool, error)

	// Get retrieves the given key if it's present in the key-value data store.
	// Getは、指定されたキーがKey-Valueデータストアに存在する場合にそれを取得します。
	Get(key []byte) ([]byte, error)
}

// KeyValueWriter wraps the Put method of a backing data store.
// KeyValueWriterは、バッキングデータストアのPutメソッドをラップします。
type KeyValueWriter interface {
	// Put inserts the given value into the key-value data store.
	// Putは、指定された値をKey-Valueデータストアに挿入します。
	Put(key []byte, value []byte) error

	// Delete removes the key from the key-value data store.
	// Deleteは、Key-Valueデータストアからキーを削除します。
	Delete(key []byte) error
}

// Stater wraps the Stat method of a backing data store.
// Staterは、バッキングデータストアのStatメソッドをラップします。
type Stater interface {
	// Stat returns a particular internal stat of the database.
	// Statは、データベースの特定の内部統計を返します。
	Stat(property string) (string, error)
}

// Compacter wraps the Compact method of a backing data store.
// Compacterは、バッキングデータストアのCompactメソッドをラップします。
type Compacter interface {
	// Compact flattens the underlying data store for the given key range. In essence,
	// deleted and overwritten versions are discarded, and the data is rearranged to
	// reduce the cost of operations needed to access them.
	//
	// A nil start is treated as a key before all keys in the data store; a nil limit
	// is treated as a key after all keys in the data store. If both is nil then it
	// will compact entire data store.
	// Compactは、指定されたキー範囲の基になるデータストアをフラット化します。
	// 基本的に、削除および上書きされたバージョンは破棄され、
	// データはそれらにアクセスするために必要な操作のコストを削減するために再配置されます。
	//
	// nil startは、データストア内のすべてのキーの前にキーとして扱われます。
	// nil制限は、データストア内のすべてのキーの後にキーとして扱われます。両方がnilの場合、データストア全体が圧縮されます。
	Compact(start []byte, limit []byte) error
}

// KeyValueStore contains all the methods required to allow handling different
// key-value data stores backing the high level database.
// KeyValueStoreには、高レベルのデータベースをサポートするさまざまな
// Key-Valueデータストアを処理できるようにするために必要なすべてのメソッドが含まれています。
type KeyValueStore interface {
	KeyValueReader
	KeyValueWriter
	Batcher
	Iteratee
	Stater
	Compacter
	io.Closer
}

// AncientReader contains the methods required to read from immutable ancient data.
// AncientReaderには、不変の古代データから読み取るために必要なメソッドが含まれています。
type AncientReader interface {
	// HasAncient returns an indicator whether the specified data exists in the
	// ancient store.
	// HasAncientは、指定されたデータが古代のストアに存在するかどうかのインジケーターを返します。
	HasAncient(kind string, number uint64) (bool, error)

	// Ancient retrieves an ancient binary blob from the append-only immutable files.
	// Ancientは、appendのみの不変ファイルからAncientバイナリブロブを取得します。
	Ancient(kind string, number uint64) ([]byte, error)

	// AncientRange retrieves multiple items in sequence, starting from the index 'start'.
	// It will return
	//  - at most 'count' items,
	//  - at least 1 item (even if exceeding the maxBytes), but will otherwise
	//   return as many items as fit into maxBytes.
	// AncientRangeは、インデックス 'start'から始めて、複数のアイテムを順番に取得します。
	//戻ります
	//-最大で「カウント」アイテム、
	//-少なくとも1つのアイテム（maxBytesを超えている場合でも）、それ以外の場合
	// maxBytesに収まる数のアイテムを返します。
	AncientRange(kind string, start, count, maxBytes uint64) ([][]byte, error)

	// Ancients returns the ancient item numbers in the ancient store.
	// Ancientsは、AncientストアのAncientアイテム番号を返します。
	Ancients() (uint64, error)

	// AncientSize returns the ancient size of the specified category.
	// AncientSizeは、指定されたカテゴリの古代のサイズを返します。
	AncientSize(kind string) (uint64, error)
}

// AncientBatchReader is the interface for 'batched' or 'atomic' reading.
// AncientBatchReaderは、「バッチ処理」または「アトミック」読み取り用のインターフェースです。
type AncientBatchReader interface {
	AncientReader

	// ReadAncients runs the given read operation while ensuring that no writes take place
	// on the underlying freezer.
	// ReadAncientsは、基になるフリーザーで書き込みが行われないようにしながら、指定された読み取り操作を実行します。
	ReadAncients(fn func(AncientReader) error) (err error)
}

// AncientWriter contains the methods required to write to immutable ancient data.
// AncientWriterには、不変の古代データに書き込むために必要なメソッドが含まれています。
type AncientWriter interface {
	// ModifyAncients runs a write operation on the ancient store.
	// If the function returns an error, any changes to the underlying store are reverted.
	// The integer return value is the total size of the written data.
	// ModifyAncientsは古代のストアで書き込み操作を実行します。
	// 関数がエラーを返した場合、基になるストアへの変更はすべて元に戻されます。
	// 整数の戻り値は、書き込まれたデータの合計サイズです。
	ModifyAncients(func(AncientWriteOp) error) (int64, error)

	// TruncateAncients discards all but the first n ancient data from the ancient store.
	// TruncateAncientsは、古代ストアから最初のn個の古代データを除くすべてを破棄します。
	TruncateAncients(n uint64) error

	// Sync flushes all in-memory ancient store data to disk.
	// Syncは、メモリ内のすべての古いストアデータをディスクにフラッシュします。
	Sync() error
}

// AncientWriteOp is given to the function argument of ModifyAncients.
// AncientWriteOpはModifyAncientsの関数引数に与えられます
type AncientWriteOp interface {
	// Append adds an RLP-encoded item.
	// Appendは、RLPでエンコードされたアイテムを追加します。
	Append(kind string, number uint64, item interface{}) error

	// AppendRaw adds an item without RLP-encoding it.
	// AppendRawは、RLPエンコードせずにアイテムを追加します。
	AppendRaw(kind string, number uint64, item []byte) error
}

// Reader contains the methods required to read data from both key-value as well as
// immutable ancient data.
// Readerには、Key-Valueと不変の古代データの両方からデータを読み取るために必要なメソッドが含まれています。
type Reader interface {
	KeyValueReader
	AncientBatchReader
}

// Writer contains the methods required to write data to both key-value as well as
// immutable ancient data.
// Writerには、Key-Valueと不変の古代データの両方にデータを書き込むために必要なメソッドが含まれています。
type Writer interface {
	KeyValueWriter
	AncientWriter
}

// AncientStore contains all the methods required to allow handling different
// ancient data stores backing immutable chain data store.
// AncientStoreには、不変のチェーンデータストアを
// サポートするさまざまな古代データストアを処理できるようにするために必要なすべてのメソッドが含まれています。
type AncientStore interface {
	AncientBatchReader
	AncientWriter
	io.Closer
}

// Database contains all the methods required by the high level database to not
// only access the key-value data store but also the chain freezer.
// データベースには、Key-Valueデータストアだけでなくチェーンフリーザーにも
// アクセスするために高レベルデータベースに必要なすべてのメソッドが含まれています。
type Database interface {
	Reader
	Writer
	Batcher
	Iteratee
	Stater
	Compacter
	io.Closer
}
