package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btclog"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightninglabs/neutrino"
	"github.com/stretchr/testify/require"
)

var (
	logLevel      = btclog.LevelDebug
	dbOpenTimeout = time.Second * 10
)

// init is called when the package is initialized, before any tests run.
//
// TestMain here because main_test.go has its own setup that doesn't
// route through TestMain.
//
//nolint:gochecknoinits // Test-only logger setup; can't easily use
func init() {
	// Set up logging once for all tests.
	logger := btclog.NewBackend(os.Stdout)
	chainLogger := logger.Logger("CHAIN")
	chainLogger.SetLevel(logLevel)
	neutrino.UseLogger(chainLogger)
	rpcLogger := logger.Logger("RPCC")
	rpcLogger.SetLevel(logLevel)
}

func TestNeutrinoSync(t *testing.T) {
	t.Skipf("Skipping for normal runs, uncomment for manual tests")

	tempDir := t.TempDir()

	// Create filters db for indexing the headers.
	db, err := walletdb.Create(
		"bdb", tempDir+"/filters.db", true, dbOpenTimeout, false,
	)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Create service with headers import but with no peers.
	// This will test that the headers are imported correctly without
	// network sync. No peers specified – we want to test the import
	// functionality by itself.
	instance := "https://signet.block-dn.org/"
	blockHeadersSource := instance + "headers/import/latest"
	filterHeadersSource := instance + "filter-headers/import/latest"
	importConfig := neutrino.Config{
		DataDir:     tempDir,
		Database:    db,
		ChainParams: chaincfg.SigNetParams,
		HeadersImport: &neutrino.HeadersImportConfig{
			BlockHeadersSource:      blockHeadersSource,
			FilterHeadersSource:     filterHeadersSource,
			WriteBatchSizePerRegion: 32000,
		},
	}

	importSvc, err := neutrino.NewChainService(importConfig)
	require.NoError(t, err)

	// Start the import service.
	err = importSvc.Start(context.Background())
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		t.Logf("Is current: %v", importSvc.IsCurrent())
		return importSvc.IsCurrent()
	}, time.Minute, time.Second)

	err = importSvc.Stop()
	require.NoError(t, err)
}
