package main

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
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

	// errUnavailableInLightMode is an error indicating that a certain HTTP
	// endpoint isn't available when running in light mode.
	errUnavailableInLightMode = errors.New(
		"endpoint not available in light mode",
	)

	// errUnavailableSPTweakDataTurnedOff is an error indicating that the SP
	// tweak data indexing is turned off.
	errUnavailableSPTweakDataTurnedOff = errors.New(
		"SP tweak data indexing is turned off",
	)

	// errStillStartingUp is an error indicating that the server is still
	// starting up and not ready to serve requests yet.
	errStillStartingUp = errors.New(
		"server still starting up, please try again later",
	)

	// errInvalidSyncStatus is an error indicating that the sync status is
	// invalid, caused by a bad configuration or unexpected behavior of the
	// backend.
	errInvalidSyncStatus = errors.New("invalid sync status")

	// errInvalidHashLength is an error indicating that the provided hash
	// length is invalid.
	errInvalidHashLength = errors.New("invalid hash length")

	// errInvalidBlockHash is an error indicating that the provided block
	// hash is invalid.
	errInvalidBlockHash = errors.New("invalid block hash")

	// errInvalidTxHash is an error indicating that the provided transaction
	// hash is invalid.
	errInvalidTxHash = errors.New("invalid transaction hash")

	//go:embed index.html
	indexHTML string
)

const (
	// headerImportVersion is the current version of the import file format.
	headerImportVersion = 0

	HeaderCache       = "Cache-Control"
	HeaderCORS        = "Access-Control-Allow-Origin"
	HeaderCORSMethods = "Access-Control-Allow-Methods"

	status400 = http.StatusBadRequest
	status500 = http.StatusInternalServerError
	status503 = http.StatusServiceUnavailable
)

type serializable interface {
	Serialize(w io.Writer) error
}

type blockProcessor interface {
	isStartupComplete() bool
	getCurrentHeight() int32
}

func (s *server) createRouter() *mux.Router {
	router := mux.NewRouter()
	router.HandleFunc("/", s.indexRequestHandler)
	router.HandleFunc("/index.html", s.indexRequestHandler)
	router.HandleFunc("/status", s.statusRequestHandler)
	router.HandleFunc("/headers/{height:[0-9]+}", s.headersRequestHandler)
	router.HandleFunc(
		"/headers/import/{height:[0-9]+}",
		s.headersImportRequestHandler,
	)
	router.HandleFunc(
		"/filter-headers/{height:[0-9]+}",
		s.filterHeadersRequestHandler,
	)
	router.HandleFunc(
		"/filter-headers/import/{height:[0-9]+}",
		s.filterHeadersImportRequestHandler,
	)
	router.HandleFunc("/filters/{height:[0-9]+}", s.filtersRequestHandler)
	router.HandleFunc(
		"/sp/tweak-data/{height:[0-9]+}",
		s.spTweakDataRequestHandler,
	)
	router.HandleFunc("/block/{hash:[0-9a-f]+}", s.blockRequestHandler)
	router.HandleFunc(
		"/tx/out-proof/{txid:[0-9a-f]+}", s.txOutProofRequestHandler,
	)
	router.HandleFunc(
		"/tx/raw/{txid:[0-9a-f]+}", s.rawTxRequestHandler,
	)

	return router
}

func (s *server) indexRequestHandler(w http.ResponseWriter, _ *http.Request) {
	addCorsHeaders(w)
	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(indexHTML))
}

func (s *server) statusRequestHandler(w http.ResponseWriter, _ *http.Request) {
	s.h2hCache.RLock()
	defer s.h2hCache.RUnlock()

	bestHeight := s.headerFiles.getCurrentHeight()
	bestBlock, err := s.h2hCache.getBlockHash(bestHeight)
	if err != nil {
		sendError(w, status500, errInvalidSyncStatus)
		return
	}

	bestFilter, ok := s.headerFiles.filterHeaders[*bestBlock]
	if !ok {
		sendError(w, status500, errInvalidSyncStatus)
		return
	}

	var (
		spHeight int32
		spSynced bool
	)
	if s.spTweakFiles != nil {
		spHeight = s.spTweakFiles.currentHeight.Load()
		spSynced = bestHeight == spHeight
	}

	status := &Status{
		ChainGenesisHash:      s.chainParams.GenesisHash.String(),
		ChainName:             s.chainParams.Name,
		BestBlockHeight:       bestHeight,
		BestBlockHash:         bestBlock.String(),
		BestFilterHeight:      s.cFilterFiles.currentHeight.Load(),
		BestFilterHeader:      bestFilter.String(),
		BestSPTweakHeight:     spHeight,
		EntriesPerHeaderFile:  s.headersPerFile,
		EntriesPerFilterFile:  s.filtersPerFile,
		EntriesPerSPTweakFile: s.spTweaksPerFile,
	}

	// nolint:gocritic
	status.AllFilesSynced = bestHeight == status.BestFilterHeight &&
		spSynced

	sendJSON(w, status, maxAgeMemory)
}

func (s *server) headersRequestHandler(w http.ResponseWriter, r *http.Request) {
	s.heightBasedRequestHandler(
		w, r, HeaderFileDir, HeaderFileNamePattern,
		int64(s.headersPerFile), s.headerFiles.serializeHeaders,
		s.headerFiles,
	)
}

func (s *server) headersImportRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	s.heightBasedImportRequestHandler(
		w, r, HeaderFileDir, HeaderFileNamePattern, s.headersPerFile,
		s.headerFiles.serializeHeaders, typeBlockHeader,
	)
}

func (s *server) filterHeadersRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	s.heightBasedRequestHandler(
		w, r, HeaderFileDir, FilterHeaderFileNamePattern,
		int64(s.headersPerFile), s.headerFiles.serializeFilterHeaders,
		s.headerFiles,
	)
}

func (s *server) filterHeadersImportRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	s.heightBasedImportRequestHandler(
		w, r, HeaderFileDir, FilterHeaderFileNamePattern,
		s.headersPerFile, s.headerFiles.serializeFilterHeaders,
		typeFilterHeader,
	)
}

func (s *server) filtersRequestHandler(w http.ResponseWriter, r *http.Request) {
	s.heightBasedRequestHandler(
		w, r, FilterFileDir, FilterFileNamePattern,
		int64(s.filtersPerFile), s.cFilterFiles.serializeFilters,
		s.cFilterFiles,
	)
}

func (s *server) spTweakDataRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	if s.spTweakFiles == nil {
		sendError(w, status503, errUnavailableSPTweakDataTurnedOff)
		return
	}

	s.heightBasedRequestHandler(
		w, r, SPTweakFileDir, SPTweakFileNamePattern,
		int64(s.spTweaksPerFile), s.spTweakFiles.serializeSPTweakData,
		s.spTweakFiles,
	)
}

func (s *server) heightBasedRequestHandler(w http.ResponseWriter,
	r *http.Request, subDir, fileNamePattern string, entriesPerFile int64,
	serializeCb func(w io.Writer, startIndex, endIndex int32) error,
	processor blockProcessor) {

	// These kinds of requests aren't available in light mode.
	if s.lightMode {
		sendError(w, status503, errUnavailableInLightMode)
		return
	}

	if !processor.isStartupComplete() {
		sendError(w, status503, errStillStartingUp)
		return
	}

	startHeight, err := parseRequestParamInt64(r, "height")
	if err != nil {
		sendError(w, status400, err)
		return
	}

	err = s.checkStartHeight(processor, startHeight, int32(entriesPerFile))
	if err != nil {
		sendError(w, status400, err)
		return
	}

	srcDir := filepath.Join(s.baseDir, subDir)
	fileName := fmt.Sprintf(
		fileNamePattern, srcDir, startHeight,
		startHeight+entriesPerFile-1,
	)
	if fileExists(fileName) {
		addCorsHeaders(w)
		addCacheHeaders(w, maxAgeDisk)
		w.WriteHeader(http.StatusOK)
		if err := streamFile(w, fileName); err != nil {
			log.Errorf("Error while streaming file: %v", err)
			sendError(w, status500, err)
		}

		return
	}

	s.h2hCache.RLock()
	defer s.h2hCache.RUnlock()

	if _, err := s.h2hCache.getBlockHash(int32(startHeight)); err != nil {
		sendError(w, status400, fmt.Errorf("invalid height"))
		return
	}

	// The requested start height wasn't yet in a file, so we need to
	// stream the headers from memory.
	addCorsHeaders(w)
	addCacheHeaders(w, maxAgeMemory)
	w.WriteHeader(http.StatusOK)
	err = serializeCb(w, int32(startHeight), processor.getCurrentHeight())
	if err != nil {
		log.Errorf("Error serializing: %v", err)
	}
}

func (s *server) heightBasedImportRequestHandler(w http.ResponseWriter,
	r *http.Request, subDir, fileNamePattern string, entriesPerFile int32,
	serializeCb func(w io.Writer, startIndex, endIndex int32) error,
	fileType byte) {

	// These kinds of requests aren't available in light mode.
	if s.lightMode {
		sendError(w, status503, errUnavailableInLightMode)
		return
	}

	if !s.headerFiles.startupComplete.Load() {
		sendError(w, status503, errStillStartingUp)
		return
	}

	endHeight, err := parseRequestParamInt64(r, "height")
	if err != nil {
		sendError(w, status400, err)
		return
	}

	err = s.checkEndHeight(s.headerFiles, endHeight, entriesPerFile)
	if err != nil {
		sendError(w, status400, err)
		return
	}

	// We allow the end height to be equal to the current height, which
	// we serve content from memory. In that case we mark the whole file as
	// short-term cacheable only.
	cache := maxAgeMemory

	// We also check that we don't need to server partial content from
	// files, as that would make things a bit more tricky.
	maxCacheFileEndHeight := int64(
		(s.headerFiles.currentHeight.Load() / entriesPerFile) *
			entriesPerFile,
	)

	// We don't want to return partial files. So for any range that can be
	// served only from files (i.e. up to the last complete cache file), we
	// require the end height to be a multiple of the entries per file.
	if endHeight <= maxCacheFileEndHeight {
		// We're in the file-only range, so we can set the cache
		// duration to disk cache time.
		cache = maxAgeDisk

		// Make sure we'll be able to serve a full cache file.
		if endHeight%int64(entriesPerFile) != 0 {
			err = fmt.Errorf("invalid end height %d, must be a "+
				"multiple of %d", endHeight, entriesPerFile)
			sendError(w, status400, err)
			return
		}
	}

	addCorsHeaders(w)
	addCacheHeaders(w, cache)
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

	// lastHeight is the beginning block of each cache file.
	lastHeight := int64(0)
	for ; lastHeight <= endHeight; lastHeight += int64(entriesPerFile) {
		// We always start at 0, so we'll always have one entry less
		// in the files than the even height we require the user to
		// enter (non-inclusive, as described in the API docs).
		if lastHeight == endHeight {
			return
		}

		srcDir := filepath.Join(s.baseDir, subDir)
		fileName := fmt.Sprintf(
			fileNamePattern, srcDir, lastHeight,
			lastHeight+int64(entriesPerFile)-1,
		)

		if !fileExists(fileName) {
			break
		}

		if err := streamFile(w, fileName); err != nil {
			log.Errorf("Error while streaming file: %v", err)
			sendError(w, status500, err)
		}
	}

	s.h2hCache.RLock()
	defer s.h2hCache.RUnlock()

	if _, err := s.h2hCache.getBlockHash(int32(lastHeight)); err != nil {
		sendError(w, status400, fmt.Errorf("invalid height"))
		return
	}

	// The requested end height goes over what's in files, so we need to
	// stream the remaining headers from memory.
	err = serializeCb(w, int32(lastHeight), int32(endHeight))
	if err != nil {
		log.Errorf("Error serializing: %v", err)
	}
}

func (s *server) blockRequestHandler(w http.ResponseWriter, r *http.Request) {
	blockHash, err := parseRequestParamChainHash(r, "hash")
	if err != nil {
		sendError(w, status400, fmt.Errorf("%w: %w",
			errInvalidBlockHash, err))
		return
	}

	block, err := s.chain.GetBlock(blockHash)
	if err != nil {
		sendError(w, status500, err)
		return
	}

	sendBinary(w, block, maxAgeDisk)
}

func (s *server) txOutProofRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	txHash, err := parseRequestParamChainHash(r, "txid")
	if err != nil {
		sendError(w, status400, fmt.Errorf("%w: %w", errInvalidTxHash,
			err))
		return
	}

	merkleBlock, err := s.chain.GetTxOutProof(
		[]string{txHash.String()}, nil,
	)
	if err != nil {
		sendError(w, status500, err)
		return
	}

	blockHash := merkleBlock.Header.BlockHash()
	verboseHeader, err := s.chain.GetBlockHeaderVerbose(&blockHash)
	if err != nil {
		sendError(w, status500, err)
		return
	}

	var buf bytes.Buffer
	err = merkleBlock.BtcEncode(
		&buf, wire.ProtocolVersion, wire.WitnessEncoding,
	)
	if err != nil {
		sendError(w, status500, err)
		return
	}

	maxAge := maxAgeDisk
	safeHeight := s.headerFiles.currentHeight.Load() -
		int32(s.reOrgSafeDepth)
	if verboseHeader.Height > safeHeight {
		maxAge = maxAgeTemporary
	}

	sendRawBytes(w, buf.Bytes(), maxAge)
}

func (s *server) rawTxRequestHandler(w http.ResponseWriter, r *http.Request) {
	txHash, err := parseRequestParamChainHash(r, "txid")
	if err != nil {
		sendError(w, status400, fmt.Errorf("%w: %w", errInvalidTxHash,
			err))
		return
	}

	tx, err := s.chain.GetRawTransaction(txHash)
	if err != nil {
		sendError(w, status500, err)
		return
	}

	sendBinary(w, tx.MsgTx(), maxAgeDisk)
}

func (s *server) checkStartHeight(processor blockProcessor, height int64,
	entriesPerFile int32) error {

	if int32(height) > processor.getCurrentHeight() {
		return fmt.Errorf("start height %d is greater than current "+
			"height %d", height, processor.getCurrentHeight())
	}

	if height != 0 && height%int64(entriesPerFile) != 0 {
		return fmt.Errorf("invalid start height %d, must be zero or "+
			"a multiple of %d", height, entriesPerFile)
	}

	return nil
}

func (s *server) checkEndHeight(processor blockProcessor, height int64,
	entriesPerFile int32) error {

	if int32(height) > processor.getCurrentHeight() {
		return fmt.Errorf("end height %d is greater than current "+
			"height %d", height, processor.getCurrentHeight())
	}

	if height == 0 {
		return fmt.Errorf("invalid end height %d, must be a multiple "+
			"of %d", height, entriesPerFile)
	}

	return nil
}

func sendJSON(w http.ResponseWriter, v any, maxAge time.Duration) {
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
		w.Header().Add(HeaderCache, "no-cache")

		return
	}

	w.Header().Add(
		HeaderCache, fmt.Sprintf("max-age=%d", int64(maxAge.Seconds())),
	)
}

// addCorsHeaders adds HTTP header fields that are required for Cross Origin
// Resource Sharing. These header fields are needed to signal to the browser
// that it's ok to allow requests to subdomains, even if the JS was served from
// the top level domain.
func addCorsHeaders(w http.ResponseWriter) {
	w.Header().Add(HeaderCORS, "*")
	w.Header().Add(HeaderCORSMethods, "GET, POST, OPTIONS")
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

func parseRequestParamChainHash(r *http.Request, name string) (*chainhash.Hash,
	error) {

	vars := mux.Vars(r)
	blockHash := vars[name]

	if len(blockHash) != hex.EncodedLen(chainhash.HashSize) {
		return nil, errInvalidHashLength
	}

	hash, err := chainhash.NewHashFromStr(blockHash)
	if err != nil {
		return nil, err
	}

	return hash, nil
}

func sendError(w http.ResponseWriter, status int, err error) {
	// By default, we don't cache error responses.
	cache := time.Duration(0)

	// But if it's a user error (4xx), we can cache it for a short time.
	if status >= 400 && status < 500 {
		cache = maxAgeMemory
	}

	addCacheHeaders(w, cache)
	addCorsHeaders(w)
	w.WriteHeader(status)
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
