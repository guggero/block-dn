package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

const (
	DefaultHeadersPerFile = 100_000
	DefaultFiltersPerFile = 2_000

	DefaultRegtestHeadersPerFile = 2_000
	DefaultRegtestFiltersPerFile = 2_000

	HeaderFileDir = "headers"
	FilterFileDir = "filters"

	HeaderFileNamePattern        = "%s/block-%07d-%07d.header"
	FilterFileSuffix             = ".cfilter"
	FilterFileNamePattern        = "%s/block-%07d-%07d.cfilter"
	FilterFileNameExtractPattern = "block-[0-9]{7}-([0-9]{7})\\.cfilter"
	FilterHeaderFileNamePattern  = "%s/block-%07d-%07d.cfheader"

	DirectoryMode = 0755
)

var (
	filterBasic = btcjson.FilterTypeBasic

	filterFileNameExtractRegex = regexp.MustCompile(
		FilterFileNameExtractPattern,
	)
)

type cFilterFiles struct {
	quit <-chan struct{}

	cache *cache

	headersPerFile int32
	filtersPerFile int32
	baseDir        string
	chain          *rpcclient.Client
	chainParams    *chaincfg.Params

	startupComplete atomic.Bool
	currentHeight   atomic.Int32
}

func newCFilterFiles(headersPerFile, filtersPerFile int32,
	chain *rpcclient.Client, quit <-chan struct{}, baseDir string,
	chainParams *chaincfg.Params, cache *cache) *cFilterFiles {

	return &cFilterFiles{
		quit:           quit,
		cache:          cache,
		headersPerFile: headersPerFile,
		filtersPerFile: filtersPerFile,
		baseDir:        baseDir,
		chain:          chain,
		chainParams:    chainParams,
	}
}

// updateFiles updates the header and filter files on disk.
//
// NOTE: Must be called as a goroutine.
func (c *cFilterFiles) updateFiles() error {
	log.Debugf("Updating filter files in %s for network %s", c.baseDir,
		c.chainParams.Name)

	info, err := c.chain.GetBlockChainInfo()
	if err != nil {
		return fmt.Errorf("error getting block chain info: %w", err)
	}

	headerDir := filepath.Join(c.baseDir, HeaderFileDir)
	err = os.MkdirAll(headerDir, DirectoryMode)
	if err != nil {
		return fmt.Errorf("error creating directory %s: %w", headerDir,
			err)
	}
	filterDir := filepath.Join(c.baseDir, FilterFileDir)
	err = os.MkdirAll(filterDir, DirectoryMode)
	if err != nil {
		return fmt.Errorf("error creating directory %s: %w", filterDir,
			err)
	}

	log.Debugf("Best block hash: %s, height: %d", info.BestBlockHash,
		info.Blocks)

	startBlock, err := lastFile(
		filterDir, FilterFileSuffix, filterFileNameExtractRegex,
	)
	if err != nil {
		return fmt.Errorf("error getting last filter file: %w", err)
	}

	log.Debugf("Found last filter file at block %d", startBlock)

	// For the headers, we need a bigger range, so drop down the start block
	// to the last header file.
	startBlock = (startBlock / c.headersPerFile) * c.headersPerFile
	log.Debugf("Need to start fetching headers and filters from block %d",
		startBlock)

	log.Debugf("Writing header files from block %d to block %d", startBlock,
		info.Blocks)
	err = c.updateCacheAndFiles(startBlock, info.Blocks)
	if err != nil {
		return fmt.Errorf("error updating blocks: %w", err)
	}

	// Allow serving requests now that we're caught up.
	c.startupComplete.Store(true)

	// Let's now go into the infinite loop of updating the filter files
	// whenever a new block is mined.
	log.Debugf("Caught up headers and filters to best block %d, starting "+
		"to poll for new blocks", info.Blocks)
	for {
		select {
		case <-time.After(blockPollInterval):
		case <-c.quit:
			return errServerShutdown
		}

		height, err := c.chain.GetBlockCount()
		if err != nil {
			return fmt.Errorf("error getting best block: %w", err)
		}

		currentBlock := c.currentHeight.Load()
		if int32(height) == currentBlock {
			continue
		}

		log.Infof("Processing headers and filters for new block mined "+
			"at height %d", height)
		err = c.updateCacheAndFiles(currentBlock+1, int32(height))
		if err != nil {
			return fmt.Errorf("error updating headers and filters "+
				"for blocks: %w", err)
		}
	}
}

func (c *cFilterFiles) updateCacheAndFiles(startBlock, endBlock int32) error {
	headerDir := filepath.Join(c.baseDir, HeaderFileDir)
	filterDir := filepath.Join(c.baseDir, FilterFileDir)

	for i := startBlock; i <= endBlock; i++ {
		// Were we interrupted?
		select {
		case <-c.quit:
			return errServerShutdown
		default:
		}

		hash, err := c.chain.GetBlockHash(int64(i))
		if err != nil {
			return fmt.Errorf("error getting block hash for "+
				"height %d: %w", i, err)
		}

		header, err := c.chain.GetBlockHeader(hash)
		if err != nil {
			return fmt.Errorf("error getting block header for "+
				"hash %s: %w", hash, err)
		}

		filter, err := c.chain.GetBlockFilter(*hash, &filterBasic)
		if err != nil {
			return fmt.Errorf("error getting block filter for "+
				"hash %s: %w", hash, err)
		}
		filterHeader, err := chainhash.NewHashFromStr(filter.Header)
		if err != nil {
			return fmt.Errorf("error parsing filter header for "+
				"hash %s: %w", hash, err)
		}
		filterBytes, err := hex.DecodeString(filter.Filter)
		if err != nil {
			return fmt.Errorf("error parsing filter bytes for "+
				"hash %s: %w", hash, err)
		}

		c.cache.Lock()
		c.cache.heightToHash[i] = *hash
		c.cache.headers[*hash] = header
		c.cache.filters[*hash] = filterBytes
		c.cache.filterHeaders[*hash] = filterHeader
		c.cache.Unlock()

		if (i+1)%c.filtersPerFile == 0 {
			fileStart := i - c.filtersPerFile + 1
			filterFileName := fmt.Sprintf(
				FilterFileNamePattern, filterDir, fileStart, i,
			)

			log.Debugf("Reached header %d, writing file starting "+
				"at %d, containing %d filters to %s", i,
				fileStart, c.filtersPerFile, filterFileName)

			err = c.cache.writeFilters(filterFileName, fileStart, i)
			if err != nil {
				return fmt.Errorf("error writing filters: %w",
					err)
			}
		}

		if (i+1)%c.headersPerFile == 0 {
			fileStart := i - c.headersPerFile + 1
			headerFileName := fmt.Sprintf(
				HeaderFileNamePattern, headerDir, fileStart, i,
			)
			filterHeaderFileName := fmt.Sprintf(
				FilterHeaderFileNamePattern, headerDir,
				fileStart, i,
			)

			log.Debugf("Reached header %d, writing file starting "+
				"at %d, containing %d headers to %s", i,
				fileStart, c.headersPerFile, headerFileName)

			err = c.cache.writeHeaders(headerFileName, fileStart, i)
			if err != nil {
				return fmt.Errorf("error writing headers: %w",
					err)
			}

			log.Debugf("Reached header %d, writing file starting "+
				"at %d, containing %d filter headers to %s", i,
				fileStart, c.headersPerFile,
				filterHeaderFileName)

			err = c.cache.writeFilterHeaders(
				filterHeaderFileName, fileStart, i,
			)
			if err != nil {
				return fmt.Errorf("error writing filter "+
					"headers: %w", err)
			}

			// We don't need the headers or filters anymore, so
			// clear them out.
			c.cache.clear()
		}

		c.currentHeight.Store(i)
	}

	return nil
}

// cache is an in-memory cache of height to block hash, block headers, filter
// headers and filters.
type cache struct {
	sync.RWMutex

	headersPerFile int32
	filtersPerFile int32

	heightToHash  map[int32]chainhash.Hash
	headers       map[chainhash.Hash]*wire.BlockHeader
	filterHeaders map[chainhash.Hash]*chainhash.Hash
	filters       map[chainhash.Hash][]byte
}

func newCache(headersPerFile, filtersPerFile int32) *cache {
	c := &cache{
		headersPerFile: headersPerFile,
		filtersPerFile: filtersPerFile,
	}
	c.clear()

	return c
}

func (c *cache) clear() {
	c.Lock()
	c.heightToHash = make(map[int32]chainhash.Hash, c.headersPerFile)
	c.headers = make(
		map[chainhash.Hash]*wire.BlockHeader, c.headersPerFile,
	)
	c.filterHeaders = make(
		map[chainhash.Hash]*chainhash.Hash, c.headersPerFile,
	)
	c.filters = make(map[chainhash.Hash][]byte, c.filtersPerFile)
	c.Unlock()
}

func (c *cache) writeHeaders(fileName string, startIndex,
	endIndex int32) error {

	c.RLock()
	defer c.RUnlock()

	log.Debugf("Writing header file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	err = c.serializeHeaders(file, startIndex, endIndex)
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

func (c *cache) serializeHeaders(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		hash, ok := c.heightToHash[j]
		if !ok {
			return fmt.Errorf("invalid height %d", j)
		}

		header, ok := c.headers[hash]
		if !ok {
			return fmt.Errorf("missing header for hash %s (height "+
				"%d)", hash.String(), j)
		}

		err := header.Serialize(w)
		if err != nil {
			return fmt.Errorf("error writing headers: %w", err)
		}
	}

	return nil
}

func (c *cache) writeFilterHeaders(fileName string, startIndex,
	endIndex int32) error {

	c.RLock()
	defer c.RUnlock()

	log.Debugf("Writing filter header file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	err = c.serializeFilterHeaders(file, startIndex, endIndex)
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

func (c *cache) serializeFilterHeaders(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		hash, ok := c.heightToHash[j]
		if !ok {
			return fmt.Errorf("invalid height %d", j)
		}

		filterHeader, ok := c.filterHeaders[hash]
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

func (c *cache) writeFilters(fileName string, startIndex,
	endIndex int32) error {

	c.RLock()
	defer c.RUnlock()

	log.Debugf("Writing filter file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	err = c.serializeFilters(file, startIndex, endIndex)
	if err != nil {
		return fmt.Errorf("error writing filters to file %s: %w",
			fileName, err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("error closing file %s: %w", fileName, err)
	}

	return nil
}

func (c *cache) serializeFilters(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		hash, ok := c.heightToHash[j]
		if !ok {
			return fmt.Errorf("invalid height %d", j)
		}

		filter, ok := c.filters[hash]
		if !ok {
			return fmt.Errorf("missing filter for hash %s (height "+
				"%d)", hash.String(), j)
		}

		err := wire.WriteVarBytes(w, 0, filter)
		if err != nil {
			return fmt.Errorf("error writing filters: %w", err)
		}
	}

	return nil
}

func lastFile(fileDir, searchPattern string,
	extractPattern *regexp.Regexp) (int32, error) {

	globPattern := fmt.Sprintf("%s/*%s", fileDir, searchPattern)
	files, err := filepath.Glob(globPattern)
	if err != nil {
		return 0, fmt.Errorf("error listing files '%s' in %s: %w",
			globPattern, fileDir, err)
	}

	if len(files) == 0 {
		return 0, nil
	}

	sort.Strings(files)
	last := files[len(files)-1]
	matches := extractPattern.FindStringSubmatch(last)
	if len(matches) != 2 || matches[1] == "" {
		return 0, fmt.Errorf("error extracting number from file %s",
			last)
	}

	numUint, err := strconv.ParseInt(matches[1], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("error parsing number '%s': %w",
			matches[1], err)
	}

	return int32(numUint), nil
}
