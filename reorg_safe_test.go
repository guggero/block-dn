package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/lightningnetwork/lnd/lntest/port"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

// TestCacheControlHeaders is a pure unit test that exercises addCacheHeaders
// directly. It guards two regressions:
//
//  1. The duplicate Cache-Control header bug (Header().Add was called twice
//     for the disk tier, once with `immutable` and once without).
//  2. The 1-year cache being marked `immutable`, which meant Cloudflare and
//     downstream browsers ignored origin updates after a deep reorg — the
//     bug that motivated this whole rewrite.
func TestCacheControlHeaders(t *testing.T) {
	cases := []struct {
		name     string
		maxAge   time.Duration
		expected string
	}{{
		name:     "disk tier (long-lived)",
		maxAge:   maxAgeDisk,
		expected: cacheDisk,
	}, {
		name:     "memory tier (1 minute)",
		maxAge:   maxAgeMemory,
		expected: cacheMemory,
	}, {
		name:     "temporary tier (1 second)",
		maxAge:   maxAgeTemporary,
		expected: cacheTemporary,
	}, {
		name:     "no-cache (zero max age)",
		maxAge:   0,
		expected: "no-cache",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			addCacheHeaders(rec, tc.maxAge)

			values := rec.Header().Values(HeaderCache)
			require.Lenf(t, values, 1, "expected exactly one "+
				"Cache-Control header, got %v", values)
			require.Equal(t, tc.expected, values[0])

			// Defense in depth: the disk tier must never claim
			// `immutable`. That assertion is what would have
			// caught the original Cloudflare-stale-bytes bug at
			// the unit level.
			if tc.maxAge >= maxAgeDisk {
				require.NotContainsf(t, values[0], "immutable",
					"disk-tier Cache-Control must not "+
						"include `immutable`")
				require.Containsf(t, values[0],
					"stale-while-revalidate",
					"disk-tier Cache-Control must "+
						"include "+
						"`stale-while-revalidate`")
			}
		})
	}
}

// reorgTestParams configures a test server with small per-file counts and a
// small reorg-safe depth so we can reach interesting state quickly.
type reorgTestParams struct {
	headersPerFile  int32
	filtersPerFile  int32
	spTweaksPerFile int32
	reOrgSafeDepth  uint32
}

func defaultReorgTestParams() reorgTestParams {
	// headersPerFile must be larger than totalStartupBlocks (~450 on
	// regtest with our miner harness) so the first file boundary lands
	// at a height we control via post-startup mining, not at an arbitrary
	// point that happens to fall inside the startup sequence.
	return reorgTestParams{
		headersPerFile:  600,
		filtersPerFile:  600,
		spTweaksPerFile: 600,
		reOrgSafeDepth:  6,
	}
}

// startReorgTestServer brings up a btcd miner + bitcoind backend + block-dn
// server with the given small file/depth params and returns a testContext
// pointing at them. The server is reachable at ctx.server.listenAddr and is
// stopped via t.Cleanup.
func startReorgTestServer(t *testing.T,
	params reorgTestParams) *testContext {

	// Re-enable Taproot from genesis on regtest so the SP indexer has
	// something to do; matches TestBlockDN's setup.
	TaprootActivationHeights[chaincfg.RegressionNetParams.Net] = 1

	miner, backend, backendCfg, _ := setupBackend(t)
	dataDir := t.TempDir()
	listenAddr := fmt.Sprintf("127.0.0.1:%d", port.NextAvailablePort())

	srv := newServer(
		false, true, true, dataDir, listenAddr, &backendCfg,
		unittest.NetParams, params.reOrgSafeDepth,
		params.headersPerFile, params.filtersPerFile,
		params.spTweaksPerFile,
		defaultReadTimeout, defaultWriteTimeout,
	)
	ctx := &testContext{
		miner:   miner,
		backend: backend,
		server:  srv,
	}

	require.NoError(t, srv.start())
	t.Cleanup(func() {
		_ = srv.stop()
	})

	return ctx
}

// waitForSealedHeight blocks until the producer's lastSealedHeight reaches at
// least target. Useful for asserting that the sealing logic actually fires
// after enough blocks have been mined past a boundary.
func waitForSealedHeight(t *testing.T, p blockProcessor, target int32) {
	t.Helper()
	err := wait.NoError(func() error {
		got := p.getLastSealedHeight()
		if got < target {
			return fmt.Errorf("lastSealedHeight=%d, want >= %d",
				got, target)
		}
		return nil
	}, syncTimeout)
	require.NoError(t, err)
}

// waitForCurrentHeight blocks until the producer's currentHeight reaches
// target.
func waitForCurrentHeight(t *testing.T, p blockProcessor, target int32) {
	t.Helper()
	err := wait.NoError(func() error {
		got := p.getCurrentHeight()
		if got != target {
			return fmt.Errorf("currentHeight=%d, want %d", got,
				target)
		}
		return nil
	}, syncTimeout)
	require.NoError(t, err)
}

// fetchURL is a tiny HTTP helper that returns body, headers and status.
func fetchURL(t *testing.T, urlPath, listenAddr string) ([]byte, http.Header,
	int) {

	t.Helper()
	url := fmt.Sprintf("http://%s/%s", listenAddr,
		strings.TrimPrefix(urlPath, "/"))
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return body, resp.Header, resp.StatusCode
}

// TestReorgSafeSealing is the big integration test for Change 1 + Change 2.
// It exercises the exact edge case the user called out: at tip 100_050 (in
// the small-scale regtest equivalent), a request for /headers/import/100
// must succeed and return the same 100 headers — but with cache headers that
// say "memory tier" before the seal, and "disk tier (with
// stale-while-revalidate)" after.
func TestReorgSafeSealing(t *testing.T) {
	params := defaultReorgTestParams()
	ctx := startReorgTestServer(t, params)

	// Mine just enough blocks to put us past the first file boundary but
	// not yet past the safe-depth threshold for that boundary.
	//
	//   boundary = headersPerFile - 1                = 99
	//   safe-tip required to seal that boundary      = 99 + 6 = 105
	//
	// We aim for a tip somewhere in (boundary, safeRequired) — say
	// boundary + 1 — so the first file MUST NOT be on disk yet.
	preSealTarget := params.headersPerFile + 1

	// Account for the blocks setupBackend mines during startup (covered
	// by totalStartupBlocks); we only need to mine the delta.
	delta := preSealTarget - int32(totalStartupBlocks)
	require.Positive(t, delta, "test setup mines too many startup "+
		"blocks")
	_ = ctx.miner.MineEmptyBlocks(int(delta))
	ctx.waitBackendSync(t)
	waitForCurrentHeight(t, ctx.server.headerFiles, preSealTarget)

	// At this point the file boundary (99) is still well within the
	// reorg-safe window. No file should exist on disk yet.
	firstFile := fmt.Sprintf(HeaderFileNamePattern,
		filepath.Join(ctx.server.baseDir, HeaderFileDir),
		int64(0), int64(params.headersPerFile-1))
	require.NoFileExists(t, firstFile, "file %s exists but boundary "+
		"hasn't crossed safe depth yet", firstFile)
	require.EqualValues(t, -1, ctx.server.headerFiles.getLastSealedHeight(),
		"lastSealedHeight should still be -1")

	// The /headers/import/<headersPerFile> endpoint should serve the
	// first headersPerFile headers from memory, with memory-tier cache
	// headers.
	importPath := fmt.Sprintf("headers/import/%d", params.headersPerFile)
	bodyBefore, headers, status := fetchURL(
		t, importPath, ctx.server.listenAddr,
	)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache),
		"pre-seal: import endpoint must use memory-tier cache "+
			"(response is still mutable until seal)")

	expectedLen := importMetadataSize + int(params.headersPerFile)*
		headerSize
	require.Lenf(t, bodyBefore, expectedLen, "pre-seal body length "+
		"mismatch")

	// Now mine enough additional blocks to bury the first boundary past
	// reOrgSafeDepth, triggering the seal.
	more := int32(params.reOrgSafeDepth) + 1
	_ = ctx.miner.MineEmptyBlocks(int(more))
	ctx.waitBackendSync(t)
	waitForSealedHeight(
		t, ctx.server.headerFiles, params.headersPerFile-1,
	)

	// The file must now exist on disk.
	require.FileExists(t, firstFile, "file %s should be sealed by now",
		firstFile)

	// And the same import URL must return byte-identical content but with
	// disk-tier cache headers including stale-while-revalidate (and NO
	// immutable).
	bodyAfter, headers, status := fetchURL(
		t, importPath, ctx.server.listenAddr,
	)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, bodyBefore, bodyAfter, "post-seal body must be "+
		"byte-identical to pre-seal body (same logical content)")
	require.Equal(t, cacheDisk, headers.Get(HeaderCache),
		"post-seal: import endpoint must use disk-tier cache")
	require.NotContains(t, headers.Get(HeaderCache), "immutable",
		"post-seal: must not advertise immutable (deep reorgs can "+
			"still invalidate the file)")
	require.Contains(t, headers.Get(HeaderCache),
		"stale-while-revalidate")

	// Spot-check the import metadata at the head of the response — the
	// network magic, version, file type and start-height bytes.
	magic := binary.LittleEndian.Uint32(bodyAfter[0:4])
	require.EqualValues(t, unittest.NetParams.Net, magic)
	require.EqualValues(t, headerImportVersion, bodyAfter[4])
	require.Equal(t, typeBlockHeader, bodyAfter[5])
	startHeight := binary.LittleEndian.Uint32(bodyAfter[6:10])
	require.EqualValues(t, 0, startHeight)

	// Repeat the cache-tier flip assertion on the single-file endpoint.
	// Before this point the second file (heights headersPerFile .. 2*
	// headersPerFile - 1) is still being built in memory; request a height
	// strictly above lastSealedHeight to land on the memory path.
	singlePath := fmt.Sprintf("headers/%d", params.headersPerFile)
	_, headers, status = fetchURL(t, singlePath, ctx.server.listenAddr)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, cacheMemory, headers.Get(HeaderCache),
		"single-file endpoint above lastSealedHeight must be "+
			"memory-tier")

	// Request the sealed file (heights 0 .. headersPerFile - 1).
	singlePath = "headers/0"
	_, headers, status = fetchURL(t, singlePath, ctx.server.listenAddr)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, cacheDisk, headers.Get(HeaderCache),
		"single-file endpoint at sealed boundary must be disk-tier")
}

// TestReorgShallowRollback verifies that when bitcoind reorgs within the
// reorg-safe window, the producers roll back their in-memory state and
// continue forward on the new chain — without touching the sealed file.
func TestReorgShallowRollback(t *testing.T) {
	params := defaultReorgTestParams()
	ctx := startReorgTestServer(t, params)

	// Seal at least one file: mine past boundary + safe depth.
	firstSealAt := params.headersPerFile + int32(params.reOrgSafeDepth)
	delta := firstSealAt - int32(totalStartupBlocks)
	require.Positive(t, delta)
	_ = ctx.miner.MineEmptyBlocks(int(delta))

	// Mine a few extra in-memory blocks on top — these will be the ones
	// we reorg away.
	const inMemoryExtra = 4
	_ = ctx.miner.MineEmptyBlocks(inMemoryExtra)

	ctx.waitBackendSync(t)
	ctx.waitFilesSync(t)
	waitForSealedHeight(
		t, ctx.server.headerFiles, params.headersPerFile-1,
	)

	// Snapshot the sealed file's bytes and modification time so we can
	// later prove the disk file was untouched by the reorg.
	sealedPath := fmt.Sprintf(HeaderFileNamePattern,
		filepath.Join(ctx.server.baseDir, HeaderFileDir),
		int64(0), int64(params.headersPerFile-1))
	sealedBefore, err := os.ReadFile(sealedPath)
	require.NoError(t, err)
	sealedStatBefore, err := os.Stat(sealedPath)
	require.NoError(t, err)

	// Trigger a reorg of inMemoryExtra blocks: invalidate the (oldTip -
	// inMemoryExtra + 1) block on the miner so it disconnects the in-
	// memory range, then mine a longer alternative.
	tipHash, oldTip := ctx.miner.GetBestBlock()
	require.NotNil(t, tipHash)
	reorgFrom := oldTip - inMemoryExtra + 1
	invalidateHash, err := ctx.backend.GetBlockHash(int64(reorgFrom))
	require.NoError(t, err)
	require.NoError(t, ctx.miner.InvalidateBlock(invalidateHash))

	// Mine more blocks than we just abandoned so the new chain wins.
	// Use GenerateBlocks (the btcd `generate` RPC) rather than
	// MineEmptyBlocks: the latter uses getblocktemplate+submitblock with
	// zero time and a stable coinbase, which after InvalidateBlock can
	// reproduce the exact same block hash we just invalidated (btcd then
	// rejects it with "already have block"). `generate` picks a fresh
	// coinbase per block, so the alt chain's blocks differ regardless of
	// wall-clock granularity.
	const altLen = inMemoryExtra + 3
	_ = ctx.miner.GenerateBlocks(altLen)

	// Wait for bitcoind to follow.
	ctx.waitBackendSync(t)

	// Wait for the server to observe the reorg and resync to the new tip.
	expectedNewTip := oldTip - inMemoryExtra + altLen
	waitForCurrentHeight(t, ctx.server.headerFiles, expectedNewTip)
	waitForCurrentHeight(t, ctx.server.cFilterFiles, expectedNewTip)
	waitForCurrentHeight(t, ctx.server.spTweakFiles, expectedNewTip)

	// The sealed file MUST NOT have been rewritten. Same bytes, same
	// mtime.
	sealedAfter, err := os.ReadFile(sealedPath)
	require.NoError(t, err)
	require.Equal(t, sealedBefore, sealedAfter, "sealed file was "+
		"rewritten during reorg — reorg-safe invariant violated")
	sealedStatAfter, err := os.Stat(sealedPath)
	require.NoError(t, err)
	require.Equal(t, sealedStatBefore.ModTime(),
		sealedStatAfter.ModTime(),
		"sealed file mtime changed during reorg")

	// The server's status endpoint should now report the new chain's
	// tip hash, not the abandoned tip.
	newTipHash, err := ctx.backend.GetBlockHash(int64(expectedNewTip))
	require.NoError(t, err)
	require.NotEqual(t, tipHash.String(), newTipHash.String())

	var status Status
	url := fmt.Sprintf("http://%s/status", ctx.server.listenAddr)
	resp, err := http.Get(url)
	require.NoError(t, err)
	statusBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.NoError(t, json.Unmarshal(statusBody, &status))
	require.Equal(t, newTipHash.String(), status.BestBlockHash,
		"status should reflect post-reorg tip")
}

// TestReorgDeepFailsFast verifies that a reorg back past lastSealedHeight is
// refused: the producer returns errDeepReorg via the server's errs queue
// rather than rewriting an already-sealed file.
func TestReorgDeepFailsFast(t *testing.T) {
	params := defaultReorgTestParams()
	ctx := startReorgTestServer(t, params)

	// Seal at least one file plus a generous in-memory tail.
	target := params.headersPerFile + int32(params.reOrgSafeDepth) + 5
	delta := target - int32(totalStartupBlocks)
	require.Positive(t, delta)
	_ = ctx.miner.MineEmptyBlocks(int(delta))
	ctx.waitBackendSync(t)
	ctx.waitFilesSync(t)
	waitForSealedHeight(
		t, ctx.server.headerFiles, params.headersPerFile-1,
	)

	sealedPath := fmt.Sprintf(HeaderFileNamePattern,
		filepath.Join(ctx.server.baseDir, HeaderFileDir),
		int64(0), int64(params.headersPerFile-1))
	sealedBefore, err := os.ReadFile(sealedPath)
	require.NoError(t, err)
	sealedStatBefore, err := os.Stat(sealedPath)
	require.NoError(t, err)

	// Capture the pre-reorg tip BEFORE invalidating, because
	// miner.GetBestBlock after InvalidateBlock would return the new
	// (shorter) tip and our altLen math depends on the original height.
	_, preInvalidateTip := ctx.miner.GetBestBlock()

	// Invalidate a block INSIDE the sealed file (height
	// headersPerFile / 2). This forces a reorg back across the sealed
	// boundary.
	deepHeight := int64(params.headersPerFile / 2)
	deepHash, err := ctx.backend.GetBlockHash(deepHeight)
	require.NoError(t, err)
	require.NoError(t, ctx.miner.InvalidateBlock(deepHash))

	// See TestReorgShallowRollback for why GenerateBlocks is used here.
	altLen := uint32(preInvalidateTip-int32(deepHeight)) + 5
	_ = ctx.miner.GenerateBlocks(altLen)
	ctx.waitBackendSync(t)

	// Expect an errDeepReorg to land in s.errs within a reasonable
	// window. The server's producers each independently detect, so any
	// of them can be the first to report.
	select {
	case err := <-ctx.server.errs.ChanOut():
		require.ErrorIs(t, err, errDeepReorg,
			"expected errDeepReorg, got %v", err)
	case <-time.After(syncTimeout):
		t.Fatal("server did not surface a deep-reorg error within " +
			"the timeout — sealed files may have been silently " +
			"rewritten")
	}

	// Verify the sealed file is untouched. This is the load-bearing
	// assertion — even on the failure path, no immutable file should
	// have been clobbered.
	sealedAfter, err := os.ReadFile(sealedPath)
	require.NoError(t, err)
	require.Equal(t, sealedBefore, sealedAfter,
		"sealed file was rewritten during deep reorg!")
	sealedStatAfter, err := os.Stat(sealedPath)
	require.NoError(t, err)
	require.Equal(t, sealedStatBefore.ModTime(),
		sealedStatAfter.ModTime(),
		"sealed file mtime changed during deep reorg")
}
