# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project

`block-dn` is a single-binary Go HTTP server that fetches Bitcoin headers,
BIP-157/158 compact filters, and (optionally) BIP-352 silent-payment tweak data
from a `bitcoind` backend, batches them into fixed-size static files on disk,
and serves them over HTTP for CDN distribution. It also exposes proxied
endpoints for blocks, raw transactions, UTXOs, merkle proofs, and fee
estimates. See README.md and AGENTS.md for user-facing and style details.

## Common commands

- `make build` — compile binary into the working directory.
- `make install` — install to `$GOPATH/bin` (preferred way to get the CLI).
- `make unit` — run all unit tests. Tests use `-tags="bitcoind integration dev"`
  and spin up a real `bitcoind` via the lnd test harness, so a `bitcoind` binary
  must be on `PATH`. CI installs it via `scripts/install_bitcoind.sh <version>`.
- `make unit case=<RegexOrTestName>` — run a single test (passed as `-run=`).
- `make fmt` — run `gosimports` then `gofmt -s` over all non-vendor `.go` files.
- `make fmt-check` — fails if `make fmt` would produce changes (CI gate).
- `make lint` — run `golangci-lint` via the version pinned in `go.mod` (`go tool
  golangci-lint`); config in `.golangci.yml`.

The CI workflow (`.github/workflows/main.yml`) runs `make fmt-check`,
`golangci-lint`, and `make unit`. Match that locally before pushing.

## Architecture

All code lives in `package main` at the repo root — there are no subpackages.
The big picture:

- **`main.go`** parses cobra flags, selects chain params (mainnet / testnet /
  testnet4 / signet / regtest), sets up `btclog`-based logging, and starts the
  server. Regtest gets smaller per-file batch sizes
  (`DefaultRegtestHeadersPerFile`, etc.).
- **`server.go`** wires everything together. On `start()` it:
  1. Connects to bitcoind via `rpcclient`.
  2. Preloads the height→hash cache by re-reading every on-disk header file
     (`heightToHashCache.loadFromHeaders`).
  3. Verifies the cache's tip block matches the backend (mismatch ⇒ refuse to
     start; this catches reorgs that crossed the previous run).
  4. Launches the HTTP server, then background goroutines for each enabled file
     type (headers, filters, optional SP tweaks). Each goroutine catches up to
     the tip, sets `startupComplete`, then polls `GetBlockCount` every
     `blockPollInterval` (1s) for new blocks.
  Errors from any goroutine flow through `s.errs` (a `fn.ConcurrentQueue`) and
  trigger shutdown.
- **`heightToHashCache`** (`height-to-hash-cache.go`) is the single source of
  truth for height→hash lookups during indexing. It's seeded from disk so
  restarts don't re-hit bitcoind for every height.
- **File producers** — `headerFiles`, `cFilterFiles`, `spTweakFiles`. Each one
  follows the same pattern: hold an in-memory map of items for the current
  batch, write a file every `<N>PerFile` blocks (`block-NNNNNNN-NNNNNNN.<ext>`),
  then `clearData()`. The file naming convention is critical — `lastFile()`
  parses the trailing height out of the filename to resume after restart, and
  the request handlers reconstruct the filename deterministically from a
  requested start height.
- **`handlers.go`** registers all routes on a `gorilla/mux` router (see
  `createRouter`) and implements them. The generic `heightBasedRequestHandler`
  serves any static-file endpoint: if the file exists on disk, stream with a
  1-year cache header; if the requested range falls inside the current
  unflushed batch, serialize from memory with a 1s cache header; otherwise
  validate the start height aligns with `entriesPerFile` and 400. There's also
  an "import" variant that prepends a small metadata header (network ID,
  version, file type, start height) used by `neutrino`-style importers.
- **Light mode** (`--light-mode`) skips all file producers and the h2h cache
  preload. Only `/status`, block, tx, utxo, and fee endpoints work; the
  height-based file endpoints return 503 `errUnavailableInLightMode`.
- **SP tweak indexing** (`--index-sp-tweak-data`) is opt-in and expensive: it
  needs every block from Taproot activation onward, plus an LRU cache of
  previous output scripts (`--prev-out-cache-size-mib`, default 1024 MiB) to
  resolve Taproot inputs. Activation heights are hardcoded in
  `TaprootActivationHeights` (`silent-payments-files.go`).
- **`index.html`** is `//go:embed`-ed and served at `/` and `/index.html`. It
  can be overridden at runtime via `--index-page`.

### Reorg handling

The server is mostly append-only and assumes a `--reorg-safe-depth` worth of
confirmations before a block becomes part of a file (default 6 mainnet, 100
testnet — auto-bumped in `main.go`). The startup cache-vs-backend hash check
in `server.start()` is the main guard against reorgs that happened while the
server was down; deeper handling is intentionally minimal.

## Style (from AGENTS.md)

- 80-character line limit, tabs count as 8. Wrap long calls/definitions per
  the examples in AGENTS.md (closing paren on its own line, args on a new
  line). Long error/log messages are the exception: minimize lines, keep
  under 80 with concatenated string literals.
- Comments explain *why*, not *what*. Every function has a doc comment that
  starts with the function name.
- Tests use `github.com/stretchr/testify/require`. Prefer table-driven tests
  or `pgregory.net/rapid` property tests.
- Commit subject format: `subsystem: short description` (≤50 chars,
  imperative). Use `multi:` for repo-wide changes. Wrap body at 72.
