package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire/v2"
)

// errDeepReorg signals that bitcoind reorganized past block-dn's reorg-safe
// depth — i.e., past the highest block already written to disk. Recovering
// would require rewriting an "immutable" file, so the producer refuses and
// asks the server to shut down. Operators can bump --reorg-safe-depth,
// regenerate, and restart.
var errDeepReorg = errors.New("reorg deeper than --reorg-safe-depth; " +
	"refusing to rewrite sealed files")

type headerFiles struct {
	producerBase

	headers       map[int32]*wire.BlockHeader
	filterHeaders map[int32]chainhash.Hash
}

func newHeaderFiles(headersPerFile, reOrgSafeDepth int32,
	chain *rpcclient.Client, quit <-chan struct{}, baseDir string,
	chainParams *chaincfg.Params, h2hCache *heightToHashCache) *headerFiles {

	hf := &headerFiles{
		headers:       make(map[int32]*wire.BlockHeader),
		filterHeaders: make(map[int32]chainhash.Hash),
	}
	hf.producerBase = producerBase{
		quit:           quit,
		chain:          chain,
		h2hCache:       h2hCache,
		chainParams:    chainParams,
		name:           "header",
		baseDir:        baseDir,
		subDir:         HeaderFileDir,
		fileSuffix:     HeaderFileSuffix,
		extractRegex:   headerFileNameExtractRegex,
		entriesPerFile: headersPerFile,
		reOrgSafeDepth: reOrgSafeDepth,
		hooks: producerHooks{
			ingest:          hf.ingest,
			writeSealedFile: hf.writeSealedFile,
			pruneLocked:     hf.pruneLocked,
			cachedHash:      hf.cachedHash,
		},
	}
	hf.lastSealedHeight.Store(-1)

	return hf
}

// ingest fetches the block header and matching compact-filter header for
// the given height and stores both in-memory.
func (h *headerFiles) ingest(height int32) error {
	hash, err := h.h2hCache.getBlockHash(height)
	if err != nil {
		return fmt.Errorf("error getting block hash for height %d: %w",
			height, err)
	}

	header, err := h.chain.GetBlockHeader(hash)
	if err != nil {
		return fmt.Errorf("error getting block header for hash %s: %w",
			hash, err)
	}

	filter, err := h.chain.GetBlockFilter(*hash, &filterBasic)
	if err != nil {
		return fmt.Errorf("error getting block filter for hash %s: %w",
			hash, err)
	}
	filterHeader, err := chainhash.NewHashFromStr(filter.Header)
	if err != nil {
		return fmt.Errorf("error parsing filter header for hash %s: %w",
			hash, err)
	}

	h.Lock()
	h.headers[height] = header
	h.filterHeaders[height] = *filterHeader
	h.Unlock()

	return nil
}

// writeSealedFile writes both the block-header and filter-header files for
// the just-sealed range to disk.
func (h *headerFiles) writeSealedFile(fileStart, fileEnd int32) error {
	headerDir := filepath.Join(h.baseDir, h.subDir)

	headerFileName := fmt.Sprintf(HeaderFileNamePattern, headerDir,
		fileStart, fileEnd)
	if err := h.writeHeaders(headerFileName, fileStart, fileEnd); err != nil {
		return err
	}

	filterHeaderFileName := fmt.Sprintf(FilterHeaderFileNamePattern,
		headerDir, fileStart, fileEnd)
	return h.writeFilterHeaders(filterHeaderFileName, fileStart, fileEnd)
}

// pruneLocked drops headers and filter-headers entries for [start, end].
// Called with the producer's write lock held.
func (h *headerFiles) pruneLocked(start, end int32) {
	for j := start; j <= end; j++ {
		delete(h.headers, j)
		delete(h.filterHeaders, j)
	}
}

// cachedHash returns the block hash for the given in-memory height by
// computing it from the stored header. Used by the base's reorg detection.
func (h *headerFiles) cachedHash(height int32) (chainhash.Hash, bool) {
	h.RLock()
	defer h.RUnlock()

	header, ok := h.headers[height]
	if !ok {
		return chainhash.Hash{}, false
	}
	return header.BlockHash(), true
}

func (h *headerFiles) writeHeaders(fileName string, startIndex,
	endIndex int32) error {

	h.RLock()
	defer h.RUnlock()

	log.Debugf("Writing header file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	err = h.serializeHeadersLocked(file, startIndex, endIndex)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("error writing headers to file %s: %w",
			fileName, err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("error closing file %s: %w", fileName, err)
	}

	return nil
}

// serializeHeaders writes raw block headers for [startIndex, endIndex] to w.
// Acquires the read lock; safe to call from request handlers.
func (h *headerFiles) serializeHeaders(w io.Writer, startIndex,
	endIndex int32) error {

	h.RLock()
	defer h.RUnlock()

	return h.serializeHeadersLocked(w, startIndex, endIndex)
}

func (h *headerFiles) serializeHeadersLocked(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		header, ok := h.headers[j]
		if !ok {
			return fmt.Errorf("missing header at height %d", j)
		}

		err := header.Serialize(w)
		if err != nil {
			return fmt.Errorf("error writing headers: %w", err)
		}
	}

	return nil
}

func (h *headerFiles) writeFilterHeaders(fileName string, startIndex,
	endIndex int32) error {

	h.RLock()
	defer h.RUnlock()

	log.Debugf("Writing filter header file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	err = h.serializeFilterHeadersLocked(file, startIndex, endIndex)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("error writing filter headers to file %s: %w",
			fileName, err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("error closing file %s: %w", fileName, err)
	}

	return nil
}

// serializeFilterHeaders writes raw filter headers for [startIndex, endIndex]
// to w. Acquires the read lock; safe to call from request handlers.
func (h *headerFiles) serializeFilterHeaders(w io.Writer, startIndex,
	endIndex int32) error {

	h.RLock()
	defer h.RUnlock()

	return h.serializeFilterHeadersLocked(w, startIndex, endIndex)
}

func (h *headerFiles) serializeFilterHeadersLocked(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		filterHeader, ok := h.filterHeaders[j]
		if !ok {
			return fmt.Errorf("missing filter header at height %d",
				j)
		}

		num, err := w.Write(filterHeader[:])
		if err != nil {
			return fmt.Errorf("error writing filter header: %w",
				err)
		}
		if num != chainhash.HashSize {
			return fmt.Errorf("short write when writing filter " +
				"headers")
		}
	}

	return nil
}

// filterHeaderAtHeight returns the cached filter header for the given height
// or false if the height is not currently in memory (typically because it's
// already been sealed to disk).
func (h *headerFiles) filterHeaderAtHeight(hgt int32) (chainhash.Hash, bool) {
	h.RLock()
	defer h.RUnlock()

	hash, ok := h.filterHeaders[hgt]
	return hash, ok
}
