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

// Package ethconfig contains the configuration of the ETH and LES protocols.
package ethconfig

import (
	"math/big"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
)

// FullNodeGPO contains default gasprice oracle settings for full node.
// FullNodeGPOには、フルノードのデフォルトのgaspriceoracle設定が含まれています。
var FullNodeGPO = gasprice.Config{
	Blocks:           20,
	Percentile:       60,
	MaxHeaderHistory: 1024,
	MaxBlockHistory:  1024,
	MaxPrice:         gasprice.DefaultMaxPrice,
	IgnorePrice:      gasprice.DefaultIgnorePrice,
}

// LightClientGPO contains default gasprice oracle settings for light client.
// LightClientGPOには、ライトクライアントのデフォルトのgaspriceoracle設定が含まれています。
var LightClientGPO = gasprice.Config{
	Blocks:           2,
	Percentile:       60,
	MaxHeaderHistory: 300,
	MaxBlockHistory:  5,
	MaxPrice:         gasprice.DefaultMaxPrice,
	IgnorePrice:      gasprice.DefaultIgnorePrice,
}

// Defaults contains default settings for use on the Ethereum main net.
//デフォルトには、イーサリアムメインネットで使用するためのデフォルト設定が含まれています。
var Defaults = Config{
	SyncMode: downloader.SnapSync,
	Ethash: ethash.Config{
		CacheDir:         "ethash",
		CachesInMem:      2,
		CachesOnDisk:     3,
		CachesLockMmap:   false,
		DatasetsInMem:    1,
		DatasetsOnDisk:   2,
		DatasetsLockMmap: false,
	},
	NetworkId:               1,
	TxLookupLimit:           2350000,
	LightPeers:              100,
	UltraLightFraction:      75,
	DatabaseCache:           512,
	TrieCleanCache:          154,
	TrieCleanCacheJournal:   "triecache",
	TrieCleanCacheRejournal: 60 * time.Minute,
	TrieDirtyCache:          256,
	TrieTimeout:             60 * time.Minute,
	SnapshotCache:           102,
	Miner: miner.Config{
		GasCeil:  8000000,
		GasPrice: big.NewInt(params.GWei),
		Recommit: 3 * time.Second,
	},
	TxPool:        core.DefaultTxPoolConfig,
	RPCGasCap:     50000000,
	RPCEVMTimeout: 5 * time.Second,
	GPO:           FullNodeGPO,
	RPCTxFeeCap:   1, // 1 ether
}

func init() {
	home := os.Getenv("HOME")
	if home == "" {
		if user, err := user.Current(); err == nil {
			home = user.HomeDir
		}
	}
	if runtime.GOOS == "darwin" {
		Defaults.Ethash.DatasetDir = filepath.Join(home, "Library", "Ethash")
	} else if runtime.GOOS == "windows" {
		localappdata := os.Getenv("LOCALAPPDATA")
		if localappdata != "" {
			Defaults.Ethash.DatasetDir = filepath.Join(localappdata, "Ethash")
		} else {
			Defaults.Ethash.DatasetDir = filepath.Join(home, "AppData", "Local", "Ethash")
		}
	} else {
		Defaults.Ethash.DatasetDir = filepath.Join(home, ".ethash")
	}
}

//go:generate gencodec -type Config -formats toml -out gen_config.go

// Config contains configuration options for of the ETH and LES protocols.
// Configには、ETHおよびLESプロトコルの構成オプションが含まれています。
type Config struct {
	// The genesis block, which is inserted if the database is empty.
	// If nil, the Ethereum main net block is used.
	// データベースが空の場合に挿入されるジェネシスブロック。
	// nilの場合、Ethereumメインネットブロックが使用されます。
	Genesis *core.Genesis `toml:",omitempty"`

	// Protocol options
	// プロトコルオプション
	NetworkId uint64 // Network ID to use for selecting peers to connect to // 接続するピアを選択するために使用するネットワークID
	SyncMode  downloader.SyncMode

	// This can be set to list of enrtree:// URLs which will be queried for
	// for nodes to connect to.
	// これは、ノードが接続するために照会されるenrtree：URLのリストに設定できます
	EthDiscoveryURLs  []string
	SnapDiscoveryURLs []string

	NoPruning  bool // プルーニングを無効にして、すべてをディスクにフラッシュするかどうか  // Whether to disable pruning and flush everything to disk
	NoPrefetch bool // プリフェッチを無効にし、オンデマンドで状態のみをロードするかどうか  // Whether to disable prefetching and only load state on demand

	TxLookupLimit uint64 `toml:",omitempty"` // txインデックスが予約されているヘッドからのブロックの最大数。// The maximum number of blocks from head whose tx indices are reserved.

	// Whitelist of required block number -> hash values to accept
	// 必要なブロック番号のホワイトリスト->受け入れるハッシュ値
	Whitelist map[uint64]common.Hash `toml:"-"`

	// Light client options
	// ライトクライアントオプション
	LightServ          int  `toml:",omitempty"` // LESリクエストの処理に許可される時間の最大パーセンテージ // Maximum percentage of time allowed for serving LES requests
	LightIngress       int  `toml:",omitempty"` // ライトサーバーの着信帯域幅制限  // Incoming bandwidth limit for light servers
	LightEgress        int  `toml:",omitempty"` // ライトサーバーの送信帯域幅制限 // Outgoing bandwidth limit for light servers
	LightPeers         int  `toml:",omitempty"` // LESクライアントピアの最大数   // Maximum number of LES client peers
	LightNoPrune       bool `toml:",omitempty"` // 軽鎖剪定を無効にするかどうか  // Whether to disable light chain pruning
	LightNoSyncServe   bool `toml:",omitempty"` // 同期する前にライトクライアントにサービスを提供するかどうか // Whether to serve light clients before syncing
	SyncFromCheckpoint bool `toml:",omitempty"` // 構成されたチェックポイントからヘッダーチェーンを同期するかどうか // Whether to sync the header chain from the configured checkpoint

	// Ultra Light client options
	// UltraLightクライアントオプション
	UltraLightServers      []string `toml:",omitempty"` // 信頼できる超軽量サーバーのリスト // List of trusted ultra light servers
	UltraLightFraction     int      `toml:",omitempty"` // アナウンスを受け入れる信頼できるサーバーの割合 // Percentage of trusted servers to accept an announcement
	UltraLightOnlyAnnounce bool     `toml:",omitempty"` // ヘッダーのみをアナウンスするか、ヘッダーを提供するか // Whether to only announce headers, or also serve them

	// Database options
	// データベースオプション
	SkipBcVersionCheck bool `toml:"-"`
	DatabaseHandles    int  `toml:"-"`
	DatabaseCache      int
	DatabaseFreezer    string

	TrieCleanCache          int
	TrieCleanCacheJournal   string        `toml:",omitempty"` //ノードの再起動後も存続するトライキャッシュ用のディスクジャーナルディレクトリ // Disk journal directory for trie cache to survive node restarts
	TrieCleanCacheRejournal time.Duration `toml:",omitempty"` //クリーンキャッシュのためにジャーナルを再生成する時間間隔 // Time interval to regenerate the journal for clean cache
	TrieDirtyCache          int
	TrieTimeout             time.Duration
	SnapshotCache           int
	Preimages               bool

	// Mining options
	// マイニングオプション
	Miner miner.Config

	// Ethash options
	// Ethashオプション
	Ethash ethash.Config

	// Transaction pool options
	// トランザクションプールオプション
	TxPool core.TxPoolConfig

	// Gas Price Oracle options
	// ガス価格Oracleオプション
	GPO gasprice.Config

	// Enables tracking of SHA3 preimages in the VM
	// VM内のSHA3プレイメージの追跡を有効にします
	EnablePreimageRecording bool

	// Miscellaneous options
	// その他のオプション
	DocRoot string `toml:"-"`

	// RPCGasCap is the global gas cap for eth-call variants.
	// RPCGasCapは、eth-callバリアントのグローバルガスキャップです
	RPCGasCap uint64

	// RPCEVMTimeout is the global timeout for eth-call.
	// RPCEVMTimeoutは、eth-callのグローバルタイムアウトです。
	RPCEVMTimeout time.Duration

	// RPCTxFeeCap is the global transaction fee(price * gaslimit) cap for
	// send-transction variants. The unit is ether.
	// RPCTxFeeCapは、送信トランザクションバリアントのグローバルトランザクション料金（価格*ガス制限）の上限です。
	// 単位はエーテルです。
	RPCTxFeeCap float64

	// Checkpoint is a hardcoded checkpoint which can be nil.
	// チェックポイントはハードコードされたチェックポイントであり、nilにすることができます。
	Checkpoint *params.TrustedCheckpoint `toml:",omitempty"`

	// CheckpointOracle is the configuration for checkpoint oracle.
	// CheckpointOracleは、チェックポイントoracleの構成です。
	CheckpointOracle *params.CheckpointOracleConfig `toml:",omitempty"`

	// Arrow Glacier block override (TODO: remove after the fork)
	// Arrow Glacierブロックオーバーライド（TODO：フォークの後で削除）
	OverrideArrowGlacier *big.Int `toml:",omitempty"`

	// OverrideTerminalTotalDifficulty (TODO: remove after the fork)
	OverrideTerminalTotalDifficulty *big.Int `toml:",omitempty"`
}

// CreateConsensusEngine creates a consensus engine for the given chain configuration.
// CreateConsensusEngineは、指定されたチェーン構成のコンセンサスエンジンを作成します。
func CreateConsensusEngine(stack *node.Node, chainConfig *params.ChainConfig, config *ethash.Config, notify []string, noverify bool, db ethdb.Database) consensus.Engine {
	// If proof-of-authority is requested, set it up
	var engine consensus.Engine
	if chainConfig.Clique != nil {
		engine = clique.New(chainConfig.Clique, db)
	} else {
		switch config.PowMode {
		case ethash.ModeFake:
			log.Warn("Ethash used in fake mode")
		case ethash.ModeTest:
			log.Warn("Ethash used in test mode")
		case ethash.ModeShared:
			log.Warn("Ethash used in shared mode")
		}
		engine = ethash.New(ethash.Config{
			PowMode:          config.PowMode,
			CacheDir:         stack.ResolvePath(config.CacheDir),
			CachesInMem:      config.CachesInMem,
			CachesOnDisk:     config.CachesOnDisk,
			CachesLockMmap:   config.CachesLockMmap,
			DatasetDir:       config.DatasetDir,
			DatasetsInMem:    config.DatasetsInMem,
			DatasetsOnDisk:   config.DatasetsOnDisk,
			DatasetsLockMmap: config.DatasetsLockMmap,
			NotifyFull:       config.NotifyFull,
		}, notify, noverify)
		engine.(*ethash.Ethash).SetThreads(-1) // Disable CPU mining
	}
	return beacon.New(engine)
}
