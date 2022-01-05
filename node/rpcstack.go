// Copyright 2020 The go-ethereum Authors
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
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/rs/cors"
)

// httpConfig is the JSON-RPC/HTTP configuration.
// httpConfigはJSON-RPC / HTTP構成です。
type httpConfig struct {
	Modules            []string
	CorsAllowedOrigins []string
	Vhosts             []string
	prefix             string // httpハンドラーをマウントするパスプレフィックス // path prefix on which to mount http handler
}

// wsConfig is the JSON-RPC/Websocket configuration
// wsConfigはJSON-RPC / Websocket構成です
type wsConfig struct {
	Origins []string
	Modules []string
	prefix  string // wsハンドラーをマウントするパスプレフィックス// path prefix on which to mount ws handler
}

type rpcHandler struct {
	http.Handler
	server *rpc.Server
}

type httpServer struct {
	log      log.Logger
	timeouts rpc.HTTPTimeouts
	mux      http.ServeMux //登録されたハンドラーはここに移動します // registered handlers go here

	mu       sync.Mutex
	server   *http.Server
	listener net.Listener //サーバーの実行中はnil以外// non-nil when server is running

	// HTTP RPC handler things.
	// HTTPRPCハンドラーのもの。

	httpConfig  httpConfig
	httpHandler atomic.Value // *rpcHandler

	// WebSocket handler things.
	// WebSocketハンドラーのもの。
	wsConfig  wsConfig
	wsHandler atomic.Value // *rpcHandler

	// These are set by setListenAddr.
	// これらはsetListenAddrによって設定されます。
	endpoint string
	host     string
	port     int

	handlerNames map[string]string
}

func newHTTPServer(log log.Logger, timeouts rpc.HTTPTimeouts) *httpServer {
	h := &httpServer{log: log, timeouts: timeouts, handlerNames: make(map[string]string)}

	h.httpHandler.Store((*rpcHandler)(nil))
	h.wsHandler.Store((*rpcHandler)(nil))
	return h
}

// setListenAddr configures the listening address of the server.
// The address can only be set while the server isn't running.
// setListenAddrは、サーバーのリスニングアドレスを構成します。
// アドレスは、サーバーが実行されていないときにのみ設定できます。
func (h *httpServer) setListenAddr(host string, port int) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.listener != nil && (host != h.host || port != h.port) {
		return fmt.Errorf("HTTP server already running on %s", h.endpoint)
	}

	h.host, h.port = host, port
	h.endpoint = fmt.Sprintf("%s:%d", host, port)
	return nil
}

// listenAddr returns the listening address of the server.
// listenAddrは、サーバーのリスニングアドレスを返します。
func (h *httpServer) listenAddr() string {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.listener != nil {
		return h.listener.Addr().String()
	}
	return h.endpoint
}

// start starts the HTTP server if it is enabled and not already running.
// 有効になっていてまだ実行されていない場合、startはHTTPサーバーを開始します。
func (h *httpServer) start() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.endpoint == "" || h.listener != nil {
		return nil //すでに実行されているか、構成されていません // already running or not configured
	}

	// Initialize the server.
	// サーバーを初期化します。
	h.server = &http.Server{Handler: h}
	if h.timeouts != (rpc.HTTPTimeouts{}) {
		CheckTimeouts(&h.timeouts)
		h.server.ReadTimeout = h.timeouts.ReadTimeout
		h.server.WriteTimeout = h.timeouts.WriteTimeout
		h.server.IdleTimeout = h.timeouts.IdleTimeout
	}

	// Start the server.
	// サーバーを起動します。
	listener, err := net.Listen("tcp", h.endpoint)
	if err != nil {
		// If the server fails to start, we need to clear out the RPC and WS
		// configuration so they can be configured another time.
		// サーバーの起動に失敗した場合は、RPCとWSの構成をクリアして、別のときに構成できるようにする必要があります。
		h.disableRPC()
		h.disableWS()
		return err
	}
	h.listener = listener
	go h.server.Serve(listener)

	if h.wsAllowed() {
		url := fmt.Sprintf("ws://%v", listener.Addr())
		if h.wsConfig.prefix != "" {
			url += h.wsConfig.prefix
		}
		h.log.Info("WebSocket enabled", "url", url)
	}
	// if server is websocket only, return after logging
	// サーバーがWebSocketのみの場合、ログ記録後に戻る
	if !h.rpcAllowed() {
		return nil
	}
	// Log http endpoint.
	// httpエンドポイントをログに記録します。
	h.log.Info("HTTP server started",
		"endpoint", listener.Addr(),
		"prefix", h.httpConfig.prefix,
		"cors", strings.Join(h.httpConfig.CorsAllowedOrigins, ","),
		"vhosts", strings.Join(h.httpConfig.Vhosts, ","),
	)

	// Log all handlers mounted on server.
	// サーバーにマウントされているすべてのハンドラーをログに記録します。
	var paths []string
	for path := range h.handlerNames {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	logged := make(map[string]bool, len(paths))
	for _, path := range paths {
		name := h.handlerNames[path]
		if !logged[name] {
			log.Info(name+" enabled", "url", "http://"+listener.Addr().String()+path)
			logged[name] = true
		}
	}
	return nil
}

func (h *httpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// check if ws request and serve if ws enabled
	// wsがリクエストしているかどうかを確認し、wsが有効になっている場合はサービスを提供します
	ws := h.wsHandler.Load().(*rpcHandler)
	if ws != nil && isWebsocket(r) {
		if checkPath(r, h.wsConfig.prefix) {
			ws.ServeHTTP(w, r)
		}
		return
	}
	// if http-rpc is enabled, try to serve request
	// http-rpcが有効になっている場合は、リクエストを処理してみてください
	rpc := h.httpHandler.Load().(*rpcHandler)
	if rpc != nil {
		// First try to route in the mux.
		// Requests to a path below root are handled by the mux,
		// which has all the handlers registered via Node.RegisterHandler.
		// These are made available when RPC is enabled.
		// 最初にマルチプレクサでルーティングを試みます。
		// rootの下のパスへのリクエストは、Node.RegisterHandlerを介して登録されたすべてのハンドラーを持つmuxによって処理されます。
		// これらはRPCが有効になっているときに利用可能になります。
		muxHandler, pattern := h.mux.Handler(r)
		if pattern != "" {
			muxHandler.ServeHTTP(w, r)
			return
		}

		if checkPath(r, h.httpConfig.prefix) {
			rpc.ServeHTTP(w, r)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

// checkPath checks whether a given request URL matches a given path prefix.
// checkPathは、指定されたリクエストURLが指定されたパスプレフィックスと一致するかどうかをチェックします。
func checkPath(r *http.Request, path string) bool {
	// if no prefix has been specified, request URL must be on root
	// プレフィックスが指定されていない場合、リクエストURLはルート上にある必要があります
	if path == "" {
		return r.URL.Path == "/"
	}
	// otherwise, check to make sure prefix matches
	// それ以外の場合は、プレフィックスが一致することを確認します
	return len(r.URL.Path) >= len(path) && r.URL.Path[:len(path)] == path
}

// validatePrefix checks if 'path' is a valid configuration value for the RPC prefix option.
// validatePrefixは、「path」がRPCプレフィックスオプションの有効な構成値であるかどうかを確認します。
func validatePrefix(what, path string) error {
	if path == "" {
		return nil
	}
	if path[0] != '/' {
		return fmt.Errorf(`%s RPC path prefix %q does not contain leading "/"`, what, path)
	}
	if strings.ContainsAny(path, "?#") {
		// This is just to avoid confusion. While these would match correctly (i.e. they'd
		// match if URL-escaped into path), it's not easy to understand for users when
		// setting that on the command line.
		// これは混乱を避けるためだけのものです。これらは正しく一致しますが
		// （つまり、URLがパスにエスケープされている場合は一致します）、
		// コマンドラインで設定するときにユーザーが理解するのは簡単ではありません。
		return fmt.Errorf("%s RPC path prefix %q contains URL meta-characters", what, path)
	}
	return nil
}

// stop shuts down the HTTP server.
// stopはHTTPサーバーをシャットダウンします。
func (h *httpServer) stop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.doStop()
}

func (h *httpServer) doStop() {
	if h.listener == nil {
		return // not running
	}

	// Shut down the server.
	// サーバーをシャットダウンします。
	httpHandler := h.httpHandler.Load().(*rpcHandler)
	wsHandler := h.wsHandler.Load().(*rpcHandler)
	if httpHandler != nil {
		h.httpHandler.Store((*rpcHandler)(nil))
		httpHandler.server.Stop()
	}
	if wsHandler != nil {
		h.wsHandler.Store((*rpcHandler)(nil))
		wsHandler.server.Stop()
	}
	h.server.Shutdown(context.Background())
	h.listener.Close()
	h.log.Info("HTTP server stopped", "endpoint", h.listener.Addr())

	// Clear out everything to allow re-configuring it later.
	// すべてをクリアして、後で再構成できるようにします。
	h.host, h.port, h.endpoint = "", 0, ""
	h.server, h.listener = nil, nil
}

// enableRPC turns on JSON-RPC over HTTP on the server.
// enableRPCは、サーバー上でJSON-RPC overHTTPをオンにします。
func (h *httpServer) enableRPC(apis []rpc.API, config httpConfig) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.rpcAllowed() {
		return fmt.Errorf("JSON-RPC over HTTP is already enabled")
	}

	// Create RPC server and handler.
	// RPCサーバーとハンドラーを作成します。
	srv := rpc.NewServer()
	if err := RegisterApis(apis, config.Modules, srv, false); err != nil {
		return err
	}
	h.httpConfig = config
	h.httpHandler.Store(&rpcHandler{
		Handler: NewHTTPHandlerStack(srv, config.CorsAllowedOrigins, config.Vhosts),
		server:  srv,
	})
	return nil
}

// disableRPC stops the HTTP RPC handler. This is internal, the caller must hold h.mu.
// disableRPCはHTTPRPCハンドラーを停止します。これは内部的なものであり、発信者はh.muを保持する必要があります。
func (h *httpServer) disableRPC() bool {
	handler := h.httpHandler.Load().(*rpcHandler)
	if handler != nil {
		h.httpHandler.Store((*rpcHandler)(nil))
		handler.server.Stop()
	}
	return handler != nil
}

// enableWS turns on JSON-RPC over WebSocket on the server.
// enableWSは、サーバー上のWebSocketを介してJSON-RPCをオンにします。
func (h *httpServer) enableWS(apis []rpc.API, config wsConfig) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.wsAllowed() {
		return fmt.Errorf("JSON-RPC over WebSocket is already enabled")
	}

	// Create RPC server and handler.
	// RPCサーバーとハンドラーを作成します。
	srv := rpc.NewServer()
	if err := RegisterApis(apis, config.Modules, srv, false); err != nil {
		return err
	}
	h.wsConfig = config
	h.wsHandler.Store(&rpcHandler{
		Handler: srv.WebsocketHandler(config.Origins),
		server:  srv,
	})
	return nil
}

// stopWS disables JSON-RPC over WebSocket and also stops the server if it only serves WebSocket.
// stopWSはWebSocketを介したJSON-RPCを無効にし、WebSocketのみを提供する場合はサーバーも停止します。
func (h *httpServer) stopWS() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.disableWS() {
		if !h.rpcAllowed() {
			h.doStop()
		}
	}
}

// disableWS disables the WebSocket handler. This is internal, the caller must hold h.mu.
// disableWSは、WebSocketハンドラーを無効にします。これは内部的なものであり、発信者はh.muを保持する必要があります。
func (h *httpServer) disableWS() bool {
	ws := h.wsHandler.Load().(*rpcHandler)
	if ws != nil {
		h.wsHandler.Store((*rpcHandler)(nil))
		ws.server.Stop()
	}
	return ws != nil
}

// rpcAllowed returns true when JSON-RPC over HTTP is enabled.
// rpcAllowedは、JSON-RPC overHTTPが有効になっている場合にtrueを返します。
func (h *httpServer) rpcAllowed() bool {
	return h.httpHandler.Load().(*rpcHandler) != nil
}

// wsAllowed returns true when JSON-RPC over WebSocket is enabled.
// wsAllowedは、JSON-RPC overWebSocketが有効になっている場合にtrueを返します。
func (h *httpServer) wsAllowed() bool {
	return h.wsHandler.Load().(*rpcHandler) != nil
}

// isWebsocket checks the header of an http request for a websocket upgrade request.
// isWebsocketは、WebSocketアップグレードリクエストのhttpリクエストのヘッダーをチェックします。
func isWebsocket(r *http.Request) bool {
	return strings.ToLower(r.Header.Get("Upgrade")) == "websocket" &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// NewHTTPHandlerStack returns wrapped http-related handlers
// NewHTTPHandlerStackは、ラップされたhttp関連のハンドラーを返します
func NewHTTPHandlerStack(srv http.Handler, cors []string, vhosts []string) http.Handler {
	// Wrap the CORS-handler within a host-handler
	// CORSハンドラーをホストハンドラー内にラップします
	handler := newCorsHandler(srv, cors)
	handler = newVHostHandler(vhosts, handler)
	return newGzipHandler(handler)
}

func newCorsHandler(srv http.Handler, allowedOrigins []string) http.Handler {
	// disable CORS support if user has not specified a custom CORS configuration
	// ユーザーがカスタムCORS構成を指定していない場合は、CORSサポートを無効にします
	if len(allowedOrigins) == 0 {
		return srv
	}
	c := cors.New(cors.Options{
		AllowedOrigins: allowedOrigins,
		AllowedMethods: []string{http.MethodPost, http.MethodGet},
		AllowedHeaders: []string{"*"},
		MaxAge:         600,
	})
	return c.Handler(srv)
}

// virtualHostHandler is a handler which validates the Host-header of incoming requests.
// Using virtual hosts can help prevent DNS rebinding attacks, where a 'random' domain name points to
// the service ip address (but without CORS headers). By verifying the targeted virtual host, we can
// ensure that it's a destination that the node operator has defined.

// virtualHostHandlerは、着信要求のホストヘッダーを検証するハンドラーです。
// 仮想ホストを使用すると、DNS再バインド攻撃を防ぐことができます。「ランダムな」ドメイン名はサービスのIPアドレスを指します
// （ただし、CORSヘッダーはありません）。
// ターゲットの仮想ホストを検証することで、ノードオペレーターが定義した宛先であることを確認できます。
type virtualHostHandler struct {
	vhosts map[string]struct{}
	next   http.Handler
}

func newVHostHandler(vhosts []string, next http.Handler) http.Handler {
	vhostMap := make(map[string]struct{})
	for _, allowedHost := range vhosts {
		vhostMap[strings.ToLower(allowedHost)] = struct{}{}
	}
	return &virtualHostHandler{vhostMap, next}
}

// ServeHTTP serves JSON-RPC requests over HTTP, implements http.Handler
// ServeHTTPはHTTP経由でJSON-RPCリクエストを処理し、http.Handlerを実装します
func (h *virtualHostHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// if r.Host is not set, we can continue serving since a browser would set the Host header
	// r.Hostが設定されていない場合、ブラウザがHostヘッダーを設定するため、サービスを継続できます
	if r.Host == "" {
		h.next.ServeHTTP(w, r)
		return
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		// Either invalid (too many colons) or no port specified
		// 無効（コロンが多すぎる）またはポートが指定されていない
		host = r.Host
	}
	if ipAddr := net.ParseIP(host); ipAddr != nil {
		// It's an IP address, we can serve that
		// これはIPアドレスであり、サービスを提供できます
		h.next.ServeHTTP(w, r)
		return

	}
	// Not an IP address, but a hostname. Need to validate
	// IPアドレスではなく、ホスト名。検証する必要があります
	if _, exist := h.vhosts["*"]; exist {
		h.next.ServeHTTP(w, r)
		return
	}
	if _, exist := h.vhosts[host]; exist {
		h.next.ServeHTTP(w, r)
		return
	}
	http.Error(w, "invalid host specified", http.StatusForbidden)
}

var gzPool = sync.Pool{
	New: func() interface{} {
		w := gzip.NewWriter(ioutil.Discard)
		return w
	},
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (w *gzipResponseWriter) WriteHeader(status int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

func newGzipHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Encoding", "gzip")

		gz := gzPool.Get().(*gzip.Writer)
		defer gzPool.Put(gz)

		gz.Reset(w)
		defer gz.Close()

		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, Writer: gz}, r)
	})
}

type ipcServer struct {
	log      log.Logger
	endpoint string

	mu       sync.Mutex
	listener net.Listener
	srv      *rpc.Server
}

func newIPCServer(log log.Logger, endpoint string) *ipcServer {
	return &ipcServer{log: log, endpoint: endpoint}
}

// Start starts the httpServer's http.Server
// 開始はhttpServerのhttp.Serverを開始します
func (is *ipcServer) start(apis []rpc.API) error {
	is.mu.Lock()
	defer is.mu.Unlock()

	if is.listener != nil {
		return nil // 既に実行中 // already running
	}
	listener, srv, err := rpc.StartIPCEndpoint(is.endpoint, apis)
	if err != nil {
		is.log.Warn("IPC opening failed", "url", is.endpoint, "error", err)
		return err
	}
	is.log.Info("IPC endpoint opened", "url", is.endpoint)
	is.listener, is.srv = listener, srv
	return nil
}

func (is *ipcServer) stop() error {
	is.mu.Lock()
	defer is.mu.Unlock()

	if is.listener == nil {
		return nil // not running
	}
	err := is.listener.Close()
	is.srv.Stop()
	is.listener, is.srv = nil, nil
	is.log.Info("IPC endpoint closed", "url", is.endpoint)
	return err
}

// RegisterApis checks the given modules' availability, generates an allowlist based on the allowed modules,
// and then registers all of the APIs exposed by the services.
// RegisterApisは、指定されたモジュールの可用性をチェックし、
// 許可されたモジュールに基づいて許可リストを生成してから、サービスによって公開されているすべてのAPIを登録します。
func RegisterApis(apis []rpc.API, modules []string, srv *rpc.Server, exposeAll bool) error {
	if bad, available := checkModuleAvailability(modules, apis); len(bad) > 0 {
		log.Error("Unavailable modules in HTTP API list", "unavailable", bad, "available", available)
	}
	// Generate the allow list based on the allowed modules
	// 許可されたモジュールに基づいて許可リストを生成します
	allowList := make(map[string]bool)
	for _, module := range modules {
		allowList[module] = true
	}
	// Register all the APIs exposed by the services
	// サービスによって公開されているすべてのAPIを登録します
	for _, api := range apis {
		if exposeAll || allowList[api.Namespace] || (len(allowList) == 0 && api.Public) {
			if err := srv.RegisterName(api.Namespace, api.Service); err != nil {
				return err
			}
		}
	}
	return nil
}
