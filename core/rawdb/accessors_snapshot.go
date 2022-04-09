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

package rawdb

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
)

// ReadSnapshotDisabled retrieves if the snapshot maintenance is disabled.
// ReadSnapshotDisabledは、スナップショットのメンテナンスが無効になっているかどうかを取得します。
func ReadSnapshotDisabled(db ethdb.KeyValueReader) bool {
	disabled, _ := db.Has(snapshotDisabledKey)
	return disabled
}

// WriteSnapshotDisabled stores the snapshot pause flag.
// WriteSnapshotDisabledは、スナップショット一時停止フラグを格納します。
func WriteSnapshotDisabled(db ethdb.KeyValueWriter) {
	if err := db.Put(snapshotDisabledKey, []byte("42")); err != nil {
		log.Crit("Failed to store snapshot disabled flag", "err", err)
	}
}

// DeleteSnapshotDisabled deletes the flag keeping the snapshot maintenance disabled.
// DeleteSnapshotDisabledは、スナップショットのメンテナンスを無効にしたままフラグを削除します。
func DeleteSnapshotDisabled(db ethdb.KeyValueWriter) {
	if err := db.Delete(snapshotDisabledKey); err != nil {
		log.Crit("Failed to remove snapshot disabled flag", "err", err)
	}
}

// ReadSnapshotRoot retrieves the root of the block whose state is contained in
// the persisted snapshot.
// ReadSnapshotRootは、永続化されたスナップショットに状態が含まれているブロックのルートを取得します。
func ReadSnapshotRoot(db ethdb.KeyValueReader) common.Hash {
	data, _ := db.Get(SnapshotRootKey)
	if len(data) != common.HashLength {
		return common.Hash{}
	}
	return common.BytesToHash(data)
}

// WriteSnapshotRoot stores the root of the block whose state is contained in
// the persisted snapshot.
// WriteSnapshotRootは、永続化されたスナップショットに状態が含まれているブロックのルートを格納します。
func WriteSnapshotRoot(db ethdb.KeyValueWriter, root common.Hash) {
	if err := db.Put(SnapshotRootKey, root[:]); err != nil {
		log.Crit("Failed to store snapshot root", "err", err)
	}
}

// DeleteSnapshotRoot deletes the hash of the block whose state is contained in
// the persisted snapshot. Since snapshots are not immutable, this  method can
// be used during updates, so a crash or failure will mark the entire snapshot
// invalid.
// DeleteSnapshotRootは、永続化されたスナップショットに状態が含まれているブロックのハッシュを削除します。
// スナップショットは不変ではないため、この方法は更新中に使用できます。
// そのため、クラッシュまたは障害が発生すると、スナップショット全体が無効になります。
func DeleteSnapshotRoot(db ethdb.KeyValueWriter) {
	if err := db.Delete(SnapshotRootKey); err != nil {
		log.Crit("Failed to remove snapshot root", "err", err)
	}
}

// ReadAccountSnapshot retrieves the snapshot entry of an account trie leaf.
// ReadAccountSnapshotは、アカウントトライリーフのスナップショットエントリを取得します。
func ReadAccountSnapshot(db ethdb.KeyValueReader, hash common.Hash) []byte {
	data, _ := db.Get(accountSnapshotKey(hash))
	return data
}

// WriteAccountSnapshot stores the snapshot entry of an account trie leaf.
// WriteAccountSnapshotは、アカウントトライリーフのスナップショットエントリを保存します。
func WriteAccountSnapshot(db ethdb.KeyValueWriter, hash common.Hash, entry []byte) {
	if err := db.Put(accountSnapshotKey(hash), entry); err != nil {
		log.Crit("Failed to store account snapshot", "err", err)
	}
}

// DeleteAccountSnapshot removes the snapshot entry of an account trie leaf.
// DeleteAccountSnapshotは、アカウントトライリーフのスナップショットエントリを削除します。
func DeleteAccountSnapshot(db ethdb.KeyValueWriter, hash common.Hash) {
	if err := db.Delete(accountSnapshotKey(hash)); err != nil {
		log.Crit("Failed to delete account snapshot", "err", err)
	}
}

// ReadStorageSnapshot retrieves the snapshot entry of an storage trie leaf.
// ReadStorageSnapshotは、ストレージトライリーフのスナップショットエントリを取得します。
func ReadStorageSnapshot(db ethdb.KeyValueReader, accountHash, storageHash common.Hash) []byte {
	data, _ := db.Get(storageSnapshotKey(accountHash, storageHash))
	return data
}

// WriteStorageSnapshot stores the snapshot entry of an storage trie leaf.
// WriteStorageSnapshotは、ストレージトライリーフのスナップショットエントリを格納します。
func WriteStorageSnapshot(db ethdb.KeyValueWriter, accountHash, storageHash common.Hash, entry []byte) {
	if err := db.Put(storageSnapshotKey(accountHash, storageHash), entry); err != nil {
		log.Crit("Failed to store storage snapshot", "err", err)
	}
}

// DeleteStorageSnapshot removes the snapshot entry of an storage trie leaf.
// DeleteStorageSnapshotは、ストレージトライリーフのスナップショットエントリを削除します。
func DeleteStorageSnapshot(db ethdb.KeyValueWriter, accountHash, storageHash common.Hash) {
	if err := db.Delete(storageSnapshotKey(accountHash, storageHash)); err != nil {
		log.Crit("Failed to delete storage snapshot", "err", err)
	}
}

// IterateStorageSnapshots returns an iterator for walking the entire storage
// space of a specific account.
// IterateStorageSnapshotsは、特定のアカウントのストレージスペース全体をウォークするためのイテレータを返します。
func IterateStorageSnapshots(db ethdb.Iteratee, accountHash common.Hash) ethdb.Iterator {
	return db.NewIterator(storageSnapshotsKey(accountHash), nil)
}

// ReadSnapshotJournal retrieves the serialized in-memory diff layers saved at
// the last shutdown. The blob is expected to be max a few 10s of megabytes.
// ReadSnapshotJournalは、最後のシャットダウン時に保存されたシリアル化されたメモリ内の差分レイヤーを取得します。
// ブロブは最大で数十メガバイトになると予想されます。
func ReadSnapshotJournal(db ethdb.KeyValueReader) []byte {
	data, _ := db.Get(snapshotJournalKey)
	return data
}

// WriteSnapshotJournal stores the serialized in-memory diff layers to save at
// shutdown. The blob is expected to be max a few 10s of megabytes.
// WriteSnapshotJournalは、シャットダウン時に保存するために、シリアル化されたメモリ内のdiffレイヤーを格納します。
// ブロブは最大で数十メガバイトになると予想されます。
func WriteSnapshotJournal(db ethdb.KeyValueWriter, journal []byte) {
	if err := db.Put(snapshotJournalKey, journal); err != nil {
		log.Crit("Failed to store snapshot journal", "err", err)
	}
}

// DeleteSnapshotJournal deletes the serialized in-memory diff layers saved at
// the last shutdown
// DeleteSnapshotJournalは、最後のシャットダウン時に保存されたシリアル化されたメモリ内の差分レイヤーを削除します
func DeleteSnapshotJournal(db ethdb.KeyValueWriter) {
	if err := db.Delete(snapshotJournalKey); err != nil {
		log.Crit("Failed to remove snapshot journal", "err", err)
	}
}

// ReadSnapshotGenerator retrieves the serialized snapshot generator saved at
// the last shutdown.
// ReadSnapshotGeneratorは、最後のシャットダウン時に保存されたシリアル化されたスナップショットジェネレーターを取得します。
func ReadSnapshotGenerator(db ethdb.KeyValueReader) []byte {
	data, _ := db.Get(snapshotGeneratorKey)
	return data
}

// WriteSnapshotGenerator stores the serialized snapshot generator to save at
// shutdown.
// WriteSnapshotGeneratorは、シャットダウン時に保存するシリアル化されたスナップショットジェネレーターを保存します。
func WriteSnapshotGenerator(db ethdb.KeyValueWriter, generator []byte) {
	if err := db.Put(snapshotGeneratorKey, generator); err != nil {
		log.Crit("Failed to store snapshot generator", "err", err)
	}
}

// DeleteSnapshotGenerator deletes the serialized snapshot generator saved at
// the last shutdown
// DeleteSnapshotGeneratorは、最後のシャットダウン時に保存されたシリアル化されたスナップショットジェネレータを削除します
func DeleteSnapshotGenerator(db ethdb.KeyValueWriter) {
	if err := db.Delete(snapshotGeneratorKey); err != nil {
		log.Crit("Failed to remove snapshot generator", "err", err)
	}
}

// ReadSnapshotRecoveryNumber retrieves the block number of the last persisted
// snapshot layer.
// ReadSnapshotRecoveryNumberは、最後に永続化されたスナップショットレイヤーのブロック番号を取得します。
func ReadSnapshotRecoveryNumber(db ethdb.KeyValueReader) *uint64 {
	data, _ := db.Get(snapshotRecoveryKey)
	if len(data) == 0 {
		return nil
	}
	if len(data) != 8 {
		return nil
	}
	number := binary.BigEndian.Uint64(data)
	return &number
}

// WriteSnapshotRecoveryNumber stores the block number of the last persisted
// snapshot layer.
// WriteSnapshotRecoveryNumberは、最後に永続化されたスナップショットレイヤーのブロック番号を格納します。
func WriteSnapshotRecoveryNumber(db ethdb.KeyValueWriter, number uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], number)
	if err := db.Put(snapshotRecoveryKey, buf[:]); err != nil {
		log.Crit("Failed to store snapshot recovery number", "err", err) // スナップショットリカバリ番号の保存に失敗しました
	}
}

// DeleteSnapshotRecoveryNumber deletes the block number of the last persisted
// snapshot layer.
// DeleteSnapshotRecoveryNumberは、最後に永続化されたスナップショットレイヤーのブロック番号を削除します。
func DeleteSnapshotRecoveryNumber(db ethdb.KeyValueWriter) {
	if err := db.Delete(snapshotRecoveryKey); err != nil {
		log.Crit("Failed to remove snapshot recovery number", "err", err)
	}
}

// ReadSnapshotSyncStatus retrieves the serialized sync status saved at shutdown.
// ReadSnapshotSyncStatusは、シャットダウン時に保存されたシリアル化された同期ステータスを取得します。
func ReadSnapshotSyncStatus(db ethdb.KeyValueReader) []byte {
	data, _ := db.Get(snapshotSyncStatusKey)
	return data
}

// WriteSnapshotSyncStatus stores the serialized sync status to save at shutdown.
// WriteSnapshotSyncStatusは、シャットダウン時に保存するシリアル化された同期ステータスを保存します。
func WriteSnapshotSyncStatus(db ethdb.KeyValueWriter, status []byte) {
	if err := db.Put(snapshotSyncStatusKey, status); err != nil {
		log.Crit("Failed to store snapshot sync status", "err", err)
	}
}
