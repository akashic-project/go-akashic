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
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"

	"github.com/docker/docker/pkg/reexec"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
)

// Node represents a node in a simulation network which is created by a
// NodeAdapter, for example:
// Nodeは、NodeAdapterによって作成されたシミュレーションネットワーク内のノードを表します。次に例を示します。
//
// * SimNode    - An in-memory node
// * ExecNode   - A child process node
// * DockerNode - A Docker container node
// * SimNode    - メモリ内ノード
// * ExecNode   - 子プロセスノード
// * DockerNode - Dockerコンテナノード
//
type Node interface {
	// Addr returns the node's address (e.g. an Enode URL)
	// Addrは、ノードのアドレス（Enode URLなど）を返します。
	Addr() []byte

	// Client returns the RPC client which is created once the node is
	// up and running
	// クライアントは、ノードが起動して実行されると作成されるRPCクライアントを返します
	Client() (*rpc.Client, error)

	// ServeRPC serves RPC requests over the given connection
	// ServeRPCは、指定された接続を介してRPC要求を処理します
	ServeRPC(*websocket.Conn) error

	// Start starts the node with the given snapshots
	// Startは、指定されたスナップショットでノードを開始します
	Start(snapshots map[string][]byte) error

	// Stop stops the node
	// Stopはノードを停止します
	Stop() error

	// NodeInfo returns information about the node
	// NodeInfoは、ノードに関する情報を返します
	NodeInfo() *p2p.NodeInfo

	// Snapshots creates snapshots of the running services
	// スナップショットは、実行中のサービスのスナップショットを作成します
	Snapshots() (map[string][]byte, error)
}

// NodeAdapter is used to create Nodes in a simulation network
// NodeAdapterは、シミュレーションネットワークでノードを作成するために使用されます
type NodeAdapter interface {
	// Name returns the name of the adapter for logging purposes
	// Nameは、ロギングの目的でアダプターの名前を返します
	Name() string

	// NewNode creates a new node with the given configuration
	// NewNodeは、指定された構成で新しいノードを作成します
	NewNode(config *NodeConfig) (Node, error)
}

// NodeConfig is the configuration used to start a node in a simulation
// network
// NodeConfigは、シミュレーションネットワークでノードを開始するために使用される構成です。
type NodeConfig struct {
	// ID is the node's ID which is used to identify the node in the
	// simulation network
	// IDは、シミュレーションネットワークでノードを識別するために使用されるノードのIDです。
	ID enode.ID

	// PrivateKey is the node's private key which is used by the devp2p
	// stack to encrypt communications
	// PrivateKeyは、通信を暗号化するためにdevp2pスタックによって使用されるノードの秘密鍵です。
	PrivateKey *ecdsa.PrivateKey

	// Enable peer events for Msgs
	// メッセージのピアイベントを有効にする
	EnableMsgEvents bool

	// Name is a human friendly name for the node like "node01"
	// Nameは、「node01」のようなノードのわかりやすい名前です。
	Name string

	// Use an existing database instead of a temporary one if non-empty
	// 空でない場合は、一時的なデータベースではなく、既存のデータベースを使用します
	DataDir string

	// Lifecycles are the names of the service lifecycles which should be run when
	// starting the node (for SimNodes it should be the names of service lifecycles
	// contained in SimAdapter.lifecycles, for other nodes it should be
	// service lifecycles registered by calling the RegisterLifecycle function)
	// ライフサイクルは、ノードの起動時に実行する必要があるサービスライフサイクルの名前です
	//（SimNodeの場合は、SimAdapter.lifecyclesに含まれるサービスライフサイクルの名前である必要があります。
	// 他のノードの場合は、RegisterLifecycle関数を呼び出して登録するサービスライフサイクルである必要があります）。
	Lifecycles []string

	// Properties are the names of the properties this node should hold
	// within running services (e.g. "bootnode", "lightnode" or any custom values)
	// These values need to be checked and acted upon by node Services
	// プロパティは、このノードが実行中のサービス内で保持する必要があるプロパティの名前です
	// （たとえば、「bootnode」、「lightnode」、または任意のカスタム値）。
	// これらの値は、ノードサービスによってチェックおよび処理される必要があります。
	Properties []string

	// ExternalSigner specifies an external URI for a clef-type signer
	// ExternalSignerは、音部記号タイプの署名者の外部URIを指定します
	ExternalSigner string

	// Enode
	node *enode.Node

	// ENR Record with entries to overwrite
	// 上書きするエントリを含むENRレコード
	Record enr.Record

	// function to sanction or prevent suggesting a peer
	// 仲間を制裁または提案することを防ぐ機能
	Reachable func(id enode.ID) bool

	Port uint16

	// LogFile is the log file name of the p2p node at runtime.
	// LogFileは、実行時のp2pノードのログファイル名です。
	//
	// The default value is empty so that the default log writer
	// is the system standard output.
	// デフォルト値は空であるため、デフォルトのログライターはシステム標準出力です。
	LogFile string

	// LogVerbosity is the log verbosity of the p2p node at runtime.
	// LogVerbosityは、実行時のp2pノードのログの詳細度です。
	//
	// The default verbosity is INFO.
	// デフォルトの詳細度はINFOです。
	LogVerbosity log.Lvl
}

// nodeConfigJSON is used to encode and decode NodeConfig as JSON by encoding
// all fields as strings
// nodeConfigJSONは、すべてのフィールドを文字列としてエンコードすることにより、
// NodeConfigをJSONとしてエンコードおよびデコードするために使用されます
type nodeConfigJSON struct {
	ID              string   `json:"id"`
	PrivateKey      string   `json:"private_key"`
	Name            string   `json:"name"`
	Lifecycles      []string `json:"lifecycles"`
	Properties      []string `json:"properties"`
	EnableMsgEvents bool     `json:"enable_msg_events"`
	Port            uint16   `json:"port"`
	LogFile         string   `json:"logfile"`
	LogVerbosity    int      `json:"log_verbosity"`
}

// MarshalJSON implements the json.Marshaler interface by encoding the config
// fields as strings
// MarshalJSONは、構成フィールドを文字列としてエンコードすることにより、json.Marshalerインターフェースを実装します
func (n *NodeConfig) MarshalJSON() ([]byte, error) {
	confJSON := nodeConfigJSON{
		ID:              n.ID.String(),
		Name:            n.Name,
		Lifecycles:      n.Lifecycles,
		Properties:      n.Properties,
		Port:            n.Port,
		EnableMsgEvents: n.EnableMsgEvents,
		LogFile:         n.LogFile,
		LogVerbosity:    int(n.LogVerbosity),
	}
	if n.PrivateKey != nil {
		confJSON.PrivateKey = hex.EncodeToString(crypto.FromECDSA(n.PrivateKey))
	}
	return json.Marshal(confJSON)
}

// UnmarshalJSON implements the json.Unmarshaler interface by decoding the json
// string values into the config fields
// UnmarshalJSONは、json文字列値を構成フィールドにデコードすることでjson.Unmarshalerインターフェースを実装します
func (n *NodeConfig) UnmarshalJSON(data []byte) error {
	var confJSON nodeConfigJSON
	if err := json.Unmarshal(data, &confJSON); err != nil {
		return err
	}

	if confJSON.ID != "" {
		if err := n.ID.UnmarshalText([]byte(confJSON.ID)); err != nil {
			return err
		}
	}

	if confJSON.PrivateKey != "" {
		key, err := hex.DecodeString(confJSON.PrivateKey)
		if err != nil {
			return err
		}
		privKey, err := crypto.ToECDSA(key)
		if err != nil {
			return err
		}
		n.PrivateKey = privKey
	}

	n.Name = confJSON.Name
	n.Lifecycles = confJSON.Lifecycles
	n.Properties = confJSON.Properties
	n.Port = confJSON.Port
	n.EnableMsgEvents = confJSON.EnableMsgEvents
	n.LogFile = confJSON.LogFile
	n.LogVerbosity = log.Lvl(confJSON.LogVerbosity)

	return nil
}

// Node returns the node descriptor represented by the config.
// Nodeは、configで表されるノード記述子を返します。
func (n *NodeConfig) Node() *enode.Node {
	return n.node
}

// RandomNodeConfig returns node configuration with a randomly generated ID and
// PrivateKey
// RandomNodeConfigは、ランダムに生成されたIDとPrivateKeyを使用してノード構成を返します
func RandomNodeConfig() *NodeConfig {
	prvkey, err := crypto.GenerateKey()
	if err != nil {
		panic("unable to generate key")
	}

	port, err := assignTCPPort()
	if err != nil {
		panic("unable to assign tcp port")
	}

	enodId := enode.PubkeyToIDV4(&prvkey.PublicKey)
	return &NodeConfig{
		PrivateKey:      prvkey,
		ID:              enodId,
		Name:            fmt.Sprintf("node_%s", enodId.String()),
		Port:            port,
		EnableMsgEvents: true,
		LogVerbosity:    log.LvlInfo,
	}
}

func assignTCPPort() (uint16, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		return 0, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return 0, err
	}
	return uint16(p), nil
}

// ServiceContext is a collection of options and methods which can be utilised
// when starting services
// ServiceContextは、サービスを開始するときに利用できるオプションとメソッドのコレクションです。
type ServiceContext struct {
	RPCDialer

	Config   *NodeConfig
	Snapshot []byte
}

// RPCDialer is used when initialising services which need to connect to
// other nodes in the network (for example a simulated Swarm node which needs
// to connect to a Geth node to resolve ENS names)
// RPCDialerは、ネットワーク内の他のノードに接続する必要があるサービスを初期化するときに使用されます
//（たとえば、ENS名を解決するためにGethノードに接続する必要があるシミュレートされたSwarmノード）
type RPCDialer interface {
	DialRPC(id enode.ID) (*rpc.Client, error)
}

// LifecycleConstructor allows a Lifecycle to be constructed during node start-up.
// While the service-specific package usually takes care of Lifecycle creation and registration,
// for testing purposes, it is useful to be able to construct a Lifecycle on spot.
// LifecycleConstructorを使用すると、ノードの起動時にライフサイクルを構築できます。
// サービス固有のパッケージは通常、ライフサイクルの作成と登録を処理しますが、
// テストの目的で、ライフサイクルをその場で構築できると便利です。
type LifecycleConstructor func(ctx *ServiceContext, stack *node.Node) (node.Lifecycle, error)

// LifecycleConstructors stores LifecycleConstructor functions to call during node start-up.
// LifecycleConstructorsは、ノードの起動時に呼び出すLifecycleConstructor関数を格納します。
type LifecycleConstructors map[string]LifecycleConstructor

// lifecycleConstructorFuncs is a map of registered services which are used to boot devp2p
// nodes
// lifecycleConstructorFuncsは、devp2pノードの起動に使用される登録済みサービスのマップです。
var lifecycleConstructorFuncs = make(LifecycleConstructors)

// RegisterLifecycles registers the given Services which can then be used to
// start devp2p nodes using either the Exec or Docker adapters.
// RegisterLifecyclesは、ExecまたはDockerアダプターのいずれかを使用して
// devp2pノードを開始するために使用できる特定のサービスを登録します。
//
// It should be called in an init function so that it has the opportunity to
// execute the services before main() is called.
// main（）が呼び出される前にサービスを実行する機会があるように、init関数で呼び出す必要があります。
func RegisterLifecycles(lifecycles LifecycleConstructors) {
	for name, f := range lifecycles {
		if _, exists := lifecycleConstructorFuncs[name]; exists {
			panic(fmt.Sprintf("node service already exists: %q", name))
		}
		lifecycleConstructorFuncs[name] = f
	}

	// now we have registered the services, run reexec.Init() which will
	// potentially start one of the services if the current binary has
	// been exec'd with argv[0] set to "p2p-node"
	// これでサービスが登録されました。reexec.Init（）を実行すると、
	// 現在のバイナリがargv [0]を「p2p-node」に設定して実行された場合に、サービスの1つが開始される可能性があります。
	if reexec.Init() {
		os.Exit(0)
	}
}

// adds the host part to the configuration's ENR, signs it
// creates and  the corresponding enode object to the configuration
// ホスト部分を構成のENRに追加し、ホスト部分が作成する署名と、対応するenodeオブジェクトを構成に追加します
func (n *NodeConfig) initEnode(ip net.IP, tcpport int, udpport int) error {
	enrIp := enr.IP(ip)
	n.Record.Set(&enrIp)
	enrTcpPort := enr.TCP(tcpport)
	n.Record.Set(&enrTcpPort)
	enrUdpPort := enr.UDP(udpport)
	n.Record.Set(&enrUdpPort)

	err := enode.SignV4(&n.Record, n.PrivateKey)
	if err != nil {
		return fmt.Errorf("unable to generate ENR: %v", err)
	}
	nod, err := enode.New(enode.V4ID{}, &n.Record)
	if err != nil {
		return fmt.Errorf("unable to create enode: %v", err)
	}
	log.Trace("simnode new", "record", n.Record)
	n.node = nod
	return nil
}

func (n *NodeConfig) initDummyEnode() error {
	return n.initEnode(net.IPv4(127, 0, 0, 1), int(n.Port), 0)
}
