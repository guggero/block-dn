package main

import (
	_ "embed"
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
	"github.com/gorilla/mux"
)

var (
	maxAgeMemory = time.Minute
	maxAgeDisk   = time.Hour * 24 * 365

	//go:embed index.html
	indexHTML string
)

type serializable interface {
	Serialize(w io.Writer) error
}

func (s *server) indexRequestHandler(w http.ResponseWriter, _ *http.Request) {
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

	status := &Status{
		ChainGenesisHash: s.chainParams.GenesisHash.String(),
		ChainName:        s.chainParams.Name,
		BestBlockHeight:  bestHeight,
		BestBlockHash:    bestBlock.String(),
		EntriesPerHeader: HeadersPerFile,
		EntriesPerFilter: FiltersPerFile,
	}

	sendJSON(w, status, maxAgeMemory)
}

func (s *server) headersRequestHandler(w http.ResponseWriter, r *http.Request) {
	s.heightBasedRequestHandler(
		w, r, HeaderFileDir, HeaderFileNamePattern,
		HeadersPerFile, s.serializeHeaders,
	)
}

func (s *server) filterHeadersRequestHandler(w http.ResponseWriter,
	r *http.Request) {

	s.heightBasedRequestHandler(
		w, r, HeaderFileDir, FilterHeaderFileNamePattern,
		HeadersPerFile, s.serializeFilterHeaders,
	)
}

func (s *server) filtersRequestHandler(w http.ResponseWriter, r *http.Request) {
	s.heightBasedRequestHandler(
		w, r, FilterFileDir, FilterFileNamePattern, FiltersPerFile,
		s.serializeFilters,
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
	addCacheHeaders(w, maxAgeMemory)
	w.WriteHeader(http.StatusOK)
	err = serializeCb(w, int32(height), s.currentHeight.Load())
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

func sendJSON(w http.ResponseWriter, v interface{}, maxAge time.Duration) {
	addCacheHeaders(w, maxAge)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	err := json.NewEncoder(w).Encode(v)
	if err != nil {
		log.Errorf("Error serializing status: %v", err)
	}
}

func sendBinary(w http.ResponseWriter, v serializable, maxAge time.Duration) {
	addCacheHeaders(w, maxAge)
	w.WriteHeader(http.StatusOK)
	err := v.Serialize(w)
	if err != nil {
		log.Errorf("Error serializing: %v", err)
	}
}

func addCacheHeaders(w http.ResponseWriter, maxAge time.Duration) {
	w.Header().Add(
		"Cache-Control",
		fmt.Sprintf("max-age=%d", int64(maxAge.Seconds())),
	)
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
