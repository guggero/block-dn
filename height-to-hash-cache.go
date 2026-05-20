package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire/v2"
)

// heightToHashCache is an in-memory cache of height to block hash.
type heightToHashCache struct {
	sync.RWMutex

	client       *rpcclient.Client
	heightToHash map[int32]chainhash.Hash
	bestHeight   atomic.Int32
}

func newH2HCache(client *rpcclient.Client) *heightToHashCache {
	return &heightToHashCache{
		client: client,
		heightToHash: make(
			map[int32]chainhash.Hash, DefaultHeightToHashCacheSize,
		),
	}
}

// loadFromHeaders loads the height to block hash mapping from the header
// files stored in headerDir. It returns the highest height loaded.
func (c *heightToHashCache) loadFromHeaders(headerDir string) (int32, error) {
	c.Lock()
	defer c.Unlock()

	fileNames, err := listFiles(headerDir, HeaderFileSuffix)
	if err != nil {
		return 0, fmt.Errorf("unable to list header files: %w", err)
	}

	var (
		height int32
		header wire.BlockHeader
	)
	for _, fileName := range fileNames {
		log.Debugf("Loading height to hash cache from header file: %s",
			fileName)
		file, err := os.Open(fileName)
		if err != nil {
			return 0, fmt.Errorf("unable to open header file %s: "+
				"%w", fileName, err)
		}

	outer:
		for {
			err := header.Deserialize(file)
			switch err {
			// No error, we read a header successfully.
			case nil:
				c.heightToHash[height] = header.BlockHash()
				height++

			// EOF means we reached the end of the file. Break and
			// continue with the next one.
			// nolint:errorlint
			case io.EOF:
				_ = file.Close()
				break outer

			default:
				_ = file.Close()
				return 0, fmt.Errorf("unable to deserialize "+
					"header at height %d from file %s: %w",
					c.bestHeight.Load(), fileName, err)
			}
		}
	}

	c.bestHeight.Store(height - 1)
	return c.bestHeight.Load(), nil
}

// getBlockHash returns the block hash for the given height. If the hash isn't
// cached yet, it is fetched from the backend and added to the cache.
func (c *heightToHashCache) getBlockHash(h int32) (*chainhash.Hash, error) {
	c.RLock()
	defer c.RUnlock()

	hash, ok := c.heightToHash[h]
	if !ok {
		hash, err := c.client.GetBlockHash(int64(h))
		if err != nil {
			return nil, fmt.Errorf("error fetching block hash "+
				"for height %d from backend: %w", h, err)
		}

		// Cache the fetched hash.
		c.RUnlock()
		c.Lock()
		c.heightToHash[h] = *hash
		if h > c.bestHeight.Load() {
			c.bestHeight.Store(h)
		}
		c.Unlock()
		c.RLock()

		return hash, nil
	}

	return &hash, nil
}

// invalidate drops all cached height→hash entries at or above fromHeight and
// resets bestHeight to fromHeight - 1. It's the cache-side counterpart to a
// reorg rollback: after the producers have pruned their per-height state for
// the affected range, invalidate clears the now-stale hashes so the next
// getBlockHash call hits bitcoind and learns the new chain.
func (c *heightToHashCache) invalidate(fromHeight int32) {
	c.Lock()
	defer c.Unlock()

	for h := fromHeight; h <= c.bestHeight.Load(); h++ {
		delete(c.heightToHash, h)
	}
	c.bestHeight.Store(fromHeight - 1)
}
