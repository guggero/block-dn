package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// touch creates an empty file at the given path. Used to populate test
// directories without depending on the real producer's serializer.
func touch(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestListFiles_EmptyAndSorted(t *testing.T) {
	dir := t.TempDir()

	// Empty directory.
	files, err := listFiles(dir, HeaderFileSuffix)
	require.NoError(t, err)
	require.Empty(t, files)

	// Create three header files out of natural order; listFiles must
	// return them sorted (the lastFile lookup depends on this).
	touch(t, filepath.Join(dir, "block-0000200-0000299.header"))
	touch(t, filepath.Join(dir, "block-0000000-0000099.header"))
	touch(t, filepath.Join(dir, "block-0000100-0000199.header"))

	// Also drop a file with a different suffix and a junk file that
	// happens to share the suffix prefix — neither should match.
	touch(t, filepath.Join(dir, "block-0000000-0000099.cfilter"))
	touch(t, filepath.Join(dir, "not-a-header-file.header"))

	files, err = listFiles(dir, HeaderFileSuffix)
	require.NoError(t, err)
	require.Len(t, files, 4) // three real + one junk that happens to end in .header
	require.Contains(t, files[0], "block-0000000-0000099.header")
	require.Contains(t, files[1], "block-0000100-0000199.header")
	require.Contains(t, files[2], "block-0000200-0000299.header")
	// The junk file sorts last (lexicographic on full path), so it's at
	// the tail. lastFile guards against this junk via the regex.
}

func TestLastFile_Empty(t *testing.T) {
	dir := t.TempDir()

	last, err := lastFile(
		dir, HeaderFileSuffix, headerFileNameExtractRegex,
	)
	require.NoError(t, err)
	require.EqualValues(t, 0, last,
		"empty directory should return 0 (sentinel for 'no files')")
}

func TestLastFile_HappyPath(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "block-0000000-0000099.header"))
	touch(t, filepath.Join(dir, "block-0000100-0000199.header"))
	touch(t, filepath.Join(dir, "block-0000200-0000299.header"))

	last, err := lastFile(
		dir, HeaderFileSuffix, headerFileNameExtractRegex,
	)
	require.NoError(t, err)
	require.EqualValues(t, 299, last)
}

func TestLastFile_MismatchedRegexReturnsError(t *testing.T) {
	dir := t.TempDir()

	// A file with the right suffix but a name the extract regex won't
	// match. lastFile must surface this loudly rather than silently
	// returning zero, because a real producer hitting this on startup
	// would otherwise re-mine the entire chain over an unparseable file.
	touch(t, filepath.Join(dir, "garbage.header"))

	_, err := lastFile(
		dir, HeaderFileSuffix, headerFileNameExtractRegex,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "error extracting number")
}

func TestLastFile_DifferentSuffixes(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "block-0000000-0001999.cfilter"))
	touch(t, filepath.Join(dir, "block-0002000-0003999.cfilter"))
	// A header file in the same directory must NOT confuse the cfilter
	// lookup, even though the prefix matches.
	touch(t, filepath.Join(dir, "block-0000000-0009999.header"))

	last, err := lastFile(
		dir, FilterFileSuffix, filterFileNameExtractRegex,
	)
	require.NoError(t, err)
	require.EqualValues(t, 3999, last)
}
