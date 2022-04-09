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
	"math/big"
	"time"

	"github.com/VictoriaMetrics/fastcache"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

var (
	// emptyRoot is the known root hash of an empty trie.
	// emptyRootは、空のトライの既知のルートハッシュです。
	emptyRoot = common.HexToHash("56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

	// emptyCode is the known hash of the empty EVM bytecode.
	// emptyCodeは、空のEVMバイトコードの既知のハッシュです。
	emptyCode = crypto.Keccak256Hash(nil)

	// accountCheckRange is the upper limit of the number of accounts involved in
	// each range check. This is a value estimated based on experience. If this
	// value is too large, the failure rate of range prove will increase. Otherwise
	// the value is too small, the efficiency of the state recovery will decrease.
	// accountCheckRangeは、各範囲チェックに関係するアカウント数の上限です。
	// これは経験に基づいて推定された値です。この値が大きすぎると、
	// 範囲証明の故障率が高くなります。
	// そうしないと、値が小さすぎるため、状態回復の効率が低下します。
	accountCheckRange = 128

	// storageCheckRange is the upper limit of the number of storage slots involved
	// in each range check. This is a value estimated based on experience. If this
	// value is too large, the failure rate of range prove will increase. Otherwise
	// the value is too small, the efficiency of the state recovery will decrease.
	// storageCheckRangeは、関係するストレージスロット数の上限です
	// 各範囲チェックで。これは経験に基づいて推定された値です。
	// この値が大きすぎると、範囲証明の故障率が高くなります。
	// そうしないと、値が小さすぎるため、状態回復の効率が低下します。
	storageCheckRange = 1024

	// errMissingTrie is returned if the target trie is missing while the generation
	// is running. In this case the generation is aborted and wait the new signal.
	// 生成の実行中にターゲットトライが欠落している場合、errMissingTrieが返されます。
	// この場合、生成は中止され、新しいシグナルを待ちます。
	errMissingTrie = errors.New("missing trie")
)

// Metrics in generation
// 生成中のメトリック
var (
	snapGeneratedAccountMeter     = metrics.NewRegisteredMeter("state/snapshot/generation/account/generated", nil)
	snapRecoveredAccountMeter     = metrics.NewRegisteredMeter("state/snapshot/generation/account/recovered", nil)
	snapWipedAccountMeter         = metrics.NewRegisteredMeter("state/snapshot/generation/account/wiped", nil)
	snapMissallAccountMeter       = metrics.NewRegisteredMeter("state/snapshot/generation/account/missall", nil)
	snapGeneratedStorageMeter     = metrics.NewRegisteredMeter("state/snapshot/generation/storage/generated", nil)
	snapRecoveredStorageMeter     = metrics.NewRegisteredMeter("state/snapshot/generation/storage/recovered", nil)
	snapWipedStorageMeter         = metrics.NewRegisteredMeter("state/snapshot/generation/storage/wiped", nil)
	snapMissallStorageMeter       = metrics.NewRegisteredMeter("state/snapshot/generation/storage/missall", nil)
	snapSuccessfulRangeProofMeter = metrics.NewRegisteredMeter("state/snapshot/generation/proof/success", nil)
	snapFailedRangeProofMeter     = metrics.NewRegisteredMeter("state/snapshot/generation/proof/failure", nil)

	// snapAccountProveCounter measures time spent on the account proving
	// snapAccountProveCounterは、アカウントの証明に費やされた時間を測定します
	snapAccountProveCounter = metrics.NewRegisteredCounter("state/snapshot/generation/duration/account/prove", nil)
	// snapAccountTrieReadCounter measures time spent on the account trie iteration
	// snapAccountTrieReadCounterは、アカウントの試行の反復に費やされた時間を測定します
	snapAccountTrieReadCounter = metrics.NewRegisteredCounter("state/snapshot/generation/duration/account/trieread", nil)
	// snapAccountSnapReadCounter measues time spent on the snapshot account iteration
	// snapAccountSnapReadCounterは、スナップショットアカウントの反復に費やされた時間を測定します
	snapAccountSnapReadCounter = metrics.NewRegisteredCounter("state/snapshot/generation/duration/account/snapread", nil)
	// snapAccountWriteCounter measures time spent on writing/updating/deleting accounts
	// snapAccountWriteCounterは、アカウントの書き込み/更新/削除に費やされた時間を測定します
	snapAccountWriteCounter = metrics.NewRegisteredCounter("state/snapshot/generation/duration/account/write", nil)
	// snapStorageProveCounter measures time spent on storage proving
	// snapStorageProveCounterは、ストレージの証明に費やされた時間を測定します
	snapStorageProveCounter = metrics.NewRegisteredCounter("state/snapshot/generation/duration/storage/prove", nil)
	// snapStorageTrieReadCounter measures time spent on the storage trie iteration
	// snapStorageTrieReadCounterは、ストレージトライの反復に費やされた時間を測定します
	snapStorageTrieReadCounter = metrics.NewRegisteredCounter("state/snapshot/generation/duration/storage/trieread", nil)
	// snapStorageSnapReadCounter measures time spent on the snapshot storage iteration
	// snapStorageSnapReadCounterは、スナップショットストレージの反復に費やされた時間を測定します
	snapStorageSnapReadCounter = metrics.NewRegisteredCounter("state/snapshot/generation/duration/storage/snapread", nil)
	// snapStorageWriteCounter measures time spent on writing/updating/deleting storages
	// snapStorageWriteCounterは、ストレージの書き込み/更新/削除に費やされた時間を測定します
	snapStorageWriteCounter = metrics.NewRegisteredCounter("state/snapshot/generation/duration/storage/write", nil)
)

// generatorStats is a collection of statistics gathered by the snapshot generator
// for logging purposes.
// generatorStatsは、ロギングの目的でスナップショットジェネレーターによって収集された統計のコレクションです。
type generatorStats struct {
	origin   uint64             // 生成が開始された元のプレフィックス                         // Origin prefix where generation started
	start    time.Time          // 生成が開始されたときのタイムスタンプ                       // Timestamp when generation started
	accounts uint64             // インデックス付けされた（生成または回復された）アカウントの数 // Number of accounts indexed(generated or recovered)
	slots    uint64             // インデックス付けされた（生成または回復された）ストレージスロットの数 // Number of storage slots indexed(generated or recovered)
	storage  common.StorageSize // アカウントとストレージスロットの合計サイズ（生成またはリカバリ） // Total account and storage slot size(generation or recovery)
}

// Log creates an contextual log with the given message and the context pulled
// from the internally maintained statistics.
// Logは、指定されたメッセージと、内部で維持されている統計から取得されたコンテキストを使用してコンテキストログを作成します。
func (gs *generatorStats) Log(msg string, root common.Hash, marker []byte) {
	var ctx []interface{}
	if root != (common.Hash{}) {
		ctx = append(ctx, []interface{}{"root", root}...)
	}
	// Figure out whether we're after or within an account
	// アカウントをフォローしているのかアカウント内にいるのかを把握する
	switch len(marker) {
	case common.HashLength:
		ctx = append(ctx, []interface{}{"at", common.BytesToHash(marker)}...)
	case 2 * common.HashLength:
		ctx = append(ctx, []interface{}{
			"in", common.BytesToHash(marker[:common.HashLength]),
			"at", common.BytesToHash(marker[common.HashLength:]),
		}...)
	}
	// Add the usual measurements
	// 通常の測定値を追加します
	ctx = append(ctx, []interface{}{
		"accounts", gs.accounts,
		"slots", gs.slots,
		"storage", gs.storage,
		"elapsed", common.PrettyDuration(time.Since(gs.start)),
	}...)
	// Calculate the estimated indexing time based on current stats
	// 現在の統計に基づいて推定インデックス作成時間を計算します
	if len(marker) > 0 {
		if done := binary.BigEndian.Uint64(marker[:8]) - gs.origin; done > 0 {
			left := math.MaxUint64 - binary.BigEndian.Uint64(marker[:8])

			speed := done/uint64(time.Since(gs.start)/time.Millisecond+1) + 1 // ゼロによる除算を回避するための+1 // +1s to avoid division by zero
			ctx = append(ctx, []interface{}{
				"eta", common.PrettyDuration(time.Duration(left/speed) * time.Millisecond),
			}...)
		}
	}
	log.Info(msg, ctx...)
}

// generateSnapshot regenerates a brand new snapshot based on an existing state
// database and head block asynchronously. The snapshot is returned immediately
// and generation is continued in the background until done.
// generateSnapshotは、既存の状態データベースとヘッドブロックに非同期で基づいて新しいスナップショットを再生成します。
// スナップショットはすぐに返され、生成は完了するまでバックグラウンドで続行されます。
func generateSnapshot(diskdb ethdb.KeyValueStore, triedb *trie.Database, cache int, root common.Hash) *diskLayer {
	// Create a new disk layer with an initialized state marker at zero
	// 初期化された状態マーカーをゼロにして新しいディスクレイヤーを作成します
	var (
		stats     = &generatorStats{start: time.Now()}
		batch     = diskdb.NewBatch()
		genMarker = []byte{} // 初期化されていますが、空です！ // Initialized but empty!
	)
	rawdb.WriteSnapshotRoot(batch, root)
	journalProgress(batch, genMarker, stats)
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write initialized state marker", "err", err) // 初期化された状態マーカーの書き込みに失敗しました
	}
	base := &diskLayer{
		diskdb:     diskdb,
		triedb:     triedb,
		root:       root,
		cache:      fastcache.New(cache * 1024 * 1024),
		genMarker:  genMarker,
		genPending: make(chan struct{}),
		genAbort:   make(chan chan *generatorStats),
	}
	go base.generate(stats)
	log.Debug("Start snapshot generation", "root", root) // スナップショットの生成を開始します
	return base
}

// journalProgress persists the generator stats into the database to resume later.
// journalProgressは、後で再開するためにジェネレーター統計をデータベースに保持します。
func journalProgress(db ethdb.KeyValueWriter, marker []byte, stats *generatorStats) {
	// Write out the generator marker. Note it's a standalone disk layer generator
	// which is not mixed with journal. It's ok if the generator is persisted while
	// journal is not.
	// ジェネレータマーカーを書き出します。
	// これは、ジャーナルと混合されていないスタンドアロンのディスクレイヤージェネレーターであることに注意してください。
	// ジャーナルが永続化されていないのにジェネレーターが永続化されていても問題ありません。
	entry := journalGenerator{
		Done:   marker == nil,
		Marker: marker,
	}
	if stats != nil {
		entry.Accounts = stats.accounts
		entry.Slots = stats.slots
		entry.Storage = uint64(stats.storage)
	}
	blob, err := rlp.EncodeToBytes(entry)
	if err != nil {
		panic(err) // 発生しません、ここで開発エラーをキャッチします // Cannot happen, here to catch dev errors
	}
	var logstr string
	switch {
	case marker == nil:
		logstr = "done"
	case bytes.Equal(marker, []byte{}):
		logstr = "empty"
	case len(marker) == common.HashLength:
		logstr = fmt.Sprintf("%#x", marker)
	default:
		logstr = fmt.Sprintf("%#x:%#x", marker[:common.HashLength], marker[common.HashLength:])
	}
	log.Debug("Journalled generator progress", "progress", logstr) // ジャーナル化されたジェネレーターの進捗状況
	rawdb.WriteSnapshotGenerator(db, blob)
}

// proofResult contains the output of range proving which can be used
// for further processing regardless if it is successful or not.
// proofResultには、成功したかどうかに関係なく、さらなる処理に使用できる範囲証明の出力が含まれています。
type proofResult struct {
	keys     [][]byte   // 反復されているすべての要素のキーセット、証明さえ失敗している // The key set of all elements being iterated, even proving is failed
	vals     [][]byte   // 反復されているすべての要素のvalセット、証明さえ失敗します // The val set of all elements being iterated, even proving is failed
	diskMore bool       // 最後の反復以降にデータベースに追加のスナップショット状態がある場合に設定 // Set when the database has extra snapshot states since last iteration
	trieMore bool       // トライに追加のスナップショット状態がある場合に設定します（正常な証明にのみ意味があります） // Set when the trie has extra snapshot states(only meaningful for successful proving)
	proofErr error      // 指定された状態範囲が有効かどうかを示します // Indicator whether the given state range is valid or not
	tr       *trie.Trie // トライは、証明者によって解決された場合（nilの場合もあります）// The trie, in case the trie was resolved by the prover (may be nil)
}

// valid returns the indicator that range proof is successful or not.
// validは、範囲証明が成功したかどうかを示すインジケーターを返します。
func (result *proofResult) valid() bool {
	return result.proofErr == nil
}

// last returns the last verified element key regardless of whether the range proof is
// successful or not. Nil is returned if nothing involved in the proving.
// 範囲証明が成功したかどうかに関係なく、lastは最後に検証された要素キーを返します。
// 証明に何も関与していない場合は、Nilが返されます。
func (result *proofResult) last() []byte {
	var last []byte
	if len(result.keys) > 0 {
		last = result.keys[len(result.keys)-1]
	}
	return last
}

// forEach iterates all the visited elements and applies the given callback on them.
// The iteration is aborted if the callback returns non-nil error.
// forEachは、訪問したすべての要素を繰り返し、指定されたコールバックをそれらに適用します。
// コールバックがnil以外のエラーを返した場合、反復は中止されます。
func (result *proofResult) forEach(callback func(key []byte, val []byte) error) error {
	for i := 0; i < len(result.keys); i++ {
		key, val := result.keys[i], result.vals[i]
		if err := callback(key, val); err != nil {
			return err
		}
	}
	return nil
}

// proveRange proves the snapshot segment with particular prefix is "valid".
// The iteration start point will be assigned if the iterator is restored from
// the last interruption. Max will be assigned in order to limit the maximum
// amount of data involved in each iteration.
//
// The proof result will be returned if the range proving is finished, otherwise
// the error will be returned to abort the entire procedure.
// proveRangeは特定のプレフィックスを持つスナップショットセグメントが「有効」であることを証明します。
// イテレータが最後の中断から復元された場合、反復開始点が割り当てられます。
// 各反復に含まれるデータの最大量を制限するために、Maxが割り当てられます。
//
// 範囲の証明が終了すると、証明結果が返されます。それ以外の場合は、エラーが返され、手順全体が中止されます。
func (dl *diskLayer) proveRange(stats *generatorStats, root common.Hash, prefix []byte, kind string, origin []byte, max int, valueConvertFn func([]byte) ([]byte, error)) (*proofResult, error) {
	var (
		keys     [][]byte
		vals     [][]byte
		proof    = rawdb.NewMemoryDatabase()
		diskMore = false
	)
	iter := dl.diskdb.NewIterator(prefix, origin)
	defer iter.Release()

	var start = time.Now()
	for iter.Next() {
		key := iter.Key()
		if len(key) != len(prefix)+common.HashLength {
			continue
		}
		if len(keys) == max {
			// Break if we've reached the max size, and signal that we're not
			// done yet.
			// 最大サイズに達した場合は中断し、まだ完了していないことを通知します。
			diskMore = true
			break
		}
		keys = append(keys, common.CopyBytes(key[len(prefix):]))

		if valueConvertFn == nil {
			vals = append(vals, common.CopyBytes(iter.Value()))
		} else {
			val, err := valueConvertFn(iter.Value())
			if err != nil {
				// Special case, the state data is corrupted (invalid slim-format account),
				// don't abort the entire procedure directly. Instead, let the fallback
				// generation to heal the invalid data.
				//
				// Here append the original value to ensure that the number of key and
				// value are the same.
				// 特別な場合、状態データが破損しています（無効なスリムフォーマットアカウント）。
				// 手順全体を直接中止しないでください。代わりに、フォールバック生成で無効なデータを修復します。
				//
				// ここで元の値を追加して、キーの数と値が同じになるようにします。
				vals = append(vals, common.CopyBytes(iter.Value()))
				log.Error("Failed to convert account state data", "err", err)
			} else {
				vals = append(vals, val)
			}
		}
	}
	// Update metrics for database iteration and merkle proving
	// データベースの反復とマークル証明のメトリックを更新します
	if kind == "storage" {
		snapStorageSnapReadCounter.Inc(time.Since(start).Nanoseconds())
	} else {
		snapAccountSnapReadCounter.Inc(time.Since(start).Nanoseconds())
	}
	defer func(start time.Time) {
		if kind == "storage" {
			snapStorageProveCounter.Inc(time.Since(start).Nanoseconds())
		} else {
			snapAccountProveCounter.Inc(time.Since(start).Nanoseconds())
		}
	}(time.Now())

	// The snap state is exhausted, pass the entire key/val set for verification
	// スナップ状態が使い果たされ、検証のためにキー/値セット全体を渡します
	if origin == nil && !diskMore {
		stackTr := trie.NewStackTrie(nil)
		for i, key := range keys {
			stackTr.TryUpdate(key, vals[i])
		}
		if gotRoot := stackTr.Hash(); gotRoot != root {
			return &proofResult{
				keys:     keys,
				vals:     vals,
				proofErr: fmt.Errorf("wrong root: have %#x want %#x", gotRoot, root),
			}, nil
		}
		return &proofResult{keys: keys, vals: vals}, nil
	}
	// Snap state is chunked, generate edge proofs for verification.
	// スナップ状態はチャンク化され、検証用のエッジプルーフを生成します。
	tr, err := trie.New(root, dl.triedb)
	if err != nil {
		stats.Log("Trie missing, state snapshotting paused", dl.root, dl.genMarker) // 試行がありません、状態のスナップショットが一時停止しました
		return nil, errMissingTrie
	}
	// Firstly find out the key of last iterated element.
	// 最初に、最後に繰り返された要素のキーを見つけます。
	var last []byte
	if len(keys) > 0 {
		last = keys[len(keys)-1]
	}
	// Generate the Merkle proofs for the first and last element
	// 最初と最後の要素のMerkle証明を生成します
	if origin == nil {
		origin = common.Hash{}.Bytes()
	}
	if err := tr.Prove(origin, 0, proof); err != nil {
		log.Debug("Failed to prove range", "kind", kind, "origin", origin, "err", err) // 範囲を証明できませんでした
		return &proofResult{
			keys:     keys,
			vals:     vals,
			diskMore: diskMore,
			proofErr: err,
			tr:       tr,
		}, nil
	}
	if last != nil {
		if err := tr.Prove(last, 0, proof); err != nil {
			log.Debug("Failed to prove range", "kind", kind, "last", last, "err", err) // 範囲を証明できませんでした
			return &proofResult{
				keys:     keys,
				vals:     vals,
				diskMore: diskMore,
				proofErr: err,
				tr:       tr,
			}, nil
		}
	}
	// Verify the snapshot segment with range prover, ensure that all flat states
	// in this range correspond to merkle trie.
	// 範囲証明者を使用してスナップショットセグメントを確認し、
	// この範囲のすべてのフラット状態がマークルトライに対応していることを確認します。
	cont, err := trie.VerifyRangeProof(root, origin, last, keys, vals, proof)
	return &proofResult{
			keys:     keys,
			vals:     vals,
			diskMore: diskMore,
			trieMore: cont,
			proofErr: err,
			tr:       tr},
		nil
}

// onStateCallback is a function that is called by generateRange, when processing a range of
// accounts or storage slots. For each element, the callback is invoked.
// If 'delete' is true, then this element (and potential slots) needs to be deleted from the snapshot.
// If 'write' is true, then this element needs to be updated with the 'val'.
// If 'write' is false, then this element is already correct, and needs no update. However,
// for accounts, the storage trie of the account needs to be checked.
// The 'val' is the canonical encoding of the value (not the slim format for accounts)
// onStateCallbackは、アカウントまたはストレージスロットの範囲を処理するときにgenerateRangeによって呼び出される関数です。
// 要素ごとに、コールバックが呼び出されます。
// 'delete'がtrueの場合、この要素（および潜在的なスロット）をスナップショットから削除する必要があります。
// 'write'がtrueの場合、この要素は'val'で更新する必要があります。
// 'write'がfalseの場合、この要素はすでに正しく、更新する必要はありません。
// ただし、アカウントの場合、アカウントのストレージトライを確認する必要があります。
// 'val'は値の正規エンコーディングです（アカウントのスリムフォーマットではありません）
type onStateCallback func(key []byte, val []byte, write bool, delete bool) error

// generateRange generates the state segment with particular prefix. Generation can
// either verify the correctness of existing state through rangeproof and skip
// generation, or iterate trie to regenerate state on demand.
// generateRangeは、特定のプレフィックスを持つ状態セグメントを生成します。
// 生成では、範囲プルーフとスキップ生成によって既存の状態の正確さを検証するか、
// 試行を繰り返してオンデマンドで状態を再生成できます。
func (dl *diskLayer) generateRange(root common.Hash, prefix []byte, kind string, origin []byte, max int, stats *generatorStats, onState onStateCallback, valueConvertFn func([]byte) ([]byte, error)) (bool, []byte, error) {
	// Use range prover to check the validity of the flat state in the range
	// 範囲証明者を使用して、範囲内のフラット状態の有効性を確認します
	result, err := dl.proveRange(stats, root, prefix, kind, origin, max, valueConvertFn)
	if err != nil {
		return false, nil, err
	}
	last := result.last()

	// Construct contextual logger
	// コンテキストロガーを構築します
	logCtx := []interface{}{"kind", kind, "prefix", hexutil.Encode(prefix)}
	if len(origin) > 0 {
		logCtx = append(logCtx, "origin", hexutil.Encode(origin))
	}
	logger := log.New(logCtx...)

	// The range prover says the range is correct, skip trie iteration
	// 範囲証明者は、範囲が正しいと言っています。トライの反復をスキップします
	if result.valid() {
		snapSuccessfulRangeProofMeter.Mark(1)
		logger.Trace("Proved state range", "last", hexutil.Encode(last))

		// The verification is passed, process each state with the given
		// callback function. If this state represents a contract, the
		// corresponding storage check will be performed in the callback
		// 検証に合格し、指定されたコールバック関数で各状態を処理します。
		// この状態が契約を表す場合、対応するストレージチェックがコールバックで実行されます
		if err := result.forEach(func(key []byte, val []byte) error { return onState(key, val, false, false) }); err != nil {
			return false, nil, err
		}
		// Only abort the iteration when both database and trie are exhausted
		// データベースとトライの両方が使い果たされた場合にのみ反復を中止します
		return !result.diskMore && !result.trieMore, last, nil
	}
	logger.Trace("Detected outdated state range", "last", hexutil.Encode(last), "err", result.proofErr)
	snapFailedRangeProofMeter.Mark(1)

	// Special case, the entire trie is missing. In the original trie scheme,
	// all the duplicated subtries will be filter out(only one copy of data
	// will be stored). While in the snapshot model, all the storage tries
	// belong to different contracts will be kept even they are duplicated.
	// Track it to a certain extent remove the noise data used for statistics.
	// 特別な場合、トライ全体が欠落しています。元のトライスキームでは、
	// 複製されたすべてのサブトライがフィルターで除外されます（データのコピーが1つだけ保存されます）。
	// スナップショットモデルでは、重複している場合でも、
	// 異なるコントラクトに属するすべてのストレージ試行が保持されます。
	// ある程度追跡し、統計に使用されるノイズデータを削除します。
	if origin == nil && last == nil {
		meter := snapMissallAccountMeter
		if kind == "storage" {
			meter = snapMissallStorageMeter
		}
		meter.Mark(1)
	}

	// We use the snap data to build up a cache which can be used by the
	// main account trie as a primary lookup when resolving hashes
	// スナップデータを使用してキャッシュを構築します。
	// このキャッシュは、メインアカウントのトライがハッシュを解決する際のプライマリルックアップとして使用できます。
	var snapNodeCache ethdb.KeyValueStore
	if len(result.keys) > 0 {
		snapNodeCache = memorydb.New()
		snapTrieDb := trie.NewDatabase(snapNodeCache)
		snapTrie, _ := trie.New(common.Hash{}, snapTrieDb)
		for i, key := range result.keys {
			snapTrie.Update(key, result.vals[i])
		}
		root, _, _ := snapTrie.Commit(nil)
		snapTrieDb.Commit(root, false, nil)
	}
	tr := result.tr
	if tr == nil {
		tr, err = trie.New(root, dl.triedb)
		if err != nil {
			stats.Log("Trie missing, state snapshotting paused", dl.root, dl.genMarker) // 試行がありません、状態のスナップショットが一時停止しました
			return false, nil, errMissingTrie
		}
	}

	var (
		trieMore       bool
		nodeIt         = tr.NodeIterator(origin)
		iter           = trie.NewIterator(nodeIt)
		kvkeys, kvvals = result.keys, result.vals

		// counters
		count     = 0 // イテレータによって配信された状態の数 // number of states delivered by iterator
		created   = 0 // トライから作成された状態            // states created from the trie
		updated   = 0 // トライから更新された状態            // states updated from the trie
		deleted   = 0 // 状態はトライではありませんが、スナップショットにありました // states not in trie, but were in snapshot
		untouched = 0 // すでに正しい状態                   // states already correct

		// timers
		start    = time.Now()
		internal time.Duration
	)
	nodeIt.AddResolver(snapNodeCache)
	for iter.Next() {
		if last != nil && bytes.Compare(iter.Key, last) > 0 {
			trieMore = true
			break
		}
		count++
		write := true
		created++
		for len(kvkeys) > 0 {
			if cmp := bytes.Compare(kvkeys[0], iter.Key); cmp < 0 {
				// delete the key
				// キーを削除します
				istart := time.Now()
				if err := onState(kvkeys[0], nil, false, true); err != nil {
					return false, nil, err
				}
				kvkeys = kvkeys[1:]
				kvvals = kvvals[1:]
				deleted++
				internal += time.Since(istart)
				continue
			} else if cmp == 0 {
				// the snapshot key can be overwritten
				// スナップショットキーは上書きできます
				created--
				if write = !bytes.Equal(kvvals[0], iter.Value); write {
					updated++
				} else {
					untouched++
				}
				kvkeys = kvkeys[1:]
				kvvals = kvvals[1:]
			}
			break
		}
		istart := time.Now()
		if err := onState(iter.Key, iter.Value, write, false); err != nil {
			return false, nil, err
		}
		internal += time.Since(istart)
	}
	if iter.Err != nil {
		return false, nil, iter.Err
	}
	// Delete all stale snapshot states remaining
	// 残りの古いスナップショット状態をすべて削除します
	istart := time.Now()
	for _, key := range kvkeys {
		if err := onState(key, nil, false, true); err != nil {
			return false, nil, err
		}
		deleted += 1
	}
	internal += time.Since(istart)

	// Update metrics for counting trie iteration
	// トライの反復をカウントするためのメトリックを更新します
	if kind == "storage" {
		snapStorageTrieReadCounter.Inc((time.Since(start) - internal).Nanoseconds())
	} else {
		snapAccountTrieReadCounter.Inc((time.Since(start) - internal).Nanoseconds())
	}
	logger.Debug("Regenerated state range", "root", root, "last", hexutil.Encode(last), // 再生状態範囲
		"count", count, "created", created, "updated", updated, "untouched", untouched, "deleted", deleted)

	// If there are either more trie items, or there are more snap items
	// (in the next segment), then we need to keep working
	// トライアイテムが多いか、スナップアイテムが（次のセグメントに）ある場合は、作業を続ける必要があります
	return !trieMore && !result.diskMore, last, nil
}

// generate is a background thread that iterates over the state and storage tries,
// constructing the state snapshot. All the arguments are purely for statistics
// gathering and logging, since the method surfs the blocks as they arrive, often
// being restarted.
// generateは、状態とストレージの試行を繰り返し、状態のスナップショットを作成するバックグラウンドスレッドです。
// メソッドはブロックが到着したときにブロックをサーフし、多くの場合再起動されるため、
// すべての引数は純粋に統計の収集とロギングのためのものです。
func (dl *diskLayer) generate(stats *generatorStats) {
	var (
		accMarker    []byte
		accountRange = accountCheckRange
	)
	if len(dl.genMarker) > 0 { // []byte{} is the start, use nil for that
		// Always reset the initial account range as 1
		// whenever recover from the interruption.
		// [] byte {}が開始です。
		// そのためにnilを使用します。
		// 中断から回復するときは常に、初期アカウント範囲を常に1にリセットします。
		accMarker, accountRange = dl.genMarker[:common.HashLength], 1
	}
	var (
		batch     = dl.diskdb.NewBatch()
		logged    = time.Now()
		accOrigin = common.CopyBytes(accMarker)
		abort     chan *generatorStats
	)
	stats.Log("Resuming state snapshot generation", dl.root, dl.genMarker) // 状態スナップショットの生成を再開します

	checkAndFlush := func(currentLocation []byte) error {
		select {
		case abort = <-dl.genAbort:
		default:
		}
		if batch.ValueSize() > ethdb.IdealBatchSize || abort != nil {
			if bytes.Compare(currentLocation, dl.genMarker) < 0 {
				log.Error("Snapshot generator went backwards", // スナップショットジェネレータが逆方向に移動しました
					"currentLocation", fmt.Sprintf("%x", currentLocation),
					"genMarker", fmt.Sprintf("%x", dl.genMarker))
			}

			// Flush out the batch anyway no matter it's empty or not.
			// It's possible that all the states are recovered and the
			// generation indeed makes progress.
			// 空であるかどうかに関係なく、とにかくバッチをフラッシュします。
			// すべての状態が回復し、生成が実際に進行する可能性があります。
			journalProgress(batch, currentLocation, stats)

			if err := batch.Write(); err != nil {
				return err
			}
			batch.Reset()

			dl.lock.Lock()
			dl.genMarker = currentLocation
			dl.lock.Unlock()

			if abort != nil {
				stats.Log("Aborting state snapshot generation", dl.root, currentLocation) // 状態スナップショット生成の中止
				return errors.New("aborted")
			}
		}
		if time.Since(logged) > 8*time.Second {
			stats.Log("Generating state snapshot", dl.root, currentLocation) // 状態スナップショットの生成
			logged = time.Now()
		}
		return nil
	}

	onAccount := func(key []byte, val []byte, write bool, delete bool) error {
		var (
			start       = time.Now()
			accountHash = common.BytesToHash(key)
		)
		if delete {
			rawdb.DeleteAccountSnapshot(batch, accountHash)
			snapWipedAccountMeter.Mark(1)

			// Ensure that any previous snapshot storage values are cleared
			// 以前のスナップショットストレージ値がすべてクリアされていることを確認します
			prefix := append(rawdb.SnapshotStoragePrefix, accountHash.Bytes()...)
			keyLen := len(rawdb.SnapshotStoragePrefix) + 2*common.HashLength
			if err := wipeKeyRange(dl.diskdb, "storage", prefix, nil, nil, keyLen, snapWipedStorageMeter, false); err != nil {
				return err
			}
			snapAccountWriteCounter.Inc(time.Since(start).Nanoseconds())
			return nil
		}
		// Retrieve the current account and flatten it into the internal format
		// 現在のアカウントを取得し、内部形式にフラット化します
		var acc struct {
			Nonce           uint64
			Balance         *big.Int
			LastBlockNumber *big.Int
			Root            common.Hash
			CodeHash        []byte
		}
		if err := rlp.DecodeBytes(val, &acc); err != nil {
			log.Crit("Invalid account encountered during snapshot creation", "err", err) // スナップショットの作成中に無効なアカウントが見つかりました
		}
		// If the account is not yet in-progress, write it out
		// アカウントがまだ進行中でない場合は、書き留めます
		if accMarker == nil || !bytes.Equal(accountHash[:], accMarker) {
			dataLen := len(val) // おおよそのサイズで、RLPエンコーディングのラウンドを節約できます // Approximate size, saves us a round of RLP-encoding
			if !write {
				if bytes.Equal(acc.CodeHash, emptyCode[:]) {
					dataLen -= 32
				}
				if acc.Root == emptyRoot {
					dataLen -= 32
				}
				snapRecoveredAccountMeter.Mark(1)
			} else {
				data := SlimAccountRLP(acc.Nonce, acc.Balance, acc.LastBlockNumber, acc.Root, acc.CodeHash)
				dataLen = len(data)
				rawdb.WriteAccountSnapshot(batch, accountHash, data)
				snapGeneratedAccountMeter.Mark(1)
			}
			stats.storage += common.StorageSize(1 + common.HashLength + dataLen)
			stats.accounts++
		}
		marker := accountHash[:]
		// If the snap generation goes here after interrupted, genMarker may go backward
		// when last genMarker is consisted of accountHash and storageHash
		// スナップ生成が中断された後にここに移動した場合、
		// 最後のgenMarkerがaccountHashとstorageHashで構成されていると、
		// genMarkerが逆方向に戻る可能性があります
		if accMarker != nil && bytes.Equal(marker, accMarker) && len(dl.genMarker) > common.HashLength {
			marker = dl.genMarker[:]
		}
		// If we've exceeded our batch allowance or termination was requested, flush to disk
		// バッチ許容量を超えた場合、または終了が要求された場合は、ディスクにフラッシュします
		if err := checkAndFlush(marker); err != nil {
			return err
		}
		// If the iterated account is the contract, create a further loop to
		// verify or regenerate the contract storage.
		// 反復アカウントがコントラクトである場合は、コントラクトストレージを検証または再生成するためのループをさらに作成します。
		if acc.Root == emptyRoot {
			// If the root is empty, we still need to ensure that any previous snapshot
			// storage values are cleared
			// TODO: investigate if this can be avoided, this will be very costly since it
			// affects every single EOA account
			//  - Perhaps we can avoid if where codeHash is emptyCode
			// ルートが空の場合でも、以前のスナップショットストレージ値がクリアされていることを確認する必要があります
			// TODO：これを回避できるかどうかを調査します。
			// これは、すべてのEOAアカウントに影響するため、非常にコストがかかります。
			// -おそらく、codeHashがemptyCodeの場合は回避できます
			prefix := append(rawdb.SnapshotStoragePrefix, accountHash.Bytes()...)
			keyLen := len(rawdb.SnapshotStoragePrefix) + 2*common.HashLength
			if err := wipeKeyRange(dl.diskdb, "storage", prefix, nil, nil, keyLen, snapWipedStorageMeter, false); err != nil {
				return err
			}
			snapAccountWriteCounter.Inc(time.Since(start).Nanoseconds())
		} else {
			snapAccountWriteCounter.Inc(time.Since(start).Nanoseconds())

			var storeMarker []byte
			if accMarker != nil && bytes.Equal(accountHash[:], accMarker) && len(dl.genMarker) > common.HashLength {
				storeMarker = dl.genMarker[common.HashLength:]
			}
			onStorage := func(key []byte, val []byte, write bool, delete bool) error {
				defer func(start time.Time) {
					snapStorageWriteCounter.Inc(time.Since(start).Nanoseconds())
				}(time.Now())

				if delete {
					rawdb.DeleteStorageSnapshot(batch, accountHash, common.BytesToHash(key))
					snapWipedStorageMeter.Mark(1)
					return nil
				}
				if write {
					rawdb.WriteStorageSnapshot(batch, accountHash, common.BytesToHash(key), val)
					snapGeneratedStorageMeter.Mark(1)
				} else {
					snapRecoveredStorageMeter.Mark(1)
				}
				stats.storage += common.StorageSize(1 + 2*common.HashLength + len(val))
				stats.slots++

				// If we've exceeded our batch allowance or termination was requested, flush to disk
				// バッチ許容量を超えた場合、または終了が要求された場合は、ディスクにフラッシュします
				if err := checkAndFlush(append(accountHash[:], key...)); err != nil {
					return err
				}
				return nil
			}
			var storeOrigin = common.CopyBytes(storeMarker)
			for {
				exhausted, last, err := dl.generateRange(acc.Root, append(rawdb.SnapshotStoragePrefix, accountHash.Bytes()...), "storage", storeOrigin, storageCheckRange, stats, onStorage, nil)
				if err != nil {
					return err
				}
				if exhausted {
					break
				}
				if storeOrigin = increaseKey(last); storeOrigin == nil {
					break // 特殊なケース、最後は0xffffffff ... fff // special case, the last is 0xffffffff...fff
				}
			}
		}
		// Some account processed, unmark the marker
		// 一部のアカウントが処理され、マーカーのマークが解除されます
		accMarker = nil
		return nil
	}

	// Global loop for regerating the entire state trie + all layered storage tries.
	// 状態トライ全体+すべての階層化ストレージ試行を再生成するためのグローバルループ。
	for {
		exhausted, last, err := dl.generateRange(dl.root, rawdb.SnapshotAccountPrefix, "account", accOrigin, accountRange, stats, onAccount, FullAccountRLP)
		// The procedure it aborted, either by external signal or internal error
		// 外部シグナルまたは内部エラーのいずれかによって中止されたプロシージャ
		if err != nil {
			if abort == nil { // 内部エラーにより中止されました。信号を待ちます // aborted by internal error, wait the signal
				abort = <-dl.genAbort
			}
			abort <- stats
			return
		}
		// Abort the procedure if the entire snapshot is generated
		// スナップショット全体が生成された場合は、手順を中止します
		if exhausted {
			break
		}
		if accOrigin = increaseKey(last); accOrigin == nil {
			break // 特殊なケース、最後は0xffffffff ... fff // special case, the last is 0xffffffff...fff
		}
		accountRange = accountCheckRange
	}
	// Snapshot fully generated, set the marker to nil.
	// Note even there is nothing to commit, persist the
	// generator anyway to mark the snapshot is complete.
	// スナップショットが完全に生成され、マーカーをnilに設定します。
	//コミットするものがない場合でも、スナップショットが完了したことを示すためにジェネレーターを永続化することに注意してください。
	journalProgress(batch, nil, stats)
	if err := batch.Write(); err != nil {
		log.Error("Failed to flush batch", "err", err) // バッチのフラッシュに失敗しました

		abort = <-dl.genAbort
		abort <- stats
		return
	}
	batch.Reset()

	log.Info("Generated state snapshot", "accounts", stats.accounts, "slots", stats.slots, // 生成された状態のスナップショット
		"storage", stats.storage, "elapsed", common.PrettyDuration(time.Since(stats.start)))

	dl.lock.Lock()
	dl.genMarker = nil
	close(dl.genPending)
	dl.lock.Unlock()

	// Someone will be looking for us, wait it out
	// 誰かが私たちを探しているので待ってください
	abort = <-dl.genAbort
	abort <- nil
}

// increaseKey increase the input key by one bit. Return nil if the entire
// addition operation overflows,
// increaseKey入力キーを1ビット増やします。加算演算全体がオーバーフローした場合はnilを返します。
func increaseKey(key []byte) []byte {
	for i := len(key) - 1; i >= 0; i-- {
		key[i]++
		if key[i] != 0x0 {
			return key
		}
	}
	return nil
}
