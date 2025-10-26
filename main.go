package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/spf13/cobra"
)

const (
	version = "1.1.3"

	defaultListenPort = 8080

	defaultMainnetReOrgSafeDepth = 6
	defaultTestnetReOrgSafeDepth = 100
)

var (
	// Commit will be injected at compile-time with the `-X` ldflag.
	Commit = ""

	logMgr *build.SubLoggerManager
	log    btclog.Logger
)

type mainCommand struct {
	testnet  bool
	testnet4 bool
	regtest  bool
	signet   bool

	lightMode bool

	baseDir   string
	logDir    string
	indexPage string

	listenAddr string

	reOrgSafeDepth uint32

	bitcoindConfig *rpcclient.ConnConfig
	cmd            *cobra.Command
}

func main() {
	workDir, err := os.Getwd()
	if err != nil {
		fmt.Printf("Error: %v", err)
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cc := &mainCommand{
		listenAddr: fmt.Sprintf("localhost:%d", defaultListenPort),
		bitcoindConfig: &rpcclient.ConnConfig{
			DisableTLS:   true,
			HTTPPostMode: true,
		},
	}
	cc.cmd = &cobra.Command{
		Use: "block-dn",
		Short: "block-dn creates static files for serving compact " +
			"filters and blocks over HTTP",
		Long:    ``,
		Version: fmt.Sprintf("v%s, commit %s", version, Commit),
		Run: func(_ *cobra.Command, _ []string) {
			chainParams := &chaincfg.MainNetParams
			headersPerFile := int32(DefaultHeadersPerFile)
			filtersPerFile := int32(DefaultFiltersPerFile)

			switch {
			case cc.testnet:
				chainParams = &chaincfg.TestNet3Params

				// The test networks are more prone to longer
				// re-orgs.
				if cc.reOrgSafeDepth ==
					defaultMainnetReOrgSafeDepth {

					cc.reOrgSafeDepth =
						defaultTestnetReOrgSafeDepth
				}

			case cc.testnet4:
				chainParams = &chaincfg.TestNet4Params

				// The test networks are more prone to longer
				// re-orgs.
				if cc.reOrgSafeDepth ==
					defaultMainnetReOrgSafeDepth {

					cc.reOrgSafeDepth =
						defaultTestnetReOrgSafeDepth
				}

			case cc.signet:
				chainParams = &chaincfg.SigNetParams

			case cc.regtest:
				chainParams = &chaincfg.RegressionNetParams

				headersPerFile = DefaultRegtestHeadersPerFile
				filtersPerFile = DefaultRegtestFiltersPerFile
			}

			setupLogging(cc.logDir)
			log.Infof("block-dn version v%s commit %s", version,
				Commit)

			if !cc.lightMode && cc.baseDir == "" {
				log.Errorf("Base directory must be set if " +
					"not running in light mode")
				return
			}

			if cc.indexPage != "" {
				pageContent, err := os.ReadFile(cc.indexPage)
				if err != nil {
					log.Errorf("Error reading index page "+
						"file '%s': %v", cc.indexPage,
						err)
					return
				}

				indexHTML = string(pageContent)
			}

			server := newServer(
				cc.lightMode, cc.baseDir, cc.listenAddr,
				cc.bitcoindConfig, chainParams,
				cc.reOrgSafeDepth, headersPerFile,
				filtersPerFile,
			)
			err := server.start()
			if err != nil {
				log.Errorf("Error starting server: %v", err)
				return
			}

			interceptor, err := signal.Intercept()
			if err != nil {
				log.Errorf("Error intercepting signals: %v",
					err)
				return
			}

			select {
			case <-interceptor.ShutdownChannel():
				log.Infof("Received shutdown signal")

			case err := <-server.errs.ChanOut():
				log.Errorf("Error running server: %v", err)
			}

			err = server.stop()
			if err != nil {
				log.Errorf("Error stopping server: %v", err)
			}
		},
		DisableAutoGenTag: true,
	}
	cc.cmd.PersistentFlags().BoolVar(
		&cc.testnet, "testnet", false, "Indicates if testnet "+
			"parameters should be used",
	)
	cc.cmd.PersistentFlags().BoolVar(
		&cc.testnet4, "testnet4", false, "Indicates if testnet4 "+
			"parameters should be used",
	)
	cc.cmd.PersistentFlags().BoolVar(
		&cc.regtest, "regtest", false, "Indicates if regtest "+
			"parameters should be used",
	)
	cc.cmd.PersistentFlags().BoolVar(
		&cc.signet, "signet", false, "Indicates if signet "+
			"parameters should be used",
	)
	cc.cmd.PersistentFlags().BoolVar(
		&cc.lightMode, "light-mode", false, "Indicates if the "+
			"server should run in light mode which creates no "+
			"files on disk and therefore requires zero disk "+
			"space; but only the status and block endpoints are "+
			"available in this mode",
	)
	cc.cmd.PersistentFlags().StringVar(
		&cc.baseDir, "base-dir", "", "The base directory "+
			"where the generated files will be stored",
	)
	cc.cmd.PersistentFlags().StringVar(
		&cc.logDir, "log-dir", workDir, "The log directory where the "+
			"log file will be written",
	)
	cc.cmd.PersistentFlags().StringVar(
		&cc.indexPage, "index-page", "", "Full path to the index.html "+
			"that should be used instead of the default one that "+
			"comes with the project",
	)
	cc.cmd.PersistentFlags().StringVar(
		&cc.listenAddr, "listen-addr", cc.listenAddr, "The local "+
			"host:port to listen on",
	)
	cc.cmd.PersistentFlags().StringVar(
		&cc.bitcoindConfig.Host, "bitcoind-host", "localhost:8332",
		"The host:port of the bitcoind instance to connect to",
	)
	cc.cmd.PersistentFlags().StringVar(
		&cc.bitcoindConfig.User, "bitcoind-user", "",
		"The RPC username of the bitcoind instance to connect to",
	)
	cc.cmd.PersistentFlags().StringVar(
		&cc.bitcoindConfig.Pass, "bitcoind-pass", "",
		"The RPC password of the bitcoind instance to connect to",
	)
	cc.cmd.PersistentFlags().Uint32VarP(
		&cc.reOrgSafeDepth, "reorg-safe-depth", "",
		defaultMainnetReOrgSafeDepth,
		"The number of blocks to wait before considering a block "+
			"safe from re-orgs",
	)

	if err := cc.cmd.Execute(); err != nil {
		fmt.Printf("Error: %v", err)
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func setupLogging(logDir string) {
	logConfig := build.DefaultLogConfig()
	logWriter := build.NewRotatingLogWriter()
	logMgr = build.NewSubLoggerManager(build.NewDefaultLogHandlers(
		logConfig, logWriter,
	)...)
	log = build.NewSubLogger("BLDN", genSubLogger(logMgr))

	setSubLogger("BLDN", log)
	err := logWriter.InitLogRotator(
		logConfig.File, filepath.Join(logDir, "block-dn.log"),
	)
	if err != nil {
		panic(err)
	}
	err = build.ParseAndSetDebugLevels("debug", logMgr)
	if err != nil {
		panic(err)
	}
}

// genSubLogger creates a sub logger with an empty shutdown function.
func genSubLogger(root *build.SubLoggerManager) func(string) btclog.Logger {
	return func(s string) btclog.Logger {
		return root.GenSubLogger(s, func() {})
	}
}

// setSubLogger is a helper method to conveniently register the logger of a sub
// system.
func setSubLogger(subsystem string, logger btclog.Logger,
	useLoggers ...func(btclog.Logger)) {

	logMgr.RegisterSubLogger(subsystem, logger)
	for _, useLogger := range useLoggers {
		useLogger(logger)
	}
}
