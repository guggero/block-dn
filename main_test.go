package main

import (
	"bytes"
	"context"
	"encoding/hex"
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
	"github.com/btcsuite/btcd/btcutil/psbt"
	sp "github.com/btcsuite/btcd/btcutil/silentpayments"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcwallet/chain"
	lntestminer "github.com/lightningnetwork/lnd/lntest/miner"
	"github.com/lightningnetwork/lnd/lntest/port"
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

	cacheTemporary = "max-age=1"
	cacheMemory    = "max-age=60"
	cacheDisk      = "max-age=31536000"

	corsAll = "*"

	unitTestDir = ".unit-test-logs"
)

var (
	testParams = chaincfg.RegressionNetParams

	totalStartupBlocks = numStartupBlocks +
		uint32(testParams.CoinbaseMaturity) +
		testParams.MinerConfirmationWindow*2
	totalInitialBlocks = totalStartupBlocks + numInitialBlocks
)

type testFunc func(t *testing.T, ctx *TestContext)

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
		name: "filter-headers",
		fn:   testFilterHeaders,
	},
	{
		name: "filter-headers-import",
		fn:   testFilterHeadersImport,
	},
	{
		name: "sp-tweak-data",
		fn:   testSPTweakData,
	},
	{
		name: "tx-out-proof",
		fn:   testTxOutProof,
	},
	{
		name: "tx-raw",
		fn:   testTxRaw,
	},
}

func testErrors(t *testing.T, ctx *TestContext) {
	type errorResponse struct {
		status int
		error  string
	}

	var (
		badHash       = strings.Repeat("k", 64)
		badInt64      = strings.Repeat("9", 20)
		badHeight     = strconv.Itoa(int(totalInitialBlocks + 1))
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
				"current height %d", badHeight,
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
				"current height %d", badHeight,
				totalInitialBlocks),
		}
		respBadHashLength = errorResponse{
			status: 400,
			error:  ErrInvalidHashLength.Error(),
		}
		respNotFound = errorResponse{
			status: 404,
			error:  "404 page not found",
		}
	)
	errorCases := map[string]errorResponse{
		"foo":                                respNotFound,
		"headers":                            respNotFound,
		"headers/" + badInt64:                respBadHeight,
		"headers/1":                          respBadStartHeight1,
		"headers/" + badHeight:               respBadStartHeightLarge,
		"headers/import":                     respNotFound,
		"headers/import/" + badInt64:         respBadHeight,
		"headers/import/0":                   respBadEndHeight0,
		"headers/import/1000":                respBadEndHeightPartial,
		"headers/import/" + badHeight:        respBadEndHeightLarge,
		"filter-headers":                     respNotFound,
		"filter-headers/" + badInt64:         respBadHeight,
		"filter-headers/1":                   respBadStartHeight1,
		"filter-headers/" + badHeight:        respBadStartHeightLarge,
		"filter-headers/import":              respNotFound,
		"filter-headers/import/" + badInt64:  respBadHeight,
		"filter-headers/import/0":            respBadEndHeight0,
		"filter-headers/import/1000":         respBadEndHeightPartial,
		"filter-headers/import/" + badHeight: respBadEndHeightLarge,
		"filters":                            respNotFound,
		"filters/" + badInt64:                respBadHeight,
		"filters/1":                          respBadStartHeight1,
		"filters/" + badHeight:               respBadStartHeightLarge,
		"block":                              respNotFound,
		"block/aaaa":                         respBadHashLength,
		"block/" + badHash:                   respNotFound,
		"tx/out-proof":                       respNotFound,
		"tx/out-proof/aaaa":                  respBadHashLength,
		"tx/out-proof/" + badHash:            respNotFound,
		"tx/raw":                             respNotFound,
		"tx/raw/aaaa":                        respBadHashLength,
		"tx/raw/" + badHash:                  respNotFound,
	}
	for endpoint, expected := range errorCases {
		body, headers, status := ctx.FetchBinaryWithStatus(t, endpoint)
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

		AssertCacheAndCorsHeaders(t, headers, cacheMemory, corsAll)
	}
}

func testIndex(t *testing.T, ctx *TestContext) {
	data, headers := ctx.FetchBinary(t, "")
	require.Contains(
		t, string(data), "<title>Block Delivery Network</title>",
	)
	require.Equal(t, "*", headers.Get(HeaderCORS))
}

func testStatus(t *testing.T, ctx *TestContext) {
	var status Status
	headers := ctx.FetchJSON(t, "status", &status)

	require.Equal(t, int32(totalInitialBlocks), status.BestBlockHeight)
	require.Equal(t, testParams.Name, status.ChainName)
	require.Equal(
		t, testParams.GenesisHash.String(), status.ChainGenesisHash,
	)
	require.EqualValues(
		t, DefaultRegtestHeadersPerFile,
		status.EntriesPerHeaderFile,
	)
	require.EqualValues(
		t, DefaultRegtestFiltersPerFile,
		status.EntriesPerFilterFile,
	)
	require.EqualValues(
		t, DefaultRegtestSPTweaksPerFile,
		status.EntriesPerSPTweakFile,
	)

	AssertCacheAndCorsHeaders(t, headers, cacheMemory, corsAll)

	height, blockHash := ctx.BestBlock(t)
	require.Equal(t, height, status.BestBlockHeight)
	require.Equal(t, height, status.BestFilterHeight)
	require.Equal(t, height, status.BestSPTweakHeight)
	require.Equal(t, blockHash.String(), status.BestBlockHash)

	filterHeader := ctx.server.headerFiles.filterHeaders[blockHash]
	require.Equal(t, filterHeader.String(), status.BestFilterHeader)
}

func testHeaders(t *testing.T, ctx *TestContext) {
	// We first query for a start block that can be served from files only.
	body, headers := ctx.FetchBinary(t, "headers/0")
	targetLen := DefaultRegtestHeadersPerFile * headerSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	AssertCacheAndCorsHeaders(t, headers, cacheDisk, corsAll)

	// And now we try to fetch all headers up to the current height, which
	// will require some of them to be served from memory.
	const startHeight = DefaultRegtestHeadersPerFile
	body, headers = ctx.FetchBinary(
		t, fmt.Sprintf("headers/%d", startHeight),
	)
	expectedBlocks := totalInitialBlocks - startHeight + 1
	targetLen = int(expectedBlocks) * headerSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	AssertCacheAndCorsHeaders(t, headers, cacheMemory, corsAll)

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

func testHeadersImport(t *testing.T, ctx *TestContext) {
	// We first query for a block height that can be served from files only.
	body, headers := ctx.FetchBinary(
		t, fmt.Sprintf("headers/import/%d",
			DefaultRegtestHeadersPerFile),
	)
	targetLen := importMetadataSize +
		DefaultRegtestHeadersPerFile*headerSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	AssertCacheAndCorsHeaders(t, headers, cacheDisk, corsAll)

	// And now we try to fetch all headers up to the current height, which
	// will require some of them to be served from memory.
	body, headers = ctx.FetchBinary(
		t, fmt.Sprintf("headers/import/%d", totalInitialBlocks),
	)
	targetLen = importMetadataSize + int(totalInitialBlocks+1)*headerSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	AssertCacheAndCorsHeaders(t, headers, cacheMemory, corsAll)

	// We make sure that the last 10 entries are actually correct.
	lastHeight := ctx.server.headerFiles.getCurrentHeight()
	require.Equal(t, int32(totalInitialBlocks), lastHeight)
	for height := lastHeight - 9; height <= lastHeight; height++ {
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

func testFilterHeaders(t *testing.T, ctx *TestContext) {
	// We first query for a start block that can be served from files only.
	body, headers := ctx.FetchBinary(t, "filter-headers/0")
	targetLen := DefaultRegtestHeadersPerFile * filterHeadersSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	AssertCacheAndCorsHeaders(t, headers, cacheDisk, corsAll)

	// And now we try to fetch all headers up to the current height, which
	// will require some of them to be served from memory.
	const startHeight = DefaultRegtestHeadersPerFile
	body, headers = ctx.FetchBinary(
		t, fmt.Sprintf("filter-headers/%d", startHeight),
	)
	expectedBlocks := totalInitialBlocks - startHeight + 1
	targetLen = int(expectedBlocks) * filterHeadersSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	AssertCacheAndCorsHeaders(t, headers, cacheMemory, corsAll)

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

func testFilterHeadersImport(t *testing.T, ctx *TestContext) {
	// We first query for a block height that can be served from files only.
	body, headers := ctx.FetchBinary(
		t, fmt.Sprintf("filter-headers/import/%d",
			DefaultRegtestHeadersPerFile),
	)
	targetLen := importMetadataSize +
		DefaultRegtestHeadersPerFile*filterHeadersSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	AssertCacheAndCorsHeaders(t, headers, cacheDisk, corsAll)

	// And now we try to fetch all headers up to the current height, which
	// will require some of them to be served from memory.
	body, headers = ctx.FetchBinary(
		t, fmt.Sprintf("filter-headers/import/%d", totalInitialBlocks),
	)
	targetLen = importMetadataSize +
		int(totalInitialBlocks+1)*filterHeadersSize
	require.Lenf(t, body, targetLen, "body length should be %d but is %d",
		targetLen, len(body))

	AssertCacheAndCorsHeaders(t, headers, cacheMemory, corsAll)

	// We make sure that the last 10 entries are actually correct.
	lastHeight := ctx.server.headerFiles.getCurrentHeight()
	require.Equal(t, int32(totalInitialBlocks), lastHeight)
	for height := lastHeight - 9; height <= lastHeight; height++ {
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

func testSPTweakData(t *testing.T, ctx *TestContext) {
	var spTweakData SPTweakFile
	headers := ctx.FetchJSON(t, "sp/tweak-data/0", &spTweakData)
	require.Equal(t, int32(0), spTweakData.StartHeight)
	require.Equal(
		t, int32(DefaultRegtestSPTweaksPerFile), spTweakData.NumBlocks,
	)
	require.Len(t, spTweakData.Blocks, int(spTweakData.NumBlocks))

	AssertCacheAndCorsHeaders(t, headers, cacheDisk, corsAll)

	// And now we try to fetch all SP tweak data up to the current height,
	// which will require some of them to be served from memory.
	const startHeight = DefaultRegtestSPTweaksPerFile
	headers = ctx.FetchJSON(
		t, fmt.Sprintf("sp/tweak-data/%d", startHeight), &spTweakData,
	)
	expectedBlocks := totalInitialBlocks - startHeight + 1
	require.Equal(
		t, int32(expectedBlocks), spTweakData.NumBlocks,
	)
	require.Len(t, spTweakData.Blocks, int(expectedBlocks))

	AssertCacheAndCorsHeaders(t, headers, cacheMemory, corsAll)

	// Now we mine some blocks with Taproot outputs to ensure we have
	// Taproot tweaks in the SP tweak data.
	numTrBlocks := uint32(20)
	for range numTrBlocks {
		ctx.miner.SendOutput(&wire.TxOut{
			Value:    5_000,
			PkScript: psbt.SilentPaymentDummyP2TROutput,
		}, 2)
		ctx.miner.SendOutput(&wire.TxOut{
			Value:    5_000,
			PkScript: psbt.SilentPaymentDummyP2TROutput,
		}, 2)
		ctx.miner.MineBlocksAndAssertNumTxes(1, 2)
	}

	ctx.WaitBackendSync(t)
	ctx.WaitFilesSync(t)

	headers = ctx.FetchJSON(
		t, fmt.Sprintf("sp/tweak-data/%d", startHeight), &spTweakData,
	)
	expectedHeight := totalInitialBlocks + numTrBlocks
	expectedBlocks = expectedHeight - DefaultRegtestSPTweaksPerFile + 1
	require.Equal(
		t, int32(expectedBlocks), spTweakData.NumBlocks,
	)
	require.Len(t, spTweakData.Blocks, int(expectedBlocks))

	AssertCacheAndCorsHeaders(t, headers, cacheMemory, corsAll)

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

		block := ctx.BlockAtHeight(t, int32(height))
		require.Len(t, block.Transactions, 3)

		key1, err := sp.TransactionTweakData(
			block.Transactions[1], ctx.FetchPrevOutScript, log,
		)
		require.NoError(t, err)
		require.Equal(
			t, trBlock[1],
			hex.EncodeToString(key1.SerializeCompressed()),
		)

		key2, err := sp.TransactionTweakData(
			block.Transactions[2], ctx.FetchPrevOutScript, log,
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
	ctx.WaitBackendSync(t)
	ctx.WaitFilesSync(t)
}

func testTxOutProof(t *testing.T, ctx *TestContext) {
	// We start with the latest block.
	bestHeight, bestHash := ctx.BestBlock(t)
	block, err := ctx.backend.GetBlock(&bestHash)
	require.NoError(t, err)

	require.Len(t, block.Transactions, 1)
	tx := block.Transactions[0]

	data, headers := ctx.FetchBinary(
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

	data, headers = ctx.FetchBinary(
		t, fmt.Sprintf("tx/out-proof/%s", buriedTx.TxHash().String()),
	)
	require.NotEmpty(t, data)

	AssertCacheAndCorsHeaders(t, headers, cacheDisk, corsAll)
}

func testTxRaw(t *testing.T, ctx *TestContext) {
	_, bestHash := ctx.BestBlock(t)
	block, err := ctx.backend.GetBlock(&bestHash)
	require.NoError(t, err)

	require.Len(t, block.Transactions, 1)
	tx := block.Transactions[0]

	data, headers := ctx.FetchBinary(
		t, fmt.Sprintf("tx/raw/%s", tx.TxHash().String()),
	)
	require.NotEmpty(t, data)

	var txBuf bytes.Buffer
	require.NoError(t, tx.Serialize(&txBuf))

	require.Equal(t, txBuf.Bytes(), data)

	AssertCacheAndCorsHeaders(t, headers, cacheDisk, corsAll)
}

func TestBlockDN(t *testing.T) {
	// Activate Taproot for regtest.
	TaprootActivationHeights[chaincfg.RegressionNetParams.Net] = 1

	miner, backend, backendCfg, _ := setupBackend(t, unitTestDir)
	ctx := NewTestContext(t, miner, backend, backendCfg)

	// Mine a couple blocks and wait for the backend to catch up.
	t.Logf("Mining %d blocks...", numInitialBlocks)
	_ = miner.MineEmptyBlocks(numInitialBlocks)

	ctx.Start(t)
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
			ZMQReadDeadline:        defaultTimeout,
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
	require.NoError(t, miner.SetUp(true, numStartupBlocks))

	// Next mine enough blocks in order for segwit and the CSV package
	// soft-fork to activate on SimNet.
	numBlocks := testParams.MinerConfirmationWindow * 2
	miner.GenerateBlocks(numBlocks)

	t.Cleanup(miner.Stop)

	backend, backendCfg, bitcoindCfg, cleanup := newBitcoind(
		t, testDir, []string{
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
