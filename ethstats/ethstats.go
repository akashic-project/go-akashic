// Copyright 2016 The go-ethereum Authors
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

// Package ethstats implements the network stats reporting service.
//パッケージethstatsは、ネットワーク統計レポートサービスを実装します。
package ethstats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	ethproto "github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/les"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/gorilla/websocket"
)

const (
	// historyUpdateRange is the number of blocks a node should report upon login or
	// history request.
	// historyUpdateRangeは、ログインまたは履歴要求時にノードが報告する必要のあるブロックの数です。
	historyUpdateRange = 50

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	// txChanSizeは、NewTxsEventをリッスンしているチャネルのサイズです。
	// 数値はtxプールのサイズから参照されます。
	txChanSize = 4096
	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	// chainHeadChanSizeは、ChainHeadEventをリッスンしているチャネルのサイズです。
	chainHeadChanSize = 10
)

// backend encompasses the bare-minimum functionality needed for ethstats reporting
// バックエンドには、ethstatsレポートに必要な最小限の機能が含まれます
type backend interface {
	SubscribeChainHeadEvent(ch chan<- core.ChainHeadEvent) event.Subscription
	SubscribeNewTxsEvent(ch chan<- core.NewTxsEvent) event.Subscription
	CurrentHeader() *types.Header
	HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Header, error)
	GetTd(ctx context.Context, hash common.Hash) *big.Int
	Stats() (pending int, queued int)
	SyncProgress() ethereum.SyncProgress
}

// fullNodeBackend encompasses the functionality necessary for a full node
// reporting to ethstats
// fullNodeBackendには、完全なノードがethstatsにレポートするために必要な機能が含まれています
type fullNodeBackend interface {
	backend
	Miner() *miner.Miner
	BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*types.Block, error)
	CurrentBlock() *types.Block
	SuggestGasTipCap(ctx context.Context) (*big.Int, error)
}

// Service implements an Ethereum netstats reporting daemon that pushes local
// chain statistics up to a monitoring server.
//サービスは、ローカルチェーン統計を監視サーバーにプッシュするEthereumnetstatsレポートデーモンを実装します。
type Service struct {
	server  *p2p.Server // ネットワーク情報を取得するためのピアツーピアサーバー // Peer-to-peer server to retrieve networking infos
	backend backend
	engine  consensus.Engine // 可変個引数ブロックフィールドを取得するためのコンセンサスエンジン // Consensus engine to retrieve variadic block fields

	node string // 監視ページに表示するノードの名前               // Name of the node to display on the monitoring page
	pass string // 監視ページへのアクセスを承認するためのパスワード // Password to authorize access to the monitoring page
	host string // 監視サービスのリモートアドレス // Remote address of the monitoring service

	pongCh chan struct{} // Pong通知はこのチャネルに送られます // Pong notifications are fed into this channel
	histCh chan []uint64 // 履歴リクエストのブロック番号がこのチャネルに入力されます // History request block numbers are fed into this channel

	headSub event.Subscription
	txSub   event.Subscription
}

// connWrapper is a wrapper to prevent concurrent-write or concurrent-read on the
// websocket.
//
// From Gorilla websocket docs:
//   Connections support one concurrent reader and one concurrent writer.
//   Applications are responsible for ensuring that no more than one goroutine calls the write methods
//     - NextWriter, SetWriteDeadline, WriteMessage, WriteJSON, EnableWriteCompression, SetCompressionLevel
//   concurrently and that no more than one goroutine calls the read methods
//     - NextReader, SetReadDeadline, ReadMessage, ReadJSON, SetPongHandler, SetPingHandler
//   concurrently.
//   The Close and WriteControl methods can be called concurrently with all other methods.
// connWrapperは、WebSocketでの同時書き込みまたは同時読み取りを防ぐためのラッパーです。
//
// Gorilla WebSocketドキュメントから：
// 接続は、1つの同時リーダーと1つの同時ライターをサポートします。
// アプリケーションは、1つだけのゴルーチンがwriteメソッドを呼び出すようにする責任があります
// --NextWriter、SetWriteDeadline、WriteMessage、WriteJSON、EnableWriteCompression、SetCompressionLevelを同時に実行し、読み取りメソッドを呼び出すゴルーチンは1つだけです。
// --NextReader、SetReadDeadline、ReadMessage、ReadJSON、SetPongHandler、SetPingHandlerを同時に。
// CloseメソッドとWriteControlメソッドは、他のすべてのメソッドと同時に呼び出すことができます。
type connWrapper struct {
	conn *websocket.Conn

	rlock sync.Mutex
	wlock sync.Mutex
}

func newConnectionWrapper(conn *websocket.Conn) *connWrapper {
	return &connWrapper{conn: conn}
}

// WriteJSON wraps corresponding method on the websocket but is safe for concurrent calling
// WriteJSONは、対応するメソッドをWebSocketでラップしますが、同時呼び出しには安全です
func (w *connWrapper) WriteJSON(v interface{}) error {
	w.wlock.Lock()
	defer w.wlock.Unlock()

	return w.conn.WriteJSON(v)
}

// ReadJSON wraps corresponding method on the websocket but is safe for concurrent calling
// ReadJSONは、対応するメソッドをWebSocketでラップしますが、同時呼び出しには安全です
func (w *connWrapper) ReadJSON(v interface{}) error {
	w.rlock.Lock()
	defer w.rlock.Unlock()

	return w.conn.ReadJSON(v)
}

// Close wraps corresponding method on the websocket but is safe for concurrent calling
// WebSocketで対応するメソッドをクローズラップしますが、同時呼び出しには安全です
func (w *connWrapper) Close() error {
	// The Close and WriteControl methods can be called concurrently with all other methods,
	// so the mutex is not used here
	// CloseメソッドとWriteControlメソッドは他のすべてのメソッドと同時に呼び出すことができるため、
	// ここではミューテックスは使用されません
	return w.conn.Close()
}

// parseEthstatsURL parses the netstats connection url.
// URL argument should be of the form <nodename:secret@host:port>
// If non-erroring, the returned slice contains 3 elements: [nodename, pass, host]
// parseEthstatsURLは、netstats接続URLを解析します。
// URL引数は<nodename：secret @ host：port>の形式である必要があります。
// エラーがない場合、返されるスライスには3つの要素が含まれます：[nodename、pass、host]
func parseEthstatsURL(url string) (parts []string, err error) {
	err = fmt.Errorf("invalid netstats url: \"%s\", should be nodename:secret@host:port", url)

	hostIndex := strings.LastIndex(url, "@")
	if hostIndex == -1 || hostIndex == len(url)-1 {
		return nil, err
	}
	preHost, host := url[:hostIndex], url[hostIndex+1:]

	passIndex := strings.LastIndex(preHost, ":")
	if passIndex == -1 {
		return []string{preHost, "", host}, nil
	}
	nodename, pass := preHost[:passIndex], ""
	if passIndex != len(preHost)-1 {
		pass = preHost[passIndex+1:]
	}

	return []string{nodename, pass, host}, nil
}

// New returns a monitoring service ready for stats reporting.
// Newは、統計レポートの準備ができている監視サービスを返します。
func New(node *node.Node, backend backend, engine consensus.Engine, url string) error {
	parts, err := parseEthstatsURL(url)
	if err != nil {
		return err
	}
	ethstats := &Service{
		backend: backend,
		engine:  engine,
		server:  node.Server(),
		node:    parts[0],
		pass:    parts[1],
		host:    parts[2],
		pongCh:  make(chan struct{}),
		histCh:  make(chan []uint64, 1),
	}

	node.RegisterLifecycle(ethstats)
	return nil
}

// Start implements node.Lifecycle, starting up the monitoring and reporting daemon.
// Startはnode.Lifecycleを実装し、監視およびレポートデーモンを起動します。
func (s *Service) Start() error {
	// Subscribe to chain events to execute updates on
	// チェーンイベントをサブスクライブして更新を実行します
	chainHeadCh := make(chan core.ChainHeadEvent, chainHeadChanSize)
	s.headSub = s.backend.SubscribeChainHeadEvent(chainHeadCh)
	txEventCh := make(chan core.NewTxsEvent, txChanSize)
	s.txSub = s.backend.SubscribeNewTxsEvent(txEventCh)
	go s.loop(chainHeadCh, txEventCh)

	log.Info("Stats daemon started")
	return nil
}

// Stop implements node.Lifecycle, terminating the monitoring and reporting daemon.
// 停止はnode.Lifecycleを実装し、監視およびレポートデーモンを終了します。
func (s *Service) Stop() error {
	s.headSub.Unsubscribe()
	s.txSub.Unsubscribe()
	log.Info("Stats daemon stopped")
	return nil
}

// loop keeps trying to connect to the netstats server, reporting chain events
// until termination.
// ループはnetstatsサーバーへの接続を試み続け、終了するまでチェーンイベントを報告します。
func (s *Service) loop(chainHeadCh chan core.ChainHeadEvent, txEventCh chan core.NewTxsEvent) {
	// Start a goroutine that exhausts the subscriptions to avoid events piling up
	// イベントが積み重なるのを防ぐために、サブスクリプションを使い果たすゴルーチンを開始します
	var (
		quitCh = make(chan struct{})
		headCh = make(chan *types.Block, 1)
		txCh   = make(chan struct{}, 1)
	)
	go func() {
		var lastTx mclock.AbsTime

	HandleLoop:
		for {
			select {
			// Notify of chain head events, but drop if too frequent
			// チェーンヘッドイベントを通知しますが、頻度が高すぎる場合は削除します
			case head := <-chainHeadCh:
				select {
				case headCh <- head.Block:
				default:
				}

			// Notify of new transaction events, but drop if too frequent
			// 新しいトランザクションイベントを通知しますが、頻度が高すぎる場合は削除します
			case <-txEventCh:
				if time.Duration(mclock.Now()-lastTx) < time.Second {
					continue
				}
				lastTx = mclock.Now()

				select {
				case txCh <- struct{}{}:
				default:
				}

			// node stopped
			// ノードが停止しました
			case <-s.txSub.Err():
				break HandleLoop
			case <-s.headSub.Err():
				break HandleLoop
			}
		}
		close(quitCh)
	}()

	// Resolve the URL, defaulting to TLS, but falling back to none too
	// URLを解決します。デフォルトはTLSですが、フォールバックもありません。
	path := fmt.Sprintf("%s/api", s.host)
	urls := []string{path}

	// url.Parse and url.IsAbs is unsuitable (https://github.com/golang/go/issues/19779)
	// url.Parseとurl.IsAbsは不適切です（https://github.com/golang/go/issues/19779）
	if !strings.Contains(path, "://") {
		urls = []string{"wss://" + path, "ws://" + path}
	}

	errTimer := time.NewTimer(0)
	defer errTimer.Stop()
	// Loop reporting until termination
	// 終了までレポートをループします
	for {
		select {
		case <-quitCh:
			return
		case <-errTimer.C:
			// Establish a websocket connection to the server on any supported URL
			// サポートされているURLでサーバーへのWebSocket接続を確立します
			var (
				conn *connWrapper
				err  error
			)
			dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
			header := make(http.Header)
			header.Set("origin", "http://localhost")
			for _, url := range urls {
				c, _, e := dialer.Dial(url, header)
				err = e
				if err == nil {
					conn = newConnectionWrapper(c)
					break
				}
			}
			if err != nil {
				log.Warn("Stats server unreachable", "err", err)
				errTimer.Reset(10 * time.Second)
				continue
			}
			// Authenticate the client with the server
			// サーバーでクライアントを認証します
			if err = s.login(conn); err != nil {
				log.Warn("Stats login failed", "err", err)
				conn.Close()
				errTimer.Reset(10 * time.Second)
				continue
			}
			go s.readLoop(conn)

			// Send the initial stats so our node looks decent from the get go
			// 初期統計を送信して、鼻が最初から下降しているように見えるようにします
			if err = s.report(conn); err != nil {
				log.Warn("Initial stats report failed", "err", err)
				conn.Close()
				errTimer.Reset(0)
				continue
			}
			// Keep sending status updates until the connection breaks
			// 接続が切断されるまでステータスの更新を送信し続けます
			fullReport := time.NewTicker(15 * time.Second)

			for err == nil {
				select {
				case <-quitCh:
					fullReport.Stop()
					// Make sure the connection is closed
					// 接続が閉じていることを確認します
					conn.Close()
					return

				case <-fullReport.C:
					if err = s.report(conn); err != nil {
						log.Warn("Full stats report failed", "err", err) // 完全な統計レポートが失敗しました
					}
				case list := <-s.histCh:
					if err = s.reportHistory(conn, list); err != nil {
						log.Warn("Requested history report failed", "err", err) // 要求された履歴レポートが失敗しました
					}
				case head := <-headCh:
					if err = s.reportBlock(conn, head); err != nil {
						log.Warn("Block stats report failed", "err", err) // ブロック統計レポートが失敗しました
					}
					if err = s.reportPending(conn); err != nil {
						log.Warn("Post-block transaction stats report failed", "err", err) // ブロック後のトランザクション統計レポートが失敗しました
					}
				case <-txCh:
					if err = s.reportPending(conn); err != nil {
						log.Warn("Transaction stats report failed", "err", err) // トランザクション統計レポートが失敗しました
					}
				}
			}
			fullReport.Stop()

			// Close the current connection and establish a new one
			// 現在の接続を閉じて、新しい接続を確立します
			conn.Close()
			errTimer.Reset(0)
		}
	}
}

// readLoop loops as long as the connection is alive and retrieves data packets
// from the network socket. If any of them match an active request, it forwards
// it, if they themselves are requests it initiates a reply, and lastly it drops
// unknown packets.
// readLoopは、接続が有効である限りループし、ネットワークソケットからデータパケットを取得します。
// それらのいずれかがアクティブな要求に一致する場合、それを転送し、それら自体が要求である場合、
// 応答を開始し、最後に不明なパケットをドロップします。
func (s *Service) readLoop(conn *connWrapper) {
	// If the read loop exits, close the connection
	// 読み取りループが終了した場合は、接続を閉じます
	defer conn.Close()

	for {
		// Retrieve the next generic network packet and bail out on error
		// 次の汎用ネットワークパケットを取得し、エラーでベイルアウトします
		var blob json.RawMessage
		if err := conn.ReadJSON(&blob); err != nil {
			log.Warn("Failed to retrieve stats server message", "err", err) // 統計サーバーメッセージの取得に失敗しました
			return
		}
		// If the network packet is a system ping, respond to it directly
		// ネットワークパケットがシステムpingの場合は、直接応答します
		var ping string
		if err := json.Unmarshal(blob, &ping); err == nil && strings.HasPrefix(ping, "primus::ping::") {
			if err := conn.WriteJSON(strings.Replace(ping, "ping", "pong", -1)); err != nil {
				log.Warn("Failed to respond to system ping message", "err", err)
				return
			}
			continue
		}
		// Not a system ping, try to decode an actual state message
		// システムのpingではなく、実際の状態メッセージをデコードしてみてください
		var msg map[string][]interface{}
		if err := json.Unmarshal(blob, &msg); err != nil {
			log.Warn("Failed to decode stats server message", "err", err) // 統計サーバーメッセージのデコードに失敗しました
			return
		}
		log.Trace("Received message from stats server", "msg", msg) // 統計サーバーからメッセージを受信しました
		if len(msg["emit"]) == 0 {
			log.Warn("Stats server sent non-broadcast", "msg", msg) // 統計サーバーが非ブロードキャストで送信されました
			return
		}
		command, ok := msg["emit"][0].(string)
		if !ok {
			log.Warn("Invalid stats server message type", "type", msg["emit"][0]) // 無効な統計サーバーメッセージタイプ
			return
		}
		// If the message is a ping reply, deliver (someone must be listening!)
		// メッセージがping応答の場合は、配信します（誰かが聞いている必要があります！）
		if len(msg["emit"]) == 2 && command == "node-pong" {
			select {
			case s.pongCh <- struct{}{}:
				// Pong delivered, continue listening
				// Pongが配信され、聞き続けます
				continue
			default:
				// Ping routine dead, abort
				// Pingルーチンが停止し、中止します
				log.Warn("Stats server pinger seems to have died") // 統計サーバーのピンガーが死んだようです
				return
			}
		}
		// If the message is a history request, forward to the event processor
		if len(msg["emit"]) == 2 && command == "history" {
			// Make sure the request is valid and doesn't crash us
			request, ok := msg["emit"][1].(map[string]interface{})
			if !ok {
				log.Warn("Invalid stats history request", "msg", msg["emit"][1]) // 無効な統計履歴リクエスト
				select {
				case s.histCh <- nil: // インデックスなしのリクエストとして扱います // Treat it as an no indexes request
				default:
				}
				continue
			}
			list, ok := request["list"].([]interface{})
			if !ok {
				log.Warn("Invalid stats history block list", "list", request["list"]) // 無効な統計履歴ブロックリスト
				return
			}
			// Convert the block number list to an integer list
			// ブロック番号リストを整数リストに変換します
			numbers := make([]uint64, len(list))
			for i, num := range list {
				n, ok := num.(float64)
				if !ok {
					log.Warn("Invalid stats history block number", "number", num) // 無効な統計履歴ブロック番号
					return
				}
				numbers[i] = uint64(n)
			}
			select {
			case s.histCh <- numbers:
				continue
			default:
			}
		}
		// Report anything else and continue
		// 他に何かを報告して続行します
		log.Info("Unknown stats message", "msg", msg)
	}
}

// nodeInfo is the collection of meta information about a node that is displayed
// on the monitoring page.
// nodeInfoは、監視ページに表示されるノードに関するメタ情報のコレクションです。
type nodeInfo struct {
	Name     string `json:"name"`
	Node     string `json:"node"`
	Port     int    `json:"port"`
	Network  string `json:"net"`
	Protocol string `json:"protocol"`
	API      string `json:"api"`
	Os       string `json:"os"`
	OsVer    string `json:"os_v"`
	Client   string `json:"client"`
	History  bool   `json:"canUpdateHistory"`
}

// authMsg is the authentication infos needed to login to a monitoring server.
// authMsgは、監視サーバーにログインするために必要な認証情報です。
type authMsg struct {
	ID     string   `json:"id"`
	Info   nodeInfo `json:"info"`
	Secret string   `json:"secret"`
}

// login tries to authorize the client at the remote server.
// ログインは、リモートサーバーでクライアントを承認しようとします。
func (s *Service) login(conn *connWrapper) error {
	// Construct and send the login authentication
	// ログイン認証を作成して送信します
	infos := s.server.NodeInfo()

	var protocols []string
	for _, proto := range s.server.Protocols {
		protocols = append(protocols, fmt.Sprintf("%s/%d", proto.Name, proto.Version))
	}
	var network string
	if info := infos.Protocols["eth"]; info != nil {
		network = fmt.Sprintf("%d", info.(*ethproto.NodeInfo).Network)
	} else {
		network = fmt.Sprintf("%d", infos.Protocols["les"].(*les.NodeInfo).Network)
	}
	auth := &authMsg{
		ID: s.node,
		Info: nodeInfo{
			Name:     s.node,
			Node:     infos.Name,
			Port:     infos.Ports.Listener,
			Network:  network,
			Protocol: strings.Join(protocols, ", "),
			API:      "No",
			Os:       runtime.GOOS,
			OsVer:    runtime.GOARCH,
			Client:   "0.1.1",
			History:  true,
		},
		Secret: s.pass,
	}
	login := map[string][]interface{}{
		"emit": {"hello", auth},
	}
	if err := conn.WriteJSON(login); err != nil {
		return err
	}
	// Retrieve the remote ack or connection termination
	// リモートACKまたは接続終了を取得します
	var ack map[string][]string
	if err := conn.ReadJSON(&ack); err != nil || len(ack["emit"]) != 1 || ack["emit"][0] != "ready" {
		return errors.New("unauthorized")
	}
	return nil
}

// report collects all possible data to report and send it to the stats server.
// This should only be used on reconnects or rarely to avoid overloading the
// server. Use the individual methods for reporting subscribed events.
// reportは、レポートする可能性のあるすべてのデータを収集し、それを統計サーバーに送信します。
// これは、再接続時にのみ使用するか、サーバーの過負荷を回避するためにまれに使用する必要があります。
// サブスクライブされたイベントをレポートするには、個々のメソッドを使用します。
func (s *Service) report(conn *connWrapper) error {
	if err := s.reportLatency(conn); err != nil {
		return err
	}
	if err := s.reportBlock(conn, nil); err != nil {
		return err
	}
	if err := s.reportPending(conn); err != nil {
		return err
	}
	if err := s.reportStats(conn); err != nil {
		return err
	}
	return nil
}

// reportLatency sends a ping request to the server, measures the RTT time and
// finally sends a latency update.
// reportLatencyはサーバーにping要求を送信し、RTT時間を測定し、最後に遅延更新を送信します。
func (s *Service) reportLatency(conn *connWrapper) error {
	// Send the current time to the ethstats server
	// 現在の時刻をethstatsサーバーに送信します
	start := time.Now()

	ping := map[string][]interface{}{
		"emit": {"node-ping", map[string]string{
			"id":         s.node,
			"clientTime": start.String(),
		}},
	}
	if err := conn.WriteJSON(ping); err != nil {
		return err
	}
	// Wait for the pong request to arrive back
	// ポンリクエストが戻ってくるのを待ちます
	select {
	case <-s.pongCh:
		// Pong delivered, report the latency
		// Pongが配信され、レイテンシが報告されます
	case <-time.After(5 * time.Second):
		// Ping timeout, abort
		// pingタイムアウト、中止
		return errors.New("ping timed out")
	}
	latency := strconv.Itoa(int((time.Since(start) / time.Duration(2)).Nanoseconds() / 1000000))

	// Send back the measured latency
	// 測定されたレイテンシを送り返します
	log.Trace("Sending measured latency to ethstats", "latency", latency) // 測定されたレイテンシーをethstatsに送信する

	stats := map[string][]interface{}{
		"emit": {"latency", map[string]string{
			"id":      s.node,
			"latency": latency,
		}},
	}
	return conn.WriteJSON(stats)
}

// blockStats is the information to report about individual blocks.
// blockStatsは、個々のブロックについて報告する情報です。
type blockStats struct {
	Number     *big.Int       `json:"number"`
	Hash       common.Hash    `json:"hash"`
	ParentHash common.Hash    `json:"parentHash"`
	Timestamp  *big.Int       `json:"timestamp"`
	Miner      common.Address `json:"miner"`
	GasUsed    uint64         `json:"gasUsed"`
	GasLimit   uint64         `json:"gasLimit"`
	Diff       string         `json:"difficulty"`
	TotalDiff  string         `json:"totalDifficulty"`
	Txs        []txStats      `json:"transactions"`
	TxHash     common.Hash    `json:"transactionsRoot"`
	Root       common.Hash    `json:"stateRoot"`
	Uncles     uncleStats     `json:"uncles"`
}

// txStats is the information to report about individual transactions.
// txStatsは、個々のトランザクションについてレポートするための情報です。
type txStats struct {
	Hash common.Hash `json:"hash"`
}

// uncleStats is a custom wrapper around an uncle array to force serializing
// empty arrays instead of returning null for them.
// uncleStatsは、空の配列にnullを返す代わりに、空の配列を強制的にシリアル化するための、叔父の配列のカスタムラッパーです。
type uncleStats []*types.Header

func (s uncleStats) MarshalJSON() ([]byte, error) {
	if uncles := ([]*types.Header)(s); len(uncles) > 0 {
		return json.Marshal(uncles)
	}
	return []byte("[]"), nil
}

// reportBlock retrieves the current chain head and reports it to the stats server.
// reportBlockは現在のチェーンヘッドを取得し、それを統計サーバーに報告します。
func (s *Service) reportBlock(conn *connWrapper, block *types.Block) error {
	// Gather the block details from the header or block chain
	// ヘッダーまたはブロックチェーンからブロックの詳細を収集します
	details := s.assembleBlockStats(block)

	// Assemble the block report and send it to the server
	// ブロックレポートをアセンブルしてサーバーに送信します
	log.Trace("Sending new block to ethstats", "number", details.Number, "hash", details.Hash) // ethstatsに新しいブロックを送信する

	stats := map[string]interface{}{
		"id":    s.node,
		"block": details,
	}
	report := map[string][]interface{}{
		"emit": {"block", stats},
	}
	return conn.WriteJSON(report)
}

// assembleBlockStats retrieves any required metadata to report a single block
// and assembles the block stats. If block is nil, the current head is processed.
// assembleBlockStatsは、単一のブロックを報告するために必要なメタデータを取得し、
// ブロック統計をアセンブルします。
// ブロックがnilの場合、現在のヘッドが処理されます。
func (s *Service) assembleBlockStats(block *types.Block) *blockStats {
	// Gather the block infos from the local blockchain
	// ローカルブロックチェーンからブロック情報を収集します
	var (
		header *types.Header
		td     *big.Int
		txs    []txStats
		uncles []*types.Header
	)

	// check if backend is a full node
	// バックエンドがフルノードかどうかを確認します
	fullBackend, ok := s.backend.(fullNodeBackend)
	if ok {
		if block == nil {
			block = fullBackend.CurrentBlock()
		}
		header = block.Header()
		td = fullBackend.GetTd(context.Background(), header.Hash())

		txs = make([]txStats, len(block.Transactions()))
		for i, tx := range block.Transactions() {
			txs[i].Hash = tx.Hash()
		}
		uncles = block.Uncles()
	} else {
		// Light nodes would need on-demand lookups for transactions/uncles, skip
		// ライトノードはトランザクション/叔父のオンデマンドルックアップを必要とします、スキップします
		if block != nil {
			header = block.Header()
		} else {
			header = s.backend.CurrentHeader()
		}
		td = s.backend.GetTd(context.Background(), header.Hash())
		txs = []txStats{}
	}

	// Assemble and return the block stats
	// ブロック統計をアセンブルして返します
	author, _ := s.engine.Author(header)

	return &blockStats{
		Number:     header.Number,
		Hash:       header.Hash(),
		ParentHash: header.ParentHash,
		Timestamp:  new(big.Int).SetUint64(header.Time),
		Miner:      author,
		GasUsed:    header.GasUsed,
		GasLimit:   header.GasLimit,
		Diff:       header.Difficulty.String(),
		TotalDiff:  td.String(),
		Txs:        txs,
		TxHash:     header.TxHash,
		Root:       header.Root,
		Uncles:     uncles,
	}
}

// reportHistory retrieves the most recent batch of blocks and reports it to the
// stats server.
// reportHistoryはブロックの最新のバッチを取得し、それを統計サーバーに報告します。
func (s *Service) reportHistory(conn *connWrapper, list []uint64) error {
	// Figure out the indexes that need reporting
	// レポートが必要なインデックスを把握します
	indexes := make([]uint64, 0, historyUpdateRange)
	if len(list) > 0 {
		// Specific indexes requested, send them back in particular
		// 要求された特定のインデックス、特にそれらを送り返します
		indexes = append(indexes, list...)
	} else {
		// No indexes requested, send back the top ones
		// インデックスは要求されていません、上位のインデックスを送り返します
		head := s.backend.CurrentHeader().Number.Int64()
		start := head - historyUpdateRange + 1
		if start < 0 {
			start = 0
		}
		for i := uint64(start); i <= uint64(head); i++ {
			indexes = append(indexes, i)
		}
	}
	// Gather the batch of blocks to report
	// レポートするブロックのバッチを収集します
	history := make([]*blockStats, len(indexes))
	for i, number := range indexes {
		fullBackend, ok := s.backend.(fullNodeBackend)
		// Retrieve the next block if it's known to us
		// 次のブロックがわかっている場合は、それを取得します
		var block *types.Block
		if ok {
			block, _ = fullBackend.BlockByNumber(context.Background(), rpc.BlockNumber(number)) // TODO ignore error here ?
		} else {
			if header, _ := s.backend.HeaderByNumber(context.Background(), rpc.BlockNumber(number)); header != nil {
				block = types.NewBlockWithHeader(header)
			}
		}
		// If we do have the block, add to the history and continue
		// ブロックがある場合は、履歴に追加して続行します
		if block != nil {
			history[len(history)-1-i] = s.assembleBlockStats(block)
			continue
		}
		// Ran out of blocks, cut the report short and send
		// ブロックを使い果たし、レポートを短くして送信します
		history = history[len(history)-i:]
		break
	}
	// Assemble the history report and send it to the server
	// 履歴レポートを作成し、サーバーに送信します
	if len(history) > 0 {
		log.Trace("Sending historical blocks to ethstats", "first", history[0].Number, "last", history[len(history)-1].Number) // 履歴ブロックをethstatsに送信する
	} else {
		log.Trace("No history to send to stats server") // 統計サーバーに送信する履歴がありません
	}
	stats := map[string]interface{}{
		"id":      s.node,
		"history": history,
	}
	report := map[string][]interface{}{
		"emit": {"history", stats},
	}
	return conn.WriteJSON(report)
}

// pendStats is the information to report about pending transactions.
// pendStatsは、保留中のトランザクションについて報告する情報です。
type pendStats struct {
	Pending int `json:"pending"`
}

// reportPending retrieves the current number of pending transactions and reports
// it to the stats server.
// reportPendingは、保留中のトランザクションの現在の数を取得し、それを統計サーバーに報告します。
func (s *Service) reportPending(conn *connWrapper) error {
	// Retrieve the pending count from the local blockchain
	// ローカルブロックチェーンから保留中のカウントを取得します
	pending, _ := s.backend.Stats()
	// Assemble the transaction stats and send it to the server
	// トランザクション統計をアセンブルし、サーバーに送信します
	log.Trace("Sending pending transactions to ethstats", "count", pending) // 保留中のトランザクションをethstatsに送信する

	stats := map[string]interface{}{
		"id": s.node,
		"stats": &pendStats{
			Pending: pending,
		},
	}
	report := map[string][]interface{}{
		"emit": {"pending", stats},
	}
	return conn.WriteJSON(report)
}

// nodeStats is the information to report about the local node.
// nodeStatsは、ローカルノードについて報告する情報です。
type nodeStats struct {
	Active   bool `json:"active"`
	Syncing  bool `json:"syncing"`
	Mining   bool `json:"mining"`
	Hashrate int  `json:"hashrate"`
	Peers    int  `json:"peers"`
	GasPrice int  `json:"gasPrice"`
	Uptime   int  `json:"uptime"`
}

// reportStats retrieves various stats about the node at the networking and
// mining layer and reports it to the stats server.
// reportStatsは、ネットワークおよびマイニングレイヤーのノードに関するさまざまな統計を取得し、統計サーバーに報告します。
func (s *Service) reportStats(conn *connWrapper) error {
	// Gather the syncing and mining infos from the local miner instance
	// ローカルマイナーインスタンスから同期およびマイニング情報を収集します
	var (
		mining   bool
		hashrate int
		syncing  bool
		gasprice int
	)
	// check if backend is a full node
	// バックエンドがフルノードかどうかを確認します
	fullBackend, ok := s.backend.(fullNodeBackend)
	if ok {
		mining = fullBackend.Miner().Mining()
		hashrate = int(fullBackend.Miner().Hashrate())

		sync := fullBackend.SyncProgress()
		syncing = fullBackend.CurrentHeader().Number.Uint64() >= sync.HighestBlock

		price, _ := fullBackend.SuggestGasTipCap(context.Background())
		gasprice = int(price.Uint64())
		if basefee := fullBackend.CurrentHeader().BaseFee; basefee != nil {
			gasprice += int(basefee.Uint64())
		}
	} else {
		sync := s.backend.SyncProgress()
		syncing = s.backend.CurrentHeader().Number.Uint64() >= sync.HighestBlock
	}
	// Assemble the node stats and send it to the server
	// ノード統計をアセンブルし、サーバーに送信します
	log.Trace("Sending node details to ethstats") // ノードの詳細をethstatsに送信する

	stats := map[string]interface{}{
		"id": s.node,
		"stats": &nodeStats{
			Active:   true,
			Mining:   mining,
			Hashrate: hashrate,
			Peers:    s.server.PeerCount(),
			GasPrice: gasprice,
			Syncing:  syncing,
			Uptime:   100,
		},
	}
	report := map[string][]interface{}{
		"emit": {"stats", stats},
	}
	return conn.WriteJSON(report)
}
