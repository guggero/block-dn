package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/lntest/unittest"
	"github.com/stretchr/testify/require"
)

// buildFilterFixture writes a synthetic base-dir with numFilters var-int
// prefixed filters (perFile per filters file) and the matching BIP157
// filter-header file, returning the verifier and the raw filters.
func buildFilterFixture(t *testing.T, perFile, numFilters int) (*verifier,
	[][]byte) {

	baseDir := t.TempDir()
	headerDir := filepath.Join(baseDir, HeaderFileDir)
	filterDir := filepath.Join(baseDir, FilterFileDir)
	require.NoError(t, os.MkdirAll(headerDir, 0o755))
	require.NoError(t, os.MkdirAll(filterDir, 0o755))

	// Deterministic dummy filters plus their real commitment chain.
	filters := make([][]byte, numFilters)
	fHeaders := make([]byte, 0, numFilters*chainhash.HashSize)
	var prev chainhash.Hash
	for i := range filters {
		filters[i] = []byte{byte(i), byte(i >> 8), 0xab}
		filterHash := chainhash.DoubleHashH(filters[i])
		prev = chainhash.DoubleHashH(
			append(filterHash[:], prev[:]...),
		)
		fHeaders = append(fHeaders, prev[:]...)
	}

	// One filter-header file covering all heights.
	fhName := fmt.Sprintf(
		FilterHeaderFileNamePattern, headerDir, 0, numFilters-1,
	)
	require.NoError(t, os.WriteFile(fhName, fHeaders, 0o644))

	// Filters files, perFile entries each.
	for start := 0; start < numFilters; start += perFile {
		name := fmt.Sprintf(
			FilterFileNamePattern, filterDir, start,
			start+perFile-1,
		)
		f, err := os.Create(name)
		require.NoError(t, err)
		for i := start; i < start+perFile; i++ {
			require.NoError(t, wire.WriteVarBytes(
				f, 0, filters[i],
			))
		}
		require.NoError(t, f.Close())
	}

	return &verifier{
		baseDir:           baseDir,
		headersPerFile:    int32(numFilters),
		filtersPerFile:    int32(perFile),
		fHeaderCacheStart: -1,
	}, filters
}

// TestVerifyFilterChain checks that the offline filter chain verification
// accepts consistent files and pinpoints corrupted ones.
func TestVerifyFilterChain(t *testing.T) {
	const perFile, numFilters = 3, 6
	v, _ := buildFilterFixture(t, perFile, numFilters)
	filterDir := filepath.Join(v.baseDir, FilterFileDir)

	// Both files verify clean.
	for start := int32(0); start < numFilters; start += perFile {
		name := fmt.Sprintf(
			FilterFileNamePattern, filterDir, start,
			start+perFile-1,
		)
		filters, err := v.readFilterFile(name)
		require.NoError(t, err)
		require.NoError(t, v.checkFilterChain(
			filters, start, start+perFile-1,
		))
	}

	// Corrupt one byte of the second file's middle filter: the chain of
	// that file must no longer close, while the first file stays OK.
	name := fmt.Sprintf(
		FilterFileNamePattern, filterDir, 3, 5,
	)
	data, err := os.ReadFile(name)
	require.NoError(t, err)
	data[6] ^= 0x01
	require.NoError(t, os.WriteFile(name, data, 0o644))

	filters, err := v.readFilterFile(name)
	require.NoError(t, err)
	require.ErrorContains(
		t, v.checkFilterChain(filters, 3, 5), "does not close",
	)

	name = fmt.Sprintf(FilterFileNamePattern, filterDir, 0, 2)
	filters, err = v.readFilterFile(name)
	require.NoError(t, err)
	require.NoError(t, v.checkFilterChain(filters, 0, 2))
}

// TestReadFilterFile checks the trailing-bytes and truncation detection of
// the filter file parser.
func TestReadFilterFile(t *testing.T) {
	const perFile, numFilters = 3, 3
	v, _ := buildFilterFixture(t, perFile, numFilters)
	filterDir := filepath.Join(v.baseDir, FilterFileDir)
	name := fmt.Sprintf(FilterFileNamePattern, filterDir, 0, 2)

	data, err := os.ReadFile(name)
	require.NoError(t, err)

	// Trailing garbage is rejected.
	require.NoError(t, os.WriteFile(
		name, append(data, 0xff), 0o644,
	))
	_, err = v.readFilterFile(name)
	require.ErrorContains(t, err, "trailing")

	// A truncated file is rejected.
	require.NoError(t, os.WriteFile(name, data[:len(data)-1], 0o644))
	_, err = v.readFilterFile(name)
	require.Error(t, err)
}

// filterHeaderChain recomputes the BIP157 commitment chain over the given
// filters, returning the filter header after the last one.
func filterHeaderChain(filters [][]byte) chainhash.Hash {
	var prev chainhash.Hash
	for _, filter := range filters {
		filterHash := chainhash.DoubleHashH(filter)
		prev = chainhash.DoubleHashH(
			append(filterHash[:], prev[:]...),
		)
	}

	return prev
}

// TestStoredFilterHeader checks the height-to-file offset lookup, including
// the out-of-range case.
func TestStoredFilterHeader(t *testing.T) {
	const perFile, numFilters = 3, 6
	v, filters := buildFilterFixture(t, perFile, numFilters)

	stored, err := v.storedFilterHeader(4)
	require.NoError(t, err)
	require.Equal(t, filterHeaderChain(filters[:5]), stored)

	// A height beyond the sealed range triggers an in-memory fetch; with
	// the server responding like the real one does for a start height
	// beyond its tip, the lookup must fail cleanly.
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("start height is greater " +
				"than current height"))
		},
	))
	t.Cleanup(srv.Close)
	v.serverURL = srv.URL
	v.httpClient = srv.Client()

	_, err = v.storedFilterHeader(int32(numFilters) + 3)
	require.ErrorContains(t, err, "unexpected status")
}

// TestStoredFilterHeaderMemory checks that filter headers of a range that
// has no sealed filter-header file on disk are fetched from the running
// server's memory, including the snapshot refresh when the verification walk
// outgrows a previously fetched snapshot.
func TestStoredFilterHeaderMemory(t *testing.T) {
	const perFile, numFilters = 2, 6
	v, filters := buildFilterFixture(t, perFile, numFilters)

	// Move the filter headers from disk into a fake block-dn server, so
	// they're only reachable over HTTP, like the unsealed tail range of
	// a running server.
	headerDir := filepath.Join(v.baseDir, HeaderFileDir)
	fhName := fmt.Sprintf(
		FilterHeaderFileNamePattern, headerDir, 0, numFilters-1,
	)
	fHeaders, err := os.ReadFile(fhName)
	require.NoError(t, err)
	require.NoError(t, os.Remove(fhName))

	// The first snapshot ends below the later requested heights, like a
	// server that hasn't indexed them yet; every following fetch returns
	// the full range (or a short one again, once alwaysShort is set).
	var (
		calls       atomic.Int32
		alwaysShort atomic.Bool
	)
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/filter-headers/0" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			if calls.Add(1) == 1 || alwaysShort.Load() {
				_, _ = w.Write(
					fHeaders[:2*chainhash.HashSize],
				)
				return
			}
			_, _ = w.Write(fHeaders)
		},
	))
	t.Cleanup(srv.Close)
	v.serverURL = srv.URL
	v.httpClient = srv.Client()

	// Height 1 is covered by the first, short snapshot.
	stored, err := v.storedFilterHeader(1)
	require.NoError(t, err)
	require.Equal(t, filterHeaderChain(filters[:2]), stored)

	// Height 4 is beyond it, so the snapshot must be refreshed.
	stored, err = v.storedFilterHeader(4)
	require.NoError(t, err)
	require.Equal(t, filterHeaderChain(filters[:5]), stored)
	require.EqualValues(t, 2, calls.Load())

	// The filter chain closure works against the memory-sourced filter
	// headers as well.
	filterDir := filepath.Join(v.baseDir, FilterFileDir)
	name := fmt.Sprintf(FilterFileNamePattern, filterDir, 2, 3)
	fileFilters, err := v.readFilterFile(name)
	require.NoError(t, err)
	require.NoError(t, v.checkFilterChain(fileFilters, 2, 3))

	// A height the server doesn't cover even after a refresh is reported
	// as truncated data.
	alwaysShort.Store(true)
	v.fHeaderCacheStart = -1
	_, err = v.storedFilterHeader(4)
	require.ErrorContains(t, err, "truncated")
}

// TestServerBaseURL checks the --listen-addr to base-URL derivation,
// especially the wildcard listen hosts that can't be dialed directly.
func TestServerBaseURL(t *testing.T) {
	cases := []struct {
		listenAddr string
		want       string
		wantErr    bool
	}{
		{listenAddr: "localhost:8080", want: "http://localhost:8080"},
		{listenAddr: "block-dn:8080", want: "http://block-dn:8080"},
		{listenAddr: "127.0.0.1:8334", want: "http://127.0.0.1:8334"},
		{listenAddr: "0.0.0.0:8080", want: "http://localhost:8080"},
		{listenAddr: ":8080", want: "http://localhost:8080"},
		{listenAddr: "[::]:8080", want: "http://localhost:8080"},
		{
			listenAddr: "[2001:db8::1]:8080",
			want:       "http://[2001:db8::1]:8080",
		},
		{listenAddr: "no-port", wantErr: true},
	}

	for _, tc := range cases {
		got, err := serverBaseURL(tc.listenAddr)
		if tc.wantErr {
			require.Error(t, err)
			continue
		}

		require.NoError(t, err)
		require.Equal(t, tc.want, got)
	}
}

// TestCheckMemoryHeaders checks the linkage and genesis validation of the
// in-memory tail headers. The backend anchor is skipped here by using a
// depth larger than the tail; it is covered by the integration test.
func TestCheckMemoryHeaders(t *testing.T) {
	v := &verifier{
		params:         &chaincfg.RegressionNetParams,
		reOrgSafeDepth: 100,
	}

	// Fake 80-byte header records: only the prev-hash field (bytes 4-36)
	// matters for the linkage check.
	makeRecord := func(i byte, prev chainhash.Hash) []byte {
		record := make([]byte, wire.MaxBlockHeaderPayload)
		record[0] = i
		copy(record[4:36], prev[:])
		return record
	}

	var running chainhash.Hash
	running[0] = 0xaa

	var data []byte
	prev := running
	for i := range 3 {
		record := makeRecord(byte(i), prev)
		prev = chainhash.DoubleHashH(record)
		data = append(data, record...)
	}

	require.NoError(t, v.checkMemoryHeaders(data, 7, 9, running))

	// A record that doesn't link to its predecessor is caught.
	data[wire.MaxBlockHeaderPayload+4] ^= 0x01
	require.ErrorContains(
		t, v.checkMemoryHeaders(data, 7, 9, running), "does not link",
	)

	// A tail that starts at height 0 must begin with the genesis block.
	var genesisBuf bytes.Buffer
	require.NoError(
		t, v.params.GenesisBlock.Header.Serialize(&genesisBuf),
	)
	second := makeRecord(1, *v.params.GenesisHash)
	data = append(genesisBuf.Bytes(), second...)
	require.NoError(t, v.checkMemoryHeaders(
		data, 0, 1, chainhash.Hash{},
	))

	// Anything else at height 0 is rejected.
	data = append(makeRecord(0, chainhash.Hash{}), second...)
	require.ErrorContains(t, v.checkMemoryHeaders(
		data, 0, 1, chainhash.Hash{},
	), "genesis")
}

// TestVerifyMemoryTail runs the verifier against a live server whose last
// header range isn't sealed yet — the mainnet steady state, where filter
// files seal every 2k blocks but header files only every 100k — so the tail
// filter files can only be checked against in-memory filter headers.
func TestVerifyMemoryTail(t *testing.T) {
	params := reorgTestParams{
		headersPerFile:  600,
		filtersPerFile:  100,
		spTweaksPerFile: 600,
		reOrgSafeDepth:  6,
	}
	ctx := startReorgTestServer(t, params)

	// Mine past the first header-file boundary (so file 0-599 seals) and
	// far enough past it that a full filter file inside the second,
	// still-unsealed header range seals too.
	_, minerHeight := ctx.miner.GetBestBlock()
	require.Less(t, minerHeight, int32(720))
	_ = ctx.miner.MineEmptyBlocks(int(750 - minerHeight))
	ctx.waitBackendSync(t)

	waitForSealedHeight(t, ctx.server.headerFiles, 599)
	waitForSealedHeight(t, ctx.server.cFilterFiles, 699)
	waitForCurrentHeight(t, ctx.server.headerFiles, 750)
	waitForCurrentHeight(t, ctx.server.cFilterFiles, 750)

	serverURL, err := serverBaseURL(ctx.server.listenAddr)
	require.NoError(t, err)

	newVerifier := func(fix bool) *verifier {
		return &verifier{
			chain:          ctx.backend,
			params:         unittest.NetParams,
			baseDir:        ctx.server.baseDir,
			headersPerFile: params.headersPerFile,
			filtersPerFile: params.filtersPerFile,
			fix:            fix,
			reOrgSafeDepth: int32(params.reOrgSafeDepth),
			serverURL:      serverURL,
			httpClient: &http.Client{
				Timeout: memoryFetchTimeout,
			},
			fHeaderCacheStart: -1,
		}
	}

	runWalk := func(v *verifier) {
		t.Helper()
		require.NoError(t, v.verifyHeaderFiles())
		require.NoError(t, v.verifyFilterFiles())
	}

	// A clean run: the sealed files verify against the disk data, the
	// tail (headers 600-750, filter file 600-699) against the server's
	// memory.
	v := newVerifier(false)
	runWalk(v)
	require.Empty(t, v.badFiles)

	// Corrupt a filter body byte of the sealed filter file inside the
	// unsealed header range: it must be detected...
	filterDir := filepath.Join(ctx.server.baseDir, FilterFileDir)
	name := fmt.Sprintf(FilterFileNamePattern, filterDir, 600, 699)
	data, err := os.ReadFile(name)
	require.NoError(t, err)
	data[1] ^= 0x01
	require.NoError(t, os.WriteFile(name, data, 0o644))

	v = newVerifier(false)
	runWalk(v)
	require.Equal(t, []string{name}, v.badFiles)

	// ...and repaired from the backend in fix mode, even though its
	// filter headers only exist in the server's memory.
	v = newVerifier(true)
	runWalk(v)
	require.Empty(t, v.badFiles)
	require.Equal(t, []string{name}, v.fixedFiles)

	// An unparseable file (trailing garbage) is detected and repaired
	// the same way.
	data, err = os.ReadFile(name)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(name, append(data, 0xff), 0o644))

	v = newVerifier(true)
	runWalk(v)
	require.Empty(t, v.badFiles)
	require.Equal(t, []string{name}, v.fixedFiles)

	// The final state verifies clean again.
	v = newVerifier(false)
	runWalk(v)
	require.Empty(t, v.badFiles)
}
