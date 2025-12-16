package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	lntestminer "github.com/lightningnetwork/lnd/lntest/miner"
	"github.com/lightningnetwork/lnd/lntest/port"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

var (
	syncTimeout  = 2 * time.Minute
	testTimeout  = 60 * time.Second
	shortTimeout = 5 * time.Second
)

// TestContext is a struct that holds a test instance of a block-dn server and
// some of its dependencies.
type TestContext struct {
	dataDir    string
	listenAddr string
	miner      *lntestminer.HarnessMiner
	backend    *rpcclient.Client
	server     *server
}

// NewTestContext creates a new instance of block-dn for testing.
func NewTestContext(t *testing.T, miner *lntestminer.HarnessMiner,
	backend *rpcclient.Client,
	backendCfg rpcclient.ConnConfig) *TestContext {

	dataDir := t.TempDir()
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port.NextAvailablePort())

	testServer := newServer(
		false, true, dataDir, listenAddr, &backendCfg,
		unittest.NetParams, 6, DefaultRegtestHeadersPerFile,
		DefaultRegtestFiltersPerFile, DefaultRegtestSPTweaksPerFile,
	)
	return &TestContext{
		dataDir:    dataDir,
		listenAddr: listenAddr,
		miner:      miner,
		backend:    backend,
		server:     testServer,
	}
}

// Start starts the block-dn instance and makes sure it is fully synced to its
// chain backend and that all files up to that point have been written.
func (ctx *TestContext) Start(t *testing.T) {
	// Wait until the backend is fully synced to the miner.
	ctx.WaitBackendSync(t)

	t.Logf("Starting block-dn server at %s...", ctx.listenAddr)
	require.NoError(t, ctx.server.start())
	ctx.WaitFilesSync(t)
}

// FetchJSON fetches a specific endpoint from the testing block-dn instance and
// attempt to parse the response as JSON.
func (ctx *TestContext) FetchJSON(t *testing.T, endpoint string,
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

// FetchBinary fetches a specific endpoint from the testing block-dn instance
// and returns it as a binary blob.
func (ctx *TestContext) FetchBinary(t *testing.T, endpoint string) ([]byte,
	http.Header) {

	data, header, _ := ctx.FetchBinaryWithStatus(t, endpoint)
	return data, header
}

// FetchBinaryWithStatus fetches a specific endpoint from the testing block-dn
// instance and returns it as a binary blob alongside the response HTTP headers.
func (ctx *TestContext) FetchBinaryWithStatus(t *testing.T,
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

// BestBlock returns the height and hash of the best block currently known to
// and processed by block-dn.
func (ctx *TestContext) BestBlock(t *testing.T) (int32, chainhash.Hash) {
	height, err := ctx.backend.GetBlockCount()
	require.NoError(t, err)

	blockHash, err := ctx.backend.GetBlockHash(height)
	require.NoError(t, err)

	return int32(height), *blockHash
}

// BlockAtHeight returns a specific block at the given height from the chain
// backend block-dn is connected to.
func (ctx *TestContext) BlockAtHeight(t *testing.T,
	height int32) *wire.MsgBlock {

	hash, err := ctx.backend.GetBlockHash(int64(height))
	require.NoError(t, err)

	block, err := ctx.backend.GetBlock(hash)
	require.NoError(t, err)

	return block
}

// FetchPrevOutScript is a helper function that fetches a previous output script
// from the chain backend.
func (ctx *TestContext) FetchPrevOutScript(op wire.OutPoint) ([]byte, error) {
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

// WaitBackendSync waits until the chain backend has fully synced to the miner.
func (ctx *TestContext) WaitBackendSync(t *testing.T) {
	waitBackendSync(t, ctx.backend, ctx.miner)
}

// WaitFilesSync waits until block-dn has fully caught up with the chain backend
// it is connected to and has written all relevant files to disk up to that
// height.
func (ctx *TestContext) WaitFilesSync(t *testing.T) {
	err := wait.NoError(func() error {
		headerHeight := ctx.server.headerFiles.getCurrentHeight()
		_, minerHeight, err := ctx.miner.Client.GetBestBlock()
		if err != nil {
			return fmt.Errorf("unable to get miner height: %w", err)
		}

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

// waitBackendSync waits until the given backend has been fully synced to the
// given miner.
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

		t.Logf("Synced backend to miner at height %d", backendHeight)

		return nil
	}, syncTimeout)
	require.NoError(t, err)
}

// AssertCacheAndCorsHeaders makes sure the cache and CORS headers match what is
// expected.
func AssertCacheAndCorsHeaders(t *testing.T, headers http.Header,
	expectedCache, expectedCors string) {

	t.Helper()

	require.Contains(t, headers, HeaderCache)
	require.Equal(t, expectedCache, headers.Get(HeaderCache))
	require.Equal(t, expectedCors, headers.Get(HeaderCORS))
}
