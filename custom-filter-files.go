package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/btcsuite/btcd/btcutil/v2/gcs"
	"github.com/btcsuite/btcd/btcutil/v2/gcs/builder"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

// scriptTypes is a bit mask of the output script types a custom filter
// configuration includes.
type scriptTypes uint8

const (
	scriptTypeP2WPKH scriptTypes = 1 << iota
	scriptTypeP2WSH
	scriptTypeP2TR
)

// customFilterConfig describes one output-type-restricted filter set.
type customFilterConfig struct {
	// name identifies the set and is used in the on-disk directory
	// names (filters/custom-<name> and headers/custom-<name>).
	name string

	// types is the set of output script types whose scripts are
	// included in this filter.
	types scriptTypes
}

// customFilterConfigs are the filter sets the custom filter producer builds
// in a single pass over the chain. The first entry additionally serves as
// the producer's resume anchor: its filter directory is the producer's
// subDir and its files are written last in each sealed batch, so its file's
// presence on disk implies the whole batch is durable.
var customFilterConfigs = []customFilterConfig{
	{
		name: "segwit",
		types: scriptTypeP2WPKH | scriptTypeP2WSH |
			scriptTypeP2TR,
	},
	{
		name:  "p2wpkh",
		types: scriptTypeP2WPKH,
	},
	{
		name:  "p2wsh",
		types: scriptTypeP2WSH,
	},
	{
		name:  "p2tr",
		types: scriptTypeP2TR,
	},
}

// customFilterDir returns the directory (relative to the base directory) the
// filter files of the given configuration are stored in.
func customFilterDir(name string) string {
	return filepath.Join(FilterFileDir, "custom-"+name)
}

// customHeaderDir returns the directory (relative to the base directory) the
// filter header files of the given configuration are stored in.
func customHeaderDir(name string) string {
	return filepath.Join(HeaderFileDir, "custom-"+name)
}

// classifyScript returns the type bit of the given output script, or zero if
// it's none of the types custom filters can be restricted to.
func classifyScript(pkScript []byte) scriptTypes {
	switch {
	case txscript.IsPayToWitnessPubKeyHash(pkScript):
		return scriptTypeP2WPKH

	case txscript.IsPayToWitnessScriptHash(pkScript):
		return scriptTypeP2WSH

	case txscript.IsPayToTaproot(pkScript):
		return scriptTypeP2TR

	default:
		return 0
	}
}

// buildCustomFilters builds one output-type-restricted filter per
// configuration from a single pass over the given block. The filters use the
// exact same parameters as bitcoind's BIP-0158 basic filters (P=19,
// M=784931, SipHash key derived from the block hash, elements de-duplicated
// by script); they only differ in which elements are included: the output
// scripts of matching types created by the block, plus the previous output
// scripts of matching types spent by it. The type restriction subsumes the
// basic filter's exclusion of empty and OP_RETURN scripts.
func buildCustomFilters(hash *chainhash.Hash,
	block *blockWithPrevOuts) ([]*gcs.Filter, error) {

	builders := make([]*builder.GCSBuilder, len(customFilterConfigs))
	for i := range customFilterConfigs {
		builders[i] = builder.WithKeyHash(hash)
	}

	addScript := func(pkScript []byte) {
		types := classifyScript(pkScript)
		if types == 0 {
			return
		}

		for i := range customFilterConfigs {
			if customFilterConfigs[i].types&types != 0 {
				builders[i].AddEntry(pkScript)
			}
		}
	}

	for _, tx := range block.txs {
		for _, txOut := range tx.TxOut {
			addScript(txOut.PkScript)
		}
	}
	for _, pkScript := range block.prevOutScripts {
		addScript(pkScript)
	}

	filters := make([]*gcs.Filter, len(customFilterConfigs))
	for i, b := range builders {
		filter, err := b.Build()
		if err != nil {
			return nil, fmt.Errorf("error building custom filter "+
				"%s: %w", customFilterConfigs[i].name, err)
		}

		filters[i] = filter
	}

	return filters, nil
}

// customFilterFiles is a producer that builds multiple sets of
// output-type-restricted compact filters (plus their BIP-0157 filter header
// chains) in a single scan over the chain, to compare their sizes against
// the basic filters. The files aren't served over any endpoint yet.
type customFilterFiles struct {
	producerBase

	// blockFetcher fetches blocks along with the previous output
	// scripts spent by them, prefetching upcoming heights in the
	// background.
	blockFetcher *blockPrefetcher

	// filters holds, per unsealed height, the serialized filter of
	// every configuration, indexed like customFilterConfigs.
	filters map[int32][][]byte

	// filterHeaders holds, per height, the filter header of every
	// configuration, indexed like customFilterConfigs. Unlike the
	// filters, the headers live in files of headersPerFile entries, so
	// they're retained in memory until their (much larger) file is
	// sealed; at the mainnet defaults that's up to 100k heights of five
	// 32-byte hashes, about 16 MiB. After a restart the unsealed window
	// is rebuilt from the sealed filter files on disk.
	filterHeaders map[int32][]chainhash.Hash

	// headersPerFile is the number of entries per filter header file.
	// It must be a multiple of the producer's entriesPerFile (the
	// filters per file), so every header file boundary coincides with a
	// filter file boundary.
	headersPerFile int32

	// heightToHash mirrors cFilterFiles: see the comment there.
	heightToHash map[int32]chainhash.Hash

	// numBlocksIndexed counts the blocks indexed since the last stats
	// log line, which we emit whenever a file is sealed. Together with
	// the fetch/compute split below it shows where the indexing time
	// goes. Only accessed from the producer goroutine, so no locking is
	// required.
	numBlocksIndexed int32
	statsFetchWait   time.Duration
	statsCompute     time.Duration
	statsLastReset   time.Time
}

// newCustomFilterFiles creates a new custom filter producer writing filter
// files of itemsPerFile entries and filter header files of headersPerFile
// entries (which must be a multiple of itemsPerFile) below the given base
// directory.
func newCustomFilterFiles(itemsPerFile, headersPerFile,
	reOrgSafeDepth int32, chain *rpcclient.Client, quit <-chan struct{},
	baseDir string, chainParams *chaincfg.Params,
	h2hCache *heightToHashCache,
	blockFetcher *blockPrevOutFetcher) *customFilterFiles {

	c := &customFilterFiles{
		filters:        make(map[int32][][]byte),
		filterHeaders:  make(map[int32][]chainhash.Hash),
		heightToHash:   make(map[int32]chainhash.Hash),
		headersPerFile: headersPerFile,
		blockFetcher: newBlockPrefetcher(
			blockFetcher, h2hCache,
		),
		statsLastReset: time.Now(),
	}
	c.producerBase = producerBase{
		quit:        quit,
		chain:       chain,
		h2hCache:    h2hCache,
		chainParams: chainParams,
		name:        "custom filter",
		baseDir:     baseDir,

		// The first configuration's filter directory doubles as the
		// resume directory; see writeSealedFile for the invariant
		// that makes this safe.
		subDir:         customFilterDir(customFilterConfigs[0].name),
		fileSuffix:     FilterFileSuffix,
		extractRegex:   filterFileNameExtractRegex,
		entriesPerFile: itemsPerFile,
		reOrgSafeDepth: reOrgSafeDepth,
		hooks: producerHooks{
			ingest:          c.ingest,
			writeSealedFile: c.writeSealedFile,
			pruneLocked:     c.pruneLocked,
			cachedHash:      c.cachedHash,
		},
	}
	c.lastSealedHeight.Store(-1)

	return c
}

// updateFiles shadows producerBase.updateFiles to validate the file sizing
// and to create the per-set filter and header directories first; the base
// only creates the single resume directory (subDir).
func (c *customFilterFiles) updateFiles(numBlocks int32) error {
	if c.headersPerFile <= 0 ||
		c.headersPerFile%c.entriesPerFile != 0 {

		return fmt.Errorf("custom filter headers per file (%d) must "+
			"be a positive multiple of the filters per file (%d)",
			c.headersPerFile, c.entriesPerFile)
	}

	for _, cfg := range customFilterConfigs {
		dirs := []string{
			customFilterDir(cfg.name), customHeaderDir(cfg.name),
		}
		for _, dir := range dirs {
			path := filepath.Join(c.baseDir, dir)
			err := os.MkdirAll(path, DirectoryMode)
			if err != nil {
				return fmt.Errorf("error creating directory "+
					"%s: %w", path, err)
			}
		}
	}

	return c.producerBase.updateFiles(numBlocks)
}

// ingest builds the filters and filter headers of every configuration for
// the block at the given height.
func (c *customFilterFiles) ingest(height int32) error {
	hash, err := c.h2hCache.getBlockHash(height)
	if err != nil {
		return fmt.Errorf("error getting block hash for height %d: %w",
			height, err)
	}

	prevHeaders, err := c.prevFilterHeaders(height)
	if err != nil {
		return fmt.Errorf("error getting previous filter headers "+
			"for height %d: %w", height, err)
	}

	fetchStart := time.Now()
	block, err := c.blockFetcher.fetchBlock(height, hash)
	if err != nil {
		return fmt.Errorf("error getting block for custom filters: "+
			"%w", err)
	}
	c.numBlocksIndexed++
	c.statsFetchWait += time.Since(fetchStart)

	computeStart := time.Now()
	filters, err := buildCustomFilters(hash, block)
	if err != nil {
		return fmt.Errorf("error building custom filters for height "+
			"%d: %w", height, err)
	}

	serialized := make([][]byte, len(customFilterConfigs))
	headers := make([]chainhash.Hash, len(customFilterConfigs))
	for i, filter := range filters {
		serialized[i], err = filter.NBytes()
		if err != nil {
			return fmt.Errorf("error serializing custom filter "+
				"%s at height %d: %w",
				customFilterConfigs[i].name, height, err)
		}

		headers[i] = filterHeaderFromBytes(
			serialized[i], &prevHeaders[i],
		)
	}
	c.statsCompute += time.Since(computeStart)

	c.Lock()
	c.filters[height] = serialized
	c.filterHeaders[height] = headers
	c.heightToHash[height] = *hash
	c.Unlock()

	return nil
}

// filterHeaderFromBytes computes the BIP-0157 filter header for a serialized
// filter, chaining onto the previous height's header:
// double-SHA256(double-SHA256(filter) || prevHeader).
func filterHeaderFromBytes(filterBytes []byte,
	prevHeader *chainhash.Hash) chainhash.Hash {

	filterHash := chainhash.DoubleHashB(filterBytes)
	return chainhash.DoubleHashH(append(filterHash, prevHeader[:]...))
}

// prevFilterHeaders returns the filter header of height-1 for every
// configuration, which the headers at the given height chain onto. For the
// genesis block that's an all-zero header per BIP-0157. After a restart the
// previous height's headers are no longer in memory and the unsealed part of
// the header chain is rebuilt from the files on disk instead.
func (c *customFilterFiles) prevFilterHeaders(height int32) ([]chainhash.Hash,
	error) {

	if height == 0 {
		return make([]chainhash.Hash, len(customFilterConfigs)), nil
	}

	c.RLock()
	headers, ok := c.filterHeaders[height-1]
	c.RUnlock()

	if ok {
		return headers, nil
	}

	return c.resumeFilterHeaders(height)
}

// resumeFilterHeaders re-creates the in-memory filter header state after a
// restart, where the given height is the first one after the last sealed
// filter file. The chain is anchored at the last sealed header file (or the
// genesis all-zero header) and the span not yet covered by a header file is
// recomputed from the sealed filter files, repopulating the in-memory map so
// the next header file can be sealed. The final chain state (the headers of
// height-1) is returned.
func (c *customFilterFiles) resumeFilterHeaders(
	height int32) ([]chainhash.Hash, error) {

	// The sealed header files cover [0, anchorCount-1]; every filter
	// file boundary is a potential resume point, but header files only
	// exist at multiples of headersPerFile.
	anchorCount := (height / c.headersPerFile) * c.headersPerFile

	prevHeaders := make([]chainhash.Hash, len(customFilterConfigs))
	if anchorCount > 0 {
		anchors, err := c.readHeaderFileAnchor(anchorCount - 1)
		if err != nil {
			return nil, err
		}
		prevHeaders = anchors
	}

	if anchorCount == height {
		return prevHeaders, nil
	}

	return c.rebuildFilterHeaders(prevHeaders, anchorCount, height-1)
}

// readHeaderFileAnchor reads the filter header of every configuration for
// the given height, which must be the last height of a sealed header file,
// from the header files on disk.
func (c *customFilterFiles) readHeaderFileAnchor(
	height int32) ([]chainhash.Hash, error) {

	fileStart := height + 1 - c.headersPerFile
	headers := make([]chainhash.Hash, len(customFilterConfigs))
	for i, cfg := range customFilterConfigs {
		headerDir := filepath.Join(
			c.baseDir, customHeaderDir(cfg.name),
		)
		fileName := fmt.Sprintf(
			FilterHeaderFileNamePattern, headerDir, fileStart,
			height,
		)

		data, err := os.ReadFile(fileName)
		if err != nil {
			return nil, fmt.Errorf("error reading filter header "+
				"file to resume the header chain: %w", err)
		}

		expectedSize := int(c.headersPerFile) * chainhash.HashSize
		if len(data) != expectedSize {
			return nil, fmt.Errorf("unexpected size %d of filter "+
				"header file %s, expected %d", len(data),
				fileName, expectedSize)
		}

		copy(headers[i][:], data[len(data)-chainhash.HashSize:])
	}

	return headers, nil
}

// rebuildFilterHeaders recomputes the filter header chains for the given
// (inclusive, filter-file-aligned) height range from the sealed filter files
// on disk, chaining onto prevHeaders. The computed headers are stored in the
// in-memory map, since they're still needed to seal their header file later,
// and the final chain state is returned.
func (c *customFilterFiles) rebuildFilterHeaders(prevHeaders []chainhash.Hash,
	start, end int32) ([]chainhash.Hash, error) {

	log.Debugf("Rebuilding custom filter headers for heights %d-%d from "+
		"sealed filter files", start, end)

	prev := slices.Clone(prevHeaders)
	for fileStart := start; fileStart <= end; fileStart += c.entriesPerFile {
		fileEnd := fileStart + c.entriesPerFile - 1

		rows := make([][]chainhash.Hash, c.entriesPerFile)
		for r := range rows {
			rows[r] = make(
				[]chainhash.Hash, len(customFilterConfigs),
			)
		}

		for i, cfg := range customFilterConfigs {
			filterDir := filepath.Join(
				c.baseDir, customFilterDir(cfg.name),
			)
			fileName := fmt.Sprintf(
				FilterFileNamePattern, filterDir, fileStart,
				fileEnd,
			)

			numRead, err := readFilterFile(
				fileName, func(idx int32, filter []byte) {
					// Corrupt oversized files are caught
					// through the entry count below.
					if idx >= int32(len(rows)) {
						return
					}

					prev[i] = filterHeaderFromBytes(
						filter, &prev[i],
					)
					rows[idx][i] = prev[i]
				},
			)
			if err != nil {
				return nil, fmt.Errorf("error rebuilding "+
					"filter headers from %s: %w", fileName,
					err)
			}
			if numRead != c.entriesPerFile {
				return nil, fmt.Errorf("filter file %s has "+
					"%d entries, expected %d", fileName,
					numRead, c.entriesPerFile)
			}
		}

		c.Lock()
		for r, headers := range rows {
			c.filterHeaders[fileStart+int32(r)] = headers
		}
		c.Unlock()
	}

	return prev, nil
}

// readFilterFile reads a sealed filter file and invokes the callback for
// every var-int prefixed filter in it, with the entry's index within the
// file. It returns the number of entries read; the caller is responsible
// for making sure the callback can handle that many.
func readFilterFile(fileName string,
	each func(idx int32, filter []byte)) (int32, error) {

	file, err := os.Open(fileName)
	if err != nil {
		return 0, fmt.Errorf("error opening file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	reader := bufio.NewReader(file)
	for idx := int32(0); ; idx++ {
		filter, err := wire.ReadVarBytes(
			reader, 0, wire.MaxMessagePayload, "filter",
		)
		if errors.Is(err, io.EOF) {
			return idx, nil
		}
		if err != nil {
			return idx, fmt.Errorf("error reading filter %d: %w",
				idx, err)
		}

		each(idx, filter)
	}
}

// writeSealedFile writes the filter files of every configuration for the
// just-sealed range and, whenever the range completes a filter header file
// span, the (larger) filter header files as well. The first configuration's
// filter file is written last: its directory is the producer's resume
// directory, so its file must only appear on disk once all other files of
// the batch are durable. A crash in between re-ingests the last filter
// batch on restart and rewrites all of its files.
func (c *customFilterFiles) writeSealedFile(fileStart, fileEnd int32) error {
	// The header files hold headersPerFile entries (a multiple of the
	// filters per file), so they seal together with the filter batch
	// that completes their span.
	if (fileEnd+1)%c.headersPerFile == 0 {
		if err := c.writeFilterHeaderFiles(fileEnd); err != nil {
			return err
		}
	}

	for i, cfg := range slices.Backward(customFilterConfigs) {
		filterDir := filepath.Join(
			c.baseDir, customFilterDir(cfg.name),
		)
		filterFile := fmt.Sprintf(
			FilterFileNamePattern, filterDir, fileStart, fileEnd,
		)
		err := createAndWrite(filterFile, func(w io.Writer) error {
			return c.serializeFilters(w, i, fileStart, fileEnd)
		})
		if err != nil {
			return err
		}
	}

	// Log the indexing throughput now that we've written another batch
	// of filter files to disk, so the initial catch-up progress can be
	// observed.
	since := time.Since(c.statsLastReset).Seconds()
	log.Debugf("Custom filters: indexed %d blocks in %.1fs (%.2f blocks "+
		"per second; %.1fs waiting on block fetch, %.1fs computing "+
		"filters)", c.numBlocksIndexed, since,
		float64(c.numBlocksIndexed)/since,
		c.statsFetchWait.Seconds(), c.statsCompute.Seconds())

	c.numBlocksIndexed = 0
	c.statsFetchWait = 0
	c.statsCompute = 0
	c.statsLastReset = time.Now()

	return nil
}

// pruneLocked drops the in-memory filter entries for [start, end]. The
// filter headers are deliberately not touched here: they're pruned by
// writeFilterHeaderFiles once their (larger) file is sealed. For rollback
// prunes that leaves stale header entries above the rollback height in
// memory, which is harmless: the sequential re-ingest overwrites each height
// before its entry is read again.
func (c *customFilterFiles) pruneLocked(start, end int32) {
	for j := start; j <= end; j++ {
		delete(c.filters, j)
		delete(c.heightToHash, j)
	}
}

// writeFilterHeaderFiles writes the filter header file of every
// configuration for the span ending at the given height, then prunes the
// span's header entries from memory: the heights above it are all still
// present (sealing lags the tip by the reorg-safe depth) and a restart
// rebuilds its state from the files on disk.
func (c *customFilterFiles) writeFilterHeaderFiles(fileEnd int32) error {
	fileStart := fileEnd + 1 - c.headersPerFile

	for i, cfg := range customFilterConfigs {
		headerDir := filepath.Join(
			c.baseDir, customHeaderDir(cfg.name),
		)
		headerFile := fmt.Sprintf(
			FilterHeaderFileNamePattern, headerDir, fileStart,
			fileEnd,
		)
		err := createAndWrite(headerFile, func(w io.Writer) error {
			return c.serializeFilterHeaders(
				w, i, fileStart, fileEnd,
			)
		})
		if err != nil {
			return err
		}
	}

	c.Lock()
	for j := fileStart; j <= fileEnd; j++ {
		delete(c.filterHeaders, j)
	}
	c.Unlock()

	return nil
}

// cachedHash returns the block hash this producer has recorded for the given
// in-memory height. Used by the base's reorg detection.
func (c *customFilterFiles) cachedHash(height int32) (chainhash.Hash, bool) {
	c.RLock()
	defer c.RUnlock()

	hash, ok := c.heightToHash[height]
	return hash, ok
}

// serializeFilters writes the var-int prefixed filters of the given
// configuration index for [startIndex, endIndex] to w, in the same format
// the basic filter files use.
func (c *customFilterFiles) serializeFilters(w io.Writer, cfgIndex int,
	startIndex, endIndex int32) error {

	c.RLock()
	defer c.RUnlock()

	for j := startIndex; j <= endIndex; j++ {
		filters, ok := c.filters[j]
		if !ok {
			return fmt.Errorf("missing custom filter at height "+
				"%d", j)
		}

		if err := wire.WriteVarBytes(w, 0, filters[cfgIndex]); err != nil {
			return fmt.Errorf("error writing filters: %w", err)
		}
	}

	return nil
}

// serializeFilterHeaders writes the raw 32-byte filter headers of the given
// configuration index for [startIndex, endIndex] to w, in the same format
// the basic filter header files use.
func (c *customFilterFiles) serializeFilterHeaders(w io.Writer, cfgIndex int,
	startIndex, endIndex int32) error {

	c.RLock()
	defer c.RUnlock()

	for j := startIndex; j <= endIndex; j++ {
		headers, ok := c.filterHeaders[j]
		if !ok {
			return fmt.Errorf("missing custom filter header at "+
				"height %d", j)
		}

		if _, err := w.Write(headers[cfgIndex][:]); err != nil {
			return fmt.Errorf("error writing filter headers: %w",
				err)
		}
	}

	return nil
}

// createAndWrite creates fileName and fills it with the output of the given
// write closure, mirroring the create/write/close handling of the other
// producers.
func createAndWrite(fileName string, write func(io.Writer) error) error {
	log.Debugf("Writing custom filter file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	if err := write(file); err != nil {
		_ = file.Close()
		return fmt.Errorf("error writing file %s: %w", fileName, err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("error closing file %s: %w", fileName, err)
	}

	return nil
}
