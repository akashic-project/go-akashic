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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

const journalVersion uint64 = 0

// journalGenerator is a disk layer entry containing the generator progress marker.
// journalGeneratorは、ジェネレータの進行状況マーカーを含むディスクレイヤーエントリです。
type journalGenerator struct {
	// Indicator that whether the database was in progress of being wiped.
	// It's deprecated but keep it here for background compatibility.
	// データベースがワイプされているかどうかを示します。
	// 非推奨ですが、バックグラウンド互換性のためにここに保持します。
	Wiping bool

	Done     bool // ジェネレータがスナップショットの作成を終了したかどうか  // Whether the generator finished creating the snapshot
	Marker   []byte
	Accounts uint64
	Slots    uint64
	Storage  uint64
}

// journalDestruct is an account deletion entry in a diffLayer's disk journal.
// journalDestructは、diffLayerのディスクジャーナルのアカウント削除エントリです。
type journalDestruct struct {
	Hash common.Hash
}

// journalAccount is an account entry in a diffLayer's disk journal.
// journalAccountは、diffLayerのディスクジャーナルのアカウントエントリです。
type journalAccount struct {
	Hash common.Hash
	Blob []byte
}

// journalStorage is an account's storage map in a diffLayer's disk journal.
// journalStorageは、diffLayerのディスクジャーナルにあるアカウントのストレージマップです。
type journalStorage struct {
	Hash common.Hash
	Keys []common.Hash
	Vals [][]byte
}

// loadAndParseJournal tries to parse the snapshot journal in latest format.
// loadAndParseJournalは、スナップショットジャーナルを最新の形式で解析しようとします。
func loadAndParseJournal(db ethdb.KeyValueStore, base *diskLayer) (snapshot, journalGenerator, error) {
	// Retrieve the disk layer generator. It must exist, no matter the
	// snapshot is fully generated or not. Otherwise the entire disk
	// layer is invalid.
	// ディスクレイヤージェネレーターを取得します。
	// スナップショットが完全に生成されているかどうかに関係なく、存在している必要があります。
	// そうしないと、ディスクレイヤー全体が無効になります。
	generatorBlob := rawdb.ReadSnapshotGenerator(db)
	if len(generatorBlob) == 0 {
		return nil, journalGenerator{}, errors.New("missing snapshot generator") // スナップショットジェネレータがありません
	}
	var generator journalGenerator
	if err := rlp.DecodeBytes(generatorBlob, &generator); err != nil {
		return nil, journalGenerator{}, fmt.Errorf("failed to decode snapshot generator: %v", err) // スナップショットジェネレータのデコードに失敗しました：％v
	}
	// Retrieve the diff layer journal. It's possible that the journal is
	// not existent, e.g. the disk layer is generating while that the Geth
	// crashes without persisting the diff journal.
	// So if there is no journal, or the journal is invalid(e.g. the journal
	// is not matched with disk layer; or the it's the legacy-format journal,
	// etc.), we just discard all diffs and try to recover them later.
	// diffレイヤージャーナルを取得します。ジャーナルが存在しない可能性があります
	// 差分ジャーナルを保持せずにGethがクラッシュする間、ディスクレイヤーが生成されます。
	// したがって、ジャーナルがない場合、またはジャーナルが無効である場合
	// （たとえば、ジャーナルがディスクレイヤーと一致していない場合、
	// またはレガシー形式のジャーナルである場合など）、すべての差分を破棄し、
	// 後でそれらを回復しようとします。
	journal := rawdb.ReadSnapshotJournal(db)
	if len(journal) == 0 {
		log.Warn("Loaded snapshot journal", "diskroot", base.root, "diffs", "missing") // ロードされたスナップショットジャーナル
		return base, generator, nil
	}
	r := rlp.NewStream(bytes.NewReader(journal), 0)

	// Firstly, resolve the first element as the journal version
	// まず、最初の要素をジャーナルバージョンとして解決します
	version, err := r.Uint()
	if err != nil {
		log.Warn("Failed to resolve the journal version", "error", err) // ジャーナルバージョンの解決に失敗しました
		return base, generator, nil
	}
	if version != journalVersion {
		log.Warn("Discarded the snapshot journal with wrong version", "required", journalVersion, "got", version) // 間違ったバージョンのスナップショットジャーナルを破棄しました

		return base, generator, nil
	}
	// Secondly, resolve the disk layer root, ensure it's continuous
	// with disk layer. Note now we can ensure it's the snapshot journal
	// correct version, so we expect everything can be resolved properly.
	// 次に、ディスクレイヤールートを解決し、ディスクレイヤーと連続していることを確認します。
	// これで、スナップショットジャーナルの正しいバージョンであることを確認できるため、
	// すべてが適切に解決されることが期待されます。
	var root common.Hash
	if err := r.Decode(&root); err != nil {
		return nil, journalGenerator{}, errors.New("missing disk layer root") // ディスクレイヤールートがありません
	}
	// The diff journal is not matched with disk, discard them.
	// It can happen that Geth crashes without persisting the latest
	// diff journal.
	// 差分ジャーナルがディスクと一致していません。破棄してください。
	// 最新のdiffジャーナルを保持せずにGethがクラッシュする可能性があります。
	if !bytes.Equal(root.Bytes(), base.root.Bytes()) {
		log.Warn("Loaded snapshot journal", "diskroot", base.root, "diffs", "unmatched") // ロードされたスナップショットジャーナル
		return base, generator, nil
	}
	// Load all the snapshot diffs from the journal
	// ジャーナルからすべてのスナップショット差分をロードします
	snapshot, err := loadDiffLayer(base, r)
	if err != nil {
		return nil, journalGenerator{}, err
	}
	log.Debug("Loaded snapshot journal", "diskroot", base.root, "diffhead", snapshot.Root()) // ロードされたスナップショットジャーナル
	return snapshot, generator, nil
}

// loadSnapshot loads a pre-existing state snapshot backed by a key-value store.
// loadSnapshotは、Key-Valueストアに基づく既存の状態スナップショットをロードします。
func loadSnapshot(diskdb ethdb.KeyValueStore, triedb *trie.Database, cache int, root common.Hash, recovery bool) (snapshot, bool, error) {
	// If snapshotting is disabled (initial sync in progress), don't do anything,
	// wait for the chain to permit us to do something meaningful
	// スナップショットが無効になっている場合（初期同期が進行中）、
	// 何もせず、チェーンが意味のあることを実行できるようになるのを待ちます
	if rawdb.ReadSnapshotDisabled(diskdb) {
		return nil, true, nil
	}
	// Retrieve the block number and hash of the snapshot, failing if no snapshot
	// is present in the database (or crashed mid-update).
	// スナップショットのブロック番号とハッシュを取得します。
	// データベースにスナップショットが存在しない場合（または更新中にクラッシュした場合）は失敗します。
	baseRoot := rawdb.ReadSnapshotRoot(diskdb)
	if baseRoot == (common.Hash{}) {
		return nil, false, errors.New("missing or corrupted snapshot") // スナップショットの欠落または破損
	}
	base := &diskLayer{
		diskdb: diskdb,
		triedb: triedb,
		cache:  fastcache.New(cache * 1024 * 1024),
		root:   baseRoot,
	}
	snapshot, generator, err := loadAndParseJournal(diskdb, base)
	if err != nil {
		log.Warn("Failed to load new-format journal", "error", err) // 新しい形式のジャーナルを読み込めませんでした
		return nil, false, err
	}
	// Entire snapshot journal loaded, sanity check the head. If the loaded
	// snapshot is not matched with current state root, print a warning log
	// or discard the entire snapshot it's legacy snapshot.
	//
	// Possible scenario: Geth was crashed without persisting journal and then
	// restart, the head is rewound to the point with available state(trie)
	// which is below the snapshot. In this case the snapshot can be recovered
	// by re-executing blocks but right now it's unavailable.
	// スナップショットジャーナル全体が読み込まれ、健全性がヘッドをチェックします。
	// ロードされたスナップショットが現在の状態のルートと一致しない場合は、
	// 警告ログを印刷するか、スナップショット全体をレガシースナップショットとして破棄します。
	//
	// 考えられるシナリオ：ジャーナルを永続化せずにGethがクラッシュしてから再起動すると、
	// スナップショットの下にある使用可能な状態（トライ）のポイントにヘッドが巻き戻されます。
	// この場合、スナップショットはブロックを再実行することで回復できますが、現在は使用できません
	if head := snapshot.Root(); head != root {
		// If it's legacy snapshot, or it's new-format snapshot but
		// it's not in recovery mode, returns the error here for
		// rebuilding the entire snapshot forcibly.
		// レガシースナップショット、または新しい形式のスナップショットであるがリカバリモードではない場合、
		// スナップショット全体を強制的に再構築すると、ここでエラーが返されます。
		if !recovery {
			return nil, false, fmt.Errorf("head doesn't match snapshot: have %#x, want %#x", head, root) // ヘッドがスナップショットと一致しません：％＃xがあり、％＃xが必要です
		}
		// It's in snapshot recovery, the assumption is held that
		// the disk layer is always higher than chain head. It can
		// be eventually recovered when the chain head beyonds the
		// disk layer.
		// スナップショットリカバリであり、ディスクレイヤーは常にチェーンヘッドよりも高いと想定されています。
		// チェーンヘッドがディスク層を超えたときに、最終的に回復することができます。
		log.Warn("Snapshot is not continuous with chain", "snaproot", head, "chainroot", root) // スナップショットはチェーンと連続していません
	}
	// Everything loaded correctly, resume any suspended operations
	// すべてが正しく読み込まれ、中断された操作を再開します
	if !generator.Done {
		// Whether or not wiping was in progress, load any generator progress too
		// ワイプが進行中であるかどうかに関係なく、ジェネレーターの進行状況もロードします
		base.genMarker = generator.Marker
		if base.genMarker == nil {
			base.genMarker = []byte{}
		}
		base.genPending = make(chan struct{})
		base.genAbort = make(chan chan *generatorStats)

		var origin uint64
		if len(generator.Marker) >= 8 {
			origin = binary.BigEndian.Uint64(generator.Marker)
		}
		go base.generate(&generatorStats{
			origin:   origin,
			start:    time.Now(),
			accounts: generator.Accounts,
			slots:    generator.Slots,
			storage:  common.StorageSize(generator.Storage),
		})
	}
	return snapshot, false, nil
}

// loadDiffLayer reads the next sections of a snapshot journal, reconstructing a new
// diff and verifying that it can be linked to the requested parent.
// loadDiffLayerは、スナップショットジャーナルの次のセクションを読み取り、
// 新しいdiffを再構築し、要求された親にリンクできることを確認します。
func loadDiffLayer(parent snapshot, r *rlp.Stream) (snapshot, error) {
	// Read the next diff journal entry
	// 次のdiffジャーナルエントリを読みます
	var root common.Hash
	if err := r.Decode(&root); err != nil {
		// The first read may fail with EOF, marking the end of the journal
		// 最初の読み取りがEOFで失敗し、ジャーナルの終わりを示す場合があります
		if err == io.EOF {
			return parent, nil
		}
		return nil, fmt.Errorf("load diff root: %v", err)
	}
	var destructs []journalDestruct
	if err := r.Decode(&destructs); err != nil {
		return nil, fmt.Errorf("load diff destructs: %v", err)
	}
	destructSet := make(map[common.Hash]struct{})
	for _, entry := range destructs {
		destructSet[entry.Hash] = struct{}{}
	}
	var accounts []journalAccount
	if err := r.Decode(&accounts); err != nil {
		return nil, fmt.Errorf("load diff accounts: %v", err)
	}
	accountData := make(map[common.Hash][]byte)
	for _, entry := range accounts {
		if len(entry.Blob) > 0 { // RLPはnil-nessを失いますが、 `[] byte {}`は有効なアイテムではないため、それを再解釈します // RLP loses nil-ness, but `[]byte{}` is not a valid item, so reinterpret that
			accountData[entry.Hash] = entry.Blob
		} else {
			accountData[entry.Hash] = nil
		}
	}
	var storage []journalStorage
	if err := r.Decode(&storage); err != nil {
		return nil, fmt.Errorf("load diff storage: %v", err) // 差分ストレージのロード：％v
	}
	storageData := make(map[common.Hash]map[common.Hash][]byte)
	for _, entry := range storage {
		slots := make(map[common.Hash][]byte)
		for i, key := range entry.Keys {
			if len(entry.Vals[i]) > 0 { // RLPはnil-nessを失いますが、 `[] byte {}`は有効なアイテムではないため、それを再解釈します // RLP loses nil-ness, but `[]byte{}` is not a valid item, so reinterpret that
				slots[key] = entry.Vals[i]
			} else {
				slots[key] = nil
			}
		}
		storageData[entry.Hash] = slots
	}
	return loadDiffLayer(newDiffLayer(parent, root, destructSet, accountData, storageData), r)
}

// Journal terminates any in-progress snapshot generation, also implicitly pushing
// the progress into the database.
// Journalは、進行中のスナップショットの生成を終了し、進行状況をデータベースに暗黙的にプッシュします。
func (dl *diskLayer) Journal(buffer *bytes.Buffer) (common.Hash, error) {
	// If the snapshot is currently being generated, abort it
	// スナップショットが現在生成されている場合は、中止します
	var stats *generatorStats
	if dl.genAbort != nil {
		abort := make(chan *generatorStats)
		dl.genAbort <- abort

		if stats = <-abort; stats != nil {
			stats.Log("Journalling in-progress snapshot", dl.root, dl.genMarker) // ジャーナルの進行中のスナップショット
		}
	}
	// Ensure the layer didn't get stale
	// レイヤーが古くなっていないことを確認します
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	if dl.stale {
		return common.Hash{}, ErrSnapshotStale
	}
	// Ensure the generator stats is written even if none was ran this cycle
	// このサイクルで何も実行されなかった場合でも、ジェネレーターの統計が書き込まれるようにします
	journalProgress(dl.diskdb, dl.genMarker, stats)

	log.Debug("Journalled disk layer", "root", dl.root) // ジャーナル化されたディスクレイヤー
	return dl.root, nil
}

// Journal writes the memory layer contents into a buffer to be stored in the
// database as the snapshot journal.
// Journalは、メモリレイヤーの内容をバッファーに書き込み、
// スナップショットジャーナルとしてデータベースに保存します。
func (dl *diffLayer) Journal(buffer *bytes.Buffer) (common.Hash, error) {
	// Journal the parent first
	// 最初に親をジャーナルします
	base, err := dl.parent.Journal(buffer)
	if err != nil {
		return common.Hash{}, err
	}
	// Ensure the layer didn't get stale
	// レイヤーが古くなっていないことを確認します
	dl.lock.RLock()
	defer dl.lock.RUnlock()

	if dl.Stale() {
		return common.Hash{}, ErrSnapshotStale
	}
	// Everything below was journalled, persist this layer too
	// 以下のすべてがジャーナル化されました、このレイヤーも永続化します
	if err := rlp.Encode(buffer, dl.root); err != nil {
		return common.Hash{}, err
	}
	destructs := make([]journalDestruct, 0, len(dl.destructSet))
	for hash := range dl.destructSet {
		destructs = append(destructs, journalDestruct{Hash: hash})
	}
	if err := rlp.Encode(buffer, destructs); err != nil {
		return common.Hash{}, err
	}
	accounts := make([]journalAccount, 0, len(dl.accountData))
	for hash, blob := range dl.accountData {
		accounts = append(accounts, journalAccount{Hash: hash, Blob: blob})
	}
	if err := rlp.Encode(buffer, accounts); err != nil {
		return common.Hash{}, err
	}
	storage := make([]journalStorage, 0, len(dl.storageData))
	for hash, slots := range dl.storageData {
		keys := make([]common.Hash, 0, len(slots))
		vals := make([][]byte, 0, len(slots))
		for key, val := range slots {
			keys = append(keys, key)
			vals = append(vals, val)
		}
		storage = append(storage, journalStorage{Hash: hash, Keys: keys, Vals: vals})
	}
	if err := rlp.Encode(buffer, storage); err != nil {
		return common.Hash{}, err
	}
	log.Debug("Journalled diff layer", "root", dl.root, "parent", dl.parent.Root())
	return base, nil
}
