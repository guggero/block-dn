package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2/gcs"
	"github.com/btcsuite/btcd/btcutil/v2/gcs/builder"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

const (
	customBlocksPerFile = 100

	// customHeadersPerFile deliberately spans multiple filter files but
	// doesn't align with where the tests stop and restart the producer,
	// so the restart has to rebuild part of the header chain from the
	// sealed filter files.
	customHeadersPerFile = 300
)

var (
	testScriptP2WPKH = append(
		[]byte{0x00, 0x14}, bytes.Repeat([]byte{0x01}, 20)...,
	)
	testScriptP2WSH = append(
		[]byte{0x00, 0x20}, bytes.Repeat([]byte{0x02}, 32)...,
	)
	testScriptP2TR = append(
		[]byte{0x51, 0x20}, bytes.Repeat([]byte{0x03}, 32)...,
	)
	testScriptP2PKH = append(append(
		[]byte{0x76, 0xa9, 0x14}, bytes.Repeat([]byte{0x04}, 20)...),
		0x88, 0xac,
	)
	testScriptOpReturn = []byte{0x6a, 0x02, 0x05, 0x05}
)

// TestClassifyScript pins the script type classification the custom filter
// configurations are based on.
func TestClassifyScript(t *testing.T) {
	require.Equal(t, scriptTypeP2WPKH, classifyScript(testScriptP2WPKH))
	require.Equal(t, scriptTypeP2WSH, classifyScript(testScriptP2WSH))
	require.Equal(t, scriptTypeP2TR, classifyScript(testScriptP2TR))
	require.Equal(t, scriptTypes(0), classifyScript(testScriptP2PKH))
	require.Equal(t, scriptTypes(0), classifyScript(testScriptOpReturn))
	require.Equal(t, scriptTypes(0), classifyScript(nil))
}

// TestBuildCustomFilters checks that each configuration's filter contains
// exactly the scripts of its configured types, from both the block's outputs
// and its spent previous outputs, with duplicates removed.
func TestBuildCustomFilters(t *testing.T) {
	blockHash := chainhash.DoubleHashH([]byte("custom filter test block"))
	key := builder.DeriveKey(&blockHash)

	// One transaction creates a p2wpkh (twice, to check de-duplication),
	// a p2pkh and an OP_RETURN output; its inputs spend a p2wsh and a
	// p2pkh output. A p2tr script is only visible on the input side.
	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: wire.OutPoint{Index: 0}},
			{PreviousOutPoint: wire.OutPoint{Index: 1}},
			{PreviousOutPoint: wire.OutPoint{Index: 2}},
		},
		TxOut: []*wire.TxOut{
			{PkScript: testScriptP2WPKH},
			{PkScript: testScriptP2WPKH},
			{PkScript: testScriptP2PKH},
			{PkScript: testScriptOpReturn},
		},
	}
	block := &blockWithPrevOuts{
		txs: []*wire.MsgTx{tx},
		prevOutScripts: map[wire.OutPoint][]byte{
			{Index: 0}: testScriptP2WSH,
			{Index: 1}: testScriptP2TR,
			{Index: 2}: testScriptP2PKH,
		},
	}

	filters, err := buildCustomFilters(&blockHash, block)
	require.NoError(t, err)
	require.Len(t, filters, len(customFilterConfigs))

	// expectedScripts maps the configuration name to the scripts its
	// filter must (and must not) contain.
	expectedScripts := map[string][][]byte{
		"segwit": {
			testScriptP2WPKH, testScriptP2WSH, testScriptP2TR,
		},
		"p2wpkh": {testScriptP2WPKH},
		"p2wsh":  {testScriptP2WSH},
		"p2tr":   {testScriptP2TR},
	}
	allScripts := [][]byte{
		testScriptP2WPKH, testScriptP2WSH, testScriptP2TR,
		testScriptP2PKH, testScriptOpReturn,
	}

	for i, cfg := range customFilterConfigs {
		expected := expectedScripts[cfg.name]

		// The duplicate p2wpkh output must only count once, so the
		// number of filter elements is the number of expected
		// scripts.
		require.EqualValues(
			t, len(expected), filters[i].N(), cfg.name,
		)

		for _, script := range allScripts {
			match, err := filters[i].Match(key, script)
			require.NoError(t, err)

			shouldMatch := false
			for _, exp := range expected {
				if bytes.Equal(exp, script) {
					shouldMatch = true
				}
			}
			require.Equalf(
				t, shouldMatch, match, "config %s, script %x",
				cfg.name, script,
			)
		}
	}

	// A block without any matching scripts must produce a valid, empty
	// filter that still serializes and chains.
	emptyBlock := &blockWithPrevOuts{
		txs: []*wire.MsgTx{{
			TxOut: []*wire.TxOut{{PkScript: testScriptP2PKH}},
		}},
	}
	filters, err = buildCustomFilters(&blockHash, emptyBlock)
	require.NoError(t, err)
	for _, filter := range filters {
		require.EqualValues(t, 0, filter.N())

		nBytes, err := filter.NBytes()
		require.NoError(t, err)
		require.Equal(t, []byte{0x00}, nBytes)

		_, err = builder.MakeHeaderForFilter(filter, chainhash.Hash{})
		require.NoError(t, err)
	}
}

// TestCustomFilterFilesUpdate runs the custom filter producer against a real
// backend, checks the produced filters match the expected scripts, verifies
// the on-disk filter header chains from genesis and checks that a restart
// resumes the header chains correctly from disk.
func TestCustomFilterFilesUpdate(t *testing.T) {
	miner, backend, backendCfg, _ := setupBackend(t)

	// Mine initial blocks. The miner starts with 200 blocks already
	// mined.
	_ = miner.MineEmptyBlocks(initialBlocks - int(totalStartupBlocks))
	waitBackendSync(t, backend, miner)

	// First run: start from scratch.
	dataDir := t.TempDir()
	quit := make(chan struct{})
	h2hCache := newH2HCache(backend)
	cf := newCustomFilterFiles(
		customBlocksPerFile, customHeadersPerFile, testReOrgSafeDepth,
		backend, quit, dataDir, &testParams, h2hCache,
		newBlockPrevOutFetcher(backend, &backendCfg),
	)

	var wg sync.WaitGroup
	waitForTargetHeight(t, &wg, cf, initialBlocks)

	// The filter files for the sealed range [0, 399] must exist (tip 450
	// minus the reorg-safe depth doesn't reach the 400-499 boundary),
	// along with the single completed header file [0, 299].
	checkCustomFilterFiles(t, dataDir, 399, 299)

	// Create one output of each custom filter type, plus a legacy p2pkh
	// control output that must not appear in any custom filter.
	spendScripts := [][]byte{
		testScriptP2WPKH, testScriptP2WSH, testScriptP2TR,
		testScriptP2PKH,
	}
	for _, pkScript := range spendScripts {
		miner.SendOutput(&wire.TxOut{
			Value:    100_000,
			PkScript: pkScript,
		}, 2)
	}

	minedBlocks := miner.MineBlocksAndAssertNumTxes(1, len(spendScripts))
	spendHeight := int32(initialBlocks + 1)
	waitBackendSync(t, backend, miner)
	waitForTargetHeight(t, &wg, cf, spendHeight)

	// The spend block is still in the unsealed tail, so we check its
	// filters straight from memory.
	spendBlockHash := minedBlocks[0].BlockHash()
	key := builder.DeriveKey(&spendBlockHash)

	cf.RLock()
	spendFilters := cf.filters[spendHeight]
	cf.RUnlock()
	require.Len(t, spendFilters, len(customFilterConfigs))

	assertMatches := func(t *testing.T, filter *gcs.Filter,
		mask scriptTypes) {

		t.Helper()

		for _, pkScript := range spendScripts {
			match, err := filter.Match(key, pkScript)
			require.NoError(t, err)
			require.Equalf(
				t, classifyScript(pkScript)&mask != 0, match,
				"script %x", pkScript,
			)
		}
	}

	for i, cfg := range customFilterConfigs {
		filter, err := gcs.FromNBytes(
			builder.DefaultP, builder.DefaultM, spendFilters[i],
		)
		require.NoError(t, err)

		assertMatches(t, filter, cfg.types)
	}

	// Cross-check against the backend: bitcoind's basic filter for the
	// same block uses the same parameters and key, so it must match all
	// of our scripts, including the p2pkh one.
	basicFilter, err := backend.GetBlockFilter(spendBlockHash, &filterBasic)
	require.NoError(t, err)
	basicBytes, err := hex.DecodeString(basicFilter.Filter)
	require.NoError(t, err)
	basic, err := gcs.FromNBytes(
		builder.DefaultP, builder.DefaultM, basicBytes,
	)
	require.NoError(t, err)
	for _, pkScript := range spendScripts {
		match, err := basic.Match(key, pkScript)
		require.NoError(t, err)
		require.Truef(t, match, "basic filter, script %x", pkScript)
	}

	// Stop the service.
	close(quit)
	wg.Wait()

	// Second run: restart and continue. The producer resumes at height
	// 400, in the middle of the header file span [300, 599], so it has
	// to rebuild the headers of [300, 399] from the sealed filter files
	// before it can continue the chains.
	const finalBlocks = 650
	_ = miner.MineEmptyBlocks(finalBlocks - int(spendHeight))
	waitBackendSync(t, backend, miner)

	quit = make(chan struct{})
	cf = newCustomFilterFiles(
		customBlocksPerFile, customHeadersPerFile, testReOrgSafeDepth,
		backend, quit, dataDir, &testParams, h2hCache,
		newBlockPrevOutFetcher(backend, &backendCfg),
	)
	waitForTargetHeight(t, &wg, cf, finalBlocks)

	// Filters are now sealed through height 599, which completes the
	// second header file [300, 599] - partially from rebuilt headers.
	checkCustomFilterFiles(t, dataDir, 599, 599)

	// Stop the service.
	close(quit)
	wg.Wait()

	// Finally, verify every configuration's filter header chain from
	// genesis across all sealed files, including the span that was
	// rebuilt after the restart.
	for _, cfg := range customFilterConfigs {
		verifyCustomFilterChain(t, dataDir, cfg.name, 599)
	}
}

// checkCustomFilterFiles asserts that the sealed custom filter files up to
// filterEnd and the sealed filter header files up to headerEnd exist for
// every configuration, that the fixed-size header files have the expected
// size and that no header file exists beyond headerEnd.
func checkCustomFilterFiles(t *testing.T, dataDir string, filterEnd,
	headerEnd int32) {

	t.Helper()

	for _, cfg := range customFilterConfigs {
		filterDir := filepath.Join(dataDir, customFilterDir(cfg.name))
		headerDir := filepath.Join(dataDir, customHeaderDir(cfg.name))

		for fileStart := int32(0); fileStart < filterEnd; fileStart += customBlocksPerFile {
			filterFile := fmt.Sprintf(
				FilterFileNamePattern, filterDir, fileStart,
				fileStart+customBlocksPerFile-1,
			)
			info, err := os.Stat(filterFile)
			require.NoError(t, err)
			require.Positive(t, info.Size())
		}

		for fileStart := int32(0); fileStart < headerEnd; fileStart += customHeadersPerFile {
			checkFile(t, fmt.Sprintf(
				FilterHeaderFileNamePattern, headerDir,
				fileStart, fileStart+customHeadersPerFile-1,
			), int64(customHeadersPerFile)*chainhash.HashSize)
		}

		require.NoFileExists(t, fmt.Sprintf(
			FilterHeaderFileNamePattern, headerDir, headerEnd+1,
			headerEnd+customHeadersPerFile,
		))
	}
}

// verifyCustomFilterChain re-computes the BIP-0157 filter header chain of
// the given configuration from the sealed filter files on disk and asserts
// it matches the sealed filter header files, starting at the genesis
// all-zero header up to the given end height (which must be both a filter
// and a header file boundary).
func verifyCustomFilterChain(t *testing.T, dataDir, name string,
	endHeight int32) {

	t.Helper()

	// First re-compute the expected header of every height from the
	// filter files alone.
	var prevHeader chainhash.Hash
	expected := make([]chainhash.Hash, 0, endHeight+1)
	filterDir := filepath.Join(dataDir, customFilterDir(name))
	for fileStart := int32(0); fileStart < endHeight; fileStart += customBlocksPerFile {
		filterFile, err := os.Open(fmt.Sprintf(
			FilterFileNamePattern, filterDir, fileStart,
			fileStart+customBlocksPerFile-1,
		))
		require.NoError(t, err)

		for range customBlocksPerFile {
			filterBytes, err := wire.ReadVarBytes(
				filterFile, 0, wire.MaxMessagePayload,
				"filter",
			)
			require.NoError(t, err)

			// header = double-SHA256(filter_hash || prev_header).
			filterHash := chainhash.DoubleHashB(filterBytes)
			prevHeader = chainhash.DoubleHashH(
				append(filterHash, prevHeader[:]...),
			)
			expected = append(expected, prevHeader)
		}

		require.NoError(t, filterFile.Close())
	}

	// Then compare them against the contents of the sealed header files.
	headerDir := filepath.Join(dataDir, customHeaderDir(name))
	for fileStart := int32(0); fileStart < endHeight; fileStart += customHeadersPerFile {
		headerBytes, err := os.ReadFile(fmt.Sprintf(
			FilterHeaderFileNamePattern, headerDir, fileStart,
			fileStart+customHeadersPerFile-1,
		))
		require.NoError(t, err)

		for j := range int32(customHeadersPerFile) {
			offset := j * chainhash.HashSize
			require.Equalf(
				t, expected[fileStart+j][:],
				headerBytes[offset:offset+chainhash.HashSize],
				"config %s, height %d", name, fileStart+j,
			)
		}
	}
}
