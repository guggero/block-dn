package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

type headerFiles struct {
	sync.RWMutex

	quit <-chan struct{}

	h2hCache *heightToHashCache

	headersPerFile int32
	baseDir        string
	chain          *rpcclient.Client
	chainParams    *chaincfg.Params

	startupComplete atomic.Bool
	currentHeight   atomic.Int32

	headers       map[chainhash.Hash]*wire.BlockHeader
	filterHeaders map[chainhash.Hash]*chainhash.Hash
}

func newHeaderFiles(headersPerFile int32, chain *rpcclient.Client,
	quit <-chan struct{}, baseDir string, chainParams *chaincfg.Params,
	h2hCache *heightToHashCache) *headerFiles {

	cf := &headerFiles{
		quit:           quit,
		h2hCache:       h2hCache,
		headersPerFile: headersPerFile,
		baseDir:        baseDir,
		chain:          chain,
		chainParams:    chainParams,
	}
	cf.clearData()

	return cf
}

func (h *headerFiles) isStartupComplete() bool {
	return h.startupComplete.Load()
}

func (h *headerFiles) getCurrentHeight() int32 {
	return h.currentHeight.Load()
}

func (h *headerFiles) clearData() {
	h.Lock()
	defer h.Unlock()

	h.headers = make(map[chainhash.Hash]*wire.BlockHeader, h.headersPerFile)
	h.filterHeaders = make(
		map[chainhash.Hash]*chainhash.Hash, h.headersPerFile,
	)
}

// updateFiles updates the header and filter files on disk.
//
// NOTE: Must be called as a goroutine.
func (h *headerFiles) updateFiles(numBlocks int32) error {
	log.Debugf("Updating header files in %s for network %s", h.baseDir,
		h.chainParams.Name)

	headerDir := filepath.Join(h.baseDir, HeaderFileDir)
	err := os.MkdirAll(headerDir, DirectoryMode)
	if err != nil {
		return fmt.Errorf("error creating directory %s: %w", headerDir,
			err)
	}

	lastBlock, err := lastFile(
		headerDir, HeaderFileSuffix, headerFileNameExtractRegex,
	)
	if err != nil {
		return fmt.Errorf("error getting last header file: %w", err)
	}

	// If we already had some blocks written, then we need to start from
	// the next block.
	startBlock := lastBlock
	if lastBlock > 0 {
		startBlock++
	}

	log.Debugf("Writing header files from block %d to block %d", startBlock,
		numBlocks)
	err = h.updateCacheAndFiles(startBlock, numBlocks)
	if err != nil {
		return fmt.Errorf("error updating blocks: %w", err)
	}

	// Allow serving requests now that we're caught up.
	h.startupComplete.Store(true)

	// Let's now go into the infinite loop of updating the filter files
	// whenever a new block is mined.
	log.Debugf("Caught up headers to best block %d, starting to poll for "+
		"new blocks", numBlocks)
	for {
		select {
		case <-time.After(blockPollInterval):
		case <-h.quit:
			return errServerShutdown
		}

		height, err := h.chain.GetBlockCount()
		if err != nil {
			return fmt.Errorf("error getting best block: %w", err)
		}

		currentBlock := h.currentHeight.Load()
		if int32(height) == currentBlock {
			continue
		}

		log.Infof("Processing headers for new block mined at height %d",
			height)
		err = h.updateCacheAndFiles(currentBlock+1, int32(height))
		if err != nil {
			return fmt.Errorf("error updating headers for blocks: "+
				"%w", err)
		}
	}
}

func (h *headerFiles) updateCacheAndFiles(startBlock, endBlock int32) error {
	headerDir := filepath.Join(h.baseDir, HeaderFileDir)

	for i := startBlock; i <= endBlock; i++ {
		// Were we interrupted?
		select {
		case <-h.quit:
			return errServerShutdown
		default:
		}

		hash, err := h.h2hCache.getBlockHash(i)
		if err != nil {
			return fmt.Errorf("error getting block hash for "+
				"height %d: %w", i, err)
		}

		header, err := h.chain.GetBlockHeader(hash)
		if err != nil {
			return fmt.Errorf("error getting block header for "+
				"hash %s: %w", hash, err)
		}

		filter, err := h.chain.GetBlockFilter(*hash, &filterBasic)
		if err != nil {
			return fmt.Errorf("error getting block filter for "+
				"hash %s: %w", hash, err)
		}
		filterHeader, err := chainhash.NewHashFromStr(filter.Header)
		if err != nil {
			return fmt.Errorf("error parsing filter header for "+
				"hash %s: %w", hash, err)
		}

		h.Lock()
		h.headers[*hash] = header
		h.filterHeaders[*hash] = filterHeader
		h.Unlock()

		if (i+1)%h.headersPerFile == 0 {
			fileStart := i - h.headersPerFile + 1
			headerFileName := fmt.Sprintf(
				HeaderFileNamePattern, headerDir, fileStart, i,
			)
			filterHeaderFileName := fmt.Sprintf(
				FilterHeaderFileNamePattern, headerDir,
				fileStart, i,
			)

			log.Debugf("Reached header height %d, writing file "+
				"starting at %d, containing %d items to %s", i,
				fileStart, h.headersPerFile, headerFileName)

			err = h.writeHeaders(headerFileName, fileStart, i)
			if err != nil {
				return fmt.Errorf("error writing headers: %w",
					err)
			}

			log.Debugf("Reached filter header height %d, writing "+
				"file starting at %d, containing %d items to "+
				"%s", i, fileStart, h.headersPerFile,
				filterHeaderFileName)

			err = h.writeFilterHeaders(
				filterHeaderFileName, fileStart, i,
			)
			if err != nil {
				return fmt.Errorf("error writing filter "+
					"headers: %w", err)
			}

			// We don't need the headers or filters anymore, so
			// clear them out.
			h.clearData()
		}

		h.currentHeight.Store(i)
	}

	return nil
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

	err = h.serializeHeaders(file, startIndex, endIndex)
	if err != nil {
		return fmt.Errorf("error writing headers to file %s: %w",
			fileName, err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("error closing file %s: %w", fileName, err)
	}

	return nil
}

func (h *headerFiles) serializeHeaders(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		hash, err := h.h2hCache.getBlockHash(j)
		if err != nil {
			return fmt.Errorf("invalid height %d", j)
		}

		header, ok := h.headers[*hash]
		if !ok {
			return fmt.Errorf("missing header for hash %s (height "+
				"%d)", hash.String(), j)
		}

		err = header.Serialize(w)
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

	err = h.serializeFilterHeaders(file, startIndex, endIndex)
	if err != nil {
		return fmt.Errorf("error writing filter headers to file %s: %w",
			fileName, err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("error closing file %s: %w", fileName, err)
	}

	return nil
}

func (h *headerFiles) serializeFilterHeaders(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		hash, err := h.h2hCache.getBlockHash(j)
		if err != nil {
			return fmt.Errorf("invalid height %d", j)
		}

		filterHeader, ok := h.filterHeaders[*hash]
		if !ok {
			return fmt.Errorf("missing filter header for hash %s "+
				"(height %d)", hash.String(), j)
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
