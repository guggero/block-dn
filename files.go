package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

// updateFiles updates the header and filter files on disk.
//
// NOTE: Must be called as a goroutine.
func (s *server) updateFiles() error {
	log.Debugf("Updating filter files in %s for network %s", s.baseDir,
		s.chainParams.Name)

	info, err := s.chain.GetBlockChainInfo()
	if err != nil {
		return fmt.Errorf("error getting block chain info: %w", err)
	}

	headerDir := filepath.Join(s.baseDir, HeaderFileDir)
	err = os.MkdirAll(headerDir, DirectoryMode)
	if err != nil {
		return fmt.Errorf("error creating directory %s: %w", headerDir,
			err)
	}
	filterDir := filepath.Join(s.baseDir, FilterFileDir)
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
	startBlock = (startBlock / s.headersPerFile) * s.headersPerFile
	log.Debugf("Need to start fetching headers and filters from block %d",
		startBlock)

	log.Debugf("Writing header files from block %d to block %d", startBlock,
		info.Blocks)
	err = s.updateCacheAndFiles(startBlock, info.Blocks)
	if err != nil {
		return fmt.Errorf("error updating blocks: %w", err)
	}

	// Allow serving requests now that we're caught up.
	s.startupComplete.Store(true)

	// Let's now go into the infinite loop of updating the filter files
	// whenever a new block is mined.
	log.Debugf("Caught up to best block %d, starting to poll for new "+
		"blocks", info.Blocks)
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

		log.Infof("New block mined at height %d", height)
		err = s.updateCacheAndFiles(currentBlock+1, int32(height))
		if err != nil {
			return fmt.Errorf("error updating blocks: %w", err)
		}
	}
}

func (s *server) updateCacheAndFiles(startBlock, endBlock int32) error {
	headerDir := filepath.Join(s.baseDir, HeaderFileDir)
	filterDir := filepath.Join(s.baseDir, FilterFileDir)

	for i := startBlock; i <= endBlock; i++ {
		// Were we interrupted?
		select {
		case <-s.quit:
			return errServerShutdown
		default:
		}

		hash, err := s.chain.GetBlockHash(int64(i))
		if err != nil {
			return fmt.Errorf("error getting block hash for "+
				"height %d: %w", i, err)
		}

		header, err := s.chain.GetBlockHeader(hash)
		if err != nil {
			return fmt.Errorf("error getting block header for "+
				"hash %s: %w", hash, err)
		}

		filter, err := s.chain.GetBlockFilter(*hash, &filterBasic)
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

		s.cacheLock.Lock()
		s.heightToHash[i] = *hash
		s.headers[*hash] = header
		s.filters[*hash] = filterBytes
		s.filterHeaders[*hash] = filterHeader
		s.cacheLock.Unlock()

		if (i+1)%s.filtersPerFile == 0 {
			fileStart := i - s.filtersPerFile + 1
			filterFileName := fmt.Sprintf(
				FilterFileNamePattern, filterDir, fileStart, i,
			)

			log.Debugf("Reached header %d, writing file starting "+
				"at %d, containing %d filters to %s", i,
				fileStart, s.filtersPerFile, filterFileName)

			err = s.writeFilters(filterFileName, fileStart, i)
			if err != nil {
				return fmt.Errorf("error writing filters: %w",
					err)
			}
		}

		if (i+1)%s.headersPerFile == 0 {
			fileStart := i - s.headersPerFile + 1
			headerFileName := fmt.Sprintf(
				HeaderFileNamePattern, headerDir, fileStart, i,
			)
			filterHeaderFileName := fmt.Sprintf(
				FilterHeaderFileNamePattern, headerDir,
				fileStart, i,
			)

			log.Debugf("Reached header %d, writing file starting "+
				"at %d, containing %d headers to %s", i,
				fileStart, s.headersPerFile, headerFileName)

			err = s.writeHeaders(headerFileName, fileStart, i)
			if err != nil {
				return fmt.Errorf("error writing headers: %w",
					err)
			}

			log.Debugf("Reached header %d, writing file starting "+
				"at %d, containing %d filter headers to %s", i,
				fileStart, s.headersPerFile,
				filterHeaderFileName)

			err = s.writeFilterHeaders(
				filterHeaderFileName, fileStart, i,
			)
			if err != nil {
				return fmt.Errorf("error writing filter "+
					"headers: %w", err)
			}

			// We don't need the headers or filters anymore, so
			// clear them out.
			s.clearCache()
		}

		s.currentHeight.Store(i)
	}

	return nil
}

func (s *server) clearCache() {
	s.cacheLock.Lock()
	s.heightToHash = make(map[int32]chainhash.Hash, s.headersPerFile)
	s.headers = make(
		map[chainhash.Hash]*wire.BlockHeader, s.headersPerFile,
	)
	s.filterHeaders = make(
		map[chainhash.Hash]*chainhash.Hash, s.headersPerFile,
	)
	s.filters = make(map[chainhash.Hash][]byte, s.filtersPerFile)
	s.cacheLock.Unlock()
}
