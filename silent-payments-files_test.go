package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/v2/gcs"
	"github.com/btcsuite/btcd/btcutil/v2/gcs/builder"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	sp "github.com/btcsuite/btcd/silentpayments"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/btcsuite/btcwallet/chain"
	"github.com/btcsuite/btcwallet/waddrmgr"
	basewallet "github.com/btcsuite/btcwallet/wallet"
	"github.com/btcsuite/btcwallet/walletdb"
	"github.com/lightningnetwork/lnd/blockcache"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/require"
)

const (
	tweakBlocksPerFile = 100
)

var (
	pollInterval = 10 * time.Millisecond

	// seedBytes is the raw entropy of the aezeed:
	//   able promote dizzy mixture sword myth share public find tattoo
	//   catalog cousin bulb unfair machine alarm cool large promote kick
	//   shop rug mean year
	// Which corresponds to the master root key:
	//   xprv9s21ZrQH143K2KADjED57FvNbptdKLp4sqKzssegwEGKQMGoDkbyhUeCKe5m3A
	//   MU44z4vqkmGswwQVKrv599nFG16PPZDEkNrogwoDGeCmZ
	seedBytes, _ = hex.DecodeString("4a7611b6979ba7c4bc5c5cd2239b2973")

	addrTypes = []lnwallet.AddressType{
		lnwallet.WitnessPubKey,
		lnwallet.NestedWitnessPubKey,
		lnwallet.TaprootPubkey,
	}

	spKeyScope = waddrmgr.KeyScope{
		Purpose: 352,
		Coin:    testParams.HDCoinType,
	}
	spKeyScopeSchema = waddrmgr.ScopeAddrSchema{
		ExternalAddrType: waddrmgr.TaprootPubKey,
		InternalAddrType: waddrmgr.TaprootPubKey,
	}

	// waddrmgrNamespaceKey is the namespace key that the waddrmgr state is
	// stored within the top-level waleltdb buckets of btcwallet.
	waddrmgrNamespaceKey = []byte("waddrmgr")
)

func TestSPTweakDataFilesUpdate(t *testing.T) {
	// Activate Taproot for regtest.
	TaprootActivationHeights[chaincfg.RegressionNetParams.Net] = 1

	miner, backend, backendCfg, _ := setupBackend(t)

	// Mine initial blocks. The miner starts with 200 blocks already mined.
	_ = miner.MineEmptyBlocks(initialBlocks - int(totalStartupBlocks))

	// Wait until the backend is fully synced to the miner.
	waitBackendSync(t, backend, miner)

	// First run: start from scratch. A leftover file of the legacy JSON
	// format must be ignored (with a warning) and not interfere with the
	// producer's resume logic.
	dataDir := t.TempDir()
	spDir := filepath.Join(dataDir, SPTweakFileDir)
	require.NoError(t, os.MkdirAll(spDir, DirectoryMode))
	legacyFile := fmt.Sprintf(SPTweakFileNamePattern, spDir, 0, 1999)
	require.NoError(t, os.WriteFile(legacyFile, []byte("{}"), 0644))

	quit := make(chan struct{})
	h2hCache := newH2HCache(backend)
	hf := newSPTweakFiles(
		tweakBlocksPerFile, testReOrgSafeDepth, backend, quit, dataDir,
		&testParams, h2hCache,
		newBlockPrevOutFetcher(backend, &backendCfg),
	)

	var wg sync.WaitGroup

	// Wait for the initial blocks to be written.
	waitForTargetHeight(t, &wg, hf, initialBlocks)

	// The startup-era blocks contain no Silent Payments eligible
	// transactions, so every sealed file is the self-describing header
	// plus one zero count byte per block, for every dust filter level.
	checkSPTweakDataFiles(t, dataDir, 0, 99)
	checkSPTweakDataFiles(t, dataDir, 100, 199)
	checkSPTweakDataFiles(t, dataDir, 200, 299)
	checkSPTweakDataFiles(t, dataDir, 300, 399)

	// The legacy file must still be there, untouched.
	checkFile(t, legacyFile, 2)

	// Stop the service.
	close(quit)
	wg.Wait()

	// Second run: restart and continue.
	const finalBlocks = 550
	_ = miner.MineEmptyBlocks(finalBlocks - initialBlocks)

	// Wait until the backend is fully synced to the miner.
	waitBackendSync(t, backend, miner)

	quit = make(chan struct{})
	hf = newSPTweakFiles(
		tweakBlocksPerFile, testReOrgSafeDepth, backend, quit, dataDir,
		&testParams, h2hCache,
		newBlockPrevOutFetcher(backend, &backendCfg),
	)

	// Wait for the final blocks to be written.
	waitForTargetHeight(t, &wg, hf, finalBlocks)

	// Check the new file of every dust filter level.
	checkSPTweakDataFiles(t, dataDir, 400, 499)

	// Stop the service.
	close(quit)
	wg.Wait()
}

// checkSPTweakDataFiles asserts that the sealed SP tweak data file covering
// [start, end] exists for every dust filter level, with the size of a file
// of empty blocks: the self-describing header plus one zero count byte per
// block.
func checkSPTweakDataFiles(t *testing.T, dataDir string, start, end int32) {
	t.Helper()

	emptySize := int64(spTweakFileHeaderSize + tweakBlocksPerFile)
	for _, dustLimit := range spTweakDustLimits {
		spDir := filepath.Join(dataDir, spTweakDustDir(dustLimit))
		checkFile(t, fmt.Sprintf(
			SPTweakFileNamePattern, spDir, start, end,
		), emptySize)
	}
}

func TestSilentPaymentsDetection(t *testing.T) {
	// Activate Taproot for regtest.
	TaprootActivationHeights[chaincfg.RegressionNetParams.Net] = 1

	miner, backend, backendCfg, bitcoindCfg := setupBackend(t)
	wallet, scopeMgr := newTestWallet(
		t, &testParams, bitcoindCfg, seedBytes,
	)

	// Mine initial blocks. The miner starts with 200 blocks already mined.
	_ = miner.MineEmptyBlocks(initialBlocks - int(totalStartupBlocks))

	// Wait until the backend is fully synced to the miner.
	waitBackendSync(t, backend, miner)

	// First run: start from scratch.
	dataDir := t.TempDir()
	quit := make(chan struct{})
	h2hCache := newH2HCache(backend)
	hf := newSPTweakFiles(
		tweakBlocksPerFile, testReOrgSafeDepth, backend, quit, dataDir,
		&testParams, h2hCache,
		newBlockPrevOutFetcher(backend, &backendCfg),
	)

	// Wait for the initial blocks to be written.
	var wg sync.WaitGroup
	waitForTargetHeight(t, &wg, hf, initialBlocks)

	// Fund an address of each type.
	for _, addrType := range addrTypes {
		addr, err := wallet.NewAddress(
			addrType, false, lnwallet.DefaultAccountName,
		)
		require.NoError(t, err)

		pkScript, err := txscript.PayToAddrScript(addr)
		require.NoError(t, err)

		t.Logf("Sending output %x (addr %s)", pkScript, addr.String())

		miner.SendOutput(&wire.TxOut{
			Value:    100_000,
			PkScript: pkScript,
		}, 2)
	}

	// Mine a block to confirm the funding transactions.
	miner.MineBlocksAndAssertNumTxes(1, len(addrTypes))
	waitBackendSync(t, backend, miner)
	waitForTargetHeight(t, &wg, hf, initialBlocks+1)

	var utxos []*lnwallet.Utxo
	err := wait.NoError(func() error {
		var err error
		utxos, err = wallet.ListUnspentWitness(1, math.MaxInt32, "")
		if err != nil {
			return fmt.Errorf("listing utxos: %w", err)
		}

		if len(utxos) != len(addrTypes) {
			return fmt.Errorf("expected %d utxos; got %d",
				len(addrTypes), len(utxos))
		}

		return nil
	}, shortTimeout)
	require.NoError(t, err)

	scanKey, scanPrivKey := deriveNextSPKey(t, wallet, scopeMgr, false)
	spendKey, _ := deriveNextSPKey(t, wallet, scopeMgr, true)
	spAddr := sp.NewAddress(sp.TestNetHRP, *scanKey, *spendKey, nil)

	tx := wire.NewMsgTx(2)
	tx.TxOut = append(tx.TxOut, &wire.TxOut{
		Value:    250_000,
		PkScript: psbt.SilentPaymentDummyP2TROutput,
	})

	pkt, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)

	pkt.Outputs[0].SilentPaymentInfo = &psbt.SilentPaymentInfo{
		ScanKey:  scanKey.SerializeCompressed(),
		SpendKey: spendKey.SerializeCompressed(),
	}

	changeIndex, err := wallet.FundPsbt(
		pkt, 0, chainfee.FeePerKwFloor, lnwallet.DefaultAccountName,
		nil, basewallet.CoinSelectionLargest, nil,
	)
	require.NoError(t, err)

	_, err = wallet.SignPsbt(pkt)
	require.NoError(t, err)

	err = psbt.MaybeFinalizeAll(pkt)
	require.NoError(t, err)

	finalTx, err := psbt.Extract(pkt)
	require.NoError(t, err)

	err = wallet.PublishTransaction(finalTx, "silent payments test")
	require.NoError(t, err)

	// Mine a block to confirm the transaction.
	minedBlocks := miner.MineBlocksAndAssertNumTxes(1, 1)

	spHeight := int32(initialBlocks + 2)
	waitBackendSync(t, backend, miner)
	waitForTargetHeight(t, &wg, hf, spHeight)

	blockData := hf.tweakData[spHeight]

	// The tweak data for the block should have exactly one entry, since the
	// coinbase transaction isn't a silent payment. The entry's max output
	// value must reflect the 250k sat taproot output, so the tweak is
	// present in every dust filter level.
	require.Len(t, blockData, 1)
	require.GreaterOrEqual(
		t, blockData[0].maxValue, uint64(250_000),
	)

	txData, err := btcec.ParsePubKey(blockData[0].tweak[:])
	require.NoError(t, err)

	spOutputKeys, err := sp.TransactionOutputKeysForFilter(
		*txData, []sp.ScanAddress{
			sp.NewScanAddress(*spAddr, *scanPrivKey),
		},
	)
	require.NoError(t, err)

	require.Len(t, spOutputKeys, 1)
	spOutputKey := spOutputKeys[0]

	txOut := finalTx.TxOut[len(finalTx.TxOut)-int(changeIndex)-1]
	txOutputKey, err := schnorr.ParsePubKey(txOut.PkScript[2:34])
	require.NoError(t, err)

	t.Logf("Derived output key: %x", spOutputKey.SerializeCompressed())
	t.Logf("Transaction output key: %x", txOutputKey.SerializeCompressed())

	require.Equal(
		t, schnorr.SerializePubKey(spOutputKey),
		schnorr.SerializePubKey(txOutputKey),
	)

	spBlockHash := minedBlocks[0].BlockHash()
	filter, err := backend.GetBlockFilter(spBlockHash, &filterBasic)
	require.NoError(t, err)

	filterBytes, err := hex.DecodeString(filter.Filter)
	require.NoError(t, err)

	cFilter, err := gcs.FromNBytes(
		builder.DefaultP, builder.DefaultM, filterBytes,
	)
	require.NoError(t, err)

	match, err := sp.MatchBlock(cFilter, &spBlockHash, spOutputKeys)
	require.NoError(t, err)
	require.True(t, match)
}

func newTestWallet(t *testing.T, netParams *chaincfg.Params,
	bitcoindConfig *chain.BitcoindConfig,
	seedBytes []byte) (*btcwallet.BtcWallet, *waddrmgr.ScopedKeyManager) {

	walletLogger := log.SubSystem("BTCW")
	walletLogger.SetLevel(btclog.LevelInfo)
	btcwallet.UseLogger(walletLogger)
	chain.UseLogger(walletLogger)
	basewallet.UseLogger(walletLogger)

	conn, err := chain.NewBitcoindConn(bitcoindConfig)
	require.NoError(t, err)

	err = conn.Start()
	require.NoError(t, err)

	loaderOpt := btcwallet.LoaderWithLocalWalletDB(
		t.TempDir(), false, time.Minute,
	)
	config := btcwallet.Config{
		PrivatePass:   []byte("some-pass"),
		HdSeed:        seedBytes,
		NetParams:     netParams,
		CoinType:      netParams.HDCoinType,
		ChainSource:   conn.NewBitcoindClient(),
		LoaderOptions: []btcwallet.LoaderOption{loaderOpt},
	}
	blockCache := blockcache.NewBlockCache(10000)
	w, err := btcwallet.New(config, blockCache)
	require.NoError(t, err)

	err = w.Start()
	require.NoError(t, err)

	t.Cleanup(func() {
		err := w.Stop()
		require.NoError(t, err)
	})

	// Add the Silent Payments key scope to the wallet.
	scopeMgr, err := w.InternalWallet().AddScopeManager(
		spKeyScope, spKeyScopeSchema,
	)
	require.NoError(t, err)

	err = w.InternalWallet().InitAccounts(scopeMgr, false, 1)
	require.NoError(t, err)

	_, err = w.SubscribeTransactions()
	require.NoError(t, err)

	return w, scopeMgr
}

func deriveNextSPKey(t *testing.T, wallet *btcwallet.BtcWallet,
	scopeMgr *waddrmgr.ScopedKeyManager, spendKey bool) (*btcec.PublicKey,
	*btcec.PrivateKey) {

	var (
		pubKey  *btcec.PublicKey
		privKey *btcec.PrivateKey
	)

	db := wallet.InternalWallet().Database()
	err := walletdb.Update(db, func(tx walletdb.ReadWriteTx) error {
		addrmgrNs := tx.ReadWriteBucket(waddrmgrNamespaceKey)

		var (
			addrs []waddrmgr.ManagedAddress
			err   error
		)

		if spendKey {
			addrs, err = scopeMgr.NextExternalAddresses(
				addrmgrNs, 0, 1,
			)
		} else {
			addrs, err = scopeMgr.NextInternalAddresses(
				addrmgrNs, 0, 1,
			)
		}
		if err != nil {
			return err
		}

		addr, ok := addrs[0].(waddrmgr.ManagedPubKeyAddress)
		if !ok {
			return fmt.Errorf("address is not a managed pubkey " +
				"addr")
		}

		pubKey = addr.PubKey()
		privKey, err = addr.PrivKey()
		return err
	})
	require.NoError(t, err)

	return pubKey, privKey
}
