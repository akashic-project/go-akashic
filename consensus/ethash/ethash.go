// Copyright 2017 The go-ethereum Authors
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

// Package ethash implements the ethash proof-of-work consensus engine.
// パッケージethashは、ethashのプルーフオブワークコンセンサスエンジンを実装します。
package ethash

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/edsrzf/mmap-go"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/metrics"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/hashicorp/golang-lru/simplelru"
)

var ErrInvalidDumpMagic = errors.New("invalid dump magic")

var (
	// two256 is a big integer representing 2^256
	// two256は、2 ^ 256を表す大きな整数です
	two256 = new(big.Int).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0))

	// sharedEthash is a full instance that can be shared between multiple users.
	// sharedEthashは、複数のユーザー間で共有できる完全なインスタンスです。
	sharedEthash *Ethash

	// algorithmRevision is the data structure version used for file naming.
	// AlgorithmRevisionは、ファイルの命名に使用されるデータ構造バージョンです。
	algorithmRevision = 23

	// dumpMagic is a dataset dump header to sanity check a data dump.
	// dumpMagicは、データダンプを健全性チェックするためのデータセットダンプヘッダーです。
	dumpMagic = []uint32{0xbaddcafe, 0xfee1dead}
)

func init() {
	sharedConfig := Config{
		PowMode:       ModeNormal,
		CachesInMem:   3,
		DatasetsInMem: 1,
	}
	sharedEthash = New(sharedConfig, nil, false)
}

// isLittleEndian returns whether the local system is running in little or big
// endian byte order.
// isLittleEndianは、ローカルシステムがリトルエンディアンまたはビッグエンディアンのどちらのバイトオーダーで実行されているかを返します。
func isLittleEndian() bool {
	n := uint32(0x01020304)
	return *(*byte)(unsafe.Pointer(&n)) == 0x04
}

// memoryMap tries to memory map a file of uint32s for read only access.
// memoryMapは、読み取り専用アクセスのためにuint32sのファイルをメモリマップしようとします。
func memoryMap(path string, lock bool) (*os.File, mmap.MMap, []uint32, error) {
	file, err := os.OpenFile(path, os.O_RDONLY, 0644)
	if err != nil {
		return nil, nil, nil, err
	}
	mem, buffer, err := memoryMapFile(file, false)
	if err != nil {
		file.Close()
		return nil, nil, nil, err
	}
	for i, magic := range dumpMagic {
		if buffer[i] != magic {
			mem.Unmap()
			file.Close()
			return nil, nil, nil, ErrInvalidDumpMagic
		}
	}
	if lock {
		if err := mem.Lock(); err != nil {
			mem.Unmap()
			file.Close()
			return nil, nil, nil, err
		}
	}
	return file, mem, buffer[len(dumpMagic):], err
}

// memoryMapFile tries to memory map an already opened file descriptor.
// memoryMapFileは、すでに開いているファイル記述子をメモリマップしようとします。
func memoryMapFile(file *os.File, write bool) (mmap.MMap, []uint32, error) {
	// Try to memory map the file
	// ファイルのメモリマップを試みます
	flag := mmap.RDONLY
	if write {
		flag = mmap.RDWR
	}
	mem, err := mmap.Map(file, flag, 0)
	if err != nil {
		return nil, nil, err
	}
	// The file is now memory-mapped. Create a []uint32 view of the file.
	// ファイルがメモリマップされました。ファイルの[] uint32ビューを作成します。
	var view []uint32
	header := (*reflect.SliceHeader)(unsafe.Pointer(&view))
	header.Data = (*reflect.SliceHeader)(unsafe.Pointer(&mem)).Data
	header.Cap = len(mem) / 4
	header.Len = header.Cap
	return mem, view, nil
}

// memoryMapAndGenerate tries to memory map a temporary file of uint32s for write
// access, fill it with the data from a generator and then move it into the final
// path requested.

// memory Map And Generateは、書き込みアクセス用にint32の一時ファイルをメモリマップし、
// ジェネレータからのデータを入力して、要求された最終パスに移動しようとします。
func memoryMapAndGenerate(path string, size uint64, lock bool, generator func(buffer []uint32)) (*os.File, mmap.MMap, []uint32, error) {
	// Ensure the data folder exists
	// データフォルダが存在することを確認します
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, nil, nil, err
	}
	// Create a huge temporary empty file to fill with data
	// データを入力するための巨大な一時的な空のファイルを作成します
	temp := path + "." + strconv.Itoa(rand.Int())

	dump, err := os.Create(temp)
	if err != nil {
		return nil, nil, nil, err
	}
	if err = ensureSize(dump, int64(len(dumpMagic))*4+int64(size)); err != nil {
		dump.Close()
		os.Remove(temp)
		return nil, nil, nil, err
	}
	// Memory map the file for writing and fill it with the generator
	// 書き込み用にファイルをメモリマップし、ジェネレータで埋めます
	mem, buffer, err := memoryMapFile(dump, true)
	if err != nil {
		dump.Close()
		os.Remove(temp)
		return nil, nil, nil, err
	}
	copy(buffer, dumpMagic)

	data := buffer[len(dumpMagic):]
	generator(data)

	if err := mem.Unmap(); err != nil {
		return nil, nil, nil, err
	}
	if err := dump.Close(); err != nil {
		return nil, nil, nil, err
	}
	if err := os.Rename(temp, path); err != nil {
		return nil, nil, nil, err
	}
	return memoryMap(path, lock)
}

// lru tracks caches or datasets by their last use time, keeping at most N of them.
// lruは、キャッシュまたはデータセットを最後の使用時間で追跡し、最大でN個を保持します。
type lru struct {
	what string
	new  func(epoch uint64) interface{}
	mu   sync.Mutex
	// Items are kept in a LRU cache, but there is a special case:
	// We always keep an item for (highest seen epoch) + 1 as the 'future item'.
	// アイテムはLRUキャッシュに保持されますが、特別な場合があります。
	//（最も高いエポック）+1のアイテムを「将来のアイテム」として常に保持します。
	cache      *simplelru.LRU
	future     uint64
	futureItem interface{}
}

// newlru create a new least-recently-used cache for either the verification caches
// or the mining datasets.
// newlruは、検証キャッシュまたはマイニングデータセットのいずれかに対して、
// 最も使用頻度の低い新しいキャッシュを作成します。
func newlru(what string, maxItems int, new func(epoch uint64) interface{}) *lru {
	if maxItems <= 0 {
		maxItems = 1
	}
	cache, _ := simplelru.NewLRU(maxItems, func(key, value interface{}) {
		log.Trace("Evicted ethash "+what, "epoch", key)
	})
	return &lru{what: what, new: new, cache: cache}
}

// get retrieves or creates an item for the given epoch. The first return value is always
// non-nil. The second return value is non-nil if lru thinks that an item will be useful in
// the near future.
// getは、指定されたエポックのアイテムを取得または作成します。最初の戻り値は常に非nilです。
// lruがアイテムが近い将来有用であると考える場合、2番目の戻り値はnilではありません。
func (lru *lru) get(epoch uint64) (item, future interface{}) {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	// Get or create the item for the requested epoch.
	// 要求されたエポックのアイテムを取得または作成します。
	item, ok := lru.cache.Get(epoch)
	if !ok {
		if lru.future > 0 && lru.future == epoch {
			item = lru.futureItem
		} else {
			log.Trace("Requiring new ethash "+lru.what, "epoch", epoch)
			item = lru.new(epoch)
		}
		lru.cache.Add(epoch, item)
	}
	// Update the 'future item' if epoch is larger than previously seen.
	// エポックが以前に表示されたものよりも大きい場合は、「将来のアイテム」を更新します。
	if epoch < maxEpoch-1 && lru.future < epoch+1 {
		log.Trace("Requiring new future ethash "+lru.what, "epoch", epoch+1)
		future = lru.new(epoch + 1)
		lru.future = epoch + 1
		lru.futureItem = future
	}
	return item, future
}

// cache wraps an ethash cache with some metadata to allow easier concurrent use.
// キャッシュは、同時使用を容易にするために、メタデータでethashキャッシュをラップします。
type cache struct {
	epoch uint64    // このキャッシュが関連するエポック                         // Epoch for which this cache is relevant
	dump  *os.File  // メモリマップトキャッシュのファイル記述子                  // File descriptor of the memory mapped cache
	mmap  mmap.MMap // メモリマップ自体を解放する前にマップを解除します           // Memory map itself to unmap before releasing
	cache []uint32  // 実際のキャッシュデータコンテンツ（メモリマップされている可能性があります）// The actual cache data content (may be memory mapped)
	once  sync.Once // キャッシュが1回だけ生成されるようにします                 // Ensures the cache is generated only once
}

// newCache creates a new ethash verification cache and returns it as a plain Go
// interface to be usable in an LRU cache.
// newCacheは、新しいethash検証キャッシュを作成し、
// LRUキャッシュで使用できるようにプレーンなGoインターフェイスとして返します。
func newCache(epoch uint64) interface{} {
	return &cache{epoch: epoch}
}

// generate ensures that the cache content is generated before use.
// generateは、キャッシュコンテンツが使用前に生成されることを保証します。
func (c *cache) generate(dir string, limit int, lock bool, test bool) {
	c.once.Do(func() {
		size := cacheSize(c.epoch*epochLength + 1)
		seed := seedHash(c.epoch*epochLength + 1)
		if test {
			size = 1024
		}
		// If we don't store anything on disk, generate and return.
		// ディスクに何も保存しない場合は、生成して返します。
		if dir == "" {
			c.cache = make([]uint32, size/4)
			generateCache(c.cache, c.epoch, seed)
			return
		}
		// Disk storage is needed, this will get fancy
		// ディスクストレージが必要です、これは派手になります
		var endian string
		if !isLittleEndian() {
			endian = ".be"
		}
		path := filepath.Join(dir, fmt.Sprintf("cache-R%d-%x%s", algorithmRevision, seed[:8], endian))
		logger := log.New("epoch", c.epoch)

		// We're about to mmap the file, ensure that the mapping is cleaned up when the
		// cache becomes unused.
		// ファイルをmmapしようとしています。キャッシュが使用されなくなったときに、
		// マッピングがクリーンアップされていることを確認してください。
		runtime.SetFinalizer(c, (*cache).finalizer)

		// Try to load the file from disk and memory map it
		// ディスクからファイルをロードし、メモリマップを試みます
		var err error
		c.dump, c.mmap, c.cache, err = memoryMap(path, lock)
		if err == nil {
			logger.Debug("Loaded old ethash cache from disk")
			return
		}
		logger.Debug("Failed to load old ethash cache", "err", err)

		// No previous cache available, create a new cache file to fill
		// 使用可能な以前のキャッシュがありません、埋めるために新しいキャッシュファイルを作成します
		c.dump, c.mmap, c.cache, err = memoryMapAndGenerate(path, size, lock, func(buffer []uint32) { generateCache(buffer, c.epoch, seed) })
		if err != nil {
			logger.Error("Failed to generate mapped ethash cache", "err", err)

			c.cache = make([]uint32, size/4)
			generateCache(c.cache, c.epoch, seed)
		}
		// Iterate over all previous instances and delete old ones
		// 以前のすべてのインスタンスを繰り返し処理し、古いインスタンスを削除します
		for ep := int(c.epoch) - limit; ep >= 0; ep-- {
			seed := seedHash(uint64(ep)*epochLength + 1)
			path := filepath.Join(dir, fmt.Sprintf("cache-R%d-%x%s", algorithmRevision, seed[:8], endian))
			os.Remove(path)
		}
	})
}

// finalizer unmaps the memory and closes the file.
// ファイナライザーはメモリのマップを解除し、ファイルを閉じます。
func (c *cache) finalizer() {
	if c.mmap != nil {
		c.mmap.Unmap()
		c.dump.Close()
		c.mmap, c.dump = nil, nil
	}
}

// dataset wraps an ethash dataset with some metadata to allow easier concurrent use.
// データセットは、同時使用を容易にするために、メタデータでethashデータセットをラップします。
type dataset struct {
	epoch   uint64    // このキャッシュが関連するエポック                // Epoch for which this cache is relevant
	dump    *os.File  // メモリマップトキャッシュのファイル記述子        // File descriptor of the memory mapped cache
	mmap    mmap.MMap // メモリマップ自体を解放する前にマップを解除します // Memory map itself to unmap before releasing
	dataset []uint32  // 実際のキャッシュデータコンテンツ               // The actual cache data content
	once    sync.Once // キャッシュが1回だけ生成されるようにします       // Ensures the cache is generated only once
	done    uint32    //生成ステータスを決定するためのアトミックフラグ   // Atomic flag to determine generation status
}

// newDataset creates a new ethash mining dataset and returns it as a plain Go
// interface to be usable in an LRU cache.
// newDatasetは、新しいethashマイニングデータセットを作成し、
// それをプレーンなGoインターフェイスとして返し、LRUキャッシュで使用できるようにします。
func newDataset(epoch uint64) interface{} {
	return &dataset{epoch: epoch}
}

// generate ensures that the dataset content is generated before use.
// generateは、データセットコンテンツが使用前に生成されることを保証します。
func (d *dataset) generate(dir string, limit int, lock bool, test bool) {
	d.once.Do(func() {
		// Mark the dataset generated after we're done. This is needed for remote
		// 完了後に生成されたデータセットをマークします。これはリモートに必要です
		defer atomic.StoreUint32(&d.done, 1)

		csize := cacheSize(d.epoch*epochLength + 1)
		dsize := datasetSize(d.epoch*epochLength + 1)
		seed := seedHash(d.epoch*epochLength + 1)
		if test {
			csize = 1024
			dsize = 32 * 1024
		}
		// If we don't store anything on disk, generate and return
		// ディスクに何も保存しない場合は、生成して返します
		if dir == "" {
			cache := make([]uint32, csize/4)
			generateCache(cache, d.epoch, seed)

			d.dataset = make([]uint32, dsize/4)
			generateDataset(d.dataset, d.epoch, cache)

			return
		}
		// Disk storage is needed, this will get fancy
		// ディスクストレージが必要です、これは派手になります
		var endian string
		if !isLittleEndian() {
			endian = ".be"
		}
		path := filepath.Join(dir, fmt.Sprintf("full-R%d-%x%s", algorithmRevision, seed[:8], endian))
		logger := log.New("epoch", d.epoch)

		// We're about to mmap the file, ensure that the mapping is cleaned up when the
		// cache becomes unused.
		// ファイルをmmapしようとしています。キャッシュが使用されなくなったときに、
		// マッピングがクリーンアップされていることを確認してください。
		runtime.SetFinalizer(d, (*dataset).finalizer)

		// Try to load the file from disk and memory map it
		// ディスクからファイルをロードし、メモリマップを試みます
		var err error
		d.dump, d.mmap, d.dataset, err = memoryMap(path, lock)
		if err == nil {
			logger.Debug("Loaded old ethash dataset from disk")
			return
		}
		logger.Debug("Failed to load old ethash dataset", "err", err)

		// No previous dataset available, create a new dataset file to fill
		// 以前のデータセットは利用できません。入力する新しいデータセットファイルを作成します
		cache := make([]uint32, csize/4)
		generateCache(cache, d.epoch, seed)

		d.dump, d.mmap, d.dataset, err = memoryMapAndGenerate(path, dsize, lock, func(buffer []uint32) { generateDataset(buffer, d.epoch, cache) })
		if err != nil {
			logger.Error("Failed to generate mapped ethash dataset", "err", err)

			d.dataset = make([]uint32, dsize/4)
			generateDataset(d.dataset, d.epoch, cache)
		}
		// Iterate over all previous instances and delete old ones
		// 以前のすべてのインスタンスを繰り返し処理し、古いインスタンスを削除します
		for ep := int(d.epoch) - limit; ep >= 0; ep-- {
			seed := seedHash(uint64(ep)*epochLength + 1)
			path := filepath.Join(dir, fmt.Sprintf("full-R%d-%x%s", algorithmRevision, seed[:8], endian))
			os.Remove(path)
		}
	})
}

// generated returns whether this particular dataset finished generating already
// or not (it may not have been started at all). This is useful for remote miners
// to default to verification caches instead of blocking on DAG generations.
// generatedは、この特定のデータセットがすでに生成を終了したかどうかを返します
// （まったく開始されていない可能性があります）。
// これは、リモートマイナーがDAG世代でブロックするのではなく、
// デフォルトで検証キャッシュを使用する場合に役立ちます。
func (d *dataset) generated() bool {
	return atomic.LoadUint32(&d.done) == 1
}

// finalizer closes any file handlers and memory maps open.
// ファイナライザーはすべてのファイルハンドラーを閉じ、メモリマップを開きます。
func (d *dataset) finalizer() {
	if d.mmap != nil {
		d.mmap.Unmap()
		d.dump.Close()
		d.mmap, d.dump = nil, nil
	}
}

// MakeCache generates a new ethash cache and optionally stores it to disk.
// MakeCacheは新しいethashキャッシュを生成し、オプションでそれをディスクに保存します。
func MakeCache(block uint64, dir string) {
	c := cache{epoch: block / epochLength}
	c.generate(dir, math.MaxInt32, false, false)
}

// MakeDataset generates a new ethash dataset and optionally stores it to disk.
// MakeDatasetは、新しいethashデータセットを生成し、オプションでそれをディスクに保存します。
func MakeDataset(block uint64, dir string) {
	d := dataset{epoch: block / epochLength}
	d.generate(dir, math.MaxInt32, false, false)
}

// Mode defines the type and amount of PoW verification an ethash engine makes.
//モードは、ethashエンジンが行うPoW検証のタイプと量を定義します。
type Mode uint

const (
	ModeNormal Mode = iota
	ModeShared
	ModeTest
	ModeFake
	ModeFullFake
)

// Config are the configuration parameters of the ethash.
// Configは、ethashの構成パラメーターです。
type Config struct {
	CacheDir         string
	CachesInMem      int
	CachesOnDisk     int
	CachesLockMmap   bool
	DatasetDir       string
	DatasetsInMem    int
	DatasetsOnDisk   int
	DatasetsLockMmap bool
	PowMode          Mode

	// When set, notifications sent by the remote sealer will
	// be block header JSON objects instead of work package arrays.
	// 設定すると、リモートシーラーによって送信される通知は、ワークパッケージ配列ではなくブロックヘッダーJSONオブジェクトになります。
	NotifyFull bool

	Log log.Logger `toml:"-"`
}

// Ethash is a consensus engine based on proof-of-work implementing the ethash
// algorithm.
// Ethashは、ethashアルゴリズムを実装するプルーフオブワークに基づくコンセンサスエンジンです。
type Ethash struct {
	config Config

	caches   *lru // 頻繁に再生成されないように、メモリキャッシュ内      // In memory caches to avoid regenerating too often
	datasets *lru //頻繁に再生成されないようにするためのメモリデータセット// In memory datasets to avoid regenerating too often

	// Mining related fields
	// マイニング関連フィールド
	rand     *rand.Rand    // ナンスの適切にシードされたランダムソース      // Properly seeded random source for nonces
	threads  int           // マイニングする場合にマイニングするスレッドの数 // Number of threads to mine on if mining
	update   chan struct{} //マイニングパラメータを更新する通知チャネル     // Notification channel to update mining parameters
	hashrate metrics.Meter //平均ハッシュレートを追跡するメーター          // Meter tracking the average hashrate
	remote   *remoteSealer

	// The fields below are hooks for testing
	// 以下のフィールドはテスト用のフックです
	shared    *Ethash       // キャッシュの再生成を回避するための共有PoWベリファイア // Shared PoW verifier to avoid cache regeneration
	fakeFail  uint64        // 偽のモードでもPoWチェックに失敗するブロック番号      // Block number which fails PoW check even in fake mode
	fakeDelay time.Duration //検証から戻る前にスリープするまでの時間遅延            // Time delay to sleep for before returning from verify

	lock      sync.Mutex // メモリ内キャッシュとマイニングフィールドのスレッドセーフを確保します // Ensures thread safety for the in-memory caches and mining fields
	closeOnce sync.Once  //出口チャネルが2回閉じられないようにします。                        // Ensures exit channel will not be closed twice.
}

// New creates a full sized ethash PoW scheme and starts a background thread for
// remote mining, also optionally notifying a batch of remote services of new work
// packages.
// Newは、フルサイズのethash PoWスキームを作成し、リモートマイニングのバックグラウンドスレッドを開始します。
// また、オプションで、リモートサービスのバッチに新しいワークパッケージを通知します。
func New(config Config, notify []string, noverify bool) *Ethash {
	if config.Log == nil {
		config.Log = log.Root()
	}
	if config.CachesInMem <= 0 {
		config.Log.Warn("One ethash cache must always be in memory", "requested", config.CachesInMem)
		config.CachesInMem = 1
	}
	if config.CacheDir != "" && config.CachesOnDisk > 0 {
		config.Log.Info("Disk storage enabled for ethash caches", "dir", config.CacheDir, "count", config.CachesOnDisk)
	}
	if config.DatasetDir != "" && config.DatasetsOnDisk > 0 {
		config.Log.Info("Disk storage enabled for ethash DAGs", "dir", config.DatasetDir, "count", config.DatasetsOnDisk)
	}
	ethash := &Ethash{
		config:   config,
		caches:   newlru("cache", config.CachesInMem, newCache),
		datasets: newlru("dataset", config.DatasetsInMem, newDataset),
		update:   make(chan struct{}),
		hashrate: metrics.NewMeterForced(),
	}
	if config.PowMode == ModeShared {
		ethash.shared = sharedEthash
	}
	ethash.remote = startRemoteSealer(ethash, notify, noverify)
	return ethash
}

// NewTester creates a small sized ethash PoW scheme useful only for testing
// purposes.
// NewTesterは、テスト目的でのみ役立つ小さなサイズのethashPoWスキームを作成します。
func NewTester(notify []string, noverify bool) *Ethash {
	return New(Config{PowMode: ModeTest}, notify, noverify)
}

// NewFaker creates a ethash consensus engine with a fake PoW scheme that accepts
// all blocks' seal as valid, though they still have to conform to the Ethereum
// consensus rules.
// NewFakerは、すべてのブロックのシールを有効なものとして受け入れる偽のPoWスキームを使用して、
// イーサリアムコンセンサスエンジンを作成しますが、イーサリアムコンセンサスルールに準拠する必要があります。
func NewFaker() *Ethash {
	return &Ethash{
		config: Config{
			PowMode: ModeFake,
			Log:     log.Root(),
		},
	}
}

// NewFakeFailer creates a ethash consensus engine with a fake PoW scheme that
// accepts all blocks as valid apart from the single one specified, though they
// still have to conform to the Ethereum consensus rules.
// NewFakeFailerは、指定された単一のブロックを除いてすべてのブロックを有効として受け入れる偽のPoWスキームを使用して、
// イーサリアムコンセンサスエンジンを作成しますが、イーサリアムコンセンサスルールに準拠する必要があります。
func NewFakeFailer(fail uint64) *Ethash {
	return &Ethash{
		config: Config{
			PowMode: ModeFake,
			Log:     log.Root(),
		},
		fakeFail: fail,
	}
}

// NewFakeDelayer creates a ethash consensus engine with a fake PoW scheme that
// accepts all blocks as valid, but delays verifications by some time, though
// they still have to conform to the Ethereum consensus rules.

// NewFakeDelayerは、すべてのブロックを有効なものとして受け入れる偽のPoWスキームを使用して、
// イーサリアムコンセンサスエンジンを作成しますが、イーサリアムコンセンサスルールに準拠する必要がありますが、検証をしばらく遅らせます。
func NewFakeDelayer(delay time.Duration) *Ethash {
	return &Ethash{
		config: Config{
			PowMode: ModeFake,
			Log:     log.Root(),
		},
		fakeDelay: delay,
	}
}

// NewFullFaker creates an ethash consensus engine with a full fake scheme that
// accepts all blocks as valid, without checking any consensus rules whatsoever.
// NewFullFakerは、コンセンサスルールをまったくチェックせずに、
// すべてのブロックを有効なものとして受け入れる完全な偽のスキームを使用してethashコンセンサスエンジンを作成します。
func NewFullFaker() *Ethash {
	return &Ethash{
		config: Config{
			PowMode: ModeFullFake,
			Log:     log.Root(),
		},
	}
}

// NewShared creates a full sized ethash PoW shared between all requesters running
// in the same process.
// NewSharedは、同じプロセスで実行されているすべてのリクエスター間で共有されるフルサイズのethashPoWを作成します。
func NewShared() *Ethash {
	return &Ethash{shared: sharedEthash}
}

// Close closes the exit channel to notify all backend threads exiting.
// Closeは、終了チャネルを閉じて、終了するすべてのバックエンドスレッドに通知します。
func (ethash *Ethash) Close() error {
	ethash.closeOnce.Do(func() {
		// Short circuit if the exit channel is not allocated.
		// 出口チャネルが割り当てられていない場合は短絡します。
		if ethash.remote == nil {
			return
		}
		close(ethash.remote.requestExit)
		<-ethash.remote.exitCh
	})
	return nil
}

// cache tries to retrieve a verification cache for the specified block number
// by first checking against a list of in-memory caches, then against caches
// stored on disk, and finally generating one if none can be found.
// cacheは、最初にメモリ内キャッシュのリストをチェックし、次にディスクに保存されているキャッシュをチェックし、
// 見つからない場合は最後にキャッシュを生成することにより、指定されたブロック番号の検証キャッシュを取得しようとします。
func (ethash *Ethash) cache(block uint64) *cache {
	epoch := block / epochLength
	currentI, futureI := ethash.caches.get(epoch)
	current := currentI.(*cache)

	// Wait for generation finish.
	// 生成が終了するのを待ちます。
	current.generate(ethash.config.CacheDir, ethash.config.CachesOnDisk, ethash.config.CachesLockMmap, ethash.config.PowMode == ModeTest)

	// If we need a new future cache, now's a good time to regenerate it.
	// 新しい将来のキャッシュが必要な場合は、今がそれを再生成する良い機会です。
	if futureI != nil {
		future := futureI.(*cache)
		go future.generate(ethash.config.CacheDir, ethash.config.CachesOnDisk, ethash.config.CachesLockMmap, ethash.config.PowMode == ModeTest)
	}
	return current
}

// dataset tries to retrieve a mining dataset for the specified block number
// by first checking against a list of in-memory datasets, then against DAGs
// stored on disk, and finally generating one if none can be found.
//
// If async is specified, not only the future but the current DAG is also
// generates on a background thread.

// データセットは、最初にメモリ内データセットのリストをチェックし、次にディスクに保存されているDAGをチェックし、
// 見つからない場合は最後に生成することで、指定されたブロック番号のマイニングデータセットを取得しようとします。
//
// 非同期が指定されている場合、futureだけでなく現在のDAGもバックグラウンドスレッドで生成されます。
func (ethash *Ethash) dataset(block uint64, async bool) *dataset {
	// Retrieve the requested ethash dataset
	// 要求されたethashデータセットを取得します
	epoch := block / epochLength
	currentI, futureI := ethash.datasets.get(epoch)
	current := currentI.(*dataset)

	// If async is specified, generate everything in a background thread
	// 非同期が指定されている場合、バックグラウンドスレッドですべてを生成します
	if async && !current.generated() {
		go func() {
			current.generate(ethash.config.DatasetDir, ethash.config.DatasetsOnDisk, ethash.config.DatasetsLockMmap, ethash.config.PowMode == ModeTest)

			if futureI != nil {
				future := futureI.(*dataset)
				future.generate(ethash.config.DatasetDir, ethash.config.DatasetsOnDisk, ethash.config.DatasetsLockMmap, ethash.config.PowMode == ModeTest)
			}
		}()
	} else {
		// Either blocking generation was requested, or already done
		// ブロッキング生成が要求されたか、すでに実行されています
		current.generate(ethash.config.DatasetDir, ethash.config.DatasetsOnDisk, ethash.config.DatasetsLockMmap, ethash.config.PowMode == ModeTest)

		if futureI != nil {
			future := futureI.(*dataset)
			go future.generate(ethash.config.DatasetDir, ethash.config.DatasetsOnDisk, ethash.config.DatasetsLockMmap, ethash.config.PowMode == ModeTest)
		}
	}
	return current
}

// Threads returns the number of mining threads currently enabled. This doesn't
// necessarily mean that mining is running!
// Threadsは、現在有効になっているマイニングスレッドの数を返します。これは必ずしもマイニングが実行されていることを意味するわけではありません！
func (ethash *Ethash) Threads() int {
	ethash.lock.Lock()
	defer ethash.lock.Unlock()

	return ethash.threads
}

// SetThreads updates the number of mining threads currently enabled. Calling
// this method does not start mining, only sets the thread count. If zero is
// specified, the miner will use all cores of the machine. Setting a thread
// count below zero is allowed and will cause the miner to idle, without any
// work being done.
// SetThreadsは、現在有効になっているマイニングスレッドの数を更新します。このメソッドを呼び出してもマイニングは開始されず、スレッド数が設定されるだけです。ゼロが指定されている場合、マイナーはマシンのすべてのコアを使用します。
// スレッド数をゼロ未満に設定することは許可されており、作業を行わずにマイナーをアイドル状態にします。
func (ethash *Ethash) SetThreads(threads int) {
	ethash.lock.Lock()
	defer ethash.lock.Unlock()

	// If we're running a shared PoW, set the thread count on that instead
	// 共有PoWを実行している場合は、代わりにその上にスレッド数を設定します
	if ethash.shared != nil {
		ethash.shared.SetThreads(threads)
		return
	}
	// Update the threads and ping any running seal to pull in any changes
	// スレッドを更新し、実行中のシールにpingを実行して、変更をプルします
	ethash.threads = threads
	select {
	case ethash.update <- struct{}{}:
	default:
	}
}

// Hashrate implements PoW, returning the measured rate of the search invocations
// per second over the last minute.
// Note the returned hashrate includes local hashrate, but also includes the total
// hashrate of all remote miner.

// HashrateはPoWを実装し、最後の1分間の1秒あたりの検索呼び出しの測定レートを返します。
// 返されるハッシュレートにはローカルハッシュレートが含まれますが、
// すべてのリモートマイナーの合計ハッシュレートも含まれることに注意してください。
func (ethash *Ethash) Hashrate() float64 {
	// Short circuit if we are run the ethash in normal/test mode.
	// 通常/テストモードでethashを実行すると短絡します。
	if ethash.config.PowMode != ModeNormal && ethash.config.PowMode != ModeTest {
		return ethash.hashrate.Rate1()
	}
	var res = make(chan uint64, 1)

	select {
	case ethash.remote.fetchRateCh <- res:
	case <-ethash.remote.exitCh:
		// Return local hashrate only if ethash is stopped.
		// ethashが停止している場合にのみローカルハッシュレートを返します。
		return ethash.hashrate.Rate1()
	}

	// Gather total submitted hash rate of remote sealers.
	// リモートシーラーの送信されたハッシュレートの合計を収集します。
	return ethash.hashrate.Rate1() + float64(<-res)
}

// APIs implements consensus.Engine, returning the user facing RPC APIs.
// APIはconsensus.Engineを実装し、RPCAPIに直面しているユーザーを返します。
func (ethash *Ethash) APIs(chain consensus.ChainHeaderReader) []rpc.API {
	// In order to ensure backward compatibility, we exposes ethash RPC APIs
	// to both eth and ethash namespaces.
	// 下位互換性を確保するために、ethash RPCAPIをethとethashの両方の名前空間に公開します。
	return []rpc.API{
		{
			Namespace: "eth",
			Version:   "1.0",
			Service:   &API{ethash},
			Public:    true,
		},
		{
			Namespace: "ethash",
			Version:   "1.0",
			Service:   &API{ethash},
			Public:    true,
		},
	}
}

// SeedHash is the seed to use for generating a verification cache and the mining
// dataset.
// SeedHashは、検証キャッシュとマイニングデータセットを生成するために使用するシードです。
func SeedHash(block uint64) []byte {
	return seedHash(block)
}
