package main

import (
	"net/http/httptest"
	"testing"

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
