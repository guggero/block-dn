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
	"github.com/btcsuite/btcd/txscript/v2"
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

	// A 16-byte OP_RETURN payload with distinct bytes, so every 4-byte
	// chunk produces a distinct filter element.
	testOpReturnLongData = []byte{
		0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
		0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	}
	testScriptOpReturnLong = append(
		[]byte{0x6a, 0x10}, testOpReturnLongData...,
	)

	// An OP_RETURN script with two data pushes; its data is the
	// concatenation of both pushes.
	testScriptOpReturnMultiPush = []byte{
		0x6a, 0x03, 0x08, 0x09, 0x0a, 0x02, 0x0b, 0x0c,
	}
)

// TestOpReturnData pins the OP_RETURN data extraction the OP_RETURN filter
// configurations are based on.
func TestOpReturnData(t *testing.T) {
	// A single data push yields its bytes.
	require.Equal(t, []byte{0x05, 0x05}, opReturnData(testScriptOpReturn))

	// Multiple data pushes are concatenated in script order.
	require.Equal(
		t, []byte{0x08, 0x09, 0x0a, 0x0b, 0x0c},
		opReturnData(testScriptOpReturnMultiPush),
	)

	// Non-OP_RETURN scripts, bare OP_RETURN without data, OP_RETURN
	// followed by non-push opcodes and unparseable pushes all yield no
	// data.
	require.Nil(t, opReturnData(testScriptP2TR))
	require.Nil(t, opReturnData(testScriptP2PKH))
	require.Nil(t, opReturnData(nil))
	require.Nil(t, opReturnData([]byte{0x6a}))
	require.Nil(t, opReturnData([]byte{0x6a, txscript.OP_DUP}))
	require.Nil(t, opReturnData([]byte{0x6a, 0x05, 0x01})) // truncated
}

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
// exactly its expected elements: for the type-restricted sets the scripts
// of the configured types (from both the block's outputs and its spent
// previous outputs), for the OP_RETURN set the complete 4-byte chunks of
// the block's OP_RETURN output data — with duplicates removed in both
// cases.
func TestBuildCustomFilters(t *testing.T) {
	blockHash := chainhash.DoubleHashH([]byte("custom filter test block"))
	key := builder.DeriveKey(&blockHash)

	// One transaction creates a p2wpkh (twice, to check de-duplication),
	// a p2pkh and three OP_RETURN outputs (one of them twice, again for
	// de-duplication); its inputs spend a p2wsh and a p2pkh output. A
	// p2tr script is only visible on the input side.
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
			{PkScript: testScriptOpReturn},
			{PkScript: testScriptOpReturnLong},
			{PkScript: testScriptOpReturnMultiPush},
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

	shortData := []byte{0x05, 0x05}
	multiPushData := []byte{0x08, 0x09, 0x0a, 0x0b, 0x0c}

	// expectedElements maps the configuration name to the elements its
	// filter must (and must not) contain: scripts for the type sets, and
	// for the OP_RETURN set the complete 4-byte data chunks — the 2-byte
	// data and the trailing byte of the 5-byte multi-push data contribute
	// nothing.
	expectedElements := map[string][][]byte{
		"segwit": {
			testScriptP2WPKH, testScriptP2WSH, testScriptP2TR,
		},
		"p2wpkh": {testScriptP2WPKH},
		"p2wsh":  {testScriptP2WSH},
		"p2tr":   {testScriptP2TR},
		"opreturn-4x16": {
			testOpReturnLongData[0:4], testOpReturnLongData[4:8],
			testOpReturnLongData[8:12], testOpReturnLongData[12:16],
			multiPushData[:4],
		},
	}

	// The probe universe: everything any filter may contain, plus
	// negatives (the p2pkh script, whole OP_RETURN scripts, data too
	// short to form a chunk and unaligned or wrongly-sized windows).
	probes := [][]byte{
		testScriptP2WPKH, testScriptP2WSH, testScriptP2TR,
		testScriptP2PKH, testScriptOpReturn, testScriptOpReturnLong,
		testScriptOpReturnMultiPush,
		shortData, multiPushData, multiPushData[:4],
		testOpReturnLongData, testOpReturnLongData[0:4],
		testOpReturnLongData[4:8], testOpReturnLongData[8:12],
		testOpReturnLongData[12:16], testOpReturnLongData[:8],
		testOpReturnLongData[2:6],
	}

	for i, cfg := range customFilterConfigs {
		expected := expectedElements[cfg.name]

		// The duplicate p2wpkh and OP_RETURN outputs must only count
		// once, so the number of filter elements is the number of
		// expected elements.
		require.EqualValues(
			t, len(expected), filters[i].N(), cfg.name,
		)

		for _, probe := range probes {
			match, err := filters[i].Match(key, probe)
			require.NoError(t, err)

			shouldMatch := false
			for _, exp := range expected {
				if bytes.Equal(exp, probe) {
					shouldMatch = true
				}
			}
			require.Equalf(
				t, shouldMatch, match, "config %s, probe %x",
				cfg.name, probe,
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
	// control output that must not appear in any custom filter, plus an
	// OP_RETURN output whose data must appear in the OP_RETURN sets (but
	// which is excluded from the basic filter, so it stays out of
	// spendScripts).
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
	miner.SendOutput(&wire.TxOut{
		Value:    0,
		PkScript: testScriptOpReturnLong,
	}, 2)

	minedBlocks := miner.MineBlocksAndAssertNumTxes(
		1, len(spendScripts)+1,
	)
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

		// Type sets must match exactly the scripts of their types;
		// OP_RETURN sets (types == 0) must match no script at all.
		assertMatches(t, filter, cfg.types)

		if !cfg.opReturn {
			continue
		}

		// The OP_RETURN set must contain every complete 4-byte chunk
		// of the block's OP_RETURN output data. No exact element
		// count is asserted, since the coinbase may contribute
		// OP_RETURN outputs of its own (e.g. the segwit witness
		// commitment).
		for c := 0; c+4 <= len(testOpReturnLongData); c += 4 {
			chunk := testOpReturnLongData[c : c+4]
			match, err := filter.Match(key, chunk)
			require.NoError(t, err)
			require.Truef(t, match, "config %s, chunk %x", cfg.name,
				chunk)
		}

		// A chunk that never appeared must not match.
		absent := bytes.Repeat([]byte{0xff}, 4)
		match, err := filter.Match(key, absent)
		require.NoError(t, err)
		require.Falsef(t, match, "config %s, data %x", cfg.name,
			absent)
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
