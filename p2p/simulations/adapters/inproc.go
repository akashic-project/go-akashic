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
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sync"

	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/simulations/pipes"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
)

// SimAdapter is a NodeAdapter which creates in-memory simulation nodes and
// connects them using net.Pipe
// SimAdapterは、メモリ内シミュレーションノードを作成し、
// net.Pipeを使用してそれらを接続するNodeAdapterです。
type SimAdapter struct {
	pipe       func() (net.Conn, net.Conn, error)
	mtx        sync.RWMutex
	nodes      map[enode.ID]*SimNode
	lifecycles LifecycleConstructors
}

// NewSimAdapter creates a SimAdapter which is capable of running in-memory
// simulation nodes running any of the given services (the services to run on a
// particular node are passed to the NewNode function in the NodeConfig)
// the adapter uses a net.Pipe for in-memory simulated network connections
// NewSimAdapterは、指定されたサービスのいずれかを実行するメモリ内シミュレーションノードを実行できる
// SimAdapterを作成します（特定のノードで実行するサービスはNodeConfigのNewNode関数に渡されます）。
// アダプターはメモリ内にnet.Pipeを使用します。シミュレートされたネットワーク接続
func NewSimAdapter(services LifecycleConstructors) *SimAdapter {
	return &SimAdapter{
		pipe:       pipes.NetPipe,
		nodes:      make(map[enode.ID]*SimNode),
		lifecycles: services,
	}
}

// Name returns the name of the adapter for logging purposes
// Nameは、ロギングの目的でアダプターの名前を返します
func (s *SimAdapter) Name() string {
	return "sim-adapter"
}

// NewNode returns a new SimNode using the given config
// NewNodeは、指定された構成を使用して新しいSimNodeを返します
func (s *SimAdapter) NewNode(config *NodeConfig) (Node, error) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	id := config.ID
	// verify that the node has a private key in the config
	// ノードの構成に秘密鍵があることを確認します
	if config.PrivateKey == nil {
		return nil, fmt.Errorf("node is missing private key: %s", id)
	}

	// check a node with the ID doesn't already exist
	// IDを持つノードがまだ存在しないことを確認してください
	if _, exists := s.nodes[id]; exists {
		return nil, fmt.Errorf("node already exists: %s", id)
	}

	// check the services are valid
	// サービスが有効であることを確認してください
	if len(config.Lifecycles) == 0 {
		return nil, errors.New("node must have at least one service")
	}
	for _, service := range config.Lifecycles {
		if _, exists := s.lifecycles[service]; !exists {
			return nil, fmt.Errorf("unknown node service %q", service)
		}
	}

	err := config.initDummyEnode()
	if err != nil {
		return nil, err
	}

	n, err := node.New(&node.Config{
		P2P: p2p.Config{
			PrivateKey:      config.PrivateKey,
			MaxPeers:        math.MaxInt32,
			NoDiscovery:     true,
			Dialer:          s,
			EnableMsgEvents: config.EnableMsgEvents,
		},
		ExternalSigner: config.ExternalSigner,
		Logger:         log.New("node.id", id.String()),
	})
	if err != nil {
		return nil, err
	}

	simNode := &SimNode{
		ID:      id,
		config:  config,
		node:    n,
		adapter: s,
		running: make(map[string]node.Lifecycle),
	}
	s.nodes[id] = simNode
	return simNode, nil
}

// Dial implements the p2p.NodeDialer interface by connecting to the node using
// an in-memory net.Pipe
// Dialは、メモリ内のnet.Pipeを使用してノードに接続することにより、
// p2p.NodeDialerインターフェイスを実装します。
func (s *SimAdapter) Dial(ctx context.Context, dest *enode.Node) (conn net.Conn, err error) {
	node, ok := s.GetNode(dest.ID())
	if !ok {
		return nil, fmt.Errorf("unknown node: %s", dest.ID())
	}
	srv := node.Server()
	if srv == nil {
		return nil, fmt.Errorf("node not running: %s", dest.ID())
	}
	// SimAdapter.pipe is net.Pipe (NewSimAdapter)
	// SimAdapter.pipeはnet.Pipe（NewSimAdapter）です
	pipe1, pipe2, err := s.pipe()
	if err != nil {
		return nil, err
	}
	// this is simulated 'listening'
	// asynchronously call the dialed destination node's p2p server
	// to set up connection on the 'listening' side
	// これはシミュレートされます。
	// 「リスニング」は、ダイヤルされた宛先ノードのp2pサーバーを非同期的に呼び出して、
	// 「リスニング」側で接続をセットアップします。
	go srv.SetupConn(pipe1, 0, nil)
	return pipe2, nil
}

// DialRPC implements the RPCDialer interface by creating an in-memory RPC
// client of the given node
// DialRPCは、指定されたノードのメモリ内RPCクライアントを作成することにより、
// RPCDialerインターフェイスを実装します。
func (s *SimAdapter) DialRPC(id enode.ID) (*rpc.Client, error) {
	node, ok := s.GetNode(id)
	if !ok {
		return nil, fmt.Errorf("unknown node: %s", id)
	}
	return node.node.Attach()
}

// GetNode returns the node with the given ID if it exists
// GetNodeは、指定されたIDのノードが存在する場合、それを返します。
func (s *SimAdapter) GetNode(id enode.ID) (*SimNode, bool) {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	node, ok := s.nodes[id]
	return node, ok
}

// SimNode is an in-memory simulation node which connects to other nodes using
// net.Pipe (see SimAdapter.Dial), running devp2p protocols directly over that
// pipe
// SimNodeは、net.Pipe（SimAdapter.Dialを参照）を使用して他のノードに接続し、
// そのパイプ上で直接devp2pプロトコルを実行するインメモリシミュレーションノードです。
type SimNode struct {
	lock         sync.RWMutex
	ID           enode.ID
	config       *NodeConfig
	adapter      *SimAdapter
	node         *node.Node
	running      map[string]node.Lifecycle
	client       *rpc.Client
	registerOnce sync.Once
}

// Close closes the underlaying node.Node to release
// acquired resources.
// Closeは、下にあるnode.Nodeを閉じて、取得したリソースを解放します。
func (sn *SimNode) Close() error {
	return sn.node.Close()
}

// Addr returns the node's discovery address
// Addrはノードの検出アドレスを返します
func (sn *SimNode) Addr() []byte {
	return []byte(sn.Node().String())
}

// Node returns a node descriptor representing the SimNode
// Nodeは、SimNodeを表すノード記述子を返します
func (sn *SimNode) Node() *enode.Node {
	return sn.config.Node()
}

// Client returns an rpc.Client which can be used to communicate with the
// underlying services (it is set once the node has started)
// クライアントは、基盤となるサービスとの通信に使用できるrpc.Clientを返します（ノードが開始されると設定されます）
func (sn *SimNode) Client() (*rpc.Client, error) {
	sn.lock.RLock()
	defer sn.lock.RUnlock()
	if sn.client == nil {
		return nil, errors.New("node not started")
	}
	return sn.client, nil
}

// ServeRPC serves RPC requests over the given connection by creating an
// in-memory client to the node's RPC server.
// ServeRPCは、ノードのRPCサーバーへのメモリ内クライアントを作成することにより、
// 指定された接続を介してRPC要求を処理します。
func (sn *SimNode) ServeRPC(conn *websocket.Conn) error {
	handler, err := sn.node.RPCHandler()
	if err != nil {
		return err
	}
	codec := rpc.NewFuncCodec(conn, conn.WriteJSON, conn.ReadJSON)
	handler.ServeCodec(codec, 0)
	return nil
}

// Snapshots creates snapshots of the services by calling the
// simulation_snapshot RPC method
// スナップショットは、simulation_snapshotRPCメソッドを呼び出すことによって
// サービスのスナップショットを作成します
func (sn *SimNode) Snapshots() (map[string][]byte, error) {
	sn.lock.RLock()
	services := make(map[string]node.Lifecycle, len(sn.running))
	for name, service := range sn.running {
		services[name] = service
	}
	sn.lock.RUnlock()
	if len(services) == 0 {
		return nil, errors.New("no running services")
	}
	snapshots := make(map[string][]byte)
	for name, service := range services {
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

// Start registers the services and starts the underlying devp2p node
// Startはサービスを登録し、基盤となるdevp2pノードを開始します
func (sn *SimNode) Start(snapshots map[string][]byte) error {
	// ensure we only register the services once in the case of the node
	// being stopped and then started again
	// ノードが停止してから再開する場合は、サービスを1回だけ登録するようにしてください。
	var regErr error
	sn.registerOnce.Do(func() {
		for _, name := range sn.config.Lifecycles {
			ctx := &ServiceContext{
				RPCDialer: sn.adapter,
				Config:    sn.config,
			}
			if snapshots != nil {
				ctx.Snapshot = snapshots[name]
			}
			serviceFunc := sn.adapter.lifecycles[name]
			service, err := serviceFunc(ctx, sn.node)
			if err != nil {
				regErr = err
				break
			}
			// if the service has already been registered, don't register it again.
			// サービスがすでに登録されている場合は、再度登録しないでください。
			if _, ok := sn.running[name]; ok {
				continue
			}
			sn.running[name] = service
		}
	})
	if regErr != nil {
		return regErr
	}

	if err := sn.node.Start(); err != nil {
		return err
	}

	// create an in-process RPC client
	// インプロセスRPCクライアントを作成する
	client, err := sn.node.Attach()
	if err != nil {
		return err
	}
	sn.lock.Lock()
	sn.client = client
	sn.lock.Unlock()

	return nil
}

// Stop closes the RPC client and stops the underlying devp2p node
// Stopは、RPCクライアントを閉じ、基になるdevp2pノードを停止します
func (sn *SimNode) Stop() error {
	sn.lock.Lock()
	if sn.client != nil {
		sn.client.Close()
		sn.client = nil
	}
	sn.lock.Unlock()
	return sn.node.Close()
}

// Service returns a running service by name
// サービスは実行中のサービスを名前で返します
func (sn *SimNode) Service(name string) node.Lifecycle {
	sn.lock.RLock()
	defer sn.lock.RUnlock()
	return sn.running[name]
}

// Services returns a copy of the underlying services
// Servicesは、基になるサービスのコピーを返します
func (sn *SimNode) Services() []node.Lifecycle {
	sn.lock.RLock()
	defer sn.lock.RUnlock()
	services := make([]node.Lifecycle, 0, len(sn.running))
	for _, service := range sn.running {
		services = append(services, service)
	}
	return services
}

// ServiceMap returns a map by names of the underlying services
// ServiceMapは、基になるサービスの名前でマップを返します
func (sn *SimNode) ServiceMap() map[string]node.Lifecycle {
	sn.lock.RLock()
	defer sn.lock.RUnlock()
	services := make(map[string]node.Lifecycle, len(sn.running))
	for name, service := range sn.running {
		services[name] = service
	}
	return services
}

// Server returns the underlying p2p.Server
// サーバーは基盤となるp2p.Serverを返します
func (sn *SimNode) Server() *p2p.Server {
	return sn.node.Server()
}

// SubscribeEvents subscribes the given channel to peer events from the
// underlying p2p.Server
// SubscribeEventsは、基になるp2p.Serverからのピアイベントに指定されたチャネルをサブスクライブします
func (sn *SimNode) SubscribeEvents(ch chan *p2p.PeerEvent) event.Subscription {
	srv := sn.Server()
	if srv == nil {
		panic("node not running")
	}
	return srv.SubscribeEvents(ch)
}

// NodeInfo returns information about the node
// NodeInfoは、ノードに関する情報を返します
func (sn *SimNode) NodeInfo() *p2p.NodeInfo {
	server := sn.Server()
	if server == nil {
		return &p2p.NodeInfo{
			ID:    sn.ID.String(),
			Enode: sn.Node().String(),
		}
	}
	return server.NodeInfo()
}
