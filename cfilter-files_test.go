package main

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/stretchr/testify/require"
)

func TestFilterHeaderConstruction(t *testing.T) {
	// Filter header from mainnet block 499.
	prevHeader, _ := chainhash.NewHashFromStr(
		"bec9900067347f68be6c6c38b2b86c026c7a2535c221cf3af0388860234c" +
			"c98b",
	)

	// Filter bytes from mainnet block 500.
	filterBytes, _ := hex.DecodeString("017f23b0")

	filterHash := chainhash.DoubleHashB(filterBytes)
	filterHash = append(filterHash, prevHeader[:]...)
	filterHeader2 := chainhash.DoubleHashH(filterHash)

	// Filter header from mainnet block 500.
	expectedHeader, _ := chainhash.NewHashFromStr(
		"0f3bc0ff8fe676832252d8fbd2aefb91b606a27e64ab4acf09971bd882c0" +
			"f011",
	)
	require.Equal(t, expectedHeader[:], filterHeader2[:])
}
