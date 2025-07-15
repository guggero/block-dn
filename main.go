package main

import (
	"fmt"
	"os"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/spf13/cobra"
)

const (
	version = "1.0.6"

	defaultListenPort = 8080
)

var (
	// Commit will be injected at compile-time with the `-X` ldflag.
	Commit = ""

	logMgr *build.SubLoggerManager
	log    btclog.Logger
)

type mainCommand struct {
	testnet bool
	regtest bool

	lightMode bool

	baseDir string

	listenAddr string

	bitcoindConfig *rpcclient.ConnConfig
	cmd            *cobra.Command
}

func main() {
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
			switch {
			case cc.testnet:
				chainParams = &chaincfg.TestNet3Params

			case cc.regtest:
				chainParams = &chaincfg.RegressionNetParams
			}

			setupLogging()
			log.Infof("block-dn version v%s commit %s", version,
				Commit)

			if !cc.lightMode && cc.baseDir == "" {
				log.Errorf("Base directory must be set if " +
					"not running in light mode")
				return
			}

			server := newServer(
				cc.lightMode, cc.baseDir, cc.listenAddr,
				cc.bitcoindConfig, chainParams,
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
	cc.cmd.PersistentFlags().BoolVarP(
		&cc.testnet, "testnet", "t", false, "Indicates if testnet "+
			"parameters should be used",
	)
	cc.cmd.PersistentFlags().BoolVarP(
		&cc.regtest, "regtest", "r", false, "Indicates if regtest "+
			"parameters should be used",
	)
	cc.cmd.PersistentFlags().BoolVarP(
		&cc.lightMode, "light-mode", "l", false, "Indicates if the "+
			"server should run in light mode which creates no "+
			"files on disk and therefore requires zero disk "+
			"space; but only the status and block endpoints are "+
			"available in this mode",
	)
	cc.cmd.PersistentFlags().StringVarP(
		&cc.baseDir, "base-dir", "", "", "The base directory "+
			"where the generated files will be stored",
	)
	cc.cmd.PersistentFlags().StringVarP(
		&cc.listenAddr, "listen-addr", "", cc.listenAddr, "The local "+
			"host:port to listen on",
	)
	cc.cmd.PersistentFlags().StringVarP(
		&cc.bitcoindConfig.Host, "bitcoind-host", "", "localhost:8332",
		"The host:port of the bitcoind instance to connect to",
	)
	cc.cmd.PersistentFlags().StringVarP(
		&cc.bitcoindConfig.User, "bitcoind-user", "", "",
		"The RPC username of the bitcoind instance to connect to",
	)
	cc.cmd.PersistentFlags().StringVarP(
		&cc.bitcoindConfig.Pass, "bitcoind-pass", "", "",
		"The RPC password of the bitcoind instance to connect to",
	)

	if err := cc.cmd.Execute(); err != nil {
		fmt.Printf("Error: %v", err)
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func setupLogging() {
	logConfig := build.DefaultLogConfig()
	logWriter := build.NewRotatingLogWriter()
	logMgr = build.NewSubLoggerManager(build.NewDefaultLogHandlers(
		logConfig, logWriter,
	)...)
	log = build.NewSubLogger("BLDN", genSubLogger(logMgr))

	setSubLogger("BLDN", log)
	err := logWriter.InitLogRotator(logConfig.File, "./block-dn.log")
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
