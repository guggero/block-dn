package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
)

const (
	HeadersPerFile = 100_000
	FiltersPerFile = 10_000

	HeaderFileDir = "headers"
	FilterFileDir = "filters"
	BlocksFileDir = "blocks/%07d-%07d"

	HeaderFileNamePattern       = "%s/block-%07d-%07d.header"
	FilterFileNamePattern       = "%s/block-%07d-%07d.filter"
	FilterHeaderFileNamePattern = "%s/block-%07d-%07d.header"
	BlocksFileNamePattern       = "%s/%s.block"
	StatusFileName              = "status.json"

	DirectoryMode = 0755
	FileMode      = 0644
)

var (
	filterBasic = btcjson.FilterTypeBasic
)

type Status struct {
	LastHeaderHeight int32 `json:"last_complete_header_file_height"`
	LastFilterHeight int32 `json:"last_complete_filter_file_height"`
	LastBlockHeight  int32 `json:"last_block_file_height"`
	KnownBestHeight  int32 `json:"known_best_height"`
}

func UpdateFilterFiles(ctx context.Context, baseDir string,
	client *rpcclient.Client, chainParams *chaincfg.Params) error {

	log.Debugf("Updating filter files in %s for network %s", baseDir,
		chainParams.Name)

	info, err := client.GetBlockChainInfo()
	if err != nil {
		return fmt.Errorf("error getting block chain info: %w", err)
	}

	headerDir := filepath.Join(baseDir, HeaderFileDir)
	err = os.MkdirAll(headerDir, DirectoryMode)
	if err != nil {
		return fmt.Errorf("error creating directory %s: %w", headerDir,
			err)
	}
	filterDir := filepath.Join(baseDir, FilterFileDir)
	err = os.MkdirAll(filterDir, DirectoryMode)
	if err != nil {
		return fmt.Errorf("error creating directory %s: %w", filterDir,
			err)
	}

	status, err := readStatus(baseDir)
	if err != nil {
		return fmt.Errorf("error reading status file: %w", err)
	}
	status.KnownBestHeight = info.Blocks

	log.Debugf("Best block hash: %s, height: %d", info.BestBlockHash,
		info.Blocks)
	log.Debugf("Writing header files up to block %d", info.Blocks)

	heightToHash := make(map[int32]chainhash.Hash, info.Blocks)
	headers := make(map[chainhash.Hash]*wire.BlockHeader, HeadersPerFile)
	filters := make(map[chainhash.Hash][]byte, FiltersPerFile)
	filterHeaders := make(
		map[chainhash.Hash]*chainhash.Hash, HeadersPerFile,
	)
	for i := status.LastFilterHeight; i <= info.Blocks; i++ {
		// Were we interrupted?
		select {
		case <-ctx.Done():
			return fmt.Errorf("sync interrupted")
		default:
		}

		hash, err := client.GetBlockHash(int64(i))
		if err != nil {
			return fmt.Errorf("error getting block hash for "+
				"height %d: %w", i, err)
		}

		header, err := client.GetBlockHeader(hash)
		if err != nil {
			return fmt.Errorf("error getting block header for "+
				"hash %s: %w", hash, err)
		}

		filter, err := client.GetBlockFilter(*hash, &filterBasic)
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

		heightToHash[i] = *hash
		headers[*hash] = header
		filters[*hash] = filterBytes
		filterHeaders[*hash] = filterHeader

		err = writeBlock(baseDir, hash, i, client)
		if err != nil {
			return fmt.Errorf("error writing block: %w", err)
		}

		if i > 0 && i%1000 == 0 {
			log.Debugf("Processed %d blocks", i)
		}

		if i%FiltersPerFile == FiltersPerFile-1 {
			fileStart := i - FiltersPerFile + 1
			filterFileName := fmt.Sprintf(
				FilterFileNamePattern, filterDir, fileStart, i,
			)

			err = writeFilters(
				filterFileName, fileStart, i, heightToHash,
				filters,
			)
			if err != nil {
				return fmt.Errorf("error writing filters: %w",
					err)
			}

			status.LastFilterHeight = i
			err = writeStatus(baseDir, status)
			if err != nil {
				return fmt.Errorf("error writing status file: "+
					"%w", err)
			}
		}

		if i%HeadersPerFile == HeadersPerFile-1 {
			fileStart := i - HeadersPerFile + 1
			headerFileName := fmt.Sprintf(
				HeaderFileNamePattern, headerDir, fileStart, i,
			)
			filterHeaderFileName := fmt.Sprintf(
				FilterHeaderFileNamePattern, filterDir,
				fileStart, i,
			)

			err = writeHeaders(
				headerFileName, fileStart, i, heightToHash,
				headers,
			)
			if err != nil {
				return fmt.Errorf("error writing headers: %w",
					err)
			}

			err = writeFilterHeaders(
				filterHeaderFileName, fileStart, i,
				heightToHash, filterHeaders,
			)
			if err != nil {
				return fmt.Errorf("error writing filter "+
					"headers: %w", err)
			}

			status.LastHeaderHeight = i
			err = writeStatus(baseDir, status)
			if err != nil {
				return fmt.Errorf("error writing status file: "+
					"%w", err)
			}
		}
	}

	return nil
}

func currentBlockDir(i int32) string {
	div := i / FiltersPerFile
	return fmt.Sprintf(
		BlocksFileDir, div*FiltersPerFile, (div+1)*FiltersPerFile-1,
	)
}

func writeBlock(baseDir string, hash *chainhash.Hash, height int32,
	client *rpcclient.Client) error {

	blockDir := filepath.Join(baseDir, currentBlockDir(height))
	err := os.MkdirAll(blockDir, DirectoryMode)
	if err != nil {
		return fmt.Errorf("error creating directory %s: %w", blockDir,
			err)
	}

	fileName := fmt.Sprintf(BlocksFileNamePattern, blockDir, hash.String())
	if fileExists(fileName) {
		return nil
	}

	block, err := client.GetBlock(hash)
	if err != nil {
		return fmt.Errorf("error getting block for hash %s: %w", hash,
			err)
	}

	// Write the actual block to disk.
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	if err := block.Serialize(file); err != nil {
		return fmt.Errorf("error writing block to file %s: %w",
			fileName, err)
	}

	err = file.Close()
	if err != nil {
		return fmt.Errorf("error closing file %s: %w", fileName, err)
	}

	return nil
}

func writeHeaders(fileName string, startIndex, endIndex int32,
	heightToHash map[int32]chainhash.Hash,
	headers map[chainhash.Hash]*wire.BlockHeader) error {

	log.Debugf("Writing header file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	for j := startIndex; j <= endIndex; j++ {
		hash := heightToHash[j]
		header := headers[hash]

		err = header.Serialize(file)
		if err != nil {
			return fmt.Errorf("error writing header to file %s: %w",
				fileName, err)
		}

		delete(headers, hash)
	}
	err = file.Close()
	if err != nil {
		return fmt.Errorf("error closing file %s: %w", fileName, err)
	}

	return nil
}

func writeFilterHeaders(fileName string, startIndex, endIndex int32,
	heightToHash map[int32]chainhash.Hash,
	filterHeaders map[chainhash.Hash]*chainhash.Hash) error {

	log.Debugf("Writing filter header file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	for j := startIndex; j <= endIndex; j++ {
		hash := heightToHash[j]
		filterHeader := filterHeaders[hash]

		num, err := file.Write(filterHeader[:])
		if err != nil {
			return fmt.Errorf("error writing filter header to "+
				"file %s: %w", fileName, err)
		}
		if num != chainhash.HashSize {
			return fmt.Errorf("short write when writing filter "+
				"header to file %s", fileName)
		}

		delete(filterHeaders, hash)
	}
	err = file.Close()
	if err != nil {
		return fmt.Errorf("error closing file %s: %w", fileName, err)
	}

	return nil
}

func writeFilters(fileName string, startIndex, endIndex int32,
	heightToHash map[int32]chainhash.Hash,
	filters map[chainhash.Hash][]byte) error {

	log.Debugf("Writing filter file %s", fileName)
	file, err := os.Create(fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", fileName, err)
	}

	for j := startIndex; j <= endIndex; j++ {
		hash := heightToHash[j]
		filter := filters[hash]

		err := wire.WriteVarBytes(file, 0, filter[:])
		if err != nil {
			return fmt.Errorf("error writing filter to file %s: %w",
				fileName, err)
		}

		delete(filters, hash)
	}
	err = file.Close()
	if err != nil {
		return fmt.Errorf("error closing file %s: %w", fileName, err)
	}

	return nil
}

func readStatus(baseDir string) (*Status, error) {
	file, err := os.Open(filepath.Join(baseDir, StatusFileName))
	if os.IsNotExist(err) {
		return &Status{}, nil
	}
	if err != nil {
		return nil, err
	}

	var s Status
	err = json.NewDecoder(file).Decode(&s)
	if err != nil {
		return nil, err
	}

	err = file.Close()
	if err != nil {
		return nil, err
	}

	return &s, nil
}

func writeStatus(baseDir string, status *Status) error {
	file, err := os.OpenFile(
		filepath.Join(baseDir, StatusFileName), os.O_WRONLY|os.O_CREATE,
		FileMode,
	)
	if err != nil {
		return err
	}

	err = json.NewEncoder(file).Encode(status)
	if err != nil {
		return err
	}

	return file.Close()
}

// filesExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}
