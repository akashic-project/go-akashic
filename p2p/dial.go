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

package p2p

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	mrand "math/rand"
	"net"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common/mclock"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/netutil"
)

const (
	// This is the amount of time spent waiting in between redialing a certain node. The
	// limit is a bit higher than inboundThrottleTime to prevent failing dials in small
	// private networks.
	// これは、特定のノードのリダイヤルの間に待機するのに費やされた時間です。
	// 小さなプライベートネットワークでのダイヤルの失敗を防ぐために、
	// 制限はinboundThrottleTimeよりも少し高くなっています。
	dialHistoryExpiration = inboundThrottleTime + 5*time.Second

	// Config for the "Looking for peers" message.
	// 「ピアを探しています」メッセージの設定。
	dialStatsLogInterval = 10 * time.Second // printed at most this often 	せいぜいこれほど頻繁に印刷される
	dialStatsPeerLimit   = 3                // but not if more than this many dialed peers 	しかし、これ以上の数のダイヤルされたピアの場合はそうではありません

	// Endpoint resolution is throttled with bounded backoff.
	// エンドポイントの解決は、制限されたバックオフで抑制されます。
	initialResolveDelay = 60 * time.Second
	maxResolveDelay     = time.Hour
)

// NodeDialer is used to connect to nodes in the network, typically by using
// an underlying net.Dialer but also using net.Pipe in tests.
// NodeDialerは、ネットワーク内のノードに接続するために使用されます。
// 通常は、基盤となるnet.Dialerを使用しますが、テストではnet.Pipeも使用します。
type NodeDialer interface {
	Dial(context.Context, *enode.Node) (net.Conn, error)
}

type nodeResolver interface {
	Resolve(*enode.Node) *enode.Node
}

// tcpDialer implements NodeDialer using real TCP connections.
// tcpDialerは、実際のTCP接続を使用してNodeDialerを実装します。
type tcpDialer struct {
	d *net.Dialer
}

func (t tcpDialer) Dial(ctx context.Context, dest *enode.Node) (net.Conn, error) {
	return t.d.DialContext(ctx, "tcp", nodeAddr(dest).String())
}

func nodeAddr(n *enode.Node) net.Addr {
	return &net.TCPAddr{IP: n.IP(), Port: n.TCP()}
}

// checkDial errors:
// checkDialエラー：
var (
	errSelf             = errors.New("is self")
	errAlreadyDialing   = errors.New("already dialing")
	errAlreadyConnected = errors.New("already connected")
	errRecentlyDialed   = errors.New("recently dialed")
	errNetRestrict      = errors.New("not contained in netrestrict list")
	errNoPort           = errors.New("node does not provide TCP port")
)

// dialer creates outbound connections and submits them into Server.
// Two types of peer connections can be created:
// ダイヤラはアウトバウンド接続を作成し、それらをサーバーに送信します。
// 2種類のピア接続を作成できます。
//
//  - static dials are pre-configured connections. The dialer attempts
//    keep these nodes connected at all times.
//  - 静的ダイヤルは事前設定された接続です。
//    ダイヤラは、これらのノードを常に接続したままにします。
//
//  - dynamic dials are created from node discovery results. The dialer
//    continuously reads candidate nodes from its input iterator and attempts
//    to create peer connections to nodes arriving through the iterator.
//    動的ダイヤルは、ノード検出結果から作成されます。
//    ダイヤラは、入力イテレータから候補ノードを継続的に読み取り、
//    イテレータを介して到着するノードへのピア接続を作成しようとします。
//
type dialScheduler struct {
	dialConfig
	setupFunc   dialSetupFunc
	wg          sync.WaitGroup
	cancel      context.CancelFunc
	ctx         context.Context
	nodesIn     chan *enode.Node
	doneCh      chan *dialTask
	addStaticCh chan *enode.Node
	remStaticCh chan *enode.Node
	addPeerCh   chan *conn
	remPeerCh   chan *conn

	// Everything below here belongs to loop and
	// should only be accessed by code on the loop goroutine.
	// 以下のすべてはループに属しており、
	// ループゴルーチンのコードによってのみアクセスする必要があります。
	dialing   map[enode.ID]*dialTask // active tasks アクティブなタスク
	peers     map[enode.ID]struct{}  // all connected peers 接続されているすべてのピア
	dialPeers int                    // current number of dialed peers ダイヤルされたピアの現在の数

	// The static map tracks all static dial tasks. The subset of usable static dial tasks
	// (i.e. those passing checkDial) is kept in staticPool. The scheduler prefers
	// launching random static tasks from the pool over launching dynamic dials from the
	// iterator.
	// 静的マップは、すべての静的ダイヤルタスクを追跡します。
	// 使用可能な静的ダイヤルタスクのサブセット
	// （つまり、checkDialを通過するタスク）はstaticPoolに保持されます。
	// スケジューラーは、イテレーターから動的ダイヤルを起動するよりも、
	// プールからランダムな静的タスクを起動することを好みます。
	static     map[enode.ID]*dialTask
	staticPool []*dialTask

	// The dial history keeps recently dialed nodes. Members of history are not dialed.
	// ダイヤル履歴は、最近ダイヤルされたノードを保持します。履歴のメンバーはダイヤルされません。
	history          expHeap
	historyTimer     mclock.Timer
	historyTimerTime mclock.AbsTime

	// for logStats
	lastStatsLog     mclock.AbsTime
	doneSinceLastLog int
}

type dialSetupFunc func(net.Conn, connFlag, *enode.Node) error

type dialConfig struct {
	self           enode.ID         // our own ID 私たち自身のID
	maxDialPeers   int              // maximum number of dialed peers ダイヤルされたピアの最大数
	maxActiveDials int              // maximum number of active dials アクティブなダイヤルの最大数
	netRestrict    *netutil.Netlist // IP netrestrict list, disabled if nil IP netrestrictリスト、nilの場合は無効
	resolver       nodeResolver
	dialer         NodeDialer
	log            log.Logger
	clock          mclock.Clock
	rand           *mrand.Rand
}

func (cfg dialConfig) withDefaults() dialConfig {
	if cfg.maxActiveDials == 0 {
		cfg.maxActiveDials = defaultMaxPendingPeers
	}
	if cfg.log == nil {
		cfg.log = log.Root()
	}
	if cfg.clock == nil {
		cfg.clock = mclock.System{}
	}
	if cfg.rand == nil {
		seedb := make([]byte, 8)
		crand.Read(seedb)
		seed := int64(binary.BigEndian.Uint64(seedb))
		cfg.rand = mrand.New(mrand.NewSource(seed))
	}
	return cfg
}

func newDialScheduler(config dialConfig, it enode.Iterator, setupFunc dialSetupFunc) *dialScheduler {
	d := &dialScheduler{
		dialConfig:  config.withDefaults(),
		setupFunc:   setupFunc,
		dialing:     make(map[enode.ID]*dialTask),
		static:      make(map[enode.ID]*dialTask),
		peers:       make(map[enode.ID]struct{}),
		doneCh:      make(chan *dialTask),
		nodesIn:     make(chan *enode.Node),
		addStaticCh: make(chan *enode.Node),
		remStaticCh: make(chan *enode.Node),
		addPeerCh:   make(chan *conn),
		remPeerCh:   make(chan *conn),
	}
	d.lastStatsLog = d.clock.Now()
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.wg.Add(2)
	go d.readNodes(it)
	go d.loop(it)
	return d
}

// stop shuts down the dialer, canceling all current dial tasks.
// stopはダイヤラをシャットダウンし、現在のすべてのダイヤルタスクをキャンセルします。
func (d *dialScheduler) stop() {
	d.cancel()
	d.wg.Wait()
}

// addStatic adds a static dial candidate.
// addStaticは、静的ダイヤル候補を追加します。
func (d *dialScheduler) addStatic(n *enode.Node) {
	select {
	case d.addStaticCh <- n:
	case <-d.ctx.Done():
	}
}

// removeStatic removes a static dial candidate.
// removeStaticは、静的ダイヤル候補を削除します。
func (d *dialScheduler) removeStatic(n *enode.Node) {
	select {
	case d.remStaticCh <- n:
	case <-d.ctx.Done():
	}
}

// peerAdded updates the peer set.
// peerAddedはピアセットを更新します。
func (d *dialScheduler) peerAdded(c *conn) {
	select {
	case d.addPeerCh <- c:
	case <-d.ctx.Done():
	}
}

// peerRemoved updates the peer set.
// peerRemovedは、ピアセットを更新します。
func (d *dialScheduler) peerRemoved(c *conn) {
	select {
	case d.remPeerCh <- c:
	case <-d.ctx.Done():
	}
}

// loop is the main loop of the dialer.
// ループはダイヤラのメインループです。
func (d *dialScheduler) loop(it enode.Iterator) {
	var (
		nodesCh    chan *enode.Node
		historyExp = make(chan struct{}, 1)
	)

loop:
	for {
		// Launch new dials if slots are available.
		// スロットが利用可能な場合は、新しいダイヤルを起動します。
		slots := d.freeDialSlots()
		slots -= d.startStaticDials(slots)
		if slots > 0 {
			nodesCh = d.nodesIn
		} else {
			nodesCh = nil
		}
		d.rearmHistoryTimer(historyExp)
		d.logStats()

		select {
		case node := <-nodesCh:
			if err := d.checkDial(node); err != nil {
				d.log.Trace("Discarding dial candidate", "id", node.ID(), "ip", node.IP(), "reason", err)
			} else {
				d.startDial(newDialTask(node, dynDialedConn))
			}

		case task := <-d.doneCh:
			id := task.dest.ID()
			delete(d.dialing, id)
			d.updateStaticPool(id)
			d.doneSinceLastLog++

		case c := <-d.addPeerCh:
			if c.is(dynDialedConn) || c.is(staticDialedConn) {
				d.dialPeers++
			}
			id := c.node.ID()
			d.peers[id] = struct{}{}
			// Remove from static pool because the node is now connected.
			// ノードが接続されたため、静的プールから削除します。
			task := d.static[id]
			if task != nil && task.staticPoolIndex >= 0 {
				d.removeFromStaticPool(task.staticPoolIndex)
			}
			// TODO: cancel dials to connected peers
			// TODO：接続されたピアへのダイヤルをキャンセルします

		case c := <-d.remPeerCh:
			if c.is(dynDialedConn) || c.is(staticDialedConn) {
				d.dialPeers--
			}
			delete(d.peers, c.node.ID())
			d.updateStaticPool(c.node.ID())

		case node := <-d.addStaticCh:
			id := node.ID()
			_, exists := d.static[id]
			d.log.Trace("Adding static node", "id", id, "ip", node.IP(), "added", !exists)
			if exists {
				continue loop
			}
			task := newDialTask(node, staticDialedConn)
			d.static[id] = task
			if d.checkDial(node) == nil {
				d.addToStaticPool(task)
			}

		case node := <-d.remStaticCh:
			id := node.ID()
			task := d.static[id]
			d.log.Trace("Removing static node", "id", id, "ok", task != nil)
			if task != nil {
				delete(d.static, id)
				if task.staticPoolIndex >= 0 {
					d.removeFromStaticPool(task.staticPoolIndex)
				}
			}

		case <-historyExp:
			d.expireHistory()

		case <-d.ctx.Done():
			it.Close()
			break loop
		}
	}

	d.stopHistoryTimer(historyExp)
	for range d.dialing {
		<-d.doneCh
	}
	d.wg.Done()
}

// readNodes runs in its own goroutine and delivers nodes from
// the input iterator to the nodesIn channel.
// readNodesは独自のゴルーチンで実行され、
// 入力イテレータからnodesInチャネルにノードを配信します。
func (d *dialScheduler) readNodes(it enode.Iterator) {
	defer d.wg.Done()

	for it.Next() {
		select {
		case d.nodesIn <- it.Node():
		case <-d.ctx.Done():
		}
	}
}

// logStats prints dialer statistics to the log. The message is suppressed when enough
// peers are connected because users should only see it while their client is starting up
// or comes back online.
// logStatsは、ダイヤラ統計をログに出力します。
// 十分な数のピアが接続されている場合、
// ユーザーはクライアントの起動中またはオンラインに戻ったときにのみメッセージを表示する必要があるため、
// メッセージは抑制されます。
func (d *dialScheduler) logStats() {
	now := d.clock.Now()
	if d.lastStatsLog.Add(dialStatsLogInterval) > now {
		return
	}
	if d.dialPeers < dialStatsPeerLimit && d.dialPeers < d.maxDialPeers {
		d.log.Info("Looking for peers", "peercount", len(d.peers), "tried", d.doneSinceLastLog, "static", len(d.static))
	}
	d.doneSinceLastLog = 0
	d.lastStatsLog = now
}

// rearmHistoryTimer configures d.historyTimer to fire when the
// next item in d.history expires.
// RearmHistoryTimerは、d.history内の次のアイテムが期限切れになったときに起動するように
// d.historyTimerを構成します。
func (d *dialScheduler) rearmHistoryTimer(ch chan struct{}) {
	if len(d.history) == 0 || d.historyTimerTime == d.history.nextExpiry() {
		return
	}
	d.stopHistoryTimer(ch)
	d.historyTimerTime = d.history.nextExpiry()
	timeout := time.Duration(d.historyTimerTime - d.clock.Now())
	d.historyTimer = d.clock.AfterFunc(timeout, func() { ch <- struct{}{} })
}

// stopHistoryTimer stops the timer and drains the channel it sends on.
// stopHistoryTimerはタイマーを停止し、送信したチャネルをドレインします。
func (d *dialScheduler) stopHistoryTimer(ch chan struct{}) {
	if d.historyTimer != nil && !d.historyTimer.Stop() {
		<-ch
	}
}

// expireHistory removes expired items from d.history.
// expireHistoryは、期限切れのアイテムをd.historyから削除します。
func (d *dialScheduler) expireHistory() {
	d.historyTimer.Stop()
	d.historyTimer = nil
	d.historyTimerTime = 0
	d.history.expire(d.clock.Now(), func(hkey string) {
		var id enode.ID
		copy(id[:], hkey)
		d.updateStaticPool(id)
	})
}

// freeDialSlots returns the number of free dial slots. The result can be negative
// when peers are connected while their task is still running.
// freeDialSlotsは、空きダイヤルスロットの数を返します。
// タスクの実行中にピアが接続されている場合、結果はマイナスになる可能性があります。
func (d *dialScheduler) freeDialSlots() int {
	slots := (d.maxDialPeers - d.dialPeers) * 2
	if slots > d.maxActiveDials {
		slots = d.maxActiveDials
	}
	free := slots - len(d.dialing)
	return free
}

// checkDial returns an error if node n should not be dialed.
// ノードnをダイヤルしない場合、checkDialはエラーを返します。
func (d *dialScheduler) checkDial(n *enode.Node) error {
	if n.ID() == d.self {
		return errSelf
	}
	if n.IP() != nil && n.TCP() == 0 {
		// This check can trigger if a non-TCP node is found
		// by discovery. If there is no IP, the node is a static
		// node and the actual endpoint will be resolved later in dialTask.
		// このチェックは、非TCPノードが検出によって検出された場合にトリガーできます。
		// IPがない場合、ノードは静的ノードであり、実際のエンドポイントは後でdialTask??で解決されます。
		return errNoPort
	}
	if _, ok := d.dialing[n.ID()]; ok {
		return errAlreadyDialing
	}
	if _, ok := d.peers[n.ID()]; ok {
		return errAlreadyConnected
	}
	if d.netRestrict != nil && !d.netRestrict.Contains(n.IP()) {
		return errNetRestrict
	}
	if d.history.contains(string(n.ID().Bytes())) {
		return errRecentlyDialed
	}
	return nil
}

// startStaticDials starts n static dial tasks.
// startStaticDialsは、n個の静的ダイヤルタスクを開始します。
func (d *dialScheduler) startStaticDials(n int) (started int) {
	for started = 0; started < n && len(d.staticPool) > 0; started++ {
		idx := d.rand.Intn(len(d.staticPool))
		task := d.staticPool[idx]
		d.startDial(task)
		d.removeFromStaticPool(idx)
	}
	return started
}

// updateStaticPool attempts to move the given static dial back into staticPool.
// updateStaticPoolは、指定された静的ダイヤルをstaticPoolに戻そうとします。
func (d *dialScheduler) updateStaticPool(id enode.ID) {
	task, ok := d.static[id]
	if ok && task.staticPoolIndex < 0 && d.checkDial(task.dest) == nil {
		d.addToStaticPool(task)
	}
}

func (d *dialScheduler) addToStaticPool(task *dialTask) {
	if task.staticPoolIndex >= 0 {
		panic("attempt to add task to staticPool twice")
	}
	d.staticPool = append(d.staticPool, task)
	task.staticPoolIndex = len(d.staticPool) - 1
}

// removeFromStaticPool removes the task at idx from staticPool. It does that by moving the
// current last element of the pool to idx and then shortening the pool by one.
// removeFromStaticPoolは、idxのタスクをstaticPoolから削除します。
// これは、プールの現在の最後の要素をidxに移動してから、プールを1つ短くすることによって行われます。
func (d *dialScheduler) removeFromStaticPool(idx int) {
	task := d.staticPool[idx]
	end := len(d.staticPool) - 1
	d.staticPool[idx] = d.staticPool[end]
	d.staticPool[idx].staticPoolIndex = idx
	d.staticPool[end] = nil
	d.staticPool = d.staticPool[:end]
	task.staticPoolIndex = -1
}

// startDial runs the given dial task in a separate goroutine.
// startDialは、指定されたダイヤルタスクを別のゴルーチンで実行します。
func (d *dialScheduler) startDial(task *dialTask) {
	d.log.Trace("Starting p2p dial", "id", task.dest.ID(), "ip", task.dest.IP(), "flag", task.flags)
	hkey := string(task.dest.ID().Bytes())
	d.history.add(hkey, d.clock.Now().Add(dialHistoryExpiration))
	d.dialing[task.dest.ID()] = task
	go func() {
		task.run(d)
		d.doneCh <- task
	}()
}

// A dialTask generated for each node that is dialed.
// ダイヤルされるノードごとに生成されるdialTask??
type dialTask struct {
	staticPoolIndex int
	flags           connFlag
	// These fields are private to the task and should not be
	// accessed by dialScheduler while the task is running.
	// これらのフィールドはタスク専用であり、
	// タスクの実行中にdialSchedulerからアクセスしないでください。
	dest         *enode.Node
	lastResolved mclock.AbsTime
	resolveDelay time.Duration
}

func newDialTask(dest *enode.Node, flags connFlag) *dialTask {
	return &dialTask{dest: dest, flags: flags, staticPoolIndex: -1}
}

type dialError struct {
	error
}

func (t *dialTask) run(d *dialScheduler) {
	if t.needResolve() && !t.resolve(d) {
		return
	}

	err := t.dial(d, t.dest)
	if err != nil {
		// For static nodes, resolve one more time if dialing fails.
		// 静的ノードの場合、ダイヤルが失敗した場合はもう一度解決します。
		if _, ok := err.(*dialError); ok && t.flags&staticDialedConn != 0 {
			if t.resolve(d) {
				t.dial(d, t.dest)
			}
		}
	}
}

func (t *dialTask) needResolve() bool {
	return t.flags&staticDialedConn != 0 && t.dest.IP() == nil
}

// resolve attempts to find the current endpoint for the destination
// using discovery.
// resolveは、検出を使用して宛先の現在のエンドポイントを検索しようとします。
//
// Resolve operations are throttled with backoff to avoid flooding the
// discovery network with useless queries for nodes that don't exist.
// The backoff delay resets when the node is found.
// 解決操作はバックオフで抑制され、存在しないノードに対する無用なクエリでディスカバリーネットワークが溢れるのを防ぎます。
// ノードが見つかると、バックオフ遅延はリセットされます。
func (t *dialTask) resolve(d *dialScheduler) bool {
	if d.resolver == nil {
		return false
	}
	if t.resolveDelay == 0 {
		t.resolveDelay = initialResolveDelay
	}
	if t.lastResolved > 0 && time.Duration(d.clock.Now()-t.lastResolved) < t.resolveDelay {
		return false
	}
	resolved := d.resolver.Resolve(t.dest)
	t.lastResolved = d.clock.Now()
	if resolved == nil {
		t.resolveDelay *= 2
		if t.resolveDelay > maxResolveDelay {
			t.resolveDelay = maxResolveDelay
		}
		d.log.Debug("Resolving node failed", "id", t.dest.ID(), "newdelay", t.resolveDelay)
		return false
	}
	// The node was found.
	// ノードが見つかりました。
	t.resolveDelay = initialResolveDelay
	t.dest = resolved
	d.log.Debug("Resolved node", "id", t.dest.ID(), "addr", &net.TCPAddr{IP: t.dest.IP(), Port: t.dest.TCP()})
	return true
}

// dial performs the actual connection attempt.
// ダイヤルは実際の接続試行を実行します。
func (t *dialTask) dial(d *dialScheduler, dest *enode.Node) error {
	fd, err := d.dialer.Dial(d.ctx, t.dest)
	if err != nil {
		d.log.Trace("Dial error", "id", t.dest.ID(), "addr", nodeAddr(t.dest), "conn", t.flags, "err", cleanupDialErr(err))
		return &dialError{err}
	}
	mfd := newMeteredConn(fd, false, &net.TCPAddr{IP: dest.IP(), Port: dest.TCP()})
	return d.setupFunc(mfd, t.flags, dest)
}

func (t *dialTask) String() string {
	id := t.dest.ID()
	return fmt.Sprintf("%v %x %v:%d", t.flags, id[:8], t.dest.IP(), t.dest.TCP())
}

func cleanupDialErr(err error) error {
	if netErr, ok := err.(*net.OpError); ok && netErr.Op == "dial" {
		return netErr.Err
	}
	return err
}
