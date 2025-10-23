package main

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/gorilla/mux"
)

var (
	maxAgeTemporary = time.Second
	maxAgeMemory    = time.Minute
	maxAgeDisk      = time.Hour * 24 * 365

	// importMetadataSize is the size of the metadata that is prepended to
	// each imported file. It consists of 4 bytes (uint32 little endian) for
	// the Bitcoin network identifier, 1 byte for the version, 1 byte for
	// the file type (0 for block headers, 1 for compact filter headers) and
	// 4 bytes (uint32 little endian) for the start header.
	importMetadataSize = 4 + 1 + 1 + 4

	// typeBlockHeader is the byte value used to indicate that the file
	// contains block headers.
	typeBlockHeader = byte(0)

	// typeFilterHeader is the byte value used to indicate that the file
	// contains compact filter headers.
	typeFilterHeader = byte(1)

	//go:embed index.html
	indexHTML string
)

const (
	// headerImportVersion is the current version of the import file format.
	headerImportVersion = 0
)

type serializable interface {
	Serialize(w io.Writer) error
}

func (s *server) indexRequestHandler(w http.ResponseWriter, _ *http.Request) {
	addCorsHeaders(w)
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(indexHTML))
}

func (s *server) statusRequestHandler(w http.ResponseWriter, _ *http.Request) {
	s.cacheLock.RLock()
	defer s.cacheLock.RUnlock()

	bestHeight := s.currentHeight.Load()
	bestBlock, ok := s.heightToHash[bestHeight]
	if !ok {
		sendError(w, fmt.Errorf("invalid sync status"))
		return
	}

	bestFilter, ok := s.filterHeaders[bestBlock]
	if !ok {
		sendError(w, fmt.Errorf("invalid sync status"))
		return
	}

	status := &Status{
		ChainGenesisHash: s.chainParams.GenesisHash.String(),
		ChainName:        s.chainParams.Name,
		BestBlockHeight:  bestHeight,
		BestBlockHash:    bestBlock.String(),
		BestFilterHeader: bestFilter.String(),
		EntriesPerHeader: s.headersPerFile,
		EntriesPerFilter: s.filtersPerFile,
	}

	sendJSON(w, status, maxAgeMemory)
}

func (s *server) headersRequestHandler(w http.ResponseWriter, r *http.Request) {
	s.heightBasedRequestHandler(
		w, r, HeaderFileDir, HeaderFileNamePattern,
		int64(s.headersPerFile), s.serializeHeaders,
	)
}

func (s *server) headersImportRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	// These kinds of requests aren't available in light mode.
	if s.lightMode {
		sendUnavailable(w, fmt.Errorf("endpoint not available in "+
			"light mode"))
		return
	}

	height, err := parseRequestParamInt64(r, "height")
	if err != nil {
		sendBadRequest(w, err)
		return
	}

	if int32(height) > s.currentHeight.Load() {
		sendBadRequest(w, fmt.Errorf("height %d is greater than "+
			"current height %d", height, s.currentHeight.Load()))
		return
	}

	if height == 0 || height%int64(s.headersPerFile) != 0 {
		sendBadRequest(w, fmt.Errorf("height %d must be a multiple of "+
			"%d", height, s.headersPerFile))
		return
	}

	s.heightBasedImportRequestHandler(
		w, height, HeaderFileDir, HeaderFileNamePattern,
		int64(s.headersPerFile), s.serializeHeaders, typeBlockHeader,
	)
}

func (s *server) filterHeadersRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	s.heightBasedRequestHandler(
		w, r, HeaderFileDir, FilterHeaderFileNamePattern,
		int64(s.headersPerFile), s.serializeFilterHeaders,
	)
}

func (s *server) filterHeadersImportRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	// These kinds of requests aren't available in light mode.
	if s.lightMode {
		sendUnavailable(w, fmt.Errorf("endpoint not available in "+
			"light mode"))
		return
	}

	height, err := parseRequestParamInt64(r, "height")
	if err != nil {
		sendBadRequest(w, err)
		return
	}

	if int32(height) > s.currentHeight.Load() {
		sendBadRequest(w, fmt.Errorf("height %d is greater than "+
			"current height %d", height, s.currentHeight.Load()))
		return
	}

	if height == 0 || height%int64(s.headersPerFile) != 0 {
		sendBadRequest(w, fmt.Errorf("height %d must be a multiple of "+
			"%d", height, s.headersPerFile))
		return
	}

	s.heightBasedImportRequestHandler(
		w, height, HeaderFileDir, FilterHeaderFileNamePattern,
		int64(s.headersPerFile), s.serializeFilterHeaders,
		typeFilterHeader,
	)
}

func (s *server) filtersRequestHandler(w http.ResponseWriter, r *http.Request) {
	s.heightBasedRequestHandler(
		w, r, FilterFileDir, FilterFileNamePattern,
		int64(s.filtersPerFile), s.serializeFilters,
	)
}

func (s *server) heightBasedRequestHandler(w http.ResponseWriter,
	r *http.Request, subDir, fileNamePattern string,
	numEntriesPerFile int64, serializeCb func(w io.Writer, startIndex,
		endIndex int32) error) {

	// These kinds of requests aren't available in light mode.
	if s.lightMode {
		sendUnavailable(w, fmt.Errorf("endpoint not available in "+
			"light mode"))
		return
	}

	height, err := parseRequestParamInt64(r, "height")
	if err != nil {
		sendBadRequest(w, err)
		return
	}

	srcDir := filepath.Join(s.baseDir, subDir)
	fileName := fmt.Sprintf(
		fileNamePattern, srcDir, height, height+numEntriesPerFile-1,
	)
	if fileExists(fileName) {
		addCorsHeaders(w)
		addCacheHeaders(w, maxAgeDisk)
		w.WriteHeader(http.StatusOK)
		if err := streamFile(w, fileName); err != nil {
			log.Errorf("Error while streaming file: %v", err)
			sendError(w, err)
		}

		return
	}

	s.cacheLock.RLock()
	defer s.cacheLock.RUnlock()

	if _, ok := s.heightToHash[int32(height)]; !ok {
		sendBadRequest(w, fmt.Errorf("invalid height"))
		return
	}

	// The requested start height wasn't yet in a file, so we need to
	// stream the headers from memory.
	addCorsHeaders(w)
	addCacheHeaders(w, maxAgeMemory)
	w.WriteHeader(http.StatusOK)
	err = serializeCb(w, int32(height), s.currentHeight.Load())
	if err != nil {
		log.Errorf("Error serializing: %v", err)
	}
}

func (s *server) heightBasedImportRequestHandler(w http.ResponseWriter,
	height int64, subDir, fileNamePattern string, numEntriesPerFile int64,
	serializeCb func(w io.Writer, startIndex, endIndex int32) error,
	fileType byte) {

	addCorsHeaders(w)
	addCacheHeaders(w, maxAgeDisk)
	w.WriteHeader(http.StatusOK)

	metadata := make([]byte, importMetadataSize)
	binary.LittleEndian.PutUint32(metadata[0:4], uint32(s.chainParams.Net))
	metadata[4] = headerImportVersion
	metadata[5] = fileType

	// We always start at height 0 for the import.
	binary.LittleEndian.PutUint32(metadata[6:10], 0)

	if _, err := w.Write(metadata); err != nil {
		log.Errorf("Error writing metadata: %v", err)
		return
	}

	var (
		startHeight = int64(0)
		lastHeight  = int64(-1)
	)
	for ; startHeight <= height; startHeight += numEntriesPerFile {
		// We always start at 0, so we'll always have one entry less
		// in the files than the even height we require the user to
		// enter.
		// TODO(guggero): Allow returning any range, even if that would
		// mean returning a partial file.
		if startHeight == height {
			return
		}

		srcDir := filepath.Join(s.baseDir, subDir)
		fileName := fmt.Sprintf(
			fileNamePattern, srcDir, startHeight,
			startHeight+numEntriesPerFile-1,
		)

		if !fileExists(fileName) {
			break
		}

		lastHeight = startHeight + numEntriesPerFile - 1
		if err := streamFile(w, fileName); err != nil {
			log.Errorf("Error while streaming file: %v", err)
			sendError(w, err)
		}
	}

	s.cacheLock.RLock()
	defer s.cacheLock.RUnlock()

	if _, ok := s.heightToHash[int32(height)]; !ok {
		sendBadRequest(w, fmt.Errorf("invalid height"))
		return
	}

	// The requested start height wasn't yet in a file, so we need to
	// stream the headers from memory.
	err := serializeCb(w, int32(lastHeight+1), int32(height))
	if err != nil {
		log.Errorf("Error serializing: %v", err)
	}
}

func (s *server) blockRequestHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	blockHash := vars["hash"]

	if len(blockHash) != hex.EncodedLen(chainhash.HashSize) {
		sendBadRequest(w, fmt.Errorf("invalid block hash"))
		return
	}

	hash, err := chainhash.NewHashFromStr(blockHash)
	if err != nil {
		sendBadRequest(w, err)
		return
	}

	block, err := s.chain.GetBlock(hash)
	if err != nil {
		sendError(w, err)
		return
	}

	sendBinary(w, block, maxAgeDisk)
}

func (s *server) txOutProofRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	vars := mux.Vars(r)
	txid := vars["txid"]

	if len(txid) != hex.EncodedLen(chainhash.HashSize) {
		sendBadRequest(w, fmt.Errorf("invalid transaction hash"))
		return
	}

	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		sendBadRequest(w, err)
		return
	}

	merkleBlock, err := s.chain.GetTxOutProof([]string{hash.String()}, nil)
	if err != nil {
		sendError(w, err)
		return
	}

	blockHash := merkleBlock.Header.BlockHash()
	verboseHeader, err := s.chain.GetBlockHeaderVerbose(&blockHash)
	if err != nil {
		sendError(w, err)
		return
	}

	var buf bytes.Buffer
	err = merkleBlock.BtcEncode(
		&buf, wire.ProtocolVersion, wire.WitnessEncoding,
	)
	if err != nil {
		sendError(w, err)
		return
	}

	maxAge := maxAgeDisk
	safeHeight := s.currentHeight.Load() - int32(s.reOrgSafeDepth)
	if verboseHeader.Height > safeHeight {
		maxAge = maxAgeTemporary
	}

	sendRawBytes(w, buf.Bytes(), maxAge)
}

func (s *server) rawTxRequestHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	txid := vars["txid"]

	if len(txid) != hex.EncodedLen(chainhash.HashSize) {
		sendBadRequest(w, fmt.Errorf("invalid transaction hash"))
		return
	}

	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		sendBadRequest(w, err)
		return
	}

	tx, err := s.chain.GetRawTransaction(hash)
	if err != nil {
		sendError(w, err)
		return
	}

	sendBinary(w, tx.MsgTx(), maxAgeDisk)
}

func sendJSON(w http.ResponseWriter, v interface{}, maxAge time.Duration) {
	addCacheHeaders(w, maxAge)
	addCorsHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	err := json.NewEncoder(w).Encode(v)
	if err != nil {
		log.Errorf("Error serializing status: %v", err)
	}
}

func sendBinary(w http.ResponseWriter, v serializable, maxAge time.Duration) {
	addCacheHeaders(w, maxAge)
	addCorsHeaders(w)
	w.WriteHeader(http.StatusOK)
	err := v.Serialize(w)
	if err != nil {
		log.Errorf("Error serializing: %v", err)
	}
}

func sendRawBytes(w http.ResponseWriter, payload []byte, maxAge time.Duration) {
	addCacheHeaders(w, maxAge)
	addCorsHeaders(w)
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(payload)
	if err != nil {
		log.Errorf("Error serializing: %v", err)
	}
}

func addCacheHeaders(w http.ResponseWriter, maxAge time.Duration) {
	// A max-age of 0 means no caching at all, as something is not safe
	// to cache yet.
	if maxAge == 0 {
		w.Header().Add("Cache-Control", "no-cache")

		return
	}

	w.Header().Add(
		"Cache-Control",
		fmt.Sprintf("max-age=%d", int64(maxAge.Seconds())),
	)
}

// addCorsHeaders adds HTTP header fields that are required for Cross Origin
// Resource Sharing. These header fields are needed to signal to the browser
// that it's ok to allow requests to subdomains, even if the JS was served from
// the top level domain.
func addCorsHeaders(w http.ResponseWriter) {
	w.Header().Add("Access-Control-Allow-Origin", "*")
	w.Header().Add("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
}

func parseRequestParamInt64(r *http.Request, name string) (int64, error) {
	vars := mux.Vars(r)
	paramStr := vars[name]

	if len(paramStr) == 0 {
		return 0, fmt.Errorf("invalid value for parameter %s", name)
	}

	paramValue, err := strconv.ParseInt(paramStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid value for parameter %s", name)
	}

	return paramValue, nil
}

func sendUnavailable(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(err.Error()))
}

func sendBadRequest(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(err.Error()))
}

func sendError(w http.ResponseWriter, err error) {
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(err.Error()))
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

func streamFile(w io.Writer, fileName string) error {
	f, err := os.Open(fileName)
	if err != nil {
		return err
	}

	_, err = io.Copy(w, f)
	return err
}
