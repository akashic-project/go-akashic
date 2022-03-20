// Copyright 2017 The go-ethereum Authors
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

package ethash

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"math/rand"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/types"
)

const (
	// staleThreshold is the maximum depth of the acceptable stale but valid ethash solution.
	// staleThresholdは、許容できる古いが有効なethashソリューションの最大深度です。
	staleThreshold = 7
)

var (
	errNoMiningWork      = errors.New("no mining work available yet")
	errInvalidSealResult = errors.New("invalid or stale proof-of-work solution")
)

// Seal implements consensus.Engine, attempting to find a nonce that satisfies
// the block's difficulty requirements.
// シールはconsensus.Engineを実装し、ブロックの難易度要件を満たすナンスを見つけようとします。
func (ethash *Ethash) Seal(chain consensus.ChainHeaderReader, block *types.Block, results chan<- *types.Block, stop <-chan struct{}) error {
	// If we're running a fake PoW, simply return a 0 nonce immediately
	// 偽のPoWを実行している場合は、すぐに0ナンスを返します
	if ethash.config.PowMode == ModeFake || ethash.config.PowMode == ModeFullFake {
		header := block.Header()
		header.Nonce, header.MixDigest = types.BlockNonce{}, common.Hash{}
		select {
		case results <- block.WithSeal(header):
		default:
			ethash.config.Log.Warn("Sealing result is not read by miner", "mode", "fake", "sealhash", ethash.SealHash(block.Header()))
		}
		return nil
	}
	// If we're running a shared PoW, delegate sealing to it
	// 共有PoWを実行している場合は、シーリングを委任します
	if ethash.shared != nil {
		return ethash.shared.Seal(chain, block, results, stop)
	}
	// Create a runner and the multiple search threads it directs
	// ランナーとそれが指示する複数の検索スレッドを作成します
	abort := make(chan struct{})

	ethash.lock.Lock()
	threads := ethash.threads
	if ethash.rand == nil {
		seed, err := crand.Int(crand.Reader, big.NewInt(math.MaxInt64))
		if err != nil {
			ethash.lock.Unlock()
			return err
		}
		ethash.rand = rand.New(rand.NewSource(seed.Int64()))
	}
	ethash.lock.Unlock()
	if threads == 0 {
		threads = runtime.NumCPU()
	}
	if threads < 0 {
		threads = 0 //ローカル/リモート周辺の追加ロジックなしでローカルマイニングを無効にできます // Allows disabling local mining without extra logic around local/remote
	}
	// Push new work to remote sealer
	// 新しい作業をリモートシーラーにプッシュします
	if ethash.remote != nil {
		ethash.remote.workCh <- &sealTask{block: block, results: results}
	}
	var (
		pend   sync.WaitGroup
		locals = make(chan *types.Block)
	)
	for i := 0; i < threads; i++ {
		pend.Add(1)
		go func(id int, nonce uint64) {
			defer pend.Done()
			ethash.mine(block, id, nonce, abort, locals)
		}(i, uint64(ethash.rand.Int63()))
	}
	// Wait until sealing is terminated or a nonce is found
	// シーリングが終了するか、ナンスが見つかるまで待ちます
	go func() {
		var result *types.Block
		select {
		case <-stop:
			// Outside abort, stop all miner threads
			// 中止の外で、すべてのマイナースレッドを停止します
			close(abort)
		case result = <-locals:
			// One of the threads found a block, abort all others
			// スレッドの1つがブロックを見つけ、他のすべてを中止します
			select {
			case results <- result:
			default:
				ethash.config.Log.Warn("Sealing result is not read by miner", "mode", "local", "sealhash", ethash.SealHash(block.Header()))
			}
			close(abort)
		case <-ethash.update:
			// Thread count was changed on user request, restart
			// スレッド数はユーザーの要求に応じて変更され、再起動します
			close(abort)
			if err := ethash.Seal(chain, block, results, stop); err != nil {
				ethash.config.Log.Error("Failed to restart sealing after update", "err", err)
			}
		}
		// Wait for all miners to terminate and return the block
		// すべてのマイナーが終了してブロックを返すのを待ちます
		pend.Wait()
	}()
	return nil
}

// mine is the actual proof-of-work miner that searches for a nonce starting from
// seed that results in correct final block difficulty.
// mineは、シードから開始して正しい最終ブロックの難易度をもたらすナンスを検索する実際のプルーフオブワークマイナーです。
func (ethash *Ethash) mine(block *types.Block, id int, seed uint64, abort chan struct{}, found chan *types.Block) {
	// Extract some data from the header
	// ヘッダーからデータを抽出します
	var (
		header  = block.Header()
		hash    = ethash.SealHash(header).Bytes()
		number  = header.Number.Uint64()
		dataset = ethash.dataset(number, false)
	)
	// Start generating random nonces until we abort or find a good one
	// 中止するか、適切なナンスが見つかるまで、ランダムなナンスの生成を開始します
	var (
		attempts = int64(0)
		nonce    = seed
	)
	logger := ethash.config.Log.New("miner", id)
	logger.Trace("Started ethash search for new nonces", "seed", seed)
search:
	for {
		select {
		case <-abort:
			// Mining terminated, update stats and abort
			// マイニングが終了し、統計を更新して中止します
			logger.Trace("Ethash nonce search aborted", "attempts", nonce-seed)
			ethash.hashrate.Mark(attempts)
			break search

		default:
			// システムタイマ取得してブロックのタイムスタンプを超えたときにブロックを作成する。
			// タイマ経過までwaitする。
			NowTime := time.Now().UnixMilli() / 10

			if NowTime < int64(header.Time) {
				time.Sleep(10 * time.Millisecond)
				continue
			}

			// We don't have to update hash rate on every nonce, so update after after 2^X nonces
			// すべてのナンスでハッシュレートを更新する必要がないため、2 ^ Xノンスの後に更新します
			attempts++
			if (attempts % (1 << 15)) == 0 {
				ethash.hashrate.Mark(attempts)
				attempts = 0
			}
			// Compute the PoW value of this nonce
			// このナンスのPoW値を計算します
			digest, _ := hashimotoFull(dataset.dataset, hash, nonce)

			// Correct nonce found, create a new header with it
			// 見つかったナンスを修正し、それを使用して新しいヘッダーを作成します
			header = types.CopyHeader(header)
			header.Nonce = types.EncodeNonce(nonce)
			header.MixDigest = common.BytesToHash(digest)
			// Seal and return a block (if still needed)
			// ブロックを封印して返却します（まだ必要な場合）
			select {
			case found <- block.WithSeal(header):
				logger.Trace("Ethash nonce found and reported", "attempts", nonce-seed, "nonce", nonce) // Ethash nonceが見つかり、報告されました
			case <-abort:
				logger.Trace("Ethash nonce found but discarded", "attempts", nonce-seed, "nonce", nonce) // Ethash nonceが見つかりましたが、破棄されました
			}
			break search
		}
	}
	// Datasets are unmapped in a finalizer. Ensure that the dataset stays live
	// during sealing so it's not unmapped while being read.
	//データセットはファイナライザーでマップ解除されます。
	// 読み取り中にマップが解除されないように、データセットがシーリング中にライブのままであることを確認してください。
	runtime.KeepAlive(dataset)
}

// This is the timeout for HTTP requests to notify external miners.
// これは、外部マイナーに通知するHTTPリクエストのタイムアウトです。
const remoteSealerTimeout = 1 * time.Second

type remoteSealer struct {
	works        map[common.Hash]*types.Block
	rates        map[common.Hash]hashrate
	currentBlock *types.Block
	currentWork  [4]string
	notifyCtx    context.Context
	cancelNotify context.CancelFunc // すべての通知リクエストをキャンセルします // cancels all notification requests
	reqWG        sync.WaitGroup     // 通知リクエストのgoroutinesを追跡します  // tracks notification request goroutines

	ethash       *Ethash
	noverify     bool
	notifyURLs   []string
	results      chan<- *types.Block
	workCh       chan *sealTask   // 新しい作業と相対結果チャネルをリモートシーラーにプッシュする通知チャネル // Notification channel to push new work and relative result channel to remote sealer
	fetchWorkCh  chan *sealWork   // リモートシーラーがマイニング作業をフェッチするために使用されるチャネル   // Channel used for remote sealer to fetch mining work
	submitWorkCh chan *mineResult // リモートシーラーがマイニング結果を送信するために使用されるチャネル       // Channel used for remote sealer to submit their mining result
	fetchRateCh  chan chan uint64 // ローカルまたはリモートシーラーの送信されたハッシュレートを収集するために使用されるチャネル。 // Channel used to gather submitted hash rate for local or remote sealer.
	submitRateCh chan *hashrate   // リモートシーラーがマイニングハッシュレートを送信するために使用されるチャネル // Channel used for remote sealer to submit their mining hashrate
	requestExit  chan struct{}
	exitCh       chan struct{}
}

// sealTask wraps a seal block with relative result channel for remote sealer thread.
// SealTask​​は、リモートシーラースレッドの相対結果チャネルでシールブロックをラップします。
type sealTask struct {
	block   *types.Block
	results chan<- *types.Block
}

// mineResult wraps the pow solution parameters for the specified block.
// mineResultは、指定されたブロックのpowソリューションパラメーターをラップします。
type mineResult struct {
	nonce     types.BlockNonce
	mixDigest common.Hash
	hash      common.Hash

	errc chan error
}

// hashrate wraps the hash rate submitted by the remote sealer.
// hashrateは、リモートシーラーによって送信されたハッシュレートをラップします。
type hashrate struct {
	id   common.Hash
	ping time.Time
	rate uint64

	done chan struct{}
}

// sealWork wraps a seal work package for remote sealer.
// SealWorkは、リモートシーラーのシールワークパッケージをラップします。
type sealWork struct {
	errc chan error
	res  chan [4]string
}

func startRemoteSealer(ethash *Ethash, urls []string, noverify bool) *remoteSealer {
	ctx, cancel := context.WithCancel(context.Background())
	s := &remoteSealer{
		ethash:       ethash,
		noverify:     noverify,
		notifyURLs:   urls,
		notifyCtx:    ctx,
		cancelNotify: cancel,
		works:        make(map[common.Hash]*types.Block),
		rates:        make(map[common.Hash]hashrate),
		workCh:       make(chan *sealTask),
		fetchWorkCh:  make(chan *sealWork),
		submitWorkCh: make(chan *mineResult),
		fetchRateCh:  make(chan chan uint64),
		submitRateCh: make(chan *hashrate),
		requestExit:  make(chan struct{}),
		exitCh:       make(chan struct{}),
	}
	go s.loop()
	return s
}

func (s *remoteSealer) loop() {
	defer func() {
		s.ethash.config.Log.Trace("Ethash remote sealer is exiting") // Ethashリモートシーラーが終了しています
		s.cancelNotify()
		s.reqWG.Wait()
		close(s.exitCh)
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case work := <-s.workCh:
			// Update current work with new received block.
			// Note same work can be past twice, happens when changing CPU threads.
			// 現在の作業を新しい受信ブロックで更新します。
			// 同じ作業が2回過ぎてしまう可能性があることに注意してください。
			// これは、CPUスレッドを変更するときに発生します。
			s.results = work.results
			s.makeWork(work.block)
			s.notifyWork()

		case work := <-s.fetchWorkCh:
			// Return current mining work to remote miner.
			// 現在のマイニング作業をリモートマイナーに返します。
			if s.currentBlock == nil {
				work.errc <- errNoMiningWork
			} else {
				work.res <- s.currentWork
			}

		case result := <-s.submitWorkCh:
			// Verify submitted PoW solution based on maintained mining blocks.
			// 維持されているマイニングブロックに基づいて、送信されたPoWソリューションを確認します。
			if s.submitWork(result.nonce, result.mixDigest, result.hash) {
				result.errc <- nil
			} else {
				result.errc <- errInvalidSealResult
			}

		case result := <-s.submitRateCh:
			// Trace remote sealer's hash rate by submitted value.
			// 送信された値によってリモートシーラーのハッシュレートをトレースします。
			s.rates[result.id] = hashrate{rate: result.rate, ping: time.Now()}
			close(result.done)

		case req := <-s.fetchRateCh:
			// Gather all hash rate submitted by remote sealer.
			// リモートシーラーによって送信されたすべてのハッシュレートを収集します。
			var total uint64
			for _, rate := range s.rates {
				// this could overflow
				// これはオーバーフローする可能性があります
				total += rate.rate
			}
			req <- total

		case <-ticker.C:
			// Clear stale submitted hash rate.
			// 古くなった送信済みハッシュレートをクリアします。
			for id, rate := range s.rates {
				if time.Since(rate.ping) > 10*time.Second {
					delete(s.rates, id)
				}
			}
			// Clear stale pending blocks
			// 古い保留中のブロックをクリアします
			if s.currentBlock != nil {
				for hash, block := range s.works {
					if block.NumberU64()+staleThreshold <= s.currentBlock.NumberU64() {
						delete(s.works, hash)
					}
				}
			}

		case <-s.requestExit:
			return
		}
	}
}

// makeWork creates a work package for external miner.
//
// The work package consists of 3 strings:
//   result[0], 32 bytes hex encoded current block header pow-hash
//   result[1], 32 bytes hex encoded seed hash used for DAG
//   result[2], 32 bytes hex encoded boundary condition ("target"), 2^256/difficulty
//   result[3], hex encoded block number
// makeWorkは、外部マイナー用のワークパッケージを作成します。
//
//ワークパッケージは3つの文字列で構成されています：
// result [0]、32バイトの16進エンコードされた現在のブロックヘッダーpow-hash
// result [1]、DAGに使用される32バイトの16進エンコードシードハッシュ
// result [2]、32バイトの16進エンコード境界条件（「ターゲット」）、2 ^ 256 /難易度
// result [3]、16進数でエンコードされたブロック番号
func (s *remoteSealer) makeWork(block *types.Block) {
	hash := s.ethash.SealHash(block.Header())
	s.currentWork[0] = hash.Hex()
	s.currentWork[1] = common.BytesToHash(SeedHash(block.NumberU64())).Hex()
	s.currentWork[2] = common.BytesToHash(new(big.Int).Div(two256, block.Difficulty()).Bytes()).Hex()
	s.currentWork[3] = hexutil.EncodeBig(block.Number())

	// Trace the seal work fetched by remote sealer.
	// リモートシーラーによってフェッチされたシール作業をトレースします。
	s.currentBlock = block
	s.works[hash] = block
}

// notifyWork notifies all the specified mining endpoints of the availability of
// new work to be processed.
// notifyWorkは、指定されたすべてのマイニングエンドポイントに、処理する新しい作業が利用可能であることを通知します。
func (s *remoteSealer) notifyWork() {
	work := s.currentWork

	// Encode the JSON payload of the notification. When NotifyFull is set,
	// this is the complete block header, otherwise it is a JSON array.
	// 通知のJSONペイロードをエンコードします。
	// NotifyFullが設定されている場合、これは完全なブロックヘッダーです。それ以外の場合は、JSON配列です。
	var blob []byte
	if s.ethash.config.NotifyFull {
		blob, _ = json.Marshal(s.currentBlock.Header())
	} else {
		blob, _ = json.Marshal(work)
	}

	s.reqWG.Add(len(s.notifyURLs))
	for _, url := range s.notifyURLs {
		go s.sendNotification(s.notifyCtx, url, blob, work)
	}
}

func (s *remoteSealer) sendNotification(ctx context.Context, url string, json []byte, work [4]string) {
	defer s.reqWG.Done()

	req, err := http.NewRequest("POST", url, bytes.NewReader(json))
	if err != nil {
		s.ethash.config.Log.Warn("Can't create remote miner notification", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, remoteSealerTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.ethash.config.Log.Warn("Failed to notify remote miner", "err", err)
	} else {
		s.ethash.config.Log.Trace("Notified remote miner", "miner", url, "hash", work[0], "target", work[2])
		resp.Body.Close()
	}
}

// submitWork verifies the submitted pow solution, returning
// whether the solution was accepted or not (not can be both a bad pow as well as
// any other error, like no pending work or stale mining result).
// submitWorkは、送信されたpowソリューションを検証し、ソリューションが受け入れられたかどうかを返します
// （powが悪いだけでなく、保留中の作業がない、マイニング結果が古いなどの他のエラーになることもありません）。
func (s *remoteSealer) submitWork(nonce types.BlockNonce, mixDigest common.Hash, sealhash common.Hash) bool {
	if s.currentBlock == nil {
		s.ethash.config.Log.Error("Pending work without block", "sealhash", sealhash)
		return false
	}
	// Make sure the work submitted is present
	// 提出された作品が存在することを確認します
	block := s.works[sealhash]
	if block == nil {
		s.ethash.config.Log.Warn("Work submitted but none pending", "sealhash", sealhash, "curnumber", s.currentBlock.NumberU64())
		return false
	}
	// Verify the correctness of submitted result.
	// 送信された結果の正確さを確認します。
	header := block.Header()
	header.Nonce = nonce
	header.MixDigest = mixDigest

	start := time.Now()
	if !s.noverify {
		if err := s.ethash.verifySeal(nil, header, true); err != nil {
			s.ethash.config.Log.Warn("Invalid proof-of-work submitted", "sealhash", sealhash, "elapsed", common.PrettyDuration(time.Since(start)), "err", err)
			return false
		}
	}
	// Make sure the result channel is assigned.
	// 結果チャネルが割り当てられていることを確認します
	if s.results == nil {
		s.ethash.config.Log.Warn("Ethash result channel is empty, submitted mining result is rejected")
		return false
	}
	s.ethash.config.Log.Trace("Verified correct proof-of-work", "sealhash", sealhash, "elapsed", common.PrettyDuration(time.Since(start)))

	// Solutions seems to be valid, return to the miner and notify acceptance.
	// ソリューションは有効であるようです。マイナーに戻り、承認を通知します。
	solution := block.WithSeal(header)

	// The submitted solution is within the scope of acceptance.
	// 提出されたソリューションは承認の範囲内です。
	if solution.NumberU64()+staleThreshold > s.currentBlock.NumberU64() {
		select {
		case s.results <- solution:
			s.ethash.config.Log.Debug("Work submitted is acceptable", "number", solution.NumberU64(), "sealhash", sealhash, "hash", solution.Hash())
			return true
		default:
			s.ethash.config.Log.Warn("Sealing result is not read by miner", "mode", "remote", "sealhash", sealhash)
			return false
		}
	}
	// The submitted block is too old to accept, drop it.
	// 送信されたブロックは古すぎて受け入れることができません。削除してください。
	s.ethash.config.Log.Warn("Work submitted is too old", "number", solution.NumberU64(), "sealhash", sealhash, "hash", solution.Hash())
	return false
}
