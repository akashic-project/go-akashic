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

package state

import (
	"bytes"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
)

var emptyCodeHash = crypto.Keccak256(nil)

type Code []byte

func (c Code) String() string {
	return string(c) //strings.Join(Disassemble(c), " ")
}

type Storage map[common.Hash]common.Hash

func (s Storage) String() (str string) {
	for key, value := range s {
		str += fmt.Sprintf("%X : %X\n", key, value)
	}

	return
}

func (s Storage) Copy() Storage {
	cpy := make(Storage)
	for key, value := range s {
		cpy[key] = value
	}

	return cpy
}

// stateObject represents an Ethereum account which is being modified.
//
// The usage pattern is as follows:
// First you need to obtain a state object.
// Account values can be accessed and modified through the object.
// Finally, call CommitTrie to write the modified storage trie into a database.
// stateObjectは、変更中のイーサリアムアカウントを表します。
//
// 使用パターンは次のとおりです。
// まず、状態オブジェクトを取得する必要があります。
// アカウント値は、オブジェクトを介してアクセスおよび変更できます。
// 最後に、CommitTrieを呼び出して、変更されたストレージトライをデータベースに書き込みます。
type stateObject struct {
	address  common.Address
	addrHash common.Hash // アカウントのイーサリアムアドレスのハッシュ // hash of ethereum address of the account
	data     types.StateAccount
	db       *StateDB

	// DB error.
	// State objects are used by the consensus core and VM which are
	// unable to deal with database-level errors. Any error that occurs
	// during a database read is memoized here and will eventually be returned
	// by StateDB.Commit.
	// DBエラー。
	// 状態オブジェクトは、データベースレベルのエラーを処理できないコンセンサスコアとVMによって使用されます。
	// データベースの読み取り中に発生したエラーはすべてここにメモされ、最終的にStateDB.Commitによって返されます。
	dbErr error

	// Write caches.
	// キャッシュを書き込みます。
	trie Trie // ストレージトライ。最初のアクセスでnil以外になります         // storage trie, which becomes non-nil on first access
	code Code // コードがロードされるときに設定されるコントラクトバイトコード // contract bytecode, which gets set when code is loaded

	originStorage  Storage // 重複排除リライトへの元のエントリのストレージキャッシュ、トランザクションごとにリセット // Storage cache of original entries to dedup rewrites, reset for every transaction
	pendingStorage Storage // ブロック全体の最後で、ディスクにフラッシュする必要があるストレージエントリ            // Storage entries that need to be flushed to disk, at the end of an entire block
	dirtyStorage   Storage // 現在のトランザクション実行で変更されたストレージエントリ                            // Storage entries that have been modified in the current transaction execution
	fakeStorage    Storage //デバッグ目的で呼び出し元によって構築された偽のストレージ。                           // Fake storage which constructed by caller for debugging purpose.

	// Cache flags.
	// When an object is marked suicided it will be delete from the trie
	// during the "update" phase of the state transition.
	// フラグをキャッシュします。
	// オブジェクトが自殺とマークされると、状態遷移の「更新」フェーズ中にトライから削除されます。
	dirtyCode bool // コードが更新された場合はtrue // true if the code was updated
	suicided  bool
	deleted   bool
}

// empty returns whether the account is considered empty.
// emptyは、アカウントが空であると見なされるかどうかを返します。
func (s *stateObject) empty() bool {
	return s.data.Nonce == 0 && s.data.Balance.Sign() == 0 && bytes.Equal(s.data.CodeHash, emptyCodeHash)
}

// newObject creates a state object.
// newObjectは状態オブジェクトを作成します。
func newObject(db *StateDB, address common.Address, data types.StateAccount) *stateObject {
	if data.Balance == nil {
		data.Balance = new(big.Int)
	}
	if data.LastBlockNumber == nil {
		data.LastBlockNumber = new(big.Int)
	}
	if data.CodeHash == nil {
		data.CodeHash = emptyCodeHash
	}
	if data.Root == (common.Hash{}) {
		data.Root = emptyRoot
	}
	return &stateObject{
		db:             db,
		address:        address,
		addrHash:       crypto.Keccak256Hash(address[:]),
		data:           data,
		originStorage:  make(Storage),
		pendingStorage: make(Storage),
		dirtyStorage:   make(Storage),
	}
}

// EncodeRLP implements rlp.Encoder.
// EncodeRLPはrlp.Encoderを実装します。
func (s *stateObject) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, &s.data)
}

// setError remembers the first non-nil error it is called with.
// setErrorは、呼び出された最初のnil以外のエラーを記憶します。
func (s *stateObject) setError(err error) {
	if s.dbErr == nil {
		s.dbErr = err
	}
}

func (s *stateObject) markSuicided() {
	s.suicided = true
}

func (s *stateObject) touch() {
	s.db.journal.append(touchChange{
		account: &s.address,
	})
	if s.address == ripemd {
		// Explicitly put it in the dirty-cache, which is otherwise generated from
		// flattened journals.
		// それをdirty-cacheに明示的に配置します。
		// それ以外の場合は、フラット化されたジャーナルから生成されます。
		s.db.journal.dirty(s.address)
	}
}

func (s *stateObject) getTrie(db Database) Trie {
	if s.trie == nil {
		// Try fetching from prefetcher first
		// We don't prefetch empty tries
		// 最初にプリフェッチャーからフェッチしてみます空の試行をプリフェッチしません
		if s.data.Root != emptyRoot && s.db.prefetcher != nil {
			// When the miner is creating the pending state, there is no
			// prefetcher
			// マイナーが保留状態を作成しているとき、プリフェッチャーはありません
			s.trie = s.db.prefetcher.trie(s.data.Root)
		}
		if s.trie == nil {
			var err error
			s.trie, err = db.OpenStorageTrie(s.addrHash, s.data.Root)
			if err != nil {
				s.trie, _ = db.OpenStorageTrie(s.addrHash, common.Hash{})
				s.setError(fmt.Errorf("can't create storage trie: %v", err))
			}
		}
	}
	return s.trie
}

// GetState retrieves a value from the account storage trie.
// GetStateは、アカウントストレージトライから値を取得します。
func (s *stateObject) GetState(db Database, key common.Hash) common.Hash {
	// If the fake storage is set, only lookup the state here(in the debugging mode)
	// 偽のストレージが設定されている場合は、ここで状態を検索するだけです（デバッグモードで）
	if s.fakeStorage != nil {
		return s.fakeStorage[key]
	}
	// If we have a dirty value for this state entry, return it
	// この状態エントリにダーティな値がある場合は、それを返します
	value, dirty := s.dirtyStorage[key]
	if dirty {
		return value
	}
	// Otherwise return the entry's original value
	// それ以外の場合は、エントリの元の値を返します
	return s.GetCommittedState(db, key)
}

// GetCommittedState retrieves a value from the committed account storage trie.
// GetCommittedStateは、コミットされたアカウントストレージトライから値を取得します。
func (s *stateObject) GetCommittedState(db Database, key common.Hash) common.Hash {
	// If the fake storage is set, only lookup the state here(in the debugging mode)
	// 偽のストレージが設定されている場合は、ここで状態を検索するだけです（デバッグモードで）
	if s.fakeStorage != nil {
		return s.fakeStorage[key]
	}
	// If we have a pending write or clean cached, return that
	// 保留中の書き込みまたはクリーンキャッシュがある場合は、それを返します
	if value, pending := s.pendingStorage[key]; pending {
		return value
	}
	if value, cached := s.originStorage[key]; cached {
		return value
	}
	// If no live objects are available, attempt to use snapshots
	// 使用可能なライブオブジェクトがない場合は、スナップショットの使用を試みます
	var (
		enc   []byte
		err   error
		meter *time.Duration
	)
	readStart := time.Now()
	if metrics.EnabledExpensive {
		// If the snap is 'under construction', the first lookup may fail. If that
		// happens, we don't want to double-count the time elapsed. Thus this
		// dance with the metering.
		// スナップが「作成中」の場合、最初のルックアップが失敗する可能性があります。
		// その場合、経過時間を二重にカウントしたくありません。 したがって、このダンスは計量で行われます。
		defer func() {
			if meter != nil {
				*meter += time.Since(readStart)
			}
		}()
	}
	if s.db.snap != nil {
		if metrics.EnabledExpensive {
			meter = &s.db.SnapshotStorageReads
		}
		// If the object was destructed in *this* block (and potentially resurrected),
		// the storage has been cleared out, and we should *not* consult the previous
		// snapshot about any storage values. The only possible alternatives are:
		//   1) resurrect happened, and new slot values were set -- those should
		//      have been handles via pendingStorage above.
		//   2) we don't have new values, and can deliver empty response back
		// オブジェクトが* this *ブロックで破棄された（そして復活する可能性がある）場合、ストレージはクリアされているため、ストレージ値について前のスナップショットを参照しないでください。 可能な唯一の選択肢は次のとおりです。
		//   1）復活が起こり、新しいスロット値が設定されました-それらは
		// 上記のpendingStorageを介して処理されています。
		//   2）新しい値がなく、空の応答を返すことができます
		if _, destructed := s.db.snapDestructs[s.addrHash]; destructed {
			return common.Hash{}
		}
		enc, err = s.db.snap.Storage(s.addrHash, crypto.Keccak256Hash(key.Bytes()))
	}
	// If the snapshot is unavailable or reading from it fails, load from the database.
	// スナップショットが使用できない場合、またはスナップショットからの読み取りに失敗した場合は、データベースからロードします。
	if s.db.snap == nil || err != nil {
		if meter != nil {
			// If we already spent time checking the snapshot, account for it
			// and reset the readStart
			// スナップショットのチェックにすでに時間を費やしている場合は、
			// それを考慮してreadStartをリセットします
			*meter += time.Since(readStart)
			readStart = time.Now()
		}
		if metrics.EnabledExpensive {
			meter = &s.db.StorageReads
		}
		if enc, err = s.getTrie(db).TryGet(key.Bytes()); err != nil {
			s.setError(err)
			return common.Hash{}
		}
	}
	var value common.Hash
	if len(enc) > 0 {
		_, content, _, err := rlp.Split(enc)
		if err != nil {
			s.setError(err)
		}
		value.SetBytes(content)
	}
	s.originStorage[key] = value
	return value
}

// SetState updates a value in account storage.
// SetStateはアカウントストレージの値を更新します。
func (s *stateObject) SetState(db Database, key, value common.Hash) {
	// If the fake storage is set, put the temporary state update here.
	// 偽のストレージが設定されている場合は、一時的な状態の更新をここに配置します。
	if s.fakeStorage != nil {
		s.fakeStorage[key] = value
		return
	}
	// If the new value is the same as old, don't set
	// 新しい値が古い値と同じである場合は、設定しないでください
	prev := s.GetState(db, key)
	if prev == value {
		return
	}
	// New value is different, update and journal the change
	// 新しい値が異なります。変更を更新してジャーナルに記録します
	s.db.journal.append(storageChange{
		account:  &s.address,
		key:      key,
		prevalue: prev,
	})
	s.setState(key, value)
}

// SetStorage replaces the entire state storage with the given one.
//
// After this function is called, all original state will be ignored and state
// lookup only happens in the fake state storage.
//
// Note this function should only be used for debugging purpose.
// SetStorageは、状態ストレージ全体を指定されたものに置き換えます。
//
// この関数が呼び出された後、元の状態はすべて無視され、
// 状態ルックアップは偽の状態ストレージでのみ発生します。
//
// この関数は、デバッグ目的でのみ使用する必要があることに注意してください。
func (s *stateObject) SetStorage(storage map[common.Hash]common.Hash) {
	// Allocate fake storage if it's nil.
	// nilの場合は、偽のストレージを割り当てます。
	if s.fakeStorage == nil {
		s.fakeStorage = make(Storage)
	}
	for key, value := range storage {
		s.fakeStorage[key] = value
	}
	// Don't bother journal since this function should only be used for
	// debugging and the `fake` storage won't be committed to database.
	// この関数はデバッグにのみ使用する必要があり、
	// `fake`ストレージはデータベースにコミットされないため、ジャーナルを気にしないでください。
}

func (s *stateObject) setState(key, value common.Hash) {
	s.dirtyStorage[key] = value
}

// finalise moves all dirty storage slots into the pending area to be hashed or
// committed later. It is invoked at the end of every transaction.
// finalizeは、すべてのダーティストレージスロットを保留領域に移動して、後でハッシュまたはコミットします。
// これは、すべてのトランザクションの最後に呼び出されます。
func (s *stateObject) finalise(prefetch bool) {
	slotsToPrefetch := make([][]byte, 0, len(s.dirtyStorage))
	for key, value := range s.dirtyStorage {
		s.pendingStorage[key] = value
		if value != s.originStorage[key] {
			slotsToPrefetch = append(slotsToPrefetch, common.CopyBytes(key[:])) // Copy needed for closure
		}
	}
	if s.db.prefetcher != nil && prefetch && len(slotsToPrefetch) > 0 && s.data.Root != emptyRoot {
		s.db.prefetcher.prefetch(s.data.Root, slotsToPrefetch)
	}
	if len(s.dirtyStorage) > 0 {
		s.dirtyStorage = make(Storage)
	}
}

// updateTrie writes cached storage modifications into the object's storage trie.
// It will return nil if the trie has not been loaded and no changes have been made
// updateTrieは、キャッシュされたストレージの変更をオブジェクトのストレージトライに書き込みます。
// トライがロードされておらず、変更が加えられていない場合はnilを返します
func (s *stateObject) updateTrie(db Database) Trie {
	// Make sure all dirty slots are finalized into the pending storage area
	// すべてのダーティスロットが保留中のストレージ領域にファイナライズされていることを確認します
	s.finalise(false) // プリフェッチしないで、必要に応じて直接プルします // Don't prefetch anymore, pull directly if need be
	if len(s.pendingStorage) == 0 {
		return s.trie
	}
	// Track the amount of time wasted on updating the storage trie
	// ストレージトライの更新に浪費された時間を追跡します
	if metrics.EnabledExpensive {
		defer func(start time.Time) { s.db.StorageUpdates += time.Since(start) }(time.Now())
	}
	// The snapshot storage map for the object
	// オブジェクトのスナップショットストレージマップ
	var storage map[common.Hash][]byte
	// Insert all the pending updates into the trie
	// 保留中のすべての更新をトライに挿入します
	tr := s.getTrie(db)
	hasher := s.db.hasher

	usedStorage := make([][]byte, 0, len(s.pendingStorage))
	for key, value := range s.pendingStorage {
		// Skip noop changes, persist actual changes
		// noopの変更をスキップし、実際の変更を保持します
		if value == s.originStorage[key] {
			continue
		}
		s.originStorage[key] = value

		var v []byte
		if (value == common.Hash{}) {
			s.setError(tr.TryDelete(key[:]))
			s.db.StorageDeleted += 1
		} else {
			// Encoding []byte cannot fail, ok to ignore the error.
			// [] byteのエンコードは失敗できません。エラーを無視してもかまいません。
			v, _ = rlp.EncodeToBytes(common.TrimLeftZeroes(value[:]))
			s.setError(tr.TryUpdate(key[:], v))
			s.db.StorageUpdated += 1
		}
		// If state snapshotting is active, cache the data til commit
		// 状態のスナップショットがアクティブな場合、コミットするまでデータをキャッシュします
		if s.db.snap != nil {
			if storage == nil {
				// Retrieve the old storage map, if available, create a new one otherwise
				// 古いストレージマップを取得します（利用可能な場合）、そうでない場合は新しいストレージマップを作成します
				if storage = s.db.snapStorage[s.addrHash]; storage == nil {
					storage = make(map[common.Hash][]byte)
					s.db.snapStorage[s.addrHash] = storage
				}
			}
			storage[crypto.HashData(hasher, key[:])] = v // 削除すると、vはnilになります // v will be nil if it's deleted
		}
		usedStorage = append(usedStorage, common.CopyBytes(key[:])) // 閉鎖に必要なコピー // Copy needed for closure
	}
	if s.db.prefetcher != nil {
		s.db.prefetcher.used(s.data.Root, usedStorage)
	}
	if len(s.pendingStorage) > 0 {
		s.pendingStorage = make(Storage)
	}
	return tr
}

// UpdateRoot sets the trie root to the current root hash of
// UpdateRootは、トライルートをの現在のルートハッシュに設定します
func (s *stateObject) updateRoot(db Database) {
	// If nothing changed, don't bother with hashing anything
	// 何も変更されていない場合は、何もハッシュする必要はありません
	if s.updateTrie(db) == nil {
		return
	}
	// Track the amount of time wasted on hashing the storage trie
	// ストレージトライのハッシュに浪費された時間を追跡します
	if metrics.EnabledExpensive {
		defer func(start time.Time) { s.db.StorageHashes += time.Since(start) }(time.Now())
	}
	s.data.Root = s.trie.Hash()
}

// CommitTrie the storage trie of the object to db.
// This updates the trie root.
// オブジェクトのストレージtrueをdbにCommitTeeします。
// これによりトライルートが更新されます。
func (s *stateObject) CommitTrie(db Database) (int, error) {
	// If nothing changed, don't bother with hashing anything
	// 何も変更されていない場合は、何もハッシュする必要はありません
	if s.updateTrie(db) == nil {
		return 0, nil
	}
	if s.dbErr != nil {
		return 0, s.dbErr
	}
	// Track the amount of time wasted on committing the storage trie
	// ストレージトライのコミットに浪費された時間を追跡します
	if metrics.EnabledExpensive {
		defer func(start time.Time) { s.db.StorageCommits += time.Since(start) }(time.Now())
	}
	root, committed, err := s.trie.Commit(nil)
	if err == nil {
		s.data.Root = root
	}
	return committed, err
}

// AddBalance adds amount to s's balance.
// It is used to add funds to the destination account of a transfer.
// AddBalanceは、sのバランスに金額を追加します。
// 転送先のアカウントに資金を追加するために使用されます。
func (s *stateObject) AddBalance(amount *big.Int) {
	// EIP161: We must check emptiness for the objects such that the account
	// clearing (0,0,0 objects) can take effect.
	// EIP161：アカウントのクリア（0,0,0オブジェクト）が有効になるように、
	// オブジェクトの空をチェックする必要があります。
	if amount.Sign() == 0 {
		if s.empty() {
			s.touch()
		}
		return
	}
	s.SetBalance(new(big.Int).Add(s.Balance(), amount))
}

// SubBalance removes amount from s's balance.
// It is used to remove funds from the origin account of a transfer.
// SubBalanceは、sの残高から金額を削除します。
// 送金の元の口座から資金を削除するために使用されます。
func (s *stateObject) SubBalance(amount *big.Int) {
	if amount.Sign() == 0 {
		return
	}
	s.SetBalance(new(big.Int).Sub(s.Balance(), amount))
}

func (s *stateObject) SetBalance(amount *big.Int) {
	s.db.journal.append(balanceChange{
		account: &s.address,
		prev:    new(big.Int).Set(s.data.Balance),
	})
	s.setBalance(amount)
}

func (s *stateObject) setBalance(amount *big.Int) {
	s.data.Balance = amount
}

func (s *stateObject) SetLastBlockNumber(LastBlockNumber *big.Int) {
	s.db.journal.append(LastBlockNumberChange{
		account: &s.address,
		prev:    new(big.Int).Set(s.data.LastBlockNumber),
	})
	s.setLastBlockNumber(LastBlockNumber)
}

func (s *stateObject) setLastBlockNumber(LastBlockNumber *big.Int) {
	s.data.LastBlockNumber = LastBlockNumber
}

func (s *stateObject) deepCopy(db *StateDB) *stateObject {
	stateObject := newObject(db, s.address, s.data)
	if s.trie != nil {
		stateObject.trie = db.db.CopyTrie(s.trie)
	}
	stateObject.code = s.code
	stateObject.dirtyStorage = s.dirtyStorage.Copy()
	stateObject.originStorage = s.originStorage.Copy()
	stateObject.pendingStorage = s.pendingStorage.Copy()
	stateObject.suicided = s.suicided
	stateObject.dirtyCode = s.dirtyCode
	stateObject.deleted = s.deleted
	return stateObject
}

//
// Attribute accessors
//
//
// 属性アクセサー
//

// Returns the address of the contract/account
// 契約/アカウントのアドレスを返します
func (s *stateObject) Address() common.Address {
	return s.address
}

// Code returns the contract code associated with this object, if any.
// コードは、このオブジェクトに関連付けられているコントラクトコードを返します（存在する場合）。
func (s *stateObject) Code(db Database) []byte {
	if s.code != nil {
		return s.code
	}
	if bytes.Equal(s.CodeHash(), emptyCodeHash) {
		return nil
	}
	code, err := db.ContractCode(s.addrHash, common.BytesToHash(s.CodeHash()))
	if err != nil {
		s.setError(fmt.Errorf("can't load code hash %x: %v", s.CodeHash(), err))
	}
	s.code = code
	return code
}

// CodeSize returns the size of the contract code associated with this object,
// or zero if none. This method is an almost mirror of Code, but uses a cache
// inside the database to avoid loading codes seen recently.
// CodeSizeは、このオブジェクトに関連付けられているコントラクトコードのサイズを返します。
// ない場合はゼロを返します。
// このメソッドはコードのほぼミラーですが、最近見られたコードのロードを回避するためにデータベース内のキャッシュを使用します。
func (s *stateObject) CodeSize(db Database) int {
	if s.code != nil {
		return len(s.code)
	}
	if bytes.Equal(s.CodeHash(), emptyCodeHash) {
		return 0
	}
	size, err := db.ContractCodeSize(s.addrHash, common.BytesToHash(s.CodeHash()))
	if err != nil {
		s.setError(fmt.Errorf("can't load code size %x: %v", s.CodeHash(), err))
	}
	return size
}

func (s *stateObject) SetCode(codeHash common.Hash, code []byte) {
	prevcode := s.Code(s.db.db)
	s.db.journal.append(codeChange{
		account:  &s.address,
		prevhash: s.CodeHash(),
		prevcode: prevcode,
	})
	s.setCode(codeHash, code)
}

func (s *stateObject) setCode(codeHash common.Hash, code []byte) {
	s.code = code
	s.data.CodeHash = codeHash[:]
	s.dirtyCode = true
}

func (s *stateObject) SetNonce(nonce uint64) {
	s.db.journal.append(nonceChange{
		account: &s.address,
		prev:    s.data.Nonce,
	})
	s.setNonce(nonce)
}

func (s *stateObject) setNonce(nonce uint64) {
	s.data.Nonce = nonce
}

func (s *stateObject) CodeHash() []byte {
	return s.data.CodeHash
}

func (s *stateObject) Balance() *big.Int {
	return s.data.Balance
}

func (s *stateObject) LastBlockNumber() *big.Int {
	return s.data.LastBlockNumber
}

func (s *stateObject) Nonce() uint64 {
	return s.data.Nonce
}

// Never called, but must be present to allow stateObject to be used
// as a vm.Account interface that also satisfies the vm.ContractRef
// interface. Interfaces are awesome.
// 呼び出されることはありませんが、stateObjectをvm.ContractRefインターフェイスも満たす
// vm.Accountインターフェイスとして使用できるようにするために存在する必要があります。
// インターフェースは素晴らしいです。
func (s *stateObject) Value() *big.Int {
	panic("Value on stateObject should never be called")
}
