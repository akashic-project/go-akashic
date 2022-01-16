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

// Package p2p implements the Ethereum p2p network protocols.
// パッケージp2pは、Ethereump2pネットワークプロトコルを実装します。
package p2p

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/nat"
	"github.com/ethereum/go-ethereum/p2p/netutil"
)

const (
	defaultDialTimeout = 15 * time.Second

	// This is the fairness knob for the discovery mixer. When looking for peers, we'll
	// wait this long for a single source of candidates before moving on and trying other
	// sources.
	// これは、ディスカバリーミキサーの公平性ノブです。
	// 仲間を探すときは、候補者の単一のソースをこれだけ長く待ってから、他のソースを試します。
	discmixTimeout = 5 * time.Second

	// Connectivity defaults.
	// 接続のデフォルト。
	defaultMaxPendingPeers = 50
	defaultDialRatio       = 3

	// This time limits inbound connection attempts per source IP.
	// この時間は、送信元IPごとのインバウンド接続の試行を制限します。
	inboundThrottleTime = 30 * time.Second

	// Maximum time allowed for reading a complete message.
	// This is effectively the amount of time a connection can be idle.
	// 完全なメッセージを読むために許可される最大時間。
	// これは事実上、接続がアイドル状態になることができる時間です。
	frameReadTimeout = 30 * time.Second

	// Maximum amount of time allowed for writing a complete message.
	// 完全なメッセージを書き込むために許可される最大時間。
	frameWriteTimeout = 20 * time.Second
)

var errServerStopped = errors.New("server stopped")

// Config holds Server options.
// Configはサーバーオプションを保持します。
type Config struct {
	// This field must be set to a valid secp256k1 private key.
	// このフィールドは、有効なsecp256k1秘密鍵に設定する必要があります。
	PrivateKey *ecdsa.PrivateKey `toml:"-"`

	// MaxPeers is the maximum number of peers that can be
	// connected. It must be greater than zero.
	// MaxPeersは、接続できるピアの最大数です。ゼロより大きくなければなりません。
	MaxPeers int

	// MaxPendingPeers is the maximum number of peers that can be pending in the
	// handshake phase, counted separately for inbound and outbound connections.
	// Zero defaults to preset values.
	// MaxPendingPeersは、ハンドシェイクフェーズで保留できるピアの最大数であり、
	// インバウンド接続とアウトバウンド接続で別々にカウントされます。
	// ゼロのデフォルトはプリセット値です。
	MaxPendingPeers int `toml:",omitempty"`

	// DialRatio controls the ratio of inbound to dialed connections.
	// Example: a DialRatio of 2 allows 1/2 of connections to be dialed.
	// Setting DialRatio to zero defaults it to 3.
	// DialRatioは、ダイヤル接続に対するインバウンド接続の比率を制御します。
	// 例：DialRatioが2の場合、接続の1/2をダイヤルできます。
	// DialRatioをゼロに設定すると、デフォルトで3になります。
	DialRatio int `toml:",omitempty"`

	// NoDiscovery can be used to disable the peer discovery mechanism.
	// Disabling is useful for protocol debugging (manual topology).
	// NoDiscoveryを使用して、ピア検出メカニズムを無効にすることができます。
	// 無効化は、プロトコルのデバッグ（手動トポロジ）に役立ちます。
	NoDiscovery bool

	// DiscoveryV5 specifies whether the new topic-discovery based V5 discovery
	// protocol should be started or not.
	// DiscoveryV5は、新しいトピック検出ベースのV5検出プロトコルを開始するかどうかを指定します。
	DiscoveryV5 bool `toml:",omitempty"`

	// Name sets the node name of this server.
	// Use common.MakeName to create a name that follows existing conventions.
	// Nameは、このサーバーのノード名を設定します。
	// common.MakeNameを使用して、既存の規則に従う名前を作成します。
	Name string `toml:"-"`

	// BootstrapNodes are used to establish connectivity
	// with the rest of the network.
	// BootstrapNodesは、ネットワークの他の部分との接続を確立するために使用されます。
	BootstrapNodes []*enode.Node

	// BootstrapNodesV5 are used to establish connectivity
	// with the rest of the network using the V5 discovery
	// protocol.
	// BootstrapNodesV5は、V5検出プロトコルを使用してネットワークの他の部分との接続を確立するために使用されます。
	BootstrapNodesV5 []*enode.Node `toml:",omitempty"`

	// Static nodes are used as pre-configured connections which are always
	// maintained and re-connected on disconnects.
	// 静的ノードは事前設定された接続として使用され、切断時に常に維持および再接続されます。
	StaticNodes []*enode.Node

	// Trusted nodes are used as pre-configured connections which are always
	// allowed to connect, even above the peer limit.
	// 信頼できるノードは、ピアの制限を超えても常に接続が許可される事前構成済みの接続として使用されます。
	TrustedNodes []*enode.Node

	// Connectivity can be restricted to certain IP networks.
	// If this option is set to a non-nil value, only hosts which match one of the
	// IP networks contained in the list are considered.
	// 接続は特定のIPネットワークに制限できます。
	// このオプションがnil以外の値に設定されている場合、
	// リストに含まれているIPネットワークの1つに一致するホストのみが考慮されます。
	NetRestrict *netutil.Netlist `toml:",omitempty"`

	// NodeDatabase is the path to the database containing the previously seen
	// live nodes in the network.
	// NodeDatabaseは、ネットワーク内で以前に確認されたライブノードを含むデータベースへのパスです。
	NodeDatabase string `toml:",omitempty"`

	// Protocols should contain the protocols supported
	// by the server. Matching protocols are launched for
	// each peer.
	// プロトコルには、サーバーでサポートされているプロトコルが含まれている必要があります。
	// マッチングプロトコルは、ピアごとに起動されます。
	Protocols []Protocol `toml:"-"`

	// If ListenAddr is set to a non-nil address, the server
	// will listen for incoming connections.
	// ListenAddrがnil以外のアドレスに設定されている場合、サーバーは着信接続をリッスンします。
	//
	// If the port is zero, the operating system will pick a port. The
	// ListenAddr field will be updated with the actual address when
	// the server is started.
	// ポートがゼロの場合、オペレーティングシステムはポートを選択します。
	// ListenAddrフィールドは、サーバーの起動時に実際のアドレスで更新されます。
	ListenAddr string

	// If set to a non-nil value, the given NAT port mapper
	// is used to make the listening port available to the
	// Internet.
	// nil以外の値に設定すると、指定されたNATポートマッパーを使用して、
	// リスニングポートをインターネットで使用できるようにします。
	NAT nat.Interface `toml:",omitempty"`

	// If Dialer is set to a non-nil value, the given Dialer
	// is used to dial outbound peer connections.
	// Dialerがnil以外の値に設定されている場合、
	// 指定されたDialerがアウトバウンドピア接続のダイヤルに使用されます。
	Dialer NodeDialer `toml:"-"`

	// If NoDial is true, the server will not dial any peers.
	// NoDialがtrueの場合、サーバーはピアにダイヤルしません。
	NoDial bool `toml:",omitempty"`

	// If EnableMsgEvents is set then the server will emit PeerEvents
	// whenever a message is sent to or received from a peer
	// EnableMsgEventsが設定されている場合、
	// メッセージがピアとの間で送受信されるたびに、サーバーはPeerEventsを発行します。
	EnableMsgEvents bool

	// Logger is a custom logger to use with the p2p.Server.
	// ロガーは、p2p.Serverで使用するカスタムロガーです。
	Logger log.Logger `toml:",omitempty"`

	clock mclock.Clock
}

// Server manages all peer connections.
// サーバーはすべてのピア接続を管理します。
type Server struct {
	// Config fields may not be modified while the server is running.
	// サーバーの実行中は、構成フィールドを変更できません。
	Config // サーバーオプション

	// Hooks for testing. These are useful because we can inhibit
	// the whole protocol stack.
	// テスト用のフック。これらは、プロトコルスタック全体を禁止できるので便利です。
	newTransport func(net.Conn, *ecdsa.PublicKey) transport
	newPeerHook  func(*Peer)
	listenFunc   func(network, addr string) (net.Listener, error)

	lock    sync.Mutex // protects running  ランニングを保護します
	running bool

	listener     net.Listener
	ourHandshake *protoHandshake
	loopWG       sync.WaitGroup // loop, listenLoop
	peerFeed     event.Feed
	log          log.Logger

	nodedb    *enode.DB
	localnode *enode.LocalNode
	ntab      *discover.UDPv4
	DiscV5    *discover.UDPv5
	discmix   *enode.FairMix
	dialsched *dialScheduler

	// Channels into the run loop.
	// 実行ループにチャネルします。
	quit                    chan struct{}
	addtrusted              chan *enode.Node
	removetrusted           chan *enode.Node
	peerOp                  chan peerOpFunc
	peerOpDone              chan struct{}
	delpeer                 chan peerDrop
	checkpointPostHandshake chan *conn
	checkpointAddPeer       chan *conn

	// State of run loop and listenLoop.
	// 実行ループとlistenLoopの状態。
	inboundHistory expHeap
}

type peerOpFunc func(map[enode.ID]*Peer)

type peerDrop struct {
	*Peer
	err       error
	requested bool // true if signaled by the peer  ピアから通知された場合はtrue
}

type connFlag int32

const (
	dynDialedConn connFlag = 1 << iota
	staticDialedConn
	inboundConn
	trustedConn
)

// conn wraps a network connection with information gathered
// during the two handshakes.
// connは、2回のハンドシェイク中に収集された情報でネットワーク接続をラップします。
type conn struct {
	fd net.Conn
	transport
	node  *enode.Node
	flags connFlag
	cont  chan error // The run loop uses cont to signal errors to SetupConn. 実行ループはcontを使用して、SetupConnにエラーを通知します。
	caps  []Cap      // valid after the protocol handshake プロトコルハンドシェイク後に有効
	name  string     // valid after the protocol handshake プロトコルハンドシェイク後に有効
}

type transport interface {
	// The two handshakes.
	// 2つの握手。
	doEncHandshake(prv *ecdsa.PrivateKey) (*ecdsa.PublicKey, error)
	doProtoHandshake(our *protoHandshake) (*protoHandshake, error)
	// The MsgReadWriter can only be used after the encryption
	// handshake has completed. The code uses conn.id to track this
	// by setting it to a non-nil value after the encryption handshake.
	// MsgReadWriterは、暗号化ハンドシェイクが完了した後にのみ使用できます。
	// コードはconn.idを使用して、
	// 暗号化ハンドシェイク後にnil以外の値に設定することにより、これを追跡します。
	MsgReadWriter
	// transports must provide Close because we use MsgPipe in some of
	// the tests. Closing the actual network connection doesn't do
	// anything in those tests because MsgPipe doesn't use it.
	//
	//一部のテストではMsgPipeを使用するため、トランスポートはCloseを提供する必要があります。
	//MsgPipeはそれを使用しないため、
	//実際のネットワーク接続を閉じても、これらのテストでは何も行われません。
	close(err error)
}

func (c *conn) String() string {
	s := c.flags.String()
	if (c.node.ID() != enode.ID{}) {
		s += " " + c.node.ID().String()
	}
	s += " " + c.fd.RemoteAddr().String()
	return s
}

func (f connFlag) String() string {
	s := ""
	if f&trustedConn != 0 {
		s += "-trusted"
	}
	if f&dynDialedConn != 0 {
		s += "-dyndial"
	}
	if f&staticDialedConn != 0 {
		s += "-staticdial"
	}
	if f&inboundConn != 0 {
		s += "-inbound"
	}
	if s != "" {
		s = s[1:]
	}
	return s
}

func (c *conn) is(f connFlag) bool {
	flags := connFlag(atomic.LoadInt32((*int32)(&c.flags)))
	return flags&f != 0
}

func (c *conn) set(f connFlag, val bool) {
	for {
		oldFlags := connFlag(atomic.LoadInt32((*int32)(&c.flags)))
		flags := oldFlags
		if val {
			flags |= f
		} else {
			flags &= ^f
		}
		if atomic.CompareAndSwapInt32((*int32)(&c.flags), int32(oldFlags), int32(flags)) {
			return
		}
	}
}

// LocalNode returns the local node record.
// LocalNodeは、ローカルノードレコードを返します。
func (srv *Server) LocalNode() *enode.LocalNode {
	return srv.localnode
}

// Peers returns all connected peers.
// ピアは、接続されているすべてのピアを返します。
func (srv *Server) Peers() []*Peer {
	var ps []*Peer
	srv.doPeerOp(func(peers map[enode.ID]*Peer) {
		for _, p := range peers {
			ps = append(ps, p)
		}
	})
	return ps
}

// PeerCount returns the number of connected peers.
// PeerCountは、接続されているピアの数を返します。
func (srv *Server) PeerCount() int {
	var count int
	srv.doPeerOp(func(ps map[enode.ID]*Peer) {
		count = len(ps)
	})
	return count
}

// AddPeer adds the given node to the static node set. When there is room in the peer set,
// the server will connect to the node. If the connection fails for any reason, the server
// will attempt to reconnect the peer.
// AddPeerは、指定されたノードを静的ノードセットに追加します。
// ピアセットに空きがある場合、サーバーはノードに接続します。
// 何らかの理由で接続が失敗した場合、サーバーはピアへの再接続を試みます。
func (srv *Server) AddPeer(node *enode.Node) {
	srv.dialsched.addStatic(node)
}

// RemovePeer removes a node from the static node set. It also disconnects from the given
// node if it is currently connected as a peer.
// RemovePeerは、静的ノードセットからノードを削除します。
// また、現在ピアとして接続されている場合は、指定されたノードから切断されます。
//
// This method blocks until all protocols have exited and the peer is removed. Do not use
// RemovePeer in protocol implementations, call Disconnect on the Peer instead.
// このメソッドは、すべてのプロトコルが終了してピアが削除されるまでブロックします。
// プロトコルの実装ではRemovePeerを使用せず、代わりにPeerでDisconnectを呼び出します。
//
func (srv *Server) RemovePeer(node *enode.Node) {
	var (
		ch  chan *PeerEvent
		sub event.Subscription
	)
	// Disconnect the peer on the main loop.
	// メインループのピアを切断します。
	srv.doPeerOp(func(peers map[enode.ID]*Peer) {
		srv.dialsched.removeStatic(node)
		if peer := peers[node.ID()]; peer != nil {
			ch = make(chan *PeerEvent, 1)
			sub = srv.peerFeed.Subscribe(ch)
			peer.Disconnect(DiscRequested)
		}
	})
	// Wait for the peer connection to end.
	// ピア接続が終了するのを待ちます。
	if ch != nil {
		defer sub.Unsubscribe()
		for ev := range ch {
			if ev.Peer == node.ID() && ev.Type == PeerEventTypeDrop {
				return
			}
		}
	}
}

// AddTrustedPeer adds the given node to a reserved trusted list which allows the
// node to always connect, even if the slot are full.
//
// AddTrustedPeerは、指定されたノードを予約済みの信頼できるリストに追加します。
// これにより、スロットがいっぱいの場合でも、ノードは常に接続できます。
func (srv *Server) AddTrustedPeer(node *enode.Node) {
	select {
	case srv.addtrusted <- node:
	case <-srv.quit:
	}
}

// RemoveTrustedPeer removes the given node from the trusted peer set.
// RemoveTrustedPeerは、指定されたノードを信頼できるピアセットから削除します。
func (srv *Server) RemoveTrustedPeer(node *enode.Node) {
	select {
	case srv.removetrusted <- node:
	case <-srv.quit:
	}
}

// SubscribeEvents subscribes the given channel to peer events
// SubscribeEventsは、指定されたチャネルをピアイベントにサブスクライブします
func (srv *Server) SubscribeEvents(ch chan *PeerEvent) event.Subscription {
	return srv.peerFeed.Subscribe(ch)
}

// Self returns the local node's endpoint information.
// Selfは、ローカルノードのエンドポイント情報を返します。
func (srv *Server) Self() *enode.Node {
	srv.lock.Lock()
	ln := srv.localnode
	srv.lock.Unlock()

	if ln == nil {
		return enode.NewV4(&srv.PrivateKey.PublicKey, net.ParseIP("0.0.0.0"), 0, 0)
	}
	return ln.Node()
}

// Stop terminates the server and all active peer connections.
// It blocks until all active connections have been closed.
// Stopは、サーバーとすべてのアクティブなピア接続を終了します。
// すべてのアクティブな接続が閉じられるまでブロックします。
func (srv *Server) Stop() {
	srv.lock.Lock()
	if !srv.running {
		srv.lock.Unlock()
		return
	}
	srv.running = false
	if srv.listener != nil {
		// this unblocks listener Accept
		// これにより、リスナーのブロックが解除されますAccept
		srv.listener.Close()
	}
	close(srv.quit)
	srv.lock.Unlock()
	srv.loopWG.Wait()
}

// sharedUDPConn implements a shared connection. Write sends messages to the underlying connection while read returns
// messages that were found unprocessable and sent to the unhandled channel by the primary listener.
// sharedUDPConnは、共有接続を実装します。
// 書き込みは基になる接続にメッセージを送信し、読み取りは処理不能であることが判明し、
// プライマリリスナーによって未処理のチャネルに送信されたメッセージを返します。
type sharedUDPConn struct {
	*net.UDPConn
	unhandled chan discover.ReadPacket
}

// ReadFromUDP implements discover.UDPConn
// ReadFromUDPはdiscover.UDPConnを実装します
func (s *sharedUDPConn) ReadFromUDP(b []byte) (n int, addr *net.UDPAddr, err error) {
	packet, ok := <-s.unhandled
	if !ok {
		return 0, nil, errors.New("connection was closed")
	}
	l := len(packet.Data)
	if l > len(b) {
		l = len(b)
	}
	copy(b[:l], packet.Data[:l])
	return l, packet.Addr, nil
}

// Close implements discover.UDPConn
// Closeはdiscover.UDPConnを実装します
func (s *sharedUDPConn) Close() error {
	return nil
}

// Start starts running the server.
// Servers can not be re-used after stopping.
// スタートはサーバーの実行を開始します。
// 停止後、サーバーを再利用することはできません。
func (srv *Server) Start() (err error) {
	srv.lock.Lock()
	defer srv.lock.Unlock()
	if srv.running {
		return errors.New("server already running")
	}
	srv.running = true
	srv.log = srv.Config.Logger
	if srv.log == nil {
		srv.log = log.Root()
	}
	if srv.clock == nil {
		srv.clock = mclock.System{}
	}
	if srv.NoDial && srv.ListenAddr == "" {
		srv.log.Warn("P2P server will be useless, neither dialing nor listening") // P2Pサーバーは役に立たず、ダイヤルもリスニングもしません
	}

	// static fields
	// 静的フィールド
	if srv.PrivateKey == nil {
		return errors.New("Server.PrivateKey must be set to a non-nil key") // Server.PrivateKeyはnil以外のキーに設定する必要があります
	}
	if srv.newTransport == nil {
		srv.newTransport = newRLPX
	}
	if srv.listenFunc == nil {
		srv.listenFunc = net.Listen
	}
	srv.quit = make(chan struct{})
	srv.delpeer = make(chan peerDrop)
	srv.checkpointPostHandshake = make(chan *conn)
	srv.checkpointAddPeer = make(chan *conn)
	srv.addtrusted = make(chan *enode.Node)
	srv.removetrusted = make(chan *enode.Node)
	srv.peerOp = make(chan peerOpFunc)
	srv.peerOpDone = make(chan struct{})

	if err := srv.setupLocalNode(); err != nil {
		return err
	}
	if srv.ListenAddr != "" {
		if err := srv.setupListening(); err != nil {
			return err
		}
	}
	if err := srv.setupDiscovery(); err != nil {
		return err
	}
	srv.setupDialScheduler()

	srv.loopWG.Add(1)
	go srv.run()
	return nil
}

func (srv *Server) setupLocalNode() error {
	// Create the devp2p handshake.
	// devp2pハンドシェイクを作成します
	pubkey := crypto.FromECDSAPub(&srv.PrivateKey.PublicKey)
	srv.ourHandshake = &protoHandshake{Version: baseProtocolVersion, Name: srv.Name, ID: pubkey[1:]}
	for _, p := range srv.Protocols {
		srv.ourHandshake.Caps = append(srv.ourHandshake.Caps, p.cap())
	}
	sort.Sort(capsByNameAndVersion(srv.ourHandshake.Caps))

	// Create the local node.
	// ローカルノードを作成します。
	db, err := enode.OpenDB(srv.Config.NodeDatabase)
	if err != nil {
		return err
	}
	srv.nodedb = db
	srv.localnode = enode.NewLocalNode(db, srv.PrivateKey)
	srv.localnode.SetFallbackIP(net.IP{127, 0, 0, 1})
	// TODO: check conflicts
	// TODO：競合を確認する
	for _, p := range srv.Protocols {
		for _, e := range p.Attributes {
			srv.localnode.Set(e)
		}
	}
	switch srv.NAT.(type) {
	case nil:
		// No NAT interface, do nothing.
		// NATインターフェースはなく、何もしません。
	case nat.ExtIP:
		// ExtIP doesn't block, set the IP right away.
		// ExtIPはブロックしません。すぐにIPを設定してください。
		ip, _ := srv.NAT.ExternalIP()
		srv.localnode.SetStaticIP(ip)
	default:
		// Ask the router about the IP. This takes a while and blocks startup,
		// do it in the background.
		// IPについてルーターに問い合わせます。
		// これには時間がかかり、起動がブロックされます。バックグラウンドで実行してください
		srv.loopWG.Add(1)
		go func() {
			defer srv.loopWG.Done()
			if ip, err := srv.NAT.ExternalIP(); err == nil {
				srv.localnode.SetStaticIP(ip)
			}
		}()
	}
	return nil
}

func (srv *Server) setupDiscovery() error {
	srv.discmix = enode.NewFairMix(discmixTimeout)

	// Add protocol-specific discovery sources.
	// プロトコル固有の検出ソースを追加します。
	added := make(map[string]bool)
	for _, proto := range srv.Protocols {
		if proto.DialCandidates != nil && !added[proto.Name] {
			srv.discmix.AddSource(proto.DialCandidates)
			added[proto.Name] = true
		}
	}

	// Don't listen on UDP endpoint if DHT is disabled.
	// DHTが無効になっている場合は、UDPエンドポイントでリッスンしないでください。
	if srv.NoDiscovery && !srv.DiscoveryV5 {
		return nil
	}

	addr, err := net.ResolveUDPAddr("udp", srv.ListenAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	realaddr := conn.LocalAddr().(*net.UDPAddr)
	srv.log.Debug("UDP listener up", "addr", realaddr)
	if srv.NAT != nil {
		if !realaddr.IP.IsLoopback() {
			srv.loopWG.Add(1)
			go func() {
				nat.Map(srv.NAT, srv.quit, "udp", realaddr.Port, realaddr.Port, "akashic discovery")
				srv.loopWG.Done()
			}()
		}
	}
	srv.localnode.SetFallbackUDP(realaddr.Port)

	// Discovery V4
	// ディスカバリーV4
	var unhandled chan discover.ReadPacket
	var sconn *sharedUDPConn
	if !srv.NoDiscovery {
		if srv.DiscoveryV5 {
			unhandled = make(chan discover.ReadPacket, 100)
			sconn = &sharedUDPConn{conn, unhandled}
		}
		cfg := discover.Config{
			PrivateKey:  srv.PrivateKey,
			NetRestrict: srv.NetRestrict,
			Bootnodes:   srv.BootstrapNodes,
			Unhandled:   unhandled,
			Log:         srv.log,
		}
		ntab, err := discover.ListenV4(conn, srv.localnode, cfg)
		if err != nil {
			return err
		}
		srv.ntab = ntab
		srv.discmix.AddSource(ntab.RandomNodes())
	}

	// Discovery V5
	if srv.DiscoveryV5 {
		cfg := discover.Config{
			PrivateKey:  srv.PrivateKey,
			NetRestrict: srv.NetRestrict,
			Bootnodes:   srv.BootstrapNodesV5,
			Log:         srv.log,
		}
		var err error
		if sconn != nil {
			srv.DiscV5, err = discover.ListenV5(sconn, srv.localnode, cfg)
		} else {
			srv.DiscV5, err = discover.ListenV5(conn, srv.localnode, cfg)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (srv *Server) setupDialScheduler() {
	config := dialConfig{
		self:           srv.localnode.ID(),
		maxDialPeers:   srv.maxDialedConns(),
		maxActiveDials: srv.MaxPendingPeers,
		log:            srv.Logger,
		netRestrict:    srv.NetRestrict,
		dialer:         srv.Dialer,
		clock:          srv.clock,
	}
	if srv.ntab != nil {
		config.resolver = srv.ntab
	}
	if config.dialer == nil {
		config.dialer = tcpDialer{&net.Dialer{Timeout: defaultDialTimeout}}
	}
	srv.dialsched = newDialScheduler(config, srv.discmix, srv.SetupConn)
	for _, n := range srv.StaticNodes {
		srv.dialsched.addStatic(n)
	}
}

func (srv *Server) maxInboundConns() int {
	return srv.MaxPeers - srv.maxDialedConns()
}

func (srv *Server) maxDialedConns() (limit int) {
	if srv.NoDial || srv.MaxPeers == 0 {
		return 0
	}
	if srv.DialRatio == 0 {
		limit = srv.MaxPeers / defaultDialRatio
	} else {
		limit = srv.MaxPeers / srv.DialRatio
	}
	if limit == 0 {
		limit = 1
	}
	return limit
}

func (srv *Server) setupListening() error {
	// Launch the listener.
	// リスナーを起動します。
	listener, err := srv.listenFunc("tcp", srv.ListenAddr)
	if err != nil {
		return err
	}
	srv.listener = listener
	srv.ListenAddr = listener.Addr().String()

	// Update the local node record and map the TCP listening port if NAT is configured.
	// NATが構成されている場合は、ローカルノードレコードを更新し、TCPリスニングポートをマップします。
	if tcp, ok := listener.Addr().(*net.TCPAddr); ok {
		srv.localnode.Set(enr.TCP(tcp.Port))
		if !tcp.IP.IsLoopback() && srv.NAT != nil {
			srv.loopWG.Add(1)
			go func() {
				nat.Map(srv.NAT, srv.quit, "tcp", tcp.Port, tcp.Port, "akashic p2p")
				srv.loopWG.Done()
			}()
		}
	}

	srv.loopWG.Add(1)
	go srv.listenLoop()
	return nil
}

// doPeerOp runs fn on the main loop.
// doPeerOpはメインループでfnを実行します
func (srv *Server) doPeerOp(fn peerOpFunc) {
	select {
	case srv.peerOp <- fn:
		<-srv.peerOpDone
	case <-srv.quit:
	}
}

// run is the main loop of the server.
// runはサーバーのメインループです。
func (srv *Server) run() {
	srv.log.Info("Started P2P networking", "self", srv.localnode.Node().URLv4())
	defer srv.loopWG.Done()
	defer srv.nodedb.Close()
	defer srv.discmix.Close()
	defer srv.dialsched.stop()

	var (
		peers        = make(map[enode.ID]*Peer)
		inboundCount = 0
		trusted      = make(map[enode.ID]bool, len(srv.TrustedNodes))
	)
	// Put trusted nodes into a map to speed up checks.
	// Trusted peers are loaded on startup or added via AddTrustedPeer RPC.
	// 信頼できるノードをマップに配置して、チェックを高速化します。
	// 信頼できるピアは、起動時にロードされるか、AddTrustedPeerRPCを介して追加されます。
	for _, n := range srv.TrustedNodes {
		trusted[n.ID()] = true
	}

running:
	for {
		select {
		case <-srv.quit:
			// The server was stopped. Run the cleanup logic.
			// サーバーが停止しました。クリーンアップロジックを実行します。
			break running

		case n := <-srv.addtrusted:
			// This channel is used by AddTrustedPeer to add a node
			// to the trusted node set.
			// このチャネルは、信頼できるノードセットにノードを追加するためにAddTrustedPeerによって使用されます。
			srv.log.Trace("Adding trusted node", "node", n)
			trusted[n.ID()] = true
			if p, ok := peers[n.ID()]; ok {
				p.rw.set(trustedConn, true)
			}

		case n := <-srv.removetrusted:
			// This channel is used by RemoveTrustedPeer to remove a node
			// from the trusted node set.
			// このチャネルは、信頼できるノードセットからノードを削除するためにRemoveTrustedPeerによって使用されます。
			srv.log.Trace("Removing trusted node", "node", n)
			delete(trusted, n.ID())
			if p, ok := peers[n.ID()]; ok {
				p.rw.set(trustedConn, false)
			}

		case op := <-srv.peerOp:
			// This channel is used by Peers and PeerCount.
			// このチャネルは、PeersとPeerCountによって使用されます。
			op(peers)
			srv.peerOpDone <- struct{}{}

		case c := <-srv.checkpointPostHandshake:
			// A connection has passed the encryption handshake so
			// the remote identity is known (but hasn't been verified yet).
			// 接続が暗号化ハンドシェイクを通過したため、リモートIDが認識されます
			//（ただし、まだ検証されていません）。
			if trusted[c.node.ID()] {
				// Ensure that the trusted flag is set before checking against MaxPeers.
				// MaxPeersと照合する前に、信頼できるフラグが設定されていることを確認してください。
				c.flags |= trustedConn
			}
			// TODO: track in-progress inbound node IDs (pre-Peer) to avoid dialing them.
			// TODO：進行中のインバウンドノードID（プレピア）を追跡して、ダイヤルを回避します。
			c.cont <- srv.postHandshakeChecks(peers, inboundCount, c)

		case c := <-srv.checkpointAddPeer:
			// At this point the connection is past the protocol handshake.
			// Its capabilities are known and the remote identity is verified.
			// この時点で、接続はプロトコルハンドシェイクを通過しています。
			// その機能は既知であり、リモートIDが検証されます。
			err := srv.addPeerChecks(peers, inboundCount, c)
			if err == nil {
				// The handshakes are done and it passed all checks.
				// ハンドシェイクが実行され、すべてのチェックに合格しました。
				p := srv.launchPeer(c)
				peers[c.node.ID()] = p
				srv.log.Debug("Adding p2p peer", "peercount", len(peers), "id", p.ID(), "conn", c.flags, "addr", p.RemoteAddr(), "name", p.Name())
				srv.dialsched.peerAdded(c)
				if p.Inbound() {
					inboundCount++
				}
			}
			c.cont <- err

		case pd := <-srv.delpeer:
			// A peer disconnected.
			// ピアが切断されました。
			d := common.PrettyDuration(mclock.Now() - pd.created)
			delete(peers, pd.ID())
			srv.log.Debug("Removing p2p peer", "peercount", len(peers), "id", pd.ID(), "duration", d, "req", pd.requested, "err", pd.err)
			srv.dialsched.peerRemoved(pd.rw)
			if pd.Inbound() {
				inboundCount--
			}
		}
	}

	srv.log.Trace("P2P networking is spinning down")

	// Terminate discovery. If there is a running lookup it will terminate soon.
	// 検出を終了します。実行中のルックアップがある場合は、すぐに終了します。
	if srv.ntab != nil {
		srv.ntab.Close()
	}
	if srv.DiscV5 != nil {
		srv.DiscV5.Close()
	}
	// Disconnect all peers.
	// すべてのピアを切断します。
	for _, p := range peers {
		p.Disconnect(DiscQuitting)
	}
	// Wait for peers to shut down. Pending connections and tasks are
	// not handled here and will terminate soon-ish because srv.quit
	// is closed.
	// ピアがシャットダウンするのを待ちます。
	// 保留中の接続とタスクはここでは処理されず、srv.quitが閉じているため、すぐに終了します。
	for len(peers) > 0 {
		p := <-srv.delpeer
		p.log.Trace("<-delpeer (spindown)")
		delete(peers, p.ID())
	}
}

func (srv *Server) postHandshakeChecks(peers map[enode.ID]*Peer, inboundCount int, c *conn) error {
	switch {
	case !c.is(trustedConn) && len(peers) >= srv.MaxPeers:
		return DiscTooManyPeers
	case !c.is(trustedConn) && c.is(inboundConn) && inboundCount >= srv.maxInboundConns():
		return DiscTooManyPeers
	case peers[c.node.ID()] != nil:
		return DiscAlreadyConnected
	case c.node.ID() == srv.localnode.ID():
		return DiscSelf
	default:
		return nil
	}
}

func (srv *Server) addPeerChecks(peers map[enode.ID]*Peer, inboundCount int, c *conn) error {
	// Drop connections with no matching protocols.
	// 一致するプロトコルのない接続をドロップします。
	if len(srv.Protocols) > 0 && countMatchingProtocols(srv.Protocols, c.caps) == 0 {
		return DiscUselessPeer
	}
	// Repeat the post-handshake checks because the
	// peer set might have changed since those checks were performed.
	// これらのチェックが実行されてからピアセットが変更された可能性があるため、
	// ハンドシェイク後のチェックを繰り返します。
	return srv.postHandshakeChecks(peers, inboundCount, c)
}

// listenLoop runs in its own goroutine and accepts
// inbound connections.
// listenLoopは独自のゴルーチンで実行され、インバウンド接続を受け入れます。
func (srv *Server) listenLoop() {
	srv.log.Debug("TCP listener up", "addr", srv.listener.Addr())

	// The slots channel limits accepts of new connections.
	// スロットチャネル制限は、新しい接続を受け入れます。
	tokens := defaultMaxPendingPeers
	if srv.MaxPendingPeers > 0 {
		tokens = srv.MaxPendingPeers
	}
	slots := make(chan struct{}, tokens)
	for i := 0; i < tokens; i++ {
		slots <- struct{}{}
	}

	// Wait for slots to be returned on exit. This ensures all connection goroutines
	// are down before listenLoop returns.
	// 終了時にスロットが返されるのを待ちます。
	// これにより、listenLoopが戻る前に、すべての接続ゴルーチンがダウンしていることが保証されます。
	defer srv.loopWG.Done()
	defer func() {
		for i := 0; i < cap(slots); i++ {
			<-slots
		}
	}()

	for {
		// Wait for a free slot before accepting.
		// 受け入れる前に空きスロットを待ちます。
		<-slots

		var (
			fd      net.Conn
			err     error
			lastLog time.Time
		)
		for {
			fd, err = srv.listener.Accept()
			if netutil.IsTemporaryError(err) {
				if time.Since(lastLog) > 1*time.Second {
					srv.log.Debug("Temporary read error", "err", err)
					lastLog = time.Now()
				}
				time.Sleep(time.Millisecond * 200)
				continue
			} else if err != nil {
				srv.log.Debug("Read error", "err", err)
				slots <- struct{}{}
				return
			}
			break
		}

		remoteIP := netutil.AddrIP(fd.RemoteAddr())
		if err := srv.checkInboundConn(remoteIP); err != nil {
			srv.log.Debug("Rejected inbound connection", "addr", fd.RemoteAddr(), "err", err)
			fd.Close()
			slots <- struct{}{}
			continue
		}
		if remoteIP != nil {
			var addr *net.TCPAddr
			if tcp, ok := fd.RemoteAddr().(*net.TCPAddr); ok {
				addr = tcp
			}
			fd = newMeteredConn(fd, true, addr)
			srv.log.Trace("Accepted connection", "addr", fd.RemoteAddr())
		}
		go func() {
			srv.SetupConn(fd, inboundConn, nil)
			slots <- struct{}{}
		}()
	}
}

func (srv *Server) checkInboundConn(remoteIP net.IP) error {
	if remoteIP == nil {
		return nil
	}
	// Reject connections that do not match NetRestrict.
	// NetRestrictと一致しない接続を拒否します。
	if srv.NetRestrict != nil && !srv.NetRestrict.Contains(remoteIP) {
		return fmt.Errorf("not in netrestrict list")
	}
	// Reject Internet peers that try too often.
	// 頻繁に試行するインターネットピアを拒否します。
	now := srv.clock.Now()
	srv.inboundHistory.expire(now, nil)
	if !netutil.IsLAN(remoteIP) && srv.inboundHistory.contains(remoteIP.String()) {
		return fmt.Errorf("too many attempts")
	}
	srv.inboundHistory.add(remoteIP.String(), now.Add(inboundThrottleTime))
	return nil
}

// SetupConn runs the handshakes and attempts to add the connection
// as a peer. It returns when the connection has been added as a peer
// or the handshakes have failed.
// SetupConnはハンドシェイクを実行し、接続をピアとして追加しようとします。
// 接続がピアとして追加されたとき、またはハンドシェイクが失敗したときに戻ります。
func (srv *Server) SetupConn(fd net.Conn, flags connFlag, dialDest *enode.Node) error {
	c := &conn{fd: fd, flags: flags, cont: make(chan error)}
	if dialDest == nil {
		c.transport = srv.newTransport(fd, nil)
	} else {
		c.transport = srv.newTransport(fd, dialDest.Pubkey())
	}

	err := srv.setupConn(c, flags, dialDest)
	if err != nil {
		c.close(err)
	}
	return err
}

func (srv *Server) setupConn(c *conn, flags connFlag, dialDest *enode.Node) error {
	// Prevent leftover pending conns from entering the handshake.
	// 残りの保留中の接続がハンドシェイクに入らないようにします。
	srv.lock.Lock()
	running := srv.running
	srv.lock.Unlock()
	if !running {
		return errServerStopped
	}

	// If dialing, figure out the remote public key.
	// ダイヤルする場合は、リモート公開鍵を見つけます。
	var dialPubkey *ecdsa.PublicKey
	if dialDest != nil {
		dialPubkey = new(ecdsa.PublicKey)
		if err := dialDest.Load((*enode.Secp256k1)(dialPubkey)); err != nil {
			err = errors.New("dial destination doesn't have a secp256k1 public key")
			srv.log.Trace("Setting up connection failed", "addr", c.fd.RemoteAddr(), "conn", c.flags, "err", err)
			return err
		}
	}

	// Run the RLPx handshake.
	// RLPxハンドシェイクを実行します。
	remotePubkey, err := c.doEncHandshake(srv.PrivateKey)
	if err != nil {
		srv.log.Trace("Failed RLPx handshake", "addr", c.fd.RemoteAddr(), "conn", c.flags, "err", err)
		return err
	}
	if dialDest != nil {
		c.node = dialDest
	} else {
		c.node = nodeFromConn(remotePubkey, c.fd)
	}
	clog := srv.log.New("id", c.node.ID(), "addr", c.fd.RemoteAddr(), "conn", c.flags)
	err = srv.checkpoint(c, srv.checkpointPostHandshake)
	if err != nil {
		clog.Trace("Rejected peer", "err", err)
		return err
	}

	// Run the capability negotiation handshake.
	// 機能ネゴシエーションハンドシェイクを実行します。
	phs, err := c.doProtoHandshake(srv.ourHandshake)
	if err != nil {
		clog.Trace("Failed p2p handshake", "err", err)
		return err
	}
	if id := c.node.ID(); !bytes.Equal(crypto.Keccak256(phs.ID), id[:]) {
		clog.Trace("Wrong devp2p handshake identity", "phsid", hex.EncodeToString(phs.ID))
		return DiscUnexpectedIdentity
	}
	c.caps, c.name = phs.Caps, phs.Name
	err = srv.checkpoint(c, srv.checkpointAddPeer)
	if err != nil {
		clog.Trace("Rejected peer", "err", err)
		return err
	}

	return nil
}

func nodeFromConn(pubkey *ecdsa.PublicKey, conn net.Conn) *enode.Node {
	var ip net.IP
	var port int
	if tcp, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		ip = tcp.IP
		port = tcp.Port
	}
	return enode.NewV4(pubkey, ip, port, port)
}

// checkpoint sends the conn to run, which performs the
// post-handshake checks for the stage (posthandshake, addpeer).
// チェックポイントはconnを実行に送信し、
// ステージ（posthandshake、addpeer）のポストハンドシェイクチェックを実行します。
func (srv *Server) checkpoint(c *conn, stage chan<- *conn) error {
	select {
	case stage <- c:
	case <-srv.quit:
		return errServerStopped
	}
	return <-c.cont
}

func (srv *Server) launchPeer(c *conn) *Peer {
	p := newPeer(srv.log, c, srv.Protocols)
	if srv.EnableMsgEvents {
		// If message events are enabled, pass the peerFeed
		// to the peer.
		// メッセージイベントが有効になっている場合は、peerFeedをピアに渡します。
		p.events = &srv.peerFeed
	}
	go srv.runPeer(p)
	return p
}

// runPeer runs in its own goroutine for each peer.
// runPeerは、ピアごとに独自のゴルーチンで実行されます。
func (srv *Server) runPeer(p *Peer) {
	if srv.newPeerHook != nil {
		srv.newPeerHook(p)
	}
	srv.peerFeed.Send(&PeerEvent{
		Type:          PeerEventTypeAdd,
		Peer:          p.ID(),
		RemoteAddress: p.RemoteAddr().String(),
		LocalAddress:  p.LocalAddr().String(),
	})

	// Run the per-peer main loop.
	// ピアごとのメインループを実行します。
	remoteRequested, err := p.run()

	// Announce disconnect on the main loop to update the peer set.
	// The main loop waits for existing peers to be sent on srv.delpeer
	// before returning, so this send should not select on srv.quit.
	// メインループで切断をアナウンスしてピアセットを更新します。
	// メインループは、既存のピアがsrv.delpeerで送信されるのを待ってから戻るため、
	// この送信はsrv.quitで選択しないでください。
	srv.delpeer <- peerDrop{p, err, remoteRequested}

	// Broadcast peer drop to external subscribers. This needs to be
	// after the send to delpeer so subscribers have a consistent view of
	// the peer set (i.e. Server.Peers() doesn't include the peer when the
	// event is received.
	// ピアドロップを外部サブスクライバにブロードキャストします。
	// これは、サブスクライバーがピアセットの一貫したビューを持つように、
	// delpeerへの送信後に行う必要があります
	// （つまり、Server.Peers（）には、イベントの受信時にピアが含まれません。
	srv.peerFeed.Send(&PeerEvent{
		Type:          PeerEventTypeDrop,
		Peer:          p.ID(),
		Error:         err.Error(),
		RemoteAddress: p.RemoteAddr().String(),
		LocalAddress:  p.LocalAddr().String(),
	})
}

// NodeInfo represents a short summary of the information known about the host.
// NodeInfoは、ホストについて知られている情報の短い要約を表します。
type NodeInfo struct {
	ID    string `json:"id"`    // 一意のノード識別子（暗号化キーでもあります）                       // Unique node identifier (also the encryption key)
	Name  string `json:"name"`  // クライアントタイプ、バージョン、OS、カスタムデータを含むノードの名前 // Name of the node, including client type, version, OS, custom data
	Enode string `json:"enode"` // リモートピアからこのピアを追加するためのEnodeURL                   // Enode URL for adding this peer from remote peers
	ENR   string `json:"enr"`   // イーサリアムノードレコード                                       // Ethereum Node Record
	IP    string `json:"ip"`    // ノードのIPアドレス                                              // IP address of the node
	Ports struct {
		Discovery int `json:"discovery"` // 検出プロトコルのUDPリスニングポート // UDP listening port for discovery protocol
		Listener  int `json:"listener"`  // RLPxのTCPリスニングポート         // TCP listening port for RLPx
	} `json:"ports"`
	ListenAddr string                 `json:"listenAddr"`
	Protocols  map[string]interface{} `json:"protocols"`
}

// NodeInfo gathers and returns a collection of metadata known about the host.
// NodeInfoは、ホストについて認識されているメタデータのコレクションを収集して返します。
func (srv *Server) NodeInfo() *NodeInfo {
	// Gather and assemble the generic node infos
	// 一般的なノード情報を収集してアセンブルします
	node := srv.Self()
	info := &NodeInfo{
		Name:       srv.Name,
		Enode:      node.URLv4(),
		ID:         node.ID().String(),
		IP:         node.IP().String(),
		ListenAddr: srv.ListenAddr,
		Protocols:  make(map[string]interface{}),
	}
	info.Ports.Discovery = node.UDP()
	info.Ports.Listener = node.TCP()
	info.ENR = node.String()

	// Gather all the running protocol infos (only once per protocol type)
	// 実行中のすべてのプロトコル情報を収集します（プロトコルタイプごとに1回のみ）
	for _, proto := range srv.Protocols {
		if _, ok := info.Protocols[proto.Name]; !ok {
			nodeInfo := interface{}("unknown")
			if query := proto.NodeInfo; query != nil {
				nodeInfo = proto.NodeInfo()
			}
			info.Protocols[proto.Name] = nodeInfo
		}
	}
	return info
}

// PeersInfo returns an array of metadata objects describing connected peers.
// PeersInfoは、接続されたピアを説明するメタデータオブジェクトの配列を返します。
func (srv *Server) PeersInfo() []*PeerInfo {
	// Gather all the generic and sub-protocol specific infos
	// すべての一般的およびサブプロトコル固有の情報を収集します
	infos := make([]*PeerInfo, 0, srv.PeerCount())
	for _, peer := range srv.Peers() {
		if peer != nil {
			infos = append(infos, peer.Info())
		}
	}
	// Sort the result array alphabetically by node identifier
	// 結果の配列をノード識別子のアルファベット順に並べ替えます
	for i := 0; i < len(infos); i++ {
		for j := i + 1; j < len(infos); j++ {
			if infos[i].ID > infos[j].ID {
				infos[i], infos[j] = infos[j], infos[i]
			}
		}
	}
	return infos
}
