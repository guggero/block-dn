package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/gorilla/mux"
	"github.com/lightningnetwork/lnd/fn/v2"
)

var (
	defaultTimeout    = 5 * time.Second
	blockPollInterval = time.Second

	errServerShutdown = errors.New("server shutting down")
)

type server struct {
	lightMode        bool
	indexSPTweakData bool
	baseDir          string
	listenAddr       string
	chainCfg         *rpcclient.ConnConfig
	chainParams      *chaincfg.Params
	reOrgSafeDepth   uint32
	chain            *rpcclient.Client
	router           *mux.Router
	httpServer       *http.Server

	headersPerFile  int32
	filtersPerFile  int32
	spTweaksPerFile int32

	h2hCache     *heightToHashCache
	headerFiles  *headerFiles
	cFilterFiles *cFilterFiles
	spTweakFiles *spTweakFiles

	wg   sync.WaitGroup
	errs *fn.ConcurrentQueue[error]
	quit chan struct{}
}

func newServer(lightMode, indexSPTweakData bool, baseDir, listenAddr string,
	chainCfg *rpcclient.ConnConfig, chainParams *chaincfg.Params,
	reOrgSafeDepth uint32, headersPerFile, filtersPerFile,
	spTweaksPerFile int32) *server {

	s := &server{
		lightMode:        lightMode,
		indexSPTweakData: indexSPTweakData,
		baseDir:          baseDir,
		listenAddr:       listenAddr,
		chainCfg:         chainCfg,
		chainParams:      chainParams,
		reOrgSafeDepth:   reOrgSafeDepth,

		headersPerFile:  headersPerFile,
		filtersPerFile:  filtersPerFile,
		spTweaksPerFile: spTweaksPerFile,

		errs: fn.NewConcurrentQueue[error](2),
		quit: make(chan struct{}),
	}

	s.router = s.createRouter()

	return s
}

func (s *server) start() error {
	client, err := rpcclient.New(s.chainCfg, nil)
	if err != nil {
		return fmt.Errorf("error connecting to bitcoind: %w", err)
	}
	s.chain = client

	s.h2hCache = newH2HCache(client)

	s.headerFiles = newHeaderFiles(
		s.headersPerFile, s.chain, s.quit, s.baseDir,
		s.chainParams, s.h2hCache,
	)
	s.cFilterFiles = newCFilterFiles(
		s.filtersPerFile, s.chain, s.quit, s.baseDir,
		s.chainParams, s.h2hCache,
	)

	// We preload all the headers into the height to hash cache on startup.
	headersDir := filepath.Join(s.baseDir, HeaderFileDir)
	cacheBestHeight, err := s.h2hCache.loadFromHeaders(headersDir)
	if err != nil {
		return fmt.Errorf("error loading headers into cache: %w", err)
	}

	// We also want to verify the current height on startup, if there were
	// any headers loaded into the cache.
	if cacheBestHeight >= 0 {
		cacheBestBlock, err := s.h2hCache.getBlockHash(cacheBestHeight)
		if err != nil {
			return fmt.Errorf("error getting best block from "+
				"cache: %w", err)
		}

		backendBlock, err := s.chain.GetBlockHash(
			int64(cacheBestHeight),
		)
		if err != nil {
			return fmt.Errorf("error getting best block from "+
				"backend: %w", err)
		}
		if *backendBlock != *cacheBestBlock {
			return fmt.Errorf("header mismatch at height %d: "+
				"cache has %s, backend has %s", cacheBestHeight,
				cacheBestBlock, backendBlock.String())
		}
	}

	info, err := s.chain.GetBlockChainInfo()
	if err != nil {
		return fmt.Errorf("error getting block chain info: %w", err)
	}

	log.Debugf("Backend best block hash: %s, height: %d",
		info.BestBlockHash, info.Blocks)

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
			log.Errorf("Error starting server: %v", err)
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
			log.Infof("Background header file update finished")
		}()

		log.Infof("Starting background header file update")
		err := s.headerFiles.updateFiles(info.Blocks)
		if err != nil && !errors.Is(err, errServerShutdown) {
			log.Errorf("Error updating header files: %v", err)
			s.errs.ChanIn() <- err
		}
	}()

	s.wg.Add(1)
	go func() {
		defer func() {
			s.wg.Done()
			log.Infof("Background filter file update finished")
		}()

		log.Infof("Starting background filter file update")
		err := s.cFilterFiles.updateFiles(info.Blocks)
		if err != nil && !errors.Is(err, errServerShutdown) {
			log.Errorf("Error updating filter files: %v", err)
			s.errs.ChanIn() <- err
		}
	}()

	// If we're not indexing SP tweak data, we can return here.
	if !s.indexSPTweakData {
		return nil
	}

	s.spTweakFiles = newSPTweakFiles(
		s.spTweaksPerFile, s.chain, s.quit, s.baseDir, s.chainParams,
		s.h2hCache,
	)

	s.wg.Add(1)
	go func() {
		defer func() {
			s.wg.Done()
			log.Infof("Background SP tweak data file update " +
				"finished")
		}()

		log.Infof("Starting background SP tweak data file update")
		err := s.spTweakFiles.updateFiles(info.Blocks)
		if err != nil && !errors.Is(err, errServerShutdown) {
			log.Errorf("Error updating SP tweak data file: %v", err)
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
