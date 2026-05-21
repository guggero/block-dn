package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
)

const (
	headersPerFile = 100
	initialBlocks  = 450

	// testReOrgSafeDepth is small enough that all expected file
	// boundaries within initialBlocks/finalBlocks are reached + buried by
	// the time the test asserts file presence. With this depth the file
	// covering [400..499] is sealed once tip ≥ 505, which finalBlocks=550
	// satisfies, but the file covering [500..599] stays unsealed (need
	// tip ≥ 605) — matching the existing expected counts.
	testReOrgSafeDepth = 6

	headerFileSize int64 = headersPerFile * headerSize
	filterFileSize int64 = headersPerFile * filterHeadersSize
)

func TestHeaderFilesUpdate(t *testing.T) {
	miner, backend, _, _ := setupBackend(t, unitTestDir)

	// Mine initial blocks. The miner starts with 438 blocks already mined.
	_ = miner.MineEmptyBlocks(initialBlocks - int(totalStartupBlocks))

	// Wait until the backend is fully synced to the miner.
	waitBackendSync(t, backend, miner)

	// First run: start from scratch.
	dataDir := t.TempDir()
	quit := make(chan struct{})
	h2hCache := newH2HCache(backend)
	hf := newHeaderFiles(
		headersPerFile, testReOrgSafeDepth, backend, quit, dataDir,
		&testParams, h2hCache,
	)

	var wg sync.WaitGroup

	// Wait for the initial blocks to be written.
	waitForTargetHeight(t, &wg, hf, initialBlocks)

	// Check files.
	headerDir := filepath.Join(dataDir, HeaderFileDir)
	files, err := os.ReadDir(headerDir)
	require.NoError(t, err)
	require.Len(t, files, 8)

	// Check file names and sizes.
	checkHeaderFiles(t, headerDir, 0, 99)
	checkHeaderFiles(t, headerDir, 100, 199)
	checkHeaderFiles(t, headerDir, 200, 299)
	checkHeaderFiles(t, headerDir, 300, 399)

	// Stop the service.
	close(quit)
	wg.Wait()

	// Second run: restart and continue.
	const finalBlocks = 550
	_ = miner.MineEmptyBlocks(finalBlocks - initialBlocks)

	// Wait until the backend is fully synced to the miner.
	waitBackendSync(t, backend, miner)

	quit = make(chan struct{})
	hf = newHeaderFiles(
		headersPerFile, testReOrgSafeDepth, backend, quit, dataDir,
		&testParams, h2hCache,
	)

	// Wait for the final blocks to be written.
	waitForTargetHeight(t, &wg, hf, finalBlocks)

	// Check files again.
	files, err = os.ReadDir(headerDir)
	require.NoError(t, err)
	require.Len(t, files, 10)

	// Check new file names and sizes.
	checkHeaderFiles(t, headerDir, 400, 499)

	// Stop the service.
	close(quit)
	wg.Wait()
}

type fileWriter interface {
	updateFiles(targetHeight int32) error
	getCurrentHeight() int32
}

func waitForTargetHeight(t *testing.T, wg *sync.WaitGroup, hf fileWriter,
	targetHeight int32) {

	t.Helper()

	wg.Go(func() {
		err := hf.updateFiles(targetHeight)

		// nolint:testifylint
		require.ErrorIs(t, err, errServerShutdown)
	})

	// Wait for sync.
	err := wait.NoError(func() error {
		if hf.getCurrentHeight() != targetHeight {
			return fmt.Errorf("not synced yet, current "+
				"height %d", hf.getCurrentHeight())
		}
		return nil
	}, syncTimeout)
	require.NoError(t, err)
}

func checkHeaderFiles(t *testing.T, headerDir string, start, end int32) {
	checkFile(
		t, fmt.Sprintf(HeaderFileNamePattern, headerDir, start, end),
		headerFileSize,
	)
	checkFile(t, fmt.Sprintf(
		FilterHeaderFileNamePattern, headerDir, start, end,
	), filterFileSize)
}

func checkFile(t *testing.T, fileName string, expectedSize int64) {
	info, err := os.Stat(fileName)
	require.NoError(t, err)
	require.Equal(t, expectedSize, info.Size())
}
