package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
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
	lightMode      bool
	baseDir        string
	listenAddr     string
	chainCfg       *rpcclient.ConnConfig
	chainParams    *chaincfg.Params
	reOrgSafeDepth uint32
	chain          *rpcclient.Client
	router         *mux.Router
	httpServer     *http.Server

	headersPerFile  int32
	filtersPerFile  int32
	startupComplete atomic.Bool
	currentHeight   atomic.Int32

	cache *cache

	wg   sync.WaitGroup
	errs *fn.ConcurrentQueue[error]
	quit chan struct{}
}

func newServer(lightMode bool, baseDir, listenAddr string,
	chainCfg *rpcclient.ConnConfig, chainParams *chaincfg.Params,
	reOrgSafeDepth uint32, headersPerFile, filtersPerFile int32) *server {

	s := &server{
		lightMode:      lightMode,
		baseDir:        baseDir,
		listenAddr:     listenAddr,
		chainCfg:       chainCfg,
		chainParams:    chainParams,
		reOrgSafeDepth: reOrgSafeDepth,

		headersPerFile: headersPerFile,
		filtersPerFile: filtersPerFile,

		cache: newCache(headersPerFile, filtersPerFile),

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
			log.Infof("Background filter file update finished")
		}()

		log.Infof("Starting background filter file update")
		err := s.updateFiles()
		if err != nil && !errors.Is(err, errServerShutdown) {
			log.Errorf("Error updating files: %v", err)
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
