package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
)

// producerHooks bundles the per-producer behavior that producerBase
// delegates to. Each hook is invoked by a corresponding generic step in the
// poll loop / seal loop / reorg-handling code on producerBase.
//
// All hooks except afterCatchUp are mandatory. The producer constructor is
// responsible for installing them.
type producerHooks struct {
	// ingest fetches the block-specific data for the given height and
	// stores it in the producer's in-memory state. The hook owns its
	// locking.
	ingest func(height int32) error

	// writeSealedFile serializes the in-memory data for the inclusive
	// range [fileStart, fileEnd] to disk. Called with no producerBase
	// lock held; the hook should grab its own read lock if it reads
	// shared state.
	writeSealedFile func(fileStart, fileEnd int32) error

	// pruneLocked drops the in-memory entries for the inclusive range
	// [start, end]. Called with producerBase's write lock already held.
	pruneLocked func(start, end int32)

	// cachedHash returns the block hash this producer has recorded for
	// the given in-memory height, or ok=false if the height isn't in
	// memory. Each producer maintains its own height→hash lookup (some
	// derive it from data they already store, others keep a side map),
	// so reorg detection is independent of the shared h2hCache being
	// invalidated by another producer mid-rollback.
	cachedHash func(height int32) (chainhash.Hash, bool)

	// afterCatchUp (optional) is invoked once, after the initial
	// catch-up to the chain tip completes and before the steady-state
	// poll loop begins. Used by spTweakFiles to release the prev-out
	// cache it only needs during the heavy initial scan.
	afterCatchUp func()
}

// producerBase encapsulates the lifecycle shared by every on-disk producer:
// catching up to the chain tip on startup, polling for new blocks, sealing
// files once their last height has dropped past the reorg-safe depth, and
// rolling in-memory state back when bitcoind reorgs. The producer-specific
// bits (what to fetch, how to write it, which maps to prune, where the
// per-producer hash lookup comes from) are supplied via producerHooks.
//
// Concrete producers embed this struct, supply hooks, and add their own
// in-memory data structures.
type producerBase struct {
	sync.RWMutex

	quit        <-chan struct{}
	chain       *rpcclient.Client
	h2hCache    *heightToHashCache
	chainParams *chaincfg.Params

	// name appears in every log message emitted by the base so the
	// three concurrent producers can be told apart in the log.
	name string

	baseDir      string
	subDir       string
	fileSuffix   string
	extractRegex *regexp.Regexp

	entriesPerFile int32
	reOrgSafeDepth int32

	startupComplete  atomic.Bool
	currentHeight    atomic.Int32
	lastSealedHeight atomic.Int32

	hooks producerHooks
}

func (p *producerBase) isStartupComplete() bool {
	return p.startupComplete.Load()
}

func (p *producerBase) getCurrentHeight() int32 {
	return p.currentHeight.Load()
}

func (p *producerBase) getLastSealedHeight() int32 {
	return p.lastSealedHeight.Load()
}

// updateFiles brings the on-disk state into sync with bitcoind, then enters
// the steady-state poll loop. Must be called as a goroutine.
//
// NOTE: Concrete producers that need to gate the lifecycle on a runtime
// precondition (e.g., spTweakFiles refusing to run when Taproot isn't
// supported on the active network) can shadow this method on the embedding
// type; the shadowed method then chooses whether to delegate.
func (p *producerBase) updateFiles(numBlocks int32) error {
	log.Debugf("Updating %s files in %s for network %s", p.name, p.baseDir,
		p.chainParams.Name)

	fileDir := filepath.Join(p.baseDir, p.subDir)
	if err := os.MkdirAll(fileDir, DirectoryMode); err != nil {
		return fmt.Errorf("error creating directory %s: %w", fileDir,
			err)
	}

	lastBlock, err := lastFile(fileDir, p.fileSuffix, p.extractRegex)
	if err != nil {
		return fmt.Errorf("error getting last %s file: %w", p.name, err)
	}

	if lastBlock > 0 {
		p.lastSealedHeight.Store(lastBlock)
		p.currentHeight.Store(lastBlock)
	} else {
		p.lastSealedHeight.Store(-1)
	}

	startBlock := p.lastSealedHeight.Load() + 1

	log.Debugf("Writing %s files from block %d to block %d", p.name,
		startBlock, numBlocks)
	if err := p.updateCacheAndFiles(startBlock, numBlocks); err != nil {
		return fmt.Errorf("error updating %s blocks: %w", p.name, err)
	}

	// Allow serving requests now that we're caught up.
	p.startupComplete.Store(true)

	if p.hooks.afterCatchUp != nil {
		p.hooks.afterCatchUp()
	}

	log.Debugf("Caught up %s to best block %d, starting to poll for new "+
		"blocks", p.name, numBlocks)
	for {
		select {
		case <-time.After(blockPollInterval):
		case <-p.quit:
			return errServerShutdown
		}

		height, err := p.chain.GetBlockCount()
		if err != nil {
			return fmt.Errorf("error getting best block: %w", err)
		}

		currentBlock := p.currentHeight.Load()
		if int32(height) == currentBlock && !p.tipReorged() {
			continue
		}

		if err := p.handleReorg(); err != nil {
			return err
		}

		currentBlock = p.currentHeight.Load()
		if int32(height) <= currentBlock {
			// The reorg rollback already covered everything we
			// would have advanced to; pick up on the next tick.
			continue
		}

		log.Infof("Processing %s for new block mined at height %d",
			p.name, height)
		err = p.updateCacheAndFiles(currentBlock+1, int32(height))
		if err != nil {
			return fmt.Errorf("error updating %s for blocks: %w",
				p.name, err)
		}
	}
}

// updateCacheAndFiles ingests every block in [startBlock, endBlock] via the
// producer's ingest hook, then seals any files that are now reorg-safe.
func (p *producerBase) updateCacheAndFiles(startBlock, endBlock int32) error {
	for i := startBlock; i <= endBlock; i++ {
		select {
		case <-p.quit:
			return errServerShutdown
		default:
		}

		if err := p.hooks.ingest(i); err != nil {
			return err
		}

		p.currentHeight.Store(i)
	}
	return p.sealReadyFiles()
}

// sealReadyFiles writes any file whose last height has fallen at least
// reOrgSafeDepth blocks below the current tip and prunes the corresponding
// in-memory entries. Files are sealed in order; the loop exits once the
// next boundary is too close to the tip.
func (p *producerBase) sealReadyFiles() error {
	for {
		safeTip := p.currentHeight.Load() - p.reOrgSafeDepth
		lastSealed := p.lastSealedHeight.Load()
		nextEnd := lastSealed + p.entriesPerFile
		if lastSealed < 0 {
			nextEnd = p.entriesPerFile - 1
		}
		if nextEnd > safeTip {
			return nil
		}

		fileStart := nextEnd - p.entriesPerFile + 1

		log.Debugf("Sealing %s file (heights %d-%d, tip %d)", p.name,
			fileStart, nextEnd, p.currentHeight.Load())

		err := p.hooks.writeSealedFile(fileStart, nextEnd)
		if err != nil {
			return fmt.Errorf("error sealing %s file: %w", p.name,
				err)
		}

		// Both files are now durably on disk. Promote
		// lastSealedHeight and prune the now-redundant in-memory
		// entries under the write lock so concurrent readers see a
		// consistent view.
		p.Lock()
		p.hooks.pruneLocked(fileStart, nextEnd)
		p.lastSealedHeight.Store(nextEnd)
		p.Unlock()
	}
}

// tipReorged is the cheap check used by the poll loop to short-circuit
// when the backend's tip height hasn't moved but a same-height reorg has
// landed.
func (p *producerBase) tipReorged() bool {
	tip := p.currentHeight.Load()
	if tip < 0 {
		return false
	}

	ourHash, ok := p.hooks.cachedHash(tip)
	if !ok {
		// Our own state doesn't have the tip cached — shouldn't
		// happen in steady state, but if it does we'd rather let
		// the next tick try again than spuriously trigger a reorg.
		return false
	}

	btcHash, err := p.chain.GetBlockHash(int64(tip))
	if err != nil {
		// Network blip; let the next tick try again.
		return false
	}

	return !ourHash.IsEqual(btcHash)
}

// handleReorg verifies that the current tip is still on bitcoind's
// canonical chain. If not, it walks back to the common ancestor and rolls
// back in-memory state plus the shared h2hCache for everything above.
// Returns errDeepReorg if the ancestor lies at or below a sealed file
// boundary — that case would require rewriting an "immutable" file, so we
// refuse and ask the server to shut down.
func (p *producerBase) handleReorg() error {
	tip := p.currentHeight.Load()
	if tip < 0 {
		return nil
	}

	ourHash, ok := p.hooks.cachedHash(tip)
	if !ok {
		return nil
	}

	btcHash, err := p.chain.GetBlockHash(int64(tip))
	if err != nil {
		return fmt.Errorf("error getting tip block hash: %w", err)
	}
	if ourHash.IsEqual(btcHash) {
		return nil
	}

	log.Warnf("%s producer detected reorg at tip %d (ours %s, backend "+
		"%s); walking back to find common ancestor", p.name, tip,
		ourHash, btcHash)

	commonHeight, err := p.findCommonAncestor(tip)
	if err != nil {
		return fmt.Errorf("error finding common ancestor: %w", err)
	}
	if commonHeight <= p.lastSealedHeight.Load() {
		return fmt.Errorf("%s reorg back to height %d (last sealed "+
			"%d): %w", p.name, commonHeight,
			p.lastSealedHeight.Load(), errDeepReorg)
	}

	log.Infof("Rolling back %s in-memory state to height %d due to reorg",
		p.name, commonHeight)
	p.rollback(commonHeight)
	return nil
}

// findCommonAncestor walks back from tip, comparing each producer-local
// hash to the hash bitcoind currently reports at that height, returning
// the highest height where they still agree. It stops once it crosses
// lastSealedHeight; the caller rejects that case as a deep reorg.
func (p *producerBase) findCommonAncestor(tip int32) (int32, error) {
	for hgt := tip; hgt > p.lastSealedHeight.Load(); hgt-- {
		ourHash, ok := p.hooks.cachedHash(hgt)
		if !ok {
			// We don't have this height cached anymore — keep
			// walking back. The loop's lastSealedHeight bound
			// guarantees termination.
			continue
		}
		btcHash, err := p.chain.GetBlockHash(int64(hgt))
		if err != nil {
			return 0, fmt.Errorf("error fetching backend hash at "+
				"height %d: %w", hgt, err)
		}
		if ourHash.IsEqual(btcHash) {
			return hgt, nil
		}
	}
	return p.lastSealedHeight.Load(), nil
}

// rollback prunes the producer's in-memory state above toHeight and
// invalidates the shared h2hCache for the same range so subsequent
// getBlockHash calls re-learn the new chain. currentHeight is reset to
// toHeight.
func (p *producerBase) rollback(toHeight int32) {
	p.Lock()
	p.hooks.pruneLocked(toHeight+1, p.currentHeight.Load())
	p.currentHeight.Store(toHeight)
	p.Unlock()

	p.h2hCache.invalidate(toHeight + 1)
}
