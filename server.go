package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire"
	"github.com/gorilla/mux"
	"github.com/lightningnetwork/lnd/fn/v2"
)

var (
	defaultTimeout    = 5 * time.Second
	blockPollInterval = time.Second

	errServerShutdown = errors.New("server shutting down")
)

type server struct {
	lightMode   bool
	baseDir     string
	listenAddr  string
	chainCfg    *rpcclient.ConnConfig
	chainParams *chaincfg.Params
	chain       *rpcclient.Client
	router      *mux.Router
	httpServer  *http.Server

	currentHeight atomic.Int32

	heightToHash  map[int32]chainhash.Hash
	headers       map[chainhash.Hash]*wire.BlockHeader
	filterHeaders map[chainhash.Hash]*chainhash.Hash
	filters       map[chainhash.Hash][]byte

	cacheLock sync.RWMutex

	wg   sync.WaitGroup
	errs *fn.ConcurrentQueue[error]
	quit chan struct{}
}

func newServer(lightMode bool, baseDir, listenAddr string,
	chainCfg *rpcclient.ConnConfig, chainParams *chaincfg.Params) *server {

	s := &server{
		lightMode:   lightMode,
		baseDir:     baseDir,
		listenAddr:  listenAddr,
		chainCfg:    chainCfg,
		chainParams: chainParams,

		heightToHash: make(map[int32]chainhash.Hash, HeadersPerFile),
		headers: make(
			map[chainhash.Hash]*wire.BlockHeader, HeadersPerFile,
		),
		filterHeaders: make(
			map[chainhash.Hash]*chainhash.Hash, HeadersPerFile,
		),
		filters: make(map[chainhash.Hash][]byte, FiltersPerFile),

		errs: fn.NewConcurrentQueue[error](2),
		quit: make(chan struct{}),
	}

	router := mux.NewRouter()
	router.HandleFunc("/", s.indexRequestHandler)
	router.HandleFunc("/index.html", s.indexRequestHandler)
	router.HandleFunc("/status", s.statusRequestHandler)
	router.HandleFunc("/headers/{height:[0-9]+}", s.headersRequestHandler)
	router.HandleFunc(
		"/filter-headers/{height:[0-9]+}",
		s.filterHeadersRequestHandler,
	)
	router.HandleFunc("/filters/{height:[0-9]+}", s.filtersRequestHandler)
	router.HandleFunc("/block/{hash:[0-9a-f]+}", s.blockRequestHandler)

	s.router = router

	return s
}

func (s *server) start() error {
	client, err := rpcclient.New(s.chainCfg, nil)
	if err != nil {
		return fmt.Errorf("error connecting to bitcoind: %w", err)
	}
	s.chain = client

	s.httpServer = &http.Server{
		Addr:         s.listenAddr,
		Handler:      s.router,
		WriteTimeout: defaultTimeout,
		ReadTimeout:  defaultTimeout,
	}
	s.errs.Start()

	s.wg.Add(1)
	go func() {
		defer func() {
			s.wg.Done()
			log.Infof("Web server finished")
		}()

		log.Infof("Starting web server at %v", s.listenAddr)
		err := s.httpServer.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.errs.ChanIn() <- err
		}
	}()

	// If we're running in light mode, we don't need to create any files,
	// so we can just return here.
	if s.lightMode {
		return nil
	}

	s.wg.Add(1)
	go func() {
		defer func() {
			s.wg.Done()
			log.Infof("Background filter file update finished")
		}()

		log.Infof("Starting background filter file update")
		err := s.updateFiles()
		if err != nil && !errors.Is(err, errServerShutdown) {
			s.errs.ChanIn() <- err
		}
	}()

	return nil
}

func (s *server) stop() error {
	close(s.quit)

	log.Infof("Shutting down, waiting for background processes to finish")

	var stopErr error
	err := s.httpServer.Shutdown(context.Background())
	if err != nil {
		log.Errorf("Error shutting down web server: %v", err)
		stopErr = fmt.Errorf("error shutting down web server: %w", err)
	}

	s.wg.Wait()
	s.errs.Stop()

	select {
	case err, ok := <-s.errs.ChanOut():
		if ok {
			log.Errorf("Error shutting down: %v", err)
			stopErr = fmt.Errorf("error shutting down: %w", err)
		}

	default:
	}

	s.chain.Shutdown()

	log.Infof("Shutdown complete")

	return stopErr
}

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
	startBlock = (startBlock / HeadersPerFile) * HeadersPerFile
	log.Debugf("Need to start fetching headers and filters from block %d",
		startBlock)

	log.Debugf("Writing header files from block %d to block %d", startBlock,
		info.Blocks)
	err = s.updateCacheAndFiles(startBlock, info.Blocks)
	if err != nil {
		return fmt.Errorf("error updating blocks: %w", err)
	}

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

		if (i+1)%FiltersPerFile == 0 {
			fileStart := i - FiltersPerFile + 1
			filterFileName := fmt.Sprintf(
				FilterFileNamePattern, filterDir, fileStart, i,
			)

			log.Debugf("Reached header %d, writing file starting "+
				"at %d, containing %d filters to %s", i,
				fileStart, FiltersPerFile, filterFileName)

			err = s.writeFilters(filterFileName, fileStart, i)
			if err != nil {
				return fmt.Errorf("error writing filters: %w",
					err)
			}
		}

		if (i+1)%HeadersPerFile == 0 {
			fileStart := i - HeadersPerFile + 1
			headerFileName := fmt.Sprintf(
				HeaderFileNamePattern, headerDir, fileStart, i,
			)
			filterHeaderFileName := fmt.Sprintf(
				FilterHeaderFileNamePattern, headerDir,
				fileStart, i,
			)

			log.Debugf("Reached header %d, writing file starting "+
				"at %d, containing %d headers to %s", i,
				fileStart, HeadersPerFile, headerFileName)

			err = s.writeHeaders(headerFileName, fileStart, i)
			if err != nil {
				return fmt.Errorf("error writing headers: %w",
					err)
			}

			log.Debugf("Reached header %d, writing file starting "+
				"at %d, containing %d filter headers to %s", i,
				fileStart, HeadersPerFile, filterHeaderFileName)

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
	s.heightToHash = make(map[int32]chainhash.Hash, HeadersPerFile)
	s.headers = make(
		map[chainhash.Hash]*wire.BlockHeader, HeadersPerFile,
	)
	s.filterHeaders = make(
		map[chainhash.Hash]*chainhash.Hash, HeadersPerFile,
	)
	s.filters = make(map[chainhash.Hash][]byte, FiltersPerFile)
	s.cacheLock.Unlock()
}
