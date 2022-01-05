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

// Package miner implements Ethereum block creation and mining.
package miner

import (
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// Backend wraps all methods required for mining.
type Backend interface {
	BlockChain() *core.BlockChain
	TxPool() *core.TxPool
}

// Config is the configuration parameters of mining.
// Configは、マイニングの構成パラメーターです。
type Config struct {
	Etherbase  common.Address `toml:",omitempty"` // ブロックマイニング報酬のパブリックアドレス（デフォルト=最初のアカウント） // Public address for block mining rewards (default = first account)
	Notify     []string       `toml:",omitempty"` // 新しいワークパッケージについて通知されるHTTPURLリスト（ethashでのみ役立ちます）。 // HTTP URL list to be notified of new work packages (only useful in ethash).
	NotifyFull bool           `toml:",omitempty"` // 作業パッケージの代わりに保留中のブロックヘッダーで通知します // Notify with pending block headers instead of work packages
	ExtraData  hexutil.Bytes  `toml:",omitempty"` // マイナーによって追加のデータセットをブロックします// Block extra data set by the miner
	GasFloor   uint64         //採掘されたブロックのターゲットガスフロア。 // Target gas floor for mined blocks.
	GasCeil    uint64         //採掘されたブロックのターゲットガス上限。   // Target gas ceiling for mined blocks.
	GasPrice   *big.Int       //トランザクションをマイニングするための最小ガス価格 // Minimum gas price for mining a transaction
	Recommit   time.Duration  //マイナーがマイニング作業を再作成するための時間間隔。 // The time interval for miner to re-create mining work.
	Noverify   bool           //リモートマイニングソリューションの検証を無効にします（ethashでのみ役立ちます）。 // Disable remote mining solution verification(only useful in ethash).
}

// Miner creates blocks and searches for proof-of-work values.
type Miner struct {
	mux      *event.TypeMux
	worker   *worker
	coinbase common.Address
	eth      Backend
	engine   consensus.Engine
	exitCh   chan struct{}
	startCh  chan common.Address
	stopCh   chan struct{}

	wg sync.WaitGroup
}

func New(eth Backend, config *Config, chainConfig *params.ChainConfig, mux *event.TypeMux, engine consensus.Engine, isLocalBlock func(header *types.Header) bool, merger *consensus.Merger) *Miner {
	miner := &Miner{
		eth:     eth,
		mux:     mux,
		engine:  engine,
		exitCh:  make(chan struct{}),
		startCh: make(chan common.Address),
		stopCh:  make(chan struct{}),
		worker:  newWorker(config, chainConfig, engine, eth, mux, isLocalBlock, true, merger),
	}
	miner.wg.Add(1)
	go miner.update()
	return miner
}

// update keeps track of the downloader events. Please be aware that this is a one shot type of update loop.
// It's entered once and as soon as `Done` or `Failed` has been broadcasted the events are unregistered and
// the loop is exited. This to prevent a major security vuln where external parties can DOS you with blocks
// and halt your mining operation for as long as the DOS continues.
// updateはダウンローダーイベントを追跡します。これはワンショットタイプの更新ループであることに注意してください。
// 一度入力すると、 `Done`または` Failed`がブロードキャストされるとすぐに、イベントの登録が解除され、ループが終了します。
// これは、外部の関係者がブロックを使用してDOSを実行し、
// DOSが継続している限り、マイニング操作を停止できるという重大なセキュリティの脆弱性を防ぐためです。
func (miner *Miner) update() {
	defer miner.wg.Done()

	events := miner.mux.Subscribe(downloader.StartEvent{}, downloader.DoneEvent{}, downloader.FailedEvent{})
	defer func() {
		if !events.Closed() {
			events.Unsubscribe()
		}
	}()

	shouldStart := false
	canStart := true
	dlEventCh := events.Chan()
	for {
		select {
		case ev := <-dlEventCh:
			if ev == nil {
				// Unsubscription done, stop listening
				// サブスクリプション解除が完了し、リスニングを停止します
				dlEventCh = nil
				continue
			}
			switch ev.Data.(type) {
			case downloader.StartEvent:
				wasMining := miner.Mining()
				miner.worker.stop()
				canStart = false
				if wasMining {
					// Resume mining after sync was finished
					// 同期が終了した後にマイニングを再開します
					shouldStart = true
					log.Info("Mining aborted due to sync")
				}
			case downloader.FailedEvent:
				canStart = true
				if shouldStart {
					miner.SetEtherbase(miner.coinbase)
					miner.worker.start()
				}
			case downloader.DoneEvent:
				canStart = true
				if shouldStart {
					miner.SetEtherbase(miner.coinbase)
					miner.worker.start()
				}
				// Stop reacting to downloader events
				// ダウンローダーイベントへの反応を停止します
				events.Unsubscribe()
			}
		case addr := <-miner.startCh:
			miner.SetEtherbase(addr)
			if canStart {
				miner.worker.start()
			}
			shouldStart = true
		case <-miner.stopCh:
			shouldStart = false
			miner.worker.stop()
		case <-miner.exitCh:
			miner.worker.close()
			return
		}
	}
}

func (miner *Miner) Start(coinbase common.Address) {
	miner.startCh <- coinbase
}

func (miner *Miner) Stop() {
	miner.stopCh <- struct{}{}
}

func (miner *Miner) Close() {
	close(miner.exitCh)
	miner.wg.Wait()
}

func (miner *Miner) Mining() bool {
	return miner.worker.isRunning()
}

func (miner *Miner) Hashrate() uint64 {
	if pow, ok := miner.engine.(consensus.PoW); ok {
		return uint64(pow.Hashrate())
	}
	return 0
}

func (miner *Miner) SetExtra(extra []byte) error {
	if uint64(len(extra)) > params.MaximumExtraDataSize {
		return fmt.Errorf("extra exceeds max length. %d > %v", len(extra), params.MaximumExtraDataSize)
	}
	miner.worker.setExtra(extra)
	return nil
}

// SetRecommitInterval sets the interval for sealing work resubmitting.
// SetRecommitIntervalは、シール作業の再送信の間隔を設定します。
func (miner *Miner) SetRecommitInterval(interval time.Duration) {
	miner.worker.setRecommitInterval(interval)
}

// Pending returns the currently pending block and associated state.
// 保留中は、現在保留中のブロックと関連する状態を返します。
func (miner *Miner) Pending() (*types.Block, *state.StateDB) {
	return miner.worker.pending()
}

// PendingBlock returns the currently pending block.
//
// Note, to access both the pending block and the pending state
// simultaneously, please use Pending(), as the pending state can
// change between multiple method calls

// PendingBlockは現在保留中のブロックを返します。
//
// 保留中のブロックと保留中の状態の両方に同時にアクセスするには、
// 保留中の状態が複数のメソッド呼び出し間で変化する可能性があるため、Pending（）を使用してください。
func (miner *Miner) PendingBlock() *types.Block {
	return miner.worker.pendingBlock()
}

// PendingBlockAndReceipts returns the currently pending block and corresponding receipts.
// PresidentingBlockAndReceiptsは、現在保留中のブロックと対応するレシートを返します。
func (miner *Miner) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	return miner.worker.pendingBlockAndReceipts()
}

func (miner *Miner) SetEtherbase(addr common.Address) {
	miner.coinbase = addr
	miner.worker.setEtherbase(addr)
}

// SetGasCeil sets the gaslimit to strive for when mining blocks post 1559.
// For pre-1559 blocks, it sets the ceiling.
// SetGasCeilは、1559以降のブロックをマイニングするときに努力するガス制限を設定します。
// 1559より前のブロックの場合、上限を設定します。
func (miner *Miner) SetGasCeil(ceil uint64) {
	miner.worker.setGasCeil(ceil)
}

// EnablePreseal turns on the preseal mining feature. It's enabled by default.
// Note this function shouldn't be exposed to API, it's unnecessary for users
// (miners) to actually know the underlying detail. It's only for outside project
// which uses this library.
// EnablePresealは、プレシールマイニング機能をオンにします。デフォルトで有効になっています。
// この関数はAPIに公開されるべきではないことに注意してください。ユーザー（マイナー）が実際に基本的な詳細を知る必要はありません。
// このライブラリを使用する外部プロジェクト専用です。
func (miner *Miner) EnablePreseal() {
	miner.worker.enablePreseal()
}

// DisablePreseal turns off the preseal mining feature. It's necessary for some
// fake consensus engine which can seal blocks instantaneously.
// Note this function shouldn't be exposed to API, it's unnecessary for users
// (miners) to actually know the underlying detail. It's only for outside project
// which uses this library.
// DisablePresealは、プレシールマイニング機能をオフにします。
// ブロックを瞬時にシールできる偽のコンセンサスエンジンが必要です。
// この関数はAPIに公開されるべきではないことに注意してください。
// ユーザー（マイナー）が実際に基本的な詳細を知る必要はありません。
// このライブラリを使用する外部プロジェクト専用です。
func (miner *Miner) DisablePreseal() {
	miner.worker.disablePreseal()
}

// SubscribePendingLogs starts delivering logs from pending transactions
// to the given channel.
// SubscribePendingLogsは、保留中のトランザクションから指定されたチャネルへのログの配信を開始します。
func (miner *Miner) SubscribePendingLogs(ch chan<- []*types.Log) event.Subscription {
	return miner.worker.pendingLogsFeed.Subscribe(ch)
}
