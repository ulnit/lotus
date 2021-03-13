package splitstore

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/lotus/blockstore"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/types/mock"

	blocks "github.com/ipfs/go-block-format"
	datastore "github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	logging "github.com/ipfs/go-log/v2"
)

func init() {
	CompactionThreshold = 5
	CompactionCold = 1
	CompactionBoundary = 2
	logging.SetLogLevel("splitstore", "DEBUG")
}

func testSplitStore(t *testing.T, cfg *Config) {
	chain := &mockChain{}

	// the myriads of stores
	ds := dssync.MutexWrap(datastore.NewMapDatastore())
	hot := blockstore.NewMemorySync()
	cold := blockstore.NewMemorySync()

	// this is necessary to avoid the garbage mock puts in the blocks
	garbage := blocks.NewBlock([]byte{1, 2, 3})
	err := cold.Put(garbage)
	if err != nil {
		t.Fatal(err)
	}

	// genesis
	genBlock := mock.MkBlock(nil, 0, 0)
	genBlock.Messages = garbage.Cid()
	genBlock.ParentMessageReceipts = garbage.Cid()
	genBlock.ParentStateRoot = garbage.Cid()

	genTs := mock.TipSet(genBlock)
	chain.push(genTs)

	// put the genesis block to cold store
	blk, err := genBlock.ToStorageBlock()
	if err != nil {
		t.Fatal(err)
	}

	err = cold.Put(blk)
	if err != nil {
		t.Fatal(err)
	}

	// open the splitstore
	ss, err := Open("", ds, hot, cold, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close() //nolint

	err = ss.Start(chain)
	if err != nil {
		t.Fatal(err)
	}

	// make some tipsets, but not enough to cause compaction
	mkBlock := func(curTs *types.TipSet, i int) *types.TipSet {
		blk := mock.MkBlock(curTs, uint64(i), uint64(i))

		blk.Messages = garbage.Cid()
		blk.ParentMessageReceipts = garbage.Cid()
		blk.ParentStateRoot = garbage.Cid()

		sblk, err := blk.ToStorageBlock()
		if err != nil {
			t.Fatal(err)
		}
		err = ss.Put(sblk)
		if err != nil {
			t.Fatal(err)
		}
		ts := mock.TipSet(blk)
		chain.push(ts)

		return ts
	}

	waitForCompaction := func() {
		for atomic.LoadInt32(&ss.compacting) == 1 {
			time.Sleep(100 * time.Millisecond)
		}
	}

	curTs := genTs
	for i := 1; i < 5; i++ {
		curTs = mkBlock(curTs, i)
		waitForCompaction()
	}

	// count objects in the cold and hot stores
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	countBlocks := func(bs blockstore.Blockstore) int {
		count := 0
		ch, err := bs.AllKeysChan(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for range ch {
			count++
		}
		return count
	}

	coldCnt := countBlocks(cold)
	hotCnt := countBlocks(hot)

	if coldCnt != 2 {
		t.Errorf("expected %d blocks, but got %d", 2, coldCnt)
	}

	if hotCnt != 5 {
		t.Errorf("expected %d blocks, but got %d", 5, hotCnt)
	}

	// trigger a compaction
	for i := 5; i < 10; i++ {
		curTs = mkBlock(curTs, i)
		waitForCompaction()
	}

	coldCnt = countBlocks(cold)
	hotCnt = countBlocks(hot)

	if coldCnt != 7 {
		t.Errorf("expected %d cold blocks, but got %d", 7, coldCnt)
	}

	if hotCnt != 4 {
		t.Errorf("expected %d hot blocks, but got %d", 4, hotCnt)
	}
}

func TestSplitStoreSimpleCompaction(t *testing.T) {
	testSplitStore(t, &Config{TrackingStoreType: "mem"})
}

type mockChain struct {
	sync.Mutex
	tipsets  []*types.TipSet
	listener func(revert []*types.TipSet, apply []*types.TipSet) error
}

func (c *mockChain) push(ts *types.TipSet) {
	c.Lock()
	c.tipsets = append(c.tipsets, ts)
	c.Unlock()

	if c.listener != nil {
		err := c.listener(nil, []*types.TipSet{ts})
		if err != nil {
			log.Errorf("mockchain: error dispatching listener: %s", err)
		}
	}
}

func (c *mockChain) GetTipsetByHeight(_ context.Context, epoch abi.ChainEpoch, _ *types.TipSet, _ bool) (*types.TipSet, error) {
	c.Lock()
	defer c.Unlock()

	iEpoch := int(epoch)
	if iEpoch > len(c.tipsets) {
		return nil, fmt.Errorf("bad epoch %d", epoch)
	}

	return c.tipsets[iEpoch], nil
}

func (c *mockChain) GetHeaviestTipSet() *types.TipSet {
	c.Lock()
	defer c.Unlock()

	return c.tipsets[len(c.tipsets)-1]
}

func (c *mockChain) SubscribeHeadChanges(change func(revert []*types.TipSet, apply []*types.TipSet) error) {
	c.listener = change
}
