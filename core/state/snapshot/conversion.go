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

package snapshot

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

// trieKV represents a trie key-value pair
// trieKVは、trieのキーと値のペアを表します
type trieKV struct {
	key   common.Hash
	value []byte
}

type (
	// trieGeneratorFn is the interface of trie generation which can
	// be implemented by different trie algorithm.
	// trieGeneratorFnは、さまざまなトライアルゴリズムで実装できるトライ生成のインターフェイスです。
	trieGeneratorFn func(db ethdb.KeyValueWriter, in chan (trieKV), out chan (common.Hash))

	// leafCallbackFn is the callback invoked at the leaves of the trie,
	// returns the subtrie root with the specified subtrie identifier.
	// leafCallbackFnは、トライのリーフで呼び出されるコールバックであり、
	// 指定されたサブトライ識別子を持つサブトライルートを返します。
	leafCallbackFn func(db ethdb.KeyValueWriter, accountHash, codeHash common.Hash, stat *generateStats) (common.Hash, error)
)

// GenerateAccountTrieRoot takes an account iterator and reproduces the root hash.
// GenerateAccountTrieRootはアカウントイテレータを取得し、ルートハッシュを再現します。
func GenerateAccountTrieRoot(it AccountIterator) (common.Hash, error) {
	return generateTrieRoot(nil, it, common.Hash{}, stackTrieGenerate, nil, newGenerateStats(), true)
}

// GenerateStorageTrieRoot takes a storage iterator and reproduces the root hash.
// GenerateStorageTrieRootは、ストレージイテレータを取得し、ルートハッシュを再現します。
func GenerateStorageTrieRoot(account common.Hash, it StorageIterator) (common.Hash, error) {
	return generateTrieRoot(nil, it, account, stackTrieGenerate, nil, newGenerateStats(), true)
}

// GenerateTrie takes the whole snapshot tree as the input, traverses all the
// accounts as well as the corresponding storages and regenerate the whole state
// (account trie + all storage tries).
// GenerateTrieは、スナップショットツリー全体を入力として受け取り、
// すべてのアカウントと対応するストレージをトラバースし、状態全体を再生成します
// （アカウントトライ+すべてのストレージ試行）。
func GenerateTrie(snaptree *Tree, root common.Hash, src ethdb.Database, dst ethdb.KeyValueWriter) error {
	// Traverse all state by snapshot, re-generate the whole state trie
	// スナップショットですべての状態をトラバースし、状態トライ全体を再生成します
	acctIt, err := snaptree.AccountIterator(root, common.Hash{})
	if err != nil {
		return err // 必要なスナップショットが存在しない可能性があります。// The required snapshot might not exist.
	}
	defer acctIt.Release()

	got, err := generateTrieRoot(dst, acctIt, common.Hash{}, stackTrieGenerate, func(dst ethdb.KeyValueWriter, accountHash, codeHash common.Hash, stat *generateStats) (common.Hash, error) {
		// Migrate the code first, commit the contract code into the tmp db.
		// 最初にコードを移行し、コントラクトコードをtmpdbにコミットします。
		if codeHash != emptyCode {
			code := rawdb.ReadCode(src, codeHash)
			if len(code) == 0 {
				return common.Hash{}, errors.New("failed to read contract code") // 契約コードの読み取りに失敗しました
			}
			rawdb.WriteCode(dst, codeHash, code)
		}
		// Then migrate all storage trie nodes into the tmp db.
		// 次に、すべてのストレージトライノードをtmpデータベースに移行します。
		storageIt, err := snaptree.StorageIterator(root, accountHash, common.Hash{})
		if err != nil {
			return common.Hash{}, err
		}
		defer storageIt.Release()

		hash, err := generateTrieRoot(dst, storageIt, accountHash, stackTrieGenerate, nil, stat, false)
		if err != nil {
			return common.Hash{}, err
		}
		return hash, nil
	}, newGenerateStats(), true)

	if err != nil {
		return err
	}
	if got != root {
		return fmt.Errorf("state root hash mismatch: got %x, want %x", got, root)
	}
	return nil
}

// generateStats is a collection of statistics gathered by the trie generator
// for logging purposes.
// generateStatsは、ロギングの目的でトライジェネレーターによって収集された統計のコレクションです。
type generateStats struct {
	head  common.Hash
	start time.Time

	accounts uint64 // 実行されたアカウントの数（クロールされているアカウントを含む） // Number of accounts done (including those being crawled)
	slots    uint64 // 実行されたストレージスロットの数（クロールされているものを含む // Number of storage slots done (including those being crawled)

	slotsStart map[common.Hash]time.Time   // アカウントスロットクロールの開始時間     // Start time for account slot crawling
	slotsHead  map[common.Hash]common.Hash // クロールされるアカウントのスロットヘッド // Slot head for accounts being crawled

	lock sync.RWMutex
}

// newGenerateStats creates a new generator stats.
// newGenerateStatsは、新しいジェネレーター統計を作成します。
func newGenerateStats() *generateStats {
	return &generateStats{
		slotsStart: make(map[common.Hash]time.Time),
		slotsHead:  make(map[common.Hash]common.Hash),
		start:      time.Now(),
	}
}

// progressAccounts updates the generator stats for the account range.
// progressAccountsは、アカウント範囲のジェネレーター統計を更新します。
func (stat *generateStats) progressAccounts(account common.Hash, done uint64) {
	stat.lock.Lock()
	defer stat.lock.Unlock()

	stat.accounts += done
	stat.head = account
}

// finishAccounts updates the gemerator stats for the finished account range.
// finishAccountsは、完成したアカウント範囲のgemerator統計を更新します。
func (stat *generateStats) finishAccounts(done uint64) {
	stat.lock.Lock()
	defer stat.lock.Unlock()

	stat.accounts += done
}

// progressContract updates the generator stats for a specific in-progress contract.
// progressContractは、特定の進行中のコントラクトのジェネレーター統計を更新します。
func (stat *generateStats) progressContract(account common.Hash, slot common.Hash, done uint64) {
	stat.lock.Lock()
	defer stat.lock.Unlock()

	stat.slots += done
	stat.slotsHead[account] = slot
	if _, ok := stat.slotsStart[account]; !ok {
		stat.slotsStart[account] = time.Now()
	}
}

// finishContract updates the generator stats for a specific just-finished contract.
// finishContractは、特定の完了したばかりのコントラクトのジェネレーター統計を更新します。
func (stat *generateStats) finishContract(account common.Hash, done uint64) {
	stat.lock.Lock()
	defer stat.lock.Unlock()

	stat.slots += done
	delete(stat.slotsHead, account)
	delete(stat.slotsStart, account)
}

// report prints the cumulative progress statistic smartly.
// レポートは、累積進捗統計をスマートに出力します。
func (stat *generateStats) report() {
	stat.lock.RLock()
	defer stat.lock.RUnlock()

	ctx := []interface{}{
		"accounts", stat.accounts,
		"slots", stat.slots,
		"elapsed", common.PrettyDuration(time.Since(stat.start)),
	}
	if stat.accounts > 0 {
		// If there's progress on the account trie, estimate the time to finish crawling it
		// アカウントトライに進捗がある場合は、クロールが完了するまでの時間を見積もります
		if done := binary.BigEndian.Uint64(stat.head[:8]) / stat.accounts; done > 0 {
			var (
				left  = (math.MaxUint64 - binary.BigEndian.Uint64(stat.head[:8])) / stat.accounts
				speed = done/uint64(time.Since(stat.start)/time.Millisecond+1) + 1 // ゼロによる除算を回避するための+1 // +1s to avoid division by zero
				eta   = time.Duration(left/speed) * time.Millisecond
			)
			// If there are large contract crawls in progress, estimate their finish time
			// 進行中の大規模な契約クロールがある場合は、それらの終了時間を見積もります
			for acc, head := range stat.slotsHead {
				start := stat.slotsStart[acc]
				if done := binary.BigEndian.Uint64(head[:8]); done > 0 {
					var (
						left  = math.MaxUint64 - binary.BigEndian.Uint64(head[:8])
						speed = done/uint64(time.Since(start)/time.Millisecond+1) + 1 // +1s to avoid division by zero
					)
					// Override the ETA if larger than the largest until now
					// これまでの最大値よりも大きい場合は、ETAをオーバーライドします
					if slotETA := time.Duration(left/speed) * time.Millisecond; eta < slotETA {
						eta = slotETA
					}
				}
			}
			ctx = append(ctx, []interface{}{
				"eta", common.PrettyDuration(eta),
			}...)
		}
	}
	log.Info("Iterating state snapshot", ctx...) // 状態スナップショットの反復
}

// reportDone prints the last log when the whole generation is finished.
// reportDoneは、生成全体が終了したときに最後のログを出力します。
func (stat *generateStats) reportDone() {
	stat.lock.RLock()
	defer stat.lock.RUnlock()

	var ctx []interface{}
	ctx = append(ctx, []interface{}{"accounts", stat.accounts}...)
	if stat.slots != 0 {
		ctx = append(ctx, []interface{}{"slots", stat.slots}...)
	}
	ctx = append(ctx, []interface{}{"elapsed", common.PrettyDuration(time.Since(stat.start))}...)
	log.Info("Iterated snapshot", ctx...)
}

// runReport periodically prints the progress information.
// runReportは、進行状況情報を定期的に出力します。
func runReport(stats *generateStats, stop chan bool) {
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			stats.report()
			timer.Reset(time.Second * 8)
		case success := <-stop:
			if success {
				stats.reportDone()
			}
			return
		}
	}
}

// generateTrieRoot generates the trie hash based on the snapshot iterator.
// It can be used for generating account trie, storage trie or even the
// whole state which connects the accounts and the corresponding storages.
// generateTrieRootは、スナップショットイテレータに基づいてトライハッシュを生成します。
// アカウントトライ、ストレージトライ、またはアカウントと対応するストレージを接続する状態全体を生成するために使用できます。
func generateTrieRoot(db ethdb.KeyValueWriter, it Iterator, account common.Hash, generatorFn trieGeneratorFn, leafCallback leafCallbackFn, stats *generateStats, report bool) (common.Hash, error) {
	var (
		in      = make(chan trieKV)         // chan to pass leaves
		out     = make(chan common.Hash, 1) // chan to collect result
		stoplog = make(chan bool, 1)        // 1サイズのバッファ、ロギングが有効になっていない場合に機能します // 1-size buffer, works when logging is not enabled
		wg      sync.WaitGroup
	)
	// Spin up a go-routine for trie hash re-generation
	// トライハッシュ再生成のためのゴールーチンをスピンアップします
	wg.Add(1)
	go func() {
		defer wg.Done()
		generatorFn(db, in, out)
	}()
	// Spin up a go-routine for progress logging
	// 進行状況ログの実行ルーチンを起動します
	if report && stats != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runReport(stats, stoplog)
		}()
	}
	// Create a semaphore to assign tasks and collect results through. We'll pre-
	// fill it with nils, thus using the same channel for both limiting concurrent
	// processing and gathering results.
	// タスクを割り当て、結果を収集するためのセマフォを作成します。
	// 事前にnilsを入力するため、同時処理の制限と結果の収集の両方に同じチャネルを使用します。
	threads := runtime.NumCPU()
	results := make(chan error, threads)
	for i := 0; i < threads; i++ {
		results <- nil // セマフォを埋めます // fill the semaphore
	}
	// stop is a helper function to shutdown the background threads
	// and return the re-generated trie hash.
	// stopは、バックグラウンドスレッドをシャットダウンし、再生成されたtrieハッシュを返すヘルパー関数です。
	stop := func(fail error) (common.Hash, error) {
		close(in)
		result := <-out
		for i := 0; i < threads; i++ {
			if err := <-results; err != nil && fail == nil {
				fail = err
			}
		}
		stoplog <- fail == nil

		wg.Wait()
		return result, fail
	}
	var (
		logged    = time.Now()
		processed = uint64(0)
		leaf      trieKV
	)
	// Start to feed leaves
	// 葉の供給を開始します
	for it.Next() {
		if account == (common.Hash{}) {
			var (
				err      error
				fullData []byte
			)
			if leafCallback == nil {
				fullData, err = FullAccountRLP(it.(AccountIterator).Account())
				if err != nil {
					return stop(err)
				}
			} else {
				// Wait until the semaphore allows us to continue, aborting if
				// a sub-task failed
				// セマフォが続行を許可するまで待機し、サブタスクが失敗した場合は中止します
				if err := <-results; err != nil {
					results <- nil // 停止すると結果が排出され、消費したばかりのこのエラーのnoopが追加されます // stop will drain the results, add a noop back for this error we just consumed
					return stop(err)
				}
				// Fetch the next account and process it concurrently
				// 次のアカウントを取得し、同時に処理します
				account, err := FullAccount(it.(AccountIterator).Account())
				if err != nil {
					return stop(err)
				}
				go func(hash common.Hash) {
					subroot, err := leafCallback(db, hash, common.BytesToHash(account.CodeHash), stats)
					if err != nil {
						results <- err
						return
					}
					if !bytes.Equal(account.Root, subroot.Bytes()) {
						results <- fmt.Errorf("invalid subroot(path %x), want %x, have %x", hash, account.Root, subroot) // 無効なサブルート（パス％x）、％xが必要、％xがあります
						return
					}
					results <- nil
				}(it.Hash())
				fullData, err = rlp.EncodeToBytes(account)
				if err != nil {
					return stop(err)
				}
			}
			leaf = trieKV{it.Hash(), fullData}
		} else {
			leaf = trieKV{it.Hash(), common.CopyBytes(it.(StorageIterator).Slot())}
		}
		in <- leaf

		// Accumulate the generation statistic if it's required.
		// 必要に応じて、生成統計を累積します。
		processed++
		if time.Since(logged) > 3*time.Second && stats != nil {
			if account == (common.Hash{}) {
				stats.progressAccounts(it.Hash(), processed)
			} else {
				stats.progressContract(account, it.Hash(), processed)
			}
			logged, processed = time.Now(), 0
		}
	}
	// Commit the last part statistic.
	// 最後の部分の統計をコミットします。
	if processed > 0 && stats != nil {
		if account == (common.Hash{}) {
			stats.finishAccounts(processed)
		} else {
			stats.finishContract(account, processed)
		}
	}
	return stop(nil)
}

func stackTrieGenerate(db ethdb.KeyValueWriter, in chan trieKV, out chan common.Hash) {
	t := trie.NewStackTrie(db)
	for leaf := range in {
		t.TryUpdate(leaf.key[:], leaf.value)
	}
	var root common.Hash
	if db == nil {
		root = t.Hash()
	} else {
		root, _ = t.Commit()
	}
	out <- root
}
