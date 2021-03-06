// Copyright 2021 The go-ethereum Authors
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

package core

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/state/snapshot"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
)

// CurrentHeader retrieves the current head header of the canonical chain. The
// header is retrieved from the HeaderChain's internal cache.
// CurrentHeaderは、正規チェーンの現在のヘッドヘッダーを取得します。
// ヘッダーはHeaderChainの内部キャッシュから取得されます。
func (bc *BlockChain) CurrentHeader() *types.Header {
	return bc.hc.CurrentHeader()
}

// CurrentBlock retrieves the current head block of the canonical chain. The
// block is retrieved from the blockchain's internal cache.
// CurrentBlockは、正規チェーンの現在のヘッドブロックを取得します。
// ブロックは、ブロックチェーンの内部キャッシュから取得されます。
func (bc *BlockChain) CurrentBlock() *types.Block {
	return bc.currentBlock.Load().(*types.Block)
}

// CurrentFastBlock retrieves the current fast-sync head block of the canonical
// chain. The block is retrieved from the blockchain's internal cache.
// CurrentFastBlockは、正規チェーンの現在の高速同期ヘッドブロックを取得します。
// ブロックは、ブロックチェーンの内部キャッシュから取得されます。
func (bc *BlockChain) CurrentFastBlock() *types.Block {
	return bc.currentFastBlock.Load().(*types.Block)
}

// HasHeader checks if a block header is present in the database or not, caching
// it if present.
// HasHeaderは、ブロックヘッダーがデータベースに存在するかどうかをチェックし、存在する場合はキャッシュします。
func (bc *BlockChain) HasHeader(hash common.Hash, number uint64) bool {
	return bc.hc.HasHeader(hash, number)
}

// GetHeader retrieves a block header from the database by hash and number,
// caching it if found.
// GetHeaderは、データベースからハッシュと数値でブロックヘッダーを取得し、見つかった場合はキャッシュします。
func (bc *BlockChain) GetHeader(hash common.Hash, number uint64) *types.Header {
	return bc.hc.GetHeader(hash, number)
}

// GetHeaderByHash retrieves a block header from the database by hash, caching it if
// found.
// GetHeaderByHashは、データベースからブロックヘッダーをハッシュで取得し、見つかった場合はキャッシュします。
func (bc *BlockChain) GetHeaderByHash(hash common.Hash) *types.Header {
	return bc.hc.GetHeaderByHash(hash)
}

// GetHeaderByNumber retrieves a block header from the database by number,
// caching it (associated with its hash) if found.
// GetHeaderByNumberは、データベースからブロックヘッダーを番号で取得し、見つかった場合はキャッシュします（ハッシュに関連付けられます）。
func (bc *BlockChain) GetHeaderByNumber(number uint64) *types.Header {
	return bc.hc.GetHeaderByNumber(number)
}

// GetHeadersFrom returns a contiguous segment of headers, in rlp-form, going
// backwards from the given number.
// GetHeadersFromは、指定された番号から逆方向に、rlp形式でヘッダーの連続したセグメントを返します。
func (bc *BlockChain) GetHeadersFrom(number, count uint64) []rlp.RawValue {
	return bc.hc.GetHeadersFrom(number, count)
}

// GetBody retrieves a block body (transactions and uncles) from the database by
// hash, caching it if found.
// GetBodyは、データベースからブロック本体（トランザクションと叔父）をハッシュで取得し、見つかった場合はキャッシュします。
func (bc *BlockChain) GetBody(hash common.Hash) *types.Body {
	// Short circuit if the body's already in the cache, retrieve otherwise
	// 本体がすでにキャッシュにある場合は短絡し、そうでない場合は取得します
	if cached, ok := bc.bodyCache.Get(hash); ok {
		body := cached.(*types.Body)
		return body
	}
	number := bc.hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	body := rawdb.ReadBody(bc.db, hash, *number)
	if body == nil {
		return nil
	}
	// Cache the found body for next time and return
	// 見つかった本文を次回のためにキャッシュし、
	bc.bodyCache.Add(hash, body)
	return body
}

// GetBodyRLP retrieves a block body in RLP encoding from the database by hash,
// caching it if found.
// GetBodyRLPは、データベースからRLPエンコーディングのブロック本体をハッシュで取得し、見つかった場合はキャッシュします。
func (bc *BlockChain) GetBodyRLP(hash common.Hash) rlp.RawValue {
	// Short circuit if the body's already in the cache, retrieve otherwise
	// 本体がすでにキャッシュにある場合は短絡し、そうでない場合は取得します
	if cached, ok := bc.bodyRLPCache.Get(hash); ok {
		return cached.(rlp.RawValue)
	}
	number := bc.hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	body := rawdb.ReadBodyRLP(bc.db, hash, *number)
	if len(body) == 0 {
		return nil
	}
	// Cache the found body for next time and return
	// 見つかった本文を次回のためにキャッシュし、
	bc.bodyRLPCache.Add(hash, body)
	return body
}

// HasBlock checks if a block is fully present in the database or not.
// HasBlockは、ブロックがデータベースに完全に存在するかどうかをチェックします。
func (bc *BlockChain) HasBlock(hash common.Hash, number uint64) bool {
	if bc.blockCache.Contains(hash) {
		return true
	}
	return rawdb.HasBody(bc.db, hash, number)
}

// HasFastBlock checks if a fast block is fully present in the database or not.
// HasFastBlockは、高速ブロックがデータベースに完全に存在するかどうかをチェックします。
func (bc *BlockChain) HasFastBlock(hash common.Hash, number uint64) bool {
	if !bc.HasBlock(hash, number) {
		return false
	}
	if bc.receiptsCache.Contains(hash) {
		return true
	}
	return rawdb.HasReceipts(bc.db, hash, number)
}

// GetBlock retrieves a block from the database by hash and number,
// caching it if found.
// GetBlockは、ハッシュと数値によってデータベースからブロックを取得し、見つかった場合はキャッシュします。
func (bc *BlockChain) GetBlock(hash common.Hash, number uint64) *types.Block {
	// Short circuit if the block's already in the cache, retrieve otherwise
	// ブロックがすでにキャッシュにある場合は短絡し、そうでない場合は取得します
	if block, ok := bc.blockCache.Get(hash); ok {
		return block.(*types.Block)
	}
	block := rawdb.ReadBlock(bc.db, hash, number)
	if block == nil {
		return nil
	}
	// Cache the found block for next time and return
	// 見つかったブロックを次回のためにキャッシュし、
	bc.blockCache.Add(block.Hash(), block)
	return block
}

// GetBlockByHash retrieves a block from the database by hash, caching it if found.
// GetBlockByHashは、データベースからハッシュによってブロックを取得し、見つかった場合はキャッシュします。
func (bc *BlockChain) GetBlockByHash(hash common.Hash) *types.Block {
	number := bc.hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	return bc.GetBlock(hash, *number)
}

// GetBlockByNumber retrieves a block from the database by number, caching it
// (associated with its hash) if found.
// GetBlockByNumberは、データベースからブロックを番号で取得し、見つかった場合は（ハッシュに関連付けられて）キャッシュします。
func (bc *BlockChain) GetBlockByNumber(number uint64) *types.Block {
	hash := rawdb.ReadCanonicalHash(bc.db, number)
	if hash == (common.Hash{}) {
		return nil
	}
	return bc.GetBlock(hash, number)
}

// GetBlocksFromHash returns the block corresponding to hash and up to n-1 ancestors.
// [deprecated by eth/62]
// GetBlocksFromHashは、ハッシュと最大n-1個の祖先に対応するブロックを返します。
// [eth / 62で非推奨]
func (bc *BlockChain) GetBlocksFromHash(hash common.Hash, n int) (blocks []*types.Block) {
	number := bc.hc.GetBlockNumber(hash)
	if number == nil {
		return nil
	}
	for i := 0; i < n; i++ {
		block := bc.GetBlock(hash, *number)
		if block == nil {
			break
		}
		blocks = append(blocks, block)
		hash = block.ParentHash()
		*number--
	}
	return
}

// GetReceiptsByHash retrieves the receipts for all transactions in a given block.
// GetReceiptsByHashは、指定されたブロック内のすべてのトランザクションの領収書を取得します。
func (bc *BlockChain) GetReceiptsByHash(hash common.Hash) types.Receipts {
	if receipts, ok := bc.receiptsCache.Get(hash); ok {
		return receipts.(types.Receipts)
	}
	number := rawdb.ReadHeaderNumber(bc.db, hash)
	if number == nil {
		return nil
	}
	receipts := rawdb.ReadReceipts(bc.db, hash, *number, bc.chainConfig)
	if receipts == nil {
		return nil
	}
	bc.receiptsCache.Add(hash, receipts)
	return receipts
}

// GetUnclesInChain retrieves all the uncles from a given block backwards until
// a specific distance is reached.
// GetUnclesInChainは、特定の距離に達するまで、
// 指定されたブロックからすべての叔父を逆方向に取得します。
func (bc *BlockChain) GetUnclesInChain(block *types.Block, length int) []*types.Header {
	uncles := []*types.Header{}
	for i := 0; block != nil && i < length; i++ {
		uncles = append(uncles, block.Uncles()...)
		block = bc.GetBlock(block.ParentHash(), block.NumberU64()-1)
	}
	return uncles
}

// GetCanonicalHash returns the canonical hash for a given block number
// GetCanonicalHashは、指定されたブロック番号の正規ハッシュを返します
func (bc *BlockChain) GetCanonicalHash(number uint64) common.Hash {
	return bc.hc.GetCanonicalHash(number)
}

// GetAncestor retrieves the Nth ancestor of a given block. It assumes that either the given block or
// a close ancestor of it is canonical. maxNonCanonical points to a downwards counter limiting the
// number of blocks to be individually checked before we reach the canonical chain.
//
// Note: ancestor == 0 returns the same block, 1 returns its parent and so on.
// GetAncestorは、指定されたブロックのN番目の祖先を取得します。
// これは、指定されたブロックまたはその近縁のブロックのいずれかが正規であると想定しています。
// maxNonCanonicalは、正規チェーンに到達する前に個別にチェックされるブロックの数を制限する下向きのカウンターを指します。
//
// 注：ancestor == 0は同じブロックを返し、1はその親を返します。
func (bc *BlockChain) GetAncestor(hash common.Hash, number, ancestor uint64, maxNonCanonical *uint64) (common.Hash, uint64) {
	return bc.hc.GetAncestor(hash, number, ancestor, maxNonCanonical)
}

// GetTransactionLookup retrieves the lookup associate with the given transaction
// hash from the cache or database.
// GetTransactionLookupは、指定されたトランザクションハッシュに関連付けられた
// ルックアップをキャッシュまたはデータベースから取得します。
func (bc *BlockChain) GetTransactionLookup(hash common.Hash) *rawdb.LegacyTxLookupEntry {
	// Short circuit if the txlookup already in the cache, retrieve otherwise
	// txlookupがすでにキャッシュにある場合は短絡し、そうでない場合は取得します
	if lookup, exist := bc.txLookupCache.Get(hash); exist {
		return lookup.(*rawdb.LegacyTxLookupEntry)
	}
	tx, blockHash, blockNumber, txIndex := rawdb.ReadTransaction(bc.db, hash)
	if tx == nil {
		return nil
	}
	lookup := &rawdb.LegacyTxLookupEntry{BlockHash: blockHash, BlockIndex: blockNumber, Index: txIndex}
	bc.txLookupCache.Add(hash, lookup)
	return lookup
}

// GetTd retrieves a block's total difficulty in the canonical chain from the
// database by hash and number, caching it if found.
// GetTdは、データベースから正規チェーン内のブロックの全体的な難易度を
// ハッシュと数値で取得し、見つかった場合はキャッシュします。
func (bc *BlockChain) GetTd(hash common.Hash, number uint64) *big.Int {
	return bc.hc.GetTd(hash, number)
}

// HasState checks if state trie is fully present in the database or not.
// HasStateは、状態トライがデータベースに完全に存在するかどうかをチェックします。
func (bc *BlockChain) HasState(hash common.Hash) bool {
	_, err := bc.stateCache.OpenTrie(hash)
	return err == nil
}

// HasBlockAndState checks if a block and associated state trie is fully present
// in the database or not, caching it if present.
// HasBlockAndStateは、ブロックと関連する状態トライがデータベースに完全に存在するかどうかをチェックし、
// 存在する場合はキャッシュします。
func (bc *BlockChain) HasBlockAndState(hash common.Hash, number uint64) bool {
	// Check first that the block itself is known
	// 最初にブロック自体が既知であることを確認します
	block := bc.GetBlock(hash, number)
	if block == nil {
		return false
	}
	return bc.HasState(block.Root())
}

// TrieNode retrieves a blob of data associated with a trie node
// either from ephemeral in-memory cache, or from persistent storage.
// TrieNodeは、一時的なメモリ内キャッシュまたは永続ストレージのいずれかから、
// トライノードに関連付けられたデータのブロブを取得します。
func (bc *BlockChain) TrieNode(hash common.Hash) ([]byte, error) {
	return bc.stateCache.TrieDB().Node(hash)
}

// ContractCode retrieves a blob of data associated with a contract hash
// either from ephemeral in-memory cache, or from persistent storage.
// ContractCodeは、一時的なメモリ内キャッシュまたは永続ストレージのいずれかから、
// コントラクトハッシュに関連付けられたデータのblobを取得します。
func (bc *BlockChain) ContractCode(hash common.Hash) ([]byte, error) {
	return bc.stateCache.ContractCode(common.Hash{}, hash)
}

// ContractCodeWithPrefix retrieves a blob of data associated with a contract
// hash either from ephemeral in-memory cache, or from persistent storage.
//
// If the code doesn't exist in the in-memory cache, check the storage with
// new code scheme.
// ContractCodeWithPrefixは、一時的なメモリ内キャッシュまたは永続ストレージのいずれかから、
// コントラクトハッシュに関連付けられたデータのblobを取得します。
//
// コードがメモリ内キャッシュに存在しない場合は、新しいコードスキームでストレージを確認します。
func (bc *BlockChain) ContractCodeWithPrefix(hash common.Hash) ([]byte, error) {
	type codeReader interface {
		ContractCodeWithPrefix(addrHash, codeHash common.Hash) ([]byte, error)
	}
	return bc.stateCache.(codeReader).ContractCodeWithPrefix(common.Hash{}, hash)
}

// State returns a new mutable state based on the current HEAD block.
// Stateは、現在のHEADブロックに基づいて新しい可変状態を返します。
func (bc *BlockChain) State() (*state.StateDB, error) {
	return bc.StateAt(bc.CurrentBlock().Root())
}

// StateAt returns a new mutable state based on a particular point in time.
// StateAtは、特定の時点に基づいて新しい可変状態を返します。
func (bc *BlockChain) StateAt(root common.Hash) (*state.StateDB, error) {
	return state.New(root, bc.stateCache, bc.snaps)
}

// Config retrieves the chain's fork configuration.
// Configは、チェーンのフォーク構成を取得します。
func (bc *BlockChain) Config() *params.ChainConfig { return bc.chainConfig }

// Engine retrieves the blockchain's consensus engine.
// エンジンは、ブロックチェーンのコンセンサスエンジンを取得します。
func (bc *BlockChain) Engine() consensus.Engine { return bc.engine }

// Snapshots returns the blockchain snapshot tree.
// スナップショットはブロックチェーンスナップショットツリーを返します。
func (bc *BlockChain) Snapshots() *snapshot.Tree {
	return bc.snaps
}

// Validator returns the current validator.
// Validatorは現在のバリデーターを返します。
func (bc *BlockChain) Validator() Validator {
	return bc.validator
}

// Processor returns the current processor.
// プロセッサは現在のプロセッサを返します。
func (bc *BlockChain) Processor() Processor {
	return bc.processor
}

// StateCache returns the caching database underpinning the blockchain instance.
// StateCacheは、ブロックチェーンインスタンスを支えるキャッシングデータベースを返します。
func (bc *BlockChain) StateCache() state.Database {
	return bc.stateCache
}

// GasLimit returns the gas limit of the current HEAD block.
// GasLimitは、現在のHEADブロックのガス制限を返します。
func (bc *BlockChain) GasLimit() uint64 {
	return bc.CurrentBlock().GasLimit()
}

// Genesis retrieves the chain's genesis block.
// Genesisは、チェーンのジェネシスブロックを取得します。
func (bc *BlockChain) Genesis() *types.Block {
	return bc.genesisBlock
}

// GetVMConfig returns the block chain VM config.
// GetVMConfigはブロックチェーンVM構成を返します。
func (bc *BlockChain) GetVMConfig() *vm.Config {
	return &bc.vmConfig
}

// SetTxLookupLimit is responsible for updating the txlookup limit to the
// original one stored in db if the new mismatches with the old one.
// SetTxLookupLimitは、新しい制限が古い制限と一致しない場合に、
// dbに保存されている元の制限にtxlookup制限を更新する役割を果たします。
func (bc *BlockChain) SetTxLookupLimit(limit uint64) {
	bc.txLookupLimit = limit
}

// TxLookupLimit retrieves the txlookup limit used by blockchain to prune
// stale transaction indices.
// TxLookupLimitは、ブロックチェーンが古いトランザクションインデックスを整理するために使用するtxlookup制限を取得します。
func (bc *BlockChain) TxLookupLimit() uint64 {
	return bc.txLookupLimit
}

// SubscribeRemovedLogsEvent registers a subscription of RemovedLogsEvent.
// SubscribeRemovedLogsEventはRemovedLogsEventのサブスクリプションを登録します。
func (bc *BlockChain) SubscribeRemovedLogsEvent(ch chan<- RemovedLogsEvent) event.Subscription {
	return bc.scope.Track(bc.rmLogsFeed.Subscribe(ch))
}

// SubscribeChainEvent registers a subscription of ChainEvent.
// SubscribeChainEventはChainEventのサブスクリプションを登録します。
func (bc *BlockChain) SubscribeChainEvent(ch chan<- ChainEvent) event.Subscription {
	return bc.scope.Track(bc.chainFeed.Subscribe(ch))
}

// SubscribeChainHeadEvent registers a subscription of ChainHeadEvent.
// SubscribeChainHeadEventは、ChainHeadEventのサブスクリプションを登録します。
func (bc *BlockChain) SubscribeChainHeadEvent(ch chan<- ChainHeadEvent) event.Subscription {
	return bc.scope.Track(bc.chainHeadFeed.Subscribe(ch))
}

// SubscribeChainSideEvent registers a subscription of ChainSideEvent.
// SubscribeChainSideEventは、ChainSideEventのサブスクリプションを登録します。
func (bc *BlockChain) SubscribeChainSideEvent(ch chan<- ChainSideEvent) event.Subscription {
	return bc.scope.Track(bc.chainSideFeed.Subscribe(ch))
}

// SubscribeLogsEvent registers a subscription of []*types.Log.
// SubscribeLogsEventは、[] * types.Logのサブスクリプションを登録します。
func (bc *BlockChain) SubscribeLogsEvent(ch chan<- []*types.Log) event.Subscription {
	return bc.scope.Track(bc.logsFeed.Subscribe(ch))
}

// SubscribeBlockProcessingEvent registers a subscription of bool where true means
// block processing has started while false means it has stopped.
// SubscribeBlockProcessingEventはboolのサブスクリプションを登録します。
// ここで、trueはブロック処理が開始されたことを意味し、falseはブロック処理が停止したことを意味します。
func (bc *BlockChain) SubscribeBlockProcessingEvent(ch chan<- bool) event.Subscription {
	return bc.scope.Track(bc.blockProcFeed.Subscribe(ch))
}
