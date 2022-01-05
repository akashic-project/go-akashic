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

package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/pkg/reexec"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
)

func init() {
	// Register a reexec function to start a simulation node when the current binary is
	// executed as "p2p-node" (rather than whatever the main() function would normally do).
	// 現在のバイナリが（main（）関数が通常行うことではなく）
	//「p2p-node」として実行されたときにシミュレーションノードを開始するために、再実行関数を登録します。
	reexec.Register("p2p-node", execP2PNode)
}

// ExecAdapter is a NodeAdapter which runs simulation nodes by executing the current binary
// as a child process.
// ExecAdapterは、現在のバイナリを子プロセスとして実行することでシミュレーションノードを実行するNodeAdapterです。
type ExecAdapter struct {
	// BaseDir is the directory under which the data directories for each
	// simulation node are created.
	// BaseDirは、各シミュレーションノードのデータディレクトリが作成されるディレクトリです。
	BaseDir string

	nodes map[enode.ID]*ExecNode
}

// NewExecAdapter returns an ExecAdapter which stores node data in
// subdirectories of the given base directory
// NewExecAdapterは、指定されたベースディレクトリのサブディレクトリにノードデータを
// 格納するExecAdapterを返します。
func NewExecAdapter(baseDir string) *ExecAdapter {
	return &ExecAdapter{
		BaseDir: baseDir,
		nodes:   make(map[enode.ID]*ExecNode),
	}
}

// Name returns the name of the adapter for logging purposes
// Nameは、ロギングの目的でアダプターの名前を返します
func (e *ExecAdapter) Name() string {
	return "exec-adapter"
}

// NewNode returns a new ExecNode using the given config
// NewNodeは、指定された構成を使用して新しいExecNodeを返します
func (e *ExecAdapter) NewNode(config *NodeConfig) (Node, error) {
	if len(config.Lifecycles) == 0 {
		return nil, errors.New("node must have at least one service lifecycle")
	}
	for _, service := range config.Lifecycles {
		if _, exists := lifecycleConstructorFuncs[service]; !exists {
			return nil, fmt.Errorf("unknown node service %q", service)
		}
	}

	// create the node directory using the first 12 characters of the ID
	// as Unix socket paths cannot be longer than 256 characters
	// Unixソケットパスは256文字を超えることはできないため、
	// IDの最初の12文字を使用してノードディレクトリを作成します
	dir := filepath.Join(e.BaseDir, config.ID.String()[:12])
	if err := os.Mkdir(dir, 0755); err != nil {
		return nil, fmt.Errorf("error creating node directory: %s", err)
	}

	err := config.initDummyEnode()
	if err != nil {
		return nil, err
	}

	// generate the config
	// 構成を生成する
	conf := &execNodeConfig{
		Stack: node.DefaultConfig,
		Node:  config,
	}
	if config.DataDir != "" {
		conf.Stack.DataDir = config.DataDir
	} else {
		conf.Stack.DataDir = filepath.Join(dir, "data")
	}

	// these parameters are crucial for execadapter node to run correctly
	// これらのパラメーターは、execadapterノードが正しく実行されるために重要です。
	conf.Stack.WSHost = "127.0.0.1"
	conf.Stack.WSPort = 0
	conf.Stack.WSOrigins = []string{"*"}
	conf.Stack.WSExposeAll = true
	conf.Stack.P2P.EnableMsgEvents = config.EnableMsgEvents
	conf.Stack.P2P.NoDiscovery = true
	conf.Stack.P2P.NAT = nil

	// Listen on a localhost port, which we set when we
	// initialise NodeConfig (usually a random port)
	// NodeConfigを初期化するときに設定したローカルホストポート
	//（通常はランダムポート）でリッスンします
	conf.Stack.P2P.ListenAddr = fmt.Sprintf(":%d", config.Port)

	node := &ExecNode{
		ID:      config.ID,
		Dir:     dir,
		Config:  conf,
		adapter: e,
	}
	node.newCmd = node.execCommand
	e.nodes[node.ID] = node
	return node, nil
}

// ExecNode starts a simulation node by exec'ing the current binary and
// running the configured services
// ExecNodeは、現在のバイナリを実行し、構成されたサービスを実行することにより、
// シミュレーションノードを開始します。
type ExecNode struct {
	ID     enode.ID
	Dir    string
	Config *execNodeConfig
	Cmd    *exec.Cmd
	Info   *p2p.NodeInfo

	adapter *ExecAdapter
	client  *rpc.Client
	wsAddr  string
	newCmd  func() *exec.Cmd
}

// Addr returns the node's enode URL
// AddrはノードのenodeURLを返します
func (n *ExecNode) Addr() []byte {
	if n.Info == nil {
		return nil
	}
	return []byte(n.Info.Enode)
}

// Client returns an rpc.Client which can be used to communicate with the
// underlying services (it is set once the node has started)
// クライアントは、基盤となるサービスとの通信に使用できるrpc.Clientを返します
//（ノードが開始されると設定されます）
func (n *ExecNode) Client() (*rpc.Client, error) {
	return n.client, nil
}

// Start exec's the node passing the ID and service as command line arguments
// and the node config encoded as JSON in an environment variable.
// IDとサービスをコマンドライン引数として渡すノードと、
// 環境変数でJSONとしてエンコードされたノード構成のexecを開始します。
func (n *ExecNode) Start(snapshots map[string][]byte) (err error) {
	if n.Cmd != nil {
		return errors.New("already started")
	}
	defer func() {
		if err != nil {
			n.Stop()
		}
	}()

	// encode a copy of the config containing the snapshot
	// スナップショットを含む構成のコピーをエンコードする
	confCopy := *n.Config
	confCopy.Snapshots = snapshots
	confCopy.PeerAddrs = make(map[string]string)
	for id, node := range n.adapter.nodes {
		confCopy.PeerAddrs[id.String()] = node.wsAddr
	}
	confData, err := json.Marshal(confCopy)
	if err != nil {
		return fmt.Errorf("error generating node config: %s", err)
	}
	// expose the admin namespace via websocket if it's not enabled
	// 有効になっていない場合は、WebSocketを介してadmin名前空間を公開します
	exposed := confCopy.Stack.WSExposeAll
	if !exposed {
		for _, api := range confCopy.Stack.WSModules {
			if api == "admin" {
				exposed = true
				break
			}
		}
	}
	if !exposed {
		confCopy.Stack.WSModules = append(confCopy.Stack.WSModules, "admin")
	}
	// start the one-shot server that waits for startup information
	// 起動情報を待機するワンショットサーバーを起動します
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	statusURL, statusC := n.waitForStartupJSON(ctx)

	// start the node
	// ノードを開始します
	cmd := n.newCmd()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		envStatusURL+"="+statusURL,
		envNodeConfig+"="+string(confData),
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("error starting node: %s", err)
	}
	n.Cmd = cmd

	// Wait for the node to start.
	// ノードが起動するのを待ちます
	status := <-statusC
	if status.Err != "" {
		return errors.New(status.Err)
	}
	client, err := rpc.DialWebsocket(ctx, status.WSEndpoint, "")
	if err != nil {
		return fmt.Errorf("can't connect to RPC server: %v", err)
	}

	// Node ready :)
	// ノード準備完了:)
	n.client = client
	n.wsAddr = status.WSEndpoint
	n.Info = status.NodeInfo
	return nil
}

// waitForStartupJSON runs a one-shot HTTP server to receive a startup report.
// waitForStartupJSONは、ワンショットHTTPサーバーを実行して、スタートアップレポートを受信します。
func (n *ExecNode) waitForStartupJSON(ctx context.Context) (string, chan nodeStartupJSON) {
	var (
		ch       = make(chan nodeStartupJSON, 1)
		quitOnce sync.Once
		srv      http.Server
	)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		ch <- nodeStartupJSON{Err: err.Error()}
		return "", ch
	}
	quit := func(status nodeStartupJSON) {
		quitOnce.Do(func() {
			l.Close()
			ch <- status
		})
	}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var status nodeStartupJSON
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			status.Err = fmt.Sprintf("can't decode startup report: %v", err)
		}
		quit(status)
	})
	// Run the HTTP server, but don't wait forever and shut it down
	// if the context is canceled.
	// HTTPサーバーを実行しますが、コンテキストがキャンセルされた場合は、
	// 永久に待機してシャットダウンしないでください。
	go srv.Serve(l)
	go func() {
		<-ctx.Done()
		quit(nodeStartupJSON{Err: "didn't get startup report"})
	}()

	url := "http://" + l.Addr().String()
	return url, ch
}

// execCommand returns a command which runs the node locally by exec'ing
// the current binary but setting argv[0] to "p2p-node" so that the child
// runs execP2PNode
// execCommandは、現在のバイナリを実行することによってノードをローカルで実行するコマンドを返しますが、
// 子がexecP2PNodeを実行するようにargv [0]を「p2p-node」に設定します。
func (n *ExecNode) execCommand() *exec.Cmd {
	return &exec.Cmd{
		Path: reexec.Self(),
		Args: []string{"p2p-node", strings.Join(n.Config.Node.Lifecycles, ","), n.ID.String()},
	}
}

// Stop stops the node by first sending SIGTERM and then SIGKILL if the node
// doesn't stop within 5s
// Stopは、最初にSIGTERMを送信し、
// ノードが5秒以内に停止しない場合はSIGKILLを送信して、
// ノードを停止します。
func (n *ExecNode) Stop() error {
	if n.Cmd == nil {
		return nil
	}
	defer func() {
		n.Cmd = nil
	}()

	if n.client != nil {
		n.client.Close()
		n.client = nil
		n.wsAddr = ""
		n.Info = nil
	}

	if err := n.Cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return n.Cmd.Process.Kill()
	}
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- n.Cmd.Wait()
	}()
	select {
	case err := <-waitErr:
		return err
	case <-time.After(5 * time.Second):
		return n.Cmd.Process.Kill()
	}
}

// NodeInfo returns information about the node
// NodeInfoは、ノードに関する情報を返します
func (n *ExecNode) NodeInfo() *p2p.NodeInfo {
	info := &p2p.NodeInfo{
		ID: n.ID.String(),
	}
	if n.client != nil {
		n.client.Call(&info, "admin_nodeInfo")
	}
	return info
}

// ServeRPC serves RPC requests over the given connection by dialling the
// node's WebSocket address and joining the two connections
// ServeRPCは、ノードのWebSocketアドレスをダイヤルし、
// 2つの接続を結合することにより、指定された接続を介してRPC要求を処理します
func (n *ExecNode) ServeRPC(clientConn *websocket.Conn) error {
	conn, _, err := websocket.DefaultDialer.Dial(n.wsAddr, nil)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go wsCopy(&wg, conn, clientConn)
	go wsCopy(&wg, clientConn, conn)
	wg.Wait()
	conn.Close()
	return nil
}

func wsCopy(wg *sync.WaitGroup, src, dst *websocket.Conn) {
	defer wg.Done()
	for {
		msgType, r, err := src.NextReader()
		if err != nil {
			return
		}
		w, err := dst.NextWriter(msgType)
		if err != nil {
			return
		}
		if _, err = io.Copy(w, r); err != nil {
			return
		}
	}
}

// Snapshots creates snapshots of the services by calling the
// simulation_snapshot RPC method
// スナップショットは、simulation_snapshotRPCメソッドを呼び出すことによって
// サービスのスナップショットを作成します
func (n *ExecNode) Snapshots() (map[string][]byte, error) {
	if n.client == nil {
		return nil, errors.New("RPC not started")
	}
	var snapshots map[string][]byte
	return snapshots, n.client.Call(&snapshots, "simulation_snapshot")
}

// execNodeConfig is used to serialize the node configuration so it can be
// passed to the child process as a JSON encoded environment variable
// execNodeConfigは、ノード構成をシリアル化するために使用されるため、
// JSONでエンコードされた環境変数として子プロセスに渡すことができます。
type execNodeConfig struct {
	Stack     node.Config       `json:"stack"`
	Node      *NodeConfig       `json:"node"`
	Snapshots map[string][]byte `json:"snapshots,omitempty"`
	PeerAddrs map[string]string `json:"peer_addrs,omitempty"`
}

func initLogging() {
	// Initialize the logging by default first.
	// デフォルトでは、最初にロギングを初期化します。
	glogger := log.NewGlogHandler(log.StreamHandler(os.Stderr, log.LogfmtFormat()))
	glogger.Verbosity(log.LvlInfo)
	log.Root().SetHandler(glogger)

	confEnv := os.Getenv(envNodeConfig)
	if confEnv == "" {
		return
	}
	var conf execNodeConfig
	if err := json.Unmarshal([]byte(confEnv), &conf); err != nil {
		return
	}
	var writer = os.Stderr
	if conf.Node.LogFile != "" {
		logWriter, err := os.Create(conf.Node.LogFile)
		if err != nil {
			return
		}
		writer = logWriter
	}
	var verbosity = log.LvlInfo
	if conf.Node.LogVerbosity <= log.LvlTrace && conf.Node.LogVerbosity >= log.LvlCrit {
		verbosity = conf.Node.LogVerbosity
	}
	// Reinitialize the logger
	// ロガーを再初期化します
	glogger = log.NewGlogHandler(log.StreamHandler(writer, log.TerminalFormat(true)))
	glogger.Verbosity(verbosity)
	log.Root().SetHandler(glogger)
}

// execP2PNode starts a simulation node when the current binary is executed with
// argv[0] being "p2p-node", reading the service / ID from argv[1] / argv[2]
// and the node config from an environment variable.
// execP2PNodeは、argv [0]が「p2p-node」で現在のバイナリが実行されるとシミュレーションノードを開始し、
// argv [1] / argv [2]からサービス/ IDを読み取り、環境変数からノード構成を読み取ります。
func execP2PNode() {
	initLogging()

	statusURL := os.Getenv(envStatusURL)
	if statusURL == "" {
		log.Crit("missing " + envStatusURL)
	}

	// Start the node and gather startup report.
	// ノードを起動し、起動レポートを収集します。
	var status nodeStartupJSON
	stack, stackErr := startExecNodeStack()
	if stackErr != nil {
		status.Err = stackErr.Error()
	} else {
		status.WSEndpoint = stack.WSEndpoint()
		status.NodeInfo = stack.Server().NodeInfo()
	}

	// Send status to the host.
	// ステータスをホストに送信します。
	statusJSON, _ := json.Marshal(status)
	if _, err := http.Post(statusURL, "application/json", bytes.NewReader(statusJSON)); err != nil {
		log.Crit("Can't post startup info", "url", statusURL, "err", err)
	}
	if stackErr != nil {
		os.Exit(1)
	}

	// Stop the stack if we get a SIGTERM signal.
	// SIGTERMシグナルを受け取ったら、スタックを停止します。
	go func() {
		sigc := make(chan os.Signal, 1)
		signal.Notify(sigc, syscall.SIGTERM)
		defer signal.Stop(sigc)
		<-sigc
		log.Info("Received SIGTERM, shutting down...")
		stack.Close()
	}()
	stack.Wait() // Wait for the stack to exit. スタックが終了するのを待ちます。
}

func startExecNodeStack() (*node.Node, error) {
	// read the services from argv
	// argvからサービスを読む
	serviceNames := strings.Split(os.Args[1], ",")

	// decode the config
	// 設定をデコードする
	confEnv := os.Getenv(envNodeConfig)
	if confEnv == "" {
		return nil, fmt.Errorf("missing " + envNodeConfig)
	}
	var conf execNodeConfig
	if err := json.Unmarshal([]byte(confEnv), &conf); err != nil {
		return nil, fmt.Errorf("error decoding %s: %v", envNodeConfig, err)
	}

	// create enode record
	// enodeレコードを作成する
	nodeTcpConn, _ := net.ResolveTCPAddr("tcp", conf.Stack.P2P.ListenAddr)
	if nodeTcpConn.IP == nil {
		nodeTcpConn.IP = net.IPv4(127, 0, 0, 1)
	}
	conf.Node.initEnode(nodeTcpConn.IP, nodeTcpConn.Port, nodeTcpConn.Port)
	conf.Stack.P2P.PrivateKey = conf.Node.PrivateKey
	conf.Stack.Logger = log.New("node.id", conf.Node.ID.String())

	// initialize the devp2p stack
	// devp2pスタックを初期化します
	stack, err := node.New(&conf.Stack)
	if err != nil {
		return nil, fmt.Errorf("error creating node stack: %v", err)
	}

	// Register the services, collecting them into a map so they can
	// be accessed by the snapshot API.
	// スナップショットAPIからアクセスできるように、
	// サービスを登録してマップに収集します。
	services := make(map[string]node.Lifecycle, len(serviceNames))
	for _, name := range serviceNames {
		lifecycleFunc, exists := lifecycleConstructorFuncs[name]
		if !exists {
			return nil, fmt.Errorf("unknown node service %q", err)
		}
		ctx := &ServiceContext{
			RPCDialer: &wsRPCDialer{addrs: conf.PeerAddrs},
			Config:    conf.Node,
		}
		if conf.Snapshots != nil {
			ctx.Snapshot = conf.Snapshots[name]
		}
		service, err := lifecycleFunc(ctx, stack)
		if err != nil {
			return nil, err
		}
		services[name] = service
	}

	// Add the snapshot API.
	// スナップショットAPIを追加します。
	stack.RegisterAPIs([]rpc.API{{
		Namespace: "simulation",
		Version:   "1.0",
		Service:   SnapshotAPI{services},
	}})

	if err = stack.Start(); err != nil {
		err = fmt.Errorf("error starting stack: %v", err)
	}
	return stack, err
}

const (
	envStatusURL  = "_P2P_STATUS_URL"
	envNodeConfig = "_P2P_NODE_CONFIG"
)

// nodeStartupJSON is sent to the simulation host after startup.
// nodeStartupJSONは、起動後にシミュレーションホストに送信されます。
type nodeStartupJSON struct {
	Err        string
	WSEndpoint string
	NodeInfo   *p2p.NodeInfo
}

// SnapshotAPI provides an RPC method to create snapshots of services
// SnapshotAPIは、サービスのスナップショットを作成するためのRPCメソッドを提供します
type SnapshotAPI struct {
	services map[string]node.Lifecycle
}

func (api SnapshotAPI) Snapshot() (map[string][]byte, error) {
	snapshots := make(map[string][]byte)
	for name, service := range api.services {
		if s, ok := service.(interface {
			Snapshot() ([]byte, error)
		}); ok {
			snap, err := s.Snapshot()
			if err != nil {
				return nil, err
			}
			snapshots[name] = snap
		}
	}
	return snapshots, nil
}

type wsRPCDialer struct {
	addrs map[string]string
}

// DialRPC implements the RPCDialer interface by creating a WebSocket RPC
// client of the given node
// DialRPCは、指定されたノードのWebSocket RPCクライアントを作成することにより、
// RPCDialerインターフェイスを実装します
func (w *wsRPCDialer) DialRPC(id enode.ID) (*rpc.Client, error) {
	addr, ok := w.addrs[id.String()]
	if !ok {
		return nil, fmt.Errorf("unknown node: %s", id)
	}
	return rpc.DialWebsocket(context.Background(), addr, "http://localhost")
}
