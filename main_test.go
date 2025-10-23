package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	lntestminer "github.com/lightningnetwork/lnd/lntest/miner"
	"github.com/lightningnetwork/lnd/lntest/port"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

const (
	numStartupBlocks = 100
	numInitialBlocks = 7000
)

var (
	syncTimeout = 2 * time.Minute
	testTimeout = 60 * time.Second

	testParams = chaincfg.RegressionNetParams

	totalInitialBlocks = numStartupBlocks + testParams.CoinbaseMaturity +
		numInitialBlocks
)

type testContext struct {
	miner   *lntestminer.HarnessMiner
	backend *rpcclient.Client
	server  *server
}

func (ctx *testContext) fetchJSON(t *testing.T, endpoint string,
	target interface{}) http.Header {

	t.Helper()

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

	t.Helper()

	url := fmt.Sprintf("http://%s/%s", ctx.server.listenAddr, endpoint)
	resp, err := http.Get(url)
	require.NoError(t, err)

	defer func() {
		require.NoError(t, resp.Body.Close())
	}()

	body := new(bytes.Buffer)
	_, err = body.ReadFrom(resp.Body)
	require.NoError(t, err)

	return body.Bytes(), resp.Header
}

func (ctx *testContext) bestBlock(t *testing.T) (int32, chainhash.Hash) {
	t.Helper()

	height, err := ctx.backend.GetBlockCount()
	require.NoError(t, err)

	blockHash, err := ctx.backend.GetBlockHash(height)
	require.NoError(t, err)

	return int32(height), *blockHash
}

type testFunc func(t *testing.T, ctx *testContext)

var testCases = []struct {
	name string
	fn   testFunc
}{
	{
		name: "index",
		fn:   testIndex,
	},
	{
		name: "status",
		fn:   testStatus,
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

func testStatus(t *testing.T, ctx *testContext) {
	var status Status
	headers := ctx.fetchJSON(t, "status", &status)

	require.Equal(t, int32(totalInitialBlocks), status.BestBlockHeight)
	require.Equal(t, testParams.Name, status.ChainName)
	require.Equal(
		t, testParams.GenesisHash.String(), status.ChainGenesisHash,
	)
	require.Equal(
		t, ctx.server.headersPerFile, status.EntriesPerHeader,
	)
	require.Equal(
		t, ctx.server.filtersPerFile, status.EntriesPerFilter,
	)

	require.Contains(t, headers, "Cache-Control")
	require.Equal(t, "max-age=60", headers.Get("Cache-Control"))
	require.Equal(t, "*", headers.Get("Access-Control-Allow-Origin"))

	height, blockHash := ctx.bestBlock(t)
	require.Equal(t, height, status.BestBlockHeight)
	require.Equal(t, blockHash.String(), status.BestBlockHash)
}

func testIndex(t *testing.T, ctx *testContext) {
	data, headers := ctx.fetchBinary(t, "")
	require.Contains(
		t, string(data), "<title>Block Delivery Network</title>",
	)
	require.Equal(t, "*", headers.Get("Access-Control-Allow-Origin"))
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

	require.Contains(t, headers, "Cache-Control")
	require.Equal(t, "max-age=1", headers.Get("Cache-Control"))
	require.Equal(t, "*", headers.Get("Access-Control-Allow-Origin"))

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

	require.Contains(t, headers, "Cache-Control")
	require.Equal(t, "max-age=31536000", headers.Get("Cache-Control"))
	require.Equal(t, "*", headers.Get("Access-Control-Allow-Origin"))
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

	require.Contains(t, headers, "Cache-Control")
	require.Equal(t, "max-age=31536000", headers.Get("Cache-Control"))
	require.Equal(t, "*", headers.Get("Access-Control-Allow-Origin"))
}

func TestBlockDN(t *testing.T) {
	ctx := context.Background()
	testDir := ".unit-test-logs"

	_ = os.RemoveAll(testDir)
	_ = os.MkdirAll(testDir, 0700)

	miner := lntestminer.NewTempMiner(
		ctx, t, filepath.Join(testDir, "temp-miner"), "miner.log",
	)
	require.NoError(t, miner.SetUp(true, 100))

	t.Cleanup(miner.Stop)

	backend, backendCfg, cleanup := newBitcoind(
		t, testDir, []string{
			"-regtest",
			"-txindex",
			"-disablewallet",
			"-peerblockfilters=1",
			"-blockfilterindex=1",
			"-dbcache=512",
		},
	)

	t.Cleanup(func() {
		require.NoError(t, cleanup())
	})

	err := wait.NoError(func() error {
		return backend.AddNode(miner.P2PAddress(), rpcclient.ANAdd)
	}, testTimeout)
	require.NoError(t, err)

	dataDir := t.TempDir()
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port.NextAvailablePort())

	setupLogging(testDir)
	testServer := newServer(
		false, dataDir, listenAddr, &backendCfg, unittest.NetParams, 6,
		DefaultRegtestHeadersPerFile, DefaultRegtestFiltersPerFile,
	)

	// Mine a couple blocks and wait for the backend to catch up.
	t.Logf("Mining %d blocks...", numInitialBlocks)
	_ = miner.MineEmptyBlocks(numInitialBlocks)

	// Wait until the backend is fully synced to the miner.
	t.Log("Waiting for bitcoind backend to sync to miner...")
	syncState := int32(0)
	err = wait.NoError(func() error {
		backendCount, err := backend.GetBlockCount()
		if err != nil {
			return fmt.Errorf("unable to get backend height: %w",
				err)
		}

		backendHeight := int32(backendCount)

		_, minerHeight, err := miner.Client.GetBestBlock()
		if err != nil {
			return fmt.Errorf("unable to get miner height: %w", err)
		}

		if backendHeight > syncState+1000 {
			t.Logf("Backend height: %d, Miner height: %d",
				backendHeight, minerHeight)
			syncState = backendHeight
		}

		if minerHeight != backendHeight {
			return fmt.Errorf("expected height %d, got %d",
				minerHeight, backendHeight)
		}

		return nil
	}, syncTimeout)
	require.NoError(t, err)

	t.Logf("Starting block-dn server at %s...", listenAddr)

	require.NoError(t, testServer.start())
	err = wait.NoError(func() error {
		height := testServer.currentHeight.Load()
		_, minerHeight, err := miner.Client.GetBestBlock()
		if err != nil {
			return fmt.Errorf("unable to get miner height: %w", err)
		}

		if minerHeight != height {
			return fmt.Errorf("expected height %d, got %d",
				minerHeight, height)
		}

		return nil
	}, syncTimeout)
	require.NoError(t, err)

	for _, testCase := range testCases {
		success := t.Run(testCase.name, func(t *testing.T) {
			testCase.fn(t, &testContext{
				miner:   miner,
				backend: backend,
				server:  testServer,
			})
		})
		if !success {
			t.Fatalf("test case %s failed", testCase.name)
		}
	}
}

func newBitcoind(t *testing.T, logdir string,
	extraArgs []string) (*rpcclient.Client, rpcclient.ConnConfig,
	func() error) {

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

	client, err := rpcclient.New(&rpcCfg, nil)
	if err != nil {
		_ = cleanUp()
		require.NoError(t, err)
	}

	return client, rpcCfg, cleanUp
}
