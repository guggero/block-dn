package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
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

// TestStoredFilterHeader checks the height-to-file offset lookup, including
// the out-of-range case.
func TestStoredFilterHeader(t *testing.T) {
	const perFile, numFilters = 3, 6
	v, filters := buildFilterFixture(t, perFile, numFilters)

	// Recompute the expected chain value at height 4.
	var prev chainhash.Hash
	for i := range 5 {
		filterHash := chainhash.DoubleHashH(filters[i])
		prev = chainhash.DoubleHashH(
			append(filterHash[:], prev[:]...),
		)
	}

	stored, err := v.storedFilterHeader(4)
	require.NoError(t, err)
	require.Equal(t, prev, stored)

	_, err = v.storedFilterHeader(int32(numFilters) + 3)
	require.Error(t, err)
}
