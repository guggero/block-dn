package main

import (
	"fmt"
	"sync"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// blockHashFetcher is a function that fetches the block hash for a given
// height.
type blockHashFetcher func(height int32) (*chainhash.Hash, error)

// heightToHashCache is an in-memory cache of height to block hash.
type heightToHashCache struct {
	sync.RWMutex
	heightToHash map[int32]chainhash.Hash
}

func newH2HCache() *heightToHashCache {
	return &heightToHashCache{
		heightToHash: make(
			map[int32]chainhash.Hash, DefaultHeightToHashCacheSize,
		),
	}
}

func (c *heightToHashCache) addBlockHash(h int32, hash chainhash.Hash) {
	c.Lock()
	defer c.Unlock()

	c.heightToHash[h] = hash
}

func (c *heightToHashCache) getBlockHash(h int32) (*chainhash.Hash, error) {
	c.RLock()
	defer c.RUnlock()

	hash, ok := c.heightToHash[h]
	if !ok {
		return nil, fmt.Errorf("block hash for height %d not found in "+
			"cache", h)
	}

	return &hash, nil
}
