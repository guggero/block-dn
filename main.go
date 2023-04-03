package main

import (
	"context"
	"fmt"
	"github.com/lightningnetwork/lnd/signal"
	"os"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/build"
	"github.com/spf13/cobra"
)

const (
	version = "0.0.1"
	Commit  = ""
)

var (
	logWriter = build.NewRotatingLogWriter()
	log       = build.NewSubLogger("CFDN", genSubLogger(logWriter))
)

type mainCommand struct {
	testnet bool
	regtest bool

	baseDir string

	bitcoindConfig *rpcclient.ConnConfig
	cmd            *cobra.Command
}

func main() {
	cc := &mainCommand{
		bitcoindConfig: &rpcclient.ConnConfig{
			DisableTLS:   true,
			HTTPPostMode: true,
		},
	}
	cc.cmd = &cobra.Command{
		Use: "cfilter-cdn",
		Short: "cfilter-cdn creates static files for serving compact " +
			"filters over HTTP",
		Long:    ``,
		Version: fmt.Sprintf("v%s, commit %s", version, Commit),
		Run: func(cmd *cobra.Command, args []string) {
			chainParams := &chaincfg.MainNetParams
			switch {
			case cc.testnet:
				chainParams = &chaincfg.TestNet3Params

			case cc.regtest:
				chainParams = &chaincfg.RegressionNetParams
			}

			setupLogging()
			log.Infof("cfilter-cdn version v%s commit %s", version,
				Commit)

			client, err := rpcclient.New(cc.bitcoindConfig, nil)
			if err != nil {
				log.Errorf("Error connecting to bitcoind: %v",
					err)
				return
			}
			defer client.Shutdown()

			// Create a context that can be canceled when the user
			// interrupts the program.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			interceptor, err := signal.Intercept()
			if err != nil {
				log.Errorf("Error intercepting signals: %v",
					err)
				return
			}

			go func() {
				select {
				case <-interceptor.ShutdownChannel():
					log.Infof("Received shutdown signal")
					cancel()
				}
			}()

			err = UpdateFilterFiles(
				ctx, cc.baseDir, client, chainParams,
			)
			if err != nil {
				log.Errorf("Error updating filter files: %v",
					err)
				return
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
	cc.cmd.PersistentFlags().StringVarP(
		&cc.baseDir, "base-dir", "", "", "The base directory "+
			"where the generated files will be stored",
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
	setSubLogger("CFDN", log)
	err := logWriter.InitLogRotator("./cfilter-cdn.log", 10, 3)
	if err != nil {
		panic(err)
	}
	err = build.ParseAndSetDebugLevels("debug", logWriter)
	if err != nil {
		panic(err)
	}
}

// genSubLogger creates a sub logger with an empty shutdown function.
func genSubLogger(logWriter *build.RotatingLogWriter) func(string) btclog.Logger {
	return func(s string) btclog.Logger {
		return logWriter.GenSubLogger(s, func() {})
	}
}

// setSubLogger is a helper method to conveniently register the logger of a sub
// system.
func setSubLogger(subsystem string, logger btclog.Logger,
	useLoggers ...func(btclog.Logger)) {

	logWriter.RegisterSubLogger(subsystem, logger)
	for _, useLogger := range useLoggers {
		useLogger(logger)
	}
}
