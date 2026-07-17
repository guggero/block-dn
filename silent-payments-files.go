package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	sp "github.com/btcsuite/btcd/silentpayments"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	DefaultSPTweaksPerFile = 2_000

	DefaultRegtestSPTweaksPerFile = 2_000

	SPTweakFileDir = "silentpayments"

	SPTweakFileSuffix             = ".sptweak"
	SPTweakFileNamePattern        = "%s/block-%07d-%07d.sptweak"
	SPTweakFileNameExtractPattern = "block-[0-9]{7}-([0-9]{7})\\.sptweak"

	// spTweakFileVersion is the current version of the binary SP tweak
	// data file format.
	spTweakFileVersion = byte(0)

	// spTweakFileHeaderSize is the size of the self-describing metadata
	// prefix of an SP tweak data file: 4 bytes network magic (uint32
	// little endian), 1 byte format version, 1 byte file type, 4 bytes
	// start height (uint32 little endian) and 8 bytes dust limit (uint64
	// little endian).
	spTweakFileHeaderSize = 4 + 1 + 1 + 4 + 8
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

	// spTweakDustLimits are the dust filter levels the SP tweak data is
	// materialized and served at, in satoshis. A transaction's tweak is
	// included in a level if the largest value among the transaction's
	// taproot outputs is strictly greater than the level's value; level
	// zero therefore contains every tweak. The non-zero values are
	// thresholds observed in real-world usage: 600 and 1000 sats cover
	// the typical inscription "postage" outputs (330-546 sats) and 3750
	// sats is the 90th percentile of the taproot UTXO value
	// distribution.
	spTweakDustLimits = []uint64{0, 600, 1000, 3750}

	spTweakFileNameExtractRegex = regexp.MustCompile(
		SPTweakFileNameExtractPattern,
	)
)

// spTweakDustDir returns the directory (relative to the base directory) the
// SP tweak data files of the given dust limit are stored in.
func spTweakDustDir(dustLimit uint64) string {
	return filepath.Join(SPTweakFileDir, fmt.Sprintf("dust-%d", dustLimit))
}

// isValidSPTweakDustLimit returns whether the given value is one of the dust
// filter levels the SP tweak data is materialized at.
func isValidSPTweakDustLimit(dustLimit uint64) bool {
	return slices.Contains(spTweakDustLimits, dustLimit)
}

// writeSPTweakFileHeader writes the metadata prefix of an SP tweak data
// file, which makes every file (and range response) self-describing: the
// network it belongs to, the format version, the file type, the height of
// the first block it covers and the dust limit its tweaks are filtered by.
func writeSPTweakFileHeader(w io.Writer, net wire.BitcoinNet,
	startHeight int32, dustLimit uint64) error {

	var header [spTweakFileHeaderSize]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(net))
	header[4] = spTweakFileVersion
	header[5] = typeSPTweaks
	binary.LittleEndian.PutUint32(header[6:10], uint32(startHeight))
	binary.LittleEndian.PutUint64(header[10:18], dustLimit)

	_, err := w.Write(header[:])
	return err
}

// spTweakEntry is the tweak key of a single Silent Payments eligible
// transaction, together with the largest value among the transaction's
// taproot outputs, which the dust filter levels select on.
type spTweakEntry struct {
	tweak    [33]byte
	maxValue uint64
}

type spTweakFiles struct {
	producerBase

	// tweakData holds, per unsealed height, the tweak entries of the
	// block's Silent Payments eligible transactions, in transaction
	// order. Heights without any eligible transactions hold a nil slice;
	// the key still exists so serialization can tell "empty block" from
	// "height not ingested".
	tweakData map[int32][]spTweakEntry

	// heightToHash mirrors cFilterFiles: see the comment there. SP
	// tweak entries are keyed by height and don't carry a block hash,
	// so we record it explicitly during ingest.
	heightToHash map[int32]chainhash.Hash

	// blockFetcher fetches blocks along with the previous output
	// scripts spent by them, prefetching upcoming heights in the
	// background.
	blockFetcher *blockPrefetcher

	// numBlocksIndexed counts the blocks indexed since the last stats
	// log line, which we emit whenever a file is sealed. Together with
	// the fetch/compute split below it shows where the indexing time
	// goes. Only accessed from the producer goroutine, so no locking is
	// required.
	numBlocksIndexed int32
	statsFetchWait   time.Duration
	statsCompute     time.Duration
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
		tweakData:    make(map[int32][]spTweakEntry),
		heightToHash: make(map[int32]chainhash.Hash),
		blockFetcher: newBlockPrefetcher(
			blockFetcher, h2hCache,
		),
		statsLastReset:     time.Now(),
		taprootStartHeight: taprootStartHeight,
		taprootSupported:   taprootSupported,
	}
	s.producerBase = producerBase{
		quit:        quit,
		chain:       chain,
		h2hCache:    h2hCache,
		chainParams: chainParams,
		name:        "SP tweak data",
		baseDir:     baseDir,

		// The unfiltered level's directory doubles as the resume
		// directory; see writeSealedFile for the invariant that makes
		// this safe.
		subDir:         spTweakDustDir(spTweakDustLimits[0]),
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
// to exist and to be queryable. We log loudly and idle until shutdown. It
// also creates the per-dust-level directories (the base only creates the
// resume directory) and warns about files of the legacy JSON format.
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

	spDir := filepath.Join(s.baseDir, SPTweakFileDir)
	for _, dustLimit := range spTweakDustLimits {
		path := filepath.Join(s.baseDir, spTweakDustDir(dustLimit))
		if err := os.MkdirAll(path, DirectoryMode); err != nil {
			return fmt.Errorf("error creating directory %s: %w",
				path, err)
		}
	}

	// Files of the legacy JSON format lived directly in the silent
	// payments directory; the binary dust level files live in
	// sub-directories, so any leftovers are simply ignored.
	legacyFiles, err := listFiles(spDir, SPTweakFileSuffix)
	if err != nil {
		return fmt.Errorf("error listing %s: %w", spDir, err)
	}
	if len(legacyFiles) > 0 {
		log.Warnf("Found %d legacy JSON SP tweak data files in %s; "+
			"they are ignored and should be deleted",
			len(legacyFiles), spDir)
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
	s.tweakData[height] = nil
	s.heightToHash[height] = *hash
	s.Unlock()

	// Pre-Taproot blocks can't contain Silent Payment outputs, so we
	// only ingest the empty tweakData entry above so sealReadyFiles
	// can serialize a coherent file.
	if height < s.taprootStartHeight {
		return nil
	}

	fetchStart := time.Now()
	block, err := s.blockFetcher.fetchBlock(height, hash)
	if err != nil {
		return fmt.Errorf("error getting block for SP tweak data: %w",
			err)
	}
	s.numBlocksIndexed++
	s.statsFetchWait += time.Since(fetchStart)

	computeStart := time.Now()
	if err := s.indexBlockSPTweakData(height, block); err != nil {
		return fmt.Errorf("error indexing SP tweak data: %w", err)
	}
	s.statsCompute += time.Since(computeStart)

	return nil
}

// writeSealedFile writes the tweak data file of every dust filter level for
// the just-sealed range. The unfiltered level's file is written last: its
// directory is the producer's resume directory, so its file must only
// appear on disk once all other files of the batch are durable. A crash in
// between re-ingests the whole batch on restart and rewrites all files.
func (s *spTweakFiles) writeSealedFile(fileStart, fileEnd int32) error {
	for _, dustLimit := range slices.Backward(spTweakDustLimits) {
		spDir := filepath.Join(s.baseDir, spTweakDustDir(dustLimit))
		fileName := fmt.Sprintf(
			SPTweakFileNamePattern, spDir, fileStart, fileEnd,
		)

		err := createAndWrite(fileName, func(w io.Writer) error {
			return s.serializeSPTweaks(
				w, dustLimit, fileStart, fileEnd,
			)
		})
		if err != nil {
			return err
		}
	}

	// Log the indexing throughput now that we've written another batch of
	// tweak data to disk, so the initial catch-up progress can be
	// observed. The fetch/compute split shows whether the time goes into
	// waiting for the backend or into the tweak computation itself.
	since := time.Since(s.statsLastReset).Seconds()
	log.Debugf("SP tweak data: indexed %d blocks in %.1fs (%.2f blocks "+
		"per second; %.1fs waiting on block fetch, %.1fs computing "+
		"tweaks)", s.numBlocksIndexed, since,
		float64(s.numBlocksIndexed)/since,
		s.statsFetchWait.Seconds(), s.statsCompute.Seconds())

	s.numBlocksIndexed = 0
	s.statsFetchWait = 0
	s.statsCompute = 0
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
// Payments tweak data and store it, in transaction order, together with the
// largest taproot output value of the transaction.
func (s *spTweakFiles) indexBlockSPTweakData(height int32,
	block *blockWithPrevOuts) error {

	entries := make([]spTweakEntry, 0, len(block.txs))
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

		// Only taproot outputs can be Silent Payment outputs, so the
		// dust filter levels select on the largest taproot output
		// value of the transaction.
		var maxValue uint64
		for _, txOut := range tx.TxOut {
			if !txscript.IsPayToTaproot(txOut.PkScript) {
				continue
			}

			maxValue = max(maxValue, uint64(txOut.Value))
		}

		entry := spTweakEntry{maxValue: maxValue}
		copy(entry.tweak[:], tweakPubKey.SerializeCompressed())
		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		return nil
	}

	s.Lock()
	s.tweakData[height] = entries
	s.Unlock()

	return nil
}

// serializeSPTweaks writes the binary SP tweak data of the given dust filter
// level for [startIndex, endIndex] to w: the self-describing file header,
// then for every block in the range a compact-size count followed by that
// many 33-byte compressed tweak keys. Block heights are implied by position,
// so a block without (matching) tweaks is a single zero byte.
func (s *spTweakFiles) serializeSPTweaks(w io.Writer, dustLimit uint64,
	startIndex, endIndex int32) error {

	s.RLock()
	defer s.RUnlock()

	err := writeSPTweakFileHeader(
		w, s.chainParams.Net, startIndex, dustLimit,
	)
	if err != nil {
		return fmt.Errorf("error writing SP tweak file header: %w",
			err)
	}

	for j := startIndex; j <= endIndex; j++ {
		entries, ok := s.tweakData[j]
		if !ok {
			return fmt.Errorf("missing SP tweak data at height "+
				"%d", j)
		}

		count := uint64(0)
		for _, entry := range entries {
			if entry.maxValue > dustLimit {
				count++
			}
		}

		if err := wire.WriteVarInt(w, 0, count); err != nil {
			return fmt.Errorf("error writing SP tweak count: %w",
				err)
		}

		for _, entry := range entries {
			if entry.maxValue <= dustLimit {
				continue
			}

			if _, err := w.Write(entry.tweak[:]); err != nil {
				return fmt.Errorf("error writing SP tweak: "+
					"%w", err)
			}
		}
	}

	return nil
}

// spTweaksSize returns the exact serialized size of the binary SP tweak
// data of the given dust filter level in [startIndex, endIndex], or false
// if any of the heights is missing from the in-memory tail.
func (s *spTweakFiles) spTweaksSize(dustLimit uint64, startIndex,
	endIndex int32) (int64, bool) {

	s.RLock()
	defer s.RUnlock()

	total := int64(spTweakFileHeaderSize)
	for j := startIndex; j <= endIndex; j++ {
		entries, ok := s.tweakData[j]
		if !ok {
			return 0, false
		}

		count := uint64(0)
		for _, entry := range entries {
			if entry.maxValue > dustLimit {
				count++
			}
		}

		total += int64(wire.VarIntSerializeSize(count)) +
			int64(count)*33
	}

	return total, true
}
