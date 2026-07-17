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
      --index-custom-filters      Indicates if the server should build custom output-type-restricted compact filters (four sets: segwit, p2wpkh, p2wsh, p2tr); adds about 8 GB more data as of block 950k; requires bitcoind v30.0 or later with the REST API enabled (rest=1)
      --index-page string         Full path to the index.html that should be used instead of the default one that comes with the project
      --index-sp-tweak-data       Indicates if the server should index BIP-0352 Silent Payments tweak data that allows light clients to scan the chain for inbound SP more efficiently; the tweaks are served as binary files at four dust filter levels (0, 600, 1000 and 3750 sats); this requires every block since the activation of Taproot to be indexed which may take a while; requires bitcoind v30.0 or later with the REST API enabled (rest=1)
      --light-mode                Indicates if the server should run in light mode which creates no files on disk and therefore requires zero disk space; but only the status and block endpoints are available in this mode
      --listen-addr string        The local host:port to listen on (default "localhost:8080")
      --log-dir string            The log directory where the log file will be written (default ".")
      --log-level string          The log level for the logger: debug, info, warn, error, critical (default "info")
      --read-timeout duration     The maximum duration for reading an HTTP request (default 5s)
      --regtest                   Indicates if regtest parameters should be used
      --reorg-safe-depth uint32   The number of blocks to wait before considering a block safe from re-orgs (default 6)
      --signet                    Indicates if signet parameters should be used
      --testnet                   Indicates if testnet parameters should be used
      --testnet4                  Indicates if testnet4 parameters should be used
  -v, --version                   version for block-dn
      --write-timeout duration    The maximum duration for writing an HTTP response; must be generous enough for the largest filter file to be streamed to a slow direct (non-CDN) client (default 5m0s)
```

## SP tweak data file format

With `--index-sp-tweak-data` enabled, the server builds and serves BIP-0352
Silent Payments tweak keys (the 33-byte compressed public key
`tweak = input_hash·A` of every eligible transaction) as compact binary
files, available through the `/sp/tweaks/<dust_limit>/<start_block>`
endpoint.

Every response — whether it comes from a sealed file on disk or from the
server's in-memory tail — starts with an 18-byte header, followed by one
record per block, in ascending height order:

```text
header:
    4 bytes        network magic (uint32, little endian)
    1 byte         format version (currently 0)
    1 byte         file type (2 = SP tweak data)
    4 bytes        height of the first block (uint32, little endian)
    8 bytes        dust limit in satoshis (uint64, little endian)

per block (height implied by position):
    varint         number of tweak keys (Bitcoin CompactSize)
    N x 33 bytes   compressed tweak public keys, in transaction order
```

A block without eligible transactions is encoded as a single zero byte. Each
file covers 2'000 blocks, so a block's record is found by walking the counts
from the file's start height.

The dust limit in the URL selects one of the pre-computed filter levels (0,
600, 1000 or 3750 satoshis): a transaction's tweak is included if the
largest value among its taproot outputs is strictly greater than the dust
limit. Level 0 therefore contains every tweak, while the higher levels
exclude transactions whose taproot outputs are all uneconomical dust (such
as inscription postage), which shrinks the download substantially for the
spam-heavy block ranges. A wallet can scan quickly with a high dust limit
first and re-scan with a lower one if an expected payment doesn't show up.
