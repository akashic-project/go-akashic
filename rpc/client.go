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

package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/log"
)

var (
	ErrClientQuit                = errors.New("client is closed")
	ErrNoResult                  = errors.New("no result in JSON-RPC response")
	ErrSubscriptionQueueOverflow = errors.New("subscription queue overflow")
	errClientReconnected         = errors.New("client reconnected")
	errDead                      = errors.New("connection lost")
)

const (
	// Timeouts
	defaultDialTimeout = 10 * time.Second // コンテキストに期限がない場合に使用 // used if context has no deadline
	subscribeTimeout   = 5 * time.Second  // 全体的なタイムアウトeth_subscribe、rpc_modules呼び出し // overall timeout eth_subscribe, rpc_modules calls
)

const (
	// Subscriptions are removed when the subscriber cannot keep up.
	//
	// This can be worked around by supplying a channel with sufficiently sized buffer,
	// but this can be inconvenient and hard to explain in the docs. Another issue with
	// buffered channels is that the buffer is static even though it might not be needed
	// most of the time.
	//
	// The approach taken here is to maintain a per-subscription linked list buffer
	// shrinks on demand. If the buffer reaches the size below, the subscription is
	// dropped.
	// サブスクライバーが追いつけない場合、サブスクリプションは削除されます。
	//
	// これは、十分なサイズのバッファをチャネルに提供することで回避できますが、これは不便であり、ドキュメントで説明するのが難しい場合があります。
	// バッファリングされたチャネルのもう1つの問題は、ほとんどの場合必要ではない場合でも、バッファが静的であるということです。
	//
	// ここで採用されているアプローチは、サブスクリプションごとのリンクリストバッファがオンデマンドで縮小するように維持することです。
	// バッファが以下のサイズに達すると、サブスクリプションはドロップされます。
	maxClientSubscriptionBuffer = 20000
)

const (
	httpScheme = "http"
	wsScheme   = "ws"
	ipcScheme  = "ipc"
)

// BatchElem is an element in a batch request.
// BatchElemはバッチリクエストの要素です。
type BatchElem struct {
	Method string
	Args   []interface{}
	// The result is unmarshaled into this field. Result must be set to a
	// non-nil pointer value of the desired type, otherwise the response will be
	// discarded.
	// 結果はこのフィールドにマーシャリングされません。結果は、
	// 目的のタイプの非nilポインター値に設定する必要があります。そうしないと、応答が破棄されます。
	Result interface{}
	// Error is set if the server returns an error for this request, or if
	// unmarshaling into Result fails. It is not set for I/O errors.
	// サーバーがこのリクエストに対してエラーを返した場合、
	// またはResultへのアンマーシャリングが失敗した場合、エラーが設定されます。
	// I / Oエラーには設定されていません。
	Error error
}

// Client represents a connection to an RPC server.
// クライアントはRPCサーバーへの接続を表します。
type Client struct {
	idgen    func() ID // サブスクリプションの場合 // for subscriptions
	scheme   string    // 接続タイプ：http、wsまたはipc // connection type: http, ws or ipc
	services *serviceRegistry

	idCounter uint32

	// This function, if non-nil, is called when the connection is lost.
	// この関数は、nil以外の場合、接続が失われたときに呼び出されます。
	reconnectFunc reconnectFunc

	// writeConn is used for writing to the connection on the caller's goroutine. It should
	// only be accessed outside of dispatch, with the write lock held. The write lock is
	// taken by sending on reqInit and released by sending on reqSent.
	// writeConnは、呼び出し元のゴルーチンの接続への書き込みに使用されます。
	// 書き込みロックを保持したまま、ディスパッチ外でのみアクセスする必要があります。
	// 書き込みロックは、reqInitで送信することによって取得され、
	// reqSentで送信することによって解放されます。
	writeConn jsonWriter

	// for dispatch
	// ディスパッチ用
	close       chan struct{}
	closing     chan struct{}    // クライアントが終了するときに閉じます // closed when client is quitting
	didClose    chan struct{}    // クライアントが終了すると閉じます // closed when client quits
	reconnected chan ServerCodec // write / reconnectが新しい接続を送信する場所 // where write/reconnect sends the new connection
	readOp      chan readOp      // メッセージを読む // read messages
	readErr     chan error       // 読み取りからのエラー // errors from read
	reqInit     chan *requestOp  // 応答IDを登録し、書き込みロックを取得します // register response IDs, takes write lock
	reqSent     chan error       // 書き込み完了を通知し、書き込みロックを解放します // signals write completion, releases write lock
	reqTimeout  chan *requestOp  // 呼び出しタイムアウトの期限が切れると応答IDを削除します // removes response IDs when call timeout expires
}

type reconnectFunc func(ctx context.Context) (ServerCodec, error)

type clientContextKey struct{}

type clientConn struct {
	codec   ServerCodec
	handler *handler
}

func (c *Client) newClientConn(conn ServerCodec) *clientConn {
	ctx := context.WithValue(context.Background(), clientContextKey{}, c)
	// Http connections have already set the scheme
	// Http接続はすでにスキームを設定しています
	if !c.isHTTP() && c.scheme != "" {
		ctx = context.WithValue(ctx, "scheme", c.scheme)
	}
	handler := newHandler(ctx, conn, c.idgen, c.services)
	return &clientConn{conn, handler}
}

func (cc *clientConn) close(err error, inflightReq *requestOp) {
	cc.handler.close(err, inflightReq)
	cc.codec.close()
}

type readOp struct {
	msgs  []*jsonrpcMessage
	batch bool
}

type requestOp struct {
	ids  []json.RawMessage
	err  error
	resp chan *jsonrpcMessage // 最大len（ids）の応答を受信します // receives up to len(ids) responses
	sub  *ClientSubscription  // EthSubscribeリクエストに対してのみ設定 // only set for EthSubscribe requests
}

func (op *requestOp) wait(ctx context.Context, c *Client) (*jsonrpcMessage, error) {
	select {
	case <-ctx.Done():
		// Send the timeout to dispatch so it can remove the request IDs.
		// タイムアウトを送信してディスパッチし、リクエストIDを削除できるようにします。
		if !c.isHTTP() {
			select {
			case c.reqTimeout <- op:
			case <-c.closing:
			}
		}
		return nil, ctx.Err()
	case resp := <-op.resp:
		return resp, op.err
	}
}

// Dial creates a new client for the given URL.
//
// The currently supported URL schemes are "http", "https", "ws" and "wss". If rawurl is a
// file name with no URL scheme, a local socket connection is established using UNIX
// domain sockets on supported platforms and named pipes on Windows. If you want to
// configure transport options, use DialHTTP, DialWebsocket or DialIPC instead.
//
// For websocket connections, the origin is set to the local host name.
//
// The client reconnects automatically if the connection is lost.
// Dialは、指定されたURLの新しいクライアントを作成します。
//
// 現在サポートされているURLスキームは、「http」、「https」、「ws」、「wss」です。
// rawurlがURLスキームのないファイル名の場合、UNIXを使用してローカルソケット接続が確立されます
// サポートされているプラ​​ットフォームのドメインソケットとWindowsの名前付きパイプ。
// トランスポートオプションを構成する場合は、代わりにDialHTTP、DialWebsocket、またはDialIPCを使用してください。
//
// WebSocket接続の場合、オリジンはローカルホスト名に設定されます。
//
// 接続が失われた場合、クライアントは自動的に再接続します。
func Dial(rawurl string) (*Client, error) {
	return DialContext(context.Background(), rawurl)
}

// DialContext creates a new RPC client, just like Dial.
//
// The context is used to cancel or time out the initial connection establishment. It does
// not affect subsequent interactions with the client.

// DialContextは、Dialと同様に、新しいRPCクライアントを作成します。
//
// コンテキストは、最初の接続確立をキャンセルまたはタイムアウトするために使用されます。
// その後のクライアントとのやり取りには影響しません。
func DialContext(ctx context.Context, rawurl string) (*Client, error) {
	u, err := url.Parse(rawurl)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "http", "https":
		return DialHTTP(rawurl)
	case "ws", "wss":
		return DialWebsocket(ctx, rawurl, "")
	case "stdio":
		return DialStdIO(ctx)
	case "":
		return DialIPC(ctx, rawurl)
	default:
		return nil, fmt.Errorf("no known transport for URL scheme %q", u.Scheme)
	}
}

// Client retrieves the client from the context, if any. This can be used to perform
// 'reverse calls' in a handler method.
// クライアントは、コンテキストからクライアントを取得します（存在する場合）。
// これは、ハンドラーメソッドで「逆呼び出し」を実行するために使用できます。
func ClientFromContext(ctx context.Context) (*Client, bool) {
	client, ok := ctx.Value(clientContextKey{}).(*Client)
	return client, ok
}

func newClient(initctx context.Context, connect reconnectFunc) (*Client, error) {
	conn, err := connect(initctx)
	if err != nil {
		return nil, err
	}
	c := initClient(conn, randomIDGenerator(), new(serviceRegistry))
	c.reconnectFunc = connect
	return c, nil
}

func initClient(conn ServerCodec, idgen func() ID, services *serviceRegistry) *Client {
	scheme := ""
	switch conn.(type) {
	case *httpConn:
		scheme = httpScheme
	case *websocketCodec:
		scheme = wsScheme
	case *jsonCodec:
		scheme = ipcScheme
	}
	c := &Client{
		idgen:       idgen,
		scheme:      scheme,
		services:    services,
		writeConn:   conn,
		close:       make(chan struct{}),
		closing:     make(chan struct{}),
		didClose:    make(chan struct{}),
		reconnected: make(chan ServerCodec),
		readOp:      make(chan readOp),
		readErr:     make(chan error),
		reqInit:     make(chan *requestOp),
		reqSent:     make(chan error, 1),
		reqTimeout:  make(chan *requestOp),
	}
	if !c.isHTTP() {
		go c.dispatch(conn)
	}
	return c
}

// RegisterName creates a service for the given receiver type under the given name. When no
// methods on the given receiver match the criteria to be either a RPC method or a
// subscription an error is returned. Otherwise a new service is created and added to the
// service collection this client provides to the server.

// RegisterNameは、指定された名前で指定されたレシーバータイプのサービスを作成します。
// 指定されたレシーバーのメソッドがRPCメソッドまたはサブスクリプションのいずれかであるという基準に一致しない場合、エラーが返されます。
// それ以外の場合は、新しいサービスが作成され、このクライアントがサーバーに提供するサービスコレクションに追加されます。
func (c *Client) RegisterName(name string, receiver interface{}) error {
	return c.services.registerName(name, receiver)
}

func (c *Client) nextID() json.RawMessage {
	id := atomic.AddUint32(&c.idCounter, 1)
	return strconv.AppendUint(nil, uint64(id), 10)
}

// SupportedModules calls the rpc_modules method, retrieving the list of
// APIs that are available on the server.
// SupportedModulesはrpc_modulesメソッドを呼び出し、
// サーバーで使用可能なAPIのリストを取得します。
func (c *Client) SupportedModules() (map[string]string, error) {
	var result map[string]string
	ctx, cancel := context.WithTimeout(context.Background(), subscribeTimeout)
	defer cancel()
	err := c.CallContext(ctx, &result, "rpc_modules")
	return result, err
}

// Close closes the client, aborting any in-flight requests.
// Closeはクライアントを閉じ、処理中のリクエストをすべて中止します。
func (c *Client) Close() {
	if c.isHTTP() {
		return
	}
	select {
	case c.close <- struct{}{}:
		<-c.didClose
	case <-c.didClose:
	}
}

// SetHeader adds a custom HTTP header to the client's requests.
// This method only works for clients using HTTP, it doesn't have
// any effect for clients using another transport.
// SetHeaderは、クライアントのリクエストにカスタムHTTPヘッダーを追加します。
// このメソッドは、HTTPを使用するクライアントに対してのみ機能し、
// 別のトランスポートを使用するクライアントには影響しません。
func (c *Client) SetHeader(key, value string) {
	if !c.isHTTP() {
		return
	}
	conn := c.writeConn.(*httpConn)
	conn.mu.Lock()
	conn.headers.Set(key, value)
	conn.mu.Unlock()
}

// Call performs a JSON-RPC call with the given arguments and unmarshals into
// result if no error occurred.
//
// The result must be a pointer so that package json can unmarshal into it. You
// can also pass nil, in which case the result is ignored.
// Callは、指定された引数を使用してJSON-RPC呼び出しを実行し、
// エラーが発生しなかった場合は結果にアンマーシャリングします。
//
// パッケージjsonがアンマーシャリングできるように、結果はポインタである必要があります。
// nilを渡すこともできます。その場合、結果は無視されます。
func (c *Client) Call(result interface{}, method string, args ...interface{}) error {
	ctx := context.Background()
	return c.CallContext(ctx, result, method, args...)
}

// CallContext performs a JSON-RPC call with the given arguments. If the context is
// canceled before the call has successfully returned, CallContext returns immediately.
//
// The result must be a pointer so that package json can unmarshal into it. You
// can also pass nil, in which case the result is ignored.
// CallContextは、指定された引数を使用してJSON-RPC呼び出しを実行します。
// 呼び出しが正常に戻る前にコンテキストがキャンセルされた場合、CallContextはすぐに戻ります。
//
// パッケージjsonがアンマーシャリングできるように、結果はポインタである必要があります。
// nilを渡すこともできます。その場合、結果は無視されます。
func (c *Client) CallContext(ctx context.Context, result interface{}, method string, args ...interface{}) error {
	if result != nil && reflect.TypeOf(result).Kind() != reflect.Ptr {
		return fmt.Errorf("call result parameter must be pointer or nil interface: %v", result)
	}
	msg, err := c.newMessage(method, args...)
	if err != nil {
		return err
	}
	op := &requestOp{ids: []json.RawMessage{msg.ID}, resp: make(chan *jsonrpcMessage, 1)}

	if c.isHTTP() {
		err = c.sendHTTP(ctx, op, msg)
	} else {
		err = c.send(ctx, op, msg)
	}
	if err != nil {
		return err
	}

	// dispatch has accepted the request and will close the channel when it quits.
	// ディスパッチはリクエストを受け入れ、終了するとチャネルを閉じます。
	switch resp, err := op.wait(ctx, c); {
	case err != nil:
		return err
	case resp.Error != nil:
		return resp.Error
	case len(resp.Result) == 0:
		return ErrNoResult
	default:
		return json.Unmarshal(resp.Result, &result)
	}
}

// BatchCall sends all given requests as a single batch and waits for the server
// to return a response for all of them.
//
// In contrast to Call, BatchCall only returns I/O errors. Any error specific to
// a request is reported through the Error field of the corresponding BatchElem.
//
// Note that batch calls may not be executed atomically on the server side.
// BatchCallは、指定されたすべてのリクエストを単一のバッチとして送信し、サーバーを待機します
// それらすべての応答を返します。
//
// Callとは対照的に、BatchCallはI / Oエラーのみを返します。リクエストに固有のエラーは、対応するBatchElemのエラーフィールドを通じて報告されます。
//
// バッチ呼び出しはサーバー側でアトミックに実行されない場合があることに注意してください。
func (c *Client) BatchCall(b []BatchElem) error {
	ctx := context.Background()
	return c.BatchCallContext(ctx, b)
}

// BatchCall sends all given requests as a single batch and waits for the server
// to return a response for all of them. The wait duration is bounded by the
// context's deadline.
//
// In contrast to CallContext, BatchCallContext only returns errors that have occurred
// while sending the request. Any error specific to a request is reported through the
// Error field of the corresponding BatchElem.
//
// Note that batch calls may not be executed atomically on the server side.
// BatchCallは、指定されたすべてのリクエストを単一のバッチとして送信し、
// サーバーがすべてのリクエストに対する応答を返すのを待ちます。
// 待機時間は、コンテキストの期限によって制限されます。
//
// CallContextとは対照的に、BatchCallContextはリクエストの送信中に発生したエラーのみを返します。
// クエストに固有のエラーは、対応するBatchElemのエラーフィールドを通じて報告されます。
//
// バッチ呼び出しはサーバー側でアトミックに実行されない場合があることに注意してください。
func (c *Client) BatchCallContext(ctx context.Context, b []BatchElem) error {
	var (
		msgs = make([]*jsonrpcMessage, len(b))
		byID = make(map[string]int, len(b))
	)
	op := &requestOp{
		ids:  make([]json.RawMessage, len(b)),
		resp: make(chan *jsonrpcMessage, len(b)),
	}
	for i, elem := range b {
		msg, err := c.newMessage(elem.Method, elem.Args...)
		if err != nil {
			return err
		}
		msgs[i] = msg
		op.ids[i] = msg.ID
		byID[string(msg.ID)] = i
	}

	var err error
	if c.isHTTP() {
		err = c.sendBatchHTTP(ctx, op, msgs)
	} else {
		err = c.send(ctx, op, msgs)
	}

	// Wait for all responses to come back.
	// すべての応答が返されるのを待ちます。
	for n := 0; n < len(b) && err == nil; n++ {
		var resp *jsonrpcMessage
		resp, err = op.wait(ctx, c)
		if err != nil {
			break
		}
		// Find the element corresponding to this response.
		// The element is guaranteed to be present because dispatch
		// only sends valid IDs to our channel.
		// この応答に対応する要素を検索します。
		//ディスパッチは有効なIDのみをチャネルに送信するため、要素は存在することが保証されます。
		elem := &b[byID[string(resp.ID)]]
		if resp.Error != nil {
			elem.Error = resp.Error
			continue
		}
		if len(resp.Result) == 0 {
			elem.Error = ErrNoResult
			continue
		}
		elem.Error = json.Unmarshal(resp.Result, elem.Result)
	}
	return err
}

// Notify sends a notification, i.e. a method call that doesn't expect a response.
func (c *Client) Notify(ctx context.Context, method string, args ...interface{}) error {
	op := new(requestOp)
	msg, err := c.newMessage(method, args...)
	if err != nil {
		return err
	}
	msg.ID = nil

	if c.isHTTP() {
		return c.sendHTTP(ctx, op, msg)
	}
	return c.send(ctx, op, msg)
}

// EthSubscribe registers a subscription under the "eth" namespace.
func (c *Client) EthSubscribe(ctx context.Context, channel interface{}, args ...interface{}) (*ClientSubscription, error) {
	return c.Subscribe(ctx, "eth", channel, args...)
}

// ShhSubscribe registers a subscription under the "shh" namespace.
// Deprecated: use Subscribe(ctx, "shh", ...).
func (c *Client) ShhSubscribe(ctx context.Context, channel interface{}, args ...interface{}) (*ClientSubscription, error) {
	return c.Subscribe(ctx, "shh", channel, args...)
}

// Subscribe calls the "<namespace>_subscribe" method with the given arguments,
// registering a subscription. Server notifications for the subscription are
// sent to the given channel. The element type of the channel must match the
// expected type of content returned by the subscription.
//
// The context argument cancels the RPC request that sets up the subscription but has no
// effect on the subscription after Subscribe has returned.
//
// Slow subscribers will be dropped eventually. Client buffers up to 20000 notifications
// before considering the subscriber dead. The subscription Err channel will receive
// ErrSubscriptionQueueOverflow. Use a sufficiently large buffer on the channel or ensure
// that the channel usually has at least one reader to prevent this issue.
func (c *Client) Subscribe(ctx context.Context, namespace string, channel interface{}, args ...interface{}) (*ClientSubscription, error) {
	// Check type of channel first.
	chanVal := reflect.ValueOf(channel)
	if chanVal.Kind() != reflect.Chan || chanVal.Type().ChanDir()&reflect.SendDir == 0 {
		panic("first argument to Subscribe must be a writable channel")
	}
	if chanVal.IsNil() {
		panic("channel given to Subscribe must not be nil")
	}
	if c.isHTTP() {
		return nil, ErrNotificationsUnsupported
	}

	msg, err := c.newMessage(namespace+subscribeMethodSuffix, args...)
	if err != nil {
		return nil, err
	}
	op := &requestOp{
		ids:  []json.RawMessage{msg.ID},
		resp: make(chan *jsonrpcMessage),
		sub:  newClientSubscription(c, namespace, chanVal),
	}

	// Send the subscription request.
	// The arrival and validity of the response is signaled on sub.quit.
	if err := c.send(ctx, op, msg); err != nil {
		return nil, err
	}
	if _, err := op.wait(ctx, c); err != nil {
		return nil, err
	}
	return op.sub, nil
}

func (c *Client) newMessage(method string, paramsIn ...interface{}) (*jsonrpcMessage, error) {
	msg := &jsonrpcMessage{Version: vsn, ID: c.nextID(), Method: method}
	if paramsIn != nil { // prevent sending "params":null
		var err error
		if msg.Params, err = json.Marshal(paramsIn); err != nil {
			return nil, err
		}
	}
	return msg, nil
}

// send registers op with the dispatch loop, then sends msg on the connection.
// if sending fails, op is deregistered.
// ディスパッチループを使用してレジスタopを送信し、接続でmsgを送信します。
// 送信に失敗した場合、opは登録解除されます。
func (c *Client) send(ctx context.Context, op *requestOp, msg interface{}) error {
	select {
	case c.reqInit <- op:
		err := c.write(ctx, msg, false)
		c.reqSent <- err
		return err
	case <-ctx.Done():
		// This can happen if the client is overloaded or unable to keep up with
		// subscription notifications.
		// これは、クライアントが過負荷になっている場合、
		// またはサブスクリプション通知に追いつけない場合に発生する可能性があります。
		return ctx.Err()
	case <-c.closing:
		return ErrClientQuit
	}
}

func (c *Client) write(ctx context.Context, msg interface{}, retry bool) error {
	// The previous write failed. Try to establish a new connection.
	// 前の書き込みが失敗しました。新しい接続を確立してみてください。
	if c.writeConn == nil {
		if err := c.reconnect(ctx); err != nil {
			return err
		}
	}
	err := c.writeConn.writeJSON(ctx, msg)
	if err != nil {
		c.writeConn = nil
		if !retry {
			return c.write(ctx, msg, true)
		}
	}
	return err
}

func (c *Client) reconnect(ctx context.Context) error {
	if c.reconnectFunc == nil {
		return errDead
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, defaultDialTimeout)
		defer cancel()
	}
	newconn, err := c.reconnectFunc(ctx)
	if err != nil {
		log.Trace("RPC client reconnect failed", "err", err)
		return err
	}
	select {
	case c.reconnected <- newconn:
		c.writeConn = newconn
		return nil
	case <-c.didClose:
		newconn.close()
		return ErrClientQuit
	}
}

// dispatch is the main loop of the client.
// It sends read messages to waiting calls to Call and BatchCall
// and subscription notifications to registered subscriptions.
func (c *Client) dispatch(codec ServerCodec) {
	var (
		lastOp      *requestOp  // tracks last send operation
		reqInitLock = c.reqInit // nil while the send lock is held
		conn        = c.newClientConn(codec)
		reading     = true
	)
	defer func() {
		close(c.closing)
		if reading {
			conn.close(ErrClientQuit, nil)
			c.drainRead()
		}
		close(c.didClose)
	}()

	// Spawn the initial read loop.
	go c.read(codec)

	for {
		select {
		case <-c.close:
			return

		// Read path:
		case op := <-c.readOp:
			if op.batch {
				conn.handler.handleBatch(op.msgs)
			} else {
				conn.handler.handleMsg(op.msgs[0])
			}

		case err := <-c.readErr:
			conn.handler.log.Debug("RPC connection read error", "err", err)
			conn.close(err, lastOp)
			reading = false

		// Reconnect:
		case newcodec := <-c.reconnected:
			log.Debug("RPC client reconnected", "reading", reading, "conn", newcodec.remoteAddr())
			if reading {
				// Wait for the previous read loop to exit. This is a rare case which
				// happens if this loop isn't notified in time after the connection breaks.
				// In those cases the caller will notice first and reconnect. Closing the
				// handler terminates all waiting requests (closing op.resp) except for
				// lastOp, which will be transferred to the new handler.
				conn.close(errClientReconnected, lastOp)
				c.drainRead()
			}
			go c.read(newcodec)
			reading = true
			conn = c.newClientConn(newcodec)
			// Re-register the in-flight request on the new handler
			// because that's where it will be sent.
			conn.handler.addRequestOp(lastOp)

		// Send path:
		case op := <-reqInitLock:
			// Stop listening for further requests until the current one has been sent.
			reqInitLock = nil
			lastOp = op
			conn.handler.addRequestOp(op)

		case err := <-c.reqSent:
			if err != nil {
				// Remove response handlers for the last send. When the read loop
				// goes down, it will signal all other current operations.
				conn.handler.removeRequestOp(lastOp)
			}
			// Let the next request in.
			reqInitLock = c.reqInit
			lastOp = nil

		case op := <-c.reqTimeout:
			conn.handler.removeRequestOp(op)
		}
	}
}

// drainRead drops read messages until an error occurs.
func (c *Client) drainRead() {
	for {
		select {
		case <-c.readOp:
		case <-c.readErr:
			return
		}
	}
}

// read decodes RPC messages from a codec, feeding them into dispatch.
func (c *Client) read(codec ServerCodec) {
	for {
		msgs, batch, err := codec.readBatch()
		if _, ok := err.(*json.SyntaxError); ok {
			codec.writeJSON(context.Background(), errorMessage(&parseError{err.Error()}))
		}
		if err != nil {
			c.readErr <- err
			return
		}
		c.readOp <- readOp{msgs, batch}
	}
}

func (c *Client) isHTTP() bool {
	return c.scheme == httpScheme
}
