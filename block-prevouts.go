package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	// restProbePath is the REST endpoint used to detect whether the
	// backend has its REST API (rest=1) enabled.
	restProbePath = "/rest/chaininfo.json"

	// restBlockPathPattern is the REST endpoint that serves a block in
	// raw binary format.
	restBlockPathPattern = "/rest/block/%s.bin"

	// restSpentTxOutsPathPattern is the REST endpoint that serves the
	// previous outputs spent by all transactions of a block (the block's
	// undo data) in raw binary format. Available since bitcoind v30.0.
	restSpentTxOutsPathPattern = "/rest/spenttxouts/%s.bin"

	// restRequestTimeout is the timeout for a single REST request to the
	// backend. Generous, since the backend only reads the block and undo
	// data from disk, without any re-encoding.
	restRequestTimeout = 30 * time.Second

	// maxSpentTxOutsPerBlock is a sanity cap on the list counts parsed
	// from a /rest/spenttxouts response, to avoid huge allocations on a
	// corrupt length prefix. A block can't contain more transactions (or
	// inputs) than its serialized size in bytes.
	maxSpentTxOutsPerBlock = wire.MaxBlockPayload

	// minPrevOutBackendVersion is the minimum bitcoind version (in the
	// numeric format reported by getnetworkinfo) we require for indexing
	// data derived from previous outputs (SP tweak data and custom
	// filters): v30.0 is the first version that serves a block's spent
	// outputs through the /rest/spenttxouts endpoint.
	minPrevOutBackendVersion = 300_000

	// prefetchDepth is the number of blocks the prefetcher keeps in
	// flight ahead of the block currently being processed, hiding the
	// fetch latency behind the tweak computation of the current block.
	// bitcoind answers REST requests from its RPC worker thread pool
	// (rpcthreads, 4 by default), so a substantially higher depth only
	// helps if the backend's pool is raised as well.
	prefetchDepth = 8
)

// getBlockScriptPubKey is the subset of the scriptPubKey JSON object of the
// getblock RPC response that we're interested in.
type getBlockScriptPubKey struct {
	Hex string `json:"hex"`
}

// getBlockPrevOut is the previous output data the getblock RPC includes for
// every transaction input when invoked with verbosity level 3. The data is
// served from the node's undo data, so it's available without a transaction
// index, but only for unpruned blocks.
type getBlockPrevOut struct {
	// The field name is dictated by bitcoind's JSON-RPC format.
	//
	//nolint:tagliatelle
	ScriptPubKey getBlockScriptPubKey `json:"scriptPubKey"`
}

// getBlockVin is the subset of a transaction input JSON object of the
// getblock RPC response that we're interested in.
type getBlockVin struct {
	PrevOut *getBlockPrevOut `json:"prevout"`
}

// getBlockTx is the subset of a transaction JSON object of the getblock RPC
// response that we're interested in.
type getBlockTx struct {
	Hex string        `json:"hex"`
	Vin []getBlockVin `json:"vin"`
}

// getBlockResult is the subset of the getblock RPC response that we're
// interested in.
type getBlockResult struct {
	Tx []getBlockTx `json:"tx"`
}

// blockWithPrevOuts bundles a block's transactions with the pkScripts of all
// previous outputs spent by them.
type blockWithPrevOuts struct {
	// txs are the block's transactions in block order.
	txs []*wire.MsgTx

	// prevOutScripts maps each outpoint spent in the block to the
	// pkScript of the output it references.
	prevOutScripts map[wire.OutPoint][]byte
}

// fetchPrevOutScript returns the pkScript of the output referenced by the
// given outpoint. It satisfies the sp.PrevOutFetcher signature, so it can be
// passed directly to the Silent Payments tweak calculation.
func (b *blockWithPrevOuts) fetchPrevOutScript(op wire.OutPoint) ([]byte,
	error) {

	pkScript, ok := b.prevOutScripts[op]
	if !ok {
		return nil, fmt.Errorf("no previous output script known for "+
			"outpoint %v", op)
	}

	return pkScript, nil
}

// blockPrevOutFetcher fetches blocks together with the previous output
// scripts of all inputs spent in them. It prefers the backend's binary REST
// API (bitcoind v30.0 or later with rest=1), which serves both the block and
// its undo data without any re-encoding and is orders of magnitude faster
// than the getblock RPC at verbosity level 3, where the JSON rendering of
// the block dominates the request time (see bitcoin/bitcoin#30495). If the
// REST API isn't available, it falls back to the RPC.
type blockPrevOutFetcher struct {
	chain *rpcclient.Client

	restURL    string
	httpClient *http.Client

	// probeOnce guards the one-time REST availability check, so the
	// fetcher can be shared between producers.
	probeOnce sync.Once
	useREST   bool
}

// backendRESTURL returns the base URL of the backend's REST API, derived
// from the RPC connection config, since bitcoind serves the REST API on its
// RPC port.
func backendRESTURL(chainCfg *rpcclient.ConnConfig) string {
	protocol := "https"
	if chainCfg.DisableTLS {
		protocol = "http"
	}

	return fmt.Sprintf("%s://%s", protocol, chainCfg.Host)
}

// newBlockPrevOutFetcher creates a fetcher that talks to the same backend as
// the given RPC client.
func newBlockPrevOutFetcher(chain *rpcclient.Client,
	chainCfg *rpcclient.ConnConfig) *blockPrevOutFetcher {

	httpClient := &http.Client{
		Timeout: restRequestTimeout,
	}

	// Allow as many keep-alive connections to the backend as we have
	// concurrent block fetches, so the prefetcher doesn't churn through
	// short-lived TCP connections.
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = transport.Clone()
		transport.MaxIdleConnsPerHost = prefetchDepth + 1
		httpClient.Transport = transport
	}

	return &blockPrevOutFetcher{
		chain:      chain,
		restURL:    backendRESTURL(chainCfg),
		httpClient: httpClient,
	}
}

// fetchBlock fetches the block with the given hash, along with the previous
// output scripts of all inputs spent in it, using the fastest mechanism the
// backend supports.
func (f *blockPrevOutFetcher) fetchBlock(
	hash *chainhash.Hash) (*blockWithPrevOuts, error) {

	f.probeOnce.Do(f.probeREST)

	if f.useREST {
		return f.fetchBlockREST(hash)
	}

	return getBlockWithPrevOuts(f.chain, hash)
}

// probeREST checks whether the backend's REST API is enabled and records the
// result. We only probe once: the RPC connection was already verified by the
// time the first block is fetched, so an unreachable REST endpoint means the
// API is disabled, not that the backend is down.
func (f *blockPrevOutFetcher) probeREST() {
	fallbackMsg := "falling back to the getblock RPC for fetching " +
		"previous outputs; enable rest=1 on bitcoind v30.0 or later " +
		"for significantly faster indexing"

	resp, err := f.httpClient.Get(f.restURL + restProbePath)
	if err != nil {
		log.Infof("REST API not reachable at %s (%v), %s", f.restURL,
			err, fallbackMsg)

		return
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		log.Infof("REST API not enabled on backend (HTTP status %d "+
			"on %s%s), %s", resp.StatusCode, f.restURL,
			restProbePath, fallbackMsg)

		return
	}

	log.Infof("Using the backend's REST API at %s to fetch blocks and "+
		"spent outputs", f.restURL)
	f.useREST = true
}

// prefetchResult is the outcome of a single background block fetch.
type prefetchResult struct {
	block *blockWithPrevOuts
	err   error
}

// prefetchedBlock tracks a single background block fetch, together with the
// block hash the fetch was started for, so a reorg that happened between
// scheduling and consumption can be detected.
type prefetchedBlock struct {
	hash   chainhash.Hash
	result chan prefetchResult
}

// blockPrefetcher hides the fetch latency of sequential block processing by
// keeping the fetches for the next few heights running in the background
// while the current block is being processed.
//
// It must only be used from a single goroutine; the background fetches only
// ever touch their own result channel, never the pending map.
type blockPrefetcher struct {
	// fetchSingle fetches a single block. Held as a function reference
	// so tests can run the prefetcher without a backend.
	fetchSingle func(*chainhash.Hash) (*blockWithPrevOuts, error)

	// getHash resolves a height to the hash of the block currently at
	// that height, or errors if the height isn't known (yet).
	getHash func(int32) (*chainhash.Hash, error)

	depth   int32
	pending map[int32]prefetchedBlock
}

// newBlockPrefetcher wraps the given fetcher with a prefetch window of
// prefetchDepth blocks, resolving upcoming heights through the given cache.
func newBlockPrefetcher(fetcher *blockPrevOutFetcher,
	h2hCache *heightToHashCache) *blockPrefetcher {

	return &blockPrefetcher{
		fetchSingle: fetcher.fetchBlock,
		getHash:     h2hCache.getBlockHash,
		depth:       prefetchDepth,
		pending:     make(map[int32]prefetchedBlock),
	}
}

// fetchBlock returns the block at the given height and hash, using a
// previously prefetched result when possible, and schedules background
// fetches for the heights following it.
func (p *blockPrefetcher) fetchBlock(height int32,
	hash *chainhash.Hash) (*blockWithPrevOuts, error) {

	p.scheduleAhead(height)

	entry, ok := p.pending[height]
	if ok {
		delete(p.pending, height)

		res := <-entry.result

		// Only use the prefetched result if it was fetched for the
		// hash the caller expects; a reorg may have replaced the
		// block since the fetch was scheduled. Stale and failed
		// prefetches alike are simply retried through the direct
		// fetch below.
		if res.err == nil && entry.hash.IsEqual(hash) {
			return res.block, nil
		}
	}

	return p.fetchSingle(hash)
}

// scheduleAhead starts background fetches for the blocks following the given
// height, until the prefetch window is full or a height can't be resolved to
// a hash, which usually means we've caught up to the chain tip.
func (p *blockPrefetcher) scheduleAhead(height int32) {
	for h := height + 1; h <= height+p.depth; h++ {
		if _, ok := p.pending[h]; ok {
			continue
		}

		hash, err := p.getHash(h)
		if err != nil {
			return
		}

		entry := prefetchedBlock{
			hash:   *hash,
			result: make(chan prefetchResult, 1),
		}
		p.pending[h] = entry

		go func() {
			block, err := p.fetchSingle(hash)
			entry.result <- prefetchResult{block: block, err: err}
		}()
	}
}

// requireREST probes the backend's REST API and returns an error if it isn't
// available. There is no RPC call that reports which options the backend was
// started with, so a test call against the REST API is the only way to find
// out whether it is enabled.
func (f *blockPrevOutFetcher) requireREST() error {
	f.probeOnce.Do(f.probeREST)

	if !f.useREST {
		return fmt.Errorf("the backend's REST API is not reachable "+
			"at %s; set rest=1 in the bitcoind configuration",
			f.restURL)
	}

	return nil
}

// verifyPrevOutBackendVersion returns an error if the given backend version
// (in the numeric format reported by getnetworkinfo) is too old for indexing
// data derived from previous outputs.
func verifyPrevOutBackendVersion(version int32) error {
	if version < minPrevOutBackendVersion {
		return fmt.Errorf("indexing SP tweak data or custom filters "+
			"requires bitcoind v30.0 or later, but the backend "+
			"reports version %d", version)
	}

	return nil
}

// restGet performs a GET request against the given REST path on the backend
// and returns the raw response body.
func (f *blockPrevOutFetcher) restGet(path string) ([]byte, error) {
	url := f.restURL + path
	resp, err := f.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("error calling REST endpoint %s: %w",
			url, err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response of REST "+
			"endpoint %s: %w", url, err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("REST endpoint %s returned HTTP "+
			"status %d: %s", url, resp.StatusCode,
			strings.TrimSpace(string(body)))
	}

	return body, nil
}

// fetchBlockREST fetches the block and its spent outputs through the
// backend's binary REST API. The spent output lists returned by the
// spenttxouts endpoint align positionally with the block's transactions and
// their inputs, so no further lookups are needed to pair them up.
func (f *blockPrevOutFetcher) fetchBlockREST(
	hash *chainhash.Hash) (*blockWithPrevOuts, error) {

	blockBytes, err := f.restGet(fmt.Sprintf(restBlockPathPattern, hash))
	if err != nil {
		return nil, fmt.Errorf("error fetching block %v: %w", hash,
			err)
	}

	block := &wire.MsgBlock{}
	if err := block.Deserialize(bytes.NewReader(blockBytes)); err != nil {
		return nil, fmt.Errorf("error deserializing block %v: %w",
			hash, err)
	}

	// A 404 here (with the block itself present) means the block's undo
	// data is unavailable, which happens for pruned blocks.
	spentBytes, err := f.restGet(
		fmt.Sprintf(restSpentTxOutsPathPattern, hash),
	)
	if err != nil {
		return nil, fmt.Errorf("error fetching spent outputs of "+
			"block %v: %w", hash, err)
	}

	spentTxOuts, err := parseSpentTxOuts(bytes.NewReader(spentBytes))
	if err != nil {
		return nil, fmt.Errorf("error parsing spent outputs of "+
			"block %v: %w", hash, err)
	}

	if len(spentTxOuts) != len(block.Transactions) {
		return nil, fmt.Errorf("tx count mismatch for block %v: %d "+
			"spent output lists vs %d txs", hash,
			len(spentTxOuts), len(block.Transactions))
	}

	result := &blockWithPrevOuts{
		txs:            block.Transactions,
		prevOutScripts: make(map[wire.OutPoint][]byte),
	}
	for txIndex, tx := range block.Transactions {
		// The first list belongs to the coinbase transaction, which
		// doesn't spend any previous outputs and is always empty.
		if txIndex == 0 {
			continue
		}

		spent := spentTxOuts[txIndex]
		if len(spent) != len(tx.TxIn) {
			return nil, fmt.Errorf("input count mismatch for tx "+
				"%d in block %v: %d spent outputs vs %d "+
				"inputs", txIndex, hash, len(spent),
				len(tx.TxIn))
		}

		for vinIndex, txIn := range tx.TxIn {
			result.prevOutScripts[txIn.PreviousOutPoint] =
				spent[vinIndex].PkScript
		}
	}

	return result, nil
}

// parseSpentTxOuts decodes the binary response of the /rest/spenttxouts
// endpoint: a compact-size count of per-transaction lists (the first being
// an always-empty placeholder for the coinbase transaction), each list
// consisting of a compact-size count of wire-serialized outputs (8-byte
// little-endian value followed by the compact-size-prefixed pkScript), one
// per input of the spending transaction, in input order.
func parseSpentTxOuts(r io.Reader) ([][]wire.TxOut, error) {
	numTxs, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, fmt.Errorf("error reading tx count: %w", err)
	}
	if numTxs > maxSpentTxOutsPerBlock {
		return nil, fmt.Errorf("tx count %d exceeds maximum %d",
			numTxs, maxSpentTxOutsPerBlock)
	}

	spentTxOuts := make([][]wire.TxOut, numTxs)
	for txIndex := range spentTxOuts {
		numIns, err := wire.ReadVarInt(r, 0)
		if err != nil {
			return nil, fmt.Errorf("error reading input count of "+
				"tx %d: %w", txIndex, err)
		}
		if numIns > maxSpentTxOutsPerBlock {
			return nil, fmt.Errorf("input count %d of tx %d "+
				"exceeds maximum %d", numIns, txIndex,
				maxSpentTxOutsPerBlock)
		}

		if numIns == 0 {
			continue
		}

		txOuts := make([]wire.TxOut, numIns)
		for vinIndex := range txOuts {
			var valueBytes [8]byte
			if _, err := io.ReadFull(r, valueBytes[:]); err != nil {
				return nil, fmt.Errorf("error reading value "+
					"of spent output %d of tx %d: %w",
					vinIndex, txIndex, err)
			}

			pkScript, err := wire.ReadVarBytes(
				r, 0, wire.MaxMessagePayload, "pkScript",
			)
			if err != nil {
				return nil, fmt.Errorf("error reading "+
					"pkScript of spent output %d of tx "+
					"%d: %w", vinIndex, txIndex, err)
			}

			txOuts[vinIndex] = wire.TxOut{
				Value: int64(binary.LittleEndian.Uint64(
					valueBytes[:],
				)),
				PkScript: pkScript,
			}
		}
		spentTxOuts[txIndex] = txOuts
	}

	// The response must not contain any trailing data, otherwise we
	// likely mis-parsed it.
	if _, err := r.Read(make([]byte, 1)); err != io.EOF {
		return nil, fmt.Errorf("unexpected trailing data after spent " +
			"output lists")
	}

	return spentTxOuts, nil
}

// getBlockWithPrevOuts fetches the block with the given hash, along with the
// previous output scripts of all inputs spent in it, using a single getblock
// RPC call with verbosity level 3. That verbosity level is only understood by
// bitcoind v25.0 or later; older versions silently fall back to verbosity
// level 2, which we detect through the missing prevout data.
func getBlockWithPrevOuts(chain *rpcclient.Client,
	hash *chainhash.Hash) (*blockWithPrevOuts, error) {

	hashParam, err := json.Marshal(hash.String())
	if err != nil {
		return nil, err
	}
	verbosityParam := json.RawMessage("3")

	rawResult, err := chain.RawRequest(
		"getblock", []json.RawMessage{hashParam, verbosityParam},
	)
	if err != nil {
		return nil, fmt.Errorf("error fetching block %v with "+
			"getblock verbosity 3: %w", hash, err)
	}

	var result getBlockResult
	if err := json.Unmarshal(rawResult, &result); err != nil {
		return nil, fmt.Errorf("error decoding getblock response "+
			"for block %v: %w", hash, err)
	}

	block := &blockWithPrevOuts{
		txs:            make([]*wire.MsgTx, 0, len(result.Tx)),
		prevOutScripts: make(map[wire.OutPoint][]byte),
	}
	for txIndex, rpcTx := range result.Tx {
		// We decode the hex on the fly while deserializing, to avoid
		// allocating an intermediate buffer for the raw transaction.
		txReader := hex.NewDecoder(strings.NewReader(rpcTx.Hex))

		tx := &wire.MsgTx{}
		if err := tx.Deserialize(txReader); err != nil {
			return nil, fmt.Errorf("error deserializing tx %d "+
				"in block %v: %w", txIndex, hash, err)
		}
		block.txs = append(block.txs, tx)

		// The coinbase transaction doesn't spend any previous
		// outputs, so there is no prevout data to collect for it.
		if blockchain.IsCoinBaseTx(tx) {
			continue
		}

		if len(rpcTx.Vin) != len(tx.TxIn) {
			return nil, fmt.Errorf("input count mismatch for tx "+
				"%d in block %v: %d vs %d", txIndex, hash,
				len(rpcTx.Vin), len(tx.TxIn))
		}

		for vinIndex, vin := range rpcTx.Vin {
			if vin.PrevOut == nil {
				return nil, fmt.Errorf("missing prevout data "+
					"for input %d of tx %d in block %v; "+
					"fetching previous outputs requires "+
					"bitcoind v25.0 or later and an "+
					"unpruned block", vinIndex, txIndex,
					hash)
			}

			pkScript, err := hex.DecodeString(
				vin.PrevOut.ScriptPubKey.Hex,
			)
			if err != nil {
				return nil, fmt.Errorf("error decoding "+
					"prevout script of input %d of tx %d "+
					"in block %v: %w", vinIndex, txIndex,
					hash, err)
			}

			op := tx.TxIn[vinIndex].PreviousOutPoint
			block.prevOutScripts[op] = pkScript
		}
	}

	return block, nil
}
