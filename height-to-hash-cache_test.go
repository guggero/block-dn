package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	expectedH2H = map[int32]string{
		0: "000000000019d6689c085ae165831e934ff763ae46a2a6c172b3" +
			"f1b60a8ce26f",
		9_999: "00000000fbc97cc6c599ce9c24dd4a2243e2bfd518eda56e1d5e" +
			"47d29e29c3a7",
		10_000: "0000000099c744455f58e6c6e98b671e1bf7f37346bfd4cf5d02" +
			"74ad8ee660cb",
		19_999: "00000000ba36eb929dc90170a96ee3efb76cbebee0e0e5c4da9e" +
			"b0b6e74d9124",
		20_000: "",
	}
)

func TestHeightToHashCache(t *testing.T) {
	testDir := t.TempDir()
	backend, _, cleanup := newBitcoind(t, testDir, []string{
		"-regtest",
		"-disablewallet",
	})

	t.Cleanup(func() {
		require.NoError(t, cleanup())
	})

	setupLogging(".unit-test-logs")
	c := newH2HCache(backend)

	bestHeight, err := c.loadFromHeaders("testdata")
	require.NoError(t, err)

	require.Equal(t, 19_999, int(bestHeight))

	for height, expectedHashStr := range expectedH2H {
		hash, ok := c.heightToHash[height]

		if expectedHashStr == "" {
			require.False(
				t, ok, "expected no hash for height %d", height,
			)

			_, err := c.getBlockHash(height)
			require.ErrorContains(
				t, err, "error fetching block hash",
			)

			continue
		}

		require.Equal(t, expectedHashStr, hash.String())

		h, err := c.getBlockHash(height)
		require.NoError(t, err)
		require.Equal(t, expectedHashStr, h.String())
	}
}
