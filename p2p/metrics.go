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

// Contains the meters and timers used by the networking layer.

package p2p

import (
	"net"

	"github.com/ethereum/go-ethereum/metrics"
)

const (
	// ingressMeterName is the prefix of the per-packet inbound metrics.
	// ingressMeterNameは、パケットごとのインバウンドメトリックのプレフィックスです。
	ingressMeterName = "p2p/ingress"

	// egressMeterName is the prefix of the per-packet outbound metrics.
	// egressMeterNameは、パケットごとのアウトバウンドメトリックのプレフィックスです。
	egressMeterName = "p2p/egress"

	// HandleHistName is the prefix of the per-packet serving time histograms.
	// HandleHistNameは、パケットごとのサービス時間ヒストグラムのプレフィックスです。
	HandleHistName = "p2p/handle"
)

var (
	ingressConnectMeter = metrics.NewRegisteredMeter("p2p/serves", nil)
	ingressTrafficMeter = metrics.NewRegisteredMeter(ingressMeterName, nil)
	egressConnectMeter  = metrics.NewRegisteredMeter("p2p/dials", nil)
	egressTrafficMeter  = metrics.NewRegisteredMeter(egressMeterName, nil)
	activePeerGauge     = metrics.NewRegisteredGauge("p2p/peers", nil)
)

// meteredConn is a wrapper around a net.Conn that meters both the
// inbound and outbound network traffic.
// MeteredConnは、net.Connのラッパーであり、
// インバウンドとアウトバウンドの両方のネットワークトラフィックを計測します。
type meteredConn struct {
	net.Conn
}

// newMeteredConn creates a new metered connection, bumps the ingress or egress
// connection meter and also increases the metered peer count. If the metrics
// system is disabled, function returns the original connection.
// newMeteredConnは、新しい従量制接続を作成し、入力または出力接続メーターをバンプし、従量制ピア数も増やします。
// メートル法が無効になっている場合、関数は元の接続を返します
func newMeteredConn(conn net.Conn, ingress bool, addr *net.TCPAddr) net.Conn {
	// Short circuit if metrics are disabled
	// メトリックが無効になっている場合の短絡
	if !metrics.Enabled {
		return conn
	}
	// Bump the connection counters and wrap the connection
	// 接続カウンターをバンプし、接続をラップします
	if ingress {
		ingressConnectMeter.Mark(1)
	} else {
		egressConnectMeter.Mark(1)
	}
	activePeerGauge.Inc(1)
	return &meteredConn{Conn: conn}
}

// Read delegates a network read to the underlying connection, bumping the common
// and the peer ingress traffic meters along the way.
// 読み取りは、ネットワーク読み取りを基盤となる接続に委任し、
// 途中で共通およびピアの入力トラフィックメーターをバンプします。
func (c *meteredConn) Read(b []byte) (n int, err error) {
	n, err = c.Conn.Read(b)
	ingressTrafficMeter.Mark(int64(n))
	return n, err
}

// Write delegates a network write to the underlying connection, bumping the common
// and the peer egress traffic meters along the way.
// 書き込みは、ネットワーク書き込みを基盤となる接続に委任し、
// 途中で共通およびピアの出力トラフィックメーターをバンプします。
func (c *meteredConn) Write(b []byte) (n int, err error) {
	n, err = c.Conn.Write(b)
	egressTrafficMeter.Mark(int64(n))
	return n, err
}

// Close delegates a close operation to the underlying connection, unregisters
// the peer from the traffic registries and emits close event.
// Closeは、基になる接続にclose操作を委任し、
// トラフィックレジストリからピアの登録を解除し、closeイベントを発行します。
func (c *meteredConn) Close() error {
	err := c.Conn.Close()
	if err == nil {
		activePeerGauge.Dec(1)
	}
	return err
}
