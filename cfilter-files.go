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
	DefaultHeightToHashCacheSize = 100_000
	DefaultHeadersPerFile        = 100_000
	DefaultFiltersPerFile        = 2_000

	DefaultRegtestHeadersPerFile = 2_000
	DefaultRegtestFiltersPerFile = 2_000

	HeaderFileDir = "headers"
	FilterFileDir = "filters"

	HeaderFileSuffix             = ".header"
	HeaderFileNamePattern        = "%s/block-%07d-%07d.header"
	HeaderFileNameExtractPattern = "block-[0-9]{7}-([0-9]{7})\\.header"
	FilterFileSuffix             = ".cfilter"
	FilterFileNamePattern        = "%s/block-%07d-%07d.cfilter"
	FilterFileNameExtractPattern = "block-[0-9]{7}-([0-9]{7})\\.cfilter"
	FilterHeaderFileNamePattern  = "%s/block-%07d-%07d.cfheader"

	DirectoryMode = 0755
)

var (
	headerFileNameExtractRegex = regexp.MustCompile(
		HeaderFileNameExtractPattern,
	)

	filterBasic = btcjson.FilterTypeBasic

	filterFileNameExtractRegex = regexp.MustCompile(
		FilterFileNameExtractPattern,
	)
)

type cFilterFiles struct {
	sync.RWMutex

	quit <-chan struct{}

	h2hCache *heightToHashCache

	filtersPerFile int32
	baseDir        string
	chain          *rpcclient.Client
	chainParams    *chaincfg.Params

	startupComplete atomic.Bool
	currentHeight   atomic.Int32

	filters map[chainhash.Hash][]byte
}

func newCFilterFiles(filtersPerFile int32, chain *rpcclient.Client,
	quit <-chan struct{}, baseDir string, chainParams *chaincfg.Params,
	h2hCache *heightToHashCache) *cFilterFiles {

	cf := &cFilterFiles{
		quit:           quit,
		h2hCache:       h2hCache,
		filtersPerFile: filtersPerFile,
		baseDir:        baseDir,
		chain:          chain,
		chainParams:    chainParams,
	}
	cf.clearData()

	return cf
}

func (c *cFilterFiles) isStartupComplete() bool {
	return c.startupComplete.Load()
}

func (c *cFilterFiles) getCurrentHeight() int32 {
	return c.currentHeight.Load()
}

func (c *cFilterFiles) clearData() {
	c.Lock()
	defer c.Unlock()

	c.filters = make(map[chainhash.Hash][]byte, c.filtersPerFile)
}

// updateFiles updates the header and filter files on disk.
//
// NOTE: Must be called as a goroutine.
func (c *cFilterFiles) updateFiles(numBlocks int32) error {
	log.Debugf("Updating filter files in %s for network %s", c.baseDir,
		c.chainParams.Name)

	filterDir := filepath.Join(c.baseDir, FilterFileDir)
	err := os.MkdirAll(filterDir, DirectoryMode)
	if err != nil {
		return fmt.Errorf("error creating directory %s: %w", filterDir,
			err)
	}

	lastBlock, err := lastFile(
		filterDir, FilterFileSuffix, filterFileNameExtractRegex,
	)
	if err != nil {
		return fmt.Errorf("error getting last filter file: %w", err)
	}

	// If we already had some blocks written, then we need to start from
	// the next block.
	startBlock := lastBlock
	if lastBlock > 0 {
		startBlock++
	}

	log.Debugf("Writing filter files from block %d to block %d", startBlock,
		numBlocks)
	err = c.updateCacheAndFiles(startBlock, numBlocks)
	if err != nil {
		return fmt.Errorf("error updating blocks: %w", err)
	}

	// Allow serving requests now that we're caught up.
	c.startupComplete.Store(true)

	// Let's now go into the infinite loop of updating the filter files
	// whenever a new block is mined.
	log.Debugf("Caught up filters to best block %d, starting to poll for "+
		"new blocks", numBlocks)
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

		log.Infof("Processing filters for new block mined at height %d",
			height)
		err = c.updateCacheAndFiles(currentBlock+1, int32(height))
		if err != nil {
			return fmt.Errorf("error updating filters for blocks: "+
				"%w", err)
		}
	}
}

func (c *cFilterFiles) updateCacheAndFiles(startBlock, endBlock int32) error {
	filterDir := filepath.Join(c.baseDir, FilterFileDir)

	for i := startBlock; i <= endBlock; i++ {
		// Were we interrupted?
		select {
		case <-c.quit:
			return errServerShutdown
		default:
		}

		hash, err := c.h2hCache.getBlockHash(i)
		if err != nil {
			return fmt.Errorf("error getting block hash for "+
				"height %d: %w", i, err)
		}

		filter, err := c.chain.GetBlockFilter(*hash, &filterBasic)
		if err != nil {
			return fmt.Errorf("error getting block filter for "+
				"hash %s: %w", hash, err)
		}
		filterBytes, err := hex.DecodeString(filter.Filter)
		if err != nil {
			return fmt.Errorf("error parsing filter bytes for "+
				"hash %s: %w", hash, err)
		}

		c.Lock()
		c.filters[*hash] = filterBytes
		c.Unlock()

		if (i+1)%c.filtersPerFile == 0 {
			fileStart := i - c.filtersPerFile + 1
			filterFileName := fmt.Sprintf(
				FilterFileNamePattern, filterDir, fileStart, i,
			)

			log.Debugf("Reached filter height %d, writing file "+
				"starting at %d, containing %d items to %s", i,
				fileStart, c.filtersPerFile, filterFileName)

			err = c.writeFilters(filterFileName, fileStart, i)
			if err != nil {
				return fmt.Errorf("error writing filters: %w",
					err)
			}

			// We don't need the filters anymore, so clear them out.
			c.clearData()
		}

		c.currentHeight.Store(i)
	}

	return nil
}

func (c *cFilterFiles) writeFilters(fileName string, startIndex,
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

func (c *cFilterFiles) serializeFilters(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		hash, err := c.h2hCache.getBlockHash(j)
		if err != nil {
			return fmt.Errorf("invalid height %d", j)
		}

		filter, ok := c.filters[*hash]
		if !ok {
			return fmt.Errorf("missing filter for hash %s (height "+
				"%d)", hash.String(), j)
		}

		err = wire.WriteVarBytes(w, 0, filter)
		if err != nil {
			return fmt.Errorf("error writing filters: %w", err)
		}
	}

	return nil
}

func listFiles(fileDir, searchPattern string) ([]string, error) {
	globPattern := fmt.Sprintf("%s/*%s", fileDir, searchPattern)
	files, err := filepath.Glob(globPattern)
	if err != nil {
		return nil, fmt.Errorf("error listing files '%s' in %s: %w",
			globPattern, fileDir, err)
	}

	sort.Strings(files)

	return files, nil
}

func lastFile(fileDir, searchPattern string,
	extractPattern *regexp.Regexp) (int32, error) {

	files, err := listFiles(fileDir, searchPattern)
	if err != nil {
		return 0, err
	}

	if len(files) == 0 {
		return 0, nil
	}

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
