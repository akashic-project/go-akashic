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

package node

import (
	"crypto/ecdsa"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	datadirPrivateKey      = "nodekey"            // データディレクトリ内のノードの秘密鍵へのパス          // Path within the datadir to the node's private key
	datadirDefaultKeyStore = "keystore"           // データディレクトリ内のキーストアへのパス              // Path within the datadir to the keystore
	datadirStaticNodes     = "static-nodes.json"  // データディレクトリ内の静的ノードリストへのパス         // Path within the datadir to the static node list
	datadirTrustedNodes    = "trusted-nodes.json" // データディレクトリ内の信頼できるノードリストへのパス    // Path within the datadir to the trusted node list
	datadirNodeDatabase    = "nodes"              //ノード情報を保存するためのdatadir内のパス              // Path within the datadir to store the node infos
)

// Config represents a small collection of configuration values to fine tune the
// P2P network layer of a protocol stack. These values can be further extended by
// all registered services.
// Configは、プロトコルスタックのP2Pネットワーク層を微調整するための構成値の小さなコレクションを表します。
// これらの値は、登録されているすべてのサービスによってさらに拡張できます。
type Config struct {
	// Name sets the instance name of the node. It must not contain the / character and is
	// used in the devp2p node identifier. The instance name of geth is "goshic". If no
	// value is specified, the basename of the current executable is used.
	// Nameは、ノードのインスタンス名を設定します。 /文字を含めることはできず、devp2pノード識別子で使用されます。
	// gethのインスタンス名は「geth」です。
	// 値が指定されていない場合は、現在の実行可能ファイルのベース名が使用されます。
	Name string `toml:"-"`

	// UserIdent, if set, is used as an additional component in the devp2p node identifier.
	// UserIdentが設定されている場合、devp2pノード識別子の追加コンポーネントとして使用されます。
	UserIdent string `toml:",omitempty"`

	// Version should be set to the version number of the program. It is used
	// in the devp2p node identifier.
	// バージョンはプログラムのバージョン番号に設定する必要があります。
	// devp2pノード識別子で使用されます。
	Version string `toml:"-"`

	// DataDir is the file system folder the node should use for any data storage
	// requirements. The configured data directory will not be directly shared with
	// registered services, instead those can use utility methods to create/access
	// databases or flat files. This enables ephemeral nodes which can fully reside
	// in memory.
	// DataDirは、ノードがデータストレージ要件に使用する必要のあるファイルシステムフォルダです。
	// 構成されたデータディレクトリは、登録されたサービスと直接共有されません。
	// 代わりに、ユーティリティメソッドを使用してデータベースまたはフラットファイルを作成/アクセスできます。
	// これにより、メモリに完全に常駐できるエフェメラルノードが有効になります。
	DataDir string

	// Configuration of peer-to-peer networking.
	// ピアツーピアネットワークの構成。
	P2P p2p.Config

	// KeyStoreDir is the file system folder that contains private keys. The directory can
	// be specified as a relative path, in which case it is resolved relative to the
	// current directory.
	//
	// If KeyStoreDir is empty, the default location is the "keystore" subdirectory of
	// DataDir. If DataDir is unspecified and KeyStoreDir is empty, an ephemeral directory
	// is created by New and destroyed when the node is stopped.
	// KeyStoreDirは、秘密鍵を含むファイルシステムフォルダです。ディレクトリは相対パスとして指定できます。
	// その場合、現在のディレクトリを基準にしてディレクトリが解決されます。
	//
	// KeyStoreDirが空の場合、デフォルトの場所はDataDirの「keystore」サブディレクトリです。
	// DataDirが指定されておらず、KeyStoreDirが空の場合、エフェメラルディレクトリがNewによって作成され、ノードが停止すると破棄されます。
	KeyStoreDir string `toml:",omitempty"`

	// ExternalSigner specifies an external URI for a clef-type signer
	// ExternalSignerは、音部記号タイプの署名者の外部URIを指定します
	ExternalSigner string `toml:",omitempty"`

	// UseLightweightKDF lowers the memory and CPU requirements of the key store
	// scrypt KDF at the expense of security.
	// UseLightweightKDFは、セキュリティを犠牲にして、キーストア暗号化KDFのメモリとCPUの要件を下げます。
	UseLightweightKDF bool `toml:",omitempty"`

	// InsecureUnlockAllowed allows user to unlock accounts in unsafe http environment.
	// InsecureUnlockAllowedを使用すると、ユーザーは安全でないhttp環境でアカウントのロックを解除できます。
	InsecureUnlockAllowed bool `toml:",omitempty"`

	// NoUSB disables hardware wallet monitoring and connectivity.
	// Deprecated: USB monitoring is disabled by default and must be enabled explicitly.
	// NoUSBは、ハードウェアウォレットの監視と接続を無効にします。
	// 非推奨：USB監視はデフォルトで無効になっているため、明示的に有効にする必要があります。
	NoUSB bool `toml:",omitempty"`

	// USB enables hardware wallet monitoring and connectivity.
	// USBは、ハードウェアウォレットの監視と接続を可能にします。
	USB bool `toml:",omitempty"`

	// SmartCardDaemonPath is the path to the smartcard daemon's socket
	// SmartCardDaemonPathは、スマートカードデーモンのソケットへのパスです
	SmartCardDaemonPath string `toml:",omitempty"`

	// IPCPath is the requested location to place the IPC endpoint. If the path is
	// a simple file name, it is placed inside the data directory (or on the root
	// pipe path on Windows), whereas if it's a resolvable path name (absolute or
	// relative), then that specific path is enforced. An empty path disables IPC.
	// IPCPathは、IPCエンドポイントを配置するために要求された場所です。
	// パスが単純なファイル名の場合、データディレクトリ内（またはWindowsのルートパイプパス）に配置されますが、
	// 解決可能なパス名（絶対パスまたは相対パス）の場合、その特定のパスが適用されます。
	// 空のパスはIPCを無効にします。
	IPCPath string

	// HTTPHost is the host interface on which to start the HTTP RPC server. If this
	// field is empty, no HTTP API endpoint will be started.
	// HTTPHostは、HTTPRPCサーバーを起動するホストインターフェイスです。
	// このフィールドが空の場合、HTTPAPIエンドポイントは開始されません。
	HTTPHost string

	// HTTPPort is the TCP port number on which to start the HTTP RPC server. The
	// default zero value is/ valid and will pick a port number randomly (useful
	// for ephemeral nodes).
	// HTTPPortは、HTTPRPCサーバーを起動するTCPポート番号です。
	// デフォルトのゼロ値は/有効であり、ポート番号をランダムに選択します（エフェメラルノードに役立ちます）。
	HTTPPort int `toml:",omitempty"`

	// HTTPCors is the Cross-Origin Resource Sharing header to send to requesting
	// clients. Please be aware that CORS is a browser enforced security, it's fully
	// useless for custom HTTP clients.
	// HTTPCorsは、リクエスト元のクライアントに送信するクロスオリジンリソースシェアリングヘッダーです。
	// CORSはブラウザで強制されるセキュリティであり、カスタムHTTPクライアントにはまったく役に立たないことに注意してください。
	HTTPCors []string `toml:",omitempty"`

	// HTTPVirtualHosts is the list of virtual hostnames which are allowed on incoming requests.
	// This is by default {'localhost'}. Using this prevents attacks like
	// DNS rebinding, which bypasses SOP by simply masquerading as being within the same
	// origin. These attacks do not utilize CORS, since they are not cross-domain.
	// By explicitly checking the Host-header, the server will not allow requests
	// made against the server with a malicious host domain.
	// Requests using ip address directly are not affected
	// HTTPVirtualHostsは、着信要求で許可される仮想ホスト名のリストです。
	// これはデフォルトで{'localhost'}です。これを使用すると、
	// 同じオリジン内にあると偽装するだけでSOPをバイパスするDNS再バインドなどの攻撃を防ぐことができます。
	// これらの攻撃はクロスドメインではないため、CORSを利用しません。
	// ホストヘッダーを明示的にチェックすることにより、サーバーは悪意のあるホストドメインを持つサーバーに対して行われたリクエストを許可しません。
	// IPアドレスを直接使用するリクエストは影響を受けません
	HTTPVirtualHosts []string `toml:",omitempty"`

	// HTTPModules is a list of API modules to expose via the HTTP RPC interface.
	// If the module list is empty, all RPC API endpoints designated public will be
	// exposed.
	// HTTPModulesは、HTTPRPCインターフェースを介して公開するAPIモジュールのリストです。
	//モジュールリストが空の場合、publicと指定されたすべてのRPCAPIエンドポイントが公開されます。
	HTTPModules []string

	// HTTPTimeouts allows for customization of the timeout values used by the HTTP RPC
	// interface.
	// HTTPTimeoutsを使用すると、HTTPRPCインターフェイスで使用されるタイムアウト値をカスタマイズできます。
	HTTPTimeouts rpc.HTTPTimeouts

	// HTTPPathPrefix specifies a path prefix on which http-rpc is to be served.
	// HTTPPathPrefixは、http-rpcが提供されるパスプレフィックスを指定します。
	HTTPPathPrefix string `toml:",omitempty"`

	// WSHost is the host interface on which to start the websocket RPC server. If
	// this field is empty, no websocket API endpoint will be started.
	// WSHostは、WebSocketRPCサーバーを起動するためのホストインターフェイスです。
	// このフィールドが空の場合、WebSocketAPIエンドポイントは開始されません。
	WSHost string

	// WSPort is the TCP port number on which to start the websocket RPC server. The
	// default zero value is/ valid and will pick a port number randomly (useful for
	// ephemeral nodes).
	// WSPortは、WebSocketRPCサーバーを起動するTCPポート番号です。
	// デフォルトのゼロ値は/有効であり、ポート番号をランダムに選択します（エフェメラルノードに役立ちます）。
	WSPort int `toml:",omitempty"`

	// WSPathPrefix specifies a path prefix on which ws-rpc is to be served.
	// WSPathPrefixは、ws-rpcが提供されるパスプレフィックスを指定します。
	WSPathPrefix string `toml:",omitempty"`

	// WSOrigins is the list of domain to accept websocket requests from. Please be
	// aware that the server can only act upon the HTTP request the client sends and
	// cannot verify the validity of the request header.
	// WSOriginsは、WebSocketリクエストを受け入れるドメインのリストです。
	// サーバーは、クライアントが送信したHTTPリクエストに対してのみ機能し、
	// リクエストヘッダーの有効性を確認できないことに注意してください。
	WSOrigins []string `toml:",omitempty"`

	// WSModules is a list of API modules to expose via the websocket RPC interface.
	// If the module list is empty, all RPC API endpoints designated public will be
	// exposed.
	// WSModulesは、WebSocketRPCインターフェースを介して公開するAPIモジュールのリストです。
	//モジュールリストが空の場合、publicと指定されたすべてのRPCAPIエンドポイントが公開されます。
	WSModules []string

	// WSExposeAll exposes all API modules via the WebSocket RPC interface rather
	// than just the public ones.
	//
	// *WARNING* Only set this if the node is running in a trusted network, exposing
	// private APIs to untrusted users is a major security risk.
	// WSExposeAllは、パブリックモジュールだけでなく、WebSocketRPCインターフェイスを介してすべてのAPIモジュールを公開します。
	//
	// *警告*ノードが信頼できるネットワークで実行されている場合にのみこれを設定します。
	// 信頼できないユーザーにプライベートAPIを公開することは、主要なセキュリティリスクです。
	WSExposeAll bool `toml:",omitempty"`

	// GraphQLCors is the Cross-Origin Resource Sharing header to send to requesting
	// clients. Please be aware that CORS is a browser enforced security, it's fully
	// useless for custom HTTP clients.
	// GraphQLCorsは、リクエスト元のクライアントに送信するクロスオリジンリソースシェアリングヘッダーです。
	// CORSはブラウザで強制されるセキュリティであり、カスタムHTTPクライアントにはまったく役に立たないことに注意してください。
	GraphQLCors []string `toml:",omitempty"`

	// GraphQLVirtualHosts is the list of virtual hostnames which are allowed on incoming requests.
	// This is by default {'localhost'}. Using this prevents attacks like
	// DNS rebinding, which bypasses SOP by simply masquerading as being within the same
	// origin. These attacks do not utilize CORS, since they are not cross-domain.
	// By explicitly checking the Host-header, the server will not allow requests
	// made against the server with a malicious host domain.
	// Requests using ip address directly are not affected
	// GraphQLVirtualHostsは、着信要求で許可される仮想ホスト名のリストです。
	// これはデフォルトで{'localhost'}です。これを使用すると、
	// 同じオリジン内にあると偽装するだけでSOPをバイパスするDNS再バインドなどの攻撃を防ぐことができます。
	// これらの攻撃はクロスドメインではないため、CORSを利用しません。
	// ホストヘッダーを明示的にチェックすることにより、サーバーは悪意のあるホストドメインを持つサーバーに対して行われたリクエストを許可しません。
	// IPアドレスを直接使用するリクエストは影響を受けません
	GraphQLVirtualHosts []string `toml:",omitempty"`

	// Logger is a custom logger to use with the p2p.Server.
	// Loggerは、p2p.Serverで使用するカスタムロガーです。
	Logger log.Logger `toml:",omitempty"`

	staticNodesWarning     bool
	trustedNodesWarning    bool
	oldGethResourceWarning bool

	// AllowUnprotectedTxs allows non EIP-155 protected transactions to be send over RPC.
	// AllowUnprotectedTxsを使用すると、EIP-155で保護されていないトランザクションをRPC経由で送信できます。
	AllowUnprotectedTxs bool `toml:",omitempty"`
}

// IPCEndpoint resolves an IPC endpoint based on a configured value, taking into
// account the set data folders as well as the designated platform we're currently
// running on.
// IPCEndpointは、設定されたデータフォルダーと、
// 現在実行している指定されたプラットフォームを考慮して、構成された値に基づいてIPCエンドポイントを解決します。
func (c *Config) IPCEndpoint() string {
	// Short circuit if IPC has not been enabled
	// IPCが有効になっていない場合は短絡
	if c.IPCPath == "" {
		return ""
	}
	// On windows we can only use plain top-level pipes
	// Windowsでは、プレーンなトップレベルのパイプのみを使用できます
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(c.IPCPath, `\\.\pipe\`) {
			return c.IPCPath
		}
		return `\\.\pipe\` + c.IPCPath
	}
	// Resolve names into the data directory full paths otherwise
	// 名前をデータディレクトリのフルパスに解決します。それ以外の場合
	if filepath.Base(c.IPCPath) == c.IPCPath {
		if c.DataDir == "" {
			return filepath.Join(os.TempDir(), c.IPCPath)
		}
		return filepath.Join(c.DataDir, c.IPCPath)
	}
	return c.IPCPath
}

// NodeDB returns the path to the discovery node database.
// NodeDBは、検出ノードデータベースへのパスを返します。
func (c *Config) NodeDB() string {
	if c.DataDir == "" {
		return "" // 一時的 // ephemeral
	}
	return c.ResolvePath(datadirNodeDatabase)
}

// DefaultIPCEndpoint returns the IPC path used by default.
// DefaultIPCEndpointは、デフォルトで使用されるIPCパスを返します。
func DefaultIPCEndpoint(clientIdentifier string) string {
	if clientIdentifier == "" {
		clientIdentifier = strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
		if clientIdentifier == "" {
			panic("empty executable name")
		}
	}
	config := &Config{DataDir: DefaultDataDir(), IPCPath: clientIdentifier + ".ipc"}
	return config.IPCEndpoint()
}

// HTTPEndpoint resolves an HTTP endpoint based on the configured host interface
// and port parameters.
// HTTPEndpointは、構成されたホストインターフェイスとポートパラメータに基づいてHTTPエンドポイントを解決します。
func (c *Config) HTTPEndpoint() string {
	if c.HTTPHost == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", c.HTTPHost, c.HTTPPort)
}

// DefaultHTTPEndpoint returns the HTTP endpoint used by default.
// DefaultHTTPEndpointは、デフォルトで使用されるHTTPエンドポイントを返します。
func DefaultHTTPEndpoint() string {
	config := &Config{HTTPHost: DefaultHTTPHost, HTTPPort: DefaultHTTPPort}
	return config.HTTPEndpoint()
}

// WSEndpoint resolves a websocket endpoint based on the configured host interface
// and port parameters.
// WSEndpointは、構成されたホストインターフェイスとポートパラメーターに基づいてWebSocketエンドポイントを解決します。
func (c *Config) WSEndpoint() string {
	if c.WSHost == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", c.WSHost, c.WSPort)
}

// DefaultWSEndpoint returns the websocket endpoint used by default.
// DefaultWSEndpointは、デフォルトで使用されるWebSocketエンドポイントを返します。
func DefaultWSEndpoint() string {
	config := &Config{WSHost: DefaultWSHost, WSPort: DefaultWSPort}
	return config.WSEndpoint()
}

// ExtRPCEnabled returns the indicator whether node enables the external
// RPC(http, ws or graphql).
// ExtRPCEnabledは、ノードが外部RPC（http、ws、またはgraphql）を有効にするかどうかのインジケーターを返します。
func (c *Config) ExtRPCEnabled() bool {
	return c.HTTPHost != "" || c.WSHost != ""
}

// NodeName returns the devp2p node identifier.
// NodeNameはdevp2pノード識別子を返します。
func (c *Config) NodeName() string {
	name := c.name()
	// Backwards compatibility: previous versions used title-cased "Goshic", keep that.
	// 下位互換性：以前のバージョンではタイトルケースの「Geth」が使用されていましたが、それを維持してください。
	if name == "goshic" || name == "goshic-testnet" {
		name = "Goshic"
	}
	if c.UserIdent != "" {
		name += "/" + c.UserIdent
	}
	if c.Version != "" {
		name += "/v" + c.Version
	}
	name += "/" + runtime.GOOS + "-" + runtime.GOARCH
	name += "/" + runtime.Version()
	return name
}

func (c *Config) name() string {
	if c.Name == "" {
		progname := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
		if progname == "" {
			panic("empty executable name, set Config.Name")
		}
		return progname
	}
	return c.Name
}

// These resources are resolved differently for "goshic" instances.
// これらのリソースは、「geth」インスタンスに対して異なる方法で解決されます。
var isOldGethResource = map[string]bool{
	"chaindata":          true,
	"nodes":              true,
	"nodekey":            true,
	"static-nodes.json":  false, // これらには警告がないため、警告はありません // no warning for these because they have their
	"trusted-nodes.json": false, // 個別の警告を所有します。 // own separate warning.
}

// ResolvePath resolves path in the instance directory.
// ResolvePathは、インスタンスディレクトリ内のパスを解決します。
func (c *Config) ResolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if c.DataDir == "" {
		return ""
	}
	// Backwards-compatibility: ensure that data directory files created
	// by geth 1.4 are used if they exist.
	// 下位互換性：geth1.4によって作成されたデータディレクトリファイルが存在する場合はそれが使用されるようにします。
	if warn, isOld := isOldGethResource[path]; isOld {
		oldpath := ""
		if c.name() == "goshic" {
			oldpath = filepath.Join(c.DataDir, path)
		}
		if oldpath != "" && common.FileExist(oldpath) {
			if warn {
				c.warnOnce(&c.oldGethResourceWarning, "Using deprecated resource file %s, please move this file to the 'geth' subdirectory of datadir.", oldpath)
			}
			return oldpath
		}
	}
	return filepath.Join(c.instanceDir(), path)
}

func (c *Config) instanceDir() string {
	if c.DataDir == "" {
		return ""
	}
	return filepath.Join(c.DataDir, c.name())
}

// NodeKey retrieves the currently configured private key of the node, checking
// first any manually set key, falling back to the one found in the configured
// data folder. If no key can be found, a new one is generated.

// NodeKeyは、ノードの現在構成されている秘密鍵を取得し、最初に手動で設定された鍵をチェックし、
// 構成されたデータフォルダーにある鍵にフォールバックします。
// キーが見つからない場合は、新しいキーが生成されます。
func (c *Config) NodeKey() *ecdsa.PrivateKey {
	// Use any specifically configured key.
	// 特別に構成されたキーを使用します。
	if c.P2P.PrivateKey != nil {
		return c.P2P.PrivateKey
	}
	// Generate ephemeral key if no datadir is being used.
	// datadirが使用されていない場合は、エフェメラルキーを生成します。
	if c.DataDir == "" {
		key, err := crypto.GenerateKey()
		if err != nil {
			log.Crit(fmt.Sprintf("Failed to generate ephemeral node key: %v", err))
		}
		return key
	}

	keyfile := c.ResolvePath(datadirPrivateKey)
	if key, err := crypto.LoadECDSA(keyfile); err == nil {
		return key
	}
	// No persistent key found, generate and store a new one.
	// 永続キーが見つかりません。新しいキーを生成して保存します。
	key, err := crypto.GenerateKey()
	if err != nil {
		log.Crit(fmt.Sprintf("Failed to generate node key: %v", err))
	}
	instanceDir := filepath.Join(c.DataDir, c.name())
	if err := os.MkdirAll(instanceDir, 0700); err != nil {
		log.Error(fmt.Sprintf("Failed to persist node key: %v", err))
		return key
	}
	keyfile = filepath.Join(instanceDir, datadirPrivateKey)
	if err := crypto.SaveECDSA(keyfile, key); err != nil {
		log.Error(fmt.Sprintf("Failed to persist node key: %v", err))
	}
	return key
}

// StaticNodes returns a list of node enode URLs configured as static nodes.
// StaticNodesは、静的ノードとして構成されたノードenodeURLのリストを返します。
func (c *Config) StaticNodes() []*enode.Node {
	return c.parsePersistentNodes(&c.staticNodesWarning, c.ResolvePath(datadirStaticNodes))
}

// TrustedNodes returns a list of node enode URLs configured as trusted nodes.
// TrustedNodesは、信頼できるノードとして構成されたノードenodeURLのリストを返します。
func (c *Config) TrustedNodes() []*enode.Node {
	return c.parsePersistentNodes(&c.trustedNodesWarning, c.ResolvePath(datadirTrustedNodes))
}

// parsePersistentNodes parses a list of discovery node URLs loaded from a .json
// file from within the data directory.
// parsePersistentNodesは、データディレクトリ内の.jsonファイルからロードされた検出ノードのURLのリストを解析します。
func (c *Config) parsePersistentNodes(w *bool, path string) []*enode.Node {
	// Short circuit if no node config is present
	// ノード構成が存在しない場合の短絡
	if c.DataDir == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	c.warnOnce(w, "Found deprecated node list file %s, please use the TOML config file instead.", path)

	// Load the nodes from the config file.
	// 構成ファイルからノードをロードします。
	var nodelist []string
	if err := common.LoadJSON(path, &nodelist); err != nil {
		log.Error(fmt.Sprintf("Can't load node list file: %v", err))
		return nil
	}
	// Interpret the list as a discovery node array
	// リストを検出ノード配列として解釈します
	var nodes []*enode.Node
	for _, url := range nodelist {
		if url == "" {
			continue
		}
		node, err := enode.Parse(enode.ValidSchemes, url)
		if err != nil {
			log.Error(fmt.Sprintf("Node URL %s: %v\n", url, err))
			continue
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// KeyDirConfig determines the settings for keydirectory
// KeyDirConfigはkeydirectoryの設定を決定します
func (c *Config) KeyDirConfig() (string, error) {
	var (
		keydir string
		err    error
	)
	switch {
	case filepath.IsAbs(c.KeyStoreDir):
		keydir = c.KeyStoreDir
	case c.DataDir != "":
		if c.KeyStoreDir == "" {
			keydir = filepath.Join(c.DataDir, datadirDefaultKeyStore)
		} else {
			keydir, err = filepath.Abs(c.KeyStoreDir)
		}
	case c.KeyStoreDir != "":
		keydir, err = filepath.Abs(c.KeyStoreDir)
	}
	return keydir, err
}

// getKeyStoreDir retrieves the key directory and will create
// and ephemeral one if necessary.
// getKeyStoreDirはキーディレクトリを取得し、必要に応じて一時的なものを作成します。
func getKeyStoreDir(conf *Config) (string, bool, error) {
	keydir, err := conf.KeyDirConfig()
	if err != nil {
		return "", false, err
	}
	isEphemeral := false
	if keydir == "" {
		// There is no datadir.
		// datadirはありません。
		keydir, err = ioutil.TempDir("", "go-ethereum-keystore")
		isEphemeral = true
	}

	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(keydir, 0700); err != nil {
		return "", false, err
	}

	return keydir, isEphemeral, nil
}

var warnLock sync.Mutex

func (c *Config) warnOnce(w *bool, format string, args ...interface{}) {
	warnLock.Lock()
	defer warnLock.Unlock()

	if *w {
		return
	}
	l := c.Logger
	if l == nil {
		l = log.Root()
	}
	l.Warn(fmt.Sprintf(format, args...))
	*w = true
}
