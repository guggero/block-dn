# block-dn

Process BIP-157/158 cfilters and Bitcoin blocks for distribution over a CDN.

Read the API documentation at [block-dn.org](https://block-dn.org).

## How to install

Make sure you have at least `Go 1.24.9` and `make` installed. Then run the
following commands:

```shell
git clone https://github.com/guggero/block-dn.git
cd block-dn
make install
```

## How to run

If you built from source as described above, the binary should now be in the
`$GOPATH/bin` directory. Depending on your installation, that should be included
in your `$PATH` (if not, make sure to add it).

You can then just run (example for testnet4):
```shell
$ block-dn --base-dir=$HOME/.block-dn \
  --log-dir=logs \
  --log-level=debug \
  --testnet4 \
  --listen-addr=0.0.0.0:8080 \
  --bitcoind-host=<bitcoind-testnet4-hostname>:48332 \
  --bitcoind-user=<bitcoind-username> \
  --bitcoind-pass=<bitcoind-password>
```

## How to run in Docker

The latest tagged version is always also available as a Docker image that can
be run with:

```shell
$ docker run -d \
  -p 8080:8080 \
  -v $HOME/block-dn-testnet4/:/root/.block-dn \
  --restart=unless-stopped \
  --name=block-dn-testnet4 \
  --hostname=block-dn-testnet4 \
  --network=internal \
  guggero/block-dn \
  --base-dir=/root/.block-dn \
  --log-dir=logs \
  --log-level=debug \
  --testnet4 \
  --listen-addr=0.0.0.0:8080 \
  --bitcoind-host=<bitcoind-testnet4-hostname>:48332 \
  --bitcoind-user=<bitcoind-username> \
  --bitcoind-pass=<bitcoind-password>
```

## Configuration flags

The following configuration flags are available (run `block-dn --help` to see
this on your machine):

```text
Usage:
  block-dn [flags]

Flags:
      --base-dir string           The base directory where the generated files will be stored
      --bitcoind-host string      The host:port of the bitcoind instance to connect to (default "localhost:8332")
      --bitcoind-pass string      The RPC password of the bitcoind instance to connect to
      --bitcoind-user string      The RPC username of the bitcoind instance to connect to
  -h, --help                      help for block-dn
      --index-page string         Full path to the index.html that should be used instead of the default one that comes with the project
      --index-sp-tweak-data       Indicates if the server should index BIP-0352 Silent Payments tweak data that allows light clients to scan the chain for inbound SP more efficiently; this requires every block since the activation of Taproot to be indexed which may take a while
      --light-mode                Indicates if the server should run in light mode which creates no files on disk and therefore requires zero disk space; but only the status and block endpoints are available in this mode
      --listen-addr string        The local host:port to listen on (default "localhost:8080")
      --log-dir string            The log directory where the log file will be written (default "/home/guggero/projects/go/src/github.com/guggero/block-dn")
      --log-level string          The log level for the logger: debug, info, warn, error, critical (default "info")
      --regtest                   Indicates if regtest parameters should be used
      --reorg-safe-depth uint32   The number of blocks to wait before considering a block safe from re-orgs (default 6)
      --signet                    Indicates if signet parameters should be used
      --testnet                   Indicates if testnet parameters should be used
      --testnet4                  Indicates if testnet4 parameters should be used
  -v, --version                   version for block-dn
```
