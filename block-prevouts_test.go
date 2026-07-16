package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// TestParseSpentTxOuts checks that the binary format of the /rest/spenttxouts
// endpoint is parsed correctly and that truncated or oversized responses are
// rejected.
func TestParseSpentTxOuts(t *testing.T) {
	var buf bytes.Buffer

	writeSpentOut := func(value int64, pkScript []byte) {
		var valueBytes [8]byte
		binary.LittleEndian.PutUint64(valueBytes[:], uint64(value))
		_, err := buf.Write(valueBytes[:])
		require.NoError(t, err)
		require.NoError(t, wire.WriteVarBytes(&buf, 0, pkScript))
	}

	// A block with three transactions: the always-empty coinbase
	// placeholder, a transaction spending two outputs and one spending a
	// single output.
	script1 := []byte{0x51}
	script2 := bytes.Repeat([]byte{0x02}, 22)
	script3 := bytes.Repeat([]byte{0x03}, 34)

	require.NoError(t, wire.WriteVarInt(&buf, 0, 3))
	require.NoError(t, wire.WriteVarInt(&buf, 0, 0))
	require.NoError(t, wire.WriteVarInt(&buf, 0, 2))
	writeSpentOut(1_000, script1)
	writeSpentOut(2_000, script2)
	require.NoError(t, wire.WriteVarInt(&buf, 0, 1))
	writeSpentOut(3_000, script3)

	parsed, err := parseSpentTxOuts(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	require.Equal(t, [][]wire.TxOut{
		nil,
		{
			{Value: 1_000, PkScript: script1},
			{Value: 2_000, PkScript: script2},
		},
		{
			{Value: 3_000, PkScript: script3},
		},
	}, parsed)

	// Trailing data must be rejected.
	_, err = parseSpentTxOuts(
		bytes.NewReader(append(buf.Bytes(), 0x00)),
	)
	require.ErrorContains(t, err, "trailing data")

	// Truncated data must be rejected.
	_, err = parseSpentTxOuts(
		bytes.NewReader(buf.Bytes()[:buf.Len()-1]),
	)
	require.Error(t, err)

	// An absurdly high tx count must be rejected before any allocation.
	var oversized bytes.Buffer
	require.NoError(
		t, wire.WriteVarInt(&oversized, 0, maxSpentTxOutsPerBlock+1),
	)
	_, err = parseSpentTxOuts(bytes.NewReader(oversized.Bytes()))
	require.ErrorContains(t, err, "exceeds maximum")
}

// TestBlockPrevOutFetcher checks that the REST-based fast path returns the
// same result as the getblock verbosity 3 RPC, and that the fetcher falls
// back to the RPC when the backend's REST API isn't reachable.
func TestBlockPrevOutFetcher(t *testing.T) {
	miner, backend, backendCfg, _ := setupBackend(t)

	// Create a transaction spending miner outputs, so the next block
	// contains a non-coinbase transaction with previous output data.
	pkScript := append(
		[]byte{0x00, 0x14}, bytes.Repeat([]byte{0x02}, 20)...,
	)
	miner.SendOutput(&wire.TxOut{
		Value:    100_000,
		PkScript: pkScript,
	}, 2)

	minedBlocks := miner.MineBlocksAndAssertNumTxes(1, 1)
	waitBackendSync(t, backend, miner)
	blockHash := minedBlocks[0].BlockHash()

	// The RPC path serves as the reference result.
	rpcBlock, err := getBlockWithPrevOuts(backend, &blockHash)
	require.NoError(t, err)
	require.NotEmpty(t, rpcBlock.prevOutScripts)

	requireSameBlock := func(t *testing.T, actual *blockWithPrevOuts) {
		t.Helper()

		require.Len(t, actual.txs, len(rpcBlock.txs))
		for idx, tx := range rpcBlock.txs {
			require.Equal(
				t, tx.TxHash(), actual.txs[idx].TxHash(),
			)
		}
		require.Equal(
			t, rpcBlock.prevOutScripts, actual.prevOutScripts,
		)
	}

	// The test harness starts bitcoind with -rest, so the fetcher must
	// pick the REST fast path and the REST requirement check must pass.
	t.Run("REST", func(t *testing.T) {
		fetcher := newBlockPrevOutFetcher(backend, &backendCfg)
		require.NoError(t, fetcher.requireREST())

		block, err := fetcher.fetchBlock(&blockHash)
		require.NoError(t, err)
		require.True(t, fetcher.useREST)

		requireSameBlock(t, block)
	})

	// With an unreachable REST endpoint, the REST requirement check must
	// fail with an actionable error, while the fetcher itself falls back
	// to the getblock RPC and still produces the same result.
	t.Run("RPC fallback", func(t *testing.T) {
		cfgCopy := backendCfg
		cfgCopy.Host = "127.0.0.1:1"
		fetcher := newBlockPrevOutFetcher(backend, &cfgCopy)
		require.ErrorContains(t, fetcher.requireREST(), "rest=1")

		block, err := fetcher.fetchBlock(&blockHash)
		require.NoError(t, err)
		require.False(t, fetcher.useREST)

		requireSameBlock(t, block)
	})
}

// prefetchTestHarness is a blockPrefetcher hooked up to fake fetch and hash
// resolution functions, recording every fetch so tests can assert how often
// and for which hashes the underlying fetcher was invoked.
type prefetchTestHarness struct {
	sync.Mutex

	prefetcher *blockPrefetcher

	tip        int32
	hashSuffix byte
	fetches    map[chainhash.Hash]int
	failNext   map[chainhash.Hash]bool
}

// hashAt derives the deterministic fake hash of the given height on the
// harness' current chain. Bumping hashSuffix simulates a reorg: every
// height resolves to a new hash.
func (h *prefetchTestHarness) hashAt(height int32) *chainhash.Hash {
	var hash chainhash.Hash
	binary.LittleEndian.PutUint32(hash[:4], uint32(height))
	hash[4] = h.hashSuffix

	return &hash
}

func newPrefetchTestHarness(tip int32, depth int32) *prefetchTestHarness {
	h := &prefetchTestHarness{
		tip:      tip,
		fetches:  make(map[chainhash.Hash]int),
		failNext: make(map[chainhash.Hash]bool),
	}

	h.prefetcher = &blockPrefetcher{
		fetchSingle: func(hash *chainhash.Hash) (*blockWithPrevOuts,
			error) {

			h.Lock()
			defer h.Unlock()

			h.fetches[*hash]++
			if h.failNext[*hash] {
				delete(h.failNext, *hash)
				return nil, fmt.Errorf("transient failure")
			}

			// Embed the fetched hash in the result, so the test
			// can verify which block a fetch produced.
			return &blockWithPrevOuts{
				prevOutScripts: map[wire.OutPoint][]byte{
					{Hash: *hash}: hash[:],
				},
			}, nil
		},
		getHash: func(height int32) (*chainhash.Hash, error) {
			h.Lock()
			defer h.Unlock()

			if height > h.tip {
				return nil, fmt.Errorf("height %d out of "+
					"range", height)
			}

			return h.hashAt(height), nil
		},
		depth:   depth,
		pending: make(map[int32]prefetchedBlock),
	}

	return h
}

// fetchAndCheck fetches the block at the given height through the prefetcher
// and asserts it corresponds to the current chain's hash at that height.
func (h *prefetchTestHarness) fetchAndCheck(t *testing.T, height int32) {
	t.Helper()

	h.Lock()
	hash := h.hashAt(height)
	h.Unlock()

	block, err := h.prefetcher.fetchBlock(height, hash)
	require.NoError(t, err)
	require.Contains(t, block.prevOutScripts, wire.OutPoint{Hash: *hash})
}

// numFetches returns how often the underlying fetcher was invoked in total.
func (h *prefetchTestHarness) numFetches() int {
	h.Lock()
	defer h.Unlock()

	total := 0
	for _, count := range h.fetches {
		total += count
	}

	return total
}

// TestBlockPrefetcherSequential checks that sequential consumption returns
// the correct block for every height, fetches every block exactly once and
// never exceeds the configured prefetch depth.
func TestBlockPrefetcherSequential(t *testing.T) {
	const (
		tip   = 20
		depth = 4
	)
	h := newPrefetchTestHarness(tip, depth)

	for height := range int32(tip + 1) {
		h.fetchAndCheck(t, height)

		require.LessOrEqual(t, len(h.prefetcher.pending), depth)
	}

	// Every block was fetched exactly once: heights consumed directly
	// plus prefetched ones, no duplicates, nothing beyond the tip.
	require.Equal(t, tip+1, h.numFetches())
	require.Empty(t, h.prefetcher.pending)
}

// TestBlockPrefetcherReorg checks that prefetched blocks that were scheduled
// before a reorg are discarded and re-fetched with the post-reorg hash.
func TestBlockPrefetcherReorg(t *testing.T) {
	h := newPrefetchTestHarness(20, 4)

	// Consume a few blocks, so the window [6, 9] is prefetched with
	// pre-reorg hashes.
	for height := range int32(6) {
		h.fetchAndCheck(t, height)
	}

	// Simulate a reorg back to height 3: every height now resolves to a
	// different hash.
	h.Lock()
	h.hashSuffix = 0xff
	h.Unlock()

	// Continuing from height 4 must transparently discard the stale
	// prefetches and return the blocks of the new chain.
	for height := int32(4); height <= 10; height++ {
		h.fetchAndCheck(t, height)
	}
}

// TestBlockPrefetcherFailedPrefetch checks that a failed background fetch is
// transparently retried when its height is consumed.
func TestBlockPrefetcherFailedPrefetch(t *testing.T) {
	h := newPrefetchTestHarness(20, 4)

	// Make the background fetch of height 2 fail once. It is scheduled
	// by the first fetchBlock call, before height 2 is consumed.
	h.Lock()
	h.failNext[*h.hashAt(2)] = true
	h.Unlock()

	for height := range int32(6) {
		h.fetchAndCheck(t, height)
	}

	h.Lock()
	defer h.Unlock()
	require.Equal(t, 2, h.fetches[*h.hashAt(2)],
		"height 2 must have been fetched again after the failed "+
			"prefetch")
}

// TestVerifyPrevOutBackendVersion pins the minimum backend version required
// for indexing SP tweak data.
func TestVerifyPrevOutBackendVersion(t *testing.T) {
	require.Error(t, verifyPrevOutBackendVersion(250_000))
	require.Error(t, verifyPrevOutBackendVersion(299_900))
	require.NoError(t, verifyPrevOutBackendVersion(300_000))
	require.NoError(t, verifyPrevOutBackendVersion(310_100))
}

// TestGetBlockWithPrevOuts checks that fetching a block through the getblock
// RPC with verbosity level 3 returns the same transactions as the binary
// getblock RPC, and that the previous output scripts collected from the undo
// data match the outputs of the referenced transactions.
func TestGetBlockWithPrevOuts(t *testing.T) {
	miner, backend, _, _ := setupBackend(t)

	// Create a transaction spending miner outputs, so the next block
	// contains a non-coinbase transaction with previous output data.
	pkScript := append(
		[]byte{0x00, 0x14}, bytes.Repeat([]byte{0x01}, 20)...,
	)
	miner.SendOutput(&wire.TxOut{
		Value:    100_000,
		PkScript: pkScript,
	}, 2)

	minedBlocks := miner.MineBlocksAndAssertNumTxes(1, 1)
	waitBackendSync(t, backend, miner)

	blockHash := minedBlocks[0].BlockHash()
	block, err := getBlockWithPrevOuts(backend, &blockHash)
	require.NoError(t, err)

	// The result must contain the coinbase plus the transaction we just
	// created, in the same order as the binary getblock RPC returns them.
	rawBlock, err := backend.GetBlock(&blockHash)
	require.NoError(t, err)
	require.Len(t, block.txs, 2)
	require.Len(t, block.txs, len(rawBlock.Transactions))
	for idx, tx := range block.txs {
		require.Equal(
			t, rawBlock.Transactions[idx].TxHash(), tx.TxHash(),
		)
	}

	// The coinbase doesn't spend any previous outputs, so it must not
	// contribute an entry to the prevout map.
	coinbase := block.txs[0]
	require.True(t, blockchain.IsCoinBaseTx(coinbase))
	_, err = block.fetchPrevOutScript(coinbase.TxIn[0].PreviousOutPoint)
	require.ErrorContains(t, err, "no previous output script")

	// Every input of the non-coinbase transaction must have its previous
	// output script recorded, matching the output of the transaction it
	// references.
	for _, txIn := range block.txs[1].TxIn {
		op := txIn.PreviousOutPoint

		prevOutScript, err := block.fetchPrevOutScript(op)
		require.NoError(t, err)

		prevTx, err := backend.GetRawTransaction(&op.Hash)
		require.NoError(t, err)
		require.Equal(
			t, prevTx.MsgTx().TxOut[op.Index].PkScript,
			prevOutScript,
		)
	}
}
