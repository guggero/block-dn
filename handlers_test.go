package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// fakeProcessor is a tiny stand-in for the producer types so we can unit
// test the height-validation helpers without spinning up bitcoind or
// pulling in any of the concrete producer plumbing.
type fakeProcessor struct {
	currentHeight    int32
	lastSealedHeight int32
	startupComplete  bool
}

func (f *fakeProcessor) isStartupComplete() bool { return f.startupComplete }
func (f *fakeProcessor) getCurrentHeight() int32 { return f.currentHeight }
func (f *fakeProcessor) getLastSealedHeight() int32 {
	return f.lastSealedHeight
}

func TestCheckStartHeight(t *testing.T) {
	const entriesPerFile = 100
	s := &server{}

	cases := []struct {
		name      string
		height    int64
		current   int32
		expectErr string
	}{{
		name:    "zero is fine",
		height:  0,
		current: 1000,
	}, {
		name:    "exact multiple of entriesPerFile is fine",
		height:  500,
		current: 1000,
	}, {
		name:    "current height itself is fine if aligned",
		height:  1000,
		current: 1000,
	}, {
		name:    "non-zero non-multiple rejected",
		height:  1,
		current: 1000,
		expectErr: "invalid start height 1, must be zero or a " +
			"multiple of 100",
	}, {
		name:    "one above current rejected even if aligned",
		height:  1100,
		current: 1000,
		expectErr: "start height 1100 is greater than current " +
			"height 1000",
	}, {
		name:    "way above current rejected first",
		height:  9999,
		current: 1000,
		expectErr: "start height 9999 is greater than current " +
			"height 1000",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeProcessor{currentHeight: tc.current}
			err := s.checkStartHeight(p, tc.height, entriesPerFile)
			if tc.expectErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.expectErr)
		})
	}
}

func TestCheckEndHeight(t *testing.T) {
	const entriesPerFile = 100
	s := &server{}

	cases := []struct {
		name      string
		height    int64
		current   int32
		expectErr string
	}{{
		// endHeight is exclusive, so 100 with currentHeight=1000
		// means "first 100 headers" — heights 0..99. Allowed.
		name:    "small value below current is fine",
		height:  100,
		current: 1000,
	}, {
		// endHeight == currentHeight means "first currentHeight
		// headers" — heights 0..currentHeight-1. Tip block excluded
		// but no error.
		name:    "exactly current height is fine",
		height:  1000,
		current: 1000,
	}, {
		// endHeight == currentHeight + 1 is the maximum useful
		// value: it includes the tip block. Must NOT be rejected.
		name:    "currentHeight + 1 is allowed (covers tip block)",
		height:  1001,
		current: 1000,
	}, {
		// One above the maximum is the first rejected value.
		name:    "currentHeight + 2 rejected",
		height:  1002,
		current: 1000,
		expectErr: "end height 1002 is greater than current " +
			"height 1000",
	}, {
		// 0 is rejected with the alignment error (the canonical
		// error message used by /headers/import/0 in testErrors).
		name:      "zero rejected",
		height:    0,
		current:   1000,
		expectErr: "invalid end height 0, must be a multiple of 100",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeProcessor{currentHeight: tc.current}
			err := s.checkEndHeight(p, tc.height, entriesPerFile)
			if tc.expectErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.expectErr)
		})
	}
}

// TestCheckImportPreconditions covers the two shared gates on the import
// endpoints (light-mode and startup) without exercising the full request
// path. Failing either gate must write the right error to the response and
// return false.
func TestCheckImportPreconditions(t *testing.T) {
	t.Run("light mode rejected", func(t *testing.T) {
		s := &server{
			lightMode:   true,
			headerFiles: &headerFiles{},
		}
		rec := httptest.NewRecorder()
		require.False(t, s.checkImportPreconditions(rec))
		require.Equal(t, 503, rec.Code)
		require.Contains(t, rec.Body.String(),
			errUnavailableInLightMode.Error())
	})

	t.Run("startup-not-complete rejected", func(t *testing.T) {
		hf := &headerFiles{}
		hf.startupComplete.Store(false)
		s := &server{
			lightMode:   false,
			headerFiles: hf,
		}
		rec := httptest.NewRecorder()
		require.False(t, s.checkImportPreconditions(rec))
		require.Equal(t, 503, rec.Code)
		require.Contains(t, rec.Body.String(),
			errStillStartingUp.Error())
	})

	t.Run("ready returns true", func(t *testing.T) {
		hf := &headerFiles{}
		hf.startupComplete.Store(true)
		s := &server{
			lightMode:   false,
			headerFiles: hf,
		}
		rec := httptest.NewRecorder()
		require.True(t, s.checkImportPreconditions(rec))
		// Default Code is 200 when WriteHeader hasn't been called.
		require.Equal(t, 200, rec.Code)
		require.Empty(t, rec.Body.String())
	})
}

// TestSizeHelpers checks the exact-size computations used for the memory-tier
// Content-Length header.
func TestSizeHelpers(t *testing.T) {
	hf := &headerFiles{}

	size, ok := hf.headersSize(0, 9)
	require.True(t, ok)
	require.EqualValues(t, 10*80, size)

	size, ok = hf.filterHeadersSize(100, 100)
	require.True(t, ok)
	require.EqualValues(t, 32, size)

	// An empty range can't be sized.
	_, ok = hf.headersSize(5, 4)
	require.False(t, ok)

	cf := &cFilterFiles{
		filters: map[int32][]byte{
			0: make([]byte, 10),
			1: make([]byte, 300),
		},
	}

	// 10-byte filter has a 1-byte var-int prefix, the 300-byte one a
	// 3-byte prefix (0xfd + uint16).
	size, ok = cf.filtersSize(0, 1)
	require.True(t, ok)
	require.EqualValues(t, 1+10+3+300, size)

	// A missing height means the size can't be computed.
	_, ok = cf.filtersSize(0, 2)
	require.False(t, ok)
}

// TestFilterSingleMemoryPath checks the /filters/single/{height} route for
// heights that are still in the unsealed in-memory tail, including the
// out-of-bounds rejection. The sealed (backend RPC) path is covered by the
// bitcoind integration test.
func TestFilterSingleMemoryPath(t *testing.T) {
	filterBytes := []byte{0x01, 0x02, 0x03, 0x04}

	cf := &cFilterFiles{
		filters: map[int32][]byte{5: filterBytes},
	}
	cf.startupComplete.Store(true)
	cf.currentHeight.Store(5)
	cf.lastSealedHeight.Store(-1)

	s := &server{cFilterFiles: cf}
	router := s.createRouter()

	t.Run("tail filter served from memory", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(
			http.MethodGet, "/filters/single/5", nil,
		)
		router.ServeHTTP(rec, req)

		require.Equal(t, 200, rec.Code)
		require.Equal(t, filterBytes, rec.Body.Bytes())
		require.Equal(t, "4", rec.Header().Get("Content-Length"))
		require.Equal(
			t, "public, max-age=60",
			rec.Header().Get(HeaderCache),
		)
		require.Equal(t, "*", rec.Header().Get(HeaderCORS))
	})

	t.Run("height above tip rejected", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(
			http.MethodGet, "/filters/single/6", nil,
		)
		router.ServeHTTP(rec, req)

		require.Equal(t, 400, rec.Code)
		require.Contains(
			t, rec.Body.String(), "greater than current height",
		)
	})
}

// TestCurrentStatus checks the /status response assembly, in particular the
// custom filter fields and their effect on all_files_synced.
func TestCurrentStatus(t *testing.T) {
	const tip = int32(42)

	h2h := newH2HCache(nil)
	h2h.heightToHash[tip] = chainhash.Hash{0x01}
	h2h.bestHeight.Store(tip)

	s := &server{
		chainParams: &testParams,
		h2hCache:    h2h,
		headerFiles: newHeaderFiles(
			100, 6, nil, nil, "", &testParams, h2h,
		),
		cFilterFiles: newCFilterFiles(
			100, 6, nil, nil, "", &testParams, h2h,
		),
	}
	s.headerFiles.currentHeight.Store(tip)
	s.headerFiles.filterHeaders[tip] = chainhash.Hash{0x02}
	s.cFilterFiles.currentHeight.Store(tip)

	// currentStatus expects the h2hCache read lock to be held.
	s.h2hCache.RLock()
	defer s.h2hCache.RUnlock()

	// Without the custom filter producer running, the status must report
	// custom filters as unavailable and ignore them for the sync flag.
	status, err := s.currentStatus()
	require.NoError(t, err)
	require.False(t, status.CustomFiltersAvailable)
	require.EqualValues(t, 0, status.BestCustomFilterHeight)
	require.True(t, status.AllFilesSynced)

	// A synced custom filter producer is reported with its height and
	// keeps the sync flag intact.
	s.customFilterFiles = newCustomFilterFiles(
		100, 200, 6, nil, nil, "", &testParams, h2h, nil,
	)
	s.customFilterFiles.currentHeight.Store(tip)

	status, err = s.currentStatus()
	require.NoError(t, err)
	require.True(t, status.CustomFiltersAvailable)
	require.Equal(t, tip, status.BestCustomFilterHeight)
	require.True(t, status.AllFilesSynced)

	// A lagging custom filter producer must flip the sync flag.
	s.customFilterFiles.currentHeight.Store(tip - 1)

	status, err = s.currentStatus()
	require.NoError(t, err)
	require.True(t, status.CustomFiltersAvailable)
	require.Equal(t, tip-1, status.BestCustomFilterHeight)
	require.False(t, status.AllFilesSynced)
}
