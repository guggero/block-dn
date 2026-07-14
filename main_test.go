package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/rpcclient"
	sp "github.com/btcsuite/btcd/silentpayments"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btcwallet/chain"
	lntestminer "github.com/lightningnetwork/lnd/lntest/miner"
	"github.com/lightningnetwork/lnd/lntest/port"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

const (
	numStartupBlocks = 50

	// numInitialBlocks is the number of blocks we mine for testing. In
	// regtest mode, when using btcd as a miner, we can only mine blocks in
	// a 2-hour window and each block needs to have a timestamp at least 1
	// second greater than the previous block. Thus, we can only mine at
	// most 7200 blocks in a short period of time with the first 200 being
	// mined when the miner is created as part of its startup procedure.
	numInitialBlocks = 3000

	headerSize        = 80
	filterHeadersSize = 32

	cacheTemporary = "public, max-age=1"
	cacheMemory    = "public, max-age=60"
	cacheDisk      = "public, max-age=31536000, " +
		"stale-while-revalidate=86400"

	unitTestDir = ".unit-test-logs"
)

var (
	syncTimeout  = 2 * time.Minute
	testTimeout  = 60 * time.Second
	shortTimeout = 5 * time.Second

	testParams = chaincfg.RegressionNetParams

	totalStartupBlocks = numStartupBlocks +
		uint32(testParams.CoinbaseMaturity) +
		testParams.MinerConfirmationWindow*2
	totalInitialBlocks = totalStartupBlocks + numInitialBlocks
)

type testContext struct {
	miner   *lntestminer.HarnessMiner
	backend *rpcclient.Client
	server  *server
}

func (ctx *testContext) fetchJSON(t *testing.T, endpoint string,
	target any) http.Header {

	url := fmt.Sprintf("http://%s/%s", ctx.server.listenAddr, endpoint)
	resp, err := http.Get(url)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	body := new(bytes.Buffer)
	_, err = body.ReadFrom(resp.Body)
	require.NoError(t, err)
	err = json.Unmarshal(body.Bytes(), target)
	if err != nil {
		require.NoError(t, err)
	}

	return resp.Header
}

func (ctx *testContext) fetchBinary(t *testing.T, endpoint string) ([]byte,
	http.Header) {

	data, header, _ := ctx.fetchBinaryWithStatus(t, endpoint)
	return data, header
}

func (ctx *testContext) fetchBinaryWithStatus(t *testing.T,
	endpoint string) ([]byte, http.Header, int) {

	url := fmt.Sprintf("http://%s/%s", ctx.server.listenAddr, endpoint)
	resp, err := http.Get(url)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	body := new(bytes.Buffer)
	_, err = body.ReadFrom(resp.Body)
	require.NoError(t, err)

	return body.Bytes(), resp.Header, resp.StatusCode
}

func (ctx *testContext) bestBlock(t *testing.T) (int32, chainhash.Hash) {
	height, err := ctx.backend.GetBlockCount()
	require.NoError(t, err)

	blockHash, err := ctx.backend.GetBlockHash(height)
	require.NoError(t, err)

	return int32(height), *blockHash
}

func (ctx *testContext) blockAtHeight(t *testing.T,
	height int32) *wire.MsgBlock {

	hash, err := ctx.backend.GetBlockHash(int64(height))
	require.NoError(t, err)

	block, err := ctx.backend.GetBlock(hash)
	require.NoError(t, err)

	return block
}

func (ctx *testContext) fetchPrevOutScript(op wire.OutPoint) ([]byte, error) {
	tx, err := ctx.backend.GetRawTransaction(&op.Hash)
	if err != nil {
		return nil, fmt.Errorf("error fetching previous "+
			"transaction: %w", err)
	}

	if int(op.Index) >= len(tx.MsgTx().TxOut) {
		return nil, fmt.Errorf("output index %d out of range for "+
			"transaction %s", op.Index, op.Hash.String())
	}

	return tx.MsgTx().TxOut[op.Index].PkScript, nil
}

func (ctx *testContext) waitBackendSync(t *testing.T) {
	waitBackendSync(t, ctx.backend, ctx.miner)
}

func (ctx *testContext) waitFilesSync(t *testing.T) {
	err := wait.NoError(func() error {
		headerHeight := ctx.server.headerFiles.getCurrentHeight()
		_, minerHeight := ctx.miner.GetBestBlock()
		if minerHeight != headerHeight {
			return fmt.Errorf("expected height %d, got %d",
				minerHeight, headerHeight)
		}

		if headerHeight != ctx.server.cFilterFiles.getCurrentHeight() {
			return fmt.Errorf("cfilter height mismatch: %d vs %d",
				ctx.server.cFilterFiles.getCurrentHeight(),
				headerHeight)
		}

		if headerHeight != ctx.server.spTweakFiles.getCurrentHeight() {
			return fmt.Errorf("sp tweak data height mismatch: %d "+
				"vs %d",
				ctx.server.spTweakFiles.getCurrentHeight(),
				headerHeight)
		}

		return nil
	}, syncTimeout)
	require.NoError(t, err)
}

type testFunc func(t *testing.T, ctx *testContext)

var testCases = []struct {
	name string
	fn   testFunc
}{
	{
		name: "errors",
		fn:   testErrors,
	},
	{
		name: "index",
		fn:   testIndex,
	},
	{
		name: "status",
		fn:   testStatus,
	},
	{
		name: "headers",
		fn:   testHeaders,
	},
	{
		name: "headers-import",
		fn:   testHeadersImport,
	},
	{
		name: "headers-import-latest",
		fn:   testHeadersImportLatest,
	},
	{
		name: "filter-headers",
		fn:   testFilterHeaders,
	},
	{
		name: "filter-headers-import",
		fn:   testFilterHeadersImport,
	},
	{
		name: "filter-headers-import-latest",
		fn:   testFilterHeadersImportLatest,
	},
	{
		name: "filter-single",
		fn:   testFilterSingle,
	},
	{
		name: "range-requests",
		fn:   testRangeRequests,
	},
	{
		name: "sp-tweak-data",
		fn:   testSPTweakData,
	},
	{
		name: "block",
		fn:   testBlock,
	},
	{
		name: "block-spenttxouts",
		fn:   testBlockSpentTxOuts,
	},
	{
		name: "tx-out-proof",
		fn:   testTxOutProof,
	},
	{
		name: "tx-raw",
		fn:   testTxRaw,
	},
	{
		name: "fees-estimate",
		fn:   testFeeEstimate,
	},
}

func testErrors(t *testing.T, ctx *testContext) {
	type errorResponse struct {
		status int
		error  string
	}

	var (
		badHash  = strings.Repeat("k", 64)
		badInt64 = strings.Repeat("9", 20)

		// badStartHeight is too high for the start-height endpoints
		// (checkStartHeight rejects > currentHeight).
		badStartHeight = strconv.Itoa(int(totalInitialBlocks + 1))

		// badEndHeight is too high for the end-height (import)
		// endpoints. checkEndHeight allows endHeight up to
		// currentHeight + 1 (inclusive of the tip block since
		// endHeight is exclusive), so we go one higher.
		badEndHeight  = strconv.Itoa(int(totalInitialBlocks + 2))
		respBadHeight = errorResponse{
			status: 400,
			error:  "invalid value for parameter height",
		}
		respBadStartHeight1 = errorResponse{
			status: 400,
			error: fmt.Sprintf("invalid start height %d, must be "+
				"zero or a multiple of %d", 1,
				DefaultRegtestHeadersPerFile),
		}
		respBadStartHeightLarge = errorResponse{
			status: 400,
			error: fmt.Sprintf("start height %s is greater than "+
				"current height %d", badStartHeight,
				totalInitialBlocks),
		}
		respBadEndHeight0 = errorResponse{
			status: 400,
			error: fmt.Sprintf("invalid end height %d, must be "+
				"a multiple of %d", 0,
				DefaultRegtestHeadersPerFile),
		}
		respBadEndHeightPartial = errorResponse{
			status: 400,
			error: fmt.Sprintf("invalid end height %d, must be "+
				"a multiple of %d", 1000,
				DefaultRegtestHeadersPerFile),
		}
		respBadEndHeightLarge = errorResponse{
			status: 400,
			error: fmt.Sprintf("end height %s is greater than "+
				"current height %d", badEndHeight,
				totalInitialBlocks),
		}
		respBadHashLength = errorResponse{
			status: 400,
			error:  errInvalidHashLength.Error(),
		}
		respNotFound = errorResponse{
			status: 404,
			error:  "404 page not found",
		}
	)
	errorCases := map[string]errorResponse{
		"foo":                                   respNotFound,
		"headers":                               respNotFound,
		"headers/" + badInt64:                   respBadHeight,
		"headers/1":                             respBadStartHeight1,
		"headers/" + badStartHeight:             respBadStartHeightLarge,
		"headers/import":                        respNotFound,
		"headers/import/" + badInt64:            respBadHeight,
		"headers/import/0":                      respBadEndHeight0,
		"headers/import/1000":                   respBadEndHeightPartial,
		"headers/import/" + badEndHeight:        respBadEndHeightLarge,
		"filter-headers":                        respNotFound,
		"filter-headers/" + badInt64:            respBadHeight,
		"filter-headers/1":                      respBadStartHeight1,
		"filter-headers/" + badStartHeight:      respBadStartHeightLarge,
		"filter-headers/import":                 respNotFound,
		"filter-headers/import/" + badInt64:     respBadHeight,
		"filter-headers/import/0":               respBadEndHeight0,
		"filter-headers/import/1000":            respBadEndHeightPartial,
		"filter-headers/import/" + badEndHeight: respBadEndHeightLarge,
		"filters":                               respNotFound,
		"filters/" + badInt64:                   respBadHeight,
		"filters/1":                             respBadStartHeight1,
		"filters/" + badStartHeight:             respBadStartHeightLarge,
		"block":                                 respNotFound,
		"block/aaaa":                            respBadHashLength,
		"block/" + badHash:                      respNotFound,
		"tx/out-proof":                          respNotFound,
		"tx/out-proof/aaaa":                     respBadHashLength,
		"tx/out-proof/" + badHash:               respNotFound,
		"tx/raw":                                respNotFound,
		"tx/raw/aaaa":                           respBadHashLength,
		"tx/raw/" + badHash:                     respNotFound,
	}
	for endpoint, expected := range errorCases {
		body, headers, status := ctx.fetchBinaryWithStatus(t, endpoint)
		require.Equalf(
			t, expected.status, status, "endpoint: %s", endpoint,
		)

		require.Containsf(
			t, string(body), expected.error, "endpoint: %s",
			endpoint,
		)

		// If the endpoint isn't found, there are no cache or CORS
		// headers.
		if expected.status == http.StatusNotFound {
			continue
		}

		require.Equalf(
			t, "*", headers.Get(HeaderCORS), "endpoint: %s",
			endpoint,
		)
		require.Equalf(
			t, cacheMemory, headers.Get(HeaderCache),
			"endpoint: %s", endpoint,
		)
	}
}

func testIndex(t *testing.T, ctx *testContext) {
	data, headers := ctx.fetchBinary(t, "")
	require.Contains(
		t, string(data), "<title>Block Delivery Network</title>",
	)
	require.Equal(t, "*", headers.Get(HeaderCORS))
}

func testFeeEstimate(t *testing.T, ctx *testContext) {
	var feeRate FeeRate
	headers := ctx.fetchJSON(t, "fees/estimate/1", &feeRate)
	t.Logf("Got fee rate: %+v", feeRate)
	require.InDelta(t, 1723, feeRate.FeeSatPerKVByte, 2)
	require.InDelta(t, 430, feeRate.FeeSatPerKWeight, 2)
	require.EqualValues(t, 1, feeRate.FeeSatPerVByte)
	require.Equal(t, "*", headers.Get(HeaderCORS))
}

func testStatus(t *testing.T, ctx *testContext) {
	var status Status
	headers := ctx.fetchJSON(t, "status", &status)

	require.Equal(t, int32(totalInitialBlocks), status.BestBlockHeight)
	require.Equal(t, testParams.Name, status.ChainName)
	require.Equal(
		t, testParams.GenesisHash.String(), status.ChainGenesisHash,
	)
	require.Equal(
		t, ctx.server.headersPerFile, status.EntriesPerHeaderFile,
	)
	require.Equal(
		t, ctx.server.filtersPerFile, status.EntriesPerFilterFile,
	)
	require.Equal(
		t, ctx.server.spTweaksPerFile, status.EntriesPerSPTweakFile,
	)
	require.True(t, status.AllFilesSynced)

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	height, blockHash := ctx.bestBlock(t)
	require.Equal(t, height, status.BestBlockHeight)
	require.Equal(t, height, status.BestFilterHeight)
	require.Equal(t, height, status.BestSPTweakHeight)
	require.Equal(t, blockHash.String(), status.BestBlockHash)

	filterHeader, ok := ctx.server.headerFiles.filterHeaderAtHeight(height)
	require.True(t, ok)
	require.Equal(t, filterHeader.String(), status.BestFilterHeader)
}

func testHeaders(t *testing.T, ctx *testContext) {
	// We first query for a start block that can be served from files only.
	body, headers := ctx.fetchBinary(t, "headers/0")
	targetLen := DefaultRegtestHeadersPerFile * headerSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// And now we try to fetch all headers up to the current height, which
	// will require some of them to be served from memory.
	const startHeight = DefaultRegtestHeadersPerFile
	body, headers = ctx.fetchBinary(
		t, fmt.Sprintf("headers/%d", startHeight),
	)
	expectedBlocks := totalInitialBlocks - startHeight + 1
	targetLen = int(expectedBlocks) * headerSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// We make sure that the last 10 entries are actually correct.
	for index := expectedBlocks - 9; index <= expectedBlocks-1; index++ {
		start := int(index) * headerSize
		end := int(index+1) * headerSize
		headerBytes := body[start:end]

		blockHash, err := ctx.backend.GetBlockHash(
			startHeight + int64(index),
		)
		require.NoError(t, err)

		block, err := ctx.backend.GetBlock(blockHash)
		require.NoError(t, err)

		var headerBuf bytes.Buffer
		err = block.Header.Serialize(&headerBuf)
		require.NoError(t, err)

		require.Equalf(
			t, headerBuf.Bytes(), headerBytes,
			"header at height %d does not match", index,
		)
	}
}

func testHeadersImport(t *testing.T, ctx *testContext) {
	// We first query for a block height that can be served from files only.
	body, headers := ctx.fetchBinary(
		t, fmt.Sprintf("headers/import/%d",
			DefaultRegtestHeadersPerFile),
	)
	targetLen := importMetadataSize +
		DefaultRegtestHeadersPerFile*headerSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// And now we try to fetch all headers up to the current height, which
	// will require some of them to be served from memory. endHeight is
	// exclusive on both paths, so /headers/import/N yields N headers
	// (heights 0..N-1).
	body, headers = ctx.fetchBinary(
		t, fmt.Sprintf("headers/import/%d", totalInitialBlocks),
	)
	targetLen = importMetadataSize + int(totalInitialBlocks)*headerSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// We make sure the last 10 entries in the response (heights
	// lastHeight-10..lastHeight-1, since endHeight is exclusive) are
	// correct.
	lastHeight := ctx.server.headerFiles.getCurrentHeight()
	require.Equal(t, int32(totalInitialBlocks), lastHeight)
	for height := lastHeight - 10; height < lastHeight; height++ {
		start := importMetadataSize + int(height)*headerSize
		end := importMetadataSize + int(height+1)*headerSize
		headerBytes := body[start:end]

		blockHash, err := ctx.backend.GetBlockHash(int64(height))
		require.NoError(t, err)

		block, err := ctx.backend.GetBlock(blockHash)
		require.NoError(t, err)

		var headerBuf bytes.Buffer
		err = block.Header.Serialize(&headerBuf)
		require.NoError(t, err)

		require.Equalf(
			t, headerBuf.Bytes(), headerBytes,
			"header at height %d does not match", height,
		)
	}
}

func testFilterHeaders(t *testing.T, ctx *testContext) {
	// We first query for a start block that can be served from files only.
	body, headers := ctx.fetchBinary(t, "filter-headers/0")
	targetLen := DefaultRegtestHeadersPerFile * filterHeadersSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// And now we try to fetch all headers up to the current height, which
	// will require some of them to be served from memory.
	const startHeight = DefaultRegtestHeadersPerFile
	body, headers = ctx.fetchBinary(
		t, fmt.Sprintf("filter-headers/%d", startHeight),
	)
	expectedBlocks := totalInitialBlocks - startHeight + 1
	targetLen = int(expectedBlocks) * filterHeadersSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// We make sure that the last 10 entries are actually correct.
	for index := expectedBlocks - 9; index <= expectedBlocks-1; index++ {
		start := int(index) * filterHeadersSize
		end := int(index+1) * filterHeadersSize
		headerBytes := body[start:end]

		blockHash, err := ctx.backend.GetBlockHash(
			startHeight + int64(index),
		)
		require.NoError(t, err)

		filter, err := ctx.backend.GetBlockFilter(
			*blockHash, &filterBasic,
		)
		require.NoError(t, err)

		filterHeaderHash, err := chainhash.NewHashFromStr(filter.Header)
		require.NoError(t, err)

		require.Equalf(
			t, filterHeaderHash[:], headerBytes,
			"filter header at height %d does not match", index,
		)
	}
}

func testFilterHeadersImport(t *testing.T, ctx *testContext) {
	// We first query for a block height that can be served from files only.
	body, headers := ctx.fetchBinary(
		t, fmt.Sprintf("filter-headers/import/%d",
			DefaultRegtestHeadersPerFile),
	)
	targetLen := importMetadataSize +
		DefaultRegtestHeadersPerFile*filterHeadersSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// And now we try to fetch all headers up to the current height, which
	// will require some of them to be served from memory. endHeight is
	// exclusive on both paths, so /filter-headers/import/N yields N
	// entries (heights 0..N-1).
	body, headers = ctx.fetchBinary(
		t, fmt.Sprintf("filter-headers/import/%d", totalInitialBlocks),
	)
	targetLen = importMetadataSize +
		int(totalInitialBlocks)*filterHeadersSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// Verify the last 10 entries (heights lastHeight-10..lastHeight-1
	// since endHeight is exclusive) match what bitcoind returns.
	lastHeight := ctx.server.headerFiles.getCurrentHeight()
	require.Equal(t, int32(totalInitialBlocks), lastHeight)
	for height := lastHeight - 10; height < lastHeight; height++ {
		start := importMetadataSize + int(height)*filterHeadersSize
		end := importMetadataSize + int(height+1)*filterHeadersSize
		headerBytes := body[start:end]

		blockHash, err := ctx.backend.GetBlockHash(int64(height))
		require.NoError(t, err)

		filter, err := ctx.backend.GetBlockFilter(
			*blockHash, &filterBasic,
		)
		require.NoError(t, err)

		filterHeaderHash, err := chainhash.NewHashFromStr(filter.Header)
		require.NoError(t, err)

		require.Equalf(
			t, filterHeaderHash[:], headerBytes,
			"filter header at height %d does not match", height,
		)
	}
}

// testHeadersImportLatest exercises the convenience shortcut
// /headers/import/latest, which auto-resolves endHeight to currentHeight+1.
// The response must be byte-equivalent to a request for the numeric URL
// /headers/import/<currentHeight+1> and must be served at the memory tier
// (the tip is mutable).
func testHeadersImportLatest(t *testing.T, ctx *testContext) {
	tipHeight := ctx.server.headerFiles.getCurrentHeight()
	require.Equal(t, int32(totalInitialBlocks), tipHeight)

	body, headers := ctx.fetchBinary(t, "headers/import/latest")

	// The response covers heights 0..tipHeight, which is tipHeight+1
	// entries.
	expectedHeaders := int(tipHeight) + 1
	targetLen := importMetadataSize + expectedHeaders*headerSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	// The tip is mutable, so the response must be memory-tier (a deeper
	// reorg could change the bytes), and never disk-tier with a long TTL.
	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// Byte-equivalence: hitting the numeric URL for the same end height
	// must yield an identical body. This guards against the two routes
	// diverging (e.g., one applying alignment checks while the other
	// doesn't).
	numericBody, _ := ctx.fetchBinary(t,
		fmt.Sprintf("headers/import/%d", tipHeight+1))
	require.Equal(t, numericBody, body, "latest body must be identical "+
		"to numeric body for the same end height")

	// Spot-check the last header in the body matches what bitcoind
	// reports for tipHeight (the last height included since endHeight is
	// exclusive at tipHeight+1).
	lastHeaderStart := importMetadataSize + int(tipHeight)*headerSize
	lastHeaderEnd := lastHeaderStart + headerSize
	headerBytes := body[lastHeaderStart:lastHeaderEnd]

	blockHash, err := ctx.backend.GetBlockHash(int64(tipHeight))
	require.NoError(t, err)

	block, err := ctx.backend.GetBlock(blockHash)
	require.NoError(t, err)

	var headerBuf bytes.Buffer
	require.NoError(t, block.Header.Serialize(&headerBuf))
	require.Equal(t, headerBuf.Bytes(), headerBytes,
		"last header in latest response must match bitcoind")
}

// testFilterHeadersImportLatest is the /filter-headers/import/latest
// counterpart to testHeadersImportLatest.
func testFilterHeadersImportLatest(t *testing.T, ctx *testContext) {
	tipHeight := ctx.server.headerFiles.getCurrentHeight()
	require.Equal(t, int32(totalInitialBlocks), tipHeight)

	body, headers := ctx.fetchBinary(t, "filter-headers/import/latest")

	expectedEntries := int(tipHeight) + 1
	targetLen := importMetadataSize + expectedEntries*filterHeadersSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	numericBody, _ := ctx.fetchBinary(t,
		fmt.Sprintf("filter-headers/import/%d", tipHeight+1))
	require.Equal(t, numericBody, body, "latest body must be identical "+
		"to numeric body for the same end height")

	// Spot-check the last filter header.
	lastStart := importMetadataSize + int(tipHeight)*filterHeadersSize
	lastEnd := lastStart + filterHeadersSize
	filterBytes := body[lastStart:lastEnd]

	blockHash, err := ctx.backend.GetBlockHash(int64(tipHeight))
	require.NoError(t, err)
	filter, err := ctx.backend.GetBlockFilter(*blockHash, &filterBasic)
	require.NoError(t, err)
	filterHeaderHash, err := chainhash.NewHashFromStr(filter.Header)
	require.NoError(t, err)
	require.Equal(t, filterHeaderHash[:], filterBytes,
		"last filter header in latest response must match bitcoind")
}

func testSPTweakData(t *testing.T, ctx *testContext) {
	var spTweakData SPTweakFile
	headers := ctx.fetchJSON(t, "sp/tweak-data/0", &spTweakData)
	require.Equal(t, int32(0), spTweakData.StartHeight)
	require.Equal(
		t, int32(DefaultRegtestSPTweaksPerFile), spTweakData.NumBlocks,
	)
	require.Len(t, spTweakData.Blocks, int(spTweakData.NumBlocks))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// And now we try to fetch all SP tweak data up to the current height,
	// which will require some of them to be served from memory.
	const startHeight = DefaultRegtestSPTweaksPerFile
	headers = ctx.fetchJSON(
		t, fmt.Sprintf("sp/tweak-data/%d", startHeight), &spTweakData,
	)
	expectedBlocks := totalInitialBlocks - startHeight + 1
	require.Equal(
		t, int32(expectedBlocks), spTweakData.NumBlocks,
	)
	require.Len(t, spTweakData.Blocks, int(expectedBlocks))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// Now we mine some blocks with Taproot outputs to ensure we have
	// Taproot tweaks in the SP tweak data.
	//
	// We use a comparatively large output value (100_000 sat instead
	// of e.g. 5_000) on purpose. The miner's memWallet selects coins
	// by iterating its UTXO map (Go map order is randomized) and stops
	// at the first running total ≥ amount+fee — so the change is
	// bounded above by the value of the last-added UTXO. By this point
	// in TestBlockDN the regtest subsidy has halved 22 times (chain is
	// past 22 × SubsidyReductionInterval=150), so the wallet's UTXO
	// set is a mix of huge initial coinbases and many tiny post-halving
	// coinbases (down to 1192 sat). If the last-added UTXO is a tiny
	// one and the change happens to fall below the dust threshold, the
	// broadcast fails with "payment is dust". Bumping the output value
	// forces the wallet to either pick at least one large coinbase
	// (yielding a huge change) or accumulate ~85 tiny ones (vanishingly
	// unlikely under Go's map randomization), so dust change becomes
	// effectively impossible. We deliberately don't use
	// SendOutputsWithoutChange, which would roll the surplus into the
	// fee and skew the fee estimator that testFeeEstimate samples
	// after this loop.
	numTrBlocks := uint32(20)
	for range numTrBlocks {
		ctx.miner.SendOutput(&wire.TxOut{
			Value:    100_000,
			PkScript: psbt.SilentPaymentDummyP2TROutput,
		}, 2)
		ctx.miner.SendOutput(&wire.TxOut{
			Value:    100_000,
			PkScript: psbt.SilentPaymentDummyP2TROutput,
		}, 2)
		ctx.miner.MineBlocksAndAssertNumTxes(1, 2)
	}

	ctx.waitBackendSync(t)
	ctx.waitFilesSync(t)

	headers = ctx.fetchJSON(
		t, fmt.Sprintf("sp/tweak-data/%d", startHeight), &spTweakData,
	)
	expectedHeight := totalInitialBlocks + numTrBlocks
	expectedBlocks = expectedHeight - DefaultRegtestSPTweaksPerFile + 1
	require.Equal(
		t, int32(expectedBlocks), spTweakData.NumBlocks,
	)
	require.Len(t, spTweakData.Blocks, int(expectedBlocks))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// We expect the last block before the Taproot blocks to not have any
	// Taproot tweaks.
	noTrHeight := expectedHeight - numTrBlocks
	noTrBlock, err := spTweakData.TweakAtHeight(int32(noTrHeight))
	require.NoError(t, err)
	require.Empty(t, noTrBlock)

	// Check the actual tweaks in the last 20 blocks.
	loopStart := expectedHeight - numTrBlocks + 1
	for height := loopStart; height <= expectedHeight; height++ {
		trBlock, err := spTweakData.TweakAtHeight(int32(height))
		require.NoError(t, err)

		require.Len(t, trBlock, 2)
		require.Lenf(
			t, trBlock[1],
			hex.EncodedLen(btcec.PubKeyBytesLenCompressed),
			"block %d, index 1", height,
		)
		require.Lenf(
			t, trBlock[2],
			hex.EncodedLen(btcec.PubKeyBytesLenCompressed),
			"block %d, index 2", height,
		)

		block := ctx.blockAtHeight(t, int32(height))
		require.Len(t, block.Transactions, 3)

		key1, err := sp.TransactionTweakData(
			block.Transactions[1], ctx.fetchPrevOutScript, log,
		)
		require.NoError(t, err)
		require.Equal(
			t, trBlock[1],
			hex.EncodeToString(key1.SerializeCompressed()),
		)

		key2, err := sp.TransactionTweakData(
			block.Transactions[2], ctx.fetchPrevOutScript, log,
		)
		require.NoError(t, err)
		require.Equal(
			t, trBlock[2],
			hex.EncodeToString(key2.SerializeCompressed()),
		)
	}

	// We mine an empty block to ensure that the following tests can assume
	// empty blocks again.
	ctx.miner.MineEmptyBlocks(1)
	ctx.waitBackendSync(t)
	ctx.waitFilesSync(t)
}

func testBlock(t *testing.T, ctx *testContext) {
	// We start with the latest block.
	_, bestHash := ctx.bestBlock(t)
	block, err := ctx.backend.GetBlock(&bestHash)
	require.NoError(t, err)

	require.Len(t, block.Transactions, 1)

	var rawBlock bytes.Buffer
	require.NoError(t, block.Serialize(&rawBlock))

	data, headers := ctx.fetchBinary(
		t, fmt.Sprintf("block/%s", block.BlockHash().String()),
	)
	require.NotEmpty(t, data)
	require.Equal(t, rawBlock.Bytes(), data)

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))
}

func testBlockSpentTxOuts(t *testing.T, ctx *testContext) {
	// We start with the latest block.
	_, bestHash := ctx.bestBlock(t)
	block, err := ctx.backend.GetBlock(&bestHash)
	require.NoError(t, err)

	require.Len(t, block.Transactions, 1)

	data, headers := ctx.fetchBinary(
		t, fmt.Sprintf("block/%s/spenttxouts",
			block.BlockHash().String()),
	)
	require.NotEmpty(t, data)

	expected := "[[]]\n"
	require.Equal(t, expected, string(data))

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))
}

func testTxOutProof(t *testing.T, ctx *testContext) {
	// We start with the latest block.
	bestHeight, bestHash := ctx.bestBlock(t)
	block, err := ctx.backend.GetBlock(&bestHash)
	require.NoError(t, err)

	require.Len(t, block.Transactions, 1)
	tx := block.Transactions[0]

	data, headers := ctx.fetchBinary(
		t, fmt.Sprintf("tx/out-proof/%s", tx.TxHash().String()),
	)
	require.NotEmpty(t, data)

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheTemporary, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))

	// Then we verify that a sufficiently confirmed block has cache headers.
	buriedHash, err := ctx.backend.GetBlockHash(
		int64(bestHeight) - defaultTestnetReOrgSafeDepth - 1,
	)
	require.NoError(t, err)

	buriedBlock, err := ctx.backend.GetBlock(buriedHash)
	require.NoError(t, err)

	require.Len(t, buriedBlock.Transactions, 1)
	buriedTx := buriedBlock.Transactions[0]

	data, headers = ctx.fetchBinary(
		t, fmt.Sprintf("tx/out-proof/%s", buriedTx.TxHash().String()),
	)
	require.NotEmpty(t, data)

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))
}

func testTxRaw(t *testing.T, ctx *testContext) {
	_, bestHash := ctx.bestBlock(t)
	block, err := ctx.backend.GetBlock(&bestHash)
	require.NoError(t, err)

	require.Len(t, block.Transactions, 1)
	tx := block.Transactions[0]

	data, headers := ctx.fetchBinary(
		t, fmt.Sprintf("tx/raw/%s", tx.TxHash().String()),
	)
	require.NotEmpty(t, data)

	var txBuf bytes.Buffer
	require.NoError(t, tx.Serialize(&txBuf))

	require.Equal(t, txBuf.Bytes(), data)

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))
}

func TestBlockDN(t *testing.T) {
	// Activate Taproot for regtest.
	TaprootActivationHeights[chaincfg.RegressionNetParams.Net] = 1

	miner, backend, backendCfg, _ := setupBackend(t, unitTestDir)

	dataDir := t.TempDir()
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port.NextAvailablePort())

	testServer := newServer(
		false, true, dataDir, listenAddr, &backendCfg,
		unittest.NetParams, 6, DefaultRegtestHeadersPerFile,
		DefaultRegtestFiltersPerFile, DefaultRegtestSPTweaksPerFile,
		DefaultPrevOutCacheMiBytes, defaultReadTimeout,
		defaultWriteTimeout,
	)
	ctx := &testContext{
		miner:   miner,
		backend: backend,
		server:  testServer,
	}

	// Mine a couple blocks and wait for the backend to catch up.
	t.Logf("Mining %d blocks...", numInitialBlocks)
	_ = miner.MineEmptyBlocks(numInitialBlocks)

	// Wait until the backend is fully synced to the miner.
	ctx.waitBackendSync(t)

	t.Logf("Starting block-dn server at %s...", listenAddr)
	require.NoError(t, testServer.start())
	ctx.waitFilesSync(t)

	for _, testCase := range testCases {
		success := t.Run(testCase.name, func(t *testing.T) {
			testCase.fn(t, ctx)
		})
		if !success {
			t.Fatalf("test case %s failed", testCase.name)
		}
	}
}

func newBitcoind(t *testing.T, logdir string,
	extraArgs []string) (*rpcclient.Client, rpcclient.ConnConfig,
	*chain.BitcoindConfig, func() error) {

	tempBitcoindDir := t.TempDir()

	err := os.MkdirAll(logdir, 0700)
	require.NoError(t, err)

	logFile, err := filepath.Abs(logdir + "/bitcoind.log")
	require.NoError(t, err)

	zmqBlockAddr := fmt.Sprintf("tcp://127.0.0.1:%d",
		port.NextAvailablePort())
	zmqTxAddr := fmt.Sprintf("tcp://127.0.0.1:%d", port.NextAvailablePort())
	rpcPort := port.NextAvailablePort()
	p2pPort := port.NextAvailablePort()
	torBindPort := port.NextAvailablePort()

	cmdArgs := []string{
		"-datadir=" + tempBitcoindDir,
		"-whitelist=127.0.0.1", // whitelist localhost to speed up relay
		"-rpcauth=weks:469e9bb14ab2360f8e226efed5ca6f" +
			"d$507c670e800a95284294edb5773b05544b" +
			"220110063096c221be9933c82d38e1",
		fmt.Sprintf("-rpcport=%d", rpcPort),
		fmt.Sprintf("-port=%d", p2pPort),
		fmt.Sprintf("-bind=127.0.0.1:%d=onion", torBindPort),
		"-zmqpubrawblock=" + zmqBlockAddr,
		"-zmqpubrawtx=" + zmqTxAddr,
		"-debuglogfile=" + logFile,
	}
	cmdArgs = append(cmdArgs, extraArgs...)
	bitcoind := exec.Command("bitcoind", cmdArgs...)

	err = bitcoind.Start()
	if err != nil {
		err := os.RemoveAll(tempBitcoindDir)
		require.NoError(t, err)
	}

	cleanUp := func() error {
		_ = bitcoind.Process.Kill()
		_ = bitcoind.Wait()

		return nil
	}

	// Allow process to start.
	time.Sleep(1 * time.Second)

	rpcHost := fmt.Sprintf("127.0.0.1:%d", rpcPort)
	rpcUser := "weks"
	rpcPass := "weks"

	rpcCfg := rpcclient.ConnConfig{
		Host:                 rpcHost,
		User:                 rpcUser,
		Pass:                 rpcPass,
		DisableConnectOnNew:  true,
		DisableAutoReconnect: false,
		DisableTLS:           true,
		HTTPPostMode:         true,
	}

	bitcoindCfg := &chain.BitcoindConfig{
		ChainParams: &testParams,
		Host:        rpcHost,
		User:        rpcUser,
		Pass:        rpcPass,
		ZMQConfig: &chain.ZMQConfig{
			ZMQBlockHost:           zmqBlockAddr,
			ZMQTxHost:              zmqTxAddr,
			MempoolPollingInterval: pollInterval,
			RPCBatchInterval:       pollInterval,
			RPCBatchSize:           1,
			ZMQReadDeadline:        defaultReadTimeout,
		},
	}

	client, err := rpcclient.New(&rpcCfg, nil)
	if err != nil {
		_ = cleanUp()
		require.NoError(t, err)
	}

	return client, rpcCfg, bitcoindCfg, cleanUp
}

// nolint:unparam
func setupBackend(t *testing.T, testDir string) (*lntestminer.HarnessMiner,
	*rpcclient.Client, rpcclient.ConnConfig, *chain.BitcoindConfig) {

	ctx := context.Background()
	setupLogging(testDir, "debug")

	_ = os.RemoveAll(testDir)
	_ = os.MkdirAll(testDir, 0700)

	miner := lntestminer.NewTempMiner(
		ctx, t, filepath.Join(testDir, "temp-miner"), "miner.log",
	)
	require.NoError(t, miner.Start(true, numStartupBlocks))

	// Next mine enough blocks in order for segwit and the CSV package
	// soft-fork to activate on SimNet.
	numBlocks := testParams.MinerConfirmationWindow * 2
	miner.GenerateBlocks(numBlocks)

	t.Cleanup(miner.Stop)

	backend, backendCfg, bitcoindCfg, cleanup := newBitcoind(
		t, testDir, []string{
			"-rest",
			"-regtest",
			"-txindex",
			"-disablewallet",
			"-peerblockfilters=1",
			"-blockfilterindex=1",
		},
	)

	t.Cleanup(func() {
		require.NoError(t, cleanup())
	})

	err := wait.NoError(func() error {
		return backend.AddNode(miner.P2PAddress(), rpcclient.ANAdd)
	}, testTimeout)
	require.NoError(t, err)

	return miner, backend, backendCfg, bitcoindCfg
}

func waitBackendSync(t *testing.T, backend *rpcclient.Client,
	miner *lntestminer.HarnessMiner) {

	t.Log("Waiting for bitcoind backend to sync to miner...")
	syncState := int32(0)
	err := wait.NoError(func() error {
		backendCount, err := backend.GetBlockCount()
		if err != nil {
			return fmt.Errorf("unable to get backend height: %w",
				err)
		}

		backendHeight := int32(backendCount)

		_, minerHeight := miner.GetBestBlock()
		if backendHeight > syncState+1000 {
			t.Logf("Backend height: %d, Miner height: %d",
				backendHeight, minerHeight)
			syncState = backendHeight
		}

		if minerHeight != backendHeight {
			return fmt.Errorf("expected height %d, got %d",
				minerHeight, backendHeight)
		}

		t.Logf("Synced backend to miner at height %d", backendHeight)

		return nil
	}, syncTimeout)
	require.NoError(t, err)
}

// fetchWithHeaders performs a GET with extra request headers and returns the
// body, response headers and status code.
func (ctx *testContext) fetchWithHeaders(t *testing.T, endpoint string,
	reqHeaders map[string]string) ([]byte, http.Header, int) {

	url := fmt.Sprintf("http://%s/%s", ctx.server.listenAddr, endpoint)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	for k, v := range reqHeaders {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	body := new(bytes.Buffer)
	_, err = body.ReadFrom(resp.Body)
	require.NoError(t, err)

	return body.Bytes(), resp.Header, resp.StatusCode
}

// expectedFilter fetches the BIP158 basic filter for the given height straight
// from the backend, as the expected value for endpoint assertions.
func (ctx *testContext) expectedFilter(t *testing.T, height int64) []byte {
	blockHash, err := ctx.backend.GetBlockHash(height)
	require.NoError(t, err)

	filter, err := ctx.backend.GetBlockFilter(*blockHash, &filterBasic)
	require.NoError(t, err)

	filterBytes, err := hex.DecodeString(filter.Filter)
	require.NoError(t, err)

	return filterBytes
}

// testFilterSingle checks the /filters/single/{height} endpoint: the sealed
// (backend-served, disk cache tier) path, the in-memory tail path and the
// out-of-bounds error.
func testFilterSingle(t *testing.T, ctx *testContext) {
	// Height 100 is long sealed (the first filter file covers heights
	// 0..1999 and more than 2000+reorg-safe-depth blocks were mined), so
	// it must come back with the disk cache tier.
	body, headers, status := ctx.fetchBinaryWithStatus(
		t, "filters/single/100",
	)
	require.Equal(t, 200, status)
	require.Equal(t, ctx.expectedFilter(t, 100), body)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache))
	require.Equal(t, "*", headers.Get(HeaderCORS))
	require.Equal(
		t, strconv.Itoa(len(body)), headers.Get("Content-Length"),
	)

	// The current tip is still in the in-memory tail, so it must be
	// served at the memory tier.
	tipHeight, _ := ctx.bestBlock(t)
	body, headers, status = ctx.fetchBinaryWithStatus(
		t, fmt.Sprintf("filters/single/%d", tipHeight),
	)
	require.Equal(t, 200, status)
	require.Equal(t, ctx.expectedFilter(t, int64(tipHeight)), body)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache))

	// A height above the tip is rejected.
	_, _, status = ctx.fetchBinaryWithStatus(
		t, fmt.Sprintf("filters/single/%d", tipHeight+1),
	)
	require.Equal(t, 400, status)
}

// testRangeRequests checks that sealed files are served with Content-Length
// and support HTTP range requests, and that memory-tier responses of
// fixed-size entries announce an exact Content-Length.
func testRangeRequests(t *testing.T, ctx *testContext) {
	// A sealed header file has a known exact size.
	fullSize := DefaultRegtestHeadersPerFile * headerSize
	body, headers := ctx.fetchBinary(t, "headers/0")
	require.Len(t, body, fullSize)
	require.Equal(
		t, strconv.Itoa(fullSize), headers.Get("Content-Length"),
	)
	require.Equal(t, "bytes", headers.Get("Accept-Ranges"))
	require.NotEmpty(t, headers.Get("Last-Modified"))

	// A range request for the second header must return exactly that
	// header with a 206 status.
	rangeBody, rangeHeaders, status := ctx.fetchWithHeaders(
		t, "headers/0", map[string]string{"Range": "bytes=80-159"},
	)
	require.Equal(t, http.StatusPartialContent, status)
	require.Len(t, rangeBody, 80)
	require.Equal(t, body[80:160], rangeBody)
	require.Equal(t, "80", rangeHeaders.Get("Content-Length"))

	// The memory-tier headers response (unsealed tail) announces its
	// exact size too: entries are fixed-size 80-byte headers.
	tipHeight, _ := ctx.bestBlock(t)
	const startHeight = DefaultRegtestHeadersPerFile
	tailBody, tailHeaders := ctx.fetchBinary(
		t, fmt.Sprintf("headers/%d", startHeight),
	)
	expectedLen := (int(tipHeight) - startHeight + 1) * headerSize
	require.Len(t, tailBody, expectedLen)
	require.Equal(
		t, strconv.Itoa(expectedLen),
		tailHeaders.Get("Content-Length"),
	)
	require.Equal(t, cacheMemory, tailHeaders.Get(HeaderCache))
}
