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

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire/v2"
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
	producerBase

	filters map[int32][]byte

	// heightToHash backs cachedHash; cFilterFiles doesn't otherwise
	// store the block hash per height (the filter bytes don't contain
	// it), so we record it explicitly during ingest so reorg detection
	// can survive a concurrent h2hCache invalidation from another
	// producer.
	heightToHash map[int32]chainhash.Hash
}

func newCFilterFiles(filtersPerFile, reOrgSafeDepth int32,
	chain *rpcclient.Client, quit <-chan struct{}, baseDir string,
	chainParams *chaincfg.Params,
	h2hCache *heightToHashCache) *cFilterFiles {

	cf := &cFilterFiles{
		filters:      make(map[int32][]byte),
		heightToHash: make(map[int32]chainhash.Hash),
	}
	cf.producerBase = producerBase{
		quit:           quit,
		chain:          chain,
		h2hCache:       h2hCache,
		chainParams:    chainParams,
		name:           "filter",
		baseDir:        baseDir,
		subDir:         FilterFileDir,
		fileSuffix:     FilterFileSuffix,
		extractRegex:   filterFileNameExtractRegex,
		entriesPerFile: filtersPerFile,
		reOrgSafeDepth: reOrgSafeDepth,
		hooks: producerHooks{
			ingest:          cf.ingest,
			writeSealedFile: cf.writeSealedFile,
			pruneLocked:     cf.pruneLocked,
			cachedHash:      cf.cachedHash,
		},
	}
	cf.lastSealedHeight.Store(-1)

	return cf
}

func (c *cFilterFiles) ingest(height int32) error {
	hash, err := c.h2hCache.getBlockHash(height)
	if err != nil {
		return fmt.Errorf("error getting block hash for height %d: %w",
			height, err)
	}

	filter, err := c.chain.GetBlockFilter(*hash, &filterBasic)
	if err != nil {
		return fmt.Errorf("error getting block filter for hash %s: %w",
			hash, err)
	}
	filterBytes, err := hex.DecodeString(filter.Filter)
	if err != nil {
		return fmt.Errorf("error parsing filter bytes for hash %s: %w",
			hash, err)
	}

	c.Lock()
	c.filters[height] = filterBytes
	c.heightToHash[height] = *hash
	c.Unlock()

	return nil
}

func (c *cFilterFiles) writeSealedFile(fileStart, fileEnd int32) error {
	filterDir := filepath.Join(c.baseDir, c.subDir)
	filterFileName := fmt.Sprintf(FilterFileNamePattern, filterDir,
		fileStart, fileEnd)
	return c.writeFilters(filterFileName, fileStart, fileEnd)
}

func (c *cFilterFiles) pruneLocked(start, end int32) {
	for j := start; j <= end; j++ {
		delete(c.filters, j)
		delete(c.heightToHash, j)
	}
}

func (c *cFilterFiles) cachedHash(height int32) (chainhash.Hash, bool) {
	c.RLock()
	defer c.RUnlock()

	hash, ok := c.heightToHash[height]
	return hash, ok
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

	err = c.serializeFiltersLocked(file, startIndex, endIndex)
	if err != nil {
		_ = file.Close()
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

	c.RLock()
	defer c.RUnlock()

	return c.serializeFiltersLocked(w, startIndex, endIndex)
}

// filterAtHeight returns the raw filter bytes for the given height if it is
// still in the in-memory (unsealed) tail, or false if it has already been
// pruned after sealing to disk.
func (c *cFilterFiles) filterAtHeight(height int32) ([]byte, bool) {
	c.RLock()
	defer c.RUnlock()

	filter, ok := c.filters[height]
	return filter, ok
}

// filtersSize returns the exact serialized size of the var-int prefixed
// filters in [startIndex, endIndex], or false if any of them is missing from
// the in-memory tail.
func (c *cFilterFiles) filtersSize(startIndex, endIndex int32) (int64, bool) {
	c.RLock()
	defer c.RUnlock()

	var total int64
	for j := startIndex; j <= endIndex; j++ {
		filter, ok := c.filters[j]
		if !ok {
			return 0, false
		}

		total += int64(wire.VarIntSerializeSize(uint64(len(filter)))) +
			int64(len(filter))
	}

	return total, true
}

func (c *cFilterFiles) serializeFiltersLocked(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		filter, ok := c.filters[j]
		if !ok {
			return fmt.Errorf("missing filter at height %d", j)
		}

		err := wire.WriteVarBytes(w, 0, filter)
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
