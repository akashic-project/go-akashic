// Copyright 2015 The go-ethereum Authors
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
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/prometheus/tsdb/fileutil"
)

// Node is a container on which services can be registered.
// ノードは、サービスを登録できるコンテナです。
type Node struct {
	eventmux      *event.TypeMux
	config        *Config
	accman        *accounts.Manager
	log           log.Logger
	keyDir        string            // キーストアディレクトリ // key store directory
	keyDirTemp    bool              // trueの場合、キーディレクトリはStopによって削除されます // If true, key directory will be removed by Stop
	dirLock       fileutil.Releaser // インスタンスディレクトリの同時使用を防止します         // prevents concurrent use of instance directory
	stop          chan struct{}     // 終了通知を待機するチャネル                           // Channel to wait for termination notifications
	server        *p2p.Server       // 現在実行中のP2Pネットワーク層                        // Currently running P2P networking layer
	startStopLock sync.Mutex        // 開始/停止は追加のロックによって保護されています        // Start/Stop are protected by an additional lock
	state         int               // ノードのライフサイクルの状態を追跡します              // Tracks state of node lifecycle

	lock          sync.Mutex
	lifecycles    []Lifecycle // ライフサイクルを持つすべての登録済みバックエンド、サービス、および補助サービス  // All registered backends, services, and auxiliary services that have a lifecycle
	rpcAPIs       []rpc.API   // ノードによって現在提供されているAPIのリスト                                 // List of APIs currently provided by the node
	http          *httpServer //
	ws            *httpServer //
	ipc           *ipcServer  // ipchttpサーバーに関する情報を格納します                       // Stores information about the ipc http server
	inprocHandler *rpc.Server // APIリクエストを処理するためのインプロセスRPCリクエストハンドラ  // In-process RPC request handler to process the API requests

	databases map[*closeTrackingDB]struct{} // 開いているすべてのデータベース // All open databases
}

const (
	initializingState = iota
	runningState
	closedState
)

// New creates a new P2P node, ready for protocol registration.
// Newは、プロトコル登録の準備ができた新しいP2Pノードを作成します。
func New(conf *Config) (*Node, error) {
	// Copy config and resolve the datadir so future changes to the current
	// working directory don't affect the node.
	// 構成をコピーしてdatadirを解決し、現在の作業ディレクトリへの今後の変更がノードに影響を与えないようにします。
	confCopy := *conf
	conf = &confCopy
	if conf.DataDir != "" {
		absdatadir, err := filepath.Abs(conf.DataDir)
		if err != nil {
			return nil, err
		}
		conf.DataDir = absdatadir
	}
	if conf.Logger == nil {
		conf.Logger = log.New()
	}

	// Ensure that the instance name doesn't cause weird conflicts with
	// other files in the data directory.
	// インスタンス名がデータディレクトリ内の他のファイルとの奇妙な競合を引き起こさないことを確認してください。
	if strings.ContainsAny(conf.Name, `/\`) {
		return nil, errors.New(`Config.Name must not contain '/' or '\'`)
	}
	if conf.Name == datadirDefaultKeyStore {
		return nil, errors.New(`Config.Name cannot be "` + datadirDefaultKeyStore + `"`)
	}
	if strings.HasSuffix(conf.Name, ".ipc") {
		return nil, errors.New(`Config.Name cannot end in ".ipc"`)
	}

	node := &Node{
		config:        conf,
		inprocHandler: rpc.NewServer(),
		eventmux:      new(event.TypeMux),
		log:           conf.Logger,
		stop:          make(chan struct{}),
		server:        &p2p.Server{Config: conf.P2P},
		databases:     make(map[*closeTrackingDB]struct{}),
	}

	// Register built-in APIs.
	// 組み込みAPIを登録します。
	node.rpcAPIs = append(node.rpcAPIs, node.apis()...)

	// Acquire the instance directory lock.
	// インスタンスディレクトリロックを取得します。
	if err := node.openDataDir(); err != nil {
		return nil, err
	}
	keyDir, isEphem, err := getKeyStoreDir(conf)
	if err != nil {
		return nil, err
	}
	node.keyDir = keyDir
	node.keyDirTemp = isEphem
	// Creates an empty AccountManager with no backends. Callers (e.g. cmd/geth)
	// are required to add the backends later on.
	// バックエンドのない空のAccountManagerを作成します。
	//　呼び出し元（cmd / gethなど）は、後でバックエンドを追加する必要があります。
	node.accman = accounts.NewManager(&accounts.Config{InsecureUnlockAllowed: conf.InsecureUnlockAllowed})

	// Initialize the p2p server. This creates the node key and discovery databases.
	// p2pサーバーを初期化します。これにより、ノードキーと検出データベースが作成されます。
	node.server.Config.PrivateKey = node.config.NodeKey()
	node.server.Config.Name = node.config.NodeName()
	node.server.Config.Logger = node.log
	if node.server.Config.StaticNodes == nil {
		node.server.Config.StaticNodes = node.config.StaticNodes()
	}
	if node.server.Config.TrustedNodes == nil {
		node.server.Config.TrustedNodes = node.config.TrustedNodes()
	}
	if node.server.Config.NodeDatabase == "" {
		node.server.Config.NodeDatabase = node.config.NodeDB()
	}

	// Check HTTP/WS prefixes are valid.
	// HTTP / WSプレフィックスが有効であることを確認してください。
	if err := validatePrefix("HTTP", conf.HTTPPathPrefix); err != nil {
		return nil, err
	}
	if err := validatePrefix("WebSocket", conf.WSPathPrefix); err != nil {
		return nil, err
	}

	// Configure RPC servers.
	// RPCサーバーを構成します。
	node.http = newHTTPServer(node.log, conf.HTTPTimeouts)
	node.ws = newHTTPServer(node.log, rpc.DefaultHTTPTimeouts)
	node.ipc = newIPCServer(node.log, conf.IPCEndpoint())

	return node, nil
}

// Start starts all registered lifecycles, RPC services and p2p networking.
// Node can only be started once.
// Startは、登録されているすべてのライフサイクル、RPCサービス、およびp2pネットワーキングを開始します。
// ノードは1回だけ開始できます。
func (n *Node) Start() error {
	n.startStopLock.Lock()
	defer n.startStopLock.Unlock()

	n.lock.Lock()
	switch n.state {
	case runningState:
		n.lock.Unlock()
		return ErrNodeRunning
	case closedState:
		n.lock.Unlock()
		return ErrNodeStopped
	}
	n.state = runningState
	// open networking and RPC endpoints
	// オープンネットワーキングとRPCエンドポイント
	err := n.openEndpoints()
	lifecycles := make([]Lifecycle, len(n.lifecycles))
	copy(lifecycles, n.lifecycles)
	n.lock.Unlock()

	// Check if endpoint startup failed.
	// エンドポイントの起動に失敗したかどうかを確認します。
	if err != nil {
		n.doClose(nil)
		return err
	}
	// Start all registered lifecycles.
	// 登録されているすべてのライフサイクルを開始します。
	var started []Lifecycle
	for _, lifecycle := range lifecycles {
		if err = lifecycle.Start(); err != nil {
			break
		}
		started = append(started, lifecycle)
	}
	// Check if any lifecycle failed to start.
	// ライフサイクルの開始に失敗したかどうかを確認します。
	if err != nil {
		n.stopServices(started)
		n.doClose(nil)
	}
	return err
}

// Close stops the Node and releases resources acquired in
// Node constructor New.
// Closeはノードを停止し、ノードコンストラクターNewで取得したリソースを解放します。
func (n *Node) Close() error {
	n.startStopLock.Lock()
	defer n.startStopLock.Unlock()

	n.lock.Lock()
	state := n.state
	n.lock.Unlock()
	switch state {
	case initializingState:
		// The node was never started.
		// ノードは開始されませんでした。
		return n.doClose(nil)
	case runningState:
		// The node was started, release resources acquired by Start().
		// ノードが開始され、Start（）によって取得されたリソースを解放します。
		var errs []error
		if err := n.stopServices(n.lifecycles); err != nil {
			errs = append(errs, err)
		}
		return n.doClose(errs)
	case closedState:
		return ErrNodeStopped
	default:
		panic(fmt.Sprintf("node is in unknown state %d", state))
	}
}

// doClose releases resources acquired by New(), collecting errors.
// doCloseは、New（）によって取得されたリソースを解放し、エラーを収集します。
func (n *Node) doClose(errs []error) error {
	// Close databases. This needs the lock because it needs to
	// synchronize with OpenDatabase*.
	// データベースを閉じます。 OpenDatabase *と同期する必要があるため、これにはロックが必要です。
	n.lock.Lock()
	n.state = closedState
	errs = append(errs, n.closeDatabases()...)
	n.lock.Unlock()

	if err := n.accman.Close(); err != nil {
		errs = append(errs, err)
	}
	if n.keyDirTemp {
		if err := os.RemoveAll(n.keyDir); err != nil {
			errs = append(errs, err)
		}
	}

	// Release instance directory lock.
	// インスタンスディレクトリロックを解除します。
	n.closeDataDir()

	// Unblock n.Wait.
	// n.Waitのブロックを解除します。
	close(n.stop)

	// Report any errors that might have occurred.
	// 発生した可能性のあるエラーを報告します。
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return fmt.Errorf("%v", errs)
	}
}

// openEndpoints starts all network and RPC endpoints.
// openEndpointsは、すべてのネットワークエンドポイントとRPCエンドポイントを開始します。
func (n *Node) openEndpoints() error {
	// start networking endpoints
	// ネットワークエンドポイントを開始します
	n.log.Info("Starting peer-to-peer node", "instance", n.server.Name)
	if err := n.server.Start(); err != nil {
		return convertFileLockError(err)
	}
	// start RPC endpoints
	// RPCエンドポイントを開始します
	err := n.startRPC()
	if err != nil {
		n.stopRPC()
		n.server.Stop()
	}
	return err
}

// containsLifecycle checks if 'lfs' contains 'l'.
// containsLifecycleは、「lfs」に「l」が含まれているかどうかを確認します。
func containsLifecycle(lfs []Lifecycle, l Lifecycle) bool {
	for _, obj := range lfs {
		if obj == l {
			return true
		}
	}
	return false
}

// stopServices terminates running services, RPC and p2p networking.
// It is the inverse of Start.
// stopServicesは、実行中のサービス、RPC、およびp2pネットワークを終了します。
// これはStartの逆です。
func (n *Node) stopServices(running []Lifecycle) error {
	n.stopRPC()

	// Stop running lifecycles in reverse order.
	// ライフサイクルの逆順での実行を停止します。
	failure := &StopError{Services: make(map[reflect.Type]error)}
	for i := len(running) - 1; i >= 0; i-- {
		if err := running[i].Stop(); err != nil {
			failure.Services[reflect.TypeOf(running[i])] = err
		}
	}

	// Stop p2p networking.
	// p2pネットワークを停止します。
	n.server.Stop()

	if len(failure.Services) > 0 {
		return failure
	}
	return nil
}

func (n *Node) openDataDir() error {
	if n.config.DataDir == "" {
		return nil // 一時的 // ephemeral
	}

	instdir := filepath.Join(n.config.DataDir, n.config.name())
	if err := os.MkdirAll(instdir, 0700); err != nil {
		return err
	}
	// Lock the instance directory to prevent concurrent use by another instance as well as
	// accidental use of the instance directory as a database.
	// インスタンスディレクトリをロックして、別のインスタンスが同時に使用したり、
	// インスタンスディレクトリをデータベースとして誤って使用したりしないようにします。
	release, _, err := fileutil.Flock(filepath.Join(instdir, "LOCK"))
	if err != nil {
		return convertFileLockError(err)
	}
	n.dirLock = release
	return nil
}

func (n *Node) closeDataDir() {
	// Release instance directory lock.
	// インスタンスディレクトリロックを解放します。
	if n.dirLock != nil {
		if err := n.dirLock.Release(); err != nil {
			n.log.Error("Can't release datadir lock", "err", err)
		}
		n.dirLock = nil
	}
}

// configureRPC is a helper method to configure all the various RPC endpoints during node
// startup. It's not meant to be called at any time afterwards as it makes certain
// assumptions about the state of the node.
// configureRPCは、ノードの起動時にさまざまなRPCエンドポイントをすべて構成するためのヘルパーメソッドです。
// ノードの状態について特定の仮定を行うため、後でいつでも呼び出されることを意図していません。
func (n *Node) startRPC() error {
	if err := n.startInProc(); err != nil {
		return err
	}

	// Configure IPC.
	// IPCを構成します。
	if n.ipc.endpoint != "" {
		if err := n.ipc.start(n.rpcAPIs); err != nil {
			return err
		}
	}

	// Configure HTTP.
	// HTTPを構成します。
	if n.config.HTTPHost != "" {
		config := httpConfig{
			CorsAllowedOrigins: n.config.HTTPCors,
			Vhosts:             n.config.HTTPVirtualHosts,
			Modules:            n.config.HTTPModules,
			prefix:             n.config.HTTPPathPrefix,
		}
		if err := n.http.setListenAddr(n.config.HTTPHost, n.config.HTTPPort); err != nil {
			return err
		}
		if err := n.http.enableRPC(n.rpcAPIs, config); err != nil {
			return err
		}
	}

	// Configure WebSocket.
	// WebSocketを構成します。
	if n.config.WSHost != "" {
		server := n.wsServerForPort(n.config.WSPort)
		config := wsConfig{
			Modules: n.config.WSModules,
			Origins: n.config.WSOrigins,
			prefix:  n.config.WSPathPrefix,
		}
		if err := server.setListenAddr(n.config.WSHost, n.config.WSPort); err != nil {
			return err
		}
		if err := server.enableWS(n.rpcAPIs, config); err != nil {
			return err
		}
	}

	if err := n.http.start(); err != nil {
		return err
	}
	return n.ws.start()
}

func (n *Node) wsServerForPort(port int) *httpServer {
	if n.config.HTTPHost == "" || n.http.port == port {
		return n.http
	}
	return n.ws
}

func (n *Node) stopRPC() {
	n.http.stop()
	n.ws.stop()
	n.ipc.stop()
	n.stopInProc()
}

// startInProc registers all RPC APIs on the inproc server.
// startInProcは、すべてのRPCAPIをinprocサーバーに登録します。
func (n *Node) startInProc() error {
	for _, api := range n.rpcAPIs {
		if err := n.inprocHandler.RegisterName(api.Namespace, api.Service); err != nil {
			return err
		}
	}
	return nil
}

// stopInProc terminates the in-process RPC endpoint.
// stopInProcは、インプロセスRPCエンドポイントを終了します。
func (n *Node) stopInProc() {
	n.inprocHandler.Stop()
}

// Wait blocks until the node is closed.
// ノードが閉じられるまでブロックを待ちます。
func (n *Node) Wait() {
	<-n.stop
}

// RegisterLifecycle registers the given Lifecycle on the node.
// RegisterLifecycleは、指定されたライフサイクルをノードに登録します。
func (n *Node) RegisterLifecycle(lifecycle Lifecycle) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.state != initializingState {
		panic("can't register lifecycle on running/stopped node")
	}
	if containsLifecycle(n.lifecycles, lifecycle) {
		panic(fmt.Sprintf("attempt to register lifecycle %T more than once", lifecycle))
	}
	n.lifecycles = append(n.lifecycles, lifecycle)
}

// RegisterProtocols adds backend's protocols to the node's p2p server.
// RegisterProtocolsは、バックエンドのプロトコルをノードのp2pサーバーに追加します。
func (n *Node) RegisterProtocols(protocols []p2p.Protocol) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.state != initializingState {
		panic("can't register protocols on running/stopped node")
	}
	n.server.Protocols = append(n.server.Protocols, protocols...)
}

// RegisterAPIs registers the APIs a service provides on the node.
// RegisterAPIsは、サービスがノードに提供するAPIを登録します。
func (n *Node) RegisterAPIs(apis []rpc.API) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.state != initializingState {
		panic("can't register APIs on running/stopped node")
	}
	n.rpcAPIs = append(n.rpcAPIs, apis...)
}

// RegisterHandler mounts a handler on the given path on the canonical HTTP server.
//
// The name of the handler is shown in a log message when the HTTP server starts
// and should be a descriptive term for the service provided by the handler.
// RegisterHandlerは、正規HTTPサーバーの指定されたパスにハンドラーをマウントします。
//
//ハンドラーの名前は、HTTPサーバーの起動時にログメッセージに表示され、ハンドラーによって提供されるサービスを説明する用語である必要があります。
func (n *Node) RegisterHandler(name, path string, handler http.Handler) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.state != initializingState {
		panic("can't register HTTP handler on running/stopped node")
	}

	n.http.mux.Handle(path, handler)
	n.http.handlerNames[path] = name
}

// Attach creates an RPC client attached to an in-process API handler.
// Attachは、インプロセスAPIハンドラーにアタッチされたRPCクライアントを作成します。
func (n *Node) Attach() (*rpc.Client, error) {
	return rpc.DialInProc(n.inprocHandler), nil
}

// RPCHandler returns the in-process RPC request handler.
// RPCHandlerは、処理中のRPC要求ハンドラーを返します。
func (n *Node) RPCHandler() (*rpc.Server, error) {
	n.lock.Lock()
	defer n.lock.Unlock()

	if n.state == closedState {
		return nil, ErrNodeStopped
	}
	return n.inprocHandler, nil
}

// Config returns the configuration of node.
// Configはノードの構成を返します。
func (n *Node) Config() *Config {
	return n.config
}

// Server retrieves the currently running P2P network layer. This method is meant
// only to inspect fields of the currently running server. Callers should not
// start or stop the returned server.
// サーバーは、現在実行中のP2Pネットワーク層を取得します。このメソッドは、
// 現在実行中のサーバーのフィールドを検査することのみを目的としています。
// 呼び出し元は、返されたサーバーを起動または停止しないでください。
func (n *Node) Server() *p2p.Server {
	n.lock.Lock()
	defer n.lock.Unlock()

	return n.server
}

// DataDir retrieves the current datadir used by the protocol stack.
// Deprecated: No files should be stored in this directory, use InstanceDir instead.
// DataDirは、プロトコルスタックによって使用されている現在のdatadirを取得します。
// 非推奨：このディレクトリにファイルを保存しないでください。代わりにInstanceDirを使用してください。
func (n *Node) DataDir() string {
	return n.config.DataDir
}

// InstanceDir retrieves the instance directory used by the protocol stack.
// InstanceDirは、プロトコルスタックによって使用されるインスタンスディレクトリを取得します。
func (n *Node) InstanceDir() string {
	return n.config.instanceDir()
}

// KeyStoreDir retrieves the key directory
// KeyStoreDirはキーディレクトリを取得します
func (n *Node) KeyStoreDir() string {
	return n.keyDir
}

// AccountManager retrieves the account manager used by the protocol stack.
// AccountManagerは、プロトコルスタックによって使用されるアカウントマネージャーを取得します。
func (n *Node) AccountManager() *accounts.Manager {
	return n.accman
}

// IPCEndpoint retrieves the current IPC endpoint used by the protocol stack.
// IPCEndpointは、プロトコルスタックによって使用されている現在のIPCエンドポイントを取得します。
func (n *Node) IPCEndpoint() string {
	return n.ipc.endpoint
}

// HTTPEndpoint returns the URL of the HTTP server. Note that this URL does not
// contain the JSON-RPC path prefix set by HTTPPathPrefix.
// HTTPEndpointは、HTTPサーバーのURLを返します。
// このURLには、HTTPPathPrefixによって設定されたJSON-RPCパスプレフィックスが含まれていないことに注意してください。
func (n *Node) HTTPEndpoint() string {
	return "http://" + n.http.listenAddr()
}

// WSEndpoint returns the current JSON-RPC over WebSocket endpoint.
// WSEndpointは、現在のJSON-RPC overWebSocketエンドポイントを返します。
func (n *Node) WSEndpoint() string {
	if n.http.wsAllowed() {
		return "ws://" + n.http.listenAddr() + n.http.wsConfig.prefix
	}
	return "ws://" + n.ws.listenAddr() + n.ws.wsConfig.prefix
}

// EventMux retrieves the event multiplexer used by all the network services in
// the current protocol stack.
// EventMuxは、現在のプロトコルスタック内のすべてのネットワークサービスで使用されているイベントマルチプレクサを取得します。
func (n *Node) EventMux() *event.TypeMux {
	return n.eventmux
}

// OpenDatabase opens an existing database with the given name (or creates one if no
// previous can be found) from within the node's instance directory. If the node is
// ephemeral, a memory database is returned.
// OpenDatabaseは、ノードのインスタンスディレクトリ内から、指定された名前で既存のデータベースを開きます
//（または、前のデータベースが見つからない場合はデータベースを作成します）。
// ノードがエフェメラルの場合、メモリデータベースが返されます。
func (n *Node) OpenDatabase(name string, cache, handles int, namespace string, readonly bool) (ethdb.Database, error) {
	n.lock.Lock()
	defer n.lock.Unlock()
	if n.state == closedState {
		return nil, ErrNodeStopped
	}

	var db ethdb.Database
	var err error
	if n.config.DataDir == "" {
		db = rawdb.NewMemoryDatabase()
	} else {
		db, err = rawdb.NewLevelDBDatabase(n.ResolvePath(name), cache, handles, namespace, readonly)
	}

	if err == nil {
		db = n.wrapDatabase(db)
	}
	return db, err
}

// OpenDatabaseWithFreezer opens an existing database with the given name (or
// creates one if no previous can be found) from within the node's data directory,
// also attaching a chain freezer to it that moves ancient chain data from the
// database to immutable append-only files. If the node is an ephemeral one, a
// memory database is returned.
// OpenDatabaseWithFreezerは、ノードのデータディレクトリ内から指定された名前で既存のデータベースを開きます
//（または、前のデータベースが見つからない場合はデータベースを作成します）。
// また、データベースから不変の追加専用ファイルに古いチェーンデータを移動するチェーンフリーザーを接続します。
// ノードが一時的なものである場合、メモリデータベースが返されます。
func (n *Node) OpenDatabaseWithFreezer(name string, cache, handles int, freezer, namespace string, readonly bool) (ethdb.Database, error) {
	n.lock.Lock()
	defer n.lock.Unlock()
	if n.state == closedState {
		return nil, ErrNodeStopped
	}

	var db ethdb.Database
	var err error
	if n.config.DataDir == "" {
		db = rawdb.NewMemoryDatabase()
	} else {
		root := n.ResolvePath(name)
		switch {
		case freezer == "":
			freezer = filepath.Join(root, "ancient")
		case !filepath.IsAbs(freezer):
			freezer = n.ResolvePath(freezer)
		}
		db, err = rawdb.NewLevelDBDatabaseWithFreezer(root, cache, handles, freezer, namespace, readonly)
	}

	if err == nil {
		db = n.wrapDatabase(db)
	}
	return db, err
}

// ResolvePath returns the absolute path of a resource in the instance directory.
// ResolvePathは、インスタンスディレクトリ内のリソースの絶対パスを返します。
func (n *Node) ResolvePath(x string) string {
	return n.config.ResolvePath(x)
}

// closeTrackingDB wraps the Close method of a database. When the database is closed by the
// service, the wrapper removes it from the node's database map. This ensures that Node
// won't auto-close the database if it is closed by the service that opened it.
// closeTrackingDBは、データベースのCloseメソッドをラップします。データベースがサービスによって閉じられると、
// ラッパーはそのデータベースをノードのデータベースマップから削除します。
// これにより、ノードを開いたサービスによってデータベースが閉じられた場合に、
// ノードがデータベースを自動的に閉じないことが保証されます。
type closeTrackingDB struct {
	ethdb.Database
	n *Node
}

func (db *closeTrackingDB) Close() error {
	db.n.lock.Lock()
	delete(db.n.databases, db)
	db.n.lock.Unlock()
	return db.Database.Close()
}

// wrapDatabase ensures the database will be auto-closed when Node is closed.
// wrapDatabaseは、ノードが閉じられたときにデータベースが自動的に閉じられることを保証します。
func (n *Node) wrapDatabase(db ethdb.Database) ethdb.Database {
	wrapper := &closeTrackingDB{db, n}
	n.databases[wrapper] = struct{}{}
	return wrapper
}

// closeDatabases closes all open databases.
// closeDatabasesは、開いているすべてのデータベースを閉じます。
func (n *Node) closeDatabases() (errors []error) {
	for db := range n.databases {
		delete(n.databases, db)
		if err := db.Database.Close(); err != nil {
			errors = append(errors, err)
		}
	}
	return errors
}
