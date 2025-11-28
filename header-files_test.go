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
	initialBlocks  = 250

	headerFileSize int64 = headersPerFile * headerSize
	filterFileSize int64 = headersPerFile * filterHeadersSize
)

func TestHeaderFilesUpdate(t *testing.T) {
	testDir := ".unit-test-logs"
	miner, backend, _ := setupBackend(t, testDir)

	// Mine initial blocks. The miner starts with 200 blocks already mined.
	_ = miner.MineEmptyBlocks(initialBlocks - int(totalStartupBlocks))

	// Wait until the backend is fully synced to the miner.
	waitBackendSync(t, backend, miner)

	// First run: start from scratch.
	dataDir := t.TempDir()
	quit := make(chan struct{})
	h2hCache := newH2HCache(backend)
	hf := newHeaderFiles(
		headersPerFile, backend, quit, dataDir, &testParams, h2hCache,
	)

	var wg sync.WaitGroup

	// Wait for the initial blocks to be written.
	waitFilesWritten(t, &wg, hf, initialBlocks)

	// Check files.
	headerDir := filepath.Join(dataDir, HeaderFileDir)
	files, err := os.ReadDir(headerDir)
	require.NoError(t, err)
	require.Len(t, files, 4)

	// Check file names and sizes.
	checkHeaderFiles(t, headerDir, 0, 99)
	checkHeaderFiles(t, headerDir, 100, 199)

	// Stop the service.
	close(quit)
	wg.Wait()

	// Second run: restart and continue.
	const finalBlocks = 350
	_ = miner.MineEmptyBlocks(finalBlocks - initialBlocks)

	// Wait until the backend is fully synced to the miner.
	waitBackendSync(t, backend, miner)

	quit = make(chan struct{})
	hf = newHeaderFiles(
		headersPerFile, backend, quit, dataDir, &testParams, h2hCache,
	)

	// Wait for the final blocks to be written.
	waitFilesWritten(t, &wg, hf, finalBlocks)

	// Check files again.
	files, err = os.ReadDir(headerDir)
	require.NoError(t, err)
	require.Len(t, files, 6)

	// Check new file names and sizes.
	checkHeaderFiles(t, headerDir, 200, 299)

	// Stop the service.
	close(quit)
	wg.Wait()
}

type fileWriter interface {
	updateFiles(targetHeight int32) error
	getCurrentHeight() int32
}

func waitFilesWritten(t *testing.T, wg *sync.WaitGroup, hf fileWriter,
	targetHeight int32) {

	t.Helper()

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := hf.updateFiles(targetHeight)
		require.ErrorIs(t, err, errServerShutdown)
	}()

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
