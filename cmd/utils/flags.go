// Copyright 2015 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

// Package utils contains internal helper functions for go-ethereum commands.
package utils

import (
	"crypto/ecdsa"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/big"
	"os"
	"path/filepath"
	godebug "runtime/debug"
	"strconv"
	"strings"
	"text/tabwriter"
	"text/template"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/fdlimit"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/clique"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/catalyst"
	"github.com/ethereum/go-ethereum/eth/downloader"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/eth/gasprice"
	"github.com/ethereum/go-ethereum/eth/tracers"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethstats"
	"github.com/ethereum/go-ethereum/graphql"
	"github.com/ethereum/go-ethereum/internal/ethapi"
	"github.com/ethereum/go-ethereum/internal/flags"
	"github.com/ethereum/go-ethereum/les"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/metrics/exp"
	"github.com/ethereum/go-ethereum/metrics/influxdb"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/nat"
	"github.com/ethereum/go-ethereum/p2p/netutil"
	"github.com/ethereum/go-ethereum/params"
	pcsclite "github.com/gballet/go-libpcsclite"
	gopsutil "github.com/shirou/gopsutil/mem"
	"gopkg.in/urfave/cli.v1"
)

func init() {
	cli.AppHelpTemplate = `{{.Name}} {{if .Flags}}[global options] {{end}}command{{if .Flags}} [command options]{{end}} [arguments...]

VERSION:
   {{.Version}}

COMMANDS:
   {{range .Commands}}{{.Name}}{{with .ShortName}}, {{.}}{{end}}{{ "\t" }}{{.Usage}}
   {{end}}{{if .Flags}}
GLOBAL OPTIONS:
   {{range .Flags}}{{.}}
   {{end}}{{end}}
`
	cli.CommandHelpTemplate = flags.CommandHelpTemplate
	cli.HelpPrinter = printHelp
}

func printHelp(out io.Writer, templ string, data interface{}) {
	funcMap := template.FuncMap{"join": strings.Join}
	t := template.Must(template.New("help").Funcs(funcMap).Parse(templ))
	w := tabwriter.NewWriter(out, 38, 8, 2, ' ', 0)
	err := t.Execute(w, data)
	if err != nil {
		panic(err)
	}
	w.Flush()
}

// These are all the command line flags we support.
// If you add to this list, please remember to include the
// flag in the appropriate command definition.
// これらはすべてサポートされているコマンドラインフラグです。
// このリストに追加する場合は、適切なコマンド定義にフラグを含めることを忘れないでください。
//
// The flags are defined here so their names and help texts
// are the same for all commands.
// フラグはここで定義されているため、フラグの名前とヘルプテキストはすべてのコマンドで同じです。

var (
	// General settings
	DataDirFlag = DirectoryFlag{
		Name:  "datadir",
		Usage: "Data directory for the databases and keystore",
		// Usage: データベースとキーストアのデータディレクトリ
		Value: DirectoryString(node.DefaultDataDir()),
	}
	AncientFlag = DirectoryFlag{
		Name:  "datadir.ancient",
		Usage: "Data directory for ancient chain segments (default = inside chaindata)",
		// Usage: 古代のチェーンセグメントのデータディレクトリ（デフォルト= chaindata内）
	}
	MinFreeDiskSpaceFlag = DirectoryFlag{
		Name:  "datadir.minfreedisk",
		Usage: "Minimum free disk space in MB, once reached triggers auto shut down (default = --cache.gc converted to MB, 0 = disabled)",
		// Usage: 最小空きディスク容量（MB単位）に達すると、自動シャットダウンがトリガーされます（デフォルト= --cache.gcがMBに変換され、0 =無効）
	}
	KeyStoreDirFlag = DirectoryFlag{
		Name:  "keystore",
		Usage: "Directory for the keystore (default = inside the datadir)",
		// Usage: キーストアのディレクトリ（デフォルト= datadir内）
	}
	USBFlag = cli.BoolFlag{
		Name:  "usb",
		Usage: "Enable monitoring and management of USB hardware wallets",
		// Usage: USBハードウェアウォレットの監視と管理を可能にします
	}
	SmartCardDaemonPathFlag = cli.StringFlag{
		Name:  "pcscdpath",
		Usage: "Path to the smartcard daemon (pcscd) socket file",
		// Usage: スマートカードデーモン（pcscd）ソケットファイルへのパス
		Value: pcsclite.PCSCDSockName,
	}
	NetworkIdFlag = cli.Uint64Flag{
		Name:  "networkid",
		Usage: "Explicitly set network id (integer)(For testnets: use --ropsten, --rinkeby, --goerli instead)",
		// Usage: ネットワークID（整数）を明示的に設定します（テストネットの場合：代わりに--ropsten、-rinkeby、-goerliを使用します）
		Value: ethconfig.Defaults.NetworkId,
	}
	MainnetFlag = cli.BoolFlag{
		Name:  "mainnet",
		Usage: "Ethereum mainnet",
	}
	GoerliFlag = cli.BoolFlag{
		Name:  "goerli",
		Usage: "Görli network: pre-configured proof-of-authority test network",
		// Usage: Görliネットワーク：事前構成された認証証明テストネットワーク
	}
	RinkebyFlag = cli.BoolFlag{
		Name:  "rinkeby",
		Usage: "Rinkeby network: pre-configured proof-of-authority test network",
		// Usage: Rinkebyネットワーク：事前設定された認証証明テストネットワーク
	}
	RopstenFlag = cli.BoolFlag{
		Name:  "ropsten",
		Usage: "Ropsten network: pre-configured proof-of-work test network",
		// Usage: Ropstenネットワーク：事前構成されたプルーフオブワークテストネットワーク
	}
	SepoliaFlag = cli.BoolFlag{
		Name:  "sepolia",
		Usage: "Sepolia network: pre-configured proof-of-work test network",
		// Usage: セポリアネットワーク：事前構成されたプルーフオブワークテストネットワーク
	}
	DeveloperFlag = cli.BoolFlag{
		Name:  "dev",
		Usage: "Ephemeral proof-of-authority network with a pre-funded developer account, mining enabled",
		// Usage: 事前に資金提供された開発者アカウントを備えた一時的な権限証明ネットワーク、マイニングが有効
	}
	DeveloperPeriodFlag = cli.IntFlag{
		Name:  "dev.period",
		Usage: "Block period to use in developer mode (0 = mine only if transaction pending)",
		// Usage: 開発者モードで使用するブロック期間（0 =トランザクションが保留中の場合にのみマイニング）
	}
	DeveloperGasLimitFlag = cli.Uint64Flag{
		Name:  "dev.gaslimit",
		Usage: "Initial block gas limit",
		// Usage: 初期ブロックガス制限
		Value: 11500000,
	}
	IdentityFlag = cli.StringFlag{
		Name:  "identity",
		Usage: "Custom node name",
		// Usage: カスタムノード名
	}
	DocRootFlag = DirectoryFlag{
		Name:  "docroot",
		Usage: "Document Root for HTTPClient file scheme",
		// Usage: HTTPClientファイルスキームのドキュメントルート
		Value: DirectoryString(HomeDir()),
	}
	ExitWhenSyncedFlag = cli.BoolFlag{
		Name:  "exitwhensynced",
		Usage: "Exits after block synchronisation completes",
		// Usage: ブロック同期が完了した後に終了します
	}
	IterativeOutputFlag = cli.BoolTFlag{
		Name:  "iterative",
		Usage: "Print streaming JSON iteratively, delimited by newlines",
		// Usage: 改行で区切られたストリーミングJSONを繰り返し印刷します
	}
	ExcludeStorageFlag = cli.BoolFlag{
		Name:  "nostorage",
		Usage: "Exclude storage entries (save db lookups)",
		// Usage: ストレージエントリを除外します（dbルックアップを保存します）
	}
	IncludeIncompletesFlag = cli.BoolFlag{
		Name:  "incompletes",
		Usage: "Include accounts for which we don't have the address (missing preimage)",
		// Usage: 住所がわからないアカウントを含める（プリイメージがない）
	}
	ExcludeCodeFlag = cli.BoolFlag{
		Name:  "nocode",
		Usage: "Exclude contract code (save db lookups)",
		// Usage: コントラクトコードを除外します（dbルックアップを保存します）
	}
	StartKeyFlag = cli.StringFlag{
		Name:  "start",
		Usage: "Start position. Either a hash or address",
		// Usage: 開始位置。ハッシュまたはアドレスのいずれか
		Value: "0x0000000000000000000000000000000000000000000000000000000000000000",
	}
	DumpLimitFlag = cli.Uint64Flag{
		Name:  "limit",
		Usage: "Max number of elements (0 = no limit)",
		// Usage: 要素の最大数（0 =制限なし）
		Value: 0,
	}
	defaultSyncMode = ethconfig.Defaults.SyncMode
	SyncModeFlag    = TextMarshalerFlag{
		Name:  "syncmode",
		Usage: `Blockchain sync mode ("snap", "full" or "light")`,
        // Usage： `ブロックチェーン同期モード（"スナップ "、"フル "または"ライト "）
		Value: &defaultSyncMode,
	}
	GCModeFlag = cli.StringFlag{
		Name:  "gcmode",
		Usage: `Blockchain garbage collection mode ("full", "archive")`,
		// Usage: ブロックチェーンガベージコレクションモード（「フル」、「アーカイブ」）
		Value: "full",
	}
	SnapshotFlag = cli.BoolTFlag{
		Name:  "snapshot",
		Usage: `Enables snapshot-database mode (default = enable)`,
		// Usage: スナップショットデータベースモードを有効にします（デフォルト=有効）
	}
	TxLookupLimitFlag = cli.Uint64Flag{
		Name:  "txlookuplimit",
		Usage: "Number of recent blocks to maintain transactions index for (default = about one year, 0 = entire chain)",
		// Usage: トランザクションインデックスを維持するための最近のブロックの数（デフォルト=約1年、0 =チェーン全体）
		Value: ethconfig.Defaults.TxLookupLimit,
	}
	LightKDFFlag = cli.BoolFlag{
		Name:  "lightkdf",
		Usage: "Reduce key-derivation RAM & CPU usage at some expense of KDF strength",
		// Usage: KDFの強度をいくらか犠牲にして、鍵導出RAMとCPU使用率を削減します
	}
	WhitelistFlag = cli.StringFlag{
		Name:  "whitelist",
		Usage: "Comma separated block number-to-hash mappings to enforce (<number>=<hash>)",
		// Usage: 強制するコンマ区切りのブロック番号からハッシュへのマッ​​ピング（<number> = <hash>）
	}
	BloomFilterSizeFlag = cli.Uint64Flag{
		Name:  "bloomfilter.size",
		Usage: "Megabytes of memory allocated to bloom-filter for pruning",
		// Usage: 剪定のためにブルームフィルターに割り当てられたメガバイトのメモリ
		Value: 2048,
	}
	OverrideArrowGlacierFlag = cli.Uint64Flag{
		Name:  "override.arrowglacier",
		Usage: "Manually specify Arrow Glacier fork-block, overriding the bundled setting",
		// Usage: バンドルされた設定を上書きして、ArrowGlacierフォークブロックを手動で指定します
	}
	OverrideTerminalTotalDifficulty = cli.Uint64Flag{
		Name:  "override.terminaltotaldifficulty",
		Usage: "Manually specify TerminalTotalDifficulty, overriding the bundled setting",
        // Usage：「バンドルされた設定を上書きして、TerminalTotalDifficultyを手動で指定します
	}
	// Light server and client settings
	LightServeFlag = cli.IntFlag{
		Name:  "light.serve",
		Usage: "Maximum percentage of time allowed for serving LES requests (multi-threaded processing allows values over 100)",
		// Usage: LES要求の処理に許可される時間の最大パーセンテージ（マルチスレッド処理では100を超える値が許可されます）
		Value: ethconfig.Defaults.LightServ,
	}
	LightIngressFlag = cli.IntFlag{
		Name:  "light.ingress",
		Usage: "Incoming bandwidth limit for serving light clients (kilobytes/sec, 0 = unlimited)",
		// Usage: ライトクライアントにサービスを提供するための着信帯域幅制限（キロバイト/秒、0 =無制限）
		Value: ethconfig.Defaults.LightIngress,
	}
	LightEgressFlag = cli.IntFlag{
		Name:  "light.egress",
		Usage: "Outgoing bandwidth limit for serving light clients (kilobytes/sec, 0 = unlimited)",
		// Usage: ライトクライアントにサービスを提供するための発信帯域幅制限（キロバイト/秒、0 =無制限）
		Value: ethconfig.Defaults.LightEgress,
	}
	LightMaxPeersFlag = cli.IntFlag{
		Name:  "light.maxpeers",
		Usage: "Maximum number of light clients to serve, or light servers to attach to",
		// Usage: サービスを提供するライトクライアント、または接続するライトサーバーの最大数
		Value: ethconfig.Defaults.LightPeers,
	}
	UltraLightServersFlag = cli.StringFlag{
		Name:  "ulc.servers",
		Usage: "List of trusted ultra-light servers",
		// Usage: 信頼できる超軽量サーバーのリスト
		Value: strings.Join(ethconfig.Defaults.UltraLightServers, ","),
	}
	UltraLightFractionFlag = cli.IntFlag{
		Name:  "ulc.fraction",
		Usage: "Minimum % of trusted ultra-light servers required to announce a new head",
		// Usage: 新しいヘッドを発表するために必要な信頼できる超軽量サーバーの最小％
		Value: ethconfig.Defaults.UltraLightFraction,
	}
	UltraLightOnlyAnnounceFlag = cli.BoolFlag{
		Name:  "ulc.onlyannounce",
		Usage: "Ultra light server sends announcements only",
		// Usage: 超軽量サーバーはアナウンスのみを送信します
	}
	LightNoPruneFlag = cli.BoolFlag{
		Name:  "light.nopruning",
		Usage: "Disable ancient light chain data pruning",
		// Usage: 古代の軽鎖データの剪定を無効にする
	}
	LightNoSyncServeFlag = cli.BoolFlag{
		Name:  "light.nosyncserve",
		Usage: "Enables serving light clients before syncing",
		// Usage: 同期する前にライトクライアントにサービスを提供できるようにします
	}
	// Ethash settings
	// エサッシュ設定
	EthashCacheDirFlag = DirectoryFlag{
		Name:  "ethash.cachedir",
		Usage: "Directory to store the ethash verification caches (default = inside the datadir)",
		// Usage: ethash検証キャッシュを格納するディレクトリ（デフォルト= datadir内）
	}
	EthashCachesInMemoryFlag = cli.IntFlag{
		Name:  "ethash.cachesinmem",
		Usage: "Number of recent ethash caches to keep in memory (16MB each)",
		// Usage: メモリに保持する最近のethashキャッシュの数（各16MB）
		Value: ethconfig.Defaults.Ethash.CachesInMem,
	}
	EthashCachesOnDiskFlag = cli.IntFlag{
		Name:  "ethash.cachesondisk",
		Usage: "Number of recent ethash caches to keep on disk (16MB each)",
		// Usage: ディスクに保持する最近のethashキャッシュの数（各16MB）
		Value: ethconfig.Defaults.Ethash.CachesOnDisk,
	}
	EthashCachesLockMmapFlag = cli.BoolFlag{
		Name:  "ethash.cacheslockmmap",
		Usage: "Lock memory maps of recent ethash caches",
		// Usage: 最近のethashキャッシュのメモリマップをロックする
	}
	EthashDatasetDirFlag = DirectoryFlag{
		Name:  "ethash.dagdir",
		Usage: "Directory to store the ethash mining DAGs",
		// Usage: ethashマイニングDAGを格納するディレクトリ
		Value: DirectoryString(ethconfig.Defaults.Ethash.DatasetDir),
	}
	EthashDatasetsInMemoryFlag = cli.IntFlag{
		Name:  "ethash.dagsinmem",
		Usage: "Number of recent ethash mining DAGs to keep in memory (1+GB each)",
		// Usage: メモリに保持する最近のethashマイニングDAGの数（各1 GB以上）
		Value: ethconfig.Defaults.Ethash.DatasetsInMem,
	}
	EthashDatasetsOnDiskFlag = cli.IntFlag{
		Name:  "ethash.dagsondisk",
		Usage: "Number of recent ethash mining DAGs to keep on disk (1+GB each)",
		// Usage: ディスクに保持する最近のethashマイニングDAGの数（各1 GB以上）
		Value: ethconfig.Defaults.Ethash.DatasetsOnDisk,
	}
	EthashDatasetsLockMmapFlag = cli.BoolFlag{
		Name:  "ethash.dagslockmmap",
		Usage: "Lock memory maps for recent ethash mining DAGs",
		// Usage: 最近のethashマイニングDAGのメモリマップをロックする
	}
	// Transaction pool settings
	TxPoolLocalsFlag = cli.StringFlag{
		Name:  "txpool.locals",
		Usage: "Comma separated accounts to treat as locals (no flush, priority inclusion)",
		// Usage: ローカルとして扱うためのカンマ区切りのアカウント（フラッシュなし、優先度の高い包含）
	}
	TxPoolNoLocalsFlag = cli.BoolFlag{
		Name:  "txpool.nolocals",
		Usage: "Disables price exemptions for locally submitted transactions",
		// Usage: ローカルで送信されたトランザクションの価格免除を無効にします
	}
	TxPoolJournalFlag = cli.StringFlag{
		Name:  "txpool.journal",
		Usage: "Disk journal for local transaction to survive node restarts",
		// Usage: ノードの再起動後も存続するローカルトランザクションのディスクジャーナル
		Value: core.DefaultTxPoolConfig.Journal,
	}
	TxPoolRejournalFlag = cli.DurationFlag{
		Name:  "txpool.rejournal",
		Usage: "Time interval to regenerate the local transaction journal",
		// Usage: ローカルトランザクションジャーナルを再生成する時間間隔
		Value: core.DefaultTxPoolConfig.Rejournal,
	}
	TxPoolPriceLimitFlag = cli.Uint64Flag{
		Name:  "txpool.pricelimit",
		Usage: "Minimum gas price limit to enforce for acceptance into the pool",
		// Usage: プールへの受け入れを強制するための最小ガス価格制限
		Value: ethconfig.Defaults.TxPool.PriceLimit,
	}
	TxPoolPriceBumpFlag = cli.Uint64Flag{
		Name:  "txpool.pricebump",
		Usage: "Price bump percentage to replace an already existing transaction",
		// Usage: 既存のトランザクションを置き換えるための価格上昇率
		Value: ethconfig.Defaults.TxPool.PriceBump,
	}
	TxPoolAccountSlotsFlag = cli.Uint64Flag{
		Name:  "txpool.accountslots",
		Usage: "Minimum number of executable transaction slots guaranteed per account",
		// Usage: アカウントごとに保証される実行可能トランザクションスロットの最小数
		Value: ethconfig.Defaults.TxPool.AccountSlots,
	}
	TxPoolGlobalSlotsFlag = cli.Uint64Flag{
		Name:  "txpool.globalslots",
		Usage: "Maximum number of executable transaction slots for all accounts",
		// Usage: すべてのアカウントの実行可能なトランザクションスロットの最大数
		Value: ethconfig.Defaults.TxPool.GlobalSlots,
	}
	TxPoolAccountQueueFlag = cli.Uint64Flag{
		Name:  "txpool.accountqueue",
		Usage: "Maximum number of non-executable transaction slots permitted per account",
		// Usage: アカウントごとに許可される実行不可能なトランザクションスロットの最大数
		Value: ethconfig.Defaults.TxPool.AccountQueue,
	}
	TxPoolGlobalQueueFlag = cli.Uint64Flag{
		Name:  "txpool.globalqueue",
		Usage: "Maximum number of non-executable transaction slots for all accounts",
		// Usage: すべてのアカウントの実行不可能なトランザクションスロットの最大数
		Value: ethconfig.Defaults.TxPool.GlobalQueue,
	}
	TxPoolLifetimeFlag = cli.DurationFlag{
		Name:  "txpool.lifetime",
		Usage: "Maximum amount of time non-executable transaction are queued",
		// Usage: 実行不可能なトランザクションがキューに入れられる最大時間
		Value: ethconfig.Defaults.TxPool.Lifetime,
	}
	// Performance tuning settings
	// パフォーマンスチューニング設定
	CacheFlag = cli.IntFlag{
		Name:  "cache",
		Usage: "Megabytes of memory allocated to internal caching (default = 4096 mainnet full node, 128 light mode)",
		// Usage: 内部キャッシュに割り当てられたメガバイトのメモリ（デフォルト= 4096メインネットフルノード、128ライトモード）
		Value: 1024,
	}
	CacheDatabaseFlag = cli.IntFlag{
		Name:  "cache.database",
		Usage: "Percentage of cache memory allowance to use for database io",
		// Usage: データベースIOに使用するキャッシュメモリ許容量の割合
		Value: 50,
	}
	CacheTrieFlag = cli.IntFlag{
		Name:  "cache.trie",
		Usage: "Percentage of cache memory allowance to use for trie caching (default = 15% full mode, 30% archive mode)",
		// Usage: トライキャッシングに使用するキャッシュメモリ許容量のパーセンテージ（デフォルト= 15％フルモード、30％アーカイブモード）
		Value: 15,
	}
	CacheTrieJournalFlag = cli.StringFlag{
		Name:  "cache.trie.journal",
		Usage: "Disk journal directory for trie cache to survive node restarts",
		// Usage: ノードの再起動後も存続するトライキャッシュ用のディスクジャーナルディレクトリ
		Value: ethconfig.Defaults.TrieCleanCacheJournal,
	}
	CacheTrieRejournalFlag = cli.DurationFlag{
		Name:  "cache.trie.rejournal",
		Usage: "Time interval to regenerate the trie cache journal",
		// Usage: トライキャッシュジャーナルを再生成する時間間隔
		Value: ethconfig.Defaults.TrieCleanCacheRejournal,
	}
	CacheGCFlag = cli.IntFlag{
		Name:  "cache.gc",
		Usage: "Percentage of cache memory allowance to use for trie pruning (default = 25% full mode, 0% archive mode)",
		// Usage: トライプルーニングに使用するキャッシュメモリ許容量のパーセンテージ（デフォルト= 25％フルモード、0％アーカイブモード）
		Value: 25,
	}
	CacheSnapshotFlag = cli.IntFlag{
		Name:  "cache.snapshot",
		Usage: "Percentage of cache memory allowance to use for snapshot caching (default = 10% full mode, 20% archive mode)",
		// Usage: スナップショットキャッシュに使用するキャッシュメモリ許容量の割合（デフォルト= 10％フルモード、20％アーカイブモード）
		Value: 10,
	}
	CacheNoPrefetchFlag = cli.BoolFlag{
		Name:  "cache.noprefetch",
		Usage: "Disable heuristic state prefetch during block import (less CPU and disk IO, more time waiting for data)",
		// Usage: ブロックインポート中のヒューリスティック状態のプリフェッチを無効にします（CPUとディスクIOが少なくなり、データの待機時間が長くなります）
	}
	CachePreimagesFlag = cli.BoolFlag{
		Name:  "cache.preimages",
		Usage: "Enable recording the SHA3/keccak preimages of trie keys",
		// Usage: トライキーのSHA3 / keccakプレイメージの記録を有効にする
	}
	// Miner settings
	MiningEnabledFlag = cli.BoolFlag{
		Name:  "mine",
		Usage: "Enable mining",
		// Usage: マイニングを有効にする
	}
	MinerThreadsFlag = cli.IntFlag{
		Name:  "miner.threads",
		Usage: "Number of CPU threads to use for mining",
		// Usage: マイニングに使用するCPUスレッドの数
		Value: 0,
	}
	MinerNotifyFlag = cli.StringFlag{
		Name:  "miner.notify",
		Usage: "Comma separated HTTP URL list to notify of new work packages",
		// Usage: 新しい作業パッケージを通知するためのコンマ区切りのHTTPURLリスト
	}
	MinerNotifyFullFlag = cli.BoolFlag{
		Name:  "miner.notify.full",
		Usage: "Notify with pending block headers instead of work packages",
		// Usage: 作業パッケージの代わりに保留中のブロックヘッダーで通知する
	}
	MinerGasLimitFlag = cli.Uint64Flag{
		Name:  "miner.gaslimit",
		Usage: "Target gas ceiling for mined blocks",
		// Usage: 採掘されたブロックの目標ガス上限
		Value: ethconfig.Defaults.Miner.GasCeil,
	}
	MinerGasPriceFlag = BigFlag{
		Name:  "miner.gasprice",
		Usage: "Minimum gas price for mining a transaction",
		// Usage: 取引を採掘するための最低ガス価格
		Value: ethconfig.Defaults.Miner.GasPrice,
	}
	MinerEtherbaseFlag = cli.StringFlag{
		Name:  "miner.etherbase",
		Usage: "Public address for block mining rewards (default = first account)",
		// Usage: ブロックマイニング報酬のパブリックアドレス（デフォルト=最初のアカウント）
		Value: "0",
	}
	MinerExtraDataFlag = cli.StringFlag{
		Name:  "miner.extradata",
		Usage: "Block extra data set by the miner (default = client version)",
		// Usage: マイナーによって追加のデータセットをブロックします（デフォルト=クライアントバージョン）
	}
	MinerRecommitIntervalFlag = cli.DurationFlag{
		Name:  "miner.recommit",
		Usage: "Time interval to recreate the block being mined",
		// Usage: マイニングされているブロックを再作成する時間間隔
		Value: ethconfig.Defaults.Miner.Recommit,
	}
	MinerNoVerifyFlag = cli.BoolFlag{
		Name:  "miner.noverify",
		Usage: "Disable remote sealing verification",
		// Usage: リモートシーリング検証を無効にする
	}
	// Account settings
	UnlockedAccountFlag = cli.StringFlag{
		Name:  "unlock",
		Usage: "Comma separated list of accounts to unlock",
		// Usage: ロックを解除するアカウントのコンマ区切りリスト
		Value: "",
	}
	PasswordFileFlag = cli.StringFlag{
		Name:  "password",
		Usage: "Password file to use for non-interactive password input",
		// Usage: 非対話型パスワード入力に使用するパスワードファイル
		Value: "",
	}
	ExternalSignerFlag = cli.StringFlag{
		Name:  "signer",
		Usage: "External signer (url or path to ipc file)",
		// Usage: 外部署名者（URLまたはIPCファイルへのパス）
		Value: "",
	}
	VMEnableDebugFlag = cli.BoolFlag{
		Name:  "vmdebug",
		Usage: "Record information useful for VM and contract debugging",
		// Usage: VMおよび契約のデバッグに役立つ情報を記録する
	}
	InsecureUnlockAllowedFlag = cli.BoolFlag{
		Name:  "allow-insecure-unlock",
		Usage: "Allow insecure account unlocking when account-related RPCs are exposed by http",
		// Usage: アカウント関連のRPCがhttpによって公開されている場合に、安全でないアカウントのロック解除を許可する
	}
	RPCGlobalGasCapFlag = cli.Uint64Flag{
		Name:  "rpc.gascap",
		Usage: "Sets a cap on gas that can be used in eth_call/estimateGas (0=infinite)",
		// Usage: eth_call / estimateGasで使用できるガスに上限を設定します（0 =無限）
		Value: ethconfig.Defaults.RPCGasCap,
	}
	RPCGlobalEVMTimeoutFlag = cli.DurationFlag{
		Name:  "rpc.evmtimeout",
		Usage: "Sets a timeout used for eth_call (0=infinite)",
		// Usage: eth_callに使用されるタイムアウトを設定します（0 =無限）
		Value: ethconfig.Defaults.RPCEVMTimeout,
	}
	RPCGlobalTxFeeCapFlag = cli.Float64Flag{
		Name:  "rpc.txfeecap",
		Usage: "Sets a cap on transaction fee (in ether) that can be sent via the RPC APIs (0 = no cap)",
		// Usage: RPC APIを介して送信できるトランザクション料金（エーテル）に上限を設定します（0 =上限なし）
		Value: ethconfig.Defaults.RPCTxFeeCap,
	}
	// Logging and debug settings
	EthStatsURLFlag = cli.StringFlag{
		Name:  "ethstats",
		Usage: "Reporting URL of a ethstats service (nodename:secret@host:port)",
		// Usage: ethstatsサービスのレポートURL（nodename：secret @ host：port）
	}
	FakePoWFlag = cli.BoolFlag{
		Name:  "fakepow",
		Usage: "Disables proof-of-work verification",
		// Usage: プルーフオブワーク検証を無効にします
	}
	NoCompactionFlag = cli.BoolFlag{
		Name:  "nocompaction",
		Usage: "Disables db compaction after import",
		// Usage: インポート後のdb圧縮を無効にします
	}
	// RPC settings
	IPCDisabledFlag = cli.BoolFlag{
		Name:  "ipcdisable",
		Usage: "Disable the IPC-RPC server",
		// Usage: IPC-RPCサーバーを無効にする
	}
	IPCPathFlag = DirectoryFlag{
		Name:  "ipcpath",
		Usage: "Filename for IPC socket/pipe within the datadir (explicit paths escape it)",
		// Usage: datadir内のIPCソケット/パイプのファイル名（明示的なパスはそれをエスケープします）
	}
	HTTPEnabledFlag = cli.BoolFlag{
		Name:  "http",
		Usage: "Enable the HTTP-RPC server",
		// Usage: HTTP-RPCサーバーを有効にする
	}
	HTTPListenAddrFlag = cli.StringFlag{
		Name:  "http.addr",
		Usage: "HTTP-RPC server listening interface",
		// Usage: HTTP-RPCサーバーリスニングインターフェイス
		Value: node.DefaultHTTPHost,
	}
	HTTPPortFlag = cli.IntFlag{
		Name:  "http.port",
		Usage: "HTTP-RPC server listening port",
		// Usage: HTTP-RPCサーバーのリスニングポート
		Value: node.DefaultHTTPPort,
	}
	HTTPCORSDomainFlag = cli.StringFlag{
		Name:  "http.corsdomain",
		Usage: "Comma separated list of domains from which to accept cross origin requests (browser enforced)",
		// Usage: クロスオリジンリクエストを受け入れるドメインのコンマ区切りリスト（ブラウザー強制）
		Value: "",
	}
	HTTPVirtualHostsFlag = cli.StringFlag{
		Name:  "http.vhosts",
		Usage: "Comma separated list of virtual hostnames from which to accept requests (server enforced). Accepts '*' wildcard.",
		// Usage: リクエストを受け入れる仮想ホスト名のコンマ区切りリスト（サーバー強制）。 '*'ワイルドカードを受け入れます。
		Value: strings.Join(node.DefaultConfig.HTTPVirtualHosts, ","),
	}
	HTTPApiFlag = cli.StringFlag{
		Name:  "http.api",
		Usage: "API's offered over the HTTP-RPC interface",
		// Usage: HTTP-RPCインターフェースを介して提供されるAPI
		Value: "",
	}
	HTTPPathPrefixFlag = cli.StringFlag{
		Name:  "http.rpcprefix",
		Usage: "HTTP path path prefix on which JSON-RPC is served. Use '/' to serve on all paths.",
		// Usage: JSON-RPCが提供されるHTTPパスパスプレフィックス。 '/'を使用して、すべてのパスでサービスを提供します。
		Value: "",
	}
	GraphQLEnabledFlag = cli.BoolFlag{
		Name:  "graphql",
		Usage: "Enable GraphQL on the HTTP-RPC server. Note that GraphQL can only be started if an HTTP server is started as well.",
		// Usage: HTTP-RPCサーバーでGraphQLを有効にします。 GraphQLは、HTTPサーバーも起動している場合にのみ起動できることに注意してください。
	}
	GraphQLCORSDomainFlag = cli.StringFlag{
		Name:  "graphql.corsdomain",
		Usage: "Comma separated list of domains from which to accept cross origin requests (browser enforced)",
		// Usage: クロスオリジンリクエストを受け入れるドメインのコンマ区切りリスト（ブラウザー強制）
		Value: "",
	}
	GraphQLVirtualHostsFlag = cli.StringFlag{
		Name:  "graphql.vhosts",
		Usage: "Comma separated list of virtual hostnames from which to accept requests (server enforced). Accepts '*' wildcard.",
		// Usage: リクエストを受け入れる仮想ホスト名のコンマ区切りリスト（サーバー強制）。 '*'ワイルドカードを受け入れます。
		Value: strings.Join(node.DefaultConfig.GraphQLVirtualHosts, ","),
	}
	WSEnabledFlag = cli.BoolFlag{
		Name:  "ws",
		Usage: "Enable the WS-RPC server",
		// Usage: WS-RPCサーバーを有効にする
	}
	WSListenAddrFlag = cli.StringFlag{
		Name:  "ws.addr",
		Usage: "WS-RPC server listening interface",
		// Usage: WS-RPCサーバーリスニングインターフェイス
		Value: node.DefaultWSHost,
	}
	WSPortFlag = cli.IntFlag{
		Name:  "ws.port",
		Usage: "WS-RPC server listening port",
		// Usage: WS-RPCサーバーのリスニングポート
		Value: node.DefaultWSPort,
	}
	WSApiFlag = cli.StringFlag{
		Name:  "ws.api",
		Usage: "API's offered over the WS-RPC interface",
		// Usage: WS-RPCインターフェースを介して提供されるAPI
		Value: "",
	}
	WSAllowedOriginsFlag = cli.StringFlag{
		Name:  "ws.origins",
		Usage: "Origins from which to accept websockets requests",
		// Usage: WebSocketリクエストを受け入れるオリジン
		Value: "",
	}
	WSPathPrefixFlag = cli.StringFlag{
		Name:  "ws.rpcprefix",
		Usage: "HTTP path prefix on which JSON-RPC is served. Use '/' to serve on all paths.",
		// Usage: JSON-RPCが提供されるHTTPパスプレフィックス。 '/'を使用して、すべてのパスでサービスを提供します。
		Value: "",
	}
	ExecFlag = cli.StringFlag{
		Name:  "exec",
		Usage: "Execute JavaScript statement",
		// Usage: JavaScriptステートメントを実行する
	}
	PreloadJSFlag = cli.StringFlag{
		Name:  "preload",
		Usage: "Comma separated list of JavaScript files to preload into the console",
		// Usage: コンソールにプリロードするJavaScriptファイルのコンマ区切りリスト
	}
	AllowUnprotectedTxs = cli.BoolFlag{
		Name:  "rpc.allow-unprotected-txs",
		Usage: "Allow for unprotected (non EIP155 signed) transactions to be submitted via RPC",
		// Usage: 保護されていない（EIP155で署名されていない）トランザクションをRPC経由で送信できるようにする
	}

	// Network Settings
	MaxPeersFlag = cli.IntFlag{
		Name:  "maxpeers",
		Usage: "Maximum number of network peers (network disabled if set to 0)",
		// Usage:	ネットワークピアの最大数（0に設定するとネットワークが無効になります）
		Value: node.DefaultConfig.P2P.MaxPeers,
	}
	MaxPendingPeersFlag = cli.IntFlag{
		Name:  "maxpendpeers",
		Usage: "Maximum number of pending connection attempts (defaults used if set to 0)",
		// Usage: 保留中の接続試行の最大数（0に設定されている場合はデフォルトが使用されます）
		Value: node.DefaultConfig.P2P.MaxPendingPeers,
	}
	ListenPortFlag = cli.IntFlag{
		Name:  "port",
		Usage: "Network listening port",
		// Usage: ネットワークリスニングポート
		Value: 30303,
	}
	BootnodesFlag = cli.StringFlag{
		Name:  "bootnodes",
		Usage: "Comma separated enode URLs for P2P discovery bootstrap",
		// Usage: P2Pディスカバリブートストラップ用のコンマ区切りのenodeURL
		Value: "",
	}
	NodeKeyFileFlag = cli.StringFlag{
		Name:  "nodekey",
		Usage: "P2P node key file",
		// Usage: P2Pノードキーファイル
	}
	NodeKeyHexFlag = cli.StringFlag{
		Name:  "nodekeyhex",
		Usage: "P2P node key as hex (for testing)",
		// Usage: 16進数としてのP2Pノードキー（テスト用）
	}
	NATFlag = cli.StringFlag{
		Name:  "nat",
		Usage: "NAT port mapping mechanism (any|none|upnp|pmp|extip:<IP>)",
		// Usage: NATポートマッピングメカニズム（any | none | upnp | pmp | extip：<IP>）
		Value: "any",
	}
	NoDiscoverFlag = cli.BoolFlag{
		Name:  "nodiscover",
		Usage: "Disables the peer discovery mechanism (manual peer addition)",
		// Usage: ピア検出メカニズムを無効にします（手動ピア追加）
	}
	DiscoveryV5Flag = cli.BoolFlag{
		Name:  "v5disc",
		Usage: "Enables the experimental RLPx V5 (Topic Discovery) mechanism",
		// Usage: 実験的なRLPxV5（トピック検出）メカニズムを有効にします
	}
	NetrestrictFlag = cli.StringFlag{
		Name:  "netrestrict",
		Usage: "Restricts network communication to the given IP networks (CIDR masks)",
		// Usage: ネットワーク通信を特定のIPネットワークに制限します（CIDRマスク）
	}
	DNSDiscoveryFlag = cli.StringFlag{
		Name:  "discovery.dns",
		Usage: "Sets DNS discovery entry points (use \"\" to disable DNS)",
		// Usage: DNS検出エントリポイントを設定します（DNSを無効にするには\ "\"を使用します）
	}

	// ATM the url is left to the user and deployment to// ATMのURLはユーザーに任され、展開は
	JSpathFlag = DirectoryFlag{
		Name:  "jspath",
		Usage: "JavaScript root path for `loadScript`",
		// Usage: `loadScript`のJavaScriptルートパス
		Value: DirectoryString("."),
	}

	// Gas price oracle settings // ガス価格オラクル設定
	GpoBlocksFlag = cli.IntFlag{
		Name:  "gpo.blocks",
		Usage: "Number of recent blocks to check for gas prices",
		// Usage: ガス価格をチェックするための最近のブロックの数
		Value: ethconfig.Defaults.GPO.Blocks,
	}
	GpoPercentileFlag = cli.IntFlag{
		Name:  "gpo.percentile",
		Usage: "Suggested gas price is the given percentile of a set of recent transaction gas prices",
		// Usage: 推奨ガス価格は、最近の一連の取引ガス価格の特定のパーセンタイルです。
		Value: ethconfig.Defaults.GPO.Percentile,
	}
	GpoMaxGasPriceFlag = cli.Int64Flag{
		Name:  "gpo.maxprice",
		Usage: "Maximum transaction priority fee (or gasprice before London fork) to be recommended by gpo",
		// Usage: gpoが推奨する最大取引優先手数料（またはロンドンフォーク前のガス価格）
		Value: ethconfig.Defaults.GPO.MaxPrice.Int64(),
	}
	GpoIgnoreGasPriceFlag = cli.Int64Flag{
		Name:  "gpo.ignoreprice",
		Usage: "Gas price below which gpo will ignore transactions",
		// Usage: gpoがトランザクションを無視するガス価格
		Value: ethconfig.Defaults.GPO.IgnorePrice.Int64(),
	}

	// Metrics flags
	MetricsEnabledFlag = cli.BoolFlag{
		Name:  "metrics",
		Usage: "Enable metrics collection and reporting",
		// Usage: メトリックの収集とレポートを有効にする
	}
	MetricsEnabledExpensiveFlag = cli.BoolFlag{
		Name:  "metrics.expensive",
		Usage: "Enable expensive metrics collection and reporting",
		// Usage: 高価なメトリクスの収集とレポートを有効にする
	}

	// MetricsHTTPFlag defines the endpoint for a stand-alone metrics HTTP endpoint.
	// Since the pprof service enables sensitive/vulnerable behavior, this allows a user
	// to enable a public-OK metrics endpoint without having to worry about ALSO exposing
	// other profiling behavior or information.
	// MetricsHTTPFlagは、スタンドアロンメトリックHTTPエンドポイントのエンドポイントを定義します。
	// pprofサービスは機密性の高い/脆弱な動作を有効にするため、
	// ユーザーは他のプロファイリング動作や情報を公開することを心配することなく、public-OKメトリックエンドポイントを有効にできます。
	MetricsHTTPFlag = cli.StringFlag{
		Name:  "metrics.addr",
		Usage: "Enable stand-alone metrics HTTP server listening interface",
		// Usage: スタンドアロンメトリックHTTPサーバーリスニングインターフェイスを有効にする
		Value: metrics.DefaultConfig.HTTP,
	}
	MetricsPortFlag = cli.IntFlag{
		Name:  "metrics.port",
		Usage: "Metrics HTTP server listening port",
		// Usage: メトリクスHTTPサーバーリスニングポート
		Value: metrics.DefaultConfig.Port,
	}
	MetricsEnableInfluxDBFlag = cli.BoolFlag{
		Name:  "metrics.influxdb",
		Usage: "Enable metrics export/push to an external InfluxDB database",
		// Usage: 外部InfluxDBデータベースへのメトリックのエクスポート/プッシュを有効にする
	}
	MetricsInfluxDBEndpointFlag = cli.StringFlag{
		Name:  "metrics.influxdb.endpoint",
		Usage: "InfluxDB API endpoint to report metrics to",
		// Usage: メトリックをレポートするInfluxDBAPIエンドポイント
		Value: metrics.DefaultConfig.InfluxDBEndpoint,
	}
	MetricsInfluxDBDatabaseFlag = cli.StringFlag{
		Name:  "metrics.influxdb.database",
		Usage: "InfluxDB database name to push reported metrics to",
		// Usage: 報告されたメトリックをプッシュするInfluxDBデータベース名
		Value: metrics.DefaultConfig.InfluxDBDatabase,
	}
	MetricsInfluxDBUsernameFlag = cli.StringFlag{
		Name:  "metrics.influxdb.username",
		Usage: "Username to authorize access to the database",
		// Usage: データベースへのアクセスを許可するユーザー名
		Value: metrics.DefaultConfig.InfluxDBUsername,
	}
	MetricsInfluxDBPasswordFlag = cli.StringFlag{
		Name:  "metrics.influxdb.password",
		Usage: "Password to authorize access to the database",
		// Usage: データベースへのアクセスを許可するためのパスワード
		Value: metrics.DefaultConfig.InfluxDBPassword,
	}
	// Tags are part of every measurement sent to InfluxDB. Queries on tags are faster in InfluxDB.
	// For example `host` tag could be used so that we can group all nodes and average a measurement
	// across all of them, but also so that we can select a specific node and inspect its measurements.
	// タグは、InfluxDBに送信されるすべての測定値の一部です。 InfluxDBでは、タグのクエリが高速になります。
	// たとえば、 `host`タグを使用すると、すべてのノードをグループ化してすべてのノードの測定値を平均化できますが、
	// 特定のノードを選択してその測定値を検査することもできます。
	// https://docs.influxdata.com/influxdb/v1.4/concepts/key_concepts/#tag-key
	MetricsInfluxDBTagsFlag = cli.StringFlag{
		Name:  "metrics.influxdb.tags",
		Usage: "Comma-separated InfluxDB tags (key/values) attached to all measurements",
		// Usage: すべての測定値に添付されたコンマ区切りのInfluxDBタグ（キー/値）
		Value: metrics.DefaultConfig.InfluxDBTags,
	}

	MetricsEnableInfluxDBV2Flag = cli.BoolFlag{
		Name:  "metrics.influxdbv2",
		Usage: "Enable metrics export/push to an external InfluxDB v2 database",
		// Usage: 外部InfluxDBv2データベースへのメトリックのエクスポート/プッシュを有効にする
	}

	MetricsInfluxDBTokenFlag = cli.StringFlag{
		Name:  "metrics.influxdb.token",
		Usage: "Token to authorize access to the database (v2 only)",
		// Usage: データベースへのアクセスを承認するためのトークン（v2のみ）
		Value: metrics.DefaultConfig.InfluxDBToken,
	}

	MetricsInfluxDBBucketFlag = cli.StringFlag{
		Name:  "metrics.influxdb.bucket",
		Usage: "InfluxDB bucket name to push reported metrics to (v2 only)",
		// Usage: 報告されたメトリックをプッシュするInfluxDBバケット名（v2のみ）
		Value: metrics.DefaultConfig.InfluxDBBucket,
	}

	MetricsInfluxDBOrganizationFlag = cli.StringFlag{
		Name:  "metrics.influxdb.organization",
		Usage: "InfluxDB organization name (v2 only)",
		// Usage: InfluxDB組織名（v2のみ）
		Value: metrics.DefaultConfig.InfluxDBOrganization,
	}

	CatalystFlag = cli.BoolFlag{
		Name:  "catalyst",
		Usage: "Catalyst mode (eth2 integration testing)",
		// Usage: Catalystモード（eth2統合テスト）
	}
)

// MakeDataDir retrieves the currently requested data directory, terminating
// if none (or the empty string) is specified. If the node is starting a testnet,
// then a subdirectory of the specified datadir will be used.
// MakeDataDirは、現在要求されているデータディレクトリを取得し、何も（または空の文字列）が指定されていない場合は終了します。
// ノードがテストネットを開始している場合は、指定されたdatadirのサブディレクトリが使用されます。
func MakeDataDir(ctx *cli.Context) string {
	if path := ctx.GlobalString(DataDirFlag.Name); path != "" {
		if ctx.GlobalBool(RopstenFlag.Name) {
			// Maintain compatibility with older Geth configurations storing the
			// Ropsten database in `testnet` instead of `ropsten`.
			// Usage: Ropstenデータベースを `ropsten`ではなく` testnet`に保存する古いGeth構成との互換性を維持します。
			return filepath.Join(path, "ropsten")
		}
		if ctx.GlobalBool(RinkebyFlag.Name) {
			return filepath.Join(path, "rinkeby")
		}
		if ctx.GlobalBool(GoerliFlag.Name) {
			return filepath.Join(path, "goerli")
		}
		if ctx.GlobalBool(SepoliaFlag.Name) {
			return filepath.Join(path, "sepolia")
		}
		return path
	}
	Fatalf("Cannot determine default data directory, please set manually (--datadir)") // デフォルトのデータディレクトリを特定できません。手動で設定してください（--datadir）
	return ""
}

// setNodeKey creates a node key from set command line flags, either loading it
// from a file or as a specified hex value. If neither flags were provided, this
// method returns nil and an emphemeral key is to be generated.
// setNodeKeyは、setコマンドラインフラグからノードキーを作成し、ファイルからロードするか、指定された16進値としてロードします。
// どちらのフラグも指定されていない場合、このメソッドはnilを返し、エフェメラルキーが生成されます。
func setNodeKey(ctx *cli.Context, cfg *p2p.Config) {
	var (
		hex  = ctx.GlobalString(NodeKeyHexFlag.Name)
		file = ctx.GlobalString(NodeKeyFileFlag.Name)
		key  *ecdsa.PrivateKey
		err  error
	)
	switch {
	case file != "" && hex != "":
		Fatalf("Options %q and %q are mutually exclusive", NodeKeyFileFlag.Name, NodeKeyHexFlag.Name)
	case file != "":
		if key, err = crypto.LoadECDSA(file); err != nil {
			Fatalf("Option %q: %v", NodeKeyFileFlag.Name, err)
		}
		cfg.PrivateKey = key
	case hex != "":
		if key, err = crypto.HexToECDSA(hex); err != nil {
			Fatalf("Option %q: %v", NodeKeyHexFlag.Name, err)
		}
		cfg.PrivateKey = key
	}
}

// setNodeUserIdent creates the user identifier from CLI flags.
// setNodeUserIdentは、CLIフラグからユーザー識別子を作成します。
func setNodeUserIdent(ctx *cli.Context, cfg *node.Config) {
	if identity := ctx.GlobalString(IdentityFlag.Name); len(identity) > 0 {
		cfg.UserIdent = identity
	}
}

// setBootstrapNodes creates a list of bootstrap nodes from the command line
// flags, reverting to pre-configured ones if none have been specified.
// setBootstrapNodesは、コマンドラインフラグからブートストラップノードのリストを作成し、何も指定されていない場合は事前設定されたノードに戻します。
func setBootstrapNodes(ctx *cli.Context, cfg *p2p.Config) {
	urls := params.MainnetBootnodes
	switch {
	case ctx.GlobalIsSet(BootnodesFlag.Name):
		urls = SplitAndTrim(ctx.GlobalString(BootnodesFlag.Name))
	case ctx.GlobalBool(RopstenFlag.Name):
		urls = params.RopstenBootnodes
	case ctx.GlobalBool(SepoliaFlag.Name):
		urls = params.SepoliaBootnodes
	case ctx.GlobalBool(RinkebyFlag.Name):
		urls = params.RinkebyBootnodes
	case ctx.GlobalBool(GoerliFlag.Name):
		urls = params.GoerliBootnodes
	case cfg.BootstrapNodes != nil:
		return // already set, don't apply defaults. すでに設定されているため、デフォルトを適用しないでください。
	}

	cfg.BootstrapNodes = make([]*enode.Node, 0, len(urls))
	for _, url := range urls {
		if url != "" {
			node, err := enode.Parse(enode.ValidSchemes, url)
			if err != nil {
				log.Crit("Bootstrap URL invalid", "enode", url, "err", err)
				continue
			}
			cfg.BootstrapNodes = append(cfg.BootstrapNodes, node)
		}
	}
}

// setBootstrapNodesV5 creates a list of bootstrap nodes from the command line
// flags, reverting to pre-configured ones if none have been specified.
// setBootstrapNodesV5は、コマンドラインフラグからブートストラップノードのリストを作成し、何も指定されていない場合は事前設定されたノードに戻します。
func setBootstrapNodesV5(ctx *cli.Context, cfg *p2p.Config) {
	urls := params.V5Bootnodes
	switch {
	case ctx.GlobalIsSet(BootnodesFlag.Name):
		urls = SplitAndTrim(ctx.GlobalString(BootnodesFlag.Name))
	case cfg.BootstrapNodesV5 != nil:
		return // already set, don't apply defaults. すでに設定されているため、デフォルトを適用しないでください。
	}

	cfg.BootstrapNodesV5 = make([]*enode.Node, 0, len(urls))
	for _, url := range urls {
		if url != "" {
			node, err := enode.Parse(enode.ValidSchemes, url)
			if err != nil {
				log.Error("Bootstrap URL invalid", "enode", url, "err", err)
				continue
			}
			cfg.BootstrapNodesV5 = append(cfg.BootstrapNodesV5, node)
		}
	}
}

// setListenAddress creates a TCP listening address string from set command
// line flags.
// setListenAddressは、setコマンドラインフラグからTCPリスニングアドレス文字列を作成します。
func setListenAddress(ctx *cli.Context, cfg *p2p.Config) {
	if ctx.GlobalIsSet(ListenPortFlag.Name) {
		cfg.ListenAddr = fmt.Sprintf(":%d", ctx.GlobalInt(ListenPortFlag.Name))
	}
}

// setNAT creates a port mapper from command line flags.
// setNATは、コマンドラインフラグからポートマッパーを作成します。
func setNAT(ctx *cli.Context, cfg *p2p.Config) {
	if ctx.GlobalIsSet(NATFlag.Name) {
		natif, err := nat.Parse(ctx.GlobalString(NATFlag.Name))
		if err != nil {
			Fatalf("Option %s: %v", NATFlag.Name, err)
		}
		cfg.NAT = natif
	}
}

// SplitAndTrim splits input separated by a comma
// and trims excessive white space from the substrings.
// SplitAndTrimは、入力をコンマで区切って分割し、部分文字列から余分な空白を削除します。
func SplitAndTrim(input string) (ret []string) {
	l := strings.Split(input, ",")
	for _, r := range l {
		if r = strings.TrimSpace(r); r != "" {
			ret = append(ret, r)
		}
	}
	return ret
}

// setHTTP creates the HTTP RPC listener interface string from the set
// command line flags, returning empty if the HTTP endpoint is disabled.
// setHTTPは、setコマンドラインフラグからHTTP RPCリスナーインターフェイス文字列を作成し、
// HTTPエンドポイントが無効になっている場合は空を返します。
func setHTTP(ctx *cli.Context, cfg *node.Config) {
	if ctx.GlobalBool(HTTPEnabledFlag.Name) && cfg.HTTPHost == "" {
		cfg.HTTPHost = "127.0.0.1"
		if ctx.GlobalIsSet(HTTPListenAddrFlag.Name) {
			cfg.HTTPHost = ctx.GlobalString(HTTPListenAddrFlag.Name)
		}
	}

	if ctx.GlobalIsSet(HTTPPortFlag.Name) {
		cfg.HTTPPort = ctx.GlobalInt(HTTPPortFlag.Name)
	}

	if ctx.GlobalIsSet(HTTPCORSDomainFlag.Name) {
		cfg.HTTPCors = SplitAndTrim(ctx.GlobalString(HTTPCORSDomainFlag.Name))
	}

	if ctx.GlobalIsSet(HTTPApiFlag.Name) {
		cfg.HTTPModules = SplitAndTrim(ctx.GlobalString(HTTPApiFlag.Name))
	}

	if ctx.GlobalIsSet(HTTPVirtualHostsFlag.Name) {
		cfg.HTTPVirtualHosts = SplitAndTrim(ctx.GlobalString(HTTPVirtualHostsFlag.Name))
	}

	if ctx.GlobalIsSet(HTTPPathPrefixFlag.Name) {
		cfg.HTTPPathPrefix = ctx.GlobalString(HTTPPathPrefixFlag.Name)
	}
	if ctx.GlobalIsSet(AllowUnprotectedTxs.Name) {
		cfg.AllowUnprotectedTxs = ctx.GlobalBool(AllowUnprotectedTxs.Name)
	}
}

// setGraphQL creates the GraphQL listener interface string from the set
// command line flags, returning empty if the GraphQL endpoint is disabled.
// setGraphQLは、setコマンドラインフラグからGraphQLリスナーインターフェイス文字列を作成し、GraphQLエンドポイントが無効になっている場合は空を返します。
func setGraphQL(ctx *cli.Context, cfg *node.Config) {
	if ctx.GlobalIsSet(GraphQLCORSDomainFlag.Name) {
		cfg.GraphQLCors = SplitAndTrim(ctx.GlobalString(GraphQLCORSDomainFlag.Name))
	}
	if ctx.GlobalIsSet(GraphQLVirtualHostsFlag.Name) {
		cfg.GraphQLVirtualHosts = SplitAndTrim(ctx.GlobalString(GraphQLVirtualHostsFlag.Name))
	}
}

// setWS creates the WebSocket RPC listener interface string from the set
// command line flags, returning empty if the HTTP endpoint is disabled.
// setWSは、setコマンドラインフラグからWebSocket RPCリスナーインターフェイス文字列を作成し、HTTPエンドポイントが無効になっている場合は空を返します。
func setWS(ctx *cli.Context, cfg *node.Config) {
	if ctx.GlobalBool(WSEnabledFlag.Name) && cfg.WSHost == "" {
		cfg.WSHost = "127.0.0.1"
		if ctx.GlobalIsSet(WSListenAddrFlag.Name) {
			cfg.WSHost = ctx.GlobalString(WSListenAddrFlag.Name)
		}
	}
	if ctx.GlobalIsSet(WSPortFlag.Name) {
		cfg.WSPort = ctx.GlobalInt(WSPortFlag.Name)
	}

	if ctx.GlobalIsSet(WSAllowedOriginsFlag.Name) {
		cfg.WSOrigins = SplitAndTrim(ctx.GlobalString(WSAllowedOriginsFlag.Name))
	}

	if ctx.GlobalIsSet(WSApiFlag.Name) {
		cfg.WSModules = SplitAndTrim(ctx.GlobalString(WSApiFlag.Name))
	}

	if ctx.GlobalIsSet(WSPathPrefixFlag.Name) {
		cfg.WSPathPrefix = ctx.GlobalString(WSPathPrefixFlag.Name)
	}
}

// setIPC creates an IPC path configuration from the set command line flags,
// returning an empty string if IPC was explicitly disabled, or the set path.
// setIPCは、setコマンドラインフラグからIPCパス構成を作成し、IPCが明示的に無効にされている場合は空の文字列、またはsetpathを返します。
func setIPC(ctx *cli.Context, cfg *node.Config) {
	CheckExclusive(ctx, IPCDisabledFlag, IPCPathFlag)
	switch {
	case ctx.GlobalBool(IPCDisabledFlag.Name):
		cfg.IPCPath = ""
	case ctx.GlobalIsSet(IPCPathFlag.Name):
		cfg.IPCPath = ctx.GlobalString(IPCPathFlag.Name)
	}
}

// setLes configures the les server and ultra light client settings from the command line flags.
// setLesは、コマンドラインフラグからファイルサーバーと超軽量クライアントの設定を構成します。
func setLes(ctx *cli.Context, cfg *ethconfig.Config) {
	if ctx.GlobalIsSet(LightServeFlag.Name) {
		cfg.LightServ = ctx.GlobalInt(LightServeFlag.Name)
	}
	if ctx.GlobalIsSet(LightIngressFlag.Name) {
		cfg.LightIngress = ctx.GlobalInt(LightIngressFlag.Name)
	}
	if ctx.GlobalIsSet(LightEgressFlag.Name) {
		cfg.LightEgress = ctx.GlobalInt(LightEgressFlag.Name)
	}
	if ctx.GlobalIsSet(LightMaxPeersFlag.Name) {
		cfg.LightPeers = ctx.GlobalInt(LightMaxPeersFlag.Name)
	}
	if ctx.GlobalIsSet(UltraLightServersFlag.Name) {
		cfg.UltraLightServers = strings.Split(ctx.GlobalString(UltraLightServersFlag.Name), ",")
	}
	if ctx.GlobalIsSet(UltraLightFractionFlag.Name) {
		cfg.UltraLightFraction = ctx.GlobalInt(UltraLightFractionFlag.Name)
	}
	if cfg.UltraLightFraction <= 0 && cfg.UltraLightFraction > 100 {
		log.Error("Ultra light fraction is invalid", "had", cfg.UltraLightFraction, "updated", ethconfig.Defaults.UltraLightFraction)
		cfg.UltraLightFraction = ethconfig.Defaults.UltraLightFraction
	}
	if ctx.GlobalIsSet(UltraLightOnlyAnnounceFlag.Name) {
		cfg.UltraLightOnlyAnnounce = ctx.GlobalBool(UltraLightOnlyAnnounceFlag.Name)
	}
	if ctx.GlobalIsSet(LightNoPruneFlag.Name) {
		cfg.LightNoPrune = ctx.GlobalBool(LightNoPruneFlag.Name)
	}
	if ctx.GlobalIsSet(LightNoSyncServeFlag.Name) {
		cfg.LightNoSyncServe = ctx.GlobalBool(LightNoSyncServeFlag.Name)
	}
}

// MakeDatabaseHandles raises out the number of allowed file handles per process
// for Geth and returns half of the allowance to assign to the database.
// MakeDatabaseHandlesは、Gethのプロセスごとに許可されるファイルハンドルの数を増やし、データベースに割り当てる許可の半分を返します。
func MakeDatabaseHandles() int {
	limit, err := fdlimit.Maximum()
	if err != nil {
		Fatalf("Failed to retrieve file descriptor allowance: %v", err)
	}
	raised, err := fdlimit.Raise(uint64(limit))
	if err != nil {
		Fatalf("Failed to raise file descriptor allowance: %v", err)
	}
	return int(raised / 2) // Leave half for networking and other stuff ネットワーキングやその他のもののために半分を残してください
}

// MakeAddress converts an account specified directly as a hex encoded string or
// a key index in the key store to an internal account representation.
// MakeAddressは、16進エンコードされた文字列またはキーストア内のキーインデックスとして直接指定されたアカウントを内部アカウント表現に変換します。
func MakeAddress(ks *keystore.KeyStore, account string) (accounts.Account, error) {
	// If the specified account is a valid address, return it // 指定されたアカウントが有効なアドレスである場合は、それを返します
	if common.IsHexAddress(account) {
		return accounts.Account{Address: common.HexToAddress(account)}, nil
	}
	// Otherwise try to interpret the account as a keystore index // それ以外の場合は、アカウントをキーストアインデックスとして解釈してみてください
	index, err := strconv.Atoi(account)
	if err != nil || index < 0 {
		return accounts.Account{}, fmt.Errorf("invalid account address or index %q", account) // 無効なアカウントアドレスまたはインデックス％q
	}
	log.Warn("-------------------------------------------------------------------")
	log.Warn("Referring to accounts by order in the keystore folder is dangerous!") // キーストアフォルダ内のアカウントを順番に参照するのは危険です。
	log.Warn("This functionality is deprecated and will be removed in the future!") // この機能は非推奨であり、将来削除される予定です。
	log.Warn("Please use explicit addresses! (can search via `geth account list`)") // 明示的なアドレスを使用してください！ （ `geth account list`で検索できます）
	log.Warn("-------------------------------------------------------------------")

	accs := ks.Accounts()
	if len(accs) <= index {
		return accounts.Account{}, fmt.Errorf("index %d higher than number of accounts %d", index, len(accs)) // インデックス％dがアカウント数％dよりも大きい
	}
	return accs[index], nil
}

// setEtherbase retrieves the etherbase either from the directly specified
// command line flags or from the keystore if CLI indexed.
// setEtherbaseは、直接指定されたコマンドラインフラグから、またはCLIがインデックス付けされている場合はキーストアから、etherbaseを取得します。
func setEtherbase(ctx *cli.Context, ks *keystore.KeyStore, cfg *ethconfig.Config) {
	// Extract the current etherbase
	// 現在のetherbaseを抽出します
	var etherbase string
	if ctx.GlobalIsSet(MinerEtherbaseFlag.Name) {
		etherbase = ctx.GlobalString(MinerEtherbaseFlag.Name)
	}
	// Convert the etherbase into an address and configure it
	// エーテルベースをアドレスに変換して構成します
	if etherbase != "" {
		if ks != nil {
			account, err := MakeAddress(ks, etherbase)
			if err != nil {
				Fatalf("Invalid miner etherbase: %v", err)
			}
			cfg.Miner.Etherbase = account.Address
		} else {
			Fatalf("No etherbase configured")
		}
	}
}

// MakePasswordList reads password lines from the file specified by the global --password flag.
// MakePasswordListは、グローバル--passwordフラグで指定されたファイルからパスワード行を読み取ります。
func MakePasswordList(ctx *cli.Context) []string {
	path := ctx.GlobalString(PasswordFileFlag.Name)
	if path == "" {
		return nil
	}
	text, err := ioutil.ReadFile(path)
	if err != nil {
		Fatalf("Failed to read password file: %v", err)
	}
	lines := strings.Split(string(text), "\n")
	// Sanitise DOS line endings.
	// DOSの行末をサニタイズします
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	return lines
}

func SetP2PConfig(ctx *cli.Context, cfg *p2p.Config) {
	setNodeKey(ctx, cfg)
	setNAT(ctx, cfg)
	setListenAddress(ctx, cfg)
	setBootstrapNodes(ctx, cfg)
	setBootstrapNodesV5(ctx, cfg)

	lightClient := ctx.GlobalString(SyncModeFlag.Name) == "light"
	lightServer := (ctx.GlobalInt(LightServeFlag.Name) != 0)

	lightPeers := ctx.GlobalInt(LightMaxPeersFlag.Name)
	if lightClient && !ctx.GlobalIsSet(LightMaxPeersFlag.Name) {
		// dynamic default - for clients we use 1/10th of the default for servers
		// 動的デフォルト-クライアントの場合、サーバーのデフォルトの1/10を使用します
		lightPeers /= 10
	}

	if ctx.GlobalIsSet(MaxPeersFlag.Name) {
		cfg.MaxPeers = ctx.GlobalInt(MaxPeersFlag.Name)
		if lightServer && !ctx.GlobalIsSet(LightMaxPeersFlag.Name) {
			cfg.MaxPeers += lightPeers
		}
	} else {
		if lightServer {
			cfg.MaxPeers += lightPeers
		}
		if lightClient && ctx.GlobalIsSet(LightMaxPeersFlag.Name) && cfg.MaxPeers < lightPeers {
			cfg.MaxPeers = lightPeers
		}
	}
	if !(lightClient || lightServer) {
		lightPeers = 0
	}
	ethPeers := cfg.MaxPeers - lightPeers
	if lightClient {
		ethPeers = 0
	}
	log.Info("Maximum peer count", "ETH", ethPeers, "LES", lightPeers, "total", cfg.MaxPeers)

	if ctx.GlobalIsSet(MaxPendingPeersFlag.Name) {
		cfg.MaxPendingPeers = ctx.GlobalInt(MaxPendingPeersFlag.Name)
	}
	if ctx.GlobalIsSet(NoDiscoverFlag.Name) || lightClient {
		cfg.NoDiscovery = true
	}

	// if we're running a light client or server, force enable the v5 peer discovery
	// unless it is explicitly disabled with --nodiscover note that explicitly specifying
	// --v5disc overrides --nodiscover, in which case the later only disables v4 discovery
	// ライトクライアントまたはサーバーを実行している場合は、-nodiscoverで明示的に無効にされていない限り、v5ピア検出を強制的に有効にします。
	// -v5discを明示的に指定すると--nodiscoverがオーバーライドされます。この場合、後者ではv4検出のみが無効になります。
	forceV5Discovery := (lightClient || lightServer) && !ctx.GlobalBool(NoDiscoverFlag.Name)
	if ctx.GlobalIsSet(DiscoveryV5Flag.Name) {
		cfg.DiscoveryV5 = ctx.GlobalBool(DiscoveryV5Flag.Name)
	} else if forceV5Discovery {
		cfg.DiscoveryV5 = true
	}

	if netrestrict := ctx.GlobalString(NetrestrictFlag.Name); netrestrict != "" {
		list, err := netutil.ParseNetlist(netrestrict)
		if err != nil {
			Fatalf("Option %q: %v", NetrestrictFlag.Name, err)
		}
		cfg.NetRestrict = list
	}

	if ctx.GlobalBool(DeveloperFlag.Name) {
		// --dev mode can't use p2p networking.
		// --devモードはp2pネットワークを使用できません。
		cfg.MaxPeers = 0
		cfg.ListenAddr = ""
		cfg.NoDial = true
		cfg.NoDiscovery = true
		cfg.DiscoveryV5 = false
	}
}

// SetNodeConfig applies node-related command line flags to the config.
// SetNodeConfigは、ノード関連のコマンドラインフラグを構成に適用します。
func SetNodeConfig(ctx *cli.Context, cfg *node.Config) {
	SetP2PConfig(ctx, &cfg.P2P)
	setIPC(ctx, cfg)
	setHTTP(ctx, cfg)
	setGraphQL(ctx, cfg)
	setWS(ctx, cfg)
	setNodeUserIdent(ctx, cfg)
	setDataDir(ctx, cfg)
	setSmartCard(ctx, cfg)

	if ctx.GlobalIsSet(ExternalSignerFlag.Name) {
		cfg.ExternalSigner = ctx.GlobalString(ExternalSignerFlag.Name)
	}

	if ctx.GlobalIsSet(KeyStoreDirFlag.Name) {
		cfg.KeyStoreDir = ctx.GlobalString(KeyStoreDirFlag.Name)
	}
	if ctx.GlobalIsSet(DeveloperFlag.Name) {
		cfg.UseLightweightKDF = true
	}
	if ctx.GlobalIsSet(LightKDFFlag.Name) {
		cfg.UseLightweightKDF = ctx.GlobalBool(LightKDFFlag.Name)
	}
	if ctx.GlobalIsSet(NoUSBFlag.Name) || cfg.NoUSB {
		log.Warn("Option nousb is deprecated and USB is deactivated by default. Use --usb to enable")
	}
	if ctx.GlobalIsSet(USBFlag.Name) {
		cfg.USB = ctx.GlobalBool(USBFlag.Name)
	}
	if ctx.GlobalIsSet(InsecureUnlockAllowedFlag.Name) {
		cfg.InsecureUnlockAllowed = ctx.GlobalBool(InsecureUnlockAllowedFlag.Name)
	}
}

func setSmartCard(ctx *cli.Context, cfg *node.Config) {
	// Skip enabling smartcards if no path is set
	// パスが設定されていない場合は、スマートカードの有効化をスキップします
	path := ctx.GlobalString(SmartCardDaemonPathFlag.Name)
	if path == "" {
		return
	}
	// Sanity check that the smartcard path is valid
	// スマートカードパスが有効であることの健全性チェック
	fi, err := os.Stat(path)
	if err != nil {
		log.Info("Smartcard socket not found, disabling", "err", err)
		return
	}
	if fi.Mode()&os.ModeType != os.ModeSocket {
		log.Error("Invalid smartcard daemon path", "path", path, "type", fi.Mode().String())
		return
	}
	// Smartcard daemon path exists and is a socket, enable it
	// スマートカードデーモンパスが存在し、ソケットである場合は、それを有効にします
	cfg.SmartCardDaemonPath = path
}

func setDataDir(ctx *cli.Context, cfg *node.Config) {
	switch {
	case ctx.GlobalIsSet(DataDirFlag.Name):
		cfg.DataDir = ctx.GlobalString(DataDirFlag.Name)
	case ctx.GlobalBool(DeveloperFlag.Name):
		cfg.DataDir = "" // unless explicitly requested, use memory databases // 明示的に要求されない限り、メモリデータベースを使用してください
	case ctx.GlobalBool(RopstenFlag.Name) && cfg.DataDir == node.DefaultDataDir():
		// Maintain compatibility with older Geth configurations storing the
		// Ropsten database in `testnet` instead of `ropsten`.
		// Ropstenデータベースを `ropsten`ではなく` testnet`に保存する古いGeth構成との互換性を維持します。
		legacyPath := filepath.Join(node.DefaultDataDir(), "testnet")
		if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
			log.Warn("Using the deprecated `testnet` datadir. Future versions will store the Ropsten chain in `ropsten`.") // 非推奨の `testnet`datadirを使用します。将来のバージョンでは、Ropstenチェーンが `ropsten`に保存されます。
			cfg.DataDir = legacyPath
		} else {
			cfg.DataDir = filepath.Join(node.DefaultDataDir(), "ropsten")
		}

		cfg.DataDir = filepath.Join(node.DefaultDataDir(), "ropsten")
	case ctx.GlobalBool(RinkebyFlag.Name) && cfg.DataDir == node.DefaultDataDir():
		cfg.DataDir = filepath.Join(node.DefaultDataDir(), "rinkeby")
	case ctx.GlobalBool(GoerliFlag.Name) && cfg.DataDir == node.DefaultDataDir():
		cfg.DataDir = filepath.Join(node.DefaultDataDir(), "goerli")
	case ctx.GlobalBool(SepoliaFlag.Name) && cfg.DataDir == node.DefaultDataDir():
		cfg.DataDir = filepath.Join(node.DefaultDataDir(), "sepolia")
	}
}

func setGPO(ctx *cli.Context, cfg *gasprice.Config, light bool) {
	// If we are running the light client, apply another group
	// settings for gas oracle.
	// ライトクライアントを実行している場合は、gasoracleに別のグループ設定を適用します。
	if light {
		*cfg = ethconfig.LightClientGPO
	}
	if ctx.GlobalIsSet(GpoBlocksFlag.Name) {
		cfg.Blocks = ctx.GlobalInt(GpoBlocksFlag.Name)
	}
	if ctx.GlobalIsSet(GpoPercentileFlag.Name) {
		cfg.Percentile = ctx.GlobalInt(GpoPercentileFlag.Name)
	}
	if ctx.GlobalIsSet(GpoMaxGasPriceFlag.Name) {
		cfg.MaxPrice = big.NewInt(ctx.GlobalInt64(GpoMaxGasPriceFlag.Name))
	}
	if ctx.GlobalIsSet(GpoIgnoreGasPriceFlag.Name) {
		cfg.IgnorePrice = big.NewInt(ctx.GlobalInt64(GpoIgnoreGasPriceFlag.Name))
	}
}

func setTxPool(ctx *cli.Context, cfg *core.TxPoolConfig) {
	if ctx.GlobalIsSet(TxPoolLocalsFlag.Name) {
		locals := strings.Split(ctx.GlobalString(TxPoolLocalsFlag.Name), ",")
		for _, account := range locals {
			if trimmed := strings.TrimSpace(account); !common.IsHexAddress(trimmed) {
				Fatalf("Invalid account in --txpool.locals: %s", trimmed)
			} else {
				cfg.Locals = append(cfg.Locals, common.HexToAddress(account))
			}
		}
	}
	if ctx.GlobalIsSet(TxPoolNoLocalsFlag.Name) {
		cfg.NoLocals = ctx.GlobalBool(TxPoolNoLocalsFlag.Name)
	}
	if ctx.GlobalIsSet(TxPoolJournalFlag.Name) {
		cfg.Journal = ctx.GlobalString(TxPoolJournalFlag.Name)
	}
	if ctx.GlobalIsSet(TxPoolRejournalFlag.Name) {
		cfg.Rejournal = ctx.GlobalDuration(TxPoolRejournalFlag.Name)
	}
	if ctx.GlobalIsSet(TxPoolPriceLimitFlag.Name) {
		cfg.PriceLimit = ctx.GlobalUint64(TxPoolPriceLimitFlag.Name)
	}
	if ctx.GlobalIsSet(TxPoolPriceBumpFlag.Name) {
		cfg.PriceBump = ctx.GlobalUint64(TxPoolPriceBumpFlag.Name)
	}
	if ctx.GlobalIsSet(TxPoolAccountSlotsFlag.Name) {
		cfg.AccountSlots = ctx.GlobalUint64(TxPoolAccountSlotsFlag.Name)
	}
	if ctx.GlobalIsSet(TxPoolGlobalSlotsFlag.Name) {
		cfg.GlobalSlots = ctx.GlobalUint64(TxPoolGlobalSlotsFlag.Name)
	}
	if ctx.GlobalIsSet(TxPoolAccountQueueFlag.Name) {
		cfg.AccountQueue = ctx.GlobalUint64(TxPoolAccountQueueFlag.Name)
	}
	if ctx.GlobalIsSet(TxPoolGlobalQueueFlag.Name) {
		cfg.GlobalQueue = ctx.GlobalUint64(TxPoolGlobalQueueFlag.Name)
	}
	if ctx.GlobalIsSet(TxPoolLifetimeFlag.Name) {
		cfg.Lifetime = ctx.GlobalDuration(TxPoolLifetimeFlag.Name)
	}
}

func setEthash(ctx *cli.Context, cfg *ethconfig.Config) {
	if ctx.GlobalIsSet(EthashCacheDirFlag.Name) {
		cfg.Ethash.CacheDir = ctx.GlobalString(EthashCacheDirFlag.Name)
	}
	if ctx.GlobalIsSet(EthashDatasetDirFlag.Name) {
		cfg.Ethash.DatasetDir = ctx.GlobalString(EthashDatasetDirFlag.Name)
	}
	if ctx.GlobalIsSet(EthashCachesInMemoryFlag.Name) {
		cfg.Ethash.CachesInMem = ctx.GlobalInt(EthashCachesInMemoryFlag.Name)
	}
	if ctx.GlobalIsSet(EthashCachesOnDiskFlag.Name) {
		cfg.Ethash.CachesOnDisk = ctx.GlobalInt(EthashCachesOnDiskFlag.Name)
	}
	if ctx.GlobalIsSet(EthashCachesLockMmapFlag.Name) {
		cfg.Ethash.CachesLockMmap = ctx.GlobalBool(EthashCachesLockMmapFlag.Name)
	}
	if ctx.GlobalIsSet(EthashDatasetsInMemoryFlag.Name) {
		cfg.Ethash.DatasetsInMem = ctx.GlobalInt(EthashDatasetsInMemoryFlag.Name)
	}
	if ctx.GlobalIsSet(EthashDatasetsOnDiskFlag.Name) {
		cfg.Ethash.DatasetsOnDisk = ctx.GlobalInt(EthashDatasetsOnDiskFlag.Name)
	}
	if ctx.GlobalIsSet(EthashDatasetsLockMmapFlag.Name) {
		cfg.Ethash.DatasetsLockMmap = ctx.GlobalBool(EthashDatasetsLockMmapFlag.Name)
	}
}

func setMiner(ctx *cli.Context, cfg *miner.Config) {
	if ctx.GlobalIsSet(MinerNotifyFlag.Name) {
		cfg.Notify = strings.Split(ctx.GlobalString(MinerNotifyFlag.Name), ",")
	}
	cfg.NotifyFull = ctx.GlobalBool(MinerNotifyFullFlag.Name)
	if ctx.GlobalIsSet(MinerExtraDataFlag.Name) {
		cfg.ExtraData = []byte(ctx.GlobalString(MinerExtraDataFlag.Name))
	}
	if ctx.GlobalIsSet(MinerGasLimitFlag.Name) {
		cfg.GasCeil = ctx.GlobalUint64(MinerGasLimitFlag.Name)
	}
	if ctx.GlobalIsSet(MinerGasPriceFlag.Name) {
		cfg.GasPrice = GlobalBig(ctx, MinerGasPriceFlag.Name)
	}
	if ctx.GlobalIsSet(MinerRecommitIntervalFlag.Name) {
		cfg.Recommit = ctx.GlobalDuration(MinerRecommitIntervalFlag.Name)
	}
	if ctx.GlobalIsSet(MinerNoVerifyFlag.Name) {
		cfg.Noverify = ctx.GlobalBool(MinerNoVerifyFlag.Name)
	}
	if ctx.GlobalIsSet(LegacyMinerGasTargetFlag.Name) {
		log.Warn("The generic --miner.gastarget flag is deprecated and will be removed in the future!") // 一般的な--miner.gastargetフラグは非推奨であり、将来削除される予定です。
	}
}

func setWhitelist(ctx *cli.Context, cfg *ethconfig.Config) {
	whitelist := ctx.GlobalString(WhitelistFlag.Name)
	if whitelist == "" {
		return
	}
	cfg.Whitelist = make(map[uint64]common.Hash)
	for _, entry := range strings.Split(whitelist, ",") {
		parts := strings.Split(entry, "=")
		if len(parts) != 2 {
			Fatalf("Invalid whitelist entry: %s", entry) // ホワイトリストエントリが無効です
		}
		number, err := strconv.ParseUint(parts[0], 0, 64)
		if err != nil {
			Fatalf("Invalid whitelist block number %s: %v", parts[0], err) // ホワイトリストのブロック番号が無効です
		}
		var hash common.Hash
		if err = hash.UnmarshalText([]byte(parts[1])); err != nil {
			Fatalf("Invalid whitelist hash %s: %v", parts[1], err) // ホワイトリストハッシュが無効です
		}
		cfg.Whitelist[number] = hash
	}
}

// CheckExclusive verifies that only a single instance of the provided flags was
// set by the user. Each flag might optionally be followed by a string type to
// specialize it further.
// CheckExclusiveは、提供されたフラグの単一のインスタンスのみがユーザーによって設定されたことを確認します。
// オプションで、各フラグの後に文字列タイプを続けて、さらに特殊化することができます。
func CheckExclusive(ctx *cli.Context, args ...interface{}) {
	set := make([]string, 0, 1)
	for i := 0; i < len(args); i++ {
		// Make sure the next argument is a flag and skip if not set
		// 次の引数がフラグであることを確認し、設定されていない場合はスキップします
		flag, ok := args[i].(cli.Flag)
		if !ok {
			panic(fmt.Sprintf("invalid argument, not cli.Flag type: %T", args[i]))
		}
		// Check if next arg extends current and expand its name if so
		// 次の引数が現在を拡張するかどうかを確認し、拡張する場合はその名前を拡張します
		name := flag.GetName()

		if i+1 < len(args) {
			switch option := args[i+1].(type) {
			case string:
				// Extended flag check, make sure value set doesn't conflict with passed in option
				// 拡張フラグチェック、値セットが渡されたオプションと競合しないことを確認します
				if ctx.GlobalString(flag.GetName()) == option {
					name += "=" + option
					set = append(set, "--"+name)
				}
				// shift arguments and continue
				// 引数をシフトして続行します
				i++
				continue

			case cli.Flag:
			default:
				panic(fmt.Sprintf("invalid argument, not cli.Flag or string extension: %T", args[i+1]))
			}
		}
		// Mark the flag if it's set
		// 設定されている場合はフラグをマークします
		if ctx.GlobalIsSet(flag.GetName()) {
			set = append(set, "--"+name)
		}
	}
	if len(set) > 1 {
		Fatalf("Flags %v can't be used at the same time", strings.Join(set, ", "))
	}
}

// SetEthConfig applies eth-related command line flags to the config.
// SetEthConfigは、eth関連のコマンドラインフラグを設定に適用します。
func SetEthConfig(ctx *cli.Context, stack *node.Node, cfg *ethconfig.Config) {
	// Avoid conflicting network flags
	// ネットワークフラグの競合を回避する
	CheckExclusive(ctx, MainnetFlag, DeveloperFlag, RopstenFlag, RinkebyFlag, GoerliFlag, SepoliaFlag)
	CheckExclusive(ctx, LightServeFlag, SyncModeFlag, "light")
	CheckExclusive(ctx, DeveloperFlag, ExternalSignerFlag) // Can't use both ephemeral unlocked and external signer // エフェメラルロック解除と外部署名者の両方を使用することはできません
	if ctx.GlobalString(GCModeFlag.Name) == "archive" && ctx.GlobalUint64(TxLookupLimitFlag.Name) != 0 {
		ctx.GlobalSet(TxLookupLimitFlag.Name, "0")
		log.Warn("Disable transaction unindexing for archive node") // アーカイブノードのトランザクションのインデックス解除を無効にする
	}
	if ctx.GlobalIsSet(LightServeFlag.Name) && ctx.GlobalUint64(TxLookupLimitFlag.Name) != 0 {
		log.Warn("LES server cannot serve old transaction status and cannot connect below les/4 protocol version if transaction lookup index is limited") // トランザクションルックアップインデックスが制限されている場合、LESサーバーは古いトランザクションステータスを提供できず、les / 4プロトコルバージョンより前に接続できません
	}
	var ks *keystore.KeyStore
	if keystores := stack.AccountManager().Backends(keystore.KeyStoreType); len(keystores) > 0 {
		ks = keystores[0].(*keystore.KeyStore)
	}
	setEtherbase(ctx, ks, cfg)
	setGPO(ctx, &cfg.GPO, ctx.GlobalString(SyncModeFlag.Name) == "light")
	setTxPool(ctx, &cfg.TxPool)
	setEthash(ctx, cfg)
	setMiner(ctx, &cfg.Miner)
	setWhitelist(ctx, cfg)
	setLes(ctx, cfg)

	// Cap the cache allowance and tune the garbage collector
	// キャッシュ許容量に上限を設定し、ガベージコレクターを調整します
	mem, err := gopsutil.VirtualMemory()
	if err == nil {
		if 32<<(^uintptr(0)>>63) == 32 && mem.Total > 2*1024*1024*1024 {
			log.Warn("Lowering memory allowance on 32bit arch", "available", mem.Total/1024/1024, "addressable", 2*1024)
			mem.Total = 2 * 1024 * 1024 * 1024
		}
		allowance := int(mem.Total / 1024 / 1024 / 3)
		if cache := ctx.GlobalInt(CacheFlag.Name); cache > allowance {
			log.Warn("Sanitizing cache to Go's GC limits", "provided", cache, "updated", allowance)
			ctx.GlobalSet(CacheFlag.Name, strconv.Itoa(allowance))
		}
	}
	// Ensure Go's GC ignores the database cache for trigger percentage
	// GoのGCがトリガーパーセンテージのデータベースキャッシュを無視することを確認します
	cache := ctx.GlobalInt(CacheFlag.Name)
	gogc := math.Max(20, math.Min(100, 100/(float64(cache)/1024)))

	log.Debug("Sanitizing Go's GC trigger", "percent", int(gogc))
	godebug.SetGCPercent(int(gogc))

	if ctx.GlobalIsSet(SyncModeFlag.Name) {
		cfg.SyncMode = *GlobalTextMarshaler(ctx, SyncModeFlag.Name).(*downloader.SyncMode)
	}
	if ctx.GlobalIsSet(NetworkIdFlag.Name) {
		cfg.NetworkId = ctx.GlobalUint64(NetworkIdFlag.Name)
	}
	if ctx.GlobalIsSet(CacheFlag.Name) || ctx.GlobalIsSet(CacheDatabaseFlag.Name) {
		cfg.DatabaseCache = ctx.GlobalInt(CacheFlag.Name) * ctx.GlobalInt(CacheDatabaseFlag.Name) / 100
	}
	cfg.DatabaseHandles = MakeDatabaseHandles()
	if ctx.GlobalIsSet(AncientFlag.Name) {
		cfg.DatabaseFreezer = ctx.GlobalString(AncientFlag.Name)
	}

	if gcmode := ctx.GlobalString(GCModeFlag.Name); gcmode != "full" && gcmode != "archive" {
		Fatalf("--%s must be either 'full' or 'archive'", GCModeFlag.Name)
	}
	if ctx.GlobalIsSet(GCModeFlag.Name) {
		cfg.NoPruning = ctx.GlobalString(GCModeFlag.Name) == "archive"
	}
	if ctx.GlobalIsSet(CacheNoPrefetchFlag.Name) {
		cfg.NoPrefetch = ctx.GlobalBool(CacheNoPrefetchFlag.Name)
	}
	// Read the value from the flag no matter if it's set or not.
	// 設定されているかどうかに関係なく、フラグから値を読み取ります。
	cfg.Preimages = ctx.GlobalBool(CachePreimagesFlag.Name)
	if cfg.NoPruning && !cfg.Preimages {
		cfg.Preimages = true
		log.Info("Enabling recording of key preimages since archive mode is used")
	}
	if ctx.GlobalIsSet(TxLookupLimitFlag.Name) {
		cfg.TxLookupLimit = ctx.GlobalUint64(TxLookupLimitFlag.Name)
	}
	if ctx.GlobalIsSet(CacheFlag.Name) || ctx.GlobalIsSet(CacheTrieFlag.Name) {
		cfg.TrieCleanCache = ctx.GlobalInt(CacheFlag.Name) * ctx.GlobalInt(CacheTrieFlag.Name) / 100
	}
	if ctx.GlobalIsSet(CacheTrieJournalFlag.Name) {
		cfg.TrieCleanCacheJournal = ctx.GlobalString(CacheTrieJournalFlag.Name)
	}
	if ctx.GlobalIsSet(CacheTrieRejournalFlag.Name) {
		cfg.TrieCleanCacheRejournal = ctx.GlobalDuration(CacheTrieRejournalFlag.Name)
	}
	if ctx.GlobalIsSet(CacheFlag.Name) || ctx.GlobalIsSet(CacheGCFlag.Name) {
		cfg.TrieDirtyCache = ctx.GlobalInt(CacheFlag.Name) * ctx.GlobalInt(CacheGCFlag.Name) / 100
	}
	if ctx.GlobalIsSet(CacheFlag.Name) || ctx.GlobalIsSet(CacheSnapshotFlag.Name) {
		cfg.SnapshotCache = ctx.GlobalInt(CacheFlag.Name) * ctx.GlobalInt(CacheSnapshotFlag.Name) / 100
	}
	if !ctx.GlobalBool(SnapshotFlag.Name) {
		// If snap-sync is requested, this flag is also required // snap-syncが要求された場合、このフラグも必要です
		if cfg.SyncMode == downloader.SnapSync {
			log.Info("Snap sync requested, enabling --snapshot") // スナップ同期が要求され、-snapshotが有効になります
		} else {
			cfg.TrieCleanCache += cfg.SnapshotCache
			cfg.SnapshotCache = 0 // Disabled // 無効
		}
	}
	if ctx.GlobalIsSet(DocRootFlag.Name) {
		cfg.DocRoot = ctx.GlobalString(DocRootFlag.Name)
	}
	if ctx.GlobalIsSet(VMEnableDebugFlag.Name) {
		// TODO(fjl): force-enable this in --dev mode
		// TODO（fjl）：-devモードでこれを強制的に有効にします
		cfg.EnablePreimageRecording = ctx.GlobalBool(VMEnableDebugFlag.Name)
	}

	if ctx.GlobalIsSet(RPCGlobalGasCapFlag.Name) {
		cfg.RPCGasCap = ctx.GlobalUint64(RPCGlobalGasCapFlag.Name)
	}
	if cfg.RPCGasCap != 0 {
		log.Info("Set global gas cap", "cap", cfg.RPCGasCap)
	} else {
		log.Info("Global gas cap disabled") // グローバルガスキャップが無効
	}
	if ctx.GlobalIsSet(RPCGlobalEVMTimeoutFlag.Name) {
		cfg.RPCEVMTimeout = ctx.GlobalDuration(RPCGlobalEVMTimeoutFlag.Name)
	}
	if ctx.GlobalIsSet(RPCGlobalTxFeeCapFlag.Name) {
		cfg.RPCTxFeeCap = ctx.GlobalFloat64(RPCGlobalTxFeeCapFlag.Name)
	}
	if ctx.GlobalIsSet(NoDiscoverFlag.Name) {
		cfg.EthDiscoveryURLs, cfg.SnapDiscoveryURLs = []string{}, []string{}
	} else if ctx.GlobalIsSet(DNSDiscoveryFlag.Name) {
		urls := ctx.GlobalString(DNSDiscoveryFlag.Name)
		if urls == "" {
			cfg.EthDiscoveryURLs = []string{}
		} else {
			cfg.EthDiscoveryURLs = SplitAndTrim(urls)
		}
	}
	// Override any default configs for hard coded networks.
	// ハードコードされたネットワークのデフォルト設定を上書きします。
	switch {
	case ctx.GlobalBool(MainnetFlag.Name):
		if !ctx.GlobalIsSet(NetworkIdFlag.Name) {
			cfg.NetworkId = 1
		}
		cfg.Genesis = core.DefaultGenesisBlock()
		SetDNSDiscoveryDefaults(cfg, params.MainnetGenesisHash)
	case ctx.GlobalBool(RopstenFlag.Name):
		if !ctx.GlobalIsSet(NetworkIdFlag.Name) {
			cfg.NetworkId = 3
		}
		cfg.Genesis = core.DefaultRopstenGenesisBlock()
		SetDNSDiscoveryDefaults(cfg, params.RopstenGenesisHash)
	case ctx.GlobalBool(SepoliaFlag.Name):
		if !ctx.GlobalIsSet(NetworkIdFlag.Name) {
			cfg.NetworkId = 11155111
		}
		cfg.Genesis = core.DefaultSepoliaGenesisBlock()
		SetDNSDiscoveryDefaults(cfg, params.SepoliaGenesisHash)
	case ctx.GlobalBool(RinkebyFlag.Name):
		if !ctx.GlobalIsSet(NetworkIdFlag.Name) {
			cfg.NetworkId = 4
		}
		cfg.Genesis = core.DefaultRinkebyGenesisBlock()
		SetDNSDiscoveryDefaults(cfg, params.RinkebyGenesisHash)
	case ctx.GlobalBool(GoerliFlag.Name):
		if !ctx.GlobalIsSet(NetworkIdFlag.Name) {
			cfg.NetworkId = 5
		}
		cfg.Genesis = core.DefaultGoerliGenesisBlock()
		SetDNSDiscoveryDefaults(cfg, params.GoerliGenesisHash)
	case ctx.GlobalBool(DeveloperFlag.Name):
		if !ctx.GlobalIsSet(NetworkIdFlag.Name) {
			cfg.NetworkId = 1337
		}
		cfg.SyncMode = downloader.FullSync
		// Create new developer account or reuse existing one
		// 新しい開発者アカウントを作成するか、既存のアカウントを再利用します
		var (
			developer  accounts.Account
			passphrase string
			err        error
		)
		if list := MakePasswordList(ctx); len(list) > 0 {
			// Just take the first value. Although the function returns a possible multiple values and
			// some usages iterate through them as attempts, that doesn't make sense in this setting,
			// when we're definitely concerned with only one account.
			// 最初の値を取るだけです。
			// 関数は可能な複数の値を返し、いくつかの使用法はそれらを試行として繰り返しますが、
			// 1つのアカウントのみに関係している場合、この設定では意味がありません。
			passphrase = list[0]
		}
		// setEtherbase has been called above, configuring the miner address from command line flags.
		// setEtherbaseは上記で呼び出され、コマンドラインフラグからマイナーアドレスを構成します。
		if cfg.Miner.Etherbase != (common.Address{}) {
			developer = accounts.Account{Address: cfg.Miner.Etherbase}
		} else if accs := ks.Accounts(); len(accs) > 0 {
			developer = ks.Accounts()[0]
		} else {
			developer, err = ks.NewAccount(passphrase)
			if err != nil {
				Fatalf("Failed to create developer account: %v", err)
			}
		}
		if err := ks.Unlock(developer, passphrase); err != nil {
			Fatalf("Failed to unlock developer account: %v", err)
		}
		log.Info("Using developer account", "address", developer.Address)

		// Create a new developer genesis block or reuse existing one
		// 新しい開発者ジェネシスブロックを作成するか、既存のブロックを再利用します
		cfg.Genesis = core.DeveloperGenesisBlock(uint64(ctx.GlobalInt(DeveloperPeriodFlag.Name)), ctx.GlobalUint64(DeveloperGasLimitFlag.Name), developer.Address)
		if ctx.GlobalIsSet(DataDirFlag.Name) {
			// Check if we have an already initialized chain and fall back to
			// that if so. Otherwise we need to generate a new genesis spec.
			// すでに初期化されたチェーンがあるかどうかを確認し、ある場合はそれにフォールバックします。
			// それ以外の場合は、新しいジェネシス仕様を生成する必要があります。
			chaindb := MakeChainDatabase(ctx, stack, false) // TODO (MariusVanDerWijden) make this read only //	TODO（MariusVanDerWijden）はこれを読み取り専用にします
			if rawdb.ReadCanonicalHash(chaindb, 0) != (common.Hash{}) {
				cfg.Genesis = nil // fallback to db content // dbコンテンツへのフォールバック
			}
			chaindb.Close()
		}
		if !ctx.GlobalIsSet(MinerGasPriceFlag.Name) {
			cfg.Miner.GasPrice = big.NewInt(1)
		}
	default:
		if cfg.NetworkId == 1 {
			SetDNSDiscoveryDefaults(cfg, params.MainnetGenesisHash)
		}
	}
}

// SetDNSDiscoveryDefaults configures DNS discovery with the given URL if
// no URLs are set.
// SetDNSDiscoveryDefaultsは、URLが設定されていない場合、指定されたURLでDNS検出を構成します。
func SetDNSDiscoveryDefaults(cfg *ethconfig.Config, genesis common.Hash) {
	if cfg.EthDiscoveryURLs != nil {
		return // already set through flags/config // すでにflags / configで設定済み
	}
	protocol := "all"
	if cfg.SyncMode == downloader.LightSync {
		protocol = "les"
	}
	if url := params.KnownDNSNetwork(genesis, protocol); url != "" {
		cfg.EthDiscoveryURLs = []string{url}
		cfg.SnapDiscoveryURLs = cfg.EthDiscoveryURLs
	}
}

// RegisterEthService adds an Ethereum client to the stack.
// The second return value is the full node instance, which may be nil if the
// node is running as a light client.
// RegisterEthServiceは、Ethereumクライアントをスタックに追加します。
// 2番目の戻り値は完全なノードインスタンスであり、
// ノードがライトクライアントとして実行されている場合はnilになる可能性があります。
func RegisterEthService(stack *node.Node, cfg *ethconfig.Config, isCatalyst bool) (ethapi.Backend, *eth.Ethereum) {
	if cfg.SyncMode == downloader.LightSync {
		backend, err := les.New(stack, cfg)
		if err != nil {
			Fatalf("Failed to register the Ethereum service: %v", err)
		}
		stack.RegisterAPIs(tracers.APIs(backend.ApiBackend))
		if isCatalyst {
			if err := catalyst.RegisterLight(stack, backend); err != nil {
				Fatalf("Failed to register the catalyst service: %v", err)
			}
		}
		return backend.ApiBackend, nil
	}
	backend, err := eth.New(stack, cfg)
	if err != nil {
		Fatalf("Failed to register the Ethereum service: %v", err)
	}
	if cfg.LightServ > 0 {
		_, err := les.NewLesServer(stack, backend, cfg)
		if err != nil {
			Fatalf("Failed to create the LES server: %v", err)
		}
	}
	if isCatalyst {
		if err := catalyst.Register(stack, backend); err != nil {
			Fatalf("Failed to register the catalyst service: %v", err)
		}
	}
	stack.RegisterAPIs(tracers.APIs(backend.APIBackend))
	return backend.APIBackend, backend
}

// RegisterEthStatsService configures the Ethereum Stats daemon and adds it to
// the given node.
// RegisterEthStatsServiceは、Ethereum Statsデーモンを構成し、それを指定されたノードに追加します。
func RegisterEthStatsService(stack *node.Node, backend ethapi.Backend, url string) {
	if err := ethstats.New(stack, backend, backend.Engine(), url); err != nil {
		Fatalf("Failed to register the Ethereum Stats service: %v", err)
	}
}

// RegisterGraphQLService is a utility function to construct a new service and register it against a node.
// RegisterGraphQLServiceは、新しいサービスを構築し、それをノードに対して登録するためのユーティリティ関数です。
func RegisterGraphQLService(stack *node.Node, backend ethapi.Backend, cfg node.Config) {
	if err := graphql.New(stack, backend, cfg.GraphQLCors, cfg.GraphQLVirtualHosts); err != nil {
		Fatalf("Failed to register the GraphQL service: %v", err)
	}
}

func SetupMetrics(ctx *cli.Context) {
	if metrics.Enabled {
		log.Info("Enabling metrics collection")

		var (
			enableExport   = ctx.GlobalBool(MetricsEnableInfluxDBFlag.Name)
			enableExportV2 = ctx.GlobalBool(MetricsEnableInfluxDBV2Flag.Name)
		)

		if enableExport || enableExportV2 {
			CheckExclusive(ctx, MetricsEnableInfluxDBFlag, MetricsEnableInfluxDBV2Flag)

			v1FlagIsSet := ctx.GlobalIsSet(MetricsInfluxDBUsernameFlag.Name) ||
				ctx.GlobalIsSet(MetricsInfluxDBPasswordFlag.Name)

			v2FlagIsSet := ctx.GlobalIsSet(MetricsInfluxDBTokenFlag.Name) ||
				ctx.GlobalIsSet(MetricsInfluxDBOrganizationFlag.Name) ||
				ctx.GlobalIsSet(MetricsInfluxDBBucketFlag.Name)

			if enableExport && v2FlagIsSet {
				Fatalf("Flags --influxdb.metrics.organization, --influxdb.metrics.token, --influxdb.metrics.bucket are only available for influxdb-v2")
			} else if enableExportV2 && v1FlagIsSet {
				Fatalf("Flags --influxdb.metrics.username, --influxdb.metrics.password are only available for influxdb-v1")
			}
		}

		var (
			endpoint = ctx.GlobalString(MetricsInfluxDBEndpointFlag.Name)
			database = ctx.GlobalString(MetricsInfluxDBDatabaseFlag.Name)
			username = ctx.GlobalString(MetricsInfluxDBUsernameFlag.Name)
			password = ctx.GlobalString(MetricsInfluxDBPasswordFlag.Name)

			token        = ctx.GlobalString(MetricsInfluxDBTokenFlag.Name)
			bucket       = ctx.GlobalString(MetricsInfluxDBBucketFlag.Name)
			organization = ctx.GlobalString(MetricsInfluxDBOrganizationFlag.Name)
		)

		if enableExport {
			tagsMap := SplitTagsFlag(ctx.GlobalString(MetricsInfluxDBTagsFlag.Name))

			log.Info("Enabling metrics export to InfluxDB")

			go influxdb.InfluxDBWithTags(metrics.DefaultRegistry, 10*time.Second, endpoint, database, username, password, "geth.", tagsMap)
		} else if enableExportV2 {
			tagsMap := SplitTagsFlag(ctx.GlobalString(MetricsInfluxDBTagsFlag.Name))

			log.Info("Enabling metrics export to InfluxDB (v2)")

			go influxdb.InfluxDBV2WithTags(metrics.DefaultRegistry, 10*time.Second, endpoint, token, bucket, organization, "geth.", tagsMap)
		}

		if ctx.GlobalIsSet(MetricsHTTPFlag.Name) {
			address := fmt.Sprintf("%s:%d", ctx.GlobalString(MetricsHTTPFlag.Name), ctx.GlobalInt(MetricsPortFlag.Name))
			log.Info("Enabling stand-alone metrics HTTP endpoint", "address", address)
			exp.Setup(address)
		}
	}
}

func SplitTagsFlag(tagsFlag string) map[string]string {
	tags := strings.Split(tagsFlag, ",")
	tagsMap := map[string]string{}

	for _, t := range tags {
		if t != "" {
			kv := strings.Split(t, "=")

			if len(kv) == 2 {
				tagsMap[kv[0]] = kv[1]
			}
		}
	}

	return tagsMap
}

// MakeChainDatabase open an LevelDB using the flags passed to the client and will hard crash if it fails.
// MakeChainDatabaseは、クライアントに渡されたフラグを使用してLevelDBを開き、失敗するとハードクラッシュします。
func MakeChainDatabase(ctx *cli.Context, stack *node.Node, readonly bool) ethdb.Database {
	var (
		cache   = ctx.GlobalInt(CacheFlag.Name) * ctx.GlobalInt(CacheDatabaseFlag.Name) / 100
		handles = MakeDatabaseHandles()

		err     error
		chainDb ethdb.Database
	)
	if ctx.GlobalString(SyncModeFlag.Name) == "light" {
		name := "lightchaindata"
		chainDb, err = stack.OpenDatabase(name, cache, handles, "", readonly)
	} else {
		name := "chaindata"
		chainDb, err = stack.OpenDatabaseWithFreezer(name, cache, handles, ctx.GlobalString(AncientFlag.Name), "", readonly)
	}
	if err != nil {
		Fatalf("Could not open database: %v", err)
	}
	return chainDb
}

func MakeGenesis(ctx *cli.Context) *core.Genesis {
	var genesis *core.Genesis
	switch {
	case ctx.GlobalBool(MainnetFlag.Name):
		genesis = core.DefaultGenesisBlock()
	case ctx.GlobalBool(RopstenFlag.Name):
		genesis = core.DefaultRopstenGenesisBlock()
	case ctx.GlobalBool(SepoliaFlag.Name):
		genesis = core.DefaultSepoliaGenesisBlock()
	case ctx.GlobalBool(RinkebyFlag.Name):
		genesis = core.DefaultRinkebyGenesisBlock()
	case ctx.GlobalBool(GoerliFlag.Name):
		genesis = core.DefaultGoerliGenesisBlock()
	case ctx.GlobalBool(DeveloperFlag.Name):
		Fatalf("Developer chains are ephemeral")
	}
	return genesis
}

// MakeChain creates a chain manager from set command line flags.
// MakeChainは、設定されたコマンドラインフラグからチェーンマネージャーを作成します。
func MakeChain(ctx *cli.Context, stack *node.Node) (chain *core.BlockChain, chainDb ethdb.Database) {
	var err error
	chainDb = MakeChainDatabase(ctx, stack, false) // TODO(rjl493456442) support read-only database // TODO（rjl493456442）は読み取り専用データベースをサポートします
	config, _, err := core.SetupGenesisBlock(chainDb, MakeGenesis(ctx))
	if err != nil {
		Fatalf("%v", err)
	}
	var engine consensus.Engine
	if config.Clique != nil {
		engine = clique.New(config.Clique, chainDb)
	} else {
		engine = ethash.NewFaker()
		if !ctx.GlobalBool(FakePoWFlag.Name) {
			engine = ethash.New(ethash.Config{
				CacheDir:         stack.ResolvePath(ethconfig.Defaults.Ethash.CacheDir),
				CachesInMem:      ethconfig.Defaults.Ethash.CachesInMem,
				CachesOnDisk:     ethconfig.Defaults.Ethash.CachesOnDisk,
				CachesLockMmap:   ethconfig.Defaults.Ethash.CachesLockMmap,
				DatasetDir:       stack.ResolvePath(ethconfig.Defaults.Ethash.DatasetDir),
				DatasetsInMem:    ethconfig.Defaults.Ethash.DatasetsInMem,
				DatasetsOnDisk:   ethconfig.Defaults.Ethash.DatasetsOnDisk,
				DatasetsLockMmap: ethconfig.Defaults.Ethash.DatasetsLockMmap,
			}, nil, false)
		}
	}
	if gcmode := ctx.GlobalString(GCModeFlag.Name); gcmode != "full" && gcmode != "archive" {
		Fatalf("--%s must be either 'full' or 'archive'", GCModeFlag.Name)
	}
	cache := &core.CacheConfig{
		TrieCleanLimit:      ethconfig.Defaults.TrieCleanCache,
		TrieCleanNoPrefetch: ctx.GlobalBool(CacheNoPrefetchFlag.Name),
		TrieDirtyLimit:      ethconfig.Defaults.TrieDirtyCache,
		TrieDirtyDisabled:   ctx.GlobalString(GCModeFlag.Name) == "archive",
		TrieTimeLimit:       ethconfig.Defaults.TrieTimeout,
		SnapshotLimit:       ethconfig.Defaults.SnapshotCache,
		Preimages:           ctx.GlobalBool(CachePreimagesFlag.Name),
	}
	if cache.TrieDirtyDisabled && !cache.Preimages {
		cache.Preimages = true
		log.Info("Enabling recording of key preimages since archive mode is used")
	}
	if !ctx.GlobalBool(SnapshotFlag.Name) {
		cache.SnapshotLimit = 0 // Disabled //無効
	}
	if ctx.GlobalIsSet(CacheFlag.Name) || ctx.GlobalIsSet(CacheTrieFlag.Name) {
		cache.TrieCleanLimit = ctx.GlobalInt(CacheFlag.Name) * ctx.GlobalInt(CacheTrieFlag.Name) / 100
	}
	if ctx.GlobalIsSet(CacheFlag.Name) || ctx.GlobalIsSet(CacheGCFlag.Name) {
		cache.TrieDirtyLimit = ctx.GlobalInt(CacheFlag.Name) * ctx.GlobalInt(CacheGCFlag.Name) / 100
	}
	vmcfg := vm.Config{EnablePreimageRecording: ctx.GlobalBool(VMEnableDebugFlag.Name)}

	// TODO(rjl493456442) disable snapshot generation/wiping if the chain is read only.
	// Disable transaction indexing/unindexing by default.
	// TODO（rjl493456442）は、チェーンが読み取り専用の場合、スナップショットの生成/ワイプを無効にします。
	//デフォルトでトランザクションのインデックス作成/インデックス解除を無効にします。
	chain, err = core.NewBlockChain(chainDb, cache, config, engine, vmcfg, nil, nil)
	if err != nil {
		Fatalf("Can't create BlockChain: %v", err)
	}
	return chain, chainDb
}

// MakeConsolePreloads retrieves the absolute paths for the console JavaScript
// scripts to preload before starting.
// MakeConsolePreloadsは、開始する前にプリロードするコンソールJavaScriptスクリプトの絶対パスを取得します。
func MakeConsolePreloads(ctx *cli.Context) []string {
	// Skip preloading if there's nothing to preload
	// プリロードするものがない場合は、プリロードをスキップします
	if ctx.GlobalString(PreloadJSFlag.Name) == "" {
		return nil
	}
	// Otherwise resolve absolute paths and return them
	// それ以外の場合は、絶対パスを解決して返します
	var preloads []string

	for _, file := range strings.Split(ctx.GlobalString(PreloadJSFlag.Name), ",") {
		preloads = append(preloads, strings.TrimSpace(file))
	}
	return preloads
}

// MigrateFlags sets the global flag from a local flag when it's set.
// This is a temporary function used for migrating old command/flags to the
// new format.
//
// e.g. geth account new --keystore /tmp/mykeystore --lightkdf
//
// is equivalent after calling this method with:
//
// geth --keystore /tmp/mykeystore --lightkdf account new
//
// This allows the use of the existing configuration functionality.
// When all flags are migrated this function can be removed and the existing
// configuration functionality must be changed that is uses local flags

// MigrateFlagsは、設定時にローカルフラグからグローバルフラグを設定します。
// これは、古いコマンド/フラグを新しい形式に移行するために使用される一時的な関数です。
//
// 例： geth account new --keystore / tmp / mykeystore --lightkdf
//
// このメソッドを次のように呼び出した後は同等です：
//
//  geth --keystore / tmp / mykeystore --lightkdf account new
//
// これにより、既存の構成機能を使用できるようになります。
// すべてのフラグが移行されたら、この関数を削除でき、
// ローカルフラグを使用する既存の構成機能を変更する必要があります
func MigrateFlags(action func(ctx *cli.Context) error) func(*cli.Context) error {
	return func(ctx *cli.Context) error {
		for _, name := range ctx.FlagNames() {
			if ctx.IsSet(name) {
				ctx.GlobalSet(name, ctx.String(name))
			}
		}
		return action(ctx)
	}
}
