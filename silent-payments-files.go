package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	sp "github.com/btcsuite/btcd/silentpayments"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	DefaultSPTweaksPerFile = 2_000

	DefaultRegtestSPTweaksPerFile = 2_000

	SPTweakFileDir = "silentpayments"

	SPTweakFileSuffix             = ".sptweak"
	SPTweakFileNamePattern        = "%s/block-%07d-%07d.sptweak"
	SPTweakFileNameExtractPattern = "block-[0-9]{7}-([0-9]{7})\\.sptweak"
)

var (
	// TaprootActivationHeights maps each supported Bitcoin network to its
	// respective Taproot activation height.
	TaprootActivationHeights = map[wire.BitcoinNet]int32{
		chaincfg.MainNetParams.Net:  709_632,
		chaincfg.TestNet3Params.Net: 2_011_968,
		chaincfg.TestNet4Params.Net: 1,
		chaincfg.SigNetParams.Net:   1,
	}

	spTweakFileNameExtractRegex = regexp.MustCompile(
		SPTweakFileNameExtractPattern,
	)
)

type spTweakFiles struct {
	producerBase

	tweakData map[int32]map[int32]*btcec.PublicKey

	// heightToHash mirrors cFilterFiles: see the comment there. SP
	// tweak entries are keyed by height and don't carry a block hash,
	// so we record it explicitly during ingest.
	heightToHash map[int32]chainhash.Hash

	// blockFetcher fetches blocks along with the previous output
	// scripts spent by them, prefetching upcoming heights in the
	// background.
	blockFetcher *blockPrefetcher

	// numBlocksIndexed counts the blocks indexed since the last stats
	// log line, which we emit whenever a file is sealed. Only accessed
	// from the producer goroutine, so no locking is required.
	numBlocksIndexed int32
	statsLastReset   time.Time

	// taprootStartHeight is the activation height for the current
	// network, or a sentinel when Taproot isn't supported. Set once at
	// construction so per-block ingest doesn't have to re-look it up.
	taprootStartHeight int32
	taprootSupported   bool
}

func newSPTweakFiles(itemsPerFile, reOrgSafeDepth int32,
	chain *rpcclient.Client, quit <-chan struct{}, baseDir string,
	chainParams *chaincfg.Params, h2hCache *heightToHashCache,
	blockFetcher *blockPrevOutFetcher) *spTweakFiles {

	taprootStartHeight, taprootSupported := TaprootActivationHeights[chainParams.Net]

	s := &spTweakFiles{
		tweakData:    make(map[int32]map[int32]*btcec.PublicKey),
		heightToHash: make(map[int32]chainhash.Hash),
		blockFetcher: newBlockPrefetcher(
			blockFetcher, h2hCache,
		),
		statsLastReset:     time.Now(),
		taprootStartHeight: taprootStartHeight,
		taprootSupported:   taprootSupported,
	}
	s.producerBase = producerBase{
		quit:           quit,
		chain:          chain,
		h2hCache:       h2hCache,
		chainParams:    chainParams,
		name:           "SP tweak data",
		baseDir:        baseDir,
		subDir:         SPTweakFileDir,
		fileSuffix:     SPTweakFileSuffix,
		extractRegex:   spTweakFileNameExtractRegex,
		entriesPerFile: itemsPerFile,
		reOrgSafeDepth: reOrgSafeDepth,
		hooks: producerHooks{
			ingest:          s.ingest,
			writeSealedFile: s.writeSealedFile,
			pruneLocked:     s.pruneLocked,
			cachedHash:      s.cachedHash,
		},
	}
	s.lastSealedHeight.Store(-1)

	return s
}

// updateFiles shadows producerBase.updateFiles so we can short-circuit when
// the active network doesn't support Taproot — SP tweak indexing is a
// no-op in that case, but the server still expects the producer goroutine
// to exist and to be queryable. We log loudly and idle until shutdown.
func (s *spTweakFiles) updateFiles(numBlocks int32) error {
	if !s.taprootSupported {
		log.Warnf("Silent Payments tweak data indexing enabled, but "+
			"Taproot is not supported on network %s — idling",
			s.chainParams.Name)

		// Mark startup complete so /status etc. don't 503, and
		// wait for shutdown. We deliberately don't ingest anything
		// so the height-keyed endpoints will report missing data
		// for any requested height.
		s.startupComplete.Store(true)
		<-s.quit
		return errServerShutdown
	}

	return s.producerBase.updateFiles(numBlocks)
}

func (s *spTweakFiles) ingest(height int32) error {
	hash, err := s.h2hCache.getBlockHash(height)
	if err != nil {
		return fmt.Errorf("error getting block hash for height %d: %w",
			height, err)
	}

	s.Lock()
	s.tweakData[height] = make(map[int32]*btcec.PublicKey)
	s.heightToHash[height] = *hash
	s.Unlock()

	// Pre-Taproot blocks can't contain Silent Payment outputs, so we
	// only ingest the empty tweakData entry above so sealReadyFiles
	// can serialize a coherent JSON file.
	if height < s.taprootStartHeight {
		return nil
	}

	block, err := s.blockFetcher.fetchBlock(height, hash)
	if err != nil {
		return fmt.Errorf("error getting block for SP tweak data: %w",
			err)
	}
	s.numBlocksIndexed++

	if err := s.indexBlockSPTweakData(height, block); err != nil {
		return fmt.Errorf("error indexing SP tweak data: %w", err)
	}

	return nil
}

func (s *spTweakFiles) writeSealedFile(fileStart, fileEnd int32) error {
	spDir := filepath.Join(s.baseDir, s.subDir)
	fileName := fmt.Sprintf(SPTweakFileNamePattern, spDir, fileStart,
		fileEnd)

	if err := s.writeSPTweaks(fileName, fileStart, fileEnd); err != nil {
		return err
	}

	// Log the indexing throughput now that we've written another batch of
	// tweak data to disk, so the initial catch-up progress can be
	// observed.
	since := time.Since(s.statsLastReset).Seconds()
	log.Debugf("SP tweak data: indexed %d blocks in %.1fs (%.2f blocks "+
		"per second)", s.numBlocksIndexed, since,
		float64(s.numBlocksIndexed)/since)

	s.numBlocksIndexed = 0
	s.statsLastReset = time.Now()

	return nil
}

func (s *spTweakFiles) pruneLocked(start, end int32) {
	for j := start; j <= end; j++ {
		delete(s.tweakData, j)
		delete(s.heightToHash, j)
	}
}

func (s *spTweakFiles) cachedHash(height int32) (chainhash.Hash, bool) {
	s.RLock()
	defer s.RUnlock()

	hash, ok := s.heightToHash[height]
	return hash, ok
}

// indexBlockSPTweakData examines the given block for transactions that
// contain Taproot outputs. For each such transaction, we compute the Silent
// Payments tweak data and store it in the provided index map, keyed by the
// transaction index within the block.
func (s *spTweakFiles) indexBlockSPTweakData(height int32,
	block *blockWithPrevOuts) error {

	for txIndex, tx := range block.txs {
		// Skip coinbase transactions, they can't contain Silent Payment
		// outputs.
		if blockchain.IsCoinBaseTx(tx) {
			continue
		}

		// The tweak calculation resolves the previous output scripts
		// of all inputs from the data we got alongside the block, and
		// returns a nil key for transactions that can't contain
		// Silent Payment outputs.
		tweakPubKey, err := sp.TransactionTweakData(
			tx, block.fetchPrevOutScript, log,
		)
		if err != nil {
			return fmt.Errorf("error calculating SP tweak "+
				"data for tx index %d in block at height "+
				"%d: %w", txIndex, height, err)
		}

		if tweakPubKey == nil {
			continue
		}

		// Store the tweak data for this transaction index.
		s.Lock()
		s.tweakData[height][int32(txIndex)] = tweakPubKey
		s.Unlock()
	}

	return nil
}

func (s *spTweakFiles) writeSPTweaks(fileName string, startIndex,
	endIndex int32) error {

	s.RLock()
	defer s.RUnlock()

	log.Debugf("Writing SP tweak data file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	defer func() {
		err = file.Close()
		if err != nil {
			log.Errorf("Error closing file %s: %w", fileName, err)
		}
	}()

	return s.serializeSPTweakDataLocked(file, startIndex, endIndex)
}

func (s *spTweakFiles) serializeSPTweakData(w io.Writer, startIndex,
	endIndex int32) error {

	s.RLock()
	defer s.RUnlock()

	return s.serializeSPTweakDataLocked(w, startIndex, endIndex)
}

func (s *spTweakFiles) serializeSPTweakDataLocked(w io.Writer, startIndex,
	endIndex int32) error {

	// We need to add plus one here since the end index is inclusive in the
	// for loop below (j <= endIndex).
	numBlocks := endIndex - startIndex + 1

	spTweakFile := &SPTweakFile{
		StartHeight: startIndex,
		NumBlocks:   numBlocks,
		Blocks:      make([]SPTweakBlock, 0, numBlocks),
	}
	for j := startIndex; j <= endIndex; j++ {
		transactionTweaks, ok := s.tweakData[j]
		if !ok {
			return fmt.Errorf("invalid height %d", j)
		}

		if len(transactionTweaks) == 0 {
			spTweakFile.Blocks = append(spTweakFile.Blocks, nil)

			continue
		}

		txIndexes := slices.Collect(maps.Keys(transactionTweaks))
		slices.Sort(txIndexes)

		block := make(SPTweakBlock, len(transactionTweaks))
		for _, txIndex := range txIndexes {
			pubKey := transactionTweaks[txIndex]
			if pubKey == nil {
				return fmt.Errorf("nil pubkey for height %d "+
					"tx index %d", j, txIndex)
			}

			block[txIndex] = hex.EncodeToString(
				pubKey.SerializeCompressed(),
			)
		}
		spTweakFile.Blocks = append(spTweakFile.Blocks, block)
	}

	err := json.NewEncoder(w).Encode(spTweakFile)
	if err != nil {
		return fmt.Errorf("error writing SP tweak data: %w", err)
	}

	return nil
}
