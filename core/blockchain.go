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

// Package core implements the Ethereum consensus protocol.
// パッケージコアは、イーサリアムコンセンサスプロトコルを実装します
package core

import (
	"errors"
	"fmt"
	"io"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/common/prque"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/state/snapshot"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/internal/syncx"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	lru "github.com/hashicorp/golang-lru"
)

var (
	headBlockGauge     = metrics.NewRegisteredGauge("chain/head/block", nil)
	headHeaderGauge    = metrics.NewRegisteredGauge("chain/head/header", nil)
	headFastBlockGauge = metrics.NewRegisteredGauge("chain/head/receipt", nil)

	accountReadTimer   = metrics.NewRegisteredTimer("chain/account/reads", nil)
	accountHashTimer   = metrics.NewRegisteredTimer("chain/account/hashes", nil)
	accountUpdateTimer = metrics.NewRegisteredTimer("chain/account/updates", nil)
	accountCommitTimer = metrics.NewRegisteredTimer("chain/account/commits", nil)

	storageReadTimer   = metrics.NewRegisteredTimer("chain/storage/reads", nil)
	storageHashTimer   = metrics.NewRegisteredTimer("chain/storage/hashes", nil)
	storageUpdateTimer = metrics.NewRegisteredTimer("chain/storage/updates", nil)
	storageCommitTimer = metrics.NewRegisteredTimer("chain/storage/commits", nil)

	snapshotAccountReadTimer = metrics.NewRegisteredTimer("chain/snapshot/account/reads", nil)
	snapshotStorageReadTimer = metrics.NewRegisteredTimer("chain/snapshot/storage/reads", nil)
	snapshotCommitTimer      = metrics.NewRegisteredTimer("chain/snapshot/commits", nil)

	blockInsertTimer     = metrics.NewRegisteredTimer("chain/inserts", nil)
	blockValidationTimer = metrics.NewRegisteredTimer("chain/validation", nil)
	blockExecutionTimer  = metrics.NewRegisteredTimer("chain/execution", nil)
	blockWriteTimer      = metrics.NewRegisteredTimer("chain/write", nil)

	blockReorgMeter         = metrics.NewRegisteredMeter("chain/reorg/executes", nil)
	blockReorgAddMeter      = metrics.NewRegisteredMeter("chain/reorg/add", nil)
	blockReorgDropMeter     = metrics.NewRegisteredMeter("chain/reorg/drop", nil)
	blockReorgInvalidatedTx = metrics.NewRegisteredMeter("chain/reorg/invalidTx", nil)

	blockPrefetchExecuteTimer   = metrics.NewRegisteredTimer("chain/prefetch/executes", nil)
	blockPrefetchInterruptMeter = metrics.NewRegisteredMeter("chain/prefetch/interrupts", nil)

	errInsertionInterrupted = errors.New("insertion is interrupted")
	errChainStopped         = errors.New("blockchain is stopped")
)

const (
	bodyCacheLimit      = 256
	blockCacheLimit     = 256
	receiptsCacheLimit  = 32
	txLookupCacheLimit  = 1024
	maxFutureBlocks     = 256
	maxTimeFutureBlocks = 30
	TriesInMemory       = 128

	// BlockChainVersion ensures that an incompatible database forces a resync from scratch.
	//
	// Changelog:
	//
	// - Version 4
	//   The following incompatible database changes were added:
	//   * the `BlockNumber`, `TxHash`, `TxIndex`, `BlockHash` and `Index` fields of log are deleted
	//   * the `Bloom` field of receipt is deleted
	//   * the `BlockIndex` and `TxIndex` fields of txlookup are deleted
	// - Version 5
	//  The following incompatible database changes were added:
	//    * the `TxHash`, `GasCost`, and `ContractAddress` fields are no longer stored for a receipt
	//    * the `TxHash`, `GasCost`, and `ContractAddress` fields are computed by looking up the
	//      receipts' corresponding block
	// - Version 6
	//  The following incompatible database changes were added:
	//    * Transaction lookup information stores the corresponding block number instead of block hash
	// - Version 7
	//  The following incompatible database changes were added:
	//    * Use freezer as the ancient database to maintain all ancient data
	// - Version 8
	//  The following incompatible database changes were added:
	//    * New scheme for contract code in order to separate the codes and trie nodes
	// BlockChainVersionは、互換性のないデータベースが最初から再同期を強制することを保証します。
	//
	// 変更ログ：
	//
	// -バージョン4
	// 次の互換性のないデータベースの変更が追加されました：
	//  *ログの `BlockNumber`、` TxHash`、 `TxIndex`、` BlockHash`、および `Index`フィールドが削除されます
	//  *領収書の `Bloom`フィールドが削除されます
	//  * txlookupの `BlockIndex`フィールドと` TxIndex`フィールドが削除されます
	// -バージョン5
	// 次の互換性のないデータベースの変更が追加されました：
	//  * `TxHash`、` GasCost`、および `ContractAddress`フィールドは、領収書用に保存されなくなりました
	//  * `TxHash`、` GasCost`、および `ContractAddress`フィールドは、
	// 領収書の対応するブロック
	// -バージョン6
	// 次の互換性のないデータベースの変更が追加されました：
	//  *トランザクションルックアップ情報は、ブロックハッシュの代わりに対応するブロック番号を格納します
	// -バージョン7
	// 次の互換性のないデータベースの変更が追加されました：
	//  *すべての古代データを維持するために、古代データベースとして冷凍庫を使用する
	// -バージョン8
	// 次の互換性のないデータベースの変更が追加されました：
	//  *コードとトライノードを分離するためのコントラクトコードの新しいスキーム
	BlockChainVersion uint64 = 8
)

// CacheConfig contains the configuration values for the trie caching/pruning
// that's resident in a blockchain.
// CacheConfigには、ブロックチェーンに常駐するトライキャッシング/プルーニングの構成値が含まれています。
type CacheConfig struct {
	TrieCleanLimit      int           // トライノードをメモリにキャッシュするために使用するメモリアローワンス（MB）            // Memory allowance (MB) to use for caching trie nodes in memory
	TrieCleanJournal    string        // クリーンキャッシュエントリを保存するためのディスクジャーナル。                       // Disk journal for saving clean cache entries.
	TrieCleanRejournal  time.Duration // クリーンキャッシュをディスクに定期的にダンプする時間間隔                            // Time interval to dump clean cache to disk periodically
	TrieCleanNoPrefetch bool          // フォローアップブロックのヒューリスティック状態のプリフェッチを無効にするかどうか      // Whether to disable heuristic state prefetching for followup blocks
	TrieDirtyLimit      int           // ダーティトライノードのディスクへのフラッシュを開始するメモリ制限（MB）               // Memory limit (MB) at which to start flushing dirty trie nodes to disk
	TrieDirtyDisabled   bool          // トライ書き込みキャッシュとGCを完全に無効にするかどうか（アーカイブノード）           // Whether to disable trie write caching and GC altogether (archive node)
	TrieTimeLimit       time.Duration // 現在のメモリ内トライをディスクにフラッシュするまでの制限時間                        // Time limit after which to flush the current in-memory trie to disk
	SnapshotLimit       int           // スナップショットエントリをメモリにキャッシュするために使用するメモリアローワンス（MB）// Memory allowance (MB) to use for caching snapshot entries in memory
	Preimages           bool          // トライキーのプリイメージをディスクに保存するかどうか                               // Whether to store preimage of trie key to the disk

	SnapshotWait bool // Wait for snapshot construction on startup. TODO(karalabe): This is a dirty hack for testing, nuke it
	// 起動時にスナップショットが作成されるのを待ちます。 TODO（karalabe）：これはテストのための汚いハックです。
}

// defaultCacheConfig are the default caching values if none are specified by the
// user (also used during testing).
// defaultCacheConfigは、ユーザーが指定していない場合のデフォルトのキャッシュ値です（テスト中にも使用されます）。
var defaultCacheConfig = &CacheConfig{
	TrieCleanLimit: 256,
	TrieDirtyLimit: 256,
	TrieTimeLimit:  5 * time.Minute,
	SnapshotLimit:  256,
	SnapshotWait:   true,
}

// BlockChain represents the canonical chain given a database with a genesis
// block. The Blockchain manages chain imports, reverts, chain reorganisations.
//
// Importing blocks in to the block chain happens according to the set of rules
// defined by the two stage Validator. Processing of blocks is done using the
// Processor which processes the included transaction. The validation of the state
// is done in the second part of the Validator. Failing results in aborting of
// the import.
//
// The BlockChain also helps in returning blocks from **any** chain included
// in the database as well as blocks that represents the canonical chain. It's
// important to note that GetBlock can return any block and does not need to be
// included in the canonical one where as GetBlockByNumber always represents the
// canonical chain.
// BlockChainは、ジェネシスブロックを含むデータベースが与えられた場合の正規チェーンを表します。
// ブロックチェーンは、チェーンのインポート、復帰、チェーンの再編成を管理します。
//
// ブロックチェーンへのブロックのインポートは、2ステージのバリデーターによって定義された一連のルールに従って行われます。
// ブロックの処理は、含まれているトランザクションを処理するプロセッサを使用して行われます。状態の検証は、バリデーターの2番目の部分で行われます。失敗すると、インポートが中止されます。
//
// BlockChainは、データベースに含まれる**任意の**チェーンからのブロックと、正規チェーンを表すブロックを返すのにも役立ちます。
// GetBlockは任意のブロックを返すことができ、正規のブロックに含める必要はないことに注意してください。
// GetBlockByNumberは常に正規のチェーンを表します。
type BlockChain struct {
	chainConfig *params.ChainConfig // チェーンとネットワークの構成 // Chain & network configuration
	cacheConfig *CacheConfig        // 剪定のためのキャッシュ構成 // Cache configuration for pruning

	db     ethdb.Database // 最終的なコンテンツを保存するための低レベルの永続データベース // Low level persistent database to store final content in
	snaps  *snapshot.Tree // 高速トライリーフアクセス用のスナップショットツリー          // Snapshot tree for fast trie leaf access
	triegc *prque.Prque   // gcを試行するための優先キューマッピングブロック番号          // Priority queue mapping block numbers to tries to gc
	gcproc time.Duration  // トライダンピングの正規ブロック処理を累積します              // Accumulates canonical block processing for trie dumping

	// txLookupLimit is the maximum number of blocks from head whose tx indices
	// are reserved:
	//  * 0:   means no limit and regenerate any missing indexes
	//  * N:   means N block limit [HEAD-N+1, HEAD] and delete extra indexes
	//  * nil: disable tx reindexer/deleter, but still index new blocks
	// txLookupLimitは、txインデックスを持つヘッドからのブロックの最大数です
	// 予約済み：
	// * 0：制限がないことを意味し、欠落しているインデックスを再生成します
	// * N：Nブロック制限[HEAD-N + 1、HEAD]を意味し、余分なインデックスを削除します
	// * nil：tx reindexer / deleterを無効にしますが、それでも新しいブロックにインデックスを付けます
	txLookupLimit uint64

	hc            *HeaderChain
	rmLogsFeed    event.Feed
	chainFeed     event.Feed
	chainSideFeed event.Feed
	chainHeadFeed event.Feed
	logsFeed      event.Feed
	blockProcFeed event.Feed
	scope         event.SubscriptionScope
	genesisBlock  *types.Block

	// This mutex synchronizes chain write operations.
	// Readers don't need to take it, they can just read the database.
	// このミューテックスはチェーン書き込み操作を同期します。
	// 読者はそれを取る必要はなく、データベースを読み取ることができます。
	chainmu *syncx.ClosableMutex

	currentBlock     atomic.Value // ブロックチェーンの現在のヘッド // Current head of the block chain
	currentFastBlock atomic.Value // 高速同期チェーンの現在のヘッド（ブロックチェーンの上にある可能性があります！） // Current head of the fast-sync chain (may be above the block chain!)

	stateCache    state.Database // インポート間で再利用する状態データベース（状態キャッシュを含む） // State database to reuse between imports (contains state cache)
	bodyCache     *lru.Cache     // 最新のブロック本体のキャッシュ                          // Cache for the most recent block bodies
	bodyRLPCache  *lru.Cache     // RLPエンコード形式の最新のブロック本体のキャッシュ         // Cache for the most recent block bodies in RLP encoded format
	receiptsCache *lru.Cache     // ブロックごとの最新の領収書のキャッシュ                   // Cache for the most recent receipts per block
	blockCache    *lru.Cache     // 最新のブロック全体のキャッシュ                           // Cache for the most recent entire blocks
	txLookupCache *lru.Cache     // 最新のトランザクションルックアップデータのキャッシュ。     // Cache for the most recent transaction lookup data.
	futureBlocks  *lru.Cache     // 将来のブロックは、後で処理するために追加されたブロックです // future blocks are blocks added for later processing

	wg            sync.WaitGroup //
	quit          chan struct{}  // シャットダウン信号、停止で閉じます。                // shutdown signal, closed in Stop.
	running       int32          // チェーンが実行されている場合は0、停止している場合は1 // 0 if chain is running, 1 when stopped
	procInterrupt int32          // ブロック処理用の割り込み信号                       // interrupt signaler for block processing

	engine     consensus.Engine
	validator  Validator // Block and state validator interface // ブロックおよび状態バリデーターインターフェイス
	prefetcher Prefetcher
	processor  Processor // Block transaction processor interface // トランザクションプロセッサインターフェイスをブロックする
	forker     *ForkChoice
	vmConfig   vm.Config
}

// NewBlockChain returns a fully initialised block chain using information
// available in the database. It initialises the default Ethereum Validator
// and Processor.
// NewBlockChainは、データベースで利用可能な情報を使用して、完全に初期化されたブロックチェーンを返します。
// デフォルトのイーサリアムバリデーターとプロセッサーを初期化します。
func NewBlockChain(db ethdb.Database, cacheConfig *CacheConfig, chainConfig *params.ChainConfig, engine consensus.Engine, vmConfig vm.Config, shouldPreserve func(header *types.Header) bool, txLookupLimit *uint64) (*BlockChain, error) {
	if cacheConfig == nil {
		cacheConfig = defaultCacheConfig
	}
	bodyCache, _ := lru.New(bodyCacheLimit)
	bodyRLPCache, _ := lru.New(bodyCacheLimit)
	receiptsCache, _ := lru.New(receiptsCacheLimit)
	blockCache, _ := lru.New(blockCacheLimit)
	txLookupCache, _ := lru.New(txLookupCacheLimit)
	futureBlocks, _ := lru.New(maxFutureBlocks)

	bc := &BlockChain{
		chainConfig: chainConfig,
		cacheConfig: cacheConfig,
		db:          db,
		triegc:      prque.New(nil),
		stateCache: state.NewDatabaseWithConfig(db, &trie.Config{
			Cache:     cacheConfig.TrieCleanLimit,
			Journal:   cacheConfig.TrieCleanJournal,
			Preimages: cacheConfig.Preimages,
		}),
		quit:          make(chan struct{}),
		chainmu:       syncx.NewClosableMutex(),
		bodyCache:     bodyCache,
		bodyRLPCache:  bodyRLPCache,
		receiptsCache: receiptsCache,
		blockCache:    blockCache,
		txLookupCache: txLookupCache,
		futureBlocks:  futureBlocks,
		engine:        engine,
		vmConfig:      vmConfig,
	}
	bc.forker = NewForkChoice(bc, shouldPreserve)
	bc.validator = NewBlockValidator(chainConfig, bc, engine)
	bc.prefetcher = newStatePrefetcher(chainConfig, bc, engine)
	bc.processor = NewStateProcessor(chainConfig, bc, engine)

	var err error
	bc.hc, err = NewHeaderChain(db, chainConfig, engine, bc.insertStopped)
	if err != nil {
		return nil, err
	}
	bc.genesisBlock = bc.GetBlockByNumber(0)
	if bc.genesisBlock == nil {
		return nil, ErrNoGenesis
	}

	var nilBlock *types.Block
	bc.currentBlock.Store(nilBlock)
	bc.currentFastBlock.Store(nilBlock)

	// Initialize the chain with ancient data if it isn't empty.
	// 空でない場合は、古いデータを使用してチェーンを初期化します。
	var txIndexBlock uint64

	if bc.empty() {
		rawdb.InitDatabaseFromFreezer(bc.db)
		// If ancient database is not empty, reconstruct all missing
		// indices in the background.
		// 古いデータベースが空でない場合は、バックグラウンドで欠落しているすべてのインデックスを再構築します。
		frozen, _ := bc.db.Ancients()
		if frozen > 0 {
			txIndexBlock = frozen
		}
	}
	if err := bc.loadLastState(); err != nil {
		return nil, err
	}

	// Make sure the state associated with the block is available
	// ブロックに関連付けられている状態が利用可能であることを確認してください
	head := bc.CurrentBlock()
	if _, err := state.New(head.Root(), bc.stateCache, bc.snaps); err != nil {
		// Head state is missing, before the state recovery, find out the
		// disk layer point of snapshot(if it's enabled). Make sure the
		// rewound point is lower than disk layer.
		// ヘッドの状態が欠落しています。状態を回復する前に、スナップショットのディスクレイヤーポイントを確認してください（有効になっている場合）。
		// 巻き戻しポイントがディスクレイヤーよりも低いことを確認してください。
		var diskRoot common.Hash
		if bc.cacheConfig.SnapshotLimit > 0 {
			diskRoot = rawdb.ReadSnapshotRoot(bc.db)
		}
		if diskRoot != (common.Hash{}) {
			log.Warn("Head state missing, repairing", "number", head.Number(), "hash", head.Hash(), "snaproot", diskRoot)

			snapDisk, err := bc.setHeadBeyondRoot(head.NumberU64(), diskRoot, true)
			if err != nil {
				return nil, err
			}
			// Chain rewound, persist old snapshot number to indicate recovery procedure
			// チェーンを巻き戻し、古いスナップショット番号を保持してリカバリ手順を示します
			if snapDisk != 0 {
				rawdb.WriteSnapshotRecoveryNumber(bc.db, snapDisk)
			}
		} else {
			log.Warn("Head state missing, repairing", "number", head.Number(), "hash", head.Hash())
			if _, err := bc.setHeadBeyondRoot(head.NumberU64(), common.Hash{}, true); err != nil {
				return nil, err
			}
		}
	}

	// Ensure that a previous crash in SetHead doesn't leave extra ancients
	// SetHeadでの以前のクラッシュが余分な古代人を残さないことを確認してください
	if frozen, err := bc.db.Ancients(); err == nil && frozen > 0 {
		var (
			needRewind bool
			low        uint64
		)
		// The head full block may be rolled back to a very low height due to
		// blockchain repair. If the head full block is even lower than the ancient
		// chain, truncate the ancient store.
		// ブロックチェーンの修理により、ヘッドフルブロックが非常に低い高さにロールバックされる場合があります。
		// ヘッドフルブロックが古代のチェーンよりもさらに低い場合は、古代のストアを切り捨てます。
		fullBlock := bc.CurrentBlock()
		if fullBlock != nil && fullBlock.Hash() != bc.genesisBlock.Hash() && fullBlock.NumberU64() < frozen-1 {
			needRewind = true
			low = fullBlock.NumberU64()
		}
		// In fast sync, it may happen that ancient data has been written to the
		// ancient store, but the LastFastBlock has not been updated, truncate the
		// extra data here.
		// 高速同期では、古いデータが古いストアに書き込まれている可能性がありますが、
		// LastFastBlockは更新されていないため、ここで余分なデータを切り捨てます。
		fastBlock := bc.CurrentFastBlock()
		if fastBlock != nil && fastBlock.NumberU64() < frozen-1 {
			needRewind = true
			if fastBlock.NumberU64() < low || low == 0 {
				low = fastBlock.NumberU64()
			}
		}
		if needRewind {
			log.Error("Truncating ancient chain", "from", bc.CurrentHeader().Number.Uint64(), "to", low)
			if err := bc.SetHead(low); err != nil {
				return nil, err
			}
		}
	}
	// The first thing the node will do is reconstruct the verification data for
	// the head block (ethash cache or clique voting snapshot). Might as well do
	// it in advance.
	// ノードが最初に行うことは、ヘッドブロックの検証データ（ethashキャッシュまたはクリーク投票スナップショット）を再構築することです。
	// 事前にそれを行うこともできます。
	bc.engine.VerifyHeader(bc, bc.CurrentHeader(), true, bc.db)

	// Check the current state of the block hashes and make sure that we do not have any of the bad blocks in our chain
	// ブロックハッシュの現在の状態をチェックし、チェーンに不良ブロックがないことを確認します
	for hash := range BadHashes {
		if header := bc.GetHeaderByHash(hash); header != nil {
			// get the canonical block corresponding to the offending header's number
			// 問題のあるヘッダーの番号に対応する正規ブロックを取得します
			headerByNumber := bc.GetHeaderByNumber(header.Number.Uint64())
			// make sure the headerByNumber (if present) is in our current canonical chain
			// headerByNumber（存在する場合）が現在の正規チェーンにあることを確認してください
			if headerByNumber != nil && headerByNumber.Hash() == header.Hash() {
				log.Error("Found bad hash, rewinding chain", "number", header.Number, "hash", header.ParentHash)
				if err := bc.SetHead(header.Number.Uint64() - 1); err != nil {
					return nil, err
				}
				log.Error("Chain rewind was successful, resuming normal operation")
			}
		}
	}

	// Load any existing snapshot, regenerating it if loading failed
	// 既存のスナップショットをロードし、ロードに失敗した場合は再生成します
	if bc.cacheConfig.SnapshotLimit > 0 {
		// If the chain was rewound past the snapshot persistent layer (causing
		// a recovery block number to be persisted to disk), check if we're still
		// in recovery mode and in that case, don't invalidate the snapshot on a
		// head mismatch.
		// チェーンがスナップショット永続層を超えて巻き戻された場合（リカバリブロック番号がディスクに永続化される原因）、
		// まだリカバリモードになっているかどうかを確認します。
		// その場合は、ヘッドの不一致でスナップショットを無効にしないでください。
		var recover bool

		head := bc.CurrentBlock()
		if layer := rawdb.ReadSnapshotRecoveryNumber(bc.db); layer != nil && *layer > head.NumberU64() {
			log.Warn("Enabling snapshot recovery", "chainhead", head.NumberU64(), "diskbase", *layer)
			recover = true
		}
		bc.snaps, _ = snapshot.New(bc.db, bc.stateCache.TrieDB(), bc.cacheConfig.SnapshotLimit, head.Root(), !bc.cacheConfig.SnapshotWait, true, recover)
	}

	// Start future block processor.
	// 将来のブロックプロセッサを起動します。
	bc.wg.Add(1)
	go bc.updateFutureBlocks()

	// Start tx indexer/unindexer.
	// txインデクサー/アンインデクサーを起動します。
	if txLookupLimit != nil {
		bc.txLookupLimit = *txLookupLimit

		bc.wg.Add(1)
		go bc.maintainTxIndex(txIndexBlock)
	}

	// If periodic cache journal is required, spin it up.
	// 定期的なキャッシュジャーナルが必要な場合は、スピンアップします。
	if bc.cacheConfig.TrieCleanRejournal > 0 {
		if bc.cacheConfig.TrieCleanRejournal < time.Minute {
			log.Warn("Sanitizing invalid trie cache journal time", "provided", bc.cacheConfig.TrieCleanRejournal, "updated", time.Minute)
			bc.cacheConfig.TrieCleanRejournal = time.Minute
		}
		triedb := bc.stateCache.TrieDB()
		bc.wg.Add(1)
		go func() {
			defer bc.wg.Done()
			triedb.SaveCachePeriodically(bc.cacheConfig.TrieCleanJournal, bc.cacheConfig.TrieCleanRejournal, bc.quit)
		}()
	}
	return bc, nil
}

// empty returns an indicator whether the blockchain is empty.
// Note, it's a special case that we connect a non-empty ancient
// database with an empty node, so that we can plugin the ancient
// into node seamlessly.
// emptyは、ブロックチェーンが空であるかどうかのインジケーターを返します。
// 空でない古代データベースを空のノードに接続するのは特殊なケースであることに注意してください。
// これにより、古代をノードにシームレスにプラグインできます。
func (bc *BlockChain) empty() bool {
	genesis := bc.genesisBlock.Hash()
	for _, hash := range []common.Hash{rawdb.ReadHeadBlockHash(bc.db), rawdb.ReadHeadHeaderHash(bc.db), rawdb.ReadHeadFastBlockHash(bc.db)} {
		if hash != genesis {
			return false
		}
	}
	return true
}

// loadLastState loads the last known chain state from the database. This method
// assumes that the chain manager mutex is held.
// loadLastStateは、データベースから最後の既知のチェーン状態をロードします。
// このメソッドは、チェーンマネージャーのミューテックスが保持されていることを前提としています。
func (bc *BlockChain) loadLastState() error {
	// Restore the last known head block
	// 最後の既知のヘッドブロックを復元します
	head := rawdb.ReadHeadBlockHash(bc.db)
	if head == (common.Hash{}) {
		// Corrupt or empty database, init from scratch
		// 破損または空のデータベース、最初から初期化
		log.Warn("Empty database, resetting chain")
		return bc.Reset()
	}
	// Make sure the entire head block is available
	// ヘッドブロック全体が利用可能であることを確認してください
	currentBlock := bc.GetBlockByHash(head)
	if currentBlock == nil {
		// Corrupt or empty database, init from scratch
		// 破損または空のデータベース、最初から初期化
		log.Warn("Head block missing, resetting chain", "hash", head)
		return bc.Reset()
	}
	// Everything seems to be fine, set as the head block
	// ヘッドブロックとして設定して、すべてがうまくいくようです
	bc.currentBlock.Store(currentBlock)
	headBlockGauge.Update(int64(currentBlock.NumberU64()))

	// Restore the last known head header
	// 最後の既知のヘッドヘッダーを復元する
	currentHeader := currentBlock.Header()
	if head := rawdb.ReadHeadHeaderHash(bc.db); head != (common.Hash{}) {
		if header := bc.GetHeaderByHash(head); header != nil {
			currentHeader = header
		}
	}
	bc.hc.SetCurrentHeader(currentHeader)

	// Restore the last known head fast block
	// 最後の既知のヘッドファストブロックを復元します
	bc.currentFastBlock.Store(currentBlock)
	headFastBlockGauge.Update(int64(currentBlock.NumberU64()))

	if head := rawdb.ReadHeadFastBlockHash(bc.db); head != (common.Hash{}) {
		if block := bc.GetBlockByHash(head); block != nil {
			bc.currentFastBlock.Store(block)
			headFastBlockGauge.Update(int64(block.NumberU64()))
		}
	}
	// Issue a status log for the user
	// ユーザーのステータスログを発行します
	currentFastBlock := bc.CurrentFastBlock()

	headerTd := bc.GetTd(currentHeader.Hash(), currentHeader.Number.Uint64())
	blockTd := bc.GetTd(currentBlock.Hash(), currentBlock.NumberU64())
	fastTd := bc.GetTd(currentFastBlock.Hash(), currentFastBlock.NumberU64())

	log.Info("Loaded most recent local header", "number", currentHeader.Number, "hash", currentHeader.Hash(), "td", headerTd, "age", common.PrettyAge(time.Unix(int64(currentHeader.Time), 0)))
	log.Info("Loaded most recent local full block", "number", currentBlock.Number(), "hash", currentBlock.Hash(), "td", blockTd, "age", common.PrettyAge(time.Unix(int64(currentBlock.Time()), 0)))
	log.Info("Loaded most recent local fast block", "number", currentFastBlock.Number(), "hash", currentFastBlock.Hash(), "td", fastTd, "age", common.PrettyAge(time.Unix(int64(currentFastBlock.Time()), 0)))
	if pivot := rawdb.ReadLastPivotNumber(bc.db); pivot != nil {
		log.Info("Loaded last fast-sync pivot marker", "number", *pivot)
	}
	return nil
}

// SetHead rewinds the local chain to a new head. Depending on whether the node
// was fast synced or full synced and in which state, the method will try to
// delete minimal data from disk whilst retaining chain consistency.
// SetHeadは、ローカルチェーンを新しいヘッドに巻き戻します。
// ノードが高速同期されたか完全同期されたか、およびどの状態にあるかに応じて、
// メソッドはチェーンの整合性を維持しながら、ディスクから最小限のデータを削除しようとします。
func (bc *BlockChain) SetHead(head uint64) error {
	_, err := bc.setHeadBeyondRoot(head, common.Hash{}, false)
	return err
}

// setHeadBeyondRoot rewinds the local chain to a new head with the extra condition
// that the rewind must pass the specified state root. This method is meant to be
// used when rewinding with snapshots enabled to ensure that we go back further than
// persistent disk layer. Depending on whether the node was fast synced or full, and
// in which state, the method will try to delete minimal data from disk whilst
// retaining chain consistency.
//
// The method returns the block number where the requested root cap was found.
// setHeadBeyondRootは、ローカルチェーンを新しいヘッドに巻き戻しますが、
// 巻き戻しは指定された状態ルートを通過する必要があるという追加の条件があります。
// この方法は、スナップショットを有効にして巻き戻すときに使用して、
// 永続ディスクレイヤーよりも先に戻るようにすることを目的としています。
// ノードが高速同期されたかフル同期されたか、およびどの状態であるかに応じて、
// メソッドはチェーンの整合性を維持しながら、ディスクから最小限のデータを削除しようとします。
//
//メソッドは、要求されたルートキャップが見つかったブロック番号を返します。
func (bc *BlockChain) setHeadBeyondRoot(head uint64, root common.Hash, repair bool) (uint64, error) {
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Track the block number of the requested root hash
	// 要求されたルートハッシュのブロック番号を追跡する
	var rootNumber uint64 // (no root == always 0)

	// Retrieve the last pivot block to short circuit rollbacks beyond it and the
	// current freezer limit to start nuking id underflown
	// 最後のピボットブロックを取得して、それを超えるロールバックを短絡し、
	// 現在のフリーザー制限を取得して、アンダーフローしたnukingidを開始します
	pivot := rawdb.ReadLastPivotNumber(bc.db)
	frozen, _ := bc.db.Ancients()

	updateFn := func(db ethdb.KeyValueWriter, header *types.Header) (uint64, bool) {
		// Rewind the blockchain, ensuring we don't end up with a stateless head
		// block. Note, depth equality is permitted to allow using SetHead as a
		// chain reparation mechanism without deleting any data!
		// ブロックチェーンを巻き戻して、ステートレスヘッドブロックになってしまわないようにします。
		// データを削除せずにSetHeadをチェーン修復メカニズムとして使用できるようにするには、
		// 深さの同等性が許可されていることに注意してください。
		if currentBlock := bc.CurrentBlock(); currentBlock != nil && header.Number.Uint64() <= currentBlock.NumberU64() {
			newHeadBlock := bc.GetBlock(header.Hash(), header.Number.Uint64())
			if newHeadBlock == nil {
				log.Error("Gap in the chain, rewinding to genesis", "number", header.Number, "hash", header.Hash())
				newHeadBlock = bc.genesisBlock
			} else {
				// Block exists, keep rewinding until we find one with state,
				// keeping rewinding until we exceed the optional threshold
				// root hash
				// ブロックが存在し、状態のあるブロックが見つかるまで巻き戻しを続け、
				// オプションのしきい値ルートハッシュを超えるまで巻き戻しを続けます
				beyondRoot := (root == common.Hash{}) // Flag whether we're beyond the requested root (no root, always true)

				for {
					// If a root threshold was requested but not yet crossed, check
					// ルートしきい値が要求されたが、まだ超えていない場合は、チェックしてください
					if root != (common.Hash{}) && !beyondRoot && newHeadBlock.Root() == root {
						beyondRoot, rootNumber = true, newHeadBlock.NumberU64()
					}
					if _, err := state.New(newHeadBlock.Root(), bc.stateCache, bc.snaps); err != nil {
						log.Trace("Block state missing, rewinding further", "number", newHeadBlock.NumberU64(), "hash", newHeadBlock.Hash())
						if pivot == nil || newHeadBlock.NumberU64() > *pivot {
							parent := bc.GetBlock(newHeadBlock.ParentHash(), newHeadBlock.NumberU64()-1)
							if parent != nil {
								newHeadBlock = parent
								continue
							}
							log.Error("Missing block in the middle, aiming genesis", "number", newHeadBlock.NumberU64()-1, "hash", newHeadBlock.ParentHash())
							newHeadBlock = bc.genesisBlock
						} else {
							log.Trace("Rewind passed pivot, aiming genesis", "number", newHeadBlock.NumberU64(), "hash", newHeadBlock.Hash(), "pivot", *pivot)
							newHeadBlock = bc.genesisBlock
						}
					}
					if beyondRoot || newHeadBlock.NumberU64() == 0 {
						log.Debug("Rewound to block with state", "number", newHeadBlock.NumberU64(), "hash", newHeadBlock.Hash())
						break
					}
					log.Debug("Skipping block with threshold state", "number", newHeadBlock.NumberU64(), "hash", newHeadBlock.Hash(), "root", newHeadBlock.Root())
					newHeadBlock = bc.GetBlock(newHeadBlock.ParentHash(), newHeadBlock.NumberU64()-1) // Keep rewinding 巻き戻しを続ける
				}
			}
			rawdb.WriteHeadBlockHash(db, newHeadBlock.Hash())

			// Degrade the chain markers if they are explicitly reverted.
			// In theory we should update all in-memory markers in the
			// last step, however the direction of SetHead is from high
			// to low, so it's safe the update in-memory markers directly.
			// チェーンマーカーが明示的に元に戻されている場合は、チェーンマーカーを劣化させます。
			// 理論的には、最後のステップですべてのメモリ内マーカーを更新する必要がありますが、
			// SetHeadの方向は高から低であるため、メモリ内マーカーを直接更新しても安全です。
			bc.currentBlock.Store(newHeadBlock)
			headBlockGauge.Update(int64(newHeadBlock.NumberU64()))
		}
		// Rewind the fast block in a simpleton way to the target head
		// 簡単な方法で高速ブロックをターゲットヘッドまで巻き戻します
		if currentFastBlock := bc.CurrentFastBlock(); currentFastBlock != nil && header.Number.Uint64() < currentFastBlock.NumberU64() {
			newHeadFastBlock := bc.GetBlock(header.Hash(), header.Number.Uint64())
			// If either blocks reached nil, reset to the genesis state
			// いずれかのブロックがゼロに達した場合は、ジェネシス状態にリセットします
			if newHeadFastBlock == nil {
				newHeadFastBlock = bc.genesisBlock
			}
			rawdb.WriteHeadFastBlockHash(db, newHeadFastBlock.Hash())

			// Degrade the chain markers if they are explicitly reverted.
			// In theory we should update all in-memory markers in the
			// last step, however the direction of SetHead is from high
			// to low, so it's safe the update in-memory markers directly.
			// チェーンマーカーが明示的に元に戻されている場合は、チェーンマーカーを劣化させます。
			// 理論的には、最後のステップですべてのメモリ内マーカーを更新する必要がありますが、
			// SetHeadの方向は高から低であるため、メモリ内マーカーを直接更新しても安全です。
			bc.currentFastBlock.Store(newHeadFastBlock)
			headFastBlockGauge.Update(int64(newHeadFastBlock.NumberU64()))
		}
		head := bc.CurrentBlock().NumberU64()

		// If setHead underflown the freezer threshold and the block processing
		// intent afterwards is full block importing, delete the chain segment
		// between the stateful-block and the sethead target.
		// setHeadがフリーザーのしきい値を下回り、その後のブロック処理インテントが完全なブロックインポートである場合は、
		// ステートフルブロックとセットヘッドターゲットの間のチェーンセグメントを削除します。
		var wipe bool
		if head+1 < frozen {
			wipe = pivot == nil || head >= *pivot
		}
		return head, wipe // Only force wipe if full synced // 完全に同期されている場合にのみ強制ワイプ
	}
	// Rewind the header chain, deleting all block bodies until then
	// ヘッダーチェーンを巻き戻し、それまでのすべてのブロック本体を削除します
	delFn := func(db ethdb.KeyValueWriter, hash common.Hash, num uint64) {
		// Ignore the error here since light client won't hit this path
		// ライトクライアントはこのパスにヒットしないため、ここでのエラーは無視してください
		frozen, _ := bc.db.Ancients()
		if num+1 <= frozen {
			// Truncate all relative data(header, total difficulty, body, receipt
			// and canonical hash) from ancient store.
			// 古代のストアからのすべての相対データ（ヘッダー、合計難易度、本文、領収書、正規ハッシュ）を切り捨てます。
			if err := bc.db.TruncateAncients(num); err != nil {
				log.Crit("Failed to truncate ancient data", "number", num, "err", err)
			}
			// Remove the hash <-> number mapping from the active store.
			// アクティブストアからハッシュ<->番号マッピングを削除します。
			rawdb.DeleteHeaderNumber(db, hash)
		} else {
			// Remove relative body and receipts from the active store.
			// The header, total difficulty and canonical hash will be
			// removed in the hc.SetHead function.
			// アクティブストアから相対的な本文と領収書を削除します。
			// ヘッダー、合計難易度、および正規ハッシュは、hc.SetHead関数で削除されます。
			rawdb.DeleteBody(db, hash, num)
			rawdb.DeleteReceipts(db, hash, num)
		}
		// Todo(rjl493456442) txlookup, bloombits, etc
		// Todo（rjl493456442）txlookup、bloombitsなど
	}
	// If SetHead was only called as a chain reparation method, try to skip
	// touching the header chain altogether, unless the freezer is broken
	// SetHeadがチェーン修復メソッドとしてのみ呼び出された場合は、
	// フリーザーが壊れていない限り、ヘッダーチェーンへのタッチを完全にスキップしてみてください
	if repair {
		if target, force := updateFn(bc.db, bc.CurrentBlock().Header()); force {
			bc.hc.SetHead(target, updateFn, delFn)
		}
	} else {
		// Rewind the chain to the requested head and keep going backwards until a
		// block with a state is found or fast sync pivot is passed
		// チェーンを要求されたヘッドに巻き戻し、状態のあるブロックが見つかるか、
		// 高速同期ピボットが渡されるまで後方に移動し続けます
		log.Warn("Rewinding blockchain", "target", head)
		bc.hc.SetHead(head, updateFn, delFn)
	}
	// Clear out any stale content from the caches
	// キャッシュから古いコンテンツをクリアします
	bc.bodyCache.Purge()
	bc.bodyRLPCache.Purge()
	bc.receiptsCache.Purge()
	bc.blockCache.Purge()
	bc.txLookupCache.Purge()
	bc.futureBlocks.Purge()

	return rootNumber, bc.loadLastState()
}

// SnapSyncCommitHead sets the current head block to the one defined by the hash
// irrelevant what the chain contents were prior.
// SnapSyncCommitHeadは、現在のヘッドブロックを、
// チェーンの内容が以前のものであったかどうかに関係なく、ハッシュによって定義されたものに設定します。
func (bc *BlockChain) SnapSyncCommitHead(hash common.Hash) error {
	// Make sure that both the block as well at its state trie exists
	// ブロックとその状態トライの両方が存在することを確認してください
	block := bc.GetBlockByHash(hash)
	if block == nil {
		return fmt.Errorf("non existent block [%x..]", hash[:4])
	}
	if _, err := trie.NewSecure(block.Root(), bc.stateCache.TrieDB()); err != nil {
		return err
	}

	// If all checks out, manually set the head block.
	// すべてチェックアウトする場合は、ヘッドブロックを手動で設定します。
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	bc.currentBlock.Store(block)
	headBlockGauge.Update(int64(block.NumberU64()))
	bc.chainmu.Unlock()

	// Destroy any existing state snapshot and regenerate it in the background,
	// also resuming the normal maintenance of any previously paused snapshot.
	// 既存の状態スナップショットを破棄してバックグラウンドで再生成し、
	// 以前に一時停止したスナップショットの通常のメンテナンスも再開します。
	if bc.snaps != nil {
		bc.snaps.Rebuild(block.Root())
	}
	log.Info("Committed new head block", "number", block.Number(), "hash", hash)
	return nil
}

// Reset purges the entire blockchain, restoring it to its genesis state.
// リセットにより、ブロックチェーン全体がパージされ、元の状態に復元されます。
func (bc *BlockChain) Reset() error {
	return bc.ResetWithGenesisBlock(bc.genesisBlock)
}

// ResetWithGenesisBlock purges the entire blockchain, restoring it to the
// specified genesis state.
// ResetWithGenesisBlockはブロックチェーン全体をパージし、指定されたジェネシス状態に復元します。
func (bc *BlockChain) ResetWithGenesisBlock(genesis *types.Block) error {
	// Dump the entire block chain and purge the caches
	// ブロックチェーン全体をダンプし、キャッシュをパージします
	if err := bc.SetHead(0); err != nil {
		return err
	}
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Prepare the genesis block and reinitialise the chain
	batch := bc.db.NewBatch()
	rawdb.WriteTd(batch, genesis.Hash(), genesis.NumberU64(), genesis.Difficulty())
	rawdb.WriteBlock(batch, genesis)
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write genesis block", "err", err)
	}
	bc.writeHeadBlock(genesis)

	// Last update all in-memory chain markers
	// ジェネシスブロックを準備し、チェーンを再初期化します
	bc.genesisBlock = genesis
	bc.currentBlock.Store(bc.genesisBlock)
	headBlockGauge.Update(int64(bc.genesisBlock.NumberU64()))
	bc.hc.SetGenesis(bc.genesisBlock.Header())
	bc.hc.SetCurrentHeader(bc.genesisBlock.Header())
	bc.currentFastBlock.Store(bc.genesisBlock)
	headFastBlockGauge.Update(int64(bc.genesisBlock.NumberU64()))
	return nil
}

// Export writes the active chain to the given writer.
// エクスポートは、アクティブチェーンを指定されたライターに書き込みます。
func (bc *BlockChain) Export(w io.Writer) error {
	return bc.ExportN(w, uint64(0), bc.CurrentBlock().NumberU64())
}

// ExportN writes a subset of the active chain to the given writer.
// ExportNは、アクティブチェーンのサブセットを指定されたライターに書き込みます。
func (bc *BlockChain) ExportN(w io.Writer, first uint64, last uint64) error {
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	defer bc.chainmu.Unlock()

	if first > last {
		return fmt.Errorf("export failed: first (%d) is greater than last (%d)", first, last)
	}
	log.Info("Exporting batch of blocks", "count", last-first+1)

	start, reported := time.Now(), time.Now()
	for nr := first; nr <= last; nr++ {
		block := bc.GetBlockByNumber(nr)
		if block == nil {
			return fmt.Errorf("export failed on #%d: not found", nr)
		}
		if err := block.EncodeRLP(w); err != nil {
			return err
		}
		if time.Since(reported) >= statsReportLimit {
			log.Info("Exporting blocks", "exported", block.NumberU64()-first, "elapsed", common.PrettyDuration(time.Since(start)))
			reported = time.Now()
		}
	}
	return nil
}

// writeHeadBlock injects a new head block into the current block chain. This method
// assumes that the block is indeed a true head. It will also reset the head
// header and the head fast sync block to this very same block if they are older
// or if they are on a different side chain.
//
// Note, this function assumes that the `mu` mutex is held!
// writeHeadBlockは、新しいヘッドブロックを現在のブロックチェーンに挿入します。
// このメソッドは、ブロックが実際に真のヘッドであることを前提としています。
// また、ヘッドヘッダーとヘッド高速同期ブロックが古い場合、または異なる側鎖にある場合は、
// これらをまったく同じブロックにリセットします。
//
//この関数は、 `mu`ミューテックスが保持されていることを前提としていることに注意してください！
func (bc *BlockChain) writeHeadBlock(block *types.Block) {
	// Add the block to the canonical chain number scheme and mark as the head
	// 正規のチェーン番号スキームにブロックを追加し、ヘッドとしてマークします
	batch := bc.db.NewBatch()
	rawdb.WriteHeadHeaderHash(batch, block.Hash())
	rawdb.WriteHeadFastBlockHash(batch, block.Hash())
	rawdb.WriteCanonicalHash(batch, block.Hash(), block.NumberU64())
	rawdb.WriteTxLookupEntriesByBlock(batch, block)
	rawdb.WriteHeadBlockHash(batch, block.Hash())

	// Flush the whole batch into the disk, exit the node if failed
	// バッチ全体をディスクにフラッシュし、失敗した場合はノードを終了します
	if err := batch.Write(); err != nil {
		log.Crit("Failed to update chain indexes and markers", "err", err)
	}
	// Update all in-memory chain markers in the last step
	// 最後のステップですべてのメモリ内チェーンマーカーを更新します
	bc.hc.SetCurrentHeader(block.Header())

	bc.currentFastBlock.Store(block)
	headFastBlockGauge.Update(int64(block.NumberU64()))

	bc.currentBlock.Store(block)
	headBlockGauge.Update(int64(block.NumberU64()))
}

// Stop stops the blockchain service. If any imports are currently in progress
// it will abort them using the procInterrupt.
// Stopは、ブロックチェーンサービスを停止します。
// インポートが現在進行中の場合は、procInterruptを使用してそれらを中止します。
func (bc *BlockChain) Stop() {
	if !atomic.CompareAndSwapInt32(&bc.running, 0, 1) {
		return
	}

	// Unsubscribe all subscriptions registered from blockchain.
	// ブロックチェーンから登録されたすべてのサブスクリプションのサブスクリプションを解除します。
	bc.scope.Close()

	// Signal shutdown to all goroutines.
	// すべてのゴルーチンにシャットダウンを通知します。
	close(bc.quit)
	bc.StopInsert()

	// Now wait for all chain modifications to end and persistent goroutines to exit.
	//
	// Note: Close waits for the mutex to become available, i.e. any running chain
	// modification will have exited when Close returns. Since we also called StopInsert,
	// the mutex should become available quickly. It cannot be taken again after Close has
	// returned.
	// ここで、すべてのチェーンの変更が終了し、永続的なゴルーチンが終了するのを待ちます。
	//
	// 注：Closeは、ミューテックスが使用可能になるのを待機します。
	// つまり、Closeが戻ると、実行中のチェーンの変更はすべて終了します。
	// StopInsertとも呼ばれるので、ミューテックスはすぐに利用可能になるはずです。
	// Closeが戻った後は、再度取得することはできません。
	bc.chainmu.Close()
	bc.wg.Wait()

	// Ensure that the entirety of the state snapshot is journalled to disk.
	// 状態スナップショット全体がディスクにジャーナルされていることを確認します。
	var snapBase common.Hash
	if bc.snaps != nil {
		var err error
		if snapBase, err = bc.snaps.Journal(bc.CurrentBlock().Root()); err != nil {
			log.Error("Failed to journal state snapshot", "err", err)
		}
	}

	// Ensure the state of a recent block is also stored to disk before exiting.
	// We're writing three different states to catch different restart scenarios:
	//  - HEAD:     So we don't need to reprocess any blocks in the general case
	//  - HEAD-1:   So we don't do large reorgs if our HEAD becomes an uncle
	//  - HEAD-127: So we have a hard limit on the number of blocks reexecuted
	// 終了する前に、最近のブロックの状態もディスクに保存されていることを確認してください。
	// さまざまな再起動シナリオをキャッチするために、3つの異なる状態を記述しています。
	// --HEAD：したがって、一般的なケースではブロックを再処理する必要はありません
	// --HEAD-1：したがって、HEADが叔父になった場合、大規模な再編成は行いません
	// --HEAD-127：したがって、再実行されるブロックの数には厳しい制限があります
	if !bc.cacheConfig.TrieDirtyDisabled {
		triedb := bc.stateCache.TrieDB()

		for _, offset := range []uint64{0, 1, TriesInMemory - 1} {
			if number := bc.CurrentBlock().NumberU64(); number > offset {
				recent := bc.GetBlockByNumber(number - offset)

				log.Info("Writing cached state to disk", "block", recent.Number(), "hash", recent.Hash(), "root", recent.Root())
				if err := triedb.Commit(recent.Root(), true, nil); err != nil {
					log.Error("Failed to commit recent state trie", "err", err)
				}
			}
		}
		if snapBase != (common.Hash{}) {
			log.Info("Writing snapshot state to disk", "root", snapBase)
			if err := triedb.Commit(snapBase, true, nil); err != nil {
				log.Error("Failed to commit recent state trie", "err", err)
			}
		}
		for !bc.triegc.Empty() {
			triedb.Dereference(bc.triegc.PopItem().(common.Hash))
		}
		if size, _ := triedb.Size(); size != 0 {
			log.Error("Dangling trie nodes after full cleanup")
		}
	}
	// Ensure all live cached entries be saved into disk, so that we can skip
	// cache warmup when node restarts.
	// ノードの再起動時にキャッシュのウォームアップをスキップできるように、
	// すべてのライブキャッシュエントリがディスクに保存されていることを確認してください。
	if bc.cacheConfig.TrieCleanJournal != "" {
		triedb := bc.stateCache.TrieDB()
		triedb.SaveCache(bc.cacheConfig.TrieCleanJournal)
	}
	log.Info("Blockchain stopped")
}

// StopInsert interrupts all insertion methods, causing them to return
// errInsertionInterrupted as soon as possible. Insertion is permanently disabled after
// calling this method.
// StopInsertはすべての挿入メソッドを中断し、それらがerrInsertionInterruptedをできるだけ早く返すようにします。
// このメソッドを呼び出した後、挿入は永続的に無効になります。
func (bc *BlockChain) StopInsert() {
	atomic.StoreInt32(&bc.procInterrupt, 1)
}

// insertStopped returns true after StopInsert has been called.
// insertStoppedは、StopInsertが呼び出された後にtrueを返します。
func (bc *BlockChain) insertStopped() bool {
	return atomic.LoadInt32(&bc.procInterrupt) == 1
}

func (bc *BlockChain) procFutureBlocks() {
	blocks := make([]*types.Block, 0, bc.futureBlocks.Len())
	for _, hash := range bc.futureBlocks.Keys() {
		if block, exist := bc.futureBlocks.Peek(hash); exist {
			blocks = append(blocks, block.(*types.Block))
		}
	}
	if len(blocks) > 0 {
		sort.Slice(blocks, func(i, j int) bool {
			return blocks[i].NumberU64() < blocks[j].NumberU64()
		})
		// Insert one by one as chain insertion needs contiguous ancestry between blocks
		// チェーンの挿入にはブロック間の連続した祖先が必要なため、1つずつ挿入します
		for i := range blocks {
			bc.InsertChain(blocks[i : i+1])
		}
	}
}

// WriteStatus status of write
// 書き込みのWriteStatusステータス
type WriteStatus byte

const (
	NonStatTy WriteStatus = iota
	CanonStatTy
	SideStatTy
)

// InsertReceiptChain attempts to complete an already existing header chain with
// transaction and receipt data.
// InsertReceiptChainは、トランザクションおよびレシートデータを使用して既存のヘッダーチェーンを完成させようとします。
func (bc *BlockChain) InsertReceiptChain(blockChain types.Blocks, receiptChain []types.Receipts, ancientLimit uint64) (int, error) {
	// We don't require the chainMu here since we want to maximize the
	// concurrency of header insertion and receipt insertion.
	// ヘッダー挿入とレシート挿入の同時実行性を最大化したいので、ここではchainMuは必要ありません。
	bc.wg.Add(1)
	defer bc.wg.Done()

	var (
		ancientBlocks, liveBlocks     types.Blocks
		ancientReceipts, liveReceipts []types.Receipts
	)
	// Do a sanity check that the provided chain is actually ordered and linked
	// 提供されたチェーンが実際に注文され、リンクされていることを健全性チェックします
	for i := 0; i < len(blockChain); i++ {
		if i != 0 {
			if blockChain[i].NumberU64() != blockChain[i-1].NumberU64()+1 || blockChain[i].ParentHash() != blockChain[i-1].Hash() {
				log.Error("Non contiguous receipt insert", "number", blockChain[i].Number(), "hash", blockChain[i].Hash(), "parent", blockChain[i].ParentHash(),
					"prevnumber", blockChain[i-1].Number(), "prevhash", blockChain[i-1].Hash())
				return 0, fmt.Errorf("non contiguous insert: item %d is #%d [%x..], item %d is #%d [%x..] (parent [%x..])", i-1, blockChain[i-1].NumberU64(),
					blockChain[i-1].Hash().Bytes()[:4], i, blockChain[i].NumberU64(), blockChain[i].Hash().Bytes()[:4], blockChain[i].ParentHash().Bytes()[:4])
			}
		}
		if blockChain[i].NumberU64() <= ancientLimit {
			ancientBlocks, ancientReceipts = append(ancientBlocks, blockChain[i]), append(ancientReceipts, receiptChain[i])
		} else {
			liveBlocks, liveReceipts = append(liveBlocks, blockChain[i]), append(liveReceipts, receiptChain[i])
		}
	}

	var (
		stats = struct{ processed, ignored int32 }{}
		start = time.Now()
		size  = int64(0)
	)

	// updateHead updates the head fast sync block if the inserted blocks are better
	// and returns an indicator whether the inserted blocks are canonical.
	// updateHeadは、挿入されたブロックの方が優れている場合にヘッド高速同期ブロックを更新し、
	// 挿入されたブロックが正規であるかどうかのインジケーターを返します。
	updateHead := func(head *types.Block) bool {
		if !bc.chainmu.TryLock() {
			return false
		}
		defer bc.chainmu.Unlock()

		// Rewind may have occurred, skip in that case.
		// 巻き戻しが発生した可能性があります。その場合はスキップしてください。
		if bc.CurrentHeader().Number.Cmp(head.Number()) >= 0 {
			reorg, err := bc.forker.ReorgNeeded(bc.CurrentFastBlock().Header(), head.Header())
			if err != nil {
				log.Warn("Reorg failed", "err", err)
				return false
			} else if !reorg {
				return false
			}
			rawdb.WriteHeadFastBlockHash(bc.db, head.Hash())
			bc.currentFastBlock.Store(head)
			headFastBlockGauge.Update(int64(head.NumberU64()))
			return true
		}
		return false
	}

	// writeAncient writes blockchain and corresponding receipt chain into ancient store.
	//
	// this function only accepts canonical chain data. All side chain will be reverted
	// eventually.
	// writeAncientは、ブロックチェーンと対応するレシートチェーンを古代のストアに書き込みます。
	//
	// この関数は、正規のチェーンデータのみを受け入れます。最終的にすべての側鎖が元に戻ります。
	writeAncient := func(blockChain types.Blocks, receiptChain []types.Receipts) (int, error) {
		first := blockChain[0]
		last := blockChain[len(blockChain)-1]

		// Ensure genesis is in ancients.
		// 起源が古代にあることを確認してください。
		if first.NumberU64() == 1 {
			if frozen, _ := bc.db.Ancients(); frozen == 0 {
				b := bc.genesisBlock
				td := bc.genesisBlock.Difficulty()
				writeSize, err := rawdb.WriteAncientBlocks(bc.db, []*types.Block{b}, []types.Receipts{nil}, td)
				size += writeSize
				if err != nil {
					log.Error("Error writing genesis to ancients", "err", err)
					return 0, err
				}
				log.Info("Wrote genesis to ancients")
			}
		}
		// Before writing the blocks to the ancients, we need to ensure that
		// they correspond to the what the headerchain 'expects'.
		// We only check the last block/header, since it's a contiguous chain.
		// 古代人にブロックを書く前に、それらがヘッダーチェーンが「期待する」ものに対応していることを確認する必要があります。
		// 連続したチェーンであるため、最後のブロック/ヘッダーのみをチェックします。
		if !bc.HasHeader(last.Hash(), last.NumberU64()) {
			return 0, fmt.Errorf("containing header #%d [%x..] unknown", last.Number(), last.Hash().Bytes()[:4])
		}

		// Write all chain data to ancients.
		// すべてのチェーンデータを古代人に書き込みます。
		td := bc.GetTd(first.Hash(), first.NumberU64())
		writeSize, err := rawdb.WriteAncientBlocks(bc.db, blockChain, receiptChain, td)
		size += writeSize
		if err != nil {
			log.Error("Error importing chain data to ancients", "err", err)
			return 0, err
		}

		// Write tx indices if any condition is satisfied:
		// * If user requires to reserve all tx indices(txlookuplimit=0)
		// * If all ancient tx indices are required to be reserved(txlookuplimit is even higher than ancientlimit)
		// * If block number is large enough to be regarded as a recent block
		// It means blocks below the ancientLimit-txlookupLimit won't be indexed.
		//
		// But if the `TxIndexTail` is not nil, e.g. Geth is initialized with
		// an external ancient database, during the setup, blockchain will start
		// a background routine to re-indexed all indices in [ancients - txlookupLimit, ancients)
		// range. In this case, all tx indices of newly imported blocks should be
		// generated.
		// いずれかの条件が満たされた場合、txインデックスを書き込みます。
		// *ユーザーがすべてのtxインデックスを予約する必要がある場合（txlookuplimit = 0）
		// *すべての古代のtxインデックスを予約する必要がある場合（txlookuplimitはancientlimitよりもさらに高い）
		// *ブロック番号が最近のブロックと見なされるほど大きい場合
		// これは、ancientLimitより下のブロックを意味します-txlookupLimitはインデックスに登録されません。
		//
		// ただし、 `TxIndexTail`がnilでない場合、たとえばGethは外部の古代データベースで初期化され、
		// セットアップ中に、ブロックチェーンはバックグラウンドルーチンを開始して、
		// [ancients --txlookupLimit、ancients）範囲のすべてのインデックスを再インデックス化します。
		// この場合、新しくインポートされたブロックのすべてのtxインデックスを生成する必要があります。
		var batch = bc.db.NewBatch()
		for _, block := range blockChain {
			if bc.txLookupLimit == 0 || ancientLimit <= bc.txLookupLimit || block.NumberU64() >= ancientLimit-bc.txLookupLimit {
				rawdb.WriteTxLookupEntriesByBlock(batch, block)
			} else if rawdb.ReadTxIndexTail(bc.db) != nil {
				rawdb.WriteTxLookupEntriesByBlock(batch, block)
			}
			stats.processed++
		}

		// Flush all tx-lookup index data.
		// すべてのtx-lookupインデックスデータをフラッシュします。
		size += int64(batch.ValueSize())
		if err := batch.Write(); err != nil {
			// The tx index data could not be written.
			// Roll back the ancient store update.
			// txインデックスデータを書き込めませんでした。
			//古いストアの更新をロールバックします。
			fastBlock := bc.CurrentFastBlock().NumberU64()
			if err := bc.db.TruncateAncients(fastBlock + 1); err != nil {
				log.Error("Can't truncate ancient store after failed insert", "err", err)
			}
			return 0, err
		}

		// Sync the ancient store explicitly to ensure all data has been flushed to disk.
		// 古いストアを明示的に同期して、すべてのデータがディスクにフラッシュされたことを確認します。
		if err := bc.db.Sync(); err != nil {
			return 0, err
		}

		// Update the current fast block because all block data is now present in DB.
		// すべてのブロックデータがDBに存在するため、現在の高速ブロックを更新します。
		previousFastBlock := bc.CurrentFastBlock().NumberU64()
		if !updateHead(blockChain[len(blockChain)-1]) {
			// We end up here if the header chain has reorg'ed, and the blocks/receipts
			// don't match the canonical chain.
			// ヘッダーチェーンが再編成され、ブロック/レシートが正規チェーンと一致しない場合は、ここで終了します。
			if err := bc.db.TruncateAncients(previousFastBlock + 1); err != nil {
				log.Error("Can't truncate ancient store after failed insert", "err", err)
			}
			return 0, errSideChainReceipts
		}

		// Delete block data from the main database.
		// メインデータベースからブロックデータを削除します。
		batch.Reset()
		canonHashes := make(map[common.Hash]struct{})
		for _, block := range blockChain {
			canonHashes[block.Hash()] = struct{}{}
			if block.NumberU64() == 0 {
				continue
			}
			rawdb.DeleteCanonicalHash(batch, block.NumberU64())
			rawdb.DeleteBlockWithoutNumber(batch, block.Hash(), block.NumberU64())
		}
		// Delete side chain hash-to-number mappings.
		// 側鎖のハッシュから数値へのマッピングを削除します。
		for _, nh := range rawdb.ReadAllHashesInRange(bc.db, first.NumberU64(), last.NumberU64()) {
			if _, canon := canonHashes[nh.Hash]; !canon {
				rawdb.DeleteHeader(batch, nh.Hash, nh.Number)
			}
		}
		if err := batch.Write(); err != nil {
			return 0, err
		}
		return 0, nil
	}

	// writeLive writes blockchain and corresponding receipt chain into active store.
	// writeLiveは、ブロックチェーンと対応するレシートチェーンをアクティブストアに書き込みます。
	writeLive := func(blockChain types.Blocks, receiptChain []types.Receipts) (int, error) {
		skipPresenceCheck := false
		batch := bc.db.NewBatch()
		for i, block := range blockChain {
			// Short circuit insertion if shutting down or processing failed
			// シャットダウンまたは処理が失敗した場合の短絡挿入
			if bc.insertStopped() {
				return 0, errInsertionInterrupted
			}
			// Short circuit if the owner header is unknown
			// 所有者ヘッダーが不明な場合の短絡
			if !bc.HasHeader(block.Hash(), block.NumberU64()) {
				return i, fmt.Errorf("containing header #%d [%x..] unknown", block.Number(), block.Hash().Bytes()[:4])
			}
			if !skipPresenceCheck {
				// Ignore if the entire data is already known
				// データ全体がすでにわかっている場合は無視します
				if bc.HasBlock(block.Hash(), block.NumberU64()) {
					stats.ignored++
					continue
				} else {
					// If block N is not present, neither are the later blocks.
					// This should be true, but if we are mistaken, the shortcut
					// here will only cause overwriting of some existing data
					// ブロックNが存在しない場合、後のブロックも存在しません。
					// これは正しいはずですが、間違っている場合、
					// ここでのショートカットは一部の既存のデータの上書きのみを引き起こします
					skipPresenceCheck = true
				}
			}
			// Write all the data out into the database
			// すべてのデータをデータベースに書き出す
			rawdb.WriteBody(batch, block.Hash(), block.NumberU64(), block.Body())
			rawdb.WriteReceipts(batch, block.Hash(), block.NumberU64(), receiptChain[i])
			rawdb.WriteTxLookupEntriesByBlock(batch, block) // Always write tx indices for live blocks, we assume they are needed ライブブロックには常にtxインデックスを記述します。これは、必要であると想定しています。

			// Write everything belongs to the blocks into the database. So that
			// we can ensure all components of body is completed(body, receipts,
			// tx indexes)
			// ブロックに属するすべてのものをデータベースに書き込みます。
			// bodyのすべてのコンポーネント（body、レシート、txインデックス）が確実に完了するようにするため
			if batch.ValueSize() >= ethdb.IdealBatchSize {
				if err := batch.Write(); err != nil {
					return 0, err
				}
				size += int64(batch.ValueSize())
				batch.Reset()
			}
			stats.processed++
		}
		// Write everything belongs to the blocks into the database. So that
		// we can ensure all components of body is completed(body, receipts,
		// tx indexes)
		// ブロックに属するすべてのものをデータベースに書き込みます。
		// bodyのすべてのコンポーネント（body、レシート、txインデックス）が確実に完了するようにするため
		if batch.ValueSize() > 0 {
			size += int64(batch.ValueSize())
			if err := batch.Write(); err != nil {
				return 0, err
			}
		}
		updateHead(blockChain[len(blockChain)-1])
		return 0, nil
	}

	// Write downloaded chain data and corresponding receipt chain data
	// ダウンロードしたチェーンデータと対応するレシートチェーンデータを書き込む
	if len(ancientBlocks) > 0 {
		if n, err := writeAncient(ancientBlocks, ancientReceipts); err != nil {
			if err == errInsertionInterrupted {
				return 0, nil
			}
			return n, err
		}
	}
	// Write the tx index tail (block number from where we index) before write any live blocks
	// ライブブロックを書き込む前に、txインデックステール（インデックスを作成する場所からのブロック番号）を書き込みます
	if len(liveBlocks) > 0 && liveBlocks[0].NumberU64() == ancientLimit+1 {
		// The tx index tail can only be one of the following two options:
		// * 0: all ancient blocks have been indexed
		// * ancient-limit: the indices of blocks before ancient-limit are ignored
		// txインデックステールは、次の2つのオプションのいずれかになります。
		// * 0：すべての古代ブロックにインデックスが付けられました
		// * Ancient-limit：Ancient-Limitより前のブロックのインデックスは無視されます
		if tail := rawdb.ReadTxIndexTail(bc.db); tail == nil {
			if bc.txLookupLimit == 0 || ancientLimit <= bc.txLookupLimit {
				rawdb.WriteTxIndexTail(bc.db, 0)
			} else {
				rawdb.WriteTxIndexTail(bc.db, ancientLimit-bc.txLookupLimit)
			}
		}
	}
	if len(liveBlocks) > 0 {
		if n, err := writeLive(liveBlocks, liveReceipts); err != nil {
			if err == errInsertionInterrupted {
				return 0, nil
			}
			return n, err
		}
	}

	head := blockChain[len(blockChain)-1]
	context := []interface{}{
		"count", stats.processed, "elapsed", common.PrettyDuration(time.Since(start)),
		"number", head.Number(), "hash", head.Hash(), "age", common.PrettyAge(time.Unix(int64(head.Time()), 0)),
		"size", common.StorageSize(size),
	}
	if stats.ignored > 0 {
		context = append(context, []interface{}{"ignored", stats.ignored}...)
	}
	log.Info("Imported new block receipts", context...)

	return 0, nil
}

var lastWrite uint64

// writeBlockWithoutState writes only the block and its metadata to the database,
// but does not write any state. This is used to construct competing side forks
// up to the point where they exceed the canonical total difficulty.
// writeBlockWithoutStateは、ブロックとそのメタデータのみをデータベースに書き込みますが、状態は書き込みません。
// これは、標準的な合計難易度を超えるまで、競合するサイドフォークを構築するために使用されます。
func (bc *BlockChain) writeBlockWithoutState(block *types.Block, td *big.Int) (err error) {
	if bc.insertStopped() {
		return errInsertionInterrupted
	}

	batch := bc.db.NewBatch()
	rawdb.WriteTd(batch, block.Hash(), block.NumberU64(), td)
	rawdb.WriteBlock(batch, block)
	if err := batch.Write(); err != nil {
		log.Crit("Failed to write block into disk", "err", err)
	}
	return nil
}

// writeKnownBlock updates the head block flag with a known block
// and introduces chain reorg if necessary.
// writeKnownBlockは、既知のブロックでヘッドブロックフラグを更新し、必要に応じてチェーン再編成を導入します。
func (bc *BlockChain) writeKnownBlock(block *types.Block) error {
	current := bc.CurrentBlock()
	if block.ParentHash() != current.Hash() {
		if err := bc.reorg(current, block); err != nil {
			return err
		}
	}
	bc.writeHeadBlock(block)
	return nil
}

// writeBlockWithState writes block, metadata and corresponding state data to the
// database.
func (bc *BlockChain) writeBlockWithState(block *types.Block, receipts []*types.Receipt, logs []*types.Log, state *state.StateDB) error {
	// Calculate the total difficulty of the block
	// ブロックの合計難易度を計算します
	ptd := bc.GetTd(block.ParentHash(), block.NumberU64()-1)
	if ptd == nil {
		return consensus.ErrUnknownAncestor
	}
	// Make sure no inconsistent state is leaked during insertion
	// 挿入中に矛盾した状態が漏れないことを確認してください
	externTd := new(big.Int).Add(block.Difficulty(), ptd)

	// Irrelevant of the canonical status, write the block itself to the database.
	//
	// Note all the components of block(td, hash->number map, header, body, receipts)
	// should be written atomically. BlockBatch is used for containing all components.
	// 正規のステータスとは関係なく、ブロック自体をデータベースに書き込みます。
	//
	// ブロックのすべてのコンポーネント（td、ハッシュ->数値マップ、ヘッダー、本文、領収書）はアトミックに書き込む必要があることに注意してください。
	// BlockBatchは、すべてのコンポーネントを含むために使用されます。
	blockBatch := bc.db.NewBatch()
	rawdb.WriteTd(blockBatch, block.Hash(), block.NumberU64(), externTd)
	rawdb.WriteBlock(blockBatch, block)
	rawdb.WriteReceipts(blockBatch, block.Hash(), block.NumberU64(), receipts)
	rawdb.WritePreimages(blockBatch, state.Preimages())
	if err := blockBatch.Write(); err != nil {
		log.Crit("Failed to write block into disk", "err", err)
	}
	// Commit all cached state changes into underlying memory database.
	// キャッシュされたすべての状態変更を基盤となるメモリデータベースにコミットします。
	root, err := state.Commit(bc.chainConfig.IsEIP158(block.Number()))
	if err != nil {
		return err
	}
	triedb := bc.stateCache.TrieDB()

	// If we're running an archive node, always flush
	// アーカイブノードを実行している場合は、常にフラッシュします
	if bc.cacheConfig.TrieDirtyDisabled {
		return triedb.Commit(root, false, nil)
	} else {
		// Full but not archive node, do proper garbage collection
		// 完全ですが、アーカイブノードではありません。適切なガベージコレクションを実行してください
		triedb.Reference(root, common.Hash{}) // metadata reference to keep trie alive
		bc.triegc.Push(root, -int64(block.NumberU64()))

		if current := block.NumberU64(); current > TriesInMemory {
			// If we exceeded our memory allowance, flush matured singleton nodes to disk
			// メモリの許容量を超えた場合は、成熟したシングルトンノードをディスクにフラッシュします
			var (
				nodes, imgs = triedb.Size()
				limit       = common.StorageSize(bc.cacheConfig.TrieDirtyLimit) * 1024 * 1024
			)
			if nodes > limit || imgs > 4*1024*1024 {
				triedb.Cap(limit - ethdb.IdealBatchSize)
			}
			// Find the next state trie we need to commit
			// コミットする必要のある次の州のトライを見つける
			chosen := current - TriesInMemory

			// If we exceeded out time allowance, flush an entire trie to disk
			// 許容時間を超えた場合は、トライ全体をディスクにフラッシュします
			if bc.gcproc > bc.cacheConfig.TrieTimeLimit {
				// If the header is missing (canonical chain behind), we're reorging a low
				// diff sidechain. Suspend committing until this operation is completed.
				// ヘッダーが欠落している場合（正規のチェーンが後ろにある場合）、低差分サイドチェーンを再編成しています。
				// この操作が完了するまでコミットを一時停止します。
				header := bc.GetHeaderByNumber(chosen)
				if header == nil {
					log.Warn("Reorg in progress, trie commit postponed", "number", chosen)
				} else {
					// If we're exceeding limits but haven't reached a large enough memory gap,
					// warn the user that the system is becoming unstable.
					// 制限を超えているが、十分なメモリギャップに達していない場合は、
					// システムが不安定になっていることをユーザーに警告します。
					if chosen < lastWrite+TriesInMemory && bc.gcproc >= 2*bc.cacheConfig.TrieTimeLimit {
						log.Info("State in memory for too long, committing", "time", bc.gcproc, "allowance", bc.cacheConfig.TrieTimeLimit, "optimum", float64(chosen-lastWrite)/TriesInMemory)
					}
					// Flush an entire trie and restart the counters
					// トライ全体をフラッシュし、カウンターを再起動します
					triedb.Commit(header.Root, true, nil)
					lastWrite = chosen
					bc.gcproc = 0
				}
			}
			// Garbage collect anything below our required write retention
			// ガベージは、必要な書き込み保持期間を下回るものを収集します
			for !bc.triegc.Empty() {
				root, number := bc.triegc.Pop()
				if uint64(-number) > chosen {
					bc.triegc.Push(root, number)
					break
				}
				triedb.Dereference(root.(common.Hash))
			}
		}
	}
	return nil
}

// WriteBlockWithState writes the block and all associated state to the database.
func (bc *BlockChain) WriteBlockAndSetHead(block *types.Block, receipts []*types.Receipt, logs []*types.Log, state *state.StateDB, emitHeadEvent bool) (status WriteStatus, err error) {
	if !bc.chainmu.TryLock() {
		return NonStatTy, errChainStopped
	}
	defer bc.chainmu.Unlock()

	return bc.writeBlockAndSetHead(block, receipts, logs, state, emitHeadEvent)
}

// writeBlockAndSetHead writes the block and all associated state to the database,
// and also it applies the given block as the new chain head. This function expects
// the chain mutex to be held.
func (bc *BlockChain) writeBlockAndSetHead(block *types.Block, receipts []*types.Receipt, logs []*types.Log, state *state.StateDB, emitHeadEvent bool) (status WriteStatus, err error) {
	if err := bc.writeBlockWithState(block, receipts, logs, state); err != nil {
		return NonStatTy, err
	}
	currentBlock := bc.CurrentBlock()
	reorg, err := bc.forker.ReorgNeeded(currentBlock.Header(), block.Header())
	if err != nil {
		return NonStatTy, err
	}
	if reorg {
		// Reorganise the chain if the parent is not the head block
		// 親がヘッドブロックでない場合は、チェーンを再編成します
		if block.ParentHash() != currentBlock.Hash() {
			if err := bc.reorg(currentBlock, block); err != nil {
				return NonStatTy, err
			}
		}
		status = CanonStatTy
	} else {
		status = SideStatTy
	}
	// Set new head.
	// 新しいヘッドを設定します。
	if status == CanonStatTy {
		bc.writeHeadBlock(block)
	}
	bc.futureBlocks.Remove(block.Hash())

	if status == CanonStatTy {
		bc.chainFeed.Send(ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
		if len(logs) > 0 {
			bc.logsFeed.Send(logs)
		}
		// In theory we should fire a ChainHeadEvent when we inject
		// a canonical block, but sometimes we can insert a batch of
		// canonicial blocks. Avoid firing too many ChainHeadEvents,
		// we will fire an accumulated ChainHeadEvent and disable fire
		// event here.
		if emitHeadEvent {
			bc.chainHeadFeed.Send(ChainHeadEvent{Block: block})
		}
	} else {
		bc.chainSideFeed.Send(ChainSideEvent{Block: block})
	}
	return status, nil
}

// addFutureBlock checks if the block is within the max allowed window to get
// accepted for future processing, and returns an error if the block is too far
// ahead and was not added.
// addFutureBlockは、ブロックが将来の処理のために受け入れられる最大許容ウィンドウ内にあるかどうかを確認し、
// ブロックが先にあり、追加されなかった場合はエラーを返します。
//
// TODO after the transition, the future block shouldn't be kept. Because
// it's not checked in the Geth side anymore.
func (bc *BlockChain) addFutureBlock(block *types.Block) error {
	max := uint64(time.Now().Unix() + maxTimeFutureBlocks)
	if block.Time() > max {
		return fmt.Errorf("future block timestamp %v > allowed %v", block.Time(), max)
	}
	if block.Difficulty().Cmp(common.Big0) == 0 {
		// Never add PoS blocks into the future queue
		return nil
	}
	bc.futureBlocks.Add(block.Hash(), block)
	return nil
}

// InsertChain attempts to insert the given batch of blocks in to the canonical
// chain or, otherwise, create a fork. If an error is returned it will return
// the index number of the failing block as well an error describing what went
// wrong. After insertion is done, all accumulated events will be fired.
// InsertChainは、指定されたブロックのバッチを正規チェーンに挿入しようとします。
// そうでない場合は、フォークを作成します。
// エラーが返された場合は、失敗したブロックのインデックス番号と、何が問題だったかを説明するエラーが返されます。
//
// 挿入が完了すると、蓄積されたすべてのイベントが発生します。
func (bc *BlockChain) InsertChain(chain types.Blocks) (int, error) {
	// Sanity check that we have something meaningful to import
	// インポートする意味のあるものがあることの健全性チェック
	if len(chain) == 0 {
		return 0, nil
	}
	bc.blockProcFeed.Send(true)
	defer bc.blockProcFeed.Send(false)

	// Do a sanity check that the provided chain is actually ordered and linked.
	// 提供されたチェーンが実際に注文され、リンクされていることを健全性チェックします。
	for i := 1; i < len(chain); i++ {
		block, prev := chain[i], chain[i-1]
		if block.NumberU64() != prev.NumberU64()+1 || block.ParentHash() != prev.Hash() {
			log.Error("Non contiguous block insert",
				"number", block.Number(),
				"hash", block.Hash(),
				"parent", block.ParentHash(),
				"prevnumber", prev.Number(),
				"prevhash", prev.Hash(),
			)
			return 0, fmt.Errorf("non contiguous insert: item %d is #%d [%x..], item %d is #%d [%x..] (parent [%x..])", i-1, prev.NumberU64(),
				prev.Hash().Bytes()[:4], i, block.NumberU64(), block.Hash().Bytes()[:4], block.ParentHash().Bytes()[:4])
		}
	}
	// Pre-checks passed, start the full block imports
	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()
	return bc.insertChain(chain, true, true)
}

// insertChain is the internal implementation of InsertChain, which assumes that
// 1) chains are contiguous, and 2) The chain mutex is held.
//
// This method is split out so that import batches that require re-injecting
// historical blocks can do so without releasing the lock, which could lead to
// racey behaviour. If a sidechain import is in progress, and the historic state
// is imported, but then new canon-head is added before the actual sidechain
// completes, then the historic state could be pruned again
// insertChainは、InsertChainの内部実装であり、
// 1）チェーンは隣接しており、2）チェーンミューテックスは保持されています。
//
// このメソッドは分割されているため、履歴ブロックの再挿入が必要なインポートバッチは、ロックを解除せずに再挿入できます。
// これにより、競合が発生する可能性があります。サイドチェーンのインポートが進行中で、履歴状態がインポートされたが、
// 実際のサイドチェーンが完了する前に新しいcanon-headが追加された場合、履歴状態が再度プルーニングされる可能性があります。
func (bc *BlockChain) insertChain(chain types.Blocks, verifySeals, setHead bool) (int, error) {
	// If the chain is terminating, don't even bother starting up.
	// チェーンが終了している場合は、わざわざ起動する必要はありません。
	if bc.insertStopped() {
		return 0, nil
	}

	// Start a parallel signature recovery (signer will fluke on fork transition, minimal perf loss)
	// 並列署名回復を開始します（署名者はフォーク遷移で失敗し、パフォーマンスの損失を最小限に抑えます）
	senderCacher.recoverFromBlocks(types.MakeSigner(bc.chainConfig, chain[0].Number()), chain)

	var (
		stats     = insertStats{startTime: mclock.Now()}
		lastCanon *types.Block
	)
	// Fire a single chain head event if we've progressed the chain
	// チェーンが進行した場合は、単一のチェーンヘッドイベントを発生させます
	defer func() {
		if lastCanon != nil && bc.CurrentBlock().Hash() == lastCanon.Hash() {
			bc.chainHeadFeed.Send(ChainHeadEvent{lastCanon})
		}
	}()
	// Start the parallel header verifier
	// 並列ヘッダーベリファイアを開始します
	headers := make([]*types.Header, len(chain))
	seals := make([]bool, len(chain))

	for i, block := range chain {
		headers[i] = block.Header()
		seals[i] = verifySeals
	}
	abort, results := bc.engine.VerifyHeaders(bc, headers, seals, bc.db)
	defer close(abort)

	// Peek the error for the first block to decide the directing import logic
	// 最初のブロックのエラーを確認して、転送インポートロジックを決定します
	it := newInsertIterator(chain, results, bc.validator)
	block, err := it.next()

	// Left-trim all the known blocks that don't need to build snapshot
	// スナップショットを作成する必要のないすべての既知のブロックを左トリミングします
	if bc.skipBlock(err, it) {
		// First block (and state) is known
		//   1. We did a roll-back, and should now do a re-import
		//   2. The block is stored as a sidechain, and is lying about it's stateroot, and passes a stateroot
		//      from the canonical chain, which has not been verified.
		// Skip all known blocks that are behind us.

		// 最初のブロック（および状態）は既知です
		// 1.ロールバックを実行したので、再インポートを実行する必要があります
		// 2.ブロックはサイドチェーンとして保存され、そのstaterootの周りにあり、
		// 検証されていない正規チェーンからstaterootを渡します。
		// 背後にあるすべての既知のブロックをスキップします。
		var (
			reorg   bool
			current = bc.CurrentBlock()
		)
		for block != nil && bc.skipBlock(err, it) {
			reorg, err = bc.forker.ReorgNeeded(current.Header(), block.Header())
			if err != nil {
				return it.index, err
			}
			if reorg {
				// Switch to import mode if the forker says the reorg is necessary
				// and also the block is not on the canonical chain.
				// In eth2 the forker always returns true for reorg decision (blindly trusting
				// the external consensus engine), but in order to prevent the unnecessary
				// reorgs when importing known blocks, the special case is handled here.
				if block.NumberU64() > current.NumberU64() || bc.GetCanonicalHash(block.NumberU64()) != block.Hash() {
					break
				}
			}
			log.Debug("Ignoring already known block", "number", block.Number(), "hash", block.Hash())
			stats.ignored++

			block, err = it.next()
		}
		// The remaining blocks are still known blocks, the only scenario here is:
		// During the fast sync, the pivot point is already submitted but rollback
		// happens. Then node resets the head full block to a lower height via `rollback`
		// and leaves a few known blocks in the database.
		//
		// When node runs a fast sync again, it can re-import a batch of known blocks via
		// `insertChain` while a part of them have higher total difficulty than current
		// head full block(new pivot point).
		// 残りのブロックはまだ既知のブロックです。ここでの唯一のシナリオは次のとおりです。
		// 高速同期中に、ピボットポイントはすでに送信されていますが、ロールバックが発生します。
		// 次に、ノードは「ロールバック」を介してヘッドフルブロックをより低い高さにリセットし、
		// データベースにいくつかの既知のブロックを残します。
		//
		// ノードが再び高速同期を実行すると、ノードは `insertChain`を介して既知のブロックのバッチを再インポートできますが、
		// それらの一部は現在のヘッドフルブロック（新しいピボットポイント）よりも全体的な難易度が高くなります。
		for block != nil && bc.skipBlock(err, it) {
			log.Debug("Writing previously known block", "number", block.Number(), "hash", block.Hash())
			if err := bc.writeKnownBlock(block); err != nil {
				return it.index, err
			}
			lastCanon = block

			block, err = it.next()
		}
		// Falls through to the block import
		// ブロックインポートにフォールスルー
	}
	switch {
	// First block is pruned
	case errors.Is(err, consensus.ErrPrunedAncestor):
		if setHead {
			// First block is pruned, insert as sidechain and reorg only if TD grows enough
			log.Debug("Pruned ancestor, inserting as sidechain", "number", block.Number(), "hash", block.Hash())
			return bc.insertSideChain(block, it)
		} else {
			// We're post-merge and the parent is pruned, try to recover the parent state
			log.Debug("Pruned ancestor", "number", block.Number(), "hash", block.Hash())
			return it.index, bc.recoverAncestors(block)
		}
	// First block is future, shove it (and all children) to the future queue (unknown ancestor)
	// 最初のブロックはfutureであり、それ（およびすべての子）をfutureキュー（不明な祖先）にプッシュします
	case errors.Is(err, consensus.ErrFutureBlock) || (errors.Is(err, consensus.ErrUnknownAncestor) && bc.futureBlocks.Contains(it.first().ParentHash())):
		for block != nil && (it.index == 0 || errors.Is(err, consensus.ErrUnknownAncestor)) {
			log.Debug("Future block, postponing import", "number", block.Number(), "hash", block.Hash())
			if err := bc.addFutureBlock(block); err != nil {
				return it.index, err
			}
			block, err = it.next()
		}
		stats.queued += it.processed()
		stats.ignored += it.remaining()

		// If there are any still remaining, mark as ignored
		// まだ残っている場合は、無視済みとしてマークします
		return it.index, err

	// Some other error(except ErrKnownBlock) occurred, abort.
	// ErrKnownBlock is allowed here since some known blocks
	// still need re-execution to generate snapshots that are missing
	// 他のエラー（ErrKnownBlockを除く）が発生しました。中止してください。
	// ErrKnownBlockはここで許可されます。これは、欠落しているスナップショットを生成するために、
	// いくつかの既知のブロックを再実行する必要があるためです。
	case err != nil && !errors.Is(err, ErrKnownBlock):
		bc.futureBlocks.Remove(block.Hash())
		stats.ignored += len(it.chain)
		bc.reportBlock(block, nil, err)
		return it.index, err
	}
	// No validation errors for the first block (or chain prefix skipped)
	// 最初のブロックの検証エラーはありません（またはチェーンプレフィックスはスキップされます）
	var activeState *state.StateDB
	defer func() {
		// The chain importer is starting and stopping trie prefetchers. If a bad
		// block or other error is hit however, an early return may not properly
		// terminate the background threads. This defer ensures that we clean up
		// and dangling prefetcher, without defering each and holding on live refs.
		// チェーンインポーターはtrieプリフェッチャーを開始および停止しています。
		// ただし、不良ブロックまたはその他のエラーが発生した場合、
		// 早期に戻るとバックグラウンドスレッドが適切に終了しない可能性があります。
		// この延期により、プリフェッチャーをそれぞれ延期してライブ参照を保持することなく、
		// プリフェッチャーをクリーンアップしてぶら下げることができます。
		if activeState != nil {
			activeState.StopPrefetcher()
		}
	}()

	for ; block != nil && err == nil || errors.Is(err, ErrKnownBlock); block, err = it.next() {
		// If the chain is terminating, stop processing blocks
		// チェーンが終了している場合は、ブロックの処理を停止します
		if bc.insertStopped() {
			log.Debug("Abort during block processing")
			break
		}
		// If the header is a banned one, straight out abort
		// ヘッダーが禁止されている場合は、すぐに中止します
		if BadHashes[block.Hash()] {
			bc.reportBlock(block, nil, ErrBannedHash)
			return it.index, ErrBannedHash
		}
		// If the block is known (in the middle of the chain), it's a special case for
		// Clique blocks where they can share state among each other, so importing an
		// older block might complete the state of the subsequent one. In this case,
		// just skip the block (we already validated it once fully (and crashed), since
		// its header and body was already in the database). But if the corresponding
		// snapshot layer is missing, forcibly rerun the execution to build it.
		// ブロックが既知の場合（チェーンの途中）、クリークブロックの特殊なケースであり、
		// 相互に状態を共有できるため、古いブロックをインポートすると、次のブロックの状態が完了する可能性があります。
		// この場合、ブロックをスキップするだけです（ヘッダーと本体がすでにデータベースにあるため、
		// ブロックを完全に検証しました（そしてクラッシュしました）。
		// ただし、対応するスナップショットレイヤーが欠落している場合は、実行を強制的に再実行してビルドします。
		if bc.skipBlock(err, it) {
			logger := log.Debug
			if bc.chainConfig.Clique == nil {
				logger = log.Warn
			}
			logger("Inserted known block", "number", block.Number(), "hash", block.Hash(),
				"uncles", len(block.Uncles()), "txs", len(block.Transactions()), "gas", block.GasUsed(),
				"root", block.Root())

			// Special case. Commit the empty receipt slice if we meet the known
			// block in the middle. It can only happen in the clique chain. Whenever
			// we insert blocks via `insertSideChain`, we only commit `td`, `header`
			// and `body` if it's non-existent. Since we don't have receipts without
			// reexecution, so nothing to commit. But if the sidechain will be adpoted
			// as the canonical chain eventually, it needs to be reexecuted for missing
			// state, but if it's this special case here(skip reexecution) we will lose
			// the empty receipt entry.
			// 特別なケース。途中で既知のブロックに遭遇した場合は、空のレシートスライスをコミットします。
			// それはクリークチェーンでのみ発生する可能性があります。
			//  `insertSideChain`を介してブロックを挿入するときは常に、存在しない場合にのみ、
			// ` td`、 `header`、および` body`をコミットします。再実行せずに領収書を持っていないので、
			// コミットするものは何もありません。
			// ただし、サイドチェーンが最終的に正規チェーンとして採用される場合は、
			// 状態が欠落しているために再実行する必要がありますが、この特殊なケース（再実行をスキップ）の場合は、
			// 空のレシートエントリが失われます。
			if len(block.Transactions()) == 0 {
				rawdb.WriteReceipts(bc.db, block.Hash(), block.NumberU64(), nil)
			} else {
				log.Error("Please file an issue, skip known block execution without receipt",
					"hash", block.Hash(), "number", block.NumberU64())
			}
			if err := bc.writeKnownBlock(block); err != nil {
				return it.index, err
			}
			stats.processed++

			// We can assume that logs are empty here, since the only way for consecutive
			// Clique blocks to have the same state is if there are no transactions.
			// 連続するCliqueブロックが同じ状態になる唯一の方法はトランザクションがない場合であるため、
			// ここではログが空であると想定できます。
			lastCanon = block
			continue
		}

		// Retrieve the parent block and it's state to execute on top
		// 親ブロックとその上で実行する状態を取得します
		start := time.Now()
		parent := it.previous()
		if parent == nil {
			parent = bc.GetHeader(block.ParentHash(), block.NumberU64()-1)
		}
		statedb, err := state.New(parent.Root, bc.stateCache, bc.snaps)
		if err != nil {
			return it.index, err
		}

		// Enable prefetching to pull in trie node paths while processing transactions
		// トランザクションの処理中に、プリフェッチを有効にしてトライノードパスをプルします
		statedb.StartPrefetcher("chain")
		activeState = statedb

		// If we have a followup block, run that against the current state to pre-cache
		// transactions and probabilistically some of the account/storage trie nodes.
		// フォローアップブロックがある場合は、それを現在の状態に対して実行して、
		// トランザクションと確率的にいくつかのアカウント/ストレージトライノードを事前にキャッシュします。
		var followupInterrupt uint32
		if !bc.cacheConfig.TrieCleanNoPrefetch {
			if followup, err := it.peek(); followup != nil && err == nil {
				throwaway, _ := state.New(parent.Root, bc.stateCache, bc.snaps)

				go func(start time.Time, followup *types.Block, throwaway *state.StateDB, interrupt *uint32) {
					bc.prefetcher.Prefetch(followup, throwaway, bc.vmConfig, &followupInterrupt)

					blockPrefetchExecuteTimer.Update(time.Since(start))
					if atomic.LoadUint32(interrupt) == 1 {
						blockPrefetchInterruptMeter.Mark(1)
					}
				}(time.Now(), followup, throwaway, &followupInterrupt)
			}
		}

		// Process block using the parent state as reference point
		// 親の状態を参照点として使用してブロックを処理します
		substart := time.Now()
		receipts, logs, usedGas, err := bc.processor.Process(block, statedb, bc.vmConfig)
		if err != nil {
			bc.reportBlock(block, receipts, err)
			atomic.StoreUint32(&followupInterrupt, 1)
			return it.index, err
		}

		// Update the metrics touched during block processing
		// ブロック処理中に触れられたメトリックを更新します
		accountReadTimer.Update(statedb.AccountReads)                 // Account reads are complete, we can mark them   // アカウントの読み取りが完了しました。マークを付けることができます
		storageReadTimer.Update(statedb.StorageReads)                 // Storage reads are complete, we can mark them   // ストレージの読み取りが完了しました。マークを付けることができます
		accountUpdateTimer.Update(statedb.AccountUpdates)             // Account updates are complete, we can mark them // アカウントの更新が完了しました。マークを付けることができます
		storageUpdateTimer.Update(statedb.StorageUpdates)             // Storage updates are complete, we can mark them // ストレージの更新が完了しました。マークを付けることができます
		snapshotAccountReadTimer.Update(statedb.SnapshotAccountReads) // Account reads are complete, we can mark them   // アカウントの読み取りが完了しました、マークを付けることができます
		snapshotStorageReadTimer.Update(statedb.SnapshotStorageReads) // Storage reads are complete, we can mark them   // ストレージの読み取りが完了しました。マークを付けることができます
		triehash := statedb.AccountHashes + statedb.StorageHashes     // Save to not double count in validation         // 検証で二重にカウントされないように保存します
		trieproc := statedb.SnapshotAccountReads + statedb.AccountReads + statedb.AccountUpdates
		trieproc += statedb.SnapshotStorageReads + statedb.StorageReads + statedb.StorageUpdates

		blockExecutionTimer.Update(time.Since(substart) - trieproc - triehash)

		// Validate the state using the default validator
		// デフォルトのバリデーターを使用して状態を検証します
		substart = time.Now()
		if err := bc.validator.ValidateState(block, statedb, receipts, usedGas); err != nil {
			bc.reportBlock(block, receipts, err)
			atomic.StoreUint32(&followupInterrupt, 1)
			return it.index, err
		}
		proctime := time.Since(start)

		// Update the metrics touched during block validation
		// ブロックの検証中に触れられたメトリックを更新します
		accountHashTimer.Update(statedb.AccountHashes) // Account hashes are complete, we can mark them // アカウントのハッシュが完了しました、マークを付けることができます
		storageHashTimer.Update(statedb.StorageHashes) // Storage hashes are complete, we can mark them // ストレージハッシュが完了しました。マークを付けることができます

		blockValidationTimer.Update(time.Since(substart) - (statedb.AccountHashes + statedb.StorageHashes - triehash))

		// Write the block to the chain and get the status.
		// ブロックをチェーンに書き込み、ステータスを取得します。
		substart = time.Now()
		var status WriteStatus
		if !setHead {
			// Don't set the head, only insert the block
			err = bc.writeBlockWithState(block, receipts, logs, statedb)
		} else {
			status, err = bc.writeBlockAndSetHead(block, receipts, logs, statedb, false)
		}
		atomic.StoreUint32(&followupInterrupt, 1)
		if err != nil {
			return it.index, err
		}
		// Update the metrics touched during block commit
		accountCommitTimer.Update(statedb.AccountCommits)   // Account commits are complete, we can mark them
		storageCommitTimer.Update(statedb.StorageCommits)   // Storage commits are complete, we can mark them
		snapshotCommitTimer.Update(statedb.SnapshotCommits) // Snapshot commits are complete, we can mark them

		blockWriteTimer.Update(time.Since(substart) - statedb.AccountCommits - statedb.StorageCommits - statedb.SnapshotCommits)
		blockInsertTimer.UpdateSince(start)

		if !setHead {
			// We did not setHead, so we don't have any stats to update
			log.Info("Inserted block", "number", block.Number(), "hash", block.Hash(), "txs", len(block.Transactions()), "elapsed", common.PrettyDuration(time.Since(start)))
			return it.index, nil
		}

		switch status {
		case CanonStatTy:
			log.Debug("Inserted new block", "number", block.Number(), "hash", block.Hash(),
				"uncles", len(block.Uncles()), "txs", len(block.Transactions()), "gas", block.GasUsed(),
				"elapsed", common.PrettyDuration(time.Since(start)),
				"root", block.Root())

			lastCanon = block

			// Only count canonical blocks for GC processing time
			// GC処理時間の正規ブロックのみをカウントします
			bc.gcproc += proctime

		case SideStatTy:
			log.Debug("Inserted forked block", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())

		default:
			// This in theory is impossible, but lets be nice to our future selves and leave
			// a log, instead of trying to track down blocks imports that don't emit logs.
			// これは理論的には不可能ですが、ログを発行しないブロックのインポートを追跡するのではなく、
			// 将来の自分に役立ち、ログを残すことができます。
			log.Warn("Inserted block with unknown status", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())
		}
		stats.processed++
		stats.usedGas += usedGas

		dirty, _ := bc.stateCache.TrieDB().Size()
		stats.report(chain, it.index, dirty)
	}

	// Any blocks remaining here? The only ones we care about are the future ones
	// ここに残っているブロックはありますか？私たちが気にするのは未来のものだけです
	if block != nil && errors.Is(err, consensus.ErrFutureBlock) {
		if err := bc.addFutureBlock(block); err != nil {
			return it.index, err
		}
		block, err = it.next()

		for ; block != nil && errors.Is(err, consensus.ErrUnknownAncestor); block, err = it.next() {
			if err := bc.addFutureBlock(block); err != nil {
				return it.index, err
			}
			stats.queued++
		}
	}
	stats.ignored += it.remaining()

	return it.index, err
}

// insertSideChain is called when an import batch hits upon a pruned ancestor
// error, which happens when a sidechain with a sufficiently old fork-block is
// found.
//
// The method writes all (header-and-body-valid) blocks to disk, then tries to
// switch over to the new chain if the TD exceeded the current chain.
// insertSideChain is only used pre-merge.
func (bc *BlockChain) insertSideChain(block *types.Block, it *insertIterator) (int, error) {
	var (
		externTd  *big.Int
		lastBlock = block
		current   = bc.CurrentBlock()
	)
	// The first sidechain block error is already verified to be ErrPrunedAncestor.
	// Since we don't import them here, we expect ErrUnknownAncestor for the remaining
	// ones. Any other errors means that the block is invalid, and should not be written
	// to disk.
	// 最初のサイドチェーンブロックエラーは、ErrPrunedAncestorであることがすでに確認されています。
	// ここではインポートしないため、残りのものにはErrUnknownAncestorが必要です。
	// その他のエラーは、ブロックが無効であることを意味し、ディスクに書き込むべきではありません。
	err := consensus.ErrPrunedAncestor
	for ; block != nil && errors.Is(err, consensus.ErrPrunedAncestor); block, err = it.next() {
		// Check the canonical state root for that number
		// その番号の正規状態ルートを確認します
		if number := block.NumberU64(); current.NumberU64() >= number {
			canonical := bc.GetBlockByNumber(number)
			if canonical != nil && canonical.Hash() == block.Hash() {
				// Not a sidechain block, this is a re-import of a canon block which has it's state pruned
				// サイドチェーンブロックではありません。これは、状態がプルーニングされたCanonブロックの再インポートです。

				// Collect the TD of the block. Since we know it's a canon one,
				// we can get it directly, and not (like further below) use
				// the parent and then add the block on top
				// ブロックのTDを収集します。これは標準的なものであることがわかっているので、直接取得できます。
				//（以下のように）親を使用してからブロックを上に追加することはできません。
				externTd = bc.GetTd(block.Hash(), block.NumberU64())
				continue
			}
			if canonical != nil && canonical.Root() == block.Root() {
				// This is most likely a shadow-state attack. When a fork is imported into the
				// database, and it eventually reaches a block height which is not pruned, we
				// just found that the state already exist! This means that the sidechain block
				// refers to a state which already exists in our canon chain.
				//
				// If left unchecked, we would now proceed importing the blocks, without actually
				// having verified the state of the previous blocks.
				// これはシャドウステート攻撃である可能性が高いです。
				// フォークがデータベースにインポートされ、最終的にプルーニングされていないブロックの高さに達すると、
				// 状態がすでに存在していることがわかりました。
				// これは、サイドチェーンブロックがキャノンチェーンにすでに存在する状態を参照していることを意味します。
				//
				// チェックを外したままにすると、前のブロックの状態を実際に確認せずに、ブロックのインポートを続行します。
				log.Warn("Sidechain ghost-state attack detected", "number", block.NumberU64(), "sideroot", block.Root(), "canonroot", canonical.Root())

				// If someone legitimately side-mines blocks, they would still be imported as usual. However,
				// we cannot risk writing unverified blocks to disk when they obviously target the pruning
				// mechanism.
				// 誰かが合法的にブロックをサイドマイニングした場合でも、通常どおりインポートされます。
				// しかし、未確認のブロックが明らかにプルーニングメカニズムを対象としている場合、
				// それらをディスクに書き込むリスクを冒すことはできません。
				return it.index, errors.New("sidechain ghost-state attack")
			}
		}
		if externTd == nil {
			externTd = bc.GetTd(block.ParentHash(), block.NumberU64()-1)
		}
		externTd = new(big.Int).Add(externTd, block.Difficulty())

		if !bc.HasBlock(block.Hash(), block.NumberU64()) {
			start := time.Now()
			if err := bc.writeBlockWithoutState(block, externTd); err != nil {
				return it.index, err
			}
			log.Debug("Injected sidechain block", "number", block.Number(), "hash", block.Hash(),
				"diff", block.Difficulty(), "elapsed", common.PrettyDuration(time.Since(start)),
				"txs", len(block.Transactions()), "gas", block.GasUsed(), "uncles", len(block.Uncles()),
				"root", block.Root())
		}
		lastBlock = block
	}
	// At this point, we've written all sidechain blocks to database. Loop ended
	// either on some other error or all were processed. If there was some other
	// error, we can ignore the rest of those blocks.
	//
	// If the externTd was larger than our local TD, we now need to reimport the previous
	// blocks to regenerate the required state
	// この時点で、すべてのサイドチェーンブロックをデータベースに書き込みました。
	// 他のエラーでループが終了したか、すべてが処理されました。他のエラーが発生した場合は、それらのブロックの残りを無視できます。
	//
	// externTdがローカルTDよりも大きかった場合、
	// 必要な状態を再生成するために前のブロックを再インポートする必要があります
	reorg, err := bc.forker.ReorgNeeded(current.Header(), lastBlock.Header())
	if err != nil {
		return it.index, err
	}
	if !reorg {
		localTd := bc.GetTd(current.Hash(), current.NumberU64())
		log.Info("Sidechain written to disk", "start", it.first().NumberU64(), "end", it.previous().Number, "sidetd", externTd, "localtd", localTd)
		return it.index, err
	}
	// Gather all the sidechain hashes (full blocks may be memory heavy)
	// すべてのサイドチェーンハッシュを収集します（完全なブロックはメモリを大量に消費する可能性があります）
	var (
		hashes  []common.Hash
		numbers []uint64
	)
	parent := it.previous()
	for parent != nil && !bc.HasState(parent.Root) {
		hashes = append(hashes, parent.Hash())
		numbers = append(numbers, parent.Number.Uint64())

		parent = bc.GetHeader(parent.ParentHash, parent.Number.Uint64()-1)
	}
	if parent == nil {
		return it.index, errors.New("missing parent")
	}
	// Import all the pruned blocks to make the state available
	// プルーニングされたすべてのブロックをインポートして、状態を使用可能にします
	var (
		blocks []*types.Block
		memory common.StorageSize
	)
	for i := len(hashes) - 1; i >= 0; i-- {
		// Append the next block to our batch
		// 次のブロックをバッチに追加します
		block := bc.GetBlock(hashes[i], numbers[i])

		blocks = append(blocks, block)
		memory += block.Size()

		// If memory use grew too large, import and continue. Sadly we need to discard
		// all raised events and logs from notifications since we're too heavy on the
		// memory here.
		// メモリ使用量が大きくなりすぎた場合は、インポートして続行します。
		// 残念ながら、ここではメモリが多すぎるため、発生したすべてのイベントとログを通知から破棄する必要があります。
		if len(blocks) >= 2048 || memory > 64*1024*1024 {
			log.Info("Importing heavy sidechain segment", "blocks", len(blocks), "start", blocks[0].NumberU64(), "end", block.NumberU64())
			if _, err := bc.insertChain(blocks, false, true); err != nil {
				return 0, err
			}
			blocks, memory = blocks[:0], 0

			// If the chain is terminating, stop processing blocks
			// チェーンが終了している場合は、ブロックの処理を停止します
			if bc.insertStopped() {
				log.Debug("Abort during blocks processing")
				return 0, nil
			}
		}
	}
	if len(blocks) > 0 {
		log.Info("Importing sidechain segment", "start", blocks[0].NumberU64(), "end", blocks[len(blocks)-1].NumberU64())
		return bc.insertChain(blocks, false, true)
	}
	return 0, nil
}

// recoverAncestors finds the closest ancestor with available state and re-execute
// all the ancestor blocks since that.
// recoverAncestors is only used post-merge.
func (bc *BlockChain) recoverAncestors(block *types.Block) error {
	// Gather all the sidechain hashes (full blocks may be memory heavy)
	var (
		hashes  []common.Hash
		numbers []uint64
		parent  = block
	)
	for parent != nil && !bc.HasState(parent.Root()) {
		hashes = append(hashes, parent.Hash())
		numbers = append(numbers, parent.NumberU64())
		parent = bc.GetBlock(parent.ParentHash(), parent.NumberU64()-1)

		// If the chain is terminating, stop iteration
		if bc.insertStopped() {
			log.Debug("Abort during blocks iteration")
			return errInsertionInterrupted
		}
	}
	if parent == nil {
		return errors.New("missing parent")
	}
	// Import all the pruned blocks to make the state available
	for i := len(hashes) - 1; i >= 0; i-- {
		// If the chain is terminating, stop processing blocks
		if bc.insertStopped() {
			log.Debug("Abort during blocks processing")
			return errInsertionInterrupted
		}
		var b *types.Block
		if i == 0 {
			b = block
		} else {
			b = bc.GetBlock(hashes[i], numbers[i])
		}
		if _, err := bc.insertChain(types.Blocks{b}, false, false); err != nil {
			return err
		}
	}
	return nil
}

// collectLogs collects the logs that were generated or removed during
// the processing of the block that corresponds with the given hash.
// These logs are later announced as deleted or reborn.
func (bc *BlockChain) collectLogs(hash common.Hash, removed bool) []*types.Log {
	number := bc.hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	receipts := rawdb.ReadReceipts(bc.db, hash, *number, bc.chainConfig)

	var logs []*types.Log
	for _, receipt := range receipts {
		for _, log := range receipt.Logs {
			l := *log
			if removed {
				l.Removed = true
			}
			logs = append(logs, &l)
		}
	}
	return logs
}

// mergeLogs returns a merged log slice with specified sort order.
func mergeLogs(logs [][]*types.Log, reverse bool) []*types.Log {
	var ret []*types.Log
	if reverse {
		for i := len(logs) - 1; i >= 0; i-- {
			ret = append(ret, logs[i]...)
		}
	} else {
		for i := 0; i < len(logs); i++ {
			ret = append(ret, logs[i]...)
		}
	}
	return ret
}

// reorg takes two blocks, an old chain and a new chain and will reconstruct the
// blocks and inserts them to be part of the new canonical chain and accumulates
// potential missing transactions and post an event about them.
// Note the new head block won't be processed here, callers need to handle it
// externally.
func (bc *BlockChain) reorg(oldBlock, newBlock *types.Block) error {
	var (
		newChain    types.Blocks
		oldChain    types.Blocks
		commonBlock *types.Block

		deletedTxs types.Transactions
		addedTxs   types.Transactions

		deletedLogs [][]*types.Log
		rebirthLogs [][]*types.Log
	)
	// Reduce the longer chain to the same number as the shorter one
	// 長いチェーンを短いチェーンと同じ数に減らします
	if oldBlock.NumberU64() > newBlock.NumberU64() {
		// Old chain is longer, gather all transactions and logs as deleted ones
		// 古いチェーンは長くなり、すべてのトランザクションとログを削除されたものとして収集します
		for ; oldBlock != nil && oldBlock.NumberU64() != newBlock.NumberU64(); oldBlock = bc.GetBlock(oldBlock.ParentHash(), oldBlock.NumberU64()-1) {
			oldChain = append(oldChain, oldBlock)
			deletedTxs = append(deletedTxs, oldBlock.Transactions()...)

			// Collect deleted logs for notification
			logs := bc.collectLogs(oldBlock.Hash(), true)
			if len(logs) > 0 {
				deletedLogs = append(deletedLogs, logs)
			}
		}
	} else {
		// New chain is longer, stash all blocks away for subsequent insertion
		// 新しいチェーンは長くなり、後続の挿入のためにすべてのブロックを隠します
		for ; newBlock != nil && newBlock.NumberU64() != oldBlock.NumberU64(); newBlock = bc.GetBlock(newBlock.ParentHash(), newBlock.NumberU64()-1) {
			newChain = append(newChain, newBlock)
		}
	}
	if oldBlock == nil {
		return fmt.Errorf("invalid old chain")
	}
	if newBlock == nil {
		return fmt.Errorf("invalid new chain")
	}
	// Both sides of the reorg are at the same number, reduce both until the common
	// ancestor is found
	// 再編成の両側は同じ数であり、共通の祖先が見つかるまで両方を減らします
	for {
		// If the common ancestor was found, bail out
		// 共通の祖先が見つかった場合は、救済します
		if oldBlock.Hash() == newBlock.Hash() {
			commonBlock = oldBlock
			break
		}
		// Remove an old block as well as stash away a new block
		// 古いブロックを削除し、新しいブロックを隠します
		oldChain = append(oldChain, oldBlock)
		deletedTxs = append(deletedTxs, oldBlock.Transactions()...)

		// Collect deleted logs for notification
		logs := bc.collectLogs(oldBlock.Hash(), true)
		if len(logs) > 0 {
			deletedLogs = append(deletedLogs, logs)
		}
		newChain = append(newChain, newBlock)

		// Step back with both chains
		// 両方のチェーンで後退します
		oldBlock = bc.GetBlock(oldBlock.ParentHash(), oldBlock.NumberU64()-1)
		if oldBlock == nil {
			return fmt.Errorf("invalid old chain")
		}
		newBlock = bc.GetBlock(newBlock.ParentHash(), newBlock.NumberU64()-1)
		if newBlock == nil {
			return fmt.Errorf("invalid new chain")
		}
	}
	// Ensure the user sees large reorgs
	// ユーザーに大規模な再編成が表示されるようにします
	if len(oldChain) > 0 && len(newChain) > 0 {
		logFn := log.Info
		msg := "Chain reorg detected"
		if len(oldChain) > 63 {
			msg = "Large chain reorg detected"
			logFn = log.Warn
		}
		logFn(msg, "number", commonBlock.Number(), "hash", commonBlock.Hash(),
			"drop", len(oldChain), "dropfrom", oldChain[0].Hash(), "add", len(newChain), "addfrom", newChain[0].Hash())
		blockReorgAddMeter.Mark(int64(len(newChain)))
		blockReorgDropMeter.Mark(int64(len(oldChain)))
		blockReorgMeter.Mark(1)
	} else if len(newChain) > 0 {
		// Special case happens in the post merge stage that current head is
		// the ancestor of new head while these two blocks are not consecutive
		log.Info("Extend chain", "add", len(newChain), "number", newChain[0].NumberU64(), "hash", newChain[0].Hash())
		blockReorgAddMeter.Mark(int64(len(newChain)))
	} else {
		// len(newChain) == 0 && len(oldChain) > 0
		// rewind the canonical chain to a lower point.
		log.Error("Impossible reorg, please file an issue", "oldnum", oldBlock.Number(), "oldhash", oldBlock.Hash(), "oldblocks", len(oldChain), "newnum", newBlock.Number(), "newhash", newBlock.Hash(), "newblocks", len(newChain))
	}
	// Insert the new chain(except the head block(reverse order)),
	// taking care of the proper incremental order.
	// 新しいチェーンを挿入し（ヘッドブロック（逆順）を除く）、適切な増分順序に注意します。
	for i := len(newChain) - 1; i >= 1; i-- {
		// Insert the block in the canonical way, re-writing history
		// 正規の方法でブロックを挿入し、履歴を書き換えます
		bc.writeHeadBlock(newChain[i])

		// Collect reborn logs due to chain reorg
		// チェーンの再編成により生まれ変わったログを収集します
		logs := bc.collectLogs(newChain[i].Hash(), false)
		if len(logs) > 0 {
			rebirthLogs = append(rebirthLogs, logs)
		}
		// Collect the new added transactions.
		// 新しく追加されたトランザクションを収集します。
		addedTxs = append(addedTxs, newChain[i].Transactions()...)
	}
	// Delete useless indexes right now which includes the non-canonical
	// transaction indexes, canonical chain indexes which above the head.
	// 非正規トランザクションインデックス、頭上にある正規チェーンインデックスを含む不要なインデックスを今すぐ削除します。
	indexesBatch := bc.db.NewBatch()
	for _, tx := range types.TxDifference(deletedTxs, addedTxs) {
		rawdb.DeleteTxLookupEntry(indexesBatch, tx.Hash())
	}
	// Delete any canonical number assignments above the new head
	// 新しい頭の上にある正規の番号の割り当てを削除します
	number := bc.CurrentBlock().NumberU64()
	for i := number + 1; ; i++ {
		hash := rawdb.ReadCanonicalHash(bc.db, i)
		if hash == (common.Hash{}) {
			break
		}
		rawdb.DeleteCanonicalHash(indexesBatch, i)
	}
	if err := indexesBatch.Write(); err != nil {
		log.Crit("Failed to delete useless indexes", "err", err)
	}
	// If any logs need to be fired, do it now. In theory we could avoid creating
	// this goroutine if there are no events to fire, but realistcally that only
	// ever happens if we're reorging empty blocks, which will only happen on idle
	// networks where performance is not an issue either way.
	// ログを起動する必要がある場合は、今すぐ実行してください。
	// 理論的には、起動するイベントがない場合はこのゴルーチンの作成を回避できますが、
	// 実際には、空のブロックを再編成する場合にのみ発生します。
	// これは、パフォーマンスがどちらの方法でも問題にならないアイドル状態のネットワークでのみ発生します。
	if len(deletedLogs) > 0 {
		bc.rmLogsFeed.Send(RemovedLogsEvent{mergeLogs(deletedLogs, true)})
	}
	if len(rebirthLogs) > 0 {
		bc.logsFeed.Send(mergeLogs(rebirthLogs, false))
	}
	if len(oldChain) > 0 {
		for i := len(oldChain) - 1; i >= 0; i-- {
			bc.chainSideFeed.Send(ChainSideEvent{Block: oldChain[i]})
		}
	}
	return nil
}

// InsertBlockWithoutSetHead executes the block, runs the necessary verification
// upon it and then persist the block and the associate state into the database.
// The key difference between the InsertChain is it won't do the canonical chain
// updating. It relies on the additional SetChainHead call to finalize the entire
// procedure.
func (bc *BlockChain) InsertBlockWithoutSetHead(block *types.Block) error {
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	defer bc.chainmu.Unlock()

	_, err := bc.insertChain(types.Blocks{block}, true, false)
	return err
}

// SetChainHead rewinds the chain to set the new head block as the specified
// block. It's possible that after the reorg the relevant state of head
// is missing. It can be fixed by inserting a new block which triggers
// the re-execution.
func (bc *BlockChain) SetChainHead(newBlock *types.Block) error {
	if !bc.chainmu.TryLock() {
		return errChainStopped
	}
	defer bc.chainmu.Unlock()

	// Run the reorg if necessary and set the given block as new head.
	if newBlock.ParentHash() != bc.CurrentBlock().Hash() {
		if err := bc.reorg(bc.CurrentBlock(), newBlock); err != nil {
			return err
		}
	}
	bc.writeHeadBlock(newBlock)

	// Emit events
	logs := bc.collectLogs(newBlock.Hash(), false)
	bc.chainFeed.Send(ChainEvent{Block: newBlock, Hash: newBlock.Hash(), Logs: logs})
	if len(logs) > 0 {
		bc.logsFeed.Send(logs)
	}
	bc.chainHeadFeed.Send(ChainHeadEvent{Block: newBlock})
	log.Info("Set the chain head", "number", newBlock.Number(), "hash", newBlock.Hash())
	return nil
}

func (bc *BlockChain) updateFutureBlocks() {
	futureTimer := time.NewTicker(5 * time.Second)
	defer futureTimer.Stop()
	defer bc.wg.Done()
	for {
		select {
		case <-futureTimer.C:
			bc.procFutureBlocks()
		case <-bc.quit:
			return
		}
	}
}

// skipBlock returns 'true', if the block being imported can be skipped over, meaning
// that the block does not need to be processed but can be considered already fully 'done'.
// インポートされているブロックをスキップできる場合、skipBlockは「true」を返します。
// つまり、ブロックを処理する必要はありませんが、すでに完全に「完了」していると見なすことができます。
func (bc *BlockChain) skipBlock(err error, it *insertIterator) bool {
	// We can only ever bypass processing if the only error returned by the validator
	// is ErrKnownBlock, which means all checks passed, but we already have the block
	// and state.
	// バリデーターによって返される唯一のエラーがErrKnownBlockである場合にのみ、処理をバイパスできます。
	// これは、すべてのチェックに合格したことを意味しますが、すでにブロックと状態があります。
	if !errors.Is(err, ErrKnownBlock) {
		return false
	}
	// If we're not using snapshots, we can skip this, since we have both block
	// and (trie-) state
	// スナップショットを使用していない場合は、ブロック状態と（試行）状態の両方があるため、これをスキップできます。
	if bc.snaps == nil {
		return true
	}
	var (
		header     = it.current() // header can't be nil // ヘッダーをnilにすることはできません
		parentRoot common.Hash
	)
	// If we also have the snapshot-state, we can skip the processing.
	// スナップショット状態もある場合は、処理をスキップできます。
	if bc.snaps.Snapshot(header.Root) != nil {
		return true
	}
	// In this case, we have the trie-state but not snapshot-state. If the parent
	// snapshot-state exists, we need to process this in order to not get a gap
	// in the snapshot layers.
	// Resolve parent block
	// この場合、トライ状態はありますが、スナップショット状態はありません。
	// 親のsnapshot-stateが存在する場合、スナップショットレイヤーにギャップが生じないように、これを処理する必要があります。
	// 親ブロックを解決します
	if parent := it.previous(); parent != nil {
		parentRoot = parent.Root
	} else if parent = bc.GetHeaderByHash(header.ParentHash); parent != nil {
		parentRoot = parent.Root
	}
	if parentRoot == (common.Hash{}) {
		return false // Theoretically impossible case // 理論的に不可能なケース
	}
	// Parent is also missing snapshot: we can skip this. Otherwise process.
	// 親にもスナップショットがありません：これはスキップできます。それ以外の場合は処理します。
	if bc.snaps.Snapshot(parentRoot) == nil {
		return true
	}
	return false
}

// maintainTxIndex is responsible for the construction and deletion of the
// transaction index.
//
// User can use flag `txlookuplimit` to specify a "recentness" block, below
// which ancient tx indices get deleted. If `txlookuplimit` is 0, it means
// all tx indices will be reserved.
//
// The user can adjust the txlookuplimit value for each launch after fast
// sync, Geth will automatically construct the missing indices and delete
// the extra indices.
// manageTxIndexは、トランザクションインデックスの作成と削除を担当します。
//
//ユーザーはフラグ `txlookuplimit`を使用して、「recentness」ブロックを指定できます。
// このブロックを下回ると、古いtxインデックスが削除されます。 `txlookuplimit`が0の場合、
// すべてのtxインデックスが予約されることを意味します。
//
// ユーザーは、高速同期後の起動ごとにtxlookuplimit値を調整できます。
// ゲスは、欠落しているインデックスを自動的に作成し、余分なインデックスを削除します。
func (bc *BlockChain) maintainTxIndex(ancients uint64) {
	defer bc.wg.Done()

	// Before starting the actual maintenance, we need to handle a special case,
	// where user might init Geth with an external ancient database. If so, we
	// need to reindex all necessary transactions before starting to process any
	// pruning requests.
	// 実際のメンテナンスを開始する前に、ユーザーが外部の古いデータベースを使用して
	// Gethを初期化する可能性がある特殊なケースを処理する必要があります。
	// その場合、プルーニング要求の処理を開始する前に、
	// 必要なすべてのトランザクションのインデックスを再作成する必要があります。
	if ancients > 0 {
		var from = uint64(0)
		if bc.txLookupLimit != 0 && ancients > bc.txLookupLimit {
			from = ancients - bc.txLookupLimit
		}
		rawdb.IndexTransactions(bc.db, from, ancients, bc.quit)
	}

	// indexBlocks reindexes or unindexes transactions depending on user configuration
	// indexBlocksは、ユーザー構成に応じてトランザクションのインデックスを再作成またはインデックス解除します
	indexBlocks := func(tail *uint64, head uint64, done chan struct{}) {
		defer func() { done <- struct{}{} }()

		// If the user just upgraded Geth to a new version which supports transaction
		// index pruning, write the new tail and remove anything older.
		// ユーザーがGethをトランザクションインデックスプルーニングをサポートする新しいバージョンに
		// アップグレードしたばかりの場合は、新しいテールを記述し、古いものをすべて削除します。
		if tail == nil {
			if bc.txLookupLimit == 0 || head < bc.txLookupLimit {
				// Nothing to delete, write the tail and return
				rawdb.WriteTxIndexTail(bc.db, 0)
			} else {
				// Prune all stale tx indices and record the tx index tail
				rawdb.UnindexTransactions(bc.db, 0, head-bc.txLookupLimit+1, bc.quit)
			}
			return
		}
		// If a previous indexing existed, make sure that we fill in any missing entries
		// 以前のインデックスが存在する場合は、不足しているエントリを必ず入力してください
		if bc.txLookupLimit == 0 || head < bc.txLookupLimit {
			if *tail > 0 {
				// It can happen when chain is rewound to a historical point which
				// is even lower than the indexes tail, recap the indexing target
				// to new head to avoid reading non-existent block bodies.
				end := *tail
				if end > head+1 {
					end = head + 1
				}
				rawdb.IndexTransactions(bc.db, 0, end, bc.quit)
			}
			return
		}
		// Update the transaction index to the new chain state
		// トランザクションインデックスを新しいチェーン状態に更新します
		if head-bc.txLookupLimit+1 < *tail {
			// Reindex a part of missing indices and rewind index tail to HEAD-limit
			// 欠落しているインデックスの一部を再インデックスし、インデックスのテールをHEAD-limitに巻き戻します
			rawdb.IndexTransactions(bc.db, head-bc.txLookupLimit+1, *tail, bc.quit)
		} else {
			// Unindex a part of stale indices and forward index tail to HEAD-limit
			// 古いインデックスの一部のインデックスを解除し、インデックスのテールをHEAD-limitに転送します
			rawdb.UnindexTransactions(bc.db, *tail, head-bc.txLookupLimit+1, bc.quit)
		}
	}

	// Any reindexing done, start listening to chain events and moving the index window
	// インデックスの再作成が完了したら、チェーンイベントのリッスンを開始し、インデックスウィンドウを移動します
	var (
		done   chan struct{}                  // Non-nil if background unindexing or reindexing routine is active. // バックグラウンドのインデックス解除またはインデックスの再作成ルーチンがアクティブな場合は、nil以外。
		headCh = make(chan ChainHeadEvent, 1) // Buffered to avoid locking up the event feed //イベントフィードのロックを回避するためにバッファリングされます
	)
	sub := bc.SubscribeChainHeadEvent(headCh)
	if sub == nil {
		return
	}
	defer sub.Unsubscribe()

	for {
		select {
		case head := <-headCh:
			if done == nil {
				done = make(chan struct{})
				go indexBlocks(rawdb.ReadTxIndexTail(bc.db), head.Block.NumberU64(), done)
			}
		case <-done:
			done = nil
		case <-bc.quit:
			if done != nil {
				log.Info("Waiting background transaction indexer to exit")
				<-done
			}
			return
		}
	}
}

// reportBlock logs a bad block error.
// reportBlockは不正なブロックエラーをログに記録します。
func (bc *BlockChain) reportBlock(block *types.Block, receipts types.Receipts, err error) {
	rawdb.WriteBadBlock(bc.db, block)

	var receiptString string
	for i, receipt := range receipts {
		receiptString += fmt.Sprintf("\t %d: cumulative: %v gas: %v contract: %v status: %v tx: %v logs: %v bloom: %x state: %x\n",
			i, receipt.CumulativeGasUsed, receipt.GasUsed, receipt.ContractAddress.Hex(),
			receipt.Status, receipt.TxHash.Hex(), receipt.Logs, receipt.Bloom, receipt.PostState)
	}
	log.Error(fmt.Sprintf(`
########## BAD BLOCK #########
Chain config: %v

Number: %v
Hash: 0x%x
%v

Error: %v
##############################
`, bc.chainConfig, block.Number(), block.Hash(), receiptString, err))
}

// InsertHeaderChain attempts to insert the given header chain in to the local
// chain, possibly creating a reorg. If an error is returned, it will return the
// index number of the failing header as well an error describing what went wrong.
//
// The verify parameter can be used to fine tune whether nonce verification
// should be done or not. The reason behind the optional check is because some
// of the header retrieval mechanisms already need to verify nonces, as well as
// because nonces can be verified sparsely, not needing to check each.

// InsertHeaderChainは、指定されたヘッダーチェーンをローカルチェーンに挿入しようとし、
// 場合によっては再編成を作成します。エラーが返された場合は、失敗したヘッダーのインデックス番号と、
// 何が問題だったかを説明するエラーが返されます。
//
// verifyパラメータを使用して、nonce検証を実行するかどうかを微調整できます。
// オプションのチェックの背後にある理由は、ヘッダー取得メカニズムの一部がすでにナンスを検証する必要があるため、
// およびナンスはそれぞれをチェックする必要がなく、まばらに検証できるためです。
func (bc *BlockChain) InsertHeaderChain(chain []*types.Header, checkFreq int) (int, error) {
	start := time.Now()
	if i, err := bc.hc.ValidateHeaderChain(chain, checkFreq); err != nil {
		return i, err
	}

	if !bc.chainmu.TryLock() {
		return 0, errChainStopped
	}
	defer bc.chainmu.Unlock()
	_, err := bc.hc.InsertHeaderChain(chain, start, bc.forker)
	return 0, err
}
