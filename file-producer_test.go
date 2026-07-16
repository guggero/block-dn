package main

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/stretchr/testify/require"
)

// sealCall records one invocation of writeSealedFile so the seal tests can
// assert the order, ranges, and the corresponding prune.
type sealCall struct {
	fileStart int32
	fileEnd   int32
	pruned    bool
}

// newTestProducer builds a minimal producerBase suitable for exercising the
// pure-logic helpers (sealReadyFiles, rollback) without touching bitcoind
// or the filesystem. The returned slice pointer is mutated by the recorded
// hooks.
//
// reOrgSafeDepth is — tests at different per-file sizes should be a
// drop-in change, not a structural one.
//
//nolint:unparam // entriesPerFile is parameterized for the same reason
func newTestProducer(entriesPerFile,
	reOrgSafeDepth int32) (*producerBase, *[]sealCall) {

	calls := &[]sealCall{}

	p := &producerBase{
		name:           "test",
		entriesPerFile: entriesPerFile,
		reOrgSafeDepth: reOrgSafeDepth,
		// Most tests don't need h2hCache; tests that call rollback
		// install one explicitly.
	}
	p.lastSealedHeight.Store(-1)

	p.hooks.writeSealedFile = func(s, e int32) error {
		*calls = append(*calls, sealCall{fileStart: s, fileEnd: e})
		return nil
	}
	p.hooks.pruneLocked = func(s, e int32) {
		// We pair each prune with the most recent seal. The base
		// always invokes them in lockstep (seal first, then prune
		// under the write lock), so the indexing is unambiguous.
		if len(*calls) == 0 {
			return
		}
		last := &(*calls)[len(*calls)-1]
		last.pruned = true
		// Sanity-check the range matches the seal that just ran.
		if last.fileStart != s || last.fileEnd != e {
			panic("prune range doesn't match preceding seal")
		}
	}

	return p, calls
}

func TestSealReadyFiles_NothingToSeal(t *testing.T) {
	// Tip well below the first boundary; nothing should seal.
	p, calls := newTestProducer(100, 6)
	p.currentHeight.Store(50)

	require.NoError(t, p.sealReadyFiles())
	require.Empty(t, *calls)
	require.EqualValues(t, -1, p.lastSealedHeight.Load())
}

func TestSealReadyFiles_TipAtThreshold(t *testing.T) {
	// One block below the safe-depth threshold for the first boundary;
	// must not seal yet.
	p, calls := newTestProducer(100, 6)
	p.currentHeight.Store(104) // safeTip=98, boundary=99 → no seal

	require.NoError(t, p.sealReadyFiles())
	require.Empty(t, *calls)
	require.EqualValues(t, -1, p.lastSealedHeight.Load())
}

func TestSealReadyFiles_FirstFileFromColdStart(t *testing.T) {
	// Exactly at the safe-depth threshold for the first boundary; must
	// seal the cold-start file [0..99].
	p, calls := newTestProducer(100, 6)
	p.currentHeight.Store(105) // safeTip=99, boundary=99 → seal

	require.NoError(t, p.sealReadyFiles())
	require.Len(t, *calls, 1)
	require.Equal(t, sealCall{0, 99, true}, (*calls)[0])
	require.EqualValues(t, 99, p.lastSealedHeight.Load())
}

func TestSealReadyFiles_MultipleFilesInOneCall(t *testing.T) {
	// Far past the safe-depth threshold for the first three boundaries:
	// the loop must seal all three in order in a single call.
	p, calls := newTestProducer(100, 6)
	p.currentHeight.Store(305) // safeTip=299, boundaries 99/199/299 ≤ 299

	require.NoError(t, p.sealReadyFiles())
	require.Len(t, *calls, 3)
	require.Equal(t, sealCall{0, 99, true}, (*calls)[0])
	require.Equal(t, sealCall{100, 199, true}, (*calls)[1])
	require.Equal(t, sealCall{200, 299, true}, (*calls)[2])
	require.EqualValues(t, 299, p.lastSealedHeight.Load())
}

func TestSealReadyFiles_ContinueFromAlreadySealed(t *testing.T) {
	// The cold-start branch (lastSealed < 0 → nextEnd = entriesPerFile-1)
	// must not fire when we restart with files already on disk; instead
	// nextEnd should advance one file at a time from lastSealedHeight.
	p, calls := newTestProducer(100, 6)
	p.lastSealedHeight.Store(99)

	p.currentHeight.Store(199) // safeTip=193, next boundary 199 > 193
	require.NoError(t, p.sealReadyFiles())
	require.Empty(t, *calls)

	p.currentHeight.Store(205) // safeTip=199, next boundary 199 ≤ 199
	require.NoError(t, p.sealReadyFiles())
	require.Len(t, *calls, 1)
	require.Equal(t, sealCall{100, 199, true}, (*calls)[0])
	require.EqualValues(t, 199, p.lastSealedHeight.Load())
}

func TestSealReadyFiles_ZeroSafeDepth(t *testing.T) {
	// reOrgSafeDepth=0 reproduces the pre-reorg-safe behavior: files
	// seal the instant their last height is reached.
	p, calls := newTestProducer(100, 0)
	p.currentHeight.Store(99) // safeTip=99, boundary=99 → seal immediately

	require.NoError(t, p.sealReadyFiles())
	require.Len(t, *calls, 1)
	require.Equal(t, sealCall{0, 99, true}, (*calls)[0])
}

func TestSealReadyFiles_WriteErrorHaltsAndDoesNotAdvance(t *testing.T) {
	// If writeSealedFile errors, lastSealedHeight must NOT advance and
	// pruneLocked must NOT run: the next attempt has to retry the same
	// range from in-memory state. This is the load-bearing invariant
	// for crash recovery.
	p, calls := newTestProducer(100, 6)
	bang := errors.New("disk full")
	p.hooks.writeSealedFile = func(s, e int32) error {
		*calls = append(*calls, sealCall{fileStart: s, fileEnd: e})
		return bang
	}

	p.currentHeight.Store(105)
	err := p.sealReadyFiles()
	require.ErrorIs(t, err, bang)
	require.Len(t, *calls, 1)
	require.False(t, (*calls)[0].pruned,
		"prune must not run after a failed write")
	require.EqualValues(t, -1, p.lastSealedHeight.Load(),
		"lastSealedHeight must not advance after a failed write")
}

func TestSealReadyFiles_SecondFileWriteErrorPreservesFirstAdvance(t *testing.T) {
	// If the first file seals fine but the second fails, the first
	// must remain promoted; only the second's lastSealedHeight bump is
	// reverted (i.e., never happens).
	p, calls := newTestProducer(100, 6)
	p.hooks.writeSealedFile = func(s, e int32) error {
		*calls = append(*calls, sealCall{fileStart: s, fileEnd: e})
		if s == 100 {
			return errors.New("transient")
		}
		return nil
	}

	p.currentHeight.Store(205) // boundaries 99 AND 199 both eligible
	err := p.sealReadyFiles()
	require.Error(t, err)
	require.Len(t, *calls, 2)
	require.True(t, (*calls)[0].pruned,
		"first file should have been pruned before the second tried")
	require.False(t, (*calls)[1].pruned,
		"second file's prune must not have run")
	require.EqualValues(t, 99, p.lastSealedHeight.Load(),
		"lastSealedHeight should reflect the one successful seal")
}

func TestUpdateCacheAndFiles_SealsDuringCatchUp(t *testing.T) {
	// Regression test: a long catch-up must seal each file as soon as its
	// range has dropped reorg-safe-depth below the ingest position, not
	// in a single burst once the entire range has been ingested. The
	// latter would hold the whole range in memory and lose all catch-up
	// progress on restart, since resumption starts after the last file
	// on disk.
	p, _ := newTestProducer(100, 6)

	// Record the ingest position at which every seal happens.
	type sealEvent struct {
		fileStart int32
		fileEnd   int32
		atHeight  int32
	}
	var seals []sealEvent
	p.hooks.ingest = func(int32) error {
		return nil
	}
	p.hooks.writeSealedFile = func(s, e int32) error {
		seals = append(seals, sealEvent{
			fileStart: s,
			fileEnd:   e,
			atHeight:  p.currentHeight.Load(),
		})
		return nil
	}

	require.NoError(t, p.updateCacheAndFiles(0, 305))

	// Every file must have been sealed at the earliest height at which
	// its last block became reorg-safe.
	require.Equal(t, []sealEvent{
		{fileStart: 0, fileEnd: 99, atHeight: 105},
		{fileStart: 100, fileEnd: 199, atHeight: 205},
		{fileStart: 200, fileEnd: 299, atHeight: 305},
	}, seals)
	require.EqualValues(t, 299, p.lastSealedHeight.Load())
}

func TestRollback_PrunesRangeAndInvalidatesCache(t *testing.T) {
	// Build a producer with a small populated h2hCache and assert that
	// rollback (a) calls pruneLocked with the right inclusive range,
	// (b) resets currentHeight, and (c) invalidates the shared cache
	// starting at toHeight+1 (so subsequent getBlockHash queries
	// re-learn the new chain).
	p, _ := newTestProducer(100, 6)
	p.h2hCache = newH2HCache(nil)
	for i := range int32(11) {
		p.h2hCache.heightToHash[i] = chainhash.Hash{byte(i)}
	}
	p.h2hCache.bestHeight.Store(10)
	p.currentHeight.Store(10)

	var (
		pruneStart, pruneEnd int32
		pruneCalls           int
	)
	p.hooks.pruneLocked = func(s, e int32) {
		pruneCalls++
		pruneStart, pruneEnd = s, e
	}

	p.rollback(7)

	// (a) prune was called once with the correct range.
	require.Equal(t, 1, pruneCalls)
	require.EqualValues(t, 8, pruneStart)
	require.EqualValues(t, 10, pruneEnd)

	// (b) currentHeight reset.
	require.EqualValues(t, 7, p.currentHeight.Load())

	// (c) Cache invalidated from 8 upward; heights ≤7 still present.
	require.EqualValues(t, 7, p.h2hCache.bestHeight.Load())
	for i := range int32(8) {
		_, ok := p.h2hCache.heightToHash[i]
		require.Truef(t, ok, "expected height %d still cached", i)
	}
	for i := int32(8); i <= 10; i++ {
		_, ok := p.h2hCache.heightToHash[i]
		require.Falsef(t, ok, "expected height %d invalidated", i)
	}
}
