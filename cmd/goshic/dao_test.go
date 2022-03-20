// Copyright 2016 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"io/ioutil"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/params"
)

// Genesis block for nodes which don't care about the DAO fork (i.e. not configured)
// DAOフォークを気にしない（つまり、構成されていない）ノードのジェネシスブロック
var daoOldGenesis = `{
	"alloc"      : {},
	"coinbase"   : "0x0000000000000000000000000000000000000000",
	"difficulty" : "0x20000",
	"extraData"  : "",
	"gasLimit"   : "0x2fefd8",
	"nonce"      : "0x0000000000000042",
	"mixhash"    : "0x0000000000000000000000000000000000000000000000000000000000000000",
	"parentHash" : "0x0000000000000000000000000000000000000000000000000000000000000000",
	"timestamp"  : "0x00",
	"config"     : {
		"homesteadBlock" : 0
	}
}`

// Genesis block for nodes which actively oppose the DAO fork
// DAOフォークに積極的に反対するノードのジェネシスブロック
var daoNoForkGenesis = `{
	"alloc"      : {},
	"coinbase"   : "0x0000000000000000000000000000000000000000",
	"difficulty" : "0x20000",
	"extraData"  : "",
	"gasLimit"   : "0x2fefd8",
	"nonce"      : "0x0000000000000042",
	"mixhash"    : "0x0000000000000000000000000000000000000000000000000000000000000000",
	"parentHash" : "0x0000000000000000000000000000000000000000000000000000000000000000",
	"timestamp"  : "0x00",
	"config"     : {
		"homesteadBlock" : 0,
		"daoForkBlock"   : 314,
		"daoForkSupport" : false
	}
}`

// Genesis block for nodes which actively support the DAO fork
// DAOフォークをアクティブにサポートするノードのジェネシスブロック
var daoProForkGenesis = `{
	"alloc"      : {},
	"coinbase"   : "0x0000000000000000000000000000000000000000",
	"difficulty" : "0x20000",
	"extraData"  : "",
	"gasLimit"   : "0x2fefd8",
	"nonce"      : "0x0000000000000042",
	"mixhash"    : "0x0000000000000000000000000000000000000000000000000000000000000000",
	"parentHash" : "0x0000000000000000000000000000000000000000000000000000000000000000",
	"timestamp"  : "0x00",
	"config"     : {
		"homesteadBlock" : 0,
		"daoForkBlock"   : 314,
		"daoForkSupport" : true
	}
}`

var daoGenesisHash = common.HexToHash("5e1fc79cb4ffa4739177b5408045cd5d51c6cf766133f23f7cd72ee1f8d790e0")
var daoGenesisForkBlock = big.NewInt(314)

// TestDAOForkBlockNewChain tests that the DAO hard-fork number and the nodes support/opposition is correctly
// set in the database after various initialization procedures and invocations.
// TestDAOForkBlockNewChainは、さまざまな初期化手順と呼び出しの後に、
// DAOハードフォーク番号とノードのサポート/反対がデータベースに正しく設定されていることをテストします。
func TestDAOForkBlockNewChain(t *testing.T) {
	for i, arg := range []struct {
		genesis     string
		expectBlock *big.Int
		expectVote  bool
	}{
		// Test DAO Default Mainnet
		// DAOのデフォルトのメインネットをテストします
		{"", params.MainnetChainConfig.DAOForkBlock, true},
		// test DAO Init Old Privnet
		// DAO Init OldPrivnetをテストします
		{daoOldGenesis, nil, false},
		// test DAO Default No Fork Privnet
		// DAOのデフォルトのフォークプライベートなしをテストします
		{daoNoForkGenesis, daoGenesisForkBlock, false},
		// test DAO Default Pro Fork Privnet
		// DAO Default Pro ForkPrivnetをテストします
		{daoProForkGenesis, daoGenesisForkBlock, true},
	} {
		testDAOForkBlockNewChain(t, i, arg.genesis, arg.expectBlock, arg.expectVote)
	}
}

func testDAOForkBlockNewChain(t *testing.T, test int, genesis string, expectBlock *big.Int, expectVote bool) {
	// Create a temporary data directory to use and inspect later
	// 後で使用および検査するための一時データディレクトリを作成します
	datadir := tmpdir(t)
	defer os.RemoveAll(datadir)

	// Start a Geth instance with the requested flags set and immediately terminate
	// 要求されたフラグを設定してGethインスタンスを開始し、すぐに終了します
	if genesis != "" {
		json := filepath.Join(datadir, "genesis.json")
		if err := ioutil.WriteFile(json, []byte(genesis), 0600); err != nil {
			t.Fatalf("test %d: failed to write genesis file: %v", test, err)
		}
		runGeth(t, "--datadir", datadir, "--networkid", "1337", "init", json).WaitExit()
	} else {
		// Force chain initialization
		// チェーンの初期化を強制します
		args := []string{"--port", "0", "--networkid", "1337", "--maxpeers", "0", "--nodiscover", "--nat", "none", "--ipcdisable", "--datadir", datadir}
		runGeth(t, append(args, []string{"--exec", "2+2", "console"}...)...).WaitExit()
	}
	// Retrieve the DAO config flag from the database
	// データベースからDAO構成フラグを取得します
	path := filepath.Join(datadir, "goshic", "chaindata")
	db, err := rawdb.NewLevelDBDatabase(path, 0, 0, "", false)
	if err != nil {
		t.Fatalf("test %d: failed to open test database: %v", test, err)
	}
	defer db.Close()

	genesisHash := common.HexToHash("0xd4e56740f876aef8c010b86a40d5f56745a118d0906a34e69aec8c0db1cb8fa3")
	if genesis != "" {
		genesisHash = daoGenesisHash
	}
	config := rawdb.ReadChainConfig(db, genesisHash)
	if config == nil {
		t.Errorf("test %d: failed to retrieve chain config: %v", test, err)
		return // ここに戻りたいのですが、他のチェックではこのポイントを通過できません（パニックなし）。// we want to return here, the other checks can't make it past this point (nil panic).
	}
	// Validate the DAO hard-fork block number against the expected value
	// DAOハードフォークブロック番号を期待値と照合して検証します
	if config.DAOForkBlock == nil {
		if expectBlock != nil {
			t.Errorf("test %d: dao hard-fork block mismatch: have nil, want %v", test, expectBlock)
		}
	} else if expectBlock == nil {
		t.Errorf("test %d: dao hard-fork block mismatch: have %v, want nil", test, config.DAOForkBlock)
	} else if config.DAOForkBlock.Cmp(expectBlock) != 0 {
		t.Errorf("test %d: dao hard-fork block mismatch: have %v, want %v", test, config.DAOForkBlock, expectBlock)
	}
	if config.DAOForkSupport != expectVote {
		t.Errorf("test %d: dao hard-fork support mismatch: have %v, want %v", test, config.DAOForkSupport, expectVote)
	}
}