package main

import (
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
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
	backend, _, _, cleanup := newBitcoind(t, testDir, []string{
		"-regtest",
		"-disablewallet",
	})

	t.Cleanup(func() {
		require.NoError(t, cleanup())
	})

	setupLogging(unitTestDir, "debug")
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

// TestHeightToHashCacheInvalidate exercises the invalidate path in
// isolation — no backend required, since invalidate only mutates in-memory
// state. The semantic under test: after invalidate(N), heights < N stay
// cached, heights >= N are gone, and bestHeight is reset to N-1 so the
// next getBlockHash for a height in the dropped range goes back to
// bitcoind.
func TestHeightToHashCacheInvalidate(t *testing.T) {
	c := newH2HCache(nil)
	for i := range int32(101) {
		c.heightToHash[i] = chainhash.Hash{byte(i)}
	}
	c.bestHeight.Store(100)

	c.invalidate(50)

	require.EqualValues(t, 49, c.bestHeight.Load())
	for i := range int32(50) {
		_, ok := c.heightToHash[i]
		require.Truef(t, ok, "expected height %d still cached", i)
	}
	for i := int32(50); i <= 100; i++ {
		_, ok := c.heightToHash[i]
		require.Falsef(t, ok, "expected height %d cleared", i)
	}
}

// TestHeightToHashCacheInvalidateEmpty covers the no-op edge case:
// invalidating at a height above bestHeight must leave the cache and
// bestHeight semantics consistent (bestHeight = fromHeight-1, even though
// we evicted nothing).
func TestHeightToHashCacheInvalidateAboveBest(t *testing.T) {
	c := newH2HCache(nil)
	for i := range int32(11) {
		c.heightToHash[i] = chainhash.Hash{byte(i)}
	}
	c.bestHeight.Store(10)

	c.invalidate(50)

	// No entries cleared (50 > bestHeight=10), but bestHeight was reset
	// to fromHeight-1=49, which is consistent with the contract: the
	// caller asserted everything from fromHeight onward is unknown.
	require.EqualValues(t, 49, c.bestHeight.Load())
	for i := range int32(11) {
		_, ok := c.heightToHash[i]
		require.Truef(t, ok, "expected height %d still cached", i)
	}
}

// TestHeightToHashCacheInvalidateFromZero covers the full-wipe case.
func TestHeightToHashCacheInvalidateFromZero(t *testing.T) {
	c := newH2HCache(nil)
	for i := range int32(11) {
		c.heightToHash[i] = chainhash.Hash{byte(i)}
	}
	c.bestHeight.Store(10)

	c.invalidate(0)

	require.EqualValues(t, -1, c.bestHeight.Load())
	require.Empty(t, c.heightToHash)
}
