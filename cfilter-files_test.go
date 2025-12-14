package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	filtersPerFile        = 100
	emptyFilterSize int64 = 5

	emptyFilterFileSize int64 = headersPerFile * emptyFilterSize
)

func TestCFilterFilesUpdate(t *testing.T) {
	testDir := ".unit-test-logs"
	miner, backend, _, _ := setupBackend(t, testDir)

	// Mine initial blocks. The miner starts with 200 blocks already mined.
	_ = miner.MineEmptyBlocks(initialBlocks - int(totalStartupBlocks))

	// Wait until the backend is fully synced to the miner.
	waitBackendSync(t, backend, miner)

	// First run: start from scratch.
	dataDir := t.TempDir()
	quit := make(chan struct{})
	h2hCache := newH2HCache(backend)
	hf := newCFilterFiles(
		filtersPerFile, backend, quit, dataDir, &testParams, h2hCache,
	)

	var wg sync.WaitGroup

	// Wait for the initial blocks to be written.
	waitForTargetHeight(t, &wg, hf, initialBlocks)

	// Check files.
	filterDir := filepath.Join(dataDir, FilterFileDir)
	files, err := os.ReadDir(filterDir)
	require.NoError(t, err)
	require.Len(t, files, 4)

	// Check file names and sizes.
	checkFilterFiles(t, filterDir, 0, 99)
	checkFilterFiles(t, filterDir, 100, 199)
	checkFilterFiles(t, filterDir, 200, 299)
	checkFilterFiles(t, filterDir, 300, 399)

	// Stop the service.
	close(quit)
	wg.Wait()

	// Second run: restart and continue.
	const finalBlocks = 550
	_ = miner.MineEmptyBlocks(finalBlocks - initialBlocks)

	// Wait until the backend is fully synced to the miner.
	waitBackendSync(t, backend, miner)

	quit = make(chan struct{})
	hf = newCFilterFiles(
		filtersPerFile, backend, quit, dataDir, &testParams, h2hCache,
	)

	// Wait for the final blocks to be written.
	waitForTargetHeight(t, &wg, hf, finalBlocks)

	// Check files again.
	files, err = os.ReadDir(filterDir)
	require.NoError(t, err)
	require.Len(t, files, 5)

	// Check new file names and sizes.
	checkFilterFiles(t, filterDir, 400, 499)

	// Stop the service.
	close(quit)
	wg.Wait()
}

func checkFilterFiles(t *testing.T, filterDir string, start, end int32) {
	checkFile(
		t, fmt.Sprintf(FilterFileNamePattern, filterDir, start, end),
		emptyFilterFileSize,
	)
}
