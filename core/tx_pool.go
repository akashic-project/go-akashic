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

package core

import (
	"errors"
	"math"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
)

const (
	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	// chainHeadChanSizeは、ChainHeadEventをリッスンしているチャネルのサイズです。
	chainHeadChanSize = 10

	// txSlotSize is used to calculate how many data slots a single transaction
	// takes up based on its size. The slots are used as DoS protection, ensuring
	// that validating a new transaction remains a constant operation (in reality
	// O(maxslots), where max slots are 4 currently).
	// txSlotSizeは、サイズに基づいて1つのトランザクションが占めるデータスロットの数を計算するために使用されます。
	// スロットはDoS保護として使用され、新しいトランザクションの検証が一定の操作であり続けることを保証します
	// （実際には、最大スロットが4であるO（maxslots））。
	txSlotSize = 32 * 1024

	// txMaxSize is the maximum size a single transaction can have. This field has
	// non-trivial consequences: larger transactions are significantly harder and
	// more expensive to propagate; larger transactions also take more resources
	// to validate whether they fit into the pool or not.
	// txMaxSizeは、単一のトランザクションが持つことができる最大サイズです。
	// このフィールドは重要な結果をもたらします。大規模なトランザクションは、
	// 伝播するのが非常に難しく、コストがかかります。
	// トランザクションが大きいほど、プールに収まるかどうかを検証するためにより多くのリソースが必要になります。
	txMaxSize = 4 * txSlotSize // 128KB
)

var (
	// ErrAlreadyKnown is returned if the transactions is already contained
	// within the pool.
	// トランザクションがすでにプール内に含まれている場合は、ErrAlreadyKnownが返されます。
	ErrAlreadyKnown = errors.New("already known")

	// ErrInvalidSender is returned if the transaction contains an invalid signature.
	// トランザクションに無効な署名が含まれている場合、ErrInvalidSenderが返されます。
	ErrInvalidSender = errors.New("invalid sender")

	// ErrUnderpriced is returned if a transaction's gas price is below the minimum
	// configured for the transaction pool.
	// トランザクションのガス価格がトランザクションプールに設定された最小値を下回った場合、ErrUnderpricedが返されます。
	ErrUnderpriced = errors.New("transaction underpriced")

	// ErrTxPoolOverflow is returned if the transaction pool is full and can't accpet
	// another remote transaction.
	// トランザクションプールがいっぱいで、別のリモートトランザクションを受け入れることができない場合、ErrXプールオーバーフローが返されます。
	ErrTxPoolOverflow = errors.New("txpool is full")

	// ErrReplaceUnderpriced is returned if a transaction is attempted to be replaced
	// with a different one without the required price bump.
	// 必要な価格の上昇なしにトランザクションを別のトランザクションに置き換えようとすると、
	// ErrReplaceUnderpricedが返されます。
	ErrReplaceUnderpriced = errors.New("replacement transaction underpriced")

	// ErrGasLimit is returned if a transaction's requested gas limit exceeds the
	// maximum allowance of the current block.
	// トランザクションの要求されたガス制限が現在のブロックの最大許容量を超えた場合、ErrGasLimitが返されます。
	ErrGasLimit = errors.New("exceeds block gas limit")

	// ErrNegativeValue is a sanity error to ensure no one is able to specify a
	// transaction with a negative value.
	// ErrNegativeValueは、負の値でトランザクションを指定できないようにするための健全なエラーです。
	ErrNegativeValue = errors.New("negative value")

	// ErrOversizedData is returned if the input data of a transaction is greater
	// than some meaningful limit a user might use. This is not a consensus error
	// making the transaction invalid, rather a DOS protection.
	// トランザクションの入力データが、ユーザーが使用する可能性のある意味のある制限よりも大きい場合、ErrOversizedDataが返されます。
	// これは、トランザクションを無効にするコンセンサスエラーではなく、DOS保護です。
	ErrOversizedData = errors.New("oversized data")
)

var (
	evictionInterval    = time.Minute     // 削除可能なトランザクションをチェックする時間間隔    // Time interval to check for evictable transactions
	statsReportInterval = 8 * time.Second // トランザクションプールの統計を報告する時間間隔      // Time interval to report transaction pool stats
)

var (
	// Metrics for the pending pool
	// 保留中のプールのメトリック
	pendingDiscardMeter   = metrics.NewRegisteredMeter("txpool/pending/discard", nil)
	pendingReplaceMeter   = metrics.NewRegisteredMeter("txpool/pending/replace", nil)
	pendingRateLimitMeter = metrics.NewRegisteredMeter("txpool/pending/ratelimit", nil) // レート制限のために削除されました // Dropped due to rate limiting
	pendingNofundsMeter   = metrics.NewRegisteredMeter("txpool/pending/nofunds", nil)   // 資金不足のために削除されました   // Dropped due to out-of-funds

	// Metrics for the queued pool
	// キューに入れられたプールのメトリック
	queuedDiscardMeter   = metrics.NewRegisteredMeter("txpool/queued/discard", nil)
	queuedReplaceMeter   = metrics.NewRegisteredMeter("txpool/queued/replace", nil)
	queuedRateLimitMeter = metrics.NewRegisteredMeter("txpool/queued/ratelimit", nil) // レート制限のために削除されました // Dropped due to rate limiting
	queuedNofundsMeter   = metrics.NewRegisteredMeter("txpool/queued/nofunds", nil)   // 資金不足のために削除されました   // Dropped due to out-of-funds
	queuedEvictionMeter  = metrics.NewRegisteredMeter("txpool/queued/eviction", nil)  // 生涯のために削除されました // Dropped due to lifetime

	// General tx metrics
	// 一般的なtxメトリック
	knownTxMeter       = metrics.NewRegisteredMeter("txpool/known", nil)
	validTxMeter       = metrics.NewRegisteredMeter("txpool/valid", nil)
	invalidTxMeter     = metrics.NewRegisteredMeter("txpool/invalid", nil)
	underpricedTxMeter = metrics.NewRegisteredMeter("txpool/underpriced", nil)
	overflowedTxMeter  = metrics.NewRegisteredMeter("txpool/overflowed", nil)
	// throttleTxMeter counts how many transactions are rejected due to too-many-changes between
	// txpool reorgs.
	// throttleTxMeterは、txpoolreorg間の変更が多すぎるために拒否されたトランザクションの数をカウントします。
	throttleTxMeter = metrics.NewRegisteredMeter("txpool/throttle", nil)
	// reorgDurationTimer measures how long time a txpool reorg takes.
	reorgDurationTimer = metrics.NewRegisteredTimer("txpool/reorgtime", nil)
	// dropBetweenReorgHistogram counts how many drops we experience between two reorg runs. It is expected
	// that this number is pretty low, since txpool reorgs happen very frequently.
	dropBetweenReorgHistogram = metrics.NewRegisteredHistogram("txpool/dropbetweenreorg", nil, metrics.NewExpDecaySample(1028, 0.015))

	pendingGauge = metrics.NewRegisteredGauge("txpool/pending", nil)
	queuedGauge  = metrics.NewRegisteredGauge("txpool/queued", nil)
	localGauge   = metrics.NewRegisteredGauge("txpool/local", nil)
	slotsGauge   = metrics.NewRegisteredGauge("txpool/slots", nil)

	reheapTimer = metrics.NewRegisteredTimer("txpool/reheap", nil)
)

// TxStatus is the current status of a transaction as seen by the pool.
type TxStatus uint

const (
	TxStatusUnknown TxStatus = iota
	TxStatusQueued
	TxStatusPending
	TxStatusIncluded
)

// blockChain provides the state of blockchain and current gas limit to do
// some pre checks in tx pool and event subscribers.
type blockChain interface {
	CurrentBlock() *types.Block
	GetBlock(hash common.Hash, number uint64) *types.Block
	StateAt(root common.Hash) (*state.StateDB, error)

	SubscribeChainHeadEvent(ch chan<- ChainHeadEvent) event.Subscription
}

// TxPoolConfig are the configuration parameters of the transaction pool.
// TxPoolConfigは、トランザクションプールの構成パラメーターです。
type TxPoolConfig struct {
	Locals    []common.Address // デフォルトでローカルとして扱われるべきアドレス                // Addresses that should be treated by default as local
	NoLocals  bool             // ローカルトランザクション処理を無効にする必要があるかどうか     // Whether local transaction handling should be disabled
	Journal   string           // ノードの再起動後も存続するローカルトランザクションのジャーナル // Journal of local transactions to survive node restarts
	Rejournal time.Duration    // ローカルトランザクションジャーナルを再生成する時間間隔        // Time interval to regenerate the local transaction journal

	PriceLimit uint64 // プールへの受け入れを強制するための最低ガス価格   // Minimum gas price to enforce for acceptance into the pool
	PriceBump  uint64 // 既存のトランザクションを置き換えるための最小価格上昇率（ノンス） // Minimum price bump percentage to replace an already existing transaction (nonce)

	AccountSlots uint64 // アカウントごとに保証された実行可能トランザクションスロットの数 // Number of executable transaction slots guaranteed per account
	GlobalSlots  uint64 // すべてのアカウントの実行可能なトランザクションスロットの最大数 // Maximum number of executable transaction slots for all accounts
	AccountQueue uint64 // アカウントごとに許可される実行不可能なトランザクションスロットの最大数 // Maximum number of non-executable transaction slots permitted per account
	GlobalQueue  uint64 // すべてのアカウントの実行不可能なトランザクションスロットの最大数      // Maximum number of non-executable transaction slots for all accounts

	Lifetime time.Duration // 実行不可能なトランザクションがキューに入れられる最大時間          // Maximum amount of time non-executable transaction are queued
}

// DefaultTxPoolConfig contains the default configurations for the transaction
// pool.
// DefaultTxPoolConfigには、トランザクションプールのデフォルト構成が含まれています。
var DefaultTxPoolConfig = TxPoolConfig{
	Journal:   "transactions.rlp",
	Rejournal: time.Hour,

	PriceLimit: 1,
	PriceBump:  10,

	AccountSlots: 16,
	GlobalSlots:  4096 + 1024, // urgent + floating queue capacity with 4:1 ratio
	AccountQueue: 64,
	GlobalQueue:  1024,

	Lifetime: 3 * time.Hour,
}

// sanitize checks the provided user configurations and changes anything that's
// unreasonable or unworkable.
// サニタイズは、提供されたユーザー構成をチェックし、不合理または実行不可能なものをすべて変更します。
func (config *TxPoolConfig) sanitize() TxPoolConfig {
	conf := *config
	if conf.Rejournal < time.Second {
		log.Warn("Sanitizing invalid txpool journal time", "provided", conf.Rejournal, "updated", time.Second)
		conf.Rejournal = time.Second
	}
	if conf.PriceLimit < 1 {
		log.Warn("Sanitizing invalid txpool price limit", "provided", conf.PriceLimit, "updated", DefaultTxPoolConfig.PriceLimit)
		conf.PriceLimit = DefaultTxPoolConfig.PriceLimit
	}
	if conf.PriceBump < 1 {
		log.Warn("Sanitizing invalid txpool price bump", "provided", conf.PriceBump, "updated", DefaultTxPoolConfig.PriceBump)
		conf.PriceBump = DefaultTxPoolConfig.PriceBump
	}
	if conf.AccountSlots < 1 {
		log.Warn("Sanitizing invalid txpool account slots", "provided", conf.AccountSlots, "updated", DefaultTxPoolConfig.AccountSlots)
		conf.AccountSlots = DefaultTxPoolConfig.AccountSlots
	}
	if conf.GlobalSlots < 1 {
		log.Warn("Sanitizing invalid txpool global slots", "provided", conf.GlobalSlots, "updated", DefaultTxPoolConfig.GlobalSlots)
		conf.GlobalSlots = DefaultTxPoolConfig.GlobalSlots
	}
	if conf.AccountQueue < 1 {
		log.Warn("Sanitizing invalid txpool account queue", "provided", conf.AccountQueue, "updated", DefaultTxPoolConfig.AccountQueue)
		conf.AccountQueue = DefaultTxPoolConfig.AccountQueue
	}
	if conf.GlobalQueue < 1 {
		log.Warn("Sanitizing invalid txpool global queue", "provided", conf.GlobalQueue, "updated", DefaultTxPoolConfig.GlobalQueue)
		conf.GlobalQueue = DefaultTxPoolConfig.GlobalQueue
	}
	if conf.Lifetime < 1 {
		log.Warn("Sanitizing invalid txpool lifetime", "provided", conf.Lifetime, "updated", DefaultTxPoolConfig.Lifetime)
		conf.Lifetime = DefaultTxPoolConfig.Lifetime
	}
	return conf
}

// TxPool contains all currently known transactions. Transactions
// enter the pool when they are received from the network or submitted
// locally. They exit the pool when they are included in the blockchain.
//
// The pool separates processable transactions (which can be applied to the
// current state) and future transactions. Transactions move between those
// two states over time as they are received and processed.
// TxPoolには、現在知られているすべてのトランザクションが含まれています。
// トランザクションは、ネットワークから受信されたとき、またはローカルで送信されたときにプールに入ります。
// それらがブロックチェーンに含まれると、プールを終了します。
//
// プールは、処理可能なトランザクション（現在の状態に適用できます）と将来のトランザクションを分離します。
// トランザクションは、受信および処理されるときに、時間の経過とともにこれら2つの状態間を移動します。
type TxPool struct {
	config      TxPoolConfig
	chainconfig *params.ChainConfig
	chain       blockChain
	gasPrice    *big.Int
	txFeed      event.Feed
	scope       event.SubscriptionScope
	signer      types.Signer
	mu          sync.RWMutex

	istanbul bool // イスタンブールの段階にあるかどうかを示すフォークインジケーター。      // Fork indicator whether we are in the istanbul stage.
	eip2718  bool // EIP-2718タイプのトランザクションを使用しているかどうかを示すフォークインジケーター。 // Fork indicator whether we are using EIP-2718 type transactions.
	eip1559  bool // EIP-1559タイプのトランザクションを使用しているかどうかを示すフォークインジケーター。 // Fork indicator whether we are using EIP-1559 type transactions.

	currentState  *state.StateDB // ブロックチェーンヘッドの現在の状態 // Current state in the blockchain head
	pendingNonces *txNoncer      // 保留中の状態追跡仮想ナンス        // Pending state tracking virtual nonces
	currentMaxGas uint64         //トランザクションキャップの現在のガス制限 // Current gas limit for transaction caps

	locals  *accountSet // 排除ルールを免除するローカルトランザクションのセット // Set of local transaction to exempt from eviction rules
	journal *txJournal  //ディスクにバックアップするローカルトランザクションのジャーナル // Journal of local transaction to back up to disk

	pending map[common.Address]*txList   // 現在処理可能なすべてのトランザクション              // All currently processable transactions
	queue   map[common.Address]*txList   // キューに入れられているが処理できないトランザクション // Queued but non-processable transactions
	beats   map[common.Address]time.Time // 既知の各アカウントからの最後のハートビート          // Last heartbeat from each known account
	all     *txLookup                    // ルックアップを許可するすべてのトランザクション      // All transactions to allow lookups
	priced  *txPricedList                // すべてのトランザクションを価格でソート             // All transactions sorted by price

	chainHeadCh     chan ChainHeadEvent
	chainHeadSub    event.Subscription
	reqResetCh      chan *txpoolResetRequest
	reqPromoteCh    chan *accountSet
	queueTxEventCh  chan *types.Transaction
	reorgDoneCh     chan chan struct{}
	reorgShutdownCh chan struct{}  // scheduleReorgLoopのシャットダウンを要求します // requests shutdown of scheduleReorgLoop
	wg              sync.WaitGroup // ループを追跡し、scheduleReorgLoop            // tracks loop, scheduleReorgLoop
	initDoneCh      chan struct{}  // プールが初期化されると閉じられます（テスト用）  // is closed once the pool is initialized (for tests)

	changesSinceReorg int // reorgの間に実行したドロップ数のカウンター。   // A counter for how many drops we've performed in-between reorg.
}

type txpoolResetRequest struct {
	oldHead, newHead *types.Header
}

// NewTxPool creates a new transaction pool to gather, sort and filter inbound
// transactions from the network.
// NewTxPoolは、ネットワークからのインバウンドトランザクションを収集、
// 並べ替え、フィルタリングするための新しいトランザクションプールを作成します。
func NewTxPool(config TxPoolConfig, chainconfig *params.ChainConfig, chain blockChain) *TxPool {
	// Sanitize the input to ensure no vulnerable gas prices are set
	// 入力をサニタイズして、脆弱なガス価格が設定されていないことを確認します
	config = (&config).sanitize()

	// Create the transaction pool with its initial settings
	// 初期設定でトランザクションプールを作成します
	pool := &TxPool{
		config:          config,
		chainconfig:     chainconfig,
		chain:           chain,
		signer:          types.LatestSigner(chainconfig),
		pending:         make(map[common.Address]*txList),
		queue:           make(map[common.Address]*txList),
		beats:           make(map[common.Address]time.Time),
		all:             newTxLookup(),
		chainHeadCh:     make(chan ChainHeadEvent, chainHeadChanSize),
		reqResetCh:      make(chan *txpoolResetRequest),
		reqPromoteCh:    make(chan *accountSet),
		queueTxEventCh:  make(chan *types.Transaction),
		reorgDoneCh:     make(chan chan struct{}),
		reorgShutdownCh: make(chan struct{}),
		initDoneCh:      make(chan struct{}),
		gasPrice:        new(big.Int).SetUint64(config.PriceLimit),
	}
	pool.locals = newAccountSet(pool.signer)
	for _, addr := range config.Locals {
		log.Info("Setting new local account", "address", addr)
		pool.locals.add(addr)
	}
	pool.priced = newTxPricedList(pool.all)
	pool.reset(nil, chain.CurrentBlock().Header())

	// Start the reorg loop early so it can handle requests generated during journal loading.
	// ジャーナルの読み込み中に生成されたリクエストを処理できるように、reorgループを早期に開始します。
	pool.wg.Add(1)
	go pool.scheduleReorgLoop()

	// If local transactions and journaling is enabled, load from disk
	// ローカルトランザクションとジャーナリングが有効になっている場合は、ディスクからロードします
	if !config.NoLocals && config.Journal != "" {
		pool.journal = newTxJournal(config.Journal)

		if err := pool.journal.load(pool.AddLocals); err != nil {
			log.Warn("Failed to load transaction journal", "err", err)
		}
		if err := pool.journal.rotate(pool.local()); err != nil {
			log.Warn("Failed to rotate transaction journal", "err", err)
		}
	}

	// Subscribe events from blockchain and start the main event loop.
	// ブロックチェーンからイベントをサブスクライブし、メインイベントループを開始します。
	pool.chainHeadSub = pool.chain.SubscribeChainHeadEvent(pool.chainHeadCh)
	pool.wg.Add(1)
	go pool.loop()

	return pool
}

// loop is the transaction pool's main event loop, waiting for and reacting to
// outside blockchain events as well as for various reporting and transaction
// eviction events.
// ループは、トランザクションプールのメインイベントループであり、外部のブロックチェーンイベント、
// およびさまざまなレポートイベントとトランザクション排除イベントを待機して反応します。
func (pool *TxPool) loop() {
	defer pool.wg.Done()

	var (
		prevPending, prevQueued, prevStales int
		// Start the stats reporting and transaction eviction tickers
		// 統計レポートとトランザクション排除ティッカーを開始します
		report  = time.NewTicker(statsReportInterval)
		evict   = time.NewTicker(evictionInterval)
		journal = time.NewTicker(pool.config.Rejournal)
		// Track the previous head headers for transaction reorgs
		// トランザクション再編成のために以前のヘッドヘッダーを追跡します
		head = pool.chain.CurrentBlock()
	)
	defer report.Stop()
	defer evict.Stop()
	defer journal.Stop()

	// Notify tests that the init phase is done
	// 初期化フェーズが完了したことをテストに通知します
	close(pool.initDoneCh)
	for {
		select {
		// Handle ChainHeadEvent
		// ChainHeadEventを処理します
		case ev := <-pool.chainHeadCh:
			if ev.Block != nil {
				pool.requestReset(head.Header(), ev.Block.Header())
				head = ev.Block
			}

		// System shutdown.
		// システムのシャットダウン。
		case <-pool.chainHeadSub.Err():
			close(pool.reorgShutdownCh)
			return

		// Handle stats reporting ticks
		// ティックを報告する統計を処理します
		case <-report.C:
			pool.mu.RLock()
			pending, queued := pool.stats()
			pool.mu.RUnlock()
			stales := int(atomic.LoadInt64(&pool.priced.stales))

			if pending != prevPending || queued != prevQueued || stales != prevStales {
				log.Debug("Transaction pool status report", "executable", pending, "queued", queued, "stales", stales)
				prevPending, prevQueued, prevStales = pending, queued, stales
			}

		// Handle inactive account transaction eviction
		// 非アクティブなアカウントトランザクションの削除を処理します
		case <-evict.C:
			pool.mu.Lock()
			for addr := range pool.queue {
				// Skip local transactions from the eviction mechanism
				// 排除メカニズムからローカルトランザクションをスキップします
				if pool.locals.contains(addr) {
					continue
				}
				// Any non-locals old enough should be removed
				// 十分に古い非ローカルは削除する必要があります
				if time.Since(pool.beats[addr]) > pool.config.Lifetime {
					list := pool.queue[addr].Flatten()
					for _, tx := range list {
						pool.removeTx(tx.Hash(), true)
					}
					queuedEvictionMeter.Mark(int64(len(list)))
				}
			}
			pool.mu.Unlock()

		// Handle local transaction journal rotation
		// ローカルトランザクションジャーナルのローテーションを処理します
		case <-journal.C:
			if pool.journal != nil {
				pool.mu.Lock()
				if err := pool.journal.rotate(pool.local()); err != nil {
					log.Warn("Failed to rotate local tx journal", "err", err)
				}
				pool.mu.Unlock()
			}
		}
	}
}

// Stop terminates the transaction pool.
// Stopはトランザクションプールを終了します。
func (pool *TxPool) Stop() {
	// Unsubscribe all subscriptions registered from txpool
	// txpoolから登録されたすべてのサブスクリプションのサブスクリプションを解除します
	pool.scope.Close()

	// Unsubscribe subscriptions registered from blockchain
	// ブロックチェーンから登録されたサブスクリプションの購読を解除します
	pool.chainHeadSub.Unsubscribe()
	pool.wg.Wait()

	if pool.journal != nil {
		pool.journal.close()
	}
	log.Info("Transaction pool stopped")
}

// SubscribeNewTxsEvent registers a subscription of NewTxsEvent and
// starts sending event to the given channel.
// SubscribeNewTxsEventは、NewTxsEventのサブスクリプションを登録し、
// 指定されたチャネルへのイベントの送信を開始します。
func (pool *TxPool) SubscribeNewTxsEvent(ch chan<- NewTxsEvent) event.Subscription {
	return pool.scope.Track(pool.txFeed.Subscribe(ch))
}

// GasPrice returns the current gas price enforced by the transaction pool.
// GasPriceは、トランザクションプールによって適用された現在のガス価格を返します。
func (pool *TxPool) GasPrice() *big.Int {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	return new(big.Int).Set(pool.gasPrice)
}

// SetGasPrice updates the minimum price required by the transaction pool for a
// new transaction, and drops all transactions below this threshold.
// SetGasPriceは、新しいトランザクションのトランザクションプールに必要な最小価格を更新し、
// すべてのトランザクションをこのしきい値未満に落とします。
func (pool *TxPool) SetGasPrice(price *big.Int) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	old := pool.gasPrice
	pool.gasPrice = price
	// if the min miner fee increased, remove transactions below the new threshold
	// 最小マイナー料金が増加した場合は、新しいしきい値を下回るトランザクションを削除します
	if price.Cmp(old) > 0 {
		// pool.priced is sorted by GasFeeCap, so we have to iterate through pool.all instead
		// pool.pricedはGasFeeCapでソートされるため、代わりにpool.allを反復処理する必要があります
		drop := pool.all.RemotesBelowTip(price)
		for _, tx := range drop {
			pool.removeTx(tx.Hash(), false)
		}
		pool.priced.Removed(len(drop))
	}

	log.Info("Transaction pool price threshold updated", "price", price)
}

// Nonce returns the next nonce of an account, with all transactions executable
// by the pool already applied on top.
// Nonceはアカウントの次のナンスを返し、
// プールによって実行可能なすべてのトランザクションがすでに最上位に適用されています。
func (pool *TxPool) Nonce(addr common.Address) uint64 {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	return pool.pendingNonces.get(addr)
}

// Stats retrieves the current pool stats, namely the number of pending and the
// number of queued (non-executable) transactions.
//統計は、現在のプール統計、つまり保留中のトランザクションの数とキューに入れられた（実行不可能な）トランザクションの数を取得します。
func (pool *TxPool) Stats() (int, int) {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	return pool.stats()
}

// stats retrieves the current pool stats, namely the number of pending and the
// number of queued (non-executable) transactions.
// statsは、現在のプール統計、つまり保留中のトランザクションの数とキューに入れられた（実行不可能な）トランザクションの数を取得します。
func (pool *TxPool) stats() (int, int) {
	pending := 0
	for _, list := range pool.pending {
		pending += list.Len()
	}
	queued := 0
	for _, list := range pool.queue {
		queued += list.Len()
	}
	return pending, queued
}

// Content retrieves the data content of the transaction pool, returning all the
// pending as well as queued transactions, grouped by account and sorted by nonce.
// Contentは、トランザクションプールのデータコンテンツを取得し、アカウントごとにグループ化され、
// nonceごとに並べ替えられた、保留中のトランザクションとキューに入れられたトランザクションをすべて返します。
func (pool *TxPool) Content() (map[common.Address]types.Transactions, map[common.Address]types.Transactions) {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	pending := make(map[common.Address]types.Transactions)
	for addr, list := range pool.pending {
		pending[addr] = list.Flatten()
	}
	queued := make(map[common.Address]types.Transactions)
	for addr, list := range pool.queue {
		queued[addr] = list.Flatten()
	}
	return pending, queued
}

// ContentFrom retrieves the data content of the transaction pool, returning the
// pending as well as queued transactions of this address, grouped by nonce.
// ContentFromは、トランザクションプールのデータコンテンツを取得し、nonceでグループ化された、
// このアドレスの保留中のトランザクションとキューに入れられたトランザクションを返します。
func (pool *TxPool) ContentFrom(addr common.Address) (types.Transactions, types.Transactions) {
	pool.mu.RLock()
	defer pool.mu.RUnlock()

	var pending types.Transactions
	if list, ok := pool.pending[addr]; ok {
		pending = list.Flatten()
	}
	var queued types.Transactions
	if list, ok := pool.queue[addr]; ok {
		queued = list.Flatten()
	}
	return pending, queued
}

// Pending retrieves all currently processable transactions, grouped by origin
// account and sorted by nonce. The returned transaction set is a copy and can be
// freely modified by calling code.
//
// The enforceTips parameter can be used to do an extra filtering on the pending
// transactions and only return those whose **effective** tip is large enough in
// the next pending execution environment.
// 保留中は、オリジンアカウントでグループ化され、ナンスでソートされた、
// 現在処理可能なすべてのトランザクションを取得します。
// 返されるトランザクションセットはコピーであり、コードを呼び出すことで自由に変更できます。
//
// EnforceTipsパラメータを使用して、保留中のトランザクションに対して追加のフィルタリングを実行し、
// 次の保留中の実行環境で**有効な**ヒントが十分に大きいトランザクションのみを返すことができます。
func (pool *TxPool) Pending(enforceTips bool) map[common.Address]types.Transactions {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	pending := make(map[common.Address]types.Transactions)
	for addr, list := range pool.pending {
		txs := list.Flatten()

		// If the miner requests tip enforcement, cap the lists now
		// マイナーがチップの強制を要求した場合は、今すぐリストに上限を設定します
		if enforceTips && !pool.locals.contains(addr) {
			for i, tx := range txs {
				if tx.EffectiveGasTipIntCmp(pool.gasPrice, pool.priced.urgent.baseFee) < 0 {
					txs = txs[:i]
					break
				}
			}
		}
		if len(txs) > 0 {
			pending[addr] = txs
		}
	}
	return pending
}

// Locals retrieves the accounts currently considered local by the pool.
// Localsは、プールによって現在ローカルと見なされているアカウントを取得します。
func (pool *TxPool) Locals() []common.Address {
	pool.mu.Lock()
	defer pool.mu.Unlock()

	return pool.locals.flatten()
}

// local retrieves all currently known local transactions, grouped by origin
// account and sorted by nonce. The returned transaction set is a copy and can be
// freely modified by calling code.
// localは、現在知られているすべてのローカルトランザクションを取得し、
// オリジンアカウントでグループ化され、nonceで並べ替えられます。
// 返されるトランザクションセットはコピーであり、コードを呼び出すことで自由に変更できます。
func (pool *TxPool) local() map[common.Address]types.Transactions {
	txs := make(map[common.Address]types.Transactions)
	for addr := range pool.locals.accounts {
		if pending := pool.pending[addr]; pending != nil {
			txs[addr] = append(txs[addr], pending.Flatten()...)
		}
		if queued := pool.queue[addr]; queued != nil {
			txs[addr] = append(txs[addr], queued.Flatten()...)
		}
	}
	return txs
}

// validateTx checks whether a transaction is valid according to the consensus
// rules and adheres to some heuristic limits of the local node (price and size).
// validateTxは、トランザクションがコンセンサスルールに従って有効であり、
// ローカルノードのヒューリスティックな制限（価格とサイズ）に準拠しているかどうかを確認します。
func (pool *TxPool) validateTx(tx *types.Transaction, local bool) error {
	// Accept only legacy transactions until EIP-2718/2930 activates.
	// EIP-2718 / 2930がアクティブになるまで、レガシートランザクションのみを受け入れます。
	if !pool.eip2718 && tx.Type() != types.LegacyTxType {
		return ErrTxTypeNotSupported
	}
	// Reject dynamic fee transactions until EIP-1559 activates.
	// EIP-1559がアクティブになるまで、動的な手数料トランザクションを拒否します。
	if !pool.eip1559 && tx.Type() == types.DynamicFeeTxType {
		return ErrTxTypeNotSupported
	}
	// Reject transactions over defined size to prevent DOS attacks
	// DOS攻撃を防ぐために、定義されたサイズを超えるトランザクションを拒否します
	if uint64(tx.Size()) > txMaxSize {
		return ErrOversizedData
	}
	// Transactions can't be negative. This may never happen using RLP decoded
	// transactions but may occur if you create a transaction using the RPC.
	// トランザクションを負にすることはできません。
	// これは、RLPデコードされたトランザクションを使用して発生することはありませんが、
	// RPCを使用してトランザクションを作成した場合に発生する可能性があります。
	if tx.Value().Sign() < 0 {
		return ErrNegativeValue
	}
	// Ensure the transaction doesn't exceed the current block limit gas.
	// トランザクションが現在のブロック制限ガスを超えないようにしてください。
	if pool.currentMaxGas < tx.Gas() {
		return ErrGasLimit
	}
	// Sanity check for extremely large numbers
	// 非常に大きな数の健全性チェック
	if tx.GasFeeCap().BitLen() > 256 {
		return ErrFeeCapVeryHigh
	}
	if tx.GasTipCap().BitLen() > 256 {
		return ErrTipVeryHigh
	}
	// Ensure gasFeeCap is greater than or equal to gasTipCap.
	// gasFeeCapがgasTipCap以上であることを確認します。
	if tx.GasFeeCapIntCmp(tx.GasTipCap()) < 0 {
		return ErrTipAboveFeeCap
	}
	// Make sure the transaction is signed properly.
	// トランザクションが正しく署名されていることを確認します。
	from, err := types.Sender(pool.signer, tx)
	if err != nil {
		return ErrInvalidSender
	}
	// Drop non-local transactions under our own minimal accepted gas price or tip
	// 非ローカルトランザクションを私たち自身の最小許容ガス価格またはチップの下でドロップします。
	if !local && tx.GasTipCapIntCmp(pool.gasPrice) < 0 {
		return ErrUnderpriced
	}
	// Ensure the transaction adheres to nonce ordering
	// トランザクションがナンスの順序に準拠していることを確認します
	if pool.currentState.GetNonce(from) > tx.Nonce() {
		return ErrNonceTooLow
	}
	// Transactor should have enough funds to cover the costs
	// Transactorには、費用を賄うのに十分な資金が必要です
	// cost == V + GP * GL
	if pool.currentState.GetBalance(from).Cmp(tx.Cost()) < 0 {
		return ErrInsufficientFunds
	}
	// Ensure the transaction has more gas than the basic tx fee.
	// トランザクションに基本的なtx料金よりも多くのガスが含まれていることを確認します。
	intrGas, err := IntrinsicGas(tx.Data(), tx.AccessList(), tx.To() == nil, true, pool.istanbul)
	if err != nil {
		return err
	}
	if tx.Gas() < intrGas {
		return ErrIntrinsicGas
	}
	return nil
}

// add validates a transaction and inserts it into the non-executable queue for later
// pending promotion and execution. If the transaction is a replacement for an already
// pending or queued one, it overwrites the previous transaction if its price is higher.
//
// If a newly added transaction is marked as local, its sending account will be
// be added to the allowlist, preventing any associated transaction from being dropped
// out of the pool due to pricing constraints.
// addはトランザクションを検証し、後で保留中のプロモーションと実行のために実行不可能なキューに挿入します。
// トランザクションがすでに保留中またはキューに入れられているトランザクションの代わりである場合、
// その価格が高いと、前のトランザクションが上書きされます。
//
// 新しく追加されたトランザクションがローカルとしてマークされている場合、
// その送信アカウントが許可リストに追加され、
// 価格の制約のために関連するトランザクションがプールからドロップされるのを防ぎます。
func (pool *TxPool) add(tx *types.Transaction, local bool) (replaced bool, err error) {
	// If the transaction is already known, discard it
	// トランザクションがすでにわかっている場合は、それを破棄します
	hash := tx.Hash()
	if pool.all.Get(hash) != nil {
		log.Trace("Discarding already known transaction", "hash", hash)
		knownTxMeter.Mark(1)
		return false, ErrAlreadyKnown
	}
	// Make the local flag. If it's from local source or it's from the network but
	// the sender is marked as local previously, treat it as the local transaction.
	// ローカルフラグを作成します。 ローカルソースからのものか、ネットワークからのものであるが、
	// 送信者が以前にローカルとしてマークされている場合は、ローカルトランザクションとして扱います。
	isLocal := local || pool.locals.containsTx(tx)

	// If the transaction fails basic validation, discard it
	// トランザクションが基本的な検証に失敗した場合は、破棄します
	if err := pool.validateTx(tx, isLocal); err != nil {
		log.Trace("Discarding invalid transaction", "hash", hash, "err", err)
		invalidTxMeter.Mark(1)
		return false, err
	}
	// If the transaction pool is full, discard underpriced transactions
	// トランザクションプールがいっぱいの場合、低価格のトランザクションを破棄します
	if uint64(pool.all.Slots()+numSlots(tx)) > pool.config.GlobalSlots+pool.config.GlobalQueue {
		// If the new transaction is underpriced, don't accept it
		// 新しいトランザクションの価格が安い場合は、受け入れないでください
		if !isLocal && pool.priced.Underpriced(tx) {
			log.Trace("Discarding underpriced transaction", "hash", hash, "gasTipCap", tx.GasTipCap(), "gasFeeCap", tx.GasFeeCap())
			underpricedTxMeter.Mark(1)
			return false, ErrUnderpriced
		}
		// We're about to replace a transaction. The reorg does a more thorough
		// analysis of what to remove and how, but it runs async. We don't want to
		// do too many replacements between reorg-runs, so we cap the number of
		// replacements to 25% of the slots
		// トランザクションを置き換えようとしています。
		// reorgは、何をどのように削除するかについてより徹底的な分析を行いますが、非同期で実行されます。
		// 再編成の実行の間にあまり多くの置換を行いたくないので、置換の数をスロットの25％に制限します
		if pool.changesSinceReorg > int(pool.config.GlobalSlots/4) {
			throttleTxMeter.Mark(1)
			return false, ErrTxPoolOverflow
		}

		// New transaction is better than our worse ones, make room for it.
		// If it's a local transaction, forcibly discard all available transactions.
		// Otherwise if we can't make enough room for new one, abort the operation.
		// 新しいトランザクションは、悪いトランザクションよりも優れています。
		// そのためのスペースを確保してください。
		// ローカルトランザクションの場合は、使用可能なすべてのトランザクションを強制的に破棄します。
		// それ以外の場合、新しいスペースを確保できない場合は、操作を中止します。
		drop, success := pool.priced.Discard(pool.all.Slots()-int(pool.config.GlobalSlots+pool.config.GlobalQueue)+numSlots(tx), isLocal)

		// Special case, we still can't make the room for the new remote one.
		// 特別な場合、まだ新しいリモート用のスペースを作ることができません。
		if !isLocal && !success {
			log.Trace("Discarding overflown transaction", "hash", hash)
			overflowedTxMeter.Mark(1)
			return false, ErrTxPoolOverflow
		}
		// Bump the counter of rejections-since-reorg
		// 拒否のカウンターをバンプします-since-reorg
		pool.changesSinceReorg += len(drop)
		// Kick out the underpriced remote transactions.
		// 低価格のリモートトランザクションを開始します。
		for _, tx := range drop {
			log.Trace("Discarding freshly underpriced transaction", "hash", tx.Hash(), "gasTipCap", tx.GasTipCap(), "gasFeeCap", tx.GasFeeCap())
			underpricedTxMeter.Mark(1)
			pool.removeTx(tx.Hash(), false)
		}
	}
	// Try to replace an existing transaction in the pending pool
	// 保留中のプール内の既存のトランザクションを置き換えようとします
	from, _ := types.Sender(pool.signer, tx) // すでに検証済み // already validated
	if list := pool.pending[from]; list != nil && list.Overlaps(tx) {
		// Nonce already pending, check if required price bump is met
		// すでに保留中ですが、必要な価格の上昇が満たされているかどうかを確認します
		inserted, old := list.Add(tx, pool.config.PriceBump)
		if !inserted {
			pendingDiscardMeter.Mark(1)
			return false, ErrReplaceUnderpriced
		}
		// New transaction is better, replace old one
		// 新しいトランザクションの方が優れています。古いトランザクションを置き換えてください
		if old != nil {
			pool.all.Remove(old.Hash())
			pool.priced.Removed(1)
			pendingReplaceMeter.Mark(1)
		}
		pool.all.Add(tx, isLocal)
		pool.priced.Put(tx, isLocal)
		pool.journalTx(from, tx)
		pool.queueTxEvent(tx)
		log.Trace("Pooled new executable transaction", "hash", hash, "from", from, "to", tx.To())

		// Successful promotion, bump the heartbeat
		// プロモーションを成功させ、ハートビートを上げます
		pool.beats[from] = time.Now()
		return old != nil, nil
	}
	// New transaction isn't replacing a pending one, push into queue
	// 新しいトランザクションは保留中のトランザクションを置き換えていません。キューにプッシュします
	replaced, err = pool.enqueueTx(hash, tx, isLocal, true)
	if err != nil {
		return false, err
	}
	// Mark local addresses and journal local transactions
	// ローカルアドレスをマークし、ローカルトランザクションをジャーナルします
	if local && !pool.locals.contains(from) {
		log.Info("Setting new local account", "address", from)
		pool.locals.add(from)
		pool.priced.Removed(pool.all.RemoteToLocals(pool.locals)) //初めてローカルとしてマークされた場合は、リモートを移行します。 // Migrate the remotes if it's marked as local first time.
	}
	if isLocal {
		localGauge.Inc(1)
	}
	pool.journalTx(from, tx)

	log.Trace("Pooled new future transaction", "hash", hash, "from", from, "to", tx.To())
	return replaced, nil
}

// enqueueTx inserts a new transaction into the non-executable transaction queue.
//
// Note, this method assumes the pool lock is held!
// enqueueTxは、実行不可能なトランザクションキューに新しいトランザクションを挿入します。
//
//このメソッドは、プールロックが保持されていることを前提としています。
func (pool *TxPool) enqueueTx(hash common.Hash, tx *types.Transaction, local bool, addAll bool) (bool, error) {
	// Try to insert the transaction into the future queue
	// トランザクションを将来のキューに挿入しようとします
	from, _ := types.Sender(pool.signer, tx) //すでに検証済み // already validated
	if pool.queue[from] == nil {
		pool.queue[from] = newTxList(false)
	}
	inserted, old := pool.queue[from].Add(tx, pool.config.PriceBump)
	if !inserted {
		// An older transaction was better, discard this
		// 古いトランザクションの方が良かったので、これを破棄します
		queuedDiscardMeter.Mark(1)
		return false, ErrReplaceUnderpriced
	}
	// Discard any previous transaction and mark this
	// 以前のトランザクションを破棄し、これにマークを付けます
	if old != nil {
		pool.all.Remove(old.Hash())
		pool.priced.Removed(1)
		queuedReplaceMeter.Mark(1)
	} else {
		// Nothing was replaced, bump the queued counter
		// 何も置き換えられませんでした、キューに入れられたカウンターをバンプします
		queuedGauge.Inc(1)
	}
	// If the transaction isn't in lookup set but it's expected to be there,
	// show the error log.
	// トランザクションがルックアップセットに含まれていないが、存在すると予想される場合は、エラーログを表示します。
	if pool.all.Get(hash) == nil && !addAll {
		log.Error("Missing transaction in lookup set, please report the issue", "hash", hash)
	}
	if addAll {
		pool.all.Add(tx, local)
		pool.priced.Put(tx, local)
	}
	// If we never record the heartbeat, do it right now.
	// ハートビートを記録しない場合は、今すぐ記録してください。
	if _, exist := pool.beats[from]; !exist {
		pool.beats[from] = time.Now()
	}
	return old != nil, nil
}

// journalTx adds the specified transaction to the local disk journal if it is
// deemed to have been sent from a local account.
// journalTxは、ローカルアカウントから送信されたと見なされる場合、
// 指定されたトランザクションをローカルディスクジャーナルに追加します。
func (pool *TxPool) journalTx(from common.Address, tx *types.Transaction) {
	// Only journal if it's enabled and the transaction is local
	// 有効になっていて、トランザクションがローカルである場合にのみジャーナル
	if pool.journal == nil || !pool.locals.contains(from) {
		return
	}
	if err := pool.journal.insert(tx); err != nil {
		log.Warn("Failed to journal local transaction", "err", err)
	}
}

// promoteTx adds a transaction to the pending (processable) list of transactions
// and returns whether it was inserted or an older was better.
//
// Note, this method assumes the pool lock is held!
// PromoteTxは、トランザクションの保留中の（処理可能な）リストにトランザクションを追加し、
// それが挿入されたか、古い方が優れているかを返します。
//
// このメソッドは、プールロックが保持されていることを前提としています。
func (pool *TxPool) promoteTx(addr common.Address, hash common.Hash, tx *types.Transaction) bool {
	// Try to insert the transaction into the pending queue
	// トランザクションを保留中のキューに挿入しようとします
	if pool.pending[addr] == nil {
		pool.pending[addr] = newTxList(true)
	}
	list := pool.pending[addr]

	inserted, old := list.Add(tx, pool.config.PriceBump)
	if !inserted {
		// An older transaction was better, discard this
		// 古いトランザクションの方が良かったので、これを破棄します
		pool.all.Remove(hash)
		pool.priced.Removed(1)
		pendingDiscardMeter.Mark(1)
		return false
	}
	// Otherwise discard any previous transaction and mark this
	// それ以外の場合は、以前のトランザクションを破棄し、これにマークを付けます
	if old != nil {
		pool.all.Remove(old.Hash())
		pool.priced.Removed(1)
		pendingReplaceMeter.Mark(1)
	} else {
		// Nothing was replaced, bump the pending counter
		// 何も置き換えられませんでした、保留中のカウンターをバンプします
		pendingGauge.Inc(1)
	}
	// Set the potentially new pending nonce and notify any subsystems of the new tx
	// 潜在的に新しい保留中のナンスを設定し、サブシステムに新しいtxを通知します
	pool.pendingNonces.set(addr, tx.Nonce()+1)

	// Successful promotion, bump the heartbeat
	// プロモーションを成功させ、ハートビートを上げます
	pool.beats[addr] = time.Now()
	return true
}

// AddLocals enqueues a batch of transactions into the pool if they are valid, marking the
// senders as a local ones, ensuring they go around the local pricing constraints.
//
// This method is used to add transactions from the RPC API and performs synchronous pool
// reorganization and event propagation.
// AddLocalsは、トランザクションのバッチが有効な場合はプールにエンキューし、
// 送信者をローカルのトランザクションとしてマークし、ローカルの価格設定の制約を回避できるようにします。
//
// このメソッドは、RPC APIからトランザクションを追加するために使用され、
// 同期プールの再編成とイベントの伝播を実行します。
func (pool *TxPool) AddLocals(txs []*types.Transaction) []error {
	return pool.addTxs(txs, !pool.config.NoLocals, true)
}

// AddLocal enqueues a single local transaction into the pool if it is valid. This is
// a convenience wrapper aroundd AddLocals.
// AddLocalは、有効な場合、単一のローカルトランザクションをプールにエンキューします。
// これは、AddLocalsを取り巻く便利なラッパーです。
func (pool *TxPool) AddLocal(tx *types.Transaction) error {
	errs := pool.AddLocals([]*types.Transaction{tx})
	return errs[0]
}

// AddRemotes enqueues a batch of transactions into the pool if they are valid. If the
// senders are not among the locally tracked ones, full pricing constraints will apply.
//
// This method is used to add transactions from the p2p network and does not wait for pool
// reorganization and internal event propagation.
// AddRemotesは、トランザクションのバッチが有効な場合、それらをプールにエンキューします。
// 送信者がローカルで追跡されている送信者に含まれていない場合は、完全な価格設定の制約が適用されます。
//
// このメソッドは、p2pネットワークからトランザクションを追加するために使用され、
// プールの再編成と内部イベントの伝播を待機しません。
func (pool *TxPool) AddRemotes(txs []*types.Transaction) []error {
	return pool.addTxs(txs, false, false)
}

// This is like AddRemotes, but waits for pool reorganization. Tests use this method.
// これはAddRemotesに似ていますが、プールの再編成を待ちます。 テストではこの方法を使用します。
func (pool *TxPool) AddRemotesSync(txs []*types.Transaction) []error {
	return pool.addTxs(txs, false, true)
}

// This is like AddRemotes with a single transaction, but waits for pool reorganization. Tests use this method.
// これは、単一のトランザクションを持つAddRemotesに似ていますが、プールの再編成を待機します。
// テストではこの方法を使用します。
func (pool *TxPool) addRemoteSync(tx *types.Transaction) error {
	errs := pool.AddRemotesSync([]*types.Transaction{tx})
	return errs[0]
}

// AddRemote enqueues a single transaction into the pool if it is valid. This is a convenience
// wrapper around AddRemotes.
//
// Deprecated: use AddRemotes
// AddRemoteは、有効な場合、単一のトランザクションをプールにエンキューします。
// これは、AddRemoteの便利なラッパーです。
//
// 非推奨：AddRemotesを使用
func (pool *TxPool) AddRemote(tx *types.Transaction) error {
	errs := pool.AddRemotes([]*types.Transaction{tx})
	return errs[0]
}

// addTxs attempts to queue a batch of transactions if they are valid.
// addTxsは、トランザクションが有効な場合、トランザクションのバッチをキューに入れようとします。
func (pool *TxPool) addTxs(txs []*types.Transaction, local, sync bool) []error {
	// Filter out known ones without obtaining the pool lock or recovering signatures
	// プールロックを取得したり、署名を回復したりせずに、既知のものを除外します
	var (
		errs = make([]error, len(txs))
		news = make([]*types.Transaction, 0, len(txs))
	)
	for i, tx := range txs {
		// If the transaction is known, pre-set the error slot
		// トランザクションがわかっている場合は、エラースロットを事前に設定します
		if pool.all.Get(tx.Hash()) != nil {
			errs[i] = ErrAlreadyKnown
			knownTxMeter.Mark(1)
			continue
		}
		// Exclude transactions with invalid signatures as soon as
		// possible and cache senders in transactions before
		// obtaining lock
		// 無効な署名のあるトランザクションをできるだけ早く除外し、
		// ロックを取得する前に送信者をトランザクションにキャッシュします
		_, err := types.Sender(pool.signer, tx)
		if err != nil {
			errs[i] = ErrInvalidSender
			invalidTxMeter.Mark(1)
			continue
		}
		// Accumulate all unknown transactions for deeper processing
		// より深い処理のためにすべての未知のトランザクションを蓄積します
		news = append(news, tx)
	}
	if len(news) == 0 {
		return errs
	}

	// Process all the new transaction and merge any errors into the original slice
	// すべての新しいトランザクションを処理し、エラーを元のスライスにマージします
	pool.mu.Lock()
	newErrs, dirtyAddrs := pool.addTxsLocked(news, local)
	pool.mu.Unlock()

	var nilSlot = 0
	for _, err := range newErrs {
		for errs[nilSlot] != nil {
			nilSlot++
		}
		errs[nilSlot] = err
		nilSlot++
	}
	// Reorg the pool internals if needed and return
	// 必要に応じてプールの内部を再編成し、
	done := pool.requestPromoteExecutables(dirtyAddrs)
	if sync {
		<-done
	}
	return errs
}

// addTxsLocked attempts to queue a batch of transactions if they are valid.
// The transaction pool lock must be held.
// addTxsLockedは、トランザクションが有効な場合、トランザクションのバッチをキューに入れようとします。
// トランザクションプールロックを保持する必要があります。
func (pool *TxPool) addTxsLocked(txs []*types.Transaction, local bool) ([]error, *accountSet) {
	dirty := newAccountSet(pool.signer)
	errs := make([]error, len(txs))
	for i, tx := range txs {
		replaced, err := pool.add(tx, local)
		errs[i] = err
		if err == nil && !replaced {
			dirty.addTx(tx)
		}
	}
	validTxMeter.Mark(int64(len(dirty.accounts)))
	return errs, dirty
}

// Status returns the status (unknown/pending/queued) of a batch of transactions
// identified by their hashes.
// ステータスは、トランザクションのバッチのステータス（不明/保留中/キューに入れられた）を返します
// ハッシュで識別されます。
func (pool *TxPool) Status(hashes []common.Hash) []TxStatus {
	status := make([]TxStatus, len(hashes))
	for i, hash := range hashes {
		tx := pool.Get(hash)
		if tx == nil {
			continue
		}
		from, _ := types.Sender(pool.signer, tx) // すでに検証済み // already validated
		pool.mu.RLock()
		if txList := pool.pending[from]; txList != nil && txList.txs.items[tx.Nonce()] != nil {
			status[i] = TxStatusPending
		} else if txList := pool.queue[from]; txList != nil && txList.txs.items[tx.Nonce()] != nil {
			status[i] = TxStatusQueued
		}
		// implicit else: the tx may have been included into a block between
		// checking pool.Get and obtaining the lock. In that case, TxStatusUnknown is correct
		// 暗黙的なelse：txがpool.Getのチェックとロックの取得の間のブロックに含まれている可能性があります。
		//  その場合、TxStatusUnknownは正しいです
		pool.mu.RUnlock()
	}
	return status
}

// Get returns a transaction if it is contained in the pool and nil otherwise.
// Getは、プールに含まれている場合はトランザクションを返し、含まれていない場合はnilを返します。
func (pool *TxPool) Get(hash common.Hash) *types.Transaction {
	return pool.all.Get(hash)
}

// Has returns an indicator whether txpool has a transaction cached with the
// given hash.
// 指定されたハッシュでキャッシュされたトランザクションがtxpoolにあるかどうかのインジケーターを返します。
func (pool *TxPool) Has(hash common.Hash) bool {
	return pool.all.Get(hash) != nil
}

// removeTx removes a single transaction from the queue, moving all subsequent
// transactions back to the future queue.
// removeTxは、キューから1つのトランザクションを削除し、
// 後続のすべてのトランザクションを将来のキューに戻します。
func (pool *TxPool) removeTx(hash common.Hash, outofbound bool) {
	// Fetch the transaction we wish to delete
	// 削除するトランザクションを取得します
	tx := pool.all.Get(hash)
	if tx == nil {
		return
	}
	addr, _ := types.Sender(pool.signer, tx) // 挿入時にすでに検証済み // already validated during insertion

	// Remove it from the list of known transactions
	// 既知のトランザクションのリストから削除します
	pool.all.Remove(hash)
	if outofbound {
		pool.priced.Removed(1)
	}
	if pool.locals.contains(addr) {
		localGauge.Dec(1)
	}
	// Remove the transaction from the pending lists and reset the account nonce
	// 保留中のリストからトランザクションを削除し、アカウントナンスをリセットします
	if pending := pool.pending[addr]; pending != nil {
		if removed, invalids := pending.Remove(tx); removed {
			// If no more pending transactions are left, remove the list
			// 保留中のトランザクションが残っていない場合は、リストを削除します
			if pending.Empty() {
				delete(pool.pending, addr)
			}
			// Postpone any invalidated transactions
			// 無効なトランザクションを延期します
			for _, tx := range invalids {
				// Internal shuffle shouldn't touch the lookup set.
				// 内部シャッフルはルックアップセットに触れないようにする必要があります。
				pool.enqueueTx(tx.Hash(), tx, false, false)
			}
			// Update the account nonce if needed
			// 必要に応じてアカウントナンスを更新します
			pool.pendingNonces.setIfLower(addr, tx.Nonce())
			// Reduce the pending counter
			// 保留中のカウンターを減らします
			pendingGauge.Dec(int64(1 + len(invalids)))
			return
		}
	}
	// Transaction is in the future queue
	// トランザクションは将来のキューにあります
	if future := pool.queue[addr]; future != nil {
		if removed, _ := future.Remove(tx); removed {
			// Reduce the queued counter
			// キューに入れられたカウンターを減らします
			queuedGauge.Dec(1)
		}
		if future.Empty() {
			delete(pool.queue, addr)
			delete(pool.beats, addr)
		}
	}
}

// requestReset requests a pool reset to the new head block.
// The returned channel is closed when the reset has occurred.
// requestResetは、新しいヘッドブロックへのプールのリセットを要求します。
// リセットが発生すると、返されたチャネルは閉じられます。
func (pool *TxPool) requestReset(oldHead *types.Header, newHead *types.Header) chan struct{} {
	select {
	case pool.reqResetCh <- &txpoolResetRequest{oldHead, newHead}:
		return <-pool.reorgDoneCh
	case <-pool.reorgShutdownCh:
		return pool.reorgShutdownCh
	}
}

// requestPromoteExecutables requests transaction promotion checks for the given addresses.
// The returned channel is closed when the promotion checks have occurred.
// requestPromoteExecutablesは、指定されたアドレスのトランザクションプロモーションチェックを要求します。
// プロモーションチェックが行われると、返されたチャネルは閉じられます。
func (pool *TxPool) requestPromoteExecutables(set *accountSet) chan struct{} {
	select {
	case pool.reqPromoteCh <- set:
		return <-pool.reorgDoneCh
	case <-pool.reorgShutdownCh:
		return pool.reorgShutdownCh
	}
}

// queueTxEvent enqueues a transaction event to be sent in the next reorg run.
// queueTxEventは、次の再編成の実行で送信されるトランザクションイベントをキューに入れます。
func (pool *TxPool) queueTxEvent(tx *types.Transaction) {
	select {
	case pool.queueTxEventCh <- tx:
	case <-pool.reorgShutdownCh:
	}
}

// scheduleReorgLoop schedules runs of reset and promoteExecutables. Code above should not
// call those methods directly, but request them being run using requestReset and
// requestPromoteExecutables instead.
// scheduleReorgLoopは、resetおよびpromoteExecutablesの実行をスケジュールします。
// 上記のコードは、これらのメソッドを直接呼び出すべきではありませんが、
// 代わりにrequestResetとrequestPromoteExecutablesを使用して実行されるように要求します。
func (pool *TxPool) scheduleReorgLoop() {
	defer pool.wg.Done()

	var (
		curDone       chan struct{} // runReorgがアクティブな間はnil以外 // non-nil while runReorg is active
		nextDone      = make(chan struct{})
		launchNextRun bool
		reset         *txpoolResetRequest
		dirtyAccounts *accountSet
		queuedEvents  = make(map[common.Address]*txSortedMap)
	)
	for {
		// Launch next background reorg if needed
		// 必要に応じて、次のバックグラウンド再編成を起動します
		if curDone == nil && launchNextRun {
			// Run the background reorg and announcements
			// バックグラウンドの再編成とアナウンスを実行します
			go pool.runReorg(nextDone, reset, dirtyAccounts, queuedEvents)

			// Prepare everything for the next round of reorg
			// 次の再編成に備えてすべてを準備します
			curDone, nextDone = nextDone, make(chan struct{})
			launchNextRun = false

			reset, dirtyAccounts = nil, nil
			queuedEvents = make(map[common.Address]*txSortedMap)
		}

		select {
		case req := <-pool.reqResetCh:
			// Reset request: update head if request is already pending.
			// リクエストをリセット：リクエストがすでに保留中の場合はヘッドを更新します。
			if reset == nil {
				reset = req
			} else {
				reset.newHead = req.newHead
			}
			launchNextRun = true
			pool.reorgDoneCh <- nextDone

		case req := <-pool.reqPromoteCh:
			// Promote request: update address set if request is already pending.
			// リクエストをプロモートします：リクエストがすでに保留中の場合は、アドレスセットを更新します。
			if dirtyAccounts == nil {
				dirtyAccounts = req
			} else {
				dirtyAccounts.merge(req)
			}
			launchNextRun = true
			pool.reorgDoneCh <- nextDone

		case tx := <-pool.queueTxEventCh:
			// Queue up the event, but don't schedule a reorg. It's up to the caller to
			// request one later if they want the events sent.
			// イベントをキューに入れますが、再編成をスケジュールしないでください。
			// イベントを送信したい場合は、後でリクエストするのは発信者の責任です。
			addr, _ := types.Sender(pool.signer, tx)
			if _, ok := queuedEvents[addr]; !ok {
				queuedEvents[addr] = newTxSortedMap()
			}
			queuedEvents[addr].Put(tx)

		case <-curDone:
			curDone = nil

		case <-pool.reorgShutdownCh:
			// Wait for current run to finish.
			// 現在の実行が終了するのを待ちます。
			if curDone != nil {
				<-curDone
			}
			close(nextDone)
			return
		}
	}
}

// runReorg runs reset and promoteExecutables on behalf of scheduleReorgLoop.
// runReorgは、scheduleReorgLoopに代わってresetおよびpromoteExecutablesを実行します。
func (pool *TxPool) runReorg(done chan struct{}, reset *txpoolResetRequest, dirtyAccounts *accountSet, events map[common.Address]*txSortedMap) {
	defer func(t0 time.Time) {
		reorgDurationTimer.Update(time.Since(t0))
	}(time.Now())
	defer close(done)

	var promoteAddrs []common.Address
	if dirtyAccounts != nil && reset == nil {
		// Only dirty accounts need to be promoted, unless we're resetting.
		// For resets, all addresses in the tx queue will be promoted and
		// the flatten operation can be avoided.
		// リセットしない限り、ダーティアカウントのみをプロモートする必要があります。
		//リセットの場合、txキュー内のすべてのアドレスがプロモートされ、フラット化操作を回避できます。
		promoteAddrs = dirtyAccounts.flatten()
	}
	pool.mu.Lock()
	if reset != nil {
		// Reset from the old head to the new, rescheduling any reorged transactions
		// 古いヘッドから新しいヘッドにリセットし、再編成されたトランザクションを再スケジュールします
		pool.reset(reset.oldHead, reset.newHead)

		// Nonces were reset, discard any events that became stale
		// ノンスはリセットされ、古くなったイベントはすべて破棄されます
		for addr := range events {
			events[addr].Forward(pool.pendingNonces.get(addr))
			if events[addr].Len() == 0 {
				delete(events, addr)
			}
		}
		// Reset needs promote for all addresses
		// リセットはすべてのアドレスに対してプロモートする必要があります
		promoteAddrs = make([]common.Address, 0, len(pool.queue))
		for addr := range pool.queue {
			promoteAddrs = append(promoteAddrs, addr)
		}
	}
	// Check for pending transactions for every account that sent new ones
	// 新しいアカウントを送信したすべてのアカウントの保留中のトランザクションを確認します
	promoted := pool.promoteExecutables(promoteAddrs)

	// If a new block appeared, validate the pool of pending transactions. This will
	// remove any transaction that has been included in the block or was invalidated
	// because of another transaction (e.g. higher gas price).
	// 新しいブロックが表示された場合は、保留中のトランザクションのプールを検証します。
	// これにより、ブロックに含まれているトランザクション、または別のトランザクション（ガス価格の上昇など）のために無効にされたトランザクションが削除されます。
	if reset != nil {
		pool.demoteUnexecutables()
		if reset.newHead != nil && pool.chainconfig.IsLondon(new(big.Int).Add(reset.newHead.Number, big.NewInt(1))) {
			pendingBaseFee := misc.CalcBaseFee(pool.chainconfig, reset.newHead)
			pool.priced.SetBaseFee(pendingBaseFee)
		}
		// Update all accounts to the latest known pending nonce
		// すべてのアカウントを最新の既知の保留中のナンスに更新します
		nonces := make(map[common.Address]uint64, len(pool.pending))
		for addr, list := range pool.pending {
			highestPending := list.LastElement()
			nonces[addr] = highestPending.Nonce() + 1
		}
		pool.pendingNonces.setAll(nonces)
	}
	// Ensure pool.queue and pool.pending sizes stay within the configured limits.
	// pool.queueとpool.pendingのサイズが設定された制限内にあることを確認します。
	pool.truncatePending()
	pool.truncateQueue()

	dropBetweenReorgHistogram.Update(int64(pool.changesSinceReorg))
	pool.changesSinceReorg = 0 // 変更カウンターをリセットします // Reset change counter
	pool.mu.Unlock()

	// Notify subsystems for newly added transactions
	// 新しく追加されたトランザクションについてサブシステムに通知します
	for _, tx := range promoted {
		addr, _ := types.Sender(pool.signer, tx)
		if _, ok := events[addr]; !ok {
			events[addr] = newTxSortedMap()
		}
		events[addr].Put(tx)
	}
	if len(events) > 0 {
		var txs []*types.Transaction
		for _, set := range events {
			txs = append(txs, set.Flatten()...)
		}
		pool.txFeed.Send(NewTxsEvent{txs})
	}
}

// reset retrieves the current state of the blockchain and ensures the content
// of the transaction pool is valid with regard to the chain state.
// resetは、ブロックチェーンの現在の状態を取得し、
// トランザクションプールのコンテンツがチェーンの状態に関して有効であることを確認します。
func (pool *TxPool) reset(oldHead, newHead *types.Header) {
	// If we're reorging an old state, reinject all dropped transactions
	// 古い状態を再編成する場合は、ドロップされたすべてのトランザクションを再注入します
	var reinject types.Transactions

	if oldHead != nil && oldHead.Hash() != newHead.ParentHash {
		// If the reorg is too deep, avoid doing it (will happen during fast sync)
		// 再編成が深すぎる場合は、実行を避けてください（高速同期中に発生します）
		oldNum := oldHead.Number.Uint64()
		newNum := newHead.Number.Uint64()

		if depth := uint64(math.Abs(float64(oldNum) - float64(newNum))); depth > 64 {
			log.Debug("Skipping deep transaction reorg", "depth", depth)
		} else {
			// Reorg seems shallow enough to pull in all transactions into memory
			// Reorgは、すべてのトランザクションをメモリに取り込むのに十分浅いようです
			var discarded, included types.Transactions
			var (
				rem = pool.chain.GetBlock(oldHead.Hash(), oldHead.Number.Uint64())
				add = pool.chain.GetBlock(newHead.Hash(), newHead.Number.Uint64())
			)
			if rem == nil {
				// This can happen if a setHead is performed, where we simply discard the old
				// head from the chain.
				// If that is the case, we don't have the lost transactions any more, and
				// there's nothing to add
				// これは、setHeadが実行された場合に発生する可能性があります。
				// この場合、チェーンから古いヘッドを破棄するだけです。
				// その場合、失われたトランザクションはもうなく、追加するものはありません
				if newNum >= oldNum {
					// If we reorged to a same or higher number, then it's not a case of setHead
					// 同じかそれ以上の数に再編成した場合、setHeadの場合ではありません
					log.Warn("Transaction pool reset with missing oldhead",
						"old", oldHead.Hash(), "oldnum", oldNum, "new", newHead.Hash(), "newnum", newNum)
					return
				}
				// If the reorg ended up on a lower number, it's indicative of setHead being the cause
				// reorgの数値が低くなった場合は、setHeadが原因であることを示しています
				log.Debug("Skipping transaction reset caused by setHead",
					"old", oldHead.Hash(), "oldnum", oldNum, "new", newHead.Hash(), "newnum", newNum)
				// We still need to update the current state s.th. the lost transactions can be readded by the user
				// 現在の状態s.thを更新する必要があります。 失われたトランザクションはユーザーが再読み込みできます
			} else {
				for rem.NumberU64() > add.NumberU64() {
					discarded = append(discarded, rem.Transactions()...)
					if rem = pool.chain.GetBlock(rem.ParentHash(), rem.NumberU64()-1); rem == nil {
						log.Error("Unrooted old chain seen by tx pool", "block", oldHead.Number, "hash", oldHead.Hash())
						return
					}
				}
				for add.NumberU64() > rem.NumberU64() {
					included = append(included, add.Transactions()...)
					if add = pool.chain.GetBlock(add.ParentHash(), add.NumberU64()-1); add == nil {
						log.Error("Unrooted new chain seen by tx pool", "block", newHead.Number, "hash", newHead.Hash())
						return
					}
				}
				for rem.Hash() != add.Hash() {
					discarded = append(discarded, rem.Transactions()...)
					if rem = pool.chain.GetBlock(rem.ParentHash(), rem.NumberU64()-1); rem == nil {
						log.Error("Unrooted old chain seen by tx pool", "block", oldHead.Number, "hash", oldHead.Hash())
						return
					}
					included = append(included, add.Transactions()...)
					if add = pool.chain.GetBlock(add.ParentHash(), add.NumberU64()-1); add == nil {
						log.Error("Unrooted new chain seen by tx pool", "block", newHead.Number, "hash", newHead.Hash())
						return
					}
				}
				reinject = types.TxDifference(discarded, included)
			}
		}
	}
	// Initialize the internal state to the current head
	// 内部状態を現在のヘッドに初期化します
	if newHead == nil {
		newHead = pool.chain.CurrentBlock().Header() // テスト中の特殊なケース // Special case during testing
	}
	statedb, err := pool.chain.StateAt(newHead.Root)
	if err != nil {
		log.Error("Failed to reset txpool state", "err", err)
		return
	}
	pool.currentState = statedb
	pool.pendingNonces = newTxNoncer(statedb)
	pool.currentMaxGas = newHead.GasLimit

	// Inject any transactions discarded due to reorgs
	// 再編成のために破棄されたトランザクションを挿入します
	log.Debug("Reinjecting stale transactions", "count", len(reinject))
	senderCacher.recover(pool.signer, reinject)
	pool.addTxsLocked(reinject, false)

	// Update all fork indicator by next pending block number.
	// 次の保留中のブロック番号ですべてのフォークインジケーターを更新します。
	next := new(big.Int).Add(newHead.Number, big.NewInt(1))
	pool.istanbul = pool.chainconfig.IsIstanbul(next)
	pool.eip2718 = pool.chainconfig.IsBerlin(next)
	pool.eip1559 = pool.chainconfig.IsLondon(next)
}

// promoteExecutables moves transactions that have become processable from the
// future queue to the set of pending transactions. During this process, all
// invalidated transactions (low nonce, low balance) are deleted.
// PromoteExecutablesは、処理可能になったトランザクションを将来のキューから保留中のトランザクションのセットに移動します。
// このプロセス中に、無効化されたすべてのトランザクション（低ナンス、低残高）が削除されます。
func (pool *TxPool) promoteExecutables(accounts []common.Address) []*types.Transaction {
	// Track the promoted transactions to broadcast them at once
	// プロモートされたトランザクションを追跡して、一度にブロードキャストします
	var promoted []*types.Transaction

	// Iterate over all accounts and promote any executable transactions
	// すべてのアカウントを繰り返し、実行可能なトランザクションを促進します
	for _, addr := range accounts {
		list := pool.queue[addr]
		if list == nil {
			continue // 誰かが存在しないアカウントで電話をかけた場合に備えて // Just in case someone calls with a non existing account
		}
		// Drop all transactions that are deemed too old (low nonce)
		// 古すぎると見なされるすべてのトランザクションを削除します（ナンスが低い）
		forwards := list.Forward(pool.currentState.GetNonce(addr))
		for _, tx := range forwards {
			hash := tx.Hash()
			pool.all.Remove(hash)
		}
		log.Trace("Removed old queued transactions", "count", len(forwards))
		// Drop all transactions that are too costly (low balance or out of gas)
		// コストが高すぎる（残高が少ない、またはガスが不足している）すべてのトランザクションを削除します
		drops, _ := list.Filter(pool.currentState.GetBalance(addr), pool.currentMaxGas)
		for _, tx := range drops {
			hash := tx.Hash()
			pool.all.Remove(hash)
		}
		log.Trace("Removed unpayable queued transactions", "count", len(drops))
		queuedNofundsMeter.Mark(int64(len(drops)))

		// Gather all executable transactions and promote them
		// 実行可能なすべてのトランザクションを収集し、それらをプロモートします
		readies := list.Ready(pool.pendingNonces.get(addr))
		for _, tx := range readies {
			hash := tx.Hash()
			if pool.promoteTx(addr, hash, tx) {
				promoted = append(promoted, tx)
			}
		}
		log.Trace("Promoted queued transactions", "count", len(promoted))
		queuedGauge.Dec(int64(len(readies)))

		// Drop all transactions over the allowed limit
		// 許可された制限を超えるすべてのトランザクションを削除します
		var caps types.Transactions
		if !pool.locals.contains(addr) {
			caps = list.Cap(int(pool.config.AccountQueue))
			for _, tx := range caps {
				hash := tx.Hash()
				pool.all.Remove(hash)
				log.Trace("Removed cap-exceeding queued transaction", "hash", hash)
			}
			queuedRateLimitMeter.Mark(int64(len(caps)))
		}
		// Mark all the items dropped as removed
		// 削除されたすべてのアイテムを削除済みとしてマークします
		pool.priced.Removed(len(forwards) + len(drops) + len(caps))
		queuedGauge.Dec(int64(len(forwards) + len(drops) + len(caps)))
		if pool.locals.contains(addr) {
			localGauge.Dec(int64(len(forwards) + len(drops) + len(caps)))
		}
		// Delete the entire queue entry if it became empty.
		// キューエントリが空になった場合は、キューエントリ全体を削除します。
		if list.Empty() {
			delete(pool.queue, addr)
			delete(pool.beats, addr)
		}
	}
	return promoted
}

// truncatePending removes transactions from the pending queue if the pool is above the
// pending limit. The algorithm tries to reduce transaction counts by an approximately
// equal number for all for accounts with many pending transactions.
// プールが保留中の制限を超えている場合、truncatePendingは保留中のキューからトランザクションを削除します。
// アルゴリズムは、保留中のトランザクションが多数あるアカウントのすべてについて、
// トランザクション数をほぼ同じ数だけ削減しようとします。
func (pool *TxPool) truncatePending() {
	pending := uint64(0)
	for _, list := range pool.pending {
		pending += uint64(list.Len())
	}
	if pending <= pool.config.GlobalSlots {
		return
	}

	pendingBeforeCap := pending
	// Assemble a spam order to penalize large transactors first
	// スパム注文を集めて、最初に大規模な取引者にペナルティを科します
	spammers := prque.New(nil)
	for addr, list := range pool.pending {
		// Only evict transactions from high rollers
		// ハイローラーからのトランザクションのみを削除します
		if !pool.locals.contains(addr) && uint64(list.Len()) > pool.config.AccountSlots {
			spammers.Push(addr, int64(list.Len()))
		}
	}
	// Gradually drop transactions from offenders
	// 違反者からのトランザクションを徐々に削除します
	offenders := []common.Address{}
	for pending > pool.config.GlobalSlots && !spammers.Empty() {
		// Retrieve the next offender if not local address
		// ローカルアドレスでない場合は、次の違反者を取得します
		offender, _ := spammers.Pop()
		offenders = append(offenders, offender.(common.Address))

		// Equalize balances until all the same or below threshold
		// すべてが同じか、しきい値を下回るまで残高を均等化します
		if len(offenders) > 1 {
			// Calculate the equalization threshold for all current offenders
			// 現在のすべての違反者の均等化しきい値を計算します
			threshold := pool.pending[offender.(common.Address)].Len()

			// Iteratively reduce all offenders until below limit or threshold reached
			// 制限またはしきい値を下回るまで、すべての違反者を繰り返し減らします
			for pending > pool.config.GlobalSlots && pool.pending[offenders[len(offenders)-2]].Len() > threshold {
				for i := 0; i < len(offenders)-1; i++ {
					list := pool.pending[offenders[i]]

					caps := list.Cap(list.Len() - 1)
					for _, tx := range caps {
						// Drop the transaction from the global pools too
						// グローバルプールからもトランザクションを削除します
						hash := tx.Hash()
						pool.all.Remove(hash)

						// Update the account nonce to the dropped transaction
						// アカウントナンスをドロップされたトランザクションに更新します
						pool.pendingNonces.setIfLower(offenders[i], tx.Nonce())
						log.Trace("Removed fairness-exceeding pending transaction", "hash", hash)
					}
					pool.priced.Removed(len(caps))
					pendingGauge.Dec(int64(len(caps)))
					if pool.locals.contains(offenders[i]) {
						localGauge.Dec(int64(len(caps)))
					}
					pending--
				}
			}
		}
	}

	// If still above threshold, reduce to limit or min allowance
	// それでもしきい値を超えている場合は、制限または最小許容値に減らします
	if pending > pool.config.GlobalSlots && len(offenders) > 0 {
		for pending > pool.config.GlobalSlots && uint64(pool.pending[offenders[len(offenders)-1]].Len()) > pool.config.AccountSlots {
			for _, addr := range offenders {
				list := pool.pending[addr]

				caps := list.Cap(list.Len() - 1)
				for _, tx := range caps {
					// Drop the transaction from the global pools too
					// グローバルプールからもトランザクションを削除します
					hash := tx.Hash()
					pool.all.Remove(hash)

					// Update the account nonce to the dropped transaction
					//アカウントナンスをドロップされたトランザクションに更新します
					pool.pendingNonces.setIfLower(addr, tx.Nonce())
					log.Trace("Removed fairness-exceeding pending transaction", "hash", hash)
				}
				pool.priced.Removed(len(caps))
				pendingGauge.Dec(int64(len(caps)))
				if pool.locals.contains(addr) {
					localGauge.Dec(int64(len(caps)))
				}
				pending--
			}
		}
	}
	pendingRateLimitMeter.Mark(int64(pendingBeforeCap - pending))
}

// truncateQueue drops the oldes transactions in the queue if the pool is above the global queue limit.
// プールがグローバルキュー制限を超えている場合、キューを切り捨ててキュー内の最も古いトランザクションを削除します。
func (pool *TxPool) truncateQueue() {
	queued := uint64(0)
	for _, list := range pool.queue {
		queued += uint64(list.Len())
	}
	if queued <= pool.config.GlobalQueue {
		return
	}

	// Sort all accounts with queued transactions by heartbeat
	// キューに入れられたトランザクションを持つすべてのアカウントをハートビートで並べ替えます
	addresses := make(addressesByHeartbeat, 0, len(pool.queue))
	for addr := range pool.queue {
		if !pool.locals.contains(addr) { // 地元の人を落とさない // don't drop locals
			addresses = append(addresses, addressByHeartbeat{addr, pool.beats[addr]})
		}
	}
	sort.Sort(addresses)

	// Drop transactions until the total is below the limit or only locals remain
	// 合計が制限を下回るか、ローカルのみが残るまでトランザクションをドロップします
	for drop := queued - pool.config.GlobalQueue; drop > 0 && len(addresses) > 0; {
		addr := addresses[len(addresses)-1]
		list := pool.queue[addr.address]

		addresses = addresses[:len(addresses)-1]

		// Drop all transactions if they are less than the overflow
		// オーバーフローよりも少ない場合は、すべてのトランザクションを削除します
		if size := uint64(list.Len()); size <= drop {
			for _, tx := range list.Flatten() {
				pool.removeTx(tx.Hash(), true)
			}
			drop -= size
			queuedRateLimitMeter.Mark(int64(size))
			continue
		}
		// Otherwise drop only last few transactions
		// それ以外の場合は、最後の数トランザクションのみをドロップします
		txs := list.Flatten()
		for i := len(txs) - 1; i >= 0 && drop > 0; i-- {
			pool.removeTx(txs[i].Hash(), true)
			drop--
			queuedRateLimitMeter.Mark(1)
		}
	}
}

// demoteUnexecutables removes invalid and processed transactions from the pools
// executable/pending queue and any subsequent transactions that become unexecutable
// are moved back into the future queue.
//
// Note: transactions are not marked as removed in the priced list because re-heaping
// is always explicitly triggered by SetBaseFee and it would be unnecessary and wasteful
// to trigger a re-heap is this function
// demoteUnexecutablesは、無効で処理されたトランザクションをプールの実行可能/保留キューから削除し、
// 実行不能になった後続のトランザクションはすべて、将来のキューに戻されます。
//
// 注：再ヒープは常にSetBaseFeeによって明示的にトリガーされ、再ヒープをトリガーする必要はなく、
// 無駄になるため、トランザクションは価格表で削除済みとしてマークされません。この関数です
func (pool *TxPool) demoteUnexecutables() {
	// Iterate over all accounts and demote any non-executable transactions
	// すべてのアカウントを繰り返し処理し、実行不可能なトランザクションを降格します
	for addr, list := range pool.pending {
		nonce := pool.currentState.GetNonce(addr)

		// Drop all transactions that are deemed too old (low nonce)
		// 古すぎると見なされるすべてのトランザクションを削除します（ナンスが低い）
		olds := list.Forward(nonce)
		for _, tx := range olds {
			hash := tx.Hash()
			pool.all.Remove(hash)
			log.Trace("Removed old pending transaction", "hash", hash)
		}
		// Drop all transactions that are too costly (low balance or out of gas), and queue any invalids back for later
		// コストが高すぎる（残高が少ない、またはガスが不足している）トランザクションをすべて削除し、無効なトランザクションを後でキューに入れます
		drops, invalids := list.Filter(pool.currentState.GetBalance(addr), pool.currentMaxGas)
		for _, tx := range drops {
			hash := tx.Hash()
			log.Trace("Removed unpayable pending transaction", "hash", hash)
			pool.all.Remove(hash)
		}
		pendingNofundsMeter.Mark(int64(len(drops)))

		for _, tx := range invalids {
			hash := tx.Hash()
			log.Trace("Demoting pending transaction", "hash", hash)

			// Internal shuffle shouldn't touch the lookup set.
			// 内部シャッフルはルックアップセットに触れないようにする必要があります。
			pool.enqueueTx(hash, tx, false, false)
		}
		pendingGauge.Dec(int64(len(olds) + len(drops) + len(invalids)))
		if pool.locals.contains(addr) {
			localGauge.Dec(int64(len(olds) + len(drops) + len(invalids)))
		}
		// If there's a gap in front, alert (should never happen) and postpone all transactions
		// 前にギャップがある場合は、警告し（決して発生しないはずです）、すべてのトランザクションを延期します
		if list.Len() > 0 && list.txs.Get(nonce) == nil {
			gapped := list.Cap(0)
			for _, tx := range gapped {
				hash := tx.Hash()
				log.Error("Demoting invalidated transaction", "hash", hash)

				// Internal shuffle shouldn't touch the lookup set.
				// 内部シャッフルはルックアップセットに触れないようにする必要があります。
				pool.enqueueTx(hash, tx, false, false)
			}
			pendingGauge.Dec(int64(len(gapped)))
			// This might happen in a reorg, so log it to the metering
			// これは再編成で発生する可能性があるため、メータリングに記録します
			blockReorgInvalidatedTx.Mark(int64(len(gapped)))
		}
		// Delete the entire pending entry if it became empty.
		// 保留中のエントリが空になった場合は、エントリ全体を削除します。
		if list.Empty() {
			delete(pool.pending, addr)
		}
	}
}

// addressByHeartbeat is an account address tagged with its last activity timestamp.
// addressByHeartbeatは、最後のアクティビティタイムスタンプでタグ付けされたアカウントアドレスです。
type addressByHeartbeat struct {
	address   common.Address
	heartbeat time.Time
}

type addressesByHeartbeat []addressByHeartbeat

func (a addressesByHeartbeat) Len() int           { return len(a) }
func (a addressesByHeartbeat) Less(i, j int) bool { return a[i].heartbeat.Before(a[j].heartbeat) }
func (a addressesByHeartbeat) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

// accountSet is simply a set of addresses to check for existence, and a signer
// capable of deriving addresses from transactions.
// accountSetは、存在を確認するためのアドレスのセットであり、
// トランザクションからアドレスを取得できる署名者です。
type accountSet struct {
	accounts map[common.Address]struct{}
	signer   types.Signer
	cache    *[]common.Address
}

// newAccountSet creates a new address set with an associated signer for sender
// derivations.
// newAccountSetは、送信者の派生に関連付けられた署名者を含む新しいアドレスセットを作成します。
func newAccountSet(signer types.Signer, addrs ...common.Address) *accountSet {
	as := &accountSet{
		accounts: make(map[common.Address]struct{}),
		signer:   signer,
	}
	for _, addr := range addrs {
		as.add(addr)
	}
	return as
}

// contains checks if a given address is contained within the set.
// 指定されたアドレスがセット内に含まれているかどうかのチェックが含まれます。
func (as *accountSet) contains(addr common.Address) bool {
	_, exist := as.accounts[addr]
	return exist
}

func (as *accountSet) empty() bool {
	return len(as.accounts) == 0
}

// containsTx checks if the sender of a given tx is within the set. If the sender
// cannot be derived, this method returns false.
// containsTxは、指定されたtxの送信者がセット内にあるかどうかをチェックします。
// 送信者を導出できない場合、このメソッドはfalseを返します。
func (as *accountSet) containsTx(tx *types.Transaction) bool {
	if addr, err := types.Sender(as.signer, tx); err == nil {
		return as.contains(addr)
	}
	return false
}

// add inserts a new address into the set to track.
// addは、追跡するセットに新しいアドレスを挿入します。
func (as *accountSet) add(addr common.Address) {
	as.accounts[addr] = struct{}{}
	as.cache = nil
}

// addTx adds the sender of tx into the set.
// addTxは、txの送信者をセットに追加します。
func (as *accountSet) addTx(tx *types.Transaction) {
	if addr, err := types.Sender(as.signer, tx); err == nil {
		as.add(addr)
	}
}

// flatten returns the list of addresses within this set, also caching it for later
// reuse. The returned slice should not be changed!
// flattenは、このセット内のアドレスのリストを返し、後で再利用するためにキャッシュします。
// 返されたスライスは変更しないでください。
func (as *accountSet) flatten() []common.Address {
	if as.cache == nil {
		accounts := make([]common.Address, 0, len(as.accounts))
		for account := range as.accounts {
			accounts = append(accounts, account)
		}
		as.cache = &accounts
	}
	return *as.cache
}

// merge adds all addresses from the 'other' set into 'as'.
// mergeは、「other」セットのすべてのアドレスを「as」に追加します。
func (as *accountSet) merge(other *accountSet) {
	for addr := range other.accounts {
		as.accounts[addr] = struct{}{}
	}
	as.cache = nil
}

// txLookup is used internally by TxPool to track transactions while allowing
// lookup without mutex contention.
//
// Note, although this type is properly protected against concurrent access, it
// is **not** a type that should ever be mutated or even exposed outside of the
// transaction pool, since its internal state is tightly coupled with the pools
// internal mechanisms. The sole purpose of the type is to permit out-of-bound
// peeking into the pool in TxPool.Get without having to acquire the widely scoped
// TxPool.mu mutex.
//
// This lookup set combines the notion of "local transactions", which is useful
// to build upper-level structure.

// txLookupは、ミューテックスの競合なしにルックアップを許可しながらトランザクションを
// 追跡するためにTxPoolによって内部的に使用されます。
//
// このタイプは同時アクセスから適切に保護されていますが、内部状態がプールの内部メカニズムと
// 緊密に結合されているため、変更したり、トランザクションプールの外部に公開したりする必要のあるタイプではありません。
// このタイプの唯一の目的は、TxPool.Getのプールを範囲外で覗き見できるようにすることであり、
// スコープの広いTxPool.muミューテックスを取得する必要はありません。
//
// このルックアップセットは、上位レベルの構造を構築するのに役立つ「ローカルトランザクション」の概念を組み合わせたものです。
type txLookup struct {
	slots   int
	lock    sync.RWMutex
	locals  map[common.Hash]*types.Transaction
	remotes map[common.Hash]*types.Transaction
}

// newTxLookup returns a new txLookup structure.
// newTxLookupは新しいtxLookup構造を返します。
func newTxLookup() *txLookup {
	return &txLookup{
		locals:  make(map[common.Hash]*types.Transaction),
		remotes: make(map[common.Hash]*types.Transaction),
	}
}

// Range calls f on each key and value present in the map. The callback passed
// should return the indicator whether the iteration needs to be continued.
// Callers need to specify which set (or both) to be iterated.
// Rangeは、マップに存在する各キーと値に対してfを呼び出します。
// 渡されたコールバックは、反復を続行する必要があるかどうかのインジケーターを返す必要があります。
// 呼び出し元は、反復するセット（または両方）を指定する必要があります。
func (t *txLookup) Range(f func(hash common.Hash, tx *types.Transaction, local bool) bool, local bool, remote bool) {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if local {
		for key, value := range t.locals {
			if !f(key, value, true) {
				return
			}
		}
	}
	if remote {
		for key, value := range t.remotes {
			if !f(key, value, false) {
				return
			}
		}
	}
}

// Get returns a transaction if it exists in the lookup, or nil if not found.
// Getは、ルックアップにトランザクションが存在する場合はトランザクションを返し、見つからない場合はnilを返します。
func (t *txLookup) Get(hash common.Hash) *types.Transaction {
	t.lock.RLock()
	defer t.lock.RUnlock()

	if tx := t.locals[hash]; tx != nil {
		return tx
	}
	return t.remotes[hash]
}

// GetLocal returns a transaction if it exists in the lookup, or nil if not found.
// GetLocalは、ルックアップにトランザクションが存在する場合はトランザクションを返し、見つからない場合はnilを返します。
func (t *txLookup) GetLocal(hash common.Hash) *types.Transaction {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.locals[hash]
}

// GetRemote returns a transaction if it exists in the lookup, or nil if not found.
// GetRemoteは、ルックアップにトランザクションが存在する場合はトランザクションを返し、見つからない場合はnilを返します。
func (t *txLookup) GetRemote(hash common.Hash) *types.Transaction {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.remotes[hash]
}

// Count returns the current number of transactions in the lookup.
// Countは、ルックアップ内の現在のトランザクション数を返します。
func (t *txLookup) Count() int {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return len(t.locals) + len(t.remotes)
}

// LocalCount returns the current number of local transactions in the lookup.
// LocalCountは、ルックアップ内のローカルトランザクションの現在の数を返します。
func (t *txLookup) LocalCount() int {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return len(t.locals)
}

// RemoteCount returns the current number of remote transactions in the lookup.
// RemoteCountは、ルックアップ内のリモートトランザクションの現在の数を返します。
func (t *txLookup) RemoteCount() int {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return len(t.remotes)
}

// Slots returns the current number of slots used in the lookup.
// Slotsは、ルックアップで使用されている現在のスロット数を返します。
func (t *txLookup) Slots() int {
	t.lock.RLock()
	defer t.lock.RUnlock()

	return t.slots
}

// Add adds a transaction to the lookup.
// Addは、ルックアップにトランザクションを追加します。
func (t *txLookup) Add(tx *types.Transaction, local bool) {
	t.lock.Lock()
	defer t.lock.Unlock()

	t.slots += numSlots(tx)
	slotsGauge.Update(int64(t.slots))

	if local {
		t.locals[tx.Hash()] = tx
	} else {
		t.remotes[tx.Hash()] = tx
	}
}

// Remove removes a transaction from the lookup.
// 削除すると、ルックアップからトランザクションが削除されます。
func (t *txLookup) Remove(hash common.Hash) {
	t.lock.Lock()
	defer t.lock.Unlock()

	tx, ok := t.locals[hash]
	if !ok {
		tx, ok = t.remotes[hash]
	}
	if !ok {
		log.Error("No transaction found to be deleted", "hash", hash)
		return
	}
	t.slots -= numSlots(tx)
	slotsGauge.Update(int64(t.slots))

	delete(t.locals, hash)
	delete(t.remotes, hash)
}

// RemoteToLocals migrates the transactions belongs to the given locals to locals
// set. The assumption is held the locals set is thread-safe to be used.
// RemoteToLocalsは、指定されたローカルに属するトランザクションをローカルセットに移行します。
// ローカルセットはスレッドセーフに使用できると想定されています。
func (t *txLookup) RemoteToLocals(locals *accountSet) int {
	t.lock.Lock()
	defer t.lock.Unlock()

	var migrated int
	for hash, tx := range t.remotes {
		if locals.containsTx(tx) {
			t.locals[hash] = tx
			delete(t.remotes, hash)
			migrated += 1
		}
	}
	return migrated
}

// RemotesBelowTip finds all remote transactions below the given tip threshold.
// RemotesBelowTipは、指定されたチップしきい値を下回るすべてのリモートトランザクションを検索します。
func (t *txLookup) RemotesBelowTip(threshold *big.Int) types.Transactions {
	found := make(types.Transactions, 0, 128)
	t.Range(func(hash common.Hash, tx *types.Transaction, local bool) bool {
		if tx.GasTipCapIntCmp(threshold) < 0 {
			found = append(found, tx)
		}
		return true
	}, false, true) // リモートのみを反復します // Only iterate remotes
	return found
}

// numSlots calculates the number of slots needed for a single transaction.
// numSlotsは、単一のトランザクションに必要なスロットの数を計算します。
func numSlots(tx *types.Transaction) int {
	return int((tx.Size() + txSlotSize - 1) / txSlotSize)
}
