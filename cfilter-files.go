package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
)

const (
	HeadersPerFile = 100_000
	FiltersPerFile = 2_000

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

func (s *server) writeHeaders(fileName string, startIndex,
	endIndex int32) error {

	s.cacheLock.RLock()
	defer s.cacheLock.RUnlock()

	log.Debugf("Writing header file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	err = s.serializeHeaders(file, startIndex, endIndex)
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

func (s *server) serializeHeaders(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		hash, ok := s.heightToHash[j]
		if !ok {
			return fmt.Errorf("invalid height %d", j)
		}

		header, ok := s.headers[hash]
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

func (s *server) writeFilterHeaders(fileName string, startIndex,
	endIndex int32) error {

	s.cacheLock.RLock()
	defer s.cacheLock.RUnlock()

	log.Debugf("Writing filter header file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	err = s.serializeFilterHeaders(file, startIndex, endIndex)
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

func (s *server) serializeFilterHeaders(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		hash, ok := s.heightToHash[j]
		if !ok {
			return fmt.Errorf("invalid height %d", j)
		}

		filterHeader, ok := s.filterHeaders[hash]
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

func (s *server) writeFilters(fileName string, startIndex,
	endIndex int32) error {

	s.cacheLock.RLock()
	defer s.cacheLock.RUnlock()

	log.Debugf("Writing filter file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	err = s.serializeFilters(file, startIndex, endIndex)
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

func (s *server) serializeFilters(w io.Writer, startIndex,
	endIndex int32) error {

	for j := startIndex; j <= endIndex; j++ {
		hash, ok := s.heightToHash[j]
		if !ok {
			return fmt.Errorf("invalid height %d", j)
		}

		filter, ok := s.filters[hash]
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
