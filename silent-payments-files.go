package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	sp "github.com/btcsuite/btcd/btcutil/silentpayments"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnutils"
)

const (
	DefaultSPTweaksPerFile = 2_000

	DefaultRegtestSPTweaksPerFile = 2_000

	SPTweakFileDir = "silentpayments"

	SPTweakFileSuffix             = ".sptweak"
	SPTweakFileNamePattern        = "%s/block-%07d-%07d.sptweak"
	SPTweakFileNameExtractPattern = "block-[0-9]{7}-([0-9]{7})\\.sptweak"

	BlockFilePattern = "%s/blk%05d.dat"
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

type prevOutCache struct {
	numBlock   int32
	numTx      int32
	numTrTx    int32
	numTrHit   int32
	numTxFetch int32

	lastReset time.Time

	chain *rpcclient.Client

	cache lnutils.SyncMap[wire.OutPoint, []byte]
}

func newPrevOutCache(chain *rpcclient.Client) *prevOutCache {
	return &prevOutCache{
		chain:     chain,
		lastReset: time.Now(),
		cache:     lnutils.SyncMap[wire.OutPoint, []byte]{},
	}
}

func (p *prevOutCache) LogAndReset() {
	since := time.Since(p.lastReset).Seconds()
	blockPerSecond := float64(p.numBlock) / since

	log.Tracef("SP PrevOutCache: fetched %d previous txs (%d cache hits) "+
		"for %d txns (%d with taproot outputs), cache size is %d, "+
		"%.2f blocks per second", p.numTxFetch, p.numTrHit, p.numTx,
		p.numTrTx, p.cache.Len(), blockPerSecond)

	// Reset stats.
	p.numBlock = 0
	p.numTx = 0
	p.numTrTx = 0
	p.numTrHit = 0
	p.numTxFetch = 0

	p.lastReset = time.Now()
}

// FetchPreviousOutputScript fetches the previous output's pkScript for the
// given outpoint.
func (p *prevOutCache) FetchPreviousOutputScript(op wire.OutPoint) ([]byte,
	error) {

	// We assume we only visit every transaction once, so each previous
	// output we fetch is only fetched a single time. Thus, we can delete it
	// from the cache after fetching it.
	pkScript, ok := p.cache.LoadAndDelete(op)
	if ok {
		p.numTrHit++

		return pkScript, nil
	}

	tx, err := p.chain.GetRawTransaction(&op.Hash)
	if err != nil {
		return nil, fmt.Errorf("error fetching previous "+
			"transaction: %w", err)
	}

	p.numTxFetch++

	if int(op.Index) >= len(tx.MsgTx().TxOut) {
		return nil, fmt.Errorf("output index %d out of range for "+
			"transaction %s", op.Index, op.Hash.String())
	}

	pkScript = tx.MsgTx().TxOut[op.Index].PkScript
	p.cache.Store(op, pkScript)

	return pkScript, nil
}

func (p *prevOutCache) AddOutputs(tx *wire.MsgTx) {
	txHash := tx.TxHash()
	for txIndex, txOut := range tx.TxOut {
		if !txscript.IsPayToTaproot(txOut.PkScript) {
			continue
		}

		p.cache.Store(wire.OutPoint{
			Hash:  txHash,
			Index: uint32(txIndex),
		}, txOut.PkScript)
	}
}

func (p *prevOutCache) RemoveInputs(tx *wire.MsgTx) {
	for _, txIn := range tx.TxIn {
		p.cache.Delete(txIn.PreviousOutPoint)
	}
}

type spTweakFiles struct {
	sync.RWMutex

	quit <-chan struct{}

	blocksPerFile int32
	baseDir       string
	chain         *rpcclient.Client
	chainParams   *chaincfg.Params
	h2hCache      *heightToHashCache

	startupComplete atomic.Bool
	currentHeight   atomic.Int32

	tweakData map[int32]map[int32]*btcec.PublicKey

	prevOutCache *prevOutCache
}

func newSPTweakFiles(itemsPerFile int32, chain *rpcclient.Client,
	quit <-chan struct{}, baseDir string, chainParams *chaincfg.Params,
	h2hCache *heightToHashCache) *spTweakFiles {

	c := &spTweakFiles{
		quit:          quit,
		blocksPerFile: itemsPerFile,
		baseDir:       baseDir,
		chain:         chain,
		chainParams:   chainParams,
		h2hCache:      h2hCache,
		prevOutCache:  newPrevOutCache(chain),
	}
	c.clearData()

	return c
}

func (s *spTweakFiles) isStartupComplete() bool {
	return s.startupComplete.Load()
}

func (s *spTweakFiles) getCurrentHeight() int32 {
	return s.currentHeight.Load()
}

func (s *spTweakFiles) clearData() {
	s.Lock()
	defer s.Unlock()

	s.tweakData = make(
		map[int32]map[int32]*btcec.PublicKey, s.blocksPerFile,
	)
}

func (s *spTweakFiles) updateFiles(targetHeight int32) error {
	log.Debugf("Updating SP tweak data in %s for network %s", s.baseDir,
		s.chainParams.Name)

	spDir := filepath.Join(s.baseDir, SPTweakFileDir)
	err := os.MkdirAll(spDir, DirectoryMode)
	if err != nil {
		return fmt.Errorf("error creating directory %s: %w", spDir, err)
	}

	lastBlock, err := lastFile(
		spDir, SPTweakFileSuffix, spTweakFileNameExtractRegex,
	)
	if err != nil {
		return fmt.Errorf("error getting last SP tweak data file: %w",
			err)
	}

	// If we already had some blocks written, then we need to start from
	// the next block.
	startBlock := lastBlock
	if lastBlock > 0 {
		startBlock++
	}

	log.Debugf("Writing SP tweak data files from block %d to block %d",
		startBlock, targetHeight)
	err = s.updateCacheAndFiles(startBlock, targetHeight)
	if err != nil {
		return fmt.Errorf("error updating blocks: %w", err)
	}

	// Allow serving requests now that we're caught up.
	s.startupComplete.Store(true)

	// Let's now go into the infinite loop of updating the filter files
	// whenever a new block is mined.
	log.Debugf("Caught up SP tweak data to best block %d, starting to "+
		"poll for new blocks", targetHeight)
	for {
		select {
		case <-time.After(blockPollInterval):
		case <-s.quit:
			return errServerShutdown
		}

		height, err := s.chain.GetBlockCount()
		if err != nil {
			return fmt.Errorf("error getting best block: %w", err)
		}

		currentBlock := s.currentHeight.Load()
		if int32(height) == currentBlock {
			continue
		}

		log.Infof("Processing SP tweak data for new block mined at "+
			"height %d", height)
		err = s.updateCacheAndFiles(currentBlock+1, int32(height))
		if err != nil {
			return fmt.Errorf("error updating SP tweak data for "+
				"blocks: %w", err)
		}
	}
}

func (s *spTweakFiles) updateCacheAndFiles(startBlock, endBlock int32) error {
	spDir := filepath.Join(s.baseDir, SPTweakFileDir)
	net := s.chainParams.Net
	taprootStartHeight, taprootSupported := TaprootActivationHeights[net]

	if !taprootSupported {
		log.Warnf("Silent Payments tweak data indexing enabled, "+
			"but Taproot is not supported on network %s",
			s.chainParams.Name)

		return nil
	}

	// Generate the silent payment tweak data as requested.
	for i := startBlock; i <= endBlock; i++ {
		// Were we interrupted?
		select {
		case <-s.quit:
			return errServerShutdown
		default:
		}

		s.Lock()
		s.tweakData[i] = make(map[int32]*btcec.PublicKey)
		s.Unlock()

		// We don't look at blocks before Taproot activation, as Silent
		// Payments require Taproot.
		if i >= taprootStartHeight {
			blockHash, err := s.h2hCache.getBlockHash(i)
			if err != nil {
				return fmt.Errorf("error getting block hash "+
					"for height %d: %w", i, err)
			}

			block, err := s.chain.GetBlock(blockHash)
			if err != nil {
				return fmt.Errorf("error getting block for SP "+
					"tweak data: %w", err)
			}
			s.prevOutCache.numBlock++

			err = s.indexBlockSPTweakData(i, block)
			if err != nil {
				return fmt.Errorf("error indexing SP tweak "+
					"data: %w", err)
			}
		}

		if (i+1)%s.blocksPerFile == 0 {
			fileStart := i - s.blocksPerFile + 1
			spTweakFileName := fmt.Sprintf(
				SPTweakFileNamePattern, spDir, fileStart, i,
			)

			log.Debugf("Reached SP tweak data height %d, writing"+
				"file starting at %d, containing %d items to "+
				"%s", i, fileStart, s.blocksPerFile,
				spTweakFileName)

			err := s.writeSPTweaks(spTweakFileName, fileStart, i)
			if err != nil {
				return fmt.Errorf("error writing SP tweak "+
					"data: %w", err)
			}

			s.prevOutCache.LogAndReset()
			s.clearData()
		}

		s.currentHeight.Store(i)
	}

	return nil
}

// indexBlockSPTweakData examines the given block for transactions that
// contain Taproot outputs. For each such transaction, we compute the Silent
// Payments tweak data and store it in the provided index map, keyed by the
// transaction index within the block.
func (s *spTweakFiles) indexBlockSPTweakData(height int32,
	block *wire.MsgBlock) error {

	for txIndex, tx := range block.Transactions {
		s.prevOutCache.AddOutputs(tx)
		s.prevOutCache.numTx++

		// Only transactions with Taproot outputs can have Silent
		// Payments.
		if !sp.HasTaprootOutputs(tx) {
			s.prevOutCache.RemoveInputs(tx)

			continue
		}
		s.prevOutCache.numTrTx++

		tweakPubKey, err := sp.TransactionTweakData(
			tx, s.prevOutCache.FetchPreviousOutputScript, log,
		)
		if err != nil {
			s.prevOutCache.RemoveInputs(tx)

			return fmt.Errorf("error calculating SP tweak "+
				"data for tx index %d in block at height "+
				"%d: %w", txIndex, height, err)
		}

		s.prevOutCache.RemoveInputs(tx)
		if tweakPubKey == nil {
			s.prevOutCache.RemoveInputs(tx)
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

	err = json.NewEncoder(file).Encode(spTweakFile)
	if err != nil {
		return fmt.Errorf("error writing SP tweak data to file %s: %w",
			fileName, err)
	}

	return nil
}
