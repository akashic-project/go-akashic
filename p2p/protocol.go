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
	"fmt"

	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
)

// Protocol represents a P2P subprotocol implementation.
// プロトコルは、P2Pサブプロトコルの実装を表します。
type Protocol struct {
	// Name should contain the official protocol name,
	// often a three-letter word.
	// 名前には、公式のプロトコル名を含める必要があります。多くの場合、3文字の単語が含まれます。
	Name string

	// Version should contain the version number of the protocol.
	// バージョンには、プロトコルのバージョン番号が含まれている必要があります。
	Version uint

	// Length should contain the number of message codes used
	// by the protocol.
	// 長さには、プロトコルで使用されるメッセージコードの数を含める必要があります。
	Length uint64

	// Run is called in a new goroutine when the protocol has been
	// negotiated with a peer. It should read and write messages from
	// rw. The Payload for each message must be fully consumed.
	// プロトコルがピアとネゴシエートされると、新しいゴルーチンで実行が呼び出されます。
	// rwからのメッセージを読み書きする必要があります。
	// 各メッセージのペイロードは完全に消費される必要があります。
	//
	// The peer connection is closed when Start returns. It should return
	// any protocol-level error (such as an I/O error) that is
	// encountered.
	// Startが戻ると、ピア接続は閉じられます。
	// 発生したプロトコルレベルのエラー（I / Oエラーなど）を返す必要があります。
	Run func(peer *Peer, rw MsgReadWriter) error

	// NodeInfo is an optional helper method to retrieve protocol specific metadata
	// about the host node.
	// NodeInfoは、ホストノードに関するプロトコル固有のメタデータを取得するためのオプションのヘルパーメソッドです。
	NodeInfo func() interface{}

	// PeerInfo is an optional helper method to retrieve protocol specific metadata
	// about a certain peer in the network. If an info retrieval function is set,
	// but returns nil, it is assumed that the protocol handshake is still running.
	// PeerInfoは、ネットワーク内の特定のピアに関するプロトコル固有のメタデータを取得するためのオプションのヘルパーメソッドです。
	// 情報検索関数が設定されているがnilを返す場合、プロトコルハンドシェイクはまだ実行中であると見なされます。
	PeerInfo func(id enode.ID) interface{}

	// DialCandidates, if non-nil, is a way to tell Server about protocol-specific nodes
	// that should be dialed. The server continuously reads nodes from the iterator and
	// attempts to create connections to them.
	// DialCandidatesは、nilでない場合、ダイヤルする必要があるプロトコル固有のノードについてサーバーに通知する方法です。
	// サーバーはイテレーターからノードを継続的に読み取り、ノードへの接続を作成しようとします。
	DialCandidates enode.Iterator

	// Attributes contains protocol specific information for the node record.
	// 属性には、ノードレコードのプロトコル固有の情報が含まれています
	Attributes []enr.Entry
}

func (p Protocol) cap() Cap {
	return Cap{p.Name, p.Version}
}

// Cap is the structure of a peer capability.
// キャップは、ピア機能の構造です。
type Cap struct {
	Name    string
	Version uint
}

func (cap Cap) String() string {
	return fmt.Sprintf("%s/%d", cap.Name, cap.Version)
}

type capsByNameAndVersion []Cap

func (cs capsByNameAndVersion) Len() int      { return len(cs) }
func (cs capsByNameAndVersion) Swap(i, j int) { cs[i], cs[j] = cs[j], cs[i] }
func (cs capsByNameAndVersion) Less(i, j int) bool {
	return cs[i].Name < cs[j].Name || (cs[i].Name == cs[j].Name && cs[i].Version < cs[j].Version)
}
