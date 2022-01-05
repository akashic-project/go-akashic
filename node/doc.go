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

/*
パッケージノードは、マルチプロトコルのイーサリアムノードをセットアップします。

このパッケージで公開されているモデルでは、ノードは共有リソースを使用してRPCAPIを提供するサービスのコレクションです。
サービスはdevp2pプロトコルを提供することもできます。これは、ノードインスタンスの起動時にdevp2pネットワークに接続されます。

ノードのライフサイクル

Nodeオブジェクトには、INITIALIZING、RUNNING、およびCLOSEDの3つの基本状態で構成されるライフサイクルがあります。


    ●───────┐
         New()
            │
            ▼
      INITIALIZING ────Start()─┐
            │                  │
            │                  ▼
        Close()             RUNNING
            │                  │
            ▼                  │
         CLOSED ◀──────Close()─┘


ノードを作成すると、データディレクトリなどの基本的なリソースが割り当てられ、ノードがINITIALIZING状態に戻ります。
この状態で、ライフサイクルオブジェクト、RPC API、およびピアツーピアネットワークプロトコルを登録できます。
初期化中は、Key-Valueデータベースを開くなどの基本的な操作が許可されます。

すべてが登録されると、ノードを開始できます。これにより、ノードはRUNNING状態に移行します。
ノードを起動すると、登録されているすべてのライフサイクルオブジェクトが起動し、RPCおよびピアツーピアネットワークが有効になります。
ノードの実行中は、追加のライフサイクル、API、またはp2pプロトコルを登録できないことに注意してください。

ノードを閉じると、保持されているすべてのリソースが解放されます。 Closeによって実行されるアクションは、それがあった状態によって異なります。
INITIALIZING状態のノードを閉じると、データディレクトリに関連するリソースが解放されます。
ノードが実行中だった場合、ノードを閉じると、すべてのライフサイクルオブジェクトも停止し、RPCとピアツーピアネットワークがシャットダウンされます。

ノードが開始されていない場合でも、常にノードでCloseを呼び出す必要があります。


ノードによって管理されるリソース

ノードインスタンスによって使用されるすべてのファイルシステムリソースは、データディレクトリと呼ばれるディレクトリにあります。
各リソースの場所は、追加のノード構成によってオーバーライドできます。データディレクトリはオプションです。
設定されておらず、リソースの場所が指定されていない場合、パッケージノードはメモリ内にリソースを作成します。

devp2pネットワークにアクセスするために、ノードはp2p.Serverを構成して起動します。 devp2pネットワーク上の各ホストには、一意の識別子であるノードキーがあります。
Nodeインスタンスは、再起動後もこのキーを保持します。
ノードは静的で信頼できるノードリストもロードし、他のホストに関する知識が保持されるようにします。

HTTP、WebSocket、またはIPCを実行するJSON-RPCサーバーをノードで起動できます。
登録されたサービスによって提供されるRPCモジュールは、これらのエンドポイントで提供されます。
 ユーザーは、任意のエンドポイントをRPCモジュールのサブセットに制限できます。ノード自体は、「debug」、「admin」、および「web3」モジュールを提供します。

サービス実装は、サービスコンテキストを介してLevelDBデータベースを開くことができます。パッケージノードは、各データベースのファイルシステムの場所を選択します。
ノードがデータディレクトリなしで実行するように構成されている場合、データベースは代わりにメモリで開かれます。

ノードは、暗号化されたイーサリアムアカウントキーの共有ストアも作成します。サービスは、サービスコンテキストを介してアカウントマネージャーにアクセスできます。


インスタンス間でのデータディレクトリの共有

複数のノードインスタンスが異なるインスタンス名を持っている場合（Name configオプションで設定）、単一のデータディレクトリを共有できます。
共有動作は、リソースのタイプによって異なります。

devp2p関連のリソース（ノードキー、静的/信頼できるノードリスト、既知のホストデータベース）は、インスタンスと同じ名前のディレクトリに保存されます。
したがって、同じデータディレクトリを使用する複数のノードインスタンスは、この情報をデータディレクトリの異なるサブディレクトリに保存します。

LevelDBデータベースもインスタンスサブディレクトリ内に保存されます。
複数のノードインスタンスが同じデータディレクトリを使用する場合、同じ名前のデータベースを開くと、インスタンスごとに1つのデータベースが作成されます。

アカウントキーストアは、KeyStoreDir構成オプションで場所が変更されない限り、同じデータディレクトリを使用するすべてのノードインスタンス間で共有されます。


データディレクトリ共有の例

この例では、AとBという名前の2つのノードインスタンスが同じデータディレクトリで開始されています。
ノードインスタンスAはデータベース「db」を開き、ノードインスタンスBはデータベース「db」と「db-2」を開きます。
次のファイルがデータディレクトリに作成されます。

   data-directory/
        A/
            nodekey            -- インスタンスAのdevp2pノードキー
            nodes/             -- インスタンスAのdevp2p発見知識データベース
            db/                -- 「db」のLevelDBコンテンツ
        A.ipc                  -- インスタンスAのJSON-RPCUNIXドメインソケットエンドポイント
        B/
            nodekey            -- ノードBのdevp2pノードキー
            nodes/             -- インスタンスBのdevp2p発見知識データベース
            static-nodes.json  -- インスタンスBのdevp2p静的ノードリスト
            db/                -- 「db」のLevelDBコンテンツ
            db-2/              -- 「db-2」のLevelDBコンテンツ
        B.ipc                  -- インスタンスBのJSON-RPCUNIXドメインソケットエンドポイント
        keystore/              -- 両方のインスタンスで使用されるアカウントキーストア
*/

/*
Package node sets up multi-protocol Ethereum nodes.

In the model exposed by this package, a node is a collection of services which use shared
resources to provide RPC APIs. Services can also offer devp2p protocols, which are wired
up to the devp2p network when the node instance is started.


Node Lifecycle

The Node object has a lifecycle consisting of three basic states, INITIALIZING, RUNNING
and CLOSED.


    ●───────┐
         New()
            │
            ▼
      INITIALIZING ────Start()─┐
            │                  │
            │                  ▼
        Close()             RUNNING
            │                  │
            ▼                  │
         CLOSED ◀──────Close()─┘


Creating a Node allocates basic resources such as the data directory and returns the node
in its INITIALIZING state. Lifecycle objects, RPC APIs and peer-to-peer networking
protocols can be registered in this state. Basic operations such as opening a key-value
database are permitted while initializing.

Once everything is registered, the node can be started, which moves it into the RUNNING
state. Starting the node starts all registered Lifecycle objects and enables RPC and
peer-to-peer networking. Note that no additional Lifecycles, APIs or p2p protocols can be
registered while the node is running.

Closing the node releases all held resources. The actions performed by Close depend on the
state it was in. When closing a node in INITIALIZING state, resources related to the data
directory are released. If the node was RUNNING, closing it also stops all Lifecycle
objects and shuts down RPC and peer-to-peer networking.

You must always call Close on Node, even if the node was not started.


Resources Managed By Node

All file-system resources used by a node instance are located in a directory called the
data directory. The location of each resource can be overridden through additional node
configuration. The data directory is optional. If it is not set and the location of a
resource is otherwise unspecified, package node will create the resource in memory.

To access to the devp2p network, Node configures and starts p2p.Server. Each host on the
devp2p network has a unique identifier, the node key. The Node instance persists this key
across restarts. Node also loads static and trusted node lists and ensures that knowledge
about other hosts is persisted.

JSON-RPC servers which run HTTP, WebSocket or IPC can be started on a Node. RPC modules
offered by registered services will be offered on those endpoints. Users can restrict any
endpoint to a subset of RPC modules. Node itself offers the "debug", "admin" and "web3"
modules.

Service implementations can open LevelDB databases through the service context. Package
node chooses the file system location of each database. If the node is configured to run
without a data directory, databases are opened in memory instead.

Node also creates the shared store of encrypted Ethereum account keys. Services can access
the account manager through the service context.


Sharing Data Directory Among Instances

Multiple node instances can share a single data directory if they have distinct instance
names (set through the Name config option). Sharing behaviour depends on the type of
resource.

devp2p-related resources (node key, static/trusted node lists, known hosts database) are
stored in a directory with the same name as the instance. Thus, multiple node instances
using the same data directory will store this information in different subdirectories of
the data directory.

LevelDB databases are also stored within the instance subdirectory. If multiple node
instances use the same data directory, opening the databases with identical names will
create one database for each instance.

The account key store is shared among all node instances using the same data directory
unless its location is changed through the KeyStoreDir configuration option.


Data Directory Sharing Example

In this example, two node instances named A and B are started with the same data
directory. Node instance A opens the database "db", node instance B opens the databases
"db" and "db-2". The following files will be created in the data directory:

   data-directory/
        A/
            nodekey            -- devp2p node key of instance A
            nodes/             -- devp2p discovery knowledge database of instance A
            db/                -- LevelDB content for "db"
        A.ipc                  -- JSON-RPC UNIX domain socket endpoint of instance A
        B/
            nodekey            -- devp2p node key of node B
            nodes/             -- devp2p discovery knowledge database of instance B
            static-nodes.json  -- devp2p static node list of instance B
            db/                -- LevelDB content for "db"
            db-2/              -- LevelDB content for "db-2"
        B.ipc                  -- JSON-RPC UNIX domain socket endpoint of instance B
        keystore/              -- account key store, used by both instances
*/
package node
