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

	"github.com/btcsuite/btcd/btcjson"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/gorilla/mux"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
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

	// errUnavailableCustomFiltersTurnedOff is an error indicating that
	// the custom filter indexing is turned off.
	errUnavailableCustomFiltersTurnedOff = errors.New(
		"custom filter indexing is turned off",
	)

	// errUnknownCustomFilterType is an error indicating that the
	// requested custom filter type doesn't exist.
	errUnknownCustomFilterType = errors.New(
		"unknown custom filter type",
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

	// errInvalidOutputIndex is an error indicating that the provided output
	// index (vout) is invalid.
	errInvalidOutputIndex = errors.New("invalid output index")

	// errFeeEsimatesUnavailable is an error indicating the current fee
	// rates cannot be estimated.
	errFeeEsimatesUnavailable = errors.New("fee rate estimates unavailable")

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
	getLastSealedHeight() int32
}

func (s *server) createRouter() *mux.Router {
	router := mux.NewRouter()
	router.HandleFunc("/", s.indexRequestHandler)
	router.HandleFunc("/index.html", s.indexRequestHandler)
	router.HandleFunc("/status", s.statusRequestHandler)
	router.HandleFunc("/headers/{height:[0-9]+}", s.headersRequestHandler)
	router.HandleFunc(
		"/headers/import/latest",
		s.headersImportLatestRequestHandler,
	)
	router.HandleFunc(
		"/headers/import/{height:[0-9]+}",
		s.headersImportRequestHandler,
	)
	router.HandleFunc(
		"/filter-headers/{height:[0-9]+}",
		s.filterHeadersRequestHandler,
	)
	router.HandleFunc(
		"/filter-headers/import/latest",
		s.filterHeadersImportLatestRequestHandler,
	)
	router.HandleFunc(
		"/filter-headers/import/{height:[0-9]+}",
		s.filterHeadersImportRequestHandler,
	)
	router.HandleFunc(
		"/filter-headers/type/{filterType}/{height:[0-9]+}",
		s.customFilterHeadersRequestHandler,
	)
	router.HandleFunc("/filters/{height:[0-9]+}", s.filtersRequestHandler)
	router.HandleFunc(
		"/filters/single/{height:[0-9]+}",
		s.filterSingleRequestHandler,
	)
	router.HandleFunc(
		"/filters/type/{filterType}/{height:[0-9]+}",
		s.customFiltersRequestHandler,
	)
	router.HandleFunc(
		"/sp/tweak-data/{height:[0-9]+}",
		s.spTweakDataRequestHandler,
	)
	router.HandleFunc("/block/{hash:[0-9a-f]+}", s.blockRequestHandler)
	router.HandleFunc(
		"/block/{hash:[0-9a-f]+}/spenttxouts",
		s.blockSpentTxOutsRequestHandler,
	)
	router.HandleFunc(
		"/tx/out-proof/{txid:[0-9a-f]+}", s.txOutProofRequestHandler,
	)
	router.HandleFunc(
		"/tx/raw/{txid:[0-9a-f]+}", s.rawTxRequestHandler,
	)
	router.HandleFunc(
		"/utxo/{txid:[0-9a-f]+}-{vout:[0-9]+}", s.utxoRequestHandler,
	)
	router.HandleFunc(
		"/fees/estimate/{blocks:[0-9]+}", s.estimateFeeRequestHandler,
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

	bestFilter, ok := s.headerFiles.filterHeaderAtHeight(bestHeight)
	if !ok {
		sendError(w, status500, errInvalidSyncStatus)
		return
	}

	var (
		spHeight     int32
		filterHeight = s.cFilterFiles.currentHeight.Load()
		allSynced    = bestHeight == filterHeight
	)
	if s.spTweakFiles != nil {
		spHeight = s.spTweakFiles.currentHeight.Load()
		allSynced = allSynced && bestHeight == spHeight
	}

	status := &Status{
		Version:               version,
		Commit:                Commit,
		ChainGenesisHash:      s.chainParams.GenesisHash.String(),
		ChainName:             s.chainParams.Name,
		BestBlockHeight:       bestHeight,
		BestBlockHash:         bestBlock.String(),
		BestFilterHeight:      filterHeight,
		BestFilterHeader:      bestFilter.String(),
		BestSPTweakHeight:     spHeight,
		EntriesPerHeaderFile:  s.headersPerFile,
		EntriesPerFilterFile:  s.filtersPerFile,
		EntriesPerSPTweakFile: s.spTweaksPerFile,
		AllFilesSynced:        allSynced,
	}

	sendJSON(w, status, maxAgeMemory)
}

func (s *server) headersRequestHandler(w http.ResponseWriter, r *http.Request) {
	s.heightBasedRequestHandler(
		w, r, HeaderFileDir, HeaderFileNamePattern,
		int64(s.headersPerFile), s.headerFiles.serializeHeaders,
		s.headerFiles.headersSize, s.headerFiles,
	)
}

func (s *server) headersImportRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	s.heightBasedImportRequestHandler(
		w, r, HeaderFileDir, HeaderFileNamePattern, s.headersPerFile,
		s.headerFiles.serializeHeaders, typeBlockHeader,
	)
}

// headersImportLatestRequestHandler serves /headers/import/latest, returning
// every block header from height 0 up to and including the server's current
// tip. This is the convenience shortcut for clients (test runners,
// neutrino-style importers) that don't want to discover the current height
// before requesting the import file. It always serves at the memory tier
// since the tip is mutable.
func (s *server) headersImportLatestRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	s.heightBasedImportLatestHandler(
		w, r, HeaderFileDir, HeaderFileNamePattern, s.headersPerFile,
		s.headerFiles.serializeHeaders, typeBlockHeader,
	)
}

func (s *server) filterHeadersRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	s.heightBasedRequestHandler(
		w, r, HeaderFileDir, FilterHeaderFileNamePattern,
		int64(s.headersPerFile), s.headerFiles.serializeFilterHeaders,
		s.headerFiles.filterHeadersSize, s.headerFiles,
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

// filterHeadersImportLatestRequestHandler is the /filter-headers/import/latest
// counterpart to headersImportLatestRequestHandler.
func (s *server) filterHeadersImportLatestRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	s.heightBasedImportLatestHandler(
		w, r, HeaderFileDir, FilterHeaderFileNamePattern,
		s.headersPerFile, s.headerFiles.serializeFilterHeaders,
		typeFilterHeader,
	)
}

func (s *server) filtersRequestHandler(w http.ResponseWriter, r *http.Request) {
	s.heightBasedRequestHandler(
		w, r, FilterFileDir, FilterFileNamePattern,
		int64(s.filtersPerFile), s.cFilterFiles.serializeFilters,
		s.cFilterFiles.filtersSize, s.cFilterFiles,
	)
}

// customFilterConfig resolves the {filterType} route variable to a custom
// filter configuration index, writing the appropriate error response if the
// custom filter producer isn't running or the type doesn't exist.
func (s *server) customFilterConfig(w http.ResponseWriter,
	r *http.Request) (string, int, bool) {

	if s.customFilterFiles == nil {
		sendError(w, status503, errUnavailableCustomFiltersTurnedOff)
		return "", 0, false
	}

	filterType := mux.Vars(r)["filterType"]
	cfgIndex, ok := customFilterConfigIndex(filterType)
	if !ok {
		err := fmt.Errorf("%w: %s", errUnknownCustomFilterType,
			filterType)
		sendError(w, status400, err)
		return "", 0, false
	}

	return filterType, cfgIndex, true
}

// customFiltersRequestHandler serves /filters/type/{filterType}/{height}:
// the batched filter files of one output-type-restricted filter set, in the
// same format as the basic /filters/{height} endpoint.
func (s *server) customFiltersRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	filterType, cfgIndex, ok := s.customFilterConfig(w, r)
	if !ok {
		return
	}

	s.heightBasedRequestHandler(
		w, r, customFilterDir(filterType), FilterFileNamePattern,
		int64(s.filtersPerFile),
		func(w io.Writer, startIndex, endIndex int32) error {
			return s.customFilterFiles.serializeFilters(
				w, cfgIndex, startIndex, endIndex,
			)
		},
		func(startIndex, endIndex int32) (int64, bool) {
			return s.customFilterFiles.filtersSize(
				cfgIndex, startIndex, endIndex,
			)
		},
		s.customFilterFiles,
	)
}

// customFilterHeadersRequestHandler serves the endpoint
// /filter-headers/type/{filterType}/{height}: the batched filter header
// files of one output-type-restricted filter set, in the same format (and
// with the same entries per file) as the basic /filter-headers/{height}
// endpoint.
func (s *server) customFilterHeadersRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	filterType, cfgIndex, ok := s.customFilterConfig(w, r)
	if !ok {
		return
	}

	s.heightBasedRequestHandler(
		w, r, customHeaderDir(filterType), FilterHeaderFileNamePattern,
		int64(s.headersPerFile),
		func(w io.Writer, startIndex, endIndex int32) error {
			return s.customFilterFiles.serializeFilterHeaders(
				w, cfgIndex, startIndex, endIndex,
			)
		},
		s.customFilterFiles.filterHeadersSize, s.customFilterFiles,
	)
}

// filterSingleRequestHandler serves /filters/single/{height}: the raw BIP158
// basic filter of a single block, without a var-int length prefix (the length
// is the Content-Length). The batch /filters/{height} route only accepts
// file-aligned start heights, which would force a tip-following client to
// re-download the whole unsealed tail file for every new block; this endpoint
// is the cheap alternative. It works in light mode too, since it's backed by
// the node's own filter index rather than the on-disk files.
func (s *server) filterSingleRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	height, err := parseRequestParamInt64(r, "height")
	if err != nil {
		sendError(w, status400, err)
		return
	}

	// Serve from the in-memory tail if this height hasn't been sealed to
	// disk yet. This avoids a backend round trip for the hot tip range.
	if !s.lightMode && s.cFilterFiles.isStartupComplete() {
		current := int64(s.cFilterFiles.getCurrentHeight())
		if height > current {
			sendError(w, status400, fmt.Errorf("start height %d "+
				"is greater than current height %d", height,
				current))
			return
		}

		if filter, ok := s.cFilterFiles.filterAtHeight(
			int32(height),
		); ok {
			sendRawBytes(w, filter, maxAgeMemory)
			return
		}
	}

	// The height is in the sealed range (or we're in light mode), so ask
	// the backend's filter index directly. Sealed heights are safe to
	// cache at the disk tier; anything within the reorg window stays at
	// the memory tier.
	hash, err := s.h2hCache.getBlockHash(int32(height))
	if err != nil {
		sendError(w, status400, fmt.Errorf("invalid height"))
		return
	}

	filter, err := s.chain.GetBlockFilter(*hash, &filterBasic)
	if err != nil {
		sendError(w, status500, err)
		return
	}

	filterBytes, err := hex.DecodeString(filter.Filter)
	if err != nil {
		sendError(w, status500, err)
		return
	}

	maxAge := maxAgeMemory
	if !s.lightMode &&
		int64(s.cFilterFiles.getLastSealedHeight()) >= height {

		maxAge = maxAgeDisk
	}

	sendRawBytes(w, filterBytes, maxAge)
}

func (s *server) spTweakDataRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	if s.spTweakFiles == nil {
		sendError(w, status503, errUnavailableSPTweakDataTurnedOff)
		return
	}

	// SP tweak data is variable-size JSON, so no size callback: computing
	// it would mean serializing twice.
	s.heightBasedRequestHandler(
		w, r, SPTweakFileDir, SPTweakFileNamePattern,
		int64(s.spTweaksPerFile), s.spTweakFiles.serializeSPTweakData,
		nil, s.spTweakFiles,
	)
}

func (s *server) estimateFeeRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	confTarget, err := parseRequestParamInt64(r, "blocks")
	if err != nil {
		sendError(w, status500, errFeeEsimatesUnavailable)
		return
	}

	feeRateResult, err := s.chain.EstimateSmartFee(
		confTarget, &btcjson.EstimateModeConservative,
	)
	if err != nil {
		sendError(w, status500, errFeeEsimatesUnavailable)
		return
	}

	if len(feeRateResult.Errors) != 0 || feeRateResult.FeeRate == nil {
		sendError(w, status500, errFeeEsimatesUnavailable)
		return
	}

	log.Debugf("Estimated fee rate for conf target %d: %f BTC/kVB",
		confTarget, *feeRateResult.FeeRate)

	// Bitcoind returns the fee rate expressed as BTC/kVB.
	feeKVB := chainfee.SatPerKVByte(
		*feeRateResult.FeeRate * btcutil.SatoshiPerBitcoin,
	)
	feeKWU := feeKVB.FeePerKWeight()
	feeVB := feeKWU.FeePerVByte()
	rate := &FeeRate{
		FeeSatPerKVByte:  int64(feeKVB),
		FeeSatPerKWeight: int64(feeKWU),
		FeeSatPerVByte:   int64(feeVB),
	}

	sendJSON(w, rate, maxAgeMemory)
}

func (s *server) heightBasedRequestHandler(w http.ResponseWriter,
	r *http.Request, subDir, fileNamePattern string, entriesPerFile int64,
	serializeCb func(w io.Writer, startIndex, endIndex int32) error,
	sizeCb func(startIndex, endIndex int32) (int64, bool),
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

		// Serving the sealed file through http.ServeContent gives
		// clients Content-Length, Last-Modified/If-Modified-Since and
		// HTTP range requests — the latter lets a browser client
		// resume a large filter file download instead of restarting
		// it.
		serveFile(w, r, fileName)

		return
	}

	s.h2hCache.RLock()
	defer s.h2hCache.RUnlock()

	if _, err := s.h2hCache.getBlockHash(int32(startHeight)); err != nil {
		sendError(w, status400, fmt.Errorf("invalid height"))
		return
	}

	// The requested start height wasn't yet in a file, so we need to
	// stream the headers from memory. The exact response size is known
	// for fixed-size entries (and cheaply computable for filters), so
	// announce it where possible to let clients detect truncation.
	endHeight := processor.getCurrentHeight()
	addCorsHeaders(w)
	addCacheHeaders(w, maxAgeMemory)
	if sizeCb != nil {
		if size, ok := sizeCb(int32(startHeight), endHeight); ok {
			w.Header().Set(
				"Content-Length", strconv.FormatInt(size, 10),
			)
		}
	}
	w.WriteHeader(http.StatusOK)
	err = serializeCb(w, int32(startHeight), endHeight)
	if err != nil {
		log.Errorf("Error serializing: %v", err)
	}
}

func (s *server) heightBasedImportRequestHandler(w http.ResponseWriter,
	r *http.Request, subDir, fileNamePattern string, entriesPerFile int32,
	serializeCb func(w io.Writer, startIndex, endIndex int32) error,
	fileType byte) {

	if !s.checkImportPreconditions(w) {
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

	s.serveHeightBasedImport(
		w, subDir, fileNamePattern, entriesPerFile, serializeCb,
		fileType, endHeight,
	)
}

// heightBasedImportLatestHandler is the shared implementation for the
// /headers/import/latest and /filter-headers/import/latest convenience
// routes. It resolves endHeight to the server's current tip + 1 (because
// endHeight is exclusive, so currentHeight+1 covers heights 0..currentHeight)
// and delegates to the same serving logic as the numeric route.
func (s *server) heightBasedImportLatestHandler(w http.ResponseWriter,
	_ *http.Request, subDir, fileNamePattern string, entriesPerFile int32,
	serializeCb func(w io.Writer, startIndex, endIndex int32) error,
	fileType byte) {

	if !s.checkImportPreconditions(w) {
		return
	}

	endHeight := int64(s.headerFiles.currentHeight.Load()) + 1

	s.serveHeightBasedImport(
		w, subDir, fileNamePattern, entriesPerFile, serializeCb,
		fileType, endHeight,
	)
}

// checkImportPreconditions enforces the two gates shared by every import
// endpoint (light mode and startup), writing the appropriate 503 error and
// returning false if either fails. Callers should bail when this returns
// false.
func (s *server) checkImportPreconditions(w http.ResponseWriter) bool {
	if s.lightMode {
		sendError(w, status503, errUnavailableInLightMode)
		return false
	}

	if !s.headerFiles.startupComplete.Load() {
		sendError(w, status503, errStillStartingUp)
		return false
	}

	return true
}

// serveHeightBasedImport writes the import metadata header and then streams
// headers from 0..endHeight-1 to w, sourced from on-disk files where
// available and from the in-memory tail otherwise. It assumes endHeight has
// already been validated (e.g., by checkEndHeight) or is server-resolved
// (e.g., from currentHeight+1 on the /latest route).
func (s *server) serveHeightBasedImport(w http.ResponseWriter,
	subDir, fileNamePattern string, entriesPerFile int32,
	serializeCb func(w io.Writer, startIndex, endIndex int32) error,
	fileType byte, endHeight int64) {

	// We allow the end height to be equal to the current height, which
	// we serve content from memory. In that case we mark the whole file as
	// short-term cacheable only.
	cache := maxAgeMemory

	// The cache-tier choice gates on whether every byte in the response
	// has already been written to a sealed file. That's
	// lastSealedHeight + 1 (the smallest endHeight that's fully covered by
	// on-disk files). Using currentHeight here would be wrong: with reorg-
	// safe sealing, a file boundary at or below currentHeight isn't
	// guaranteed to be on disk yet, and marking a memory-served (still
	// mutable) response as long-lived would re-create the very bug that
	// motivated reorg-safe sealing.
	lastSealedBoundary := s.headerFiles.lastSealedHeight.Load() + 1

	// We don't want to return partial files. So for any range that can be
	// served only from files (i.e. up to the last complete cache file), we
	// require the end height to be a multiple of the entries per file.
	if endHeight <= int64(lastSealedBoundary) {
		// We're in the file-only range, so we can set the cache
		// duration to disk cache time.
		cache = maxAgeDisk

		// Make sure we'll be able to serve a full cache file.
		if endHeight%int64(entriesPerFile) != 0 {
			err := fmt.Errorf("invalid end height %d, must be a "+
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
			// Response headers are already on the wire and we've
			// streamed an unknown number of bytes. Continuing the
			// loop would append the next file's bytes after a
			// truncated current file, producing a corrupt stream
			// that decodes as misaligned headers — exactly the
			// bug that motivated reorg-safe sealing on the
			// producer side. Just log and bail.
			log.Errorf("Error while streaming file %s: %v",
				fileName, err)
			return
		}
	}

	s.h2hCache.RLock()
	defer s.h2hCache.RUnlock()

	if _, err := s.h2hCache.getBlockHash(int32(lastHeight)); err != nil {
		sendError(w, status400, fmt.Errorf("invalid height"))
		return
	}

	// The requested end height goes over what's in files, so we need to
	// stream the remaining headers from memory. endHeight is exclusive
	// (matches the file path's behavior of stopping the loop when
	// lastHeight == endHeight without writing one more entry), so the
	// inclusive last index here is endHeight - 1.
	err := serializeCb(w, int32(lastHeight), int32(endHeight)-1)
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

func (s *server) blockSpentTxOutsRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	blockHash, err := parseRequestParamChainHash(r, "hash")
	if err != nil {
		sendError(w, status400, fmt.Errorf("%w: %w",
			errInvalidBlockHash, err))
		return
	}

	// We only allow "bin" or "hex to be set, anything else will default to
	// "json", which is the easiest format to read for humans.
	format := "json"
	queryFormat := r.URL.Query().Get("format")
	if queryFormat == "bin" || queryFormat == "hex" {
		format = queryFormat
	}

	url := fmt.Sprintf("%s/rest/spenttxouts/%s.%s",
		backendRESTURL(s.chainCfg), blockHash.String(), format)

	client := &http.Client{
		Timeout: backendRequestTimeout,
	}

	req, err := http.NewRequestWithContext(
		r.Context(), http.MethodGet, url, nil,
	)
	if err != nil {
		sendError(w, status500, err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}

		log.Errorf("Error %d fetching spent tx outs from %s: %v",
			code, url, err)
		sendError(w, status500, err)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Errorf("Error fetching spent tx outs from %s: %s",
			url, string(body))
		sendError(w, resp.StatusCode, fmt.Errorf("upstream error: "+
			"code %d", resp.StatusCode))
		return
	}

	sendJsonCopy(w, resp.Body, maxAgeDisk)
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

func (s *server) utxoRequestHandler(w http.ResponseWriter, r *http.Request) {
	txHash, err := parseRequestParamChainHash(r, "txid")
	if err != nil {
		sendError(w, status400, fmt.Errorf("%w: %w",
			errInvalidBlockHash, err))
		return
	}

	vOut, err := parseRequestParamInt64(r, "vout")
	if err != nil {
		sendError(w, status400, fmt.Errorf("%w: %w",
			errInvalidOutputIndex, err))
		return
	}

	mempool := ""
	if r.URL.Query().Get("mempool") == "true" {
		mempool = "checkmempool/"
	}

	// We only allow "bin" or "hex to be set, anything else will default to
	// "json", which is the easiest format to read for humans.
	format := "json"
	queryFormat := r.URL.Query().Get("format")
	if queryFormat == "bin" || queryFormat == "hex" {
		format = queryFormat
	}

	url := fmt.Sprintf("%s/rest/getutxos/%s%s-%d.%s",
		backendRESTURL(s.chainCfg), mempool, txHash.String(), vOut,
		format)

	client := &http.Client{
		Timeout: backendRequestTimeout,
	}

	req, err := http.NewRequestWithContext(
		r.Context(), http.MethodGet, url, nil,
	)
	if err != nil {
		sendError(w, status500, err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}

		log.Errorf("Error %d fetching utxo from %s: %v", code, url, err)
		sendError(w, status500, err)
		return
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Errorf("Error fetching utxo from %s: %s", url, string(body))
		sendError(w, resp.StatusCode, fmt.Errorf("upstream error: "+
			"code %d", resp.StatusCode))
		return
	}

	sendJsonCopy(w, resp.Body, maxAgeMemory)
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

	// endHeight is exclusive, so the maximum valid value is
	// currentHeight + 1 — that yields a response containing every header
	// up to and including the current tip. Rejecting at strictly greater
	// than currentHeight would make the tip block unreachable through the
	// numeric import URL (the convenience /latest route already uses
	// currentHeight + 1 internally).
	if int32(height) > processor.getCurrentHeight()+1 {
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

	// The payload is fully known here, so announce its length; explicitly
	// calling WriteHeader below disables Go's automatic inference.
	w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(http.StatusOK)
	_, err := w.Write(payload)
	if err != nil {
		log.Errorf("Error serializing: %v", err)
	}
}

func sendJsonCopy(w http.ResponseWriter, r io.Reader, maxAge time.Duration) {
	addCacheHeaders(w, maxAge)
	addCorsHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err := io.Copy(w, r)
	if err != nil {
		log.Errorf("Error copying response: %v", err)
	}
}

func addCacheHeaders(w http.ResponseWriter, maxAge time.Duration) {
	// A max-age of 0 means no caching at all, as something is not safe
	// to cache yet.
	if maxAge == 0 {
		w.Header().Set(HeaderCache, "no-cache")

		return
	}

	secs := int64(maxAge.Seconds())

	// The disk tier serves bytes that are durably on disk past the
	// reorg-safe depth. They effectively never change — but "effectively"
	// is not "never" (a reorg deeper than --reorg-safe-depth would
	// invalidate them), so we deliberately do NOT advertise `immutable`.
	// Instead we let intermediaries serve a stale response while they
	// revalidate in the background, so even browsers that already cached
	// bad bytes can self-heal after a manual CDN purge.
	if maxAge >= maxAgeDisk {
		w.Header().Set(HeaderCache, fmt.Sprintf(
			"public, max-age=%d, stale-while-revalidate=86400",
			secs,
		))

		return
	}

	w.Header().Set(
		HeaderCache, fmt.Sprintf("public, max-age=%d", secs),
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

	defer func(f *os.File) {
		err := f.Close()
		if err != nil {
			log.Errorf("Error closing file %s: %v", fileName, err)
		}
	}(f)

	_, err = io.Copy(w, f)
	return err
}

// serveFile serves a single sealed file via http.ServeContent, which handles
// Content-Length, Last-Modified/If-Modified-Since and range requests. Unlike
// streamFile it must own the status code (206 for ranges), so callers must
// not call WriteHeader themselves.
func serveFile(w http.ResponseWriter, r *http.Request, fileName string) {
	f, err := os.Open(fileName)
	if err != nil {
		log.Errorf("Error opening file %s: %v", fileName, err)
		sendError(w, status500, fmt.Errorf("error reading file"))
		return
	}

	defer func(f *os.File) {
		err := f.Close()
		if err != nil {
			log.Errorf("Error closing file %s: %v", fileName, err)
		}
	}(f)

	stat, err := f.Stat()
	if err != nil {
		log.Errorf("Error getting file info %s: %v", fileName, err)
		sendError(w, status500, fmt.Errorf("error reading file"))
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, stat.Name(), stat.ModTime(), f)
}
