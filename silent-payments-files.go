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
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightninglabs/neutrino/cache/lru"
)

const (
	DefaultSPTweaksPerFile = 2_000

	DefaultRegtestSPTweaksPerFile = 2_000

	// DefaultPrevOutCacheMiBytes is the default size we allow the Taproot
	// previous output script cache to grow to. This is a hard limit as
	// we're using an LRU cache, so once we reach this limit, we will evict
	// the least recently used entry from the cache for each new entry we
	// add. We set this to 1GiB by default, to make sure systems with
	// limited resources can still use the SP tweak data indexing without
	// running out of memory.
	DefaultPrevOutCacheMiBytes = 1024

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

// byteCacheEntry is a simple wrapper around a byte slice that implements the
// cache.Value interface, which allows us to store previous output scripts in
// the LRU cache.
type byteCacheEntry []byte

// Size determines how big this entry is in the cache. We need to account for
// the size of the key (the outpoint) as well, which is 36 bytes (32 for the
// txid and 4 for the index).
// nolint:unparam
func (b byteCacheEntry) Size() (uint64, error) {
	return uint64(len(b)) + 36, nil
}

type prevOutCache struct {
	numBlock   int32
	numTx      int32
	numTrTx    int32
	numTrHit   int32
	numTxFetch int32

	enabled bool

	lastReset time.Time

	chain *rpcclient.Client

	trPrevOutCache *lru.Cache[wire.OutPoint, byteCacheEntry]
}

func newPrevOutCache(chain *rpcclient.Client,
	prevOutCacheSizeMiB uint16) *prevOutCache {

	return &prevOutCache{
		enabled:   true,
		chain:     chain,
		lastReset: time.Now(),
		trPrevOutCache: lru.NewCache[wire.OutPoint, byteCacheEntry](
			uint64(prevOutCacheSizeMiB) * 1024 * 1024,
		),
	}
}

func (p *prevOutCache) LogAndReset() {
	since := time.Since(p.lastReset).Seconds()
	blockPerSecond := float64(p.numBlock) / since

	log.Tracef("SP PrevOutCache: fetched %d previous txs (%d cache hits) "+
		"for %d txns (%d with taproot outputs), cache size is %d, "+
		"%.2f blocks per second", p.numTxFetch, p.numTrHit, p.numTx,
		p.numTrTx, p.trPrevOutCache.Len(), blockPerSecond)

	// Reset stats.
	p.numBlock = 0
	p.numTx = 0
	p.numTrTx = 0
	p.numTrHit = 0
	p.numTxFetch = 0

	p.lastReset = time.Now()
}

func (p *prevOutCache) Disable() {
	p.enabled = false
	p.trPrevOutCache = lru.NewCache[wire.OutPoint, byteCacheEntry](0)
	p.LogAndReset()
}

// FetchPreviousOutputScript fetches the previous output's pkScript for the
// given outpoint.
func (p *prevOutCache) FetchPreviousOutputScript(op wire.OutPoint) ([]byte,
	error) {

	// We assume we only visit every transaction once, so each previous
	// output we fetch is only fetched a single time. Thus, we can delete it
	// from the cache after fetching it.
	pkScript, ok := p.trPrevOutCache.LoadAndDelete(op)
	if ok {
		p.numTrHit++

		return pkScript, nil
	}

	tx, err := p.chain.GetRawTransaction(&op.Hash)
	if err != nil {
		return nil, fmt.Errorf("error fetching previous transaction: "+
			"%w", err)
	}

	p.numTxFetch++

	if int(op.Index) >= len(tx.MsgTx().TxOut) {
		return nil, fmt.Errorf("output index %d out of range for "+
			"transaction %s", op.Index, op.Hash.String())
	}

	pkScript = tx.MsgTx().TxOut[op.Index].PkScript
	if p.enabled {
		_, _ = p.trPrevOutCache.Put(op, pkScript)
	}

	return pkScript, nil
}

func (p *prevOutCache) AddOutputs(tx *wire.MsgTx) {
	if !p.enabled {
		return
	}

	txHash := tx.TxHash()
	for txIndex, txOut := range tx.TxOut {
		if !txscript.IsPayToTaproot(txOut.PkScript) {
			continue
		}

		_, _ = p.trPrevOutCache.Put(wire.OutPoint{
			Hash:  txHash,
			Index: uint32(txIndex),
		}, txOut.PkScript)
	}
}

func (p *prevOutCache) RemoveInputs(tx *wire.MsgTx) {
	if !p.enabled {
		return
	}

	for _, txIn := range tx.TxIn {
		p.trPrevOutCache.Delete(txIn.PreviousOutPoint)
	}
}

type spTweakFiles struct {
	producerBase

	tweakData map[int32]map[int32]*btcec.PublicKey

	// heightToHash mirrors cFilterFiles: see the comment there. SP
	// tweak entries are keyed by height and don't carry a block hash,
	// so we record it explicitly during ingest.
	heightToHash map[int32]chainhash.Hash

	prevOutCache *prevOutCache

	// taprootStartHeight is the activation height for the current
	// network, or a sentinel when Taproot isn't supported. Set once at
	// construction so per-block ingest doesn't have to re-look it up.
	taprootStartHeight int32
	taprootSupported   bool
}

func newSPTweakFiles(itemsPerFile, reOrgSafeDepth int32,
	chain *rpcclient.Client, quit <-chan struct{}, baseDir string,
	chainParams *chaincfg.Params, h2hCache *heightToHashCache,
	prevOutCacheSizeMiB uint16) *spTweakFiles {

	taprootStartHeight, taprootSupported := TaprootActivationHeights[chainParams.Net]

	s := &spTweakFiles{
		tweakData:          make(map[int32]map[int32]*btcec.PublicKey),
		heightToHash:       make(map[int32]chainhash.Hash),
		prevOutCache:       newPrevOutCache(chain, prevOutCacheSizeMiB),
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
			afterCatchUp:    s.prevOutCache.Disable,
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

	block, err := s.chain.GetBlock(hash)
	if err != nil {
		return fmt.Errorf("error getting block for SP tweak data: %w",
			err)
	}
	s.prevOutCache.numBlock++

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

	// Log + reset the prev-out cache stats now that we've written
	// another batch of tweak data to disk. (The cache itself gets
	// fully released by the afterCatchUp hook once the initial
	// catch-up completes.)
	s.prevOutCache.LogAndReset()
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
	block *wire.MsgBlock) error {

	for txIndex, tx := range block.Transactions {
		// Skip coinbase transactions, they can't contain Silent Payment
		// outputs.
		if blockchain.IsCoinBaseTx(tx) {
			continue
		}

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
