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

package p2p

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/rlp"
)

var (
	ErrShuttingDown = errors.New("shutting down")
)

const (
	baseProtocolVersion    = 5
	baseProtocolLength     = uint64(16)
	baseProtocolMaxMsgSize = 2 * 1024

	snappyProtocolVersion = 5

	pingInterval = 15 * time.Second
)

const (
	// devp2p message codes
	// devp2pメッセージコード
	handshakeMsg = 0x00
	discMsg      = 0x01
	pingMsg      = 0x02
	pongMsg      = 0x03
)

// protoHandshake is the RLP structure of the protocol handshake.
// protoHandshakeは、プロトコルハンドシェイクのRLP構造です。
type protoHandshake struct {
	Version    uint64
	Name       string
	Caps       []Cap
	ListenPort uint64
	ID         []byte // secp256k1 public key

	// Ignore additional fields (for forward compatibility).
	// 追加のフィールドを無視します（上位互換性のため）。
	Rest []rlp.RawValue `rlp:"tail"`
}

// PeerEventType is the type of peer events emitted by a p2p.Server
// PeerEventTypeは、p2p.Serverによって発行されるピアイベントのタイプです。
type PeerEventType string

const (
	// PeerEventTypeAdd is the type of event emitted when a peer is added
	// to a p2p.Server
	// PeerEventTypeAddは、ピアがp2p.Serverに追加されたときに発行されるイベントのタイプです。
	PeerEventTypeAdd PeerEventType = "add"

	// PeerEventTypeDrop is the type of event emitted when a peer is
	// dropped from a p2p.Server
	//PeerEventTypeDropは、ピアがp2p.Serverからドロップされたときに発行されるイベントのタイプです。
	PeerEventTypeDrop PeerEventType = "drop"

	// PeerEventTypeMsgSend is the type of event emitted when a
	// message is successfully sent to a peer
	// PeerEventTypeMsgSendは、メッセージがピアに正常に送信されたときに発行されるイベントのタイプです。
	PeerEventTypeMsgSend PeerEventType = "msgsend"

	// PeerEventTypeMsgRecv is the type of event emitted when a
	// message is received from a peer
	// PeerEventTypeMsgRecvは、ピアからメッセージを受信したときに発行されるイベントのタイプです。
	PeerEventTypeMsgRecv PeerEventType = "msgrecv"
)

// PeerEvent is an event emitted when peers are either added or dropped from
// a p2p.Server or when a message is sent or received on a peer connection
// PeerEventは、ピアがp2p.Serverに追加または削除されたとき、またはメッセージがピア接続で送受信されたときに発行されるイベントです。
type PeerEvent struct {
	Type          PeerEventType `json:"type"`
	Peer          enode.ID      `json:"peer"`
	Error         string        `json:"error,omitempty"`
	Protocol      string        `json:"protocol,omitempty"`
	MsgCode       *uint64       `json:"msg_code,omitempty"`
	MsgSize       *uint32       `json:"msg_size,omitempty"`
	LocalAddress  string        `json:"local,omitempty"`
	RemoteAddress string        `json:"remote,omitempty"`
}

// Peer represents a connected remote node.
// ピアは、接続されたリモートノードを表します
type Peer struct {
	rw      *conn
	running map[string]*protoRW
	log     log.Logger
	created mclock.AbsTime

	wg       sync.WaitGroup
	protoErr chan error
	closed   chan struct{}
	disc     chan DiscReason

	// events receives message send / receive events if set
	// イベントはメッセージを受信します設定されている場合はイベントを送信/受信します
	events   *event.Feed
	testPipe *MsgPipeRW // for testing
}

// NewPeer returns a peer for testing purposes.
// NewPeerは、テスト目的でピアを返します。
func NewPeer(id enode.ID, name string, caps []Cap) *Peer {
	// Generate a fake set of local protocols to match as running caps. Almost
	// no fields needs to be meaningful here as we're only using it to cross-
	// check with the "remote" caps array.
	protos := make([]Protocol, len(caps))
	for i, cap := range caps {
		protos[i].Name = cap.Name
		protos[i].Version = cap.Version
	}
	pipe, _ := net.Pipe()
	node := enode.SignNull(new(enr.Record), id)
	conn := &conn{fd: pipe, transport: nil, node: node, caps: caps, name: name}
	peer := newPeer(log.Root(), conn, protos)
	close(peer.closed) // ensures Disconnect doesn't block 切断がブロックされないことを保証します
	return peer
}

// NewPeerPipe creates a peer for testing purposes.
// The message pipe given as the last parameter is closed when
// Disconnect is called on the peer.
// NewPeerPipeは、テスト目的でピアを作成します。
// ピアでDisconnectが呼び出されると、
// 最後のパラメータとして指定されたメッセージパイプが閉じられます。
func NewPeerPipe(id enode.ID, name string, caps []Cap, pipe *MsgPipeRW) *Peer {
	p := NewPeer(id, name, caps)
	p.testPipe = pipe
	return p
}

// ID returns the node's public key.
// IDはノードの公開鍵を返します。
func (p *Peer) ID() enode.ID {
	return p.rw.node.ID()
}

// Node returns the peer's node descriptor.
// Nodeは、ピアのノード記述子を返します。
func (p *Peer) Node() *enode.Node {
	return p.rw.node
}

// Name returns an abbreviated form of the name
// Nameは、名前の省略形を返します
func (p *Peer) Name() string {
	s := p.rw.name
	if len(s) > 20 {
		return s[:20] + "..."
	}
	return s
}

// Fullname returns the node name that the remote node advertised.
// フルネームは、リモートノードがアドバタイズしたノード名を返します。
func (p *Peer) Fullname() string {
	return p.rw.name
}

// Caps returns the capabilities (supported subprotocols) of the remote peer.
// Capsは、リモートピアの機能（サポートされているサブプロトコル）を返します
func (p *Peer) Caps() []Cap {
	// TODO: maybe return copy
	return p.rw.caps
}

// RunningCap returns true if the peer is actively connected using any of the
// enumerated versions of a specific protocol, meaning that at least one of the
// versions is supported by both this node and the peer p.
// ピアが特定のプロトコルの列挙されたバージョンのいずれかを使用してアクティブに接続されている場合、
// RunningCapはtrueを返します。
// つまり、バージョンの少なくとも1つがこのノードとピアpの両方でサポートされていることを意味します。
func (p *Peer) RunningCap(protocol string, versions []uint) bool {
	if proto, ok := p.running[protocol]; ok {
		for _, ver := range versions {
			if proto.Version == ver {
				return true
			}
		}
	}
	return false
}

// RemoteAddr returns the remote address of the network connection.
// RemoteAddrは、ネットワーク接続のリモートアドレスを返します。
func (p *Peer) RemoteAddr() net.Addr {
	return p.rw.fd.RemoteAddr()
}

// LocalAddr returns the local address of the network connection.
// LocalAddrは、ネットワーク接続のローカルアドレスを返します。
func (p *Peer) LocalAddr() net.Addr {
	return p.rw.fd.LocalAddr()
}

// Disconnect terminates the peer connection with the given reason.
// It returns immediately and does not wait until the connection is closed.
// 切断は、指定された理由でピア接続を終了します。
// すぐに戻り、接続が閉じられるまで待機しません。
func (p *Peer) Disconnect(reason DiscReason) {
	if p.testPipe != nil {
		p.testPipe.Close()
	}

	select {
	case p.disc <- reason:
	case <-p.closed:
	}
}

// String implements fmt.Stringer.
// Stringはfmt.Stringerを実装します。
func (p *Peer) String() string {
	id := p.ID()
	return fmt.Sprintf("Peer %x %v", id[:8], p.RemoteAddr())
}

// Inbound returns true if the peer is an inbound connection
// ピアがインバウンド接続の場合、インバウンドはtrueを返します
func (p *Peer) Inbound() bool {
	return p.rw.is(inboundConn)
}

func newPeer(log log.Logger, conn *conn, protocols []Protocol) *Peer {
	protomap := matchProtocols(protocols, conn.caps, conn)
	p := &Peer{
		rw:       conn,
		running:  protomap,
		created:  mclock.Now(),
		disc:     make(chan DiscReason),
		protoErr: make(chan error, len(protomap)+1), // protocols + pingLoop
		closed:   make(chan struct{}),
		log:      log.New("id", conn.node.ID(), "conn", conn.flags),
	}
	return p
}

func (p *Peer) Log() log.Logger {
	return p.log
}

func (p *Peer) run() (remoteRequested bool, err error) {
	var (
		writeStart = make(chan struct{}, 1)
		writeErr   = make(chan error, 1)
		readErr    = make(chan error, 1)
		reason     DiscReason // sent to the peer ピアに送信
	)
	p.wg.Add(2)
	go p.readLoop(readErr)
	go p.pingLoop()

	// Start all protocol handlers.
	// すべてのプロトコルハンドラーを開始します。
	writeStart <- struct{}{}
	p.startProtocols(writeStart, writeErr)

	// Wait for an error or disconnect.
	// エラーを待つか、切断します。
loop:
	for {
		select {
		case err = <-writeErr:
			// A write finished. Allow the next write to start if
			// there was no error.
			// 書き込みが終了しました。エラーがなかった場合は、次の書き込みの開始を許可します。
			if err != nil {
				reason = DiscNetworkError
				break loop
			}
			writeStart <- struct{}{}
		case err = <-readErr:
			if r, ok := err.(DiscReason); ok {
				remoteRequested = true
				reason = r
			} else {
				reason = DiscNetworkError
			}
			break loop
		case err = <-p.protoErr:
			reason = discReasonForError(err)
			break loop
		case err = <-p.disc:
			reason = discReasonForError(err)
			break loop
		}
	}

	close(p.closed)
	p.rw.close(reason)
	p.wg.Wait()
	return remoteRequested, err
}

func (p *Peer) pingLoop() {
	ping := time.NewTimer(pingInterval)
	defer p.wg.Done()
	defer ping.Stop()
	for {
		select {
		case <-ping.C:
			if err := SendItems(p.rw, pingMsg); err != nil {
				p.protoErr <- err
				return
			}
			ping.Reset(pingInterval)
		case <-p.closed:
			return
		}
	}
}

func (p *Peer) readLoop(errc chan<- error) {
	defer p.wg.Done()
	for {
		msg, err := p.rw.ReadMsg()
		if err != nil {
			errc <- err
			return
		}
		msg.ReceivedAt = time.Now()
		if err = p.handle(msg); err != nil {
			errc <- err
			return
		}
	}
}

func (p *Peer) handle(msg Msg) error {
	switch {
	case msg.Code == pingMsg:
		msg.Discard()
		go SendItems(p.rw, pongMsg)
	case msg.Code == discMsg:
		var reason [1]DiscReason
		// This is the last message. We don't need to discard or
		// check errors because, the connection will be closed after it.
		// これが最後のメッセージです。エラーが発生すると接続が閉じられるため、エラーを破棄したりチェックしたりする必要はありません。
		rlp.Decode(msg.Payload, &reason)
		return reason[0]
	case msg.Code < baseProtocolLength:
		// ignore other base protocol messages
		// 他の基本プロトコルメッセージを無視する
		return msg.Discard()
	default:
		// it's a subprotocol message
		// サブプロトコルメッセージです
		proto, err := p.getProto(msg.Code)
		if err != nil {
			return fmt.Errorf("msg code out of range: %v", msg.Code)
		}
		if metrics.Enabled {
			m := fmt.Sprintf("%s/%s/%d/%#02x", ingressMeterName, proto.Name, proto.Version, msg.Code-proto.offset)
			metrics.GetOrRegisterMeter(m, nil).Mark(int64(msg.meterSize))
			metrics.GetOrRegisterMeter(m+"/packets", nil).Mark(1)
		}
		select {
		case proto.in <- msg:
			return nil
		case <-p.closed:
			return io.EOF
		}
	}
	return nil
}

func countMatchingProtocols(protocols []Protocol, caps []Cap) int {
	n := 0
	for _, cap := range caps {
		for _, proto := range protocols {
			if proto.Name == cap.Name && proto.Version == cap.Version {
				n++
			}
		}
	}
	return n
}

// matchProtocols creates structures for matching named subprotocols.
// matchProtocolsは、名前付きサブプロトコルを照合するための構造を作成します。
func matchProtocols(protocols []Protocol, caps []Cap, rw MsgReadWriter) map[string]*protoRW {
	sort.Sort(capsByNameAndVersion(caps))
	offset := baseProtocolLength
	result := make(map[string]*protoRW)

outer:
	for _, cap := range caps {
		for _, proto := range protocols {
			if proto.Name == cap.Name && proto.Version == cap.Version {
				// If an old protocol version matched, revert it
				// 古いプロトコルバージョンが一致した場合は、元に戻します
				if old := result[cap.Name]; old != nil {
					offset -= old.Length
				}
				// Assign the new match
				// 新しい一致を割り当てます
				result[cap.Name] = &protoRW{Protocol: proto, offset: offset, in: make(chan Msg), w: rw}
				offset += proto.Length

				continue outer
			}
		}
	}
	return result
}

func (p *Peer) startProtocols(writeStart <-chan struct{}, writeErr chan<- error) {
	p.wg.Add(len(p.running))
	for _, proto := range p.running {
		proto := proto
		proto.closed = p.closed
		proto.wstart = writeStart
		proto.werr = writeErr
		var rw MsgReadWriter = proto
		if p.events != nil {
			rw = newMsgEventer(rw, p.events, p.ID(), proto.Name, p.Info().Network.RemoteAddress, p.Info().Network.LocalAddress)
		}
		p.log.Trace(fmt.Sprintf("Starting protocol %s/%d", proto.Name, proto.Version))
		go func() {
			defer p.wg.Done()
			err := proto.Run(p, rw)
			if err == nil {
				p.log.Trace(fmt.Sprintf("Protocol %s/%d returned", proto.Name, proto.Version))
				err = errProtocolReturned
			} else if err != io.EOF {
				p.log.Trace(fmt.Sprintf("Protocol %s/%d failed", proto.Name, proto.Version), "err", err)
			}
			p.protoErr <- err
		}()
	}
}

// getProto finds the protocol responsible for handling
// the given message code.
// getProtoは、指定されたメッセージコードの処理を担当するプロトコルを検索します。
func (p *Peer) getProto(code uint64) (*protoRW, error) {
	for _, proto := range p.running {
		if code >= proto.offset && code < proto.offset+proto.Length {
			return proto, nil
		}
	}
	return nil, newPeerError(errInvalidMsgCode, "%d", code)
}

type protoRW struct {
	Protocol
	in     chan Msg        // receives read messages              // 既読メッセージを受信する
	closed <-chan struct{} // receives when peer is shutting down // ピアがシャットダウンしているときに受信します
	wstart <-chan struct{} // receives when write may start       // 書き込みが開始される可能性があるときに受信します
	werr   chan<- error    // for write results                   // 書き込み結果の場合
	offset uint64
	w      MsgWriter
}

func (rw *protoRW) WriteMsg(msg Msg) (err error) {
	if msg.Code >= rw.Length {
		return newPeerError(errInvalidMsgCode, "not handled")
	}
	msg.meterCap = rw.cap()
	msg.meterCode = msg.Code

	msg.Code += rw.offset

	select {
	case <-rw.wstart:
		err = rw.w.WriteMsg(msg)
		// Report write status back to Peer.run. It will initiate
		// shutdown if the error is non-nil and unblock the next write
		// otherwise. The calling protocol code should exit for errors
		// as well but we don't want to rely on that.
		// 書き込みステータスをPeer.runに報告します。
		// エラーがnil以外の場合はシャットダウンを開始し、
		// それ以外の場合は次の書き込みのブロックを解除します。
		// 呼び出し元のプロトコルコードもエラーが発生した場合に終了する必要がありますが、
		// これに依存することは望ましくありません。
		rw.werr <- err
	case <-rw.closed:
		err = ErrShuttingDown
	}
	return err
}

func (rw *protoRW) ReadMsg() (Msg, error) {
	select {
	case msg := <-rw.in:
		msg.Code -= rw.offset
		return msg, nil
	case <-rw.closed:
		return Msg{}, io.EOF
	}
}

// PeerInfo represents a short summary of the information known about a connected
// peer. Sub-protocol independent fields are contained and initialized here, with
// protocol specifics delegated to all connected sub-protocols.
// PeerInfoは、接続されたピアについて既知の情報の短い要約を表します。
// サブプロトコルに依存しないフィールドがここに含まれ、初期化され、
// 接続されているすべてのサブプロトコルにプロトコルの詳細が委任されます。
type PeerInfo struct {
	ENR     string   `json:"enr,omitempty"` // Ethereum Node Record //イーサリアムノードレコード
	Enode   string   `json:"enode"`         // Node URL //ノードURL
	ID      string   `json:"id"`            // Unique node identifier //一意のノード識別子
	Name    string   `json:"name"`          // Name of the node, including client type, version, OS, custom data//クライアントタイプ、バージョン、OS、カスタムデータを含むノードの名前
	Caps    []string `json:"caps"`          // Protocols advertised by this peer	//このピアによってアドバタイズされたプロトコル
	Network struct {
		LocalAddress  string `json:"localAddress"`  // Local endpoint of the TCP data connection TCPデータ接続のローカルエンドポイント
		RemoteAddress string `json:"remoteAddress"` // Remote endpoint of the TCP data connection TCPデータ接続のリモートエンドポイント
		Inbound       bool   `json:"inbound"`
		Trusted       bool   `json:"trusted"`
		Static        bool   `json:"static"`
	} `json:"network"`
	Protocols map[string]interface{} `json:"protocols"` // Sub-protocol specific metadata fields サブプロトコル固有のメタデータフィールド
}

// Info gathers and returns a collection of metadata known about a peer.
// Infoは、ピアについて認識されているメタデータのコレクションを収集して返します。
func (p *Peer) Info() *PeerInfo {
	// Gather the protocol capabilities
	// プロトコル機能を収集する
	var caps []string
	for _, cap := range p.Caps() {
		caps = append(caps, cap.String())
	}
	// Assemble the generic peer metadata
	// 汎用ピアメタデータをアセンブルします
	info := &PeerInfo{
		Enode:     p.Node().URLv4(),
		ID:        p.ID().String(),
		Name:      p.Fullname(),
		Caps:      caps,
		Protocols: make(map[string]interface{}),
	}
	if p.Node().Seq() > 0 {
		info.ENR = p.Node().String()
	}
	info.Network.LocalAddress = p.LocalAddr().String()
	info.Network.RemoteAddress = p.RemoteAddr().String()
	info.Network.Inbound = p.rw.is(inboundConn)
	info.Network.Trusted = p.rw.is(trustedConn)
	info.Network.Static = p.rw.is(staticDialedConn)

	// Gather all the running protocol infos
	// 実行中のすべてのプロトコル情報を収集します
	for _, proto := range p.running {
		protoInfo := interface{}("unknown")
		if query := proto.Protocol.PeerInfo; query != nil {
			if metadata := query(p.ID()); metadata != nil {
				protoInfo = metadata
			} else {
				protoInfo = "handshake"
			}
		}
		info.Protocols[proto.Name] = protoInfo
	}
	return info
}
