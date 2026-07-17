package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/spf13/cobra"
)

const (
	// memoryHeadersRoute and memoryFilterHeadersRoute are the server's
	// height-based HTTP routes (see createRouter) that serve unsealed
	// ranges from memory.
	memoryHeadersRoute       = "headers"
	memoryFilterHeadersRoute = "filter-headers"
)

// memoryFetchTimeout bounds one in-memory range fetch from the running
// server; even the largest possible response (a full header range) is only a
// few MiB served from memory, so this is generous.
var memoryFetchTimeout = 30 * time.Second

// newVerifyCommand creates the "verify" subcommand: an integrity check of
// all sealed files against each other and the backend chain, with an
// optional --fix that regenerates corrupt files in place. This exists
// because sealed files are advertised as immutable (year-long CDN caching),
// so any historical corruption — e.g. a filter ingested from a reorged-away
// block by a version that predates the seal-time verification — persists
// until the operator repairs it. Filter files of the last, incomplete
// header range have no sealed filter-header file to check against yet, so
// their filter headers are fetched from the running server's memory (via
// --listen-addr).
func newVerifyCommand(cc *mainCommand) *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use: "verify",
		Short: "Verify all sealed header and filter files against " +
			"the backend chain, optionally fixing corrupt files",
		Long: "Checks every sealed block-header file (chain linkage, " +
			"genesis, backend anchors) and every sealed compact-" +
			"filter file (BIP157 commitment chain closure " +
			"against " +
			"the filter-header files). The unsealed tail range " +
			"that only exists in the running block-dn server's " +
			"memory (reached through the --listen-addr flag) is " +
			"verified against the backend as well. Requires the " +
			"same --base-dir, network and bitcoind flags as the " +
			"server. SP tweak data files are not covered. After " +
			"--fix, purge the CDN cache for the repaired URLs.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runVerify(cc, fix)
		},
	}
	cmd.Flags().BoolVar(
		&fix, "fix", false, "Regenerate corrupt files in place "+
			"from the backend",
	)

	return cmd
}

// verifier holds the state of one verification run.
type verifier struct {
	chain          *rpcclient.Client
	params         *chaincfg.Params
	baseDir        string
	headersPerFile int32
	filtersPerFile int32
	fix            bool

	// reOrgSafeDepth mirrors the server's --reorg-safe-depth: heights
	// above tip minus this depth may legitimately still change, so they
	// are never anchored against the backend.
	reOrgSafeDepth int32

	// serverURL is the base URL of the running block-dn server, derived
	// from --listen-addr. Ranges that aren't sealed to disk yet only
	// exist in that server's memory and are fetched from it over HTTP.
	serverURL  string
	httpClient *http.Client

	badFiles   []string
	fixedFiles []string

	// fHeaderCache is the most recently loaded filter-header range, so
	// per-height lookups don't re-read the same file over and over.
	// fHeaderCacheFromMemory marks a snapshot fetched from the running
	// server instead of a sealed file: the server keeps indexing while
	// we verify, so such a snapshot is refreshed instead of being
	// declared truncated when the walk outgrows it.
	fHeaderCacheStart      int32
	fHeaderCache           []byte
	fHeaderCacheFromMemory bool
}

func runVerify(cc *mainCommand, fix bool) error {
	chainParams, headersPerFile, filtersPerFile, _ := cc.resolveNetwork()
	if cc.baseDir == "" {
		return fmt.Errorf("--base-dir must be set")
	}

	serverURL, err := serverBaseURL(cc.listenAddr)
	if err != nil {
		return err
	}

	client, err := rpcclient.New(cc.bitcoindConfig, nil)
	if err != nil {
		return fmt.Errorf("error connecting to bitcoind: %w", err)
	}
	defer client.Shutdown()

	v := &verifier{
		chain:          client,
		params:         chainParams,
		baseDir:        cc.baseDir,
		headersPerFile: headersPerFile,
		filtersPerFile: filtersPerFile,
		fix:            fix,
		reOrgSafeDepth: int32(cc.reOrgSafeDepth),
		serverURL:      serverURL,
		httpClient: &http.Client{
			Timeout: memoryFetchTimeout,
		},
		fHeaderCacheStart: -1,
	}

	fmt.Printf("fetching unsealed ranges from the block-dn server at %s\n",
		serverURL)

	if err := v.verifyHeaderFiles(); err != nil {
		return err
	}
	if err := v.verifyFilterFiles(); err != nil {
		return err
	}

	if len(v.fixedFiles) > 0 {
		fmt.Printf("fixed %d file(s):\n", len(v.fixedFiles))
		for _, f := range v.fixedFiles {
			fmt.Printf("  %s\n", f)
		}
		fmt.Println("remember to purge the CDN cache for the " +
			"corresponding URLs")
	}
	if len(v.badFiles) > 0 {
		return fmt.Errorf("%d corrupt file(s) found: %v",
			len(v.badFiles), v.badFiles)
	}

	fmt.Println("all sealed files verified OK")

	return nil
}

// verifyHeaderFiles walks all sealed block-header files in order, verifying
// the prev-hash linkage within and across files, the genesis block, and one
// backend anchor per file end. The paired filter-header files are checked
// for size and end anchor here; their interior is covered by the filter
// chain closure in verifyFilterFiles. The unsealed tail range past the last
// sealed file is verified against the running server's memory.
func (v *verifier) verifyHeaderFiles() error {
	headerDir := filepath.Join(v.baseDir, HeaderFileDir)

	var running chainhash.Hash
	for start := int32(0); ; start += v.headersPerFile {
		end := start + v.headersPerFile - 1
		name := fmt.Sprintf(
			HeaderFileNamePattern, headerDir, start, end,
		)
		if !fileExists(name) {
			// Everything past the sealed files only exists in
			// the running server's memory.
			return v.verifyMemoryTail(start, running)
		}

		err := v.verifyOneHeaderFile(name, start, end, &running)
		if err != nil {
			return err
		}

		fhName := fmt.Sprintf(
			FilterHeaderFileNamePattern, headerDir, start, end,
		)
		if err := v.verifyFilterHeaderAnchor(
			fhName, start, end,
		); err != nil {
			return err
		}
	}
}

// verifyOneHeaderFile checks a single sealed block-header file and updates
// running to its last block hash. On corruption it records (and with --fix
// regenerates) the file.
func (v *verifier) verifyOneHeaderFile(name string, start, end int32,
	running *chainhash.Hash) error {

	verify := func() error {
		data, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("error reading: %w", err)
		}
		want := int(v.headersPerFile) * wire.MaxBlockHeaderPayload
		if len(data) != want {
			return fmt.Errorf("size %d, want %d", len(data), want)
		}

		prev := *running
		for height := start; height <= end; height++ {
			offset := int(height-start) * wire.MaxBlockHeaderPayload
			record := data[offset : offset+wire.MaxBlockHeaderPayload]

			hash := chainhash.DoubleHashH(record)
			if height == 0 {
				if hash != *v.params.GenesisHash {
					return fmt.Errorf("wrong genesis " +
						"block")
				}
			} else if !bytes.Equal(record[4:36], prev[:]) {
				return fmt.Errorf("header at height %d does "+
					"not link to its predecessor", height)
			}
			prev = hash
		}

		anchor, err := v.chain.GetBlockHash(int64(end))
		if err != nil {
			return fmt.Errorf("error getting anchor: %w", err)
		}
		if prev != *anchor {
			return fmt.Errorf("last header (%s) does not match "+
				"the backend's block at height %d (%s)", prev,
				end, anchor)
		}

		*running = prev

		return nil
	}

	err := verify()
	if err == nil {
		fmt.Printf("headers %d-%d: OK\n", start, end)
		return nil
	}

	fmt.Printf("headers %d-%d: BAD (%v)\n", start, end, err)
	if !v.fix {
		v.badFiles = append(v.badFiles, name)
		return nil
	}

	err = v.regenerate(name, start, end, v.writeHeaderRecord, "headers")
	if err != nil {
		return err
	}

	if err := verify(); err != nil {
		return fmt.Errorf("%s still corrupt after fix: %w", name, err)
	}
	fmt.Printf("headers %d-%d: fixed\n", start, end)

	return nil
}

// verifyFilterHeaderAnchor checks a sealed filter-header file's size and its
// last entry against the backend. Interior entries are covered by the
// filter chain closure in verifyFilterFiles.
func (v *verifier) verifyFilterHeaderAnchor(name string, start,
	end int32) error {

	verify := func() error {
		data, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("error reading: %w", err)
		}
		want := int(v.headersPerFile) * chainhash.HashSize
		if len(data) != want {
			return fmt.Errorf("size %d, want %d", len(data), want)
		}

		authority, err := v.authFilterHeader(end)
		if err != nil {
			return err
		}
		stored := data[len(data)-chainhash.HashSize:]
		if !bytes.Equal(stored, authority[:]) {
			return fmt.Errorf("last filter header does not " +
				"match the backend")
		}

		return nil
	}

	err := verify()
	if err == nil {
		fmt.Printf("filter headers %d-%d: OK\n", start, end)
		return nil
	}

	fmt.Printf("filter headers %d-%d: BAD (%v)\n", start, end, err)
	if !v.fix {
		v.badFiles = append(v.badFiles, name)
		return nil
	}

	err = v.regenerate(
		name, start, end, v.writeFilterHeaderRecord, "filter headers",
	)
	if err != nil {
		return err
	}

	v.fHeaderCacheStart = -1
	if err := verify(); err != nil {
		return fmt.Errorf("%s still corrupt after fix: %w", name, err)
	}
	fmt.Printf("filter headers %d-%d: fixed\n", start, end)

	return nil
}

// verifyMemoryTail verifies the unsealed tail range that only exists in the
// running server's memory: the block headers must continue the chain of the
// last sealed file and anchor at the backend, and the filter headers must
// anchor at the backend too. --fix can't repair this range — a divergence
// means the server ingested a block that was later reorged away, and only a
// restart (which re-indexes the unsealed range) heals its memory.
func (v *verifier) verifyMemoryTail(start int32,
	running chainhash.Hash) error {

	headerData, err := v.fetchInMemory(memoryHeadersRoute, start)
	if err != nil {
		return err
	}
	if len(headerData) == 0 ||
		len(headerData)%wire.MaxBlockHeaderPayload != 0 {

		return fmt.Errorf("in-memory headers response has invalid "+
			"length %d", len(headerData))
	}
	end := start + int32(len(headerData)/wire.MaxBlockHeaderPayload) - 1

	numBad := len(v.badFiles)
	err = v.checkMemoryHeaders(headerData, start, end, running)
	if err != nil {
		fmt.Printf("in-memory headers %d-%d: BAD (%v)\n", start, end,
			err)
		v.badFiles = append(v.badFiles, fmt.Sprintf(
			"in-memory headers %d-%d", start, end,
		))
	} else {
		fmt.Printf("in-memory headers %d-%d: OK\n", start, end)
	}

	fhData, err := v.fetchInMemory(memoryFilterHeadersRoute, start)
	if err != nil {
		return err
	}
	if len(fhData) == 0 || len(fhData)%chainhash.HashSize != 0 {
		return fmt.Errorf("in-memory filter headers response has "+
			"invalid length %d", len(fhData))
	}
	fhEnd := start + int32(len(fhData)/chainhash.HashSize) - 1

	err = v.checkMemoryFilterHeaders(fhData, start, fhEnd)
	if err != nil {
		fmt.Printf("in-memory filter headers %d-%d: BAD (%v)\n",
			start, fhEnd, err)
		v.badFiles = append(v.badFiles, fmt.Sprintf(
			"in-memory filter headers %d-%d", start, fhEnd,
		))
	} else {
		fmt.Printf("in-memory filter headers %d-%d: OK\n", start,
			fhEnd)
	}

	if v.fix && len(v.badFiles) > numBad {
		fmt.Println("in-memory data can't be fixed; restart the " +
			"block-dn server to re-index the unsealed range")
	}

	return nil
}

// checkMemoryHeaders verifies that the in-memory tail headers continue the
// chain of the last sealed file (or start at genesis) and anchors the
// highest reorg-safe height at the backend. The topmost reOrgSafeDepth
// headers stay unanchored on purpose: they may legitimately still be
// reorged away, and a spurious mismatch would read like corruption.
func (v *verifier) checkMemoryHeaders(data []byte, start, end int32,
	running chainhash.Hash) error {

	var (
		anchorHeight = end - v.reOrgSafeDepth
		anchorHash   chainhash.Hash
	)

	prev := running
	for height := start; height <= end; height++ {
		offset := int(height-start) * wire.MaxBlockHeaderPayload
		record := data[offset : offset+wire.MaxBlockHeaderPayload]

		hash := chainhash.DoubleHashH(record)
		if height == 0 {
			if hash != *v.params.GenesisHash {
				return fmt.Errorf("wrong genesis block")
			}
		} else if !bytes.Equal(record[4:36], prev[:]) {
			return fmt.Errorf("header at height %d does not "+
				"link to its predecessor", height)
		}
		prev = hash

		if height == anchorHeight {
			anchorHash = hash
		}
	}

	if anchorHeight < start {
		return nil
	}

	anchor, err := v.chain.GetBlockHash(int64(anchorHeight))
	if err != nil {
		return fmt.Errorf("error getting anchor: %w", err)
	}
	if anchorHash != *anchor {
		return fmt.Errorf("header at height %d (%s) does not match "+
			"the backend's block (%s)", anchorHeight, anchorHash,
			anchor)
	}

	return nil
}

// checkMemoryFilterHeaders anchors the in-memory tail filter headers at the
// backend at the highest reorg-safe height. Because a filter header commits
// to all filters before it, a single anchor proves the whole chain below
// it; the sealed filter files of the tail range additionally chain-close
// against the individual values in verifyFilterFiles.
func (v *verifier) checkMemoryFilterHeaders(data []byte, start,
	end int32) error {

	anchorHeight := end - v.reOrgSafeDepth
	if anchorHeight < start {
		return nil
	}

	authority, err := v.authFilterHeader(anchorHeight)
	if err != nil {
		return err
	}

	var stored chainhash.Hash
	offset := int(anchorHeight-start) * chainhash.HashSize
	copy(stored[:], data[offset:])
	if stored != authority {
		return fmt.Errorf("filter header at height %d does not "+
			"match the backend", anchorHeight)
	}

	return nil
}

// verifyFilterFiles walks all sealed compact-filter files and verifies each
// one exactly like a light client does: the BIP157 commitment chain over
// the file's filters, anchored at the stored filter headers on both ends,
// must close. A failing range is diagnosed per height against the backend
// to identify (and with --fix, regenerate) the culprit file(s).
func (v *verifier) verifyFilterFiles() error {
	filterDir := filepath.Join(v.baseDir, FilterFileDir)

	for start := int32(0); ; start += v.filtersPerFile {
		end := start + v.filtersPerFile - 1
		name := fmt.Sprintf(
			FilterFileNamePattern, filterDir, start, end,
		)
		if !fileExists(name) {
			return nil
		}

		if err := v.verifyOneFilterFile(name, start, end); err != nil {
			return err
		}
	}
}

// verifyOneFilterFile checks the chain closure of one sealed filter file
// against the stored filter headers.
func (v *verifier) verifyOneFilterFile(name string, start, end int32) error {
	filters, err := v.readFilterFile(name)
	if err == nil {
		err = v.checkFilterChain(filters, start, end)
	}
	if err == nil {
		fmt.Printf("filters %d-%d: OK\n", start, end)
		return nil
	}

	fmt.Printf("filters %d-%d: BAD (%v)\n", start, end, err)
	if !v.fix {
		v.badFiles = append(v.badFiles, name)
		return nil
	}

	// Diagnose which store diverges from the backend before rewriting
	// anything: the filters file, the filter-header file, or both.
	badFilters, badFHeaders, err := v.diagnoseRange(filters, start, end)
	if err != nil {
		return err
	}

	if badFilters {
		err := v.regenerate(
			name, start, end, v.writeFilterRecord, "filters",
		)
		if err != nil {
			return err
		}
	}
	if badFHeaders {
		headerDir := filepath.Join(v.baseDir, HeaderFileDir)
		fhStart := start - (start % v.headersPerFile)
		fhName := fmt.Sprintf(
			FilterHeaderFileNamePattern, headerDir, fhStart,
			fhStart+v.headersPerFile-1,
		)

		// An unsealed range's filter headers only exist in the
		// running server's memory; there is no file to regenerate.
		if !fileExists(fhName) {
			return fmt.Errorf("filter headers of heights %d-%d "+
				"diverge from the backend but aren't sealed "+
				"to disk yet; restart the block-dn server to "+
				"re-index them", start, end)
		}

		err := v.regenerate(
			fhName, fhStart, fhStart+v.headersPerFile-1,
			v.writeFilterHeaderRecord, "filter headers",
		)
		if err != nil {
			return err
		}
		v.fHeaderCacheStart = -1
	}

	filters, err = v.readFilterFile(name)
	if err == nil {
		err = v.checkFilterChain(filters, start, end)
	}
	if err != nil {
		return fmt.Errorf("%s still corrupt after fix: %w", name, err)
	}
	fmt.Printf("filters %d-%d: fixed\n", start, end)

	return nil
}

// checkFilterChain recomputes the BIP157 commitment chain over the given
// filters and requires it to run from the stored filter header before the
// range to the stored filter header at its end.
func (v *verifier) checkFilterChain(filters [][]byte, start,
	end int32) error {

	var prev chainhash.Hash
	if start > 0 {
		stored, err := v.storedFilterHeader(start - 1)
		if err != nil {
			return err
		}
		prev = stored
	}
	want, err := v.storedFilterHeader(end)
	if err != nil {
		return err
	}

	for _, filter := range filters {
		filterHash := chainhash.DoubleHashH(filter)
		prev = chainhash.DoubleHashH(
			append(filterHash[:], prev[:]...),
		)
	}
	if prev != want {
		return fmt.Errorf("filter chain does not close against the " +
			"stored filter headers")
	}

	return nil
}

// diagnoseRange compares every height of a failing range against the
// backend and reports whether the filters file and/or the filter-header
// entries diverge.
func (v *verifier) diagnoseRange(filters [][]byte, start, end int32) (bool,
	bool, error) {

	var badFilters, badFHeaders bool
	for height := start; height <= end; height++ {
		hash, err := v.chain.GetBlockHash(int64(height))
		if err != nil {
			return false, false, err
		}
		auth, err := v.chain.GetBlockFilter(*hash, &filterBasic)
		if err != nil {
			return false, false, err
		}

		authFilter, err := hex.DecodeString(auth.Filter)
		if err != nil {
			return false, false, err
		}
		if !bytes.Equal(filters[height-start], authFilter) {
			fmt.Printf("  filter at height %d diverges from "+
				"the backend\n", height)
			badFilters = true
		}

		authHeader, err := chainhash.NewHashFromStr(auth.Header)
		if err != nil {
			return false, false, err
		}
		stored, err := v.storedFilterHeader(height)
		if err != nil {
			return false, false, err
		}
		if stored != *authHeader {
			fmt.Printf("  filter header at height %d diverges "+
				"from the backend\n", height)
			badFHeaders = true
		}
	}

	return badFilters, badFHeaders, nil
}

// storedFilterHeader returns the filter header at the given height, reading
// it from the sealed filter-header files or, for a range that isn't sealed
// yet, from the running server's memory, caching the most recently loaded
// range.
func (v *verifier) storedFilterHeader(height int32) (chainhash.Hash, error) {
	var result chainhash.Hash

	fileStart := height - (height % v.headersPerFile)
	for attempt := 0; ; attempt++ {
		if v.fHeaderCacheStart != fileStart {
			if err := v.loadFilterHeaders(fileStart); err != nil {
				return result, fmt.Errorf("error loading "+
					"filter headers for height %d: %w",
					height, err)
			}
		}

		offset := int(height-fileStart) * chainhash.HashSize
		if offset+chainhash.HashSize <= len(v.fHeaderCache) {
			copy(result[:], v.fHeaderCache[offset:])
			return result, nil
		}

		// A memory-sourced snapshot may simply predate this height
		// (the server keeps indexing while we verify), so refresh it
		// once before giving up.
		if !v.fHeaderCacheFromMemory || attempt > 0 {
			return result, fmt.Errorf("filter header data for "+
				"height %d is truncated", height)
		}
		v.fHeaderCacheStart = -1
	}
}

// loadFilterHeaders fills the filter-header cache with the range starting at
// fileStart, from the sealed file on disk if it exists or from the running
// server's memory otherwise.
func (v *verifier) loadFilterHeaders(fileStart int32) error {
	headerDir := filepath.Join(v.baseDir, HeaderFileDir)
	name := fmt.Sprintf(
		FilterHeaderFileNamePattern, headerDir, fileStart,
		fileStart+v.headersPerFile-1,
	)

	data, err := os.ReadFile(name)
	switch {
	case err == nil:
		v.fHeaderCacheFromMemory = false

	// The range isn't sealed yet, so its filter headers only exist in
	// the running server's memory.
	case os.IsNotExist(err):
		data, err = v.fetchInMemory(memoryFilterHeadersRoute, fileStart)
		if err != nil {
			return err
		}
		v.fHeaderCacheFromMemory = true

	default:
		return err
	}

	v.fHeaderCache = data
	v.fHeaderCacheStart = fileStart

	return nil
}

// fetchInMemory retrieves a height-based range that isn't sealed to disk yet
// from the running block-dn server's memory.
func (v *verifier) fetchInMemory(route string, start int32) ([]byte, error) {
	url := fmt.Sprintf("%s/%s/%d", v.serverURL, route, start)
	resp, err := v.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("error fetching unsealed range from "+
			"%s (is the block-dn server running?): %w", url, err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error reading response of %s: %w",
			url, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from %s: %s",
			resp.StatusCode, url, body)
	}

	return body, nil
}

// serverBaseURL derives the HTTP base URL of the running block-dn server
// from its --listen-addr value. A wildcard or empty listen host (":8080",
// "0.0.0.0:8080", "[::]:8080") can't be dialed, so it is replaced with the
// loopback name, which reaches the server from within the same (docker)
// network namespace.
func serverBaseURL(listenAddr string) (string, error) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		return "", fmt.Errorf("invalid --listen-addr %s: %w",
			listenAddr, err)
	}

	if ip := net.ParseIP(host); host == "" ||
		(ip != nil && ip.IsUnspecified()) {

		host = "localhost"
	}

	return "http://" + net.JoinHostPort(host, port), nil
}

// readFilterFile parses a sealed var-int prefixed filter file.
func (v *verifier) readFilterFile(name string) ([][]byte, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("error reading: %w", err)
	}

	reader := bytes.NewReader(data)
	filters := make([][]byte, 0, v.filtersPerFile)
	for i := range v.filtersPerFile {
		filter, err := wire.ReadVarBytes(
			reader, 0, wire.MaxBlockPayload, "filter",
		)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		filters = append(filters, filter)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("%d trailing bytes", reader.Len())
	}

	return filters, nil
}

// regenerate rewrites one sealed file from the backend, writing every
// height's record via writeRecord into a temp file that atomically replaces
// the corrupt one.
func (v *verifier) regenerate(name string, start, end int32,
	writeRecord func(w *os.File, height int32) error,
	kind string) error {

	fmt.Printf("  regenerating %s %d-%d from the backend...\n", kind,
		start, end)

	tmp, err := os.CreateTemp(filepath.Dir(name), "verify-fix-*")
	if err != nil {
		return fmt.Errorf("error creating temp file: %w", err)
	}
	defer func() {
		_ = os.Remove(tmp.Name())
	}()

	for height := start; height <= end; height++ {
		if err := writeRecord(tmp, height); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("error writing height %d: %w",
				height, err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("error closing temp file: %w", err)
	}

	if err := os.Rename(tmp.Name(), name); err != nil {
		return fmt.Errorf("error replacing %s: %w", name, err)
	}
	v.fixedFiles = append(v.fixedFiles, name)

	return nil
}

// writeHeaderRecord fetches and serializes one block header.
func (v *verifier) writeHeaderRecord(w *os.File, height int32) error {
	hash, err := v.chain.GetBlockHash(int64(height))
	if err != nil {
		return err
	}
	header, err := v.chain.GetBlockHeader(hash)
	if err != nil {
		return err
	}

	return header.Serialize(w)
}

// writeFilterHeaderRecord fetches and writes one 32-byte filter header.
func (v *verifier) writeFilterHeaderRecord(w *os.File, height int32) error {
	header, err := v.authFilterHeader(height)
	if err != nil {
		return err
	}
	_, err = w.Write(header[:])

	return err
}

// writeFilterRecord fetches and writes one var-int prefixed filter.
func (v *verifier) writeFilterRecord(w *os.File, height int32) error {
	hash, err := v.chain.GetBlockHash(int64(height))
	if err != nil {
		return err
	}
	filter, err := v.chain.GetBlockFilter(*hash, &filterBasic)
	if err != nil {
		return err
	}
	filterBytes, err := hex.DecodeString(filter.Filter)
	if err != nil {
		return err
	}

	return wire.WriteVarBytes(w, 0, filterBytes)
}

// authFilterHeader returns the backend's authoritative filter header at the
// given height.
func (v *verifier) authFilterHeader(height int32) (chainhash.Hash, error) {
	var result chainhash.Hash

	hash, err := v.chain.GetBlockHash(int64(height))
	if err != nil {
		return result, err
	}
	filter, err := v.chain.GetBlockFilter(*hash, &filterBasic)
	if err != nil {
		return result, err
	}
	parsed, err := chainhash.NewHashFromStr(filter.Header)
	if err != nil {
		return result, err
	}

	return *parsed, nil
}
