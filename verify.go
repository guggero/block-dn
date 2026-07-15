package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/rpcclient"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/spf13/cobra"
)

// newVerifyCommand creates the "verify" subcommand: an offline integrity
// check of all sealed files against each other and the backend chain, with
// an optional --fix that regenerates corrupt files in place. This exists
// because sealed files are advertised as immutable (year-long CDN caching),
// so any historical corruption — e.g. a filter ingested from a reorged-away
// block by a version that predates the seal-time verification — persists
// until the operator repairs it.
func newVerifyCommand(cc *mainCommand) *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use: "verify",
		Short: "Verify all sealed header and filter files against " +
			"the backend chain, optionally fixing corrupt files",
		Long: "Checks every sealed block-header file (chain linkage, " +
			"genesis, backend anchors) and every sealed compact-" +
			"filter file (BIP157 commitment chain closure " +
			"against " +
			"the filter-header files). Requires the same " +
			"--base-dir, network and bitcoind flags as the " +
			"server. SP tweak data files are not covered. After " +
			"--fix, purge the CDN cache for the repaired URLs.",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runVerify(cc, fix)
		},
	}
	cmd.Flags().BoolVar(
		&fix, "fix", false, "Regenerate corrupt files in place "+
			"from the backend",
	)

	return cmd
}

// verifier holds the state of one verification run.
type verifier struct {
	chain          *rpcclient.Client
	params         *chaincfg.Params
	baseDir        string
	headersPerFile int32
	filtersPerFile int32
	fix            bool

	badFiles   []string
	fixedFiles []string

	// fHeaderCache is the most recently loaded filter-header file, so
	// per-range lookups don't re-read the same file over and over.
	fHeaderCacheStart int32
	fHeaderCache      []byte
}

func runVerify(cc *mainCommand, fix bool) error {
	chainParams, headersPerFile, filtersPerFile, _ := cc.resolveNetwork()
	if cc.baseDir == "" {
		return fmt.Errorf("--base-dir must be set")
	}

	client, err := rpcclient.New(cc.bitcoindConfig, nil)
	if err != nil {
		return fmt.Errorf("error connecting to bitcoind: %w", err)
	}
	defer client.Shutdown()

	v := &verifier{
		chain:             client,
		params:            chainParams,
		baseDir:           cc.baseDir,
		headersPerFile:    headersPerFile,
		filtersPerFile:    filtersPerFile,
		fix:               fix,
		fHeaderCacheStart: -1,
	}

	if err := v.verifyHeaderFiles(); err != nil {
		return err
	}
	if err := v.verifyFilterFiles(); err != nil {
		return err
	}

	if len(v.fixedFiles) > 0 {
		fmt.Printf("fixed %d file(s):\n", len(v.fixedFiles))
		for _, f := range v.fixedFiles {
			fmt.Printf("  %s\n", f)
		}
		fmt.Println("remember to purge the CDN cache for the " +
			"corresponding URLs")
	}
	if len(v.badFiles) > 0 {
		return fmt.Errorf("%d corrupt file(s) found: %v",
			len(v.badFiles), v.badFiles)
	}

	fmt.Println("all sealed files verified OK")

	return nil
}

// verifyHeaderFiles walks all sealed block-header files in order, verifying
// the prev-hash linkage within and across files, the genesis block, and one
// backend anchor per file end. The paired filter-header files are checked
// for size and end anchor here; their interior is covered by the filter
// chain closure in verifyFilterFiles.
func (v *verifier) verifyHeaderFiles() error {
	headerDir := filepath.Join(v.baseDir, HeaderFileDir)

	var running chainhash.Hash
	for start := int32(0); ; start += v.headersPerFile {
		end := start + v.headersPerFile - 1
		name := fmt.Sprintf(
			HeaderFileNamePattern, headerDir, start, end,
		)
		if !fileExists(name) {
			return nil
		}

		err := v.verifyOneHeaderFile(name, start, end, &running)
		if err != nil {
			return err
		}

		fhName := fmt.Sprintf(
			FilterHeaderFileNamePattern, headerDir, start, end,
		)
		if err := v.verifyFilterHeaderAnchor(
			fhName, start, end,
		); err != nil {
			return err
		}
	}
}

// verifyOneHeaderFile checks a single sealed block-header file and updates
// running to its last block hash. On corruption it records (and with --fix
// regenerates) the file.
func (v *verifier) verifyOneHeaderFile(name string, start, end int32,
	running *chainhash.Hash) error {

	verify := func() error {
		data, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("error reading: %w", err)
		}
		want := int(v.headersPerFile) * wire.MaxBlockHeaderPayload
		if len(data) != want {
			return fmt.Errorf("size %d, want %d", len(data), want)
		}

		prev := *running
		for height := start; height <= end; height++ {
			offset := int(height-start) * wire.MaxBlockHeaderPayload
			record := data[offset : offset+wire.MaxBlockHeaderPayload]

			hash := chainhash.DoubleHashH(record)
			if height == 0 {
				if hash != *v.params.GenesisHash {
					return fmt.Errorf("wrong genesis " +
						"block")
				}
			} else if !bytes.Equal(record[4:36], prev[:]) {
				return fmt.Errorf("header at height %d does "+
					"not link to its predecessor", height)
			}
			prev = hash
		}

		anchor, err := v.chain.GetBlockHash(int64(end))
		if err != nil {
			return fmt.Errorf("error getting anchor: %w", err)
		}
		if prev != *anchor {
			return fmt.Errorf("last header (%s) does not match "+
				"the backend's block at height %d (%s)", prev,
				end, anchor)
		}

		*running = prev

		return nil
	}

	err := verify()
	if err == nil {
		fmt.Printf("headers %d-%d: OK\n", start, end)
		return nil
	}

	fmt.Printf("headers %d-%d: BAD (%v)\n", start, end, err)
	if !v.fix {
		v.badFiles = append(v.badFiles, name)
		return nil
	}

	err = v.regenerate(name, start, end, v.writeHeaderRecord, "headers")
	if err != nil {
		return err
	}

	if err := verify(); err != nil {
		return fmt.Errorf("%s still corrupt after fix: %w", name, err)
	}
	fmt.Printf("headers %d-%d: fixed\n", start, end)

	return nil
}

// verifyFilterHeaderAnchor checks a sealed filter-header file's size and its
// last entry against the backend. Interior entries are covered by the
// filter chain closure in verifyFilterFiles.
func (v *verifier) verifyFilterHeaderAnchor(name string, start,
	end int32) error {

	verify := func() error {
		data, err := os.ReadFile(name)
		if err != nil {
			return fmt.Errorf("error reading: %w", err)
		}
		want := int(v.headersPerFile) * chainhash.HashSize
		if len(data) != want {
			return fmt.Errorf("size %d, want %d", len(data), want)
		}

		authority, err := v.authFilterHeader(end)
		if err != nil {
			return err
		}
		stored := data[len(data)-chainhash.HashSize:]
		if !bytes.Equal(stored, authority[:]) {
			return fmt.Errorf("last filter header does not " +
				"match the backend")
		}

		return nil
	}

	err := verify()
	if err == nil {
		fmt.Printf("filter headers %d-%d: OK\n", start, end)
		return nil
	}

	fmt.Printf("filter headers %d-%d: BAD (%v)\n", start, end, err)
	if !v.fix {
		v.badFiles = append(v.badFiles, name)
		return nil
	}

	err = v.regenerate(
		name, start, end, v.writeFilterHeaderRecord, "filter headers",
	)
	if err != nil {
		return err
	}

	v.fHeaderCacheStart = -1
	if err := verify(); err != nil {
		return fmt.Errorf("%s still corrupt after fix: %w", name, err)
	}
	fmt.Printf("filter headers %d-%d: fixed\n", start, end)

	return nil
}

// verifyFilterFiles walks all sealed compact-filter files and verifies each
// one exactly like a light client does: the BIP157 commitment chain over
// the file's filters, anchored at the stored filter headers on both ends,
// must close. A failing range is diagnosed per height against the backend
// to identify (and with --fix, regenerate) the culprit file(s).
func (v *verifier) verifyFilterFiles() error {
	filterDir := filepath.Join(v.baseDir, FilterFileDir)

	for start := int32(0); ; start += v.filtersPerFile {
		end := start + v.filtersPerFile - 1
		name := fmt.Sprintf(
			FilterFileNamePattern, filterDir, start, end,
		)
		if !fileExists(name) {
			return nil
		}

		if err := v.verifyOneFilterFile(name, start, end); err != nil {
			return err
		}
	}
}

// verifyOneFilterFile checks the chain closure of one sealed filter file
// against the stored filter headers.
func (v *verifier) verifyOneFilterFile(name string, start, end int32) error {
	filters, err := v.readFilterFile(name)
	if err == nil {
		err = v.checkFilterChain(filters, start, end)
	}
	if err == nil {
		fmt.Printf("filters %d-%d: OK\n", start, end)
		return nil
	}

	fmt.Printf("filters %d-%d: BAD (%v)\n", start, end, err)
	if !v.fix {
		v.badFiles = append(v.badFiles, name)
		return nil
	}

	// Diagnose which store diverges from the backend before rewriting
	// anything: the filters file, the filter-header file, or both.
	badFilters, badFHeaders, err := v.diagnoseRange(filters, start, end)
	if err != nil {
		return err
	}

	if badFilters {
		err := v.regenerate(
			name, start, end, v.writeFilterRecord, "filters",
		)
		if err != nil {
			return err
		}
	}
	if badFHeaders {
		headerDir := filepath.Join(v.baseDir, HeaderFileDir)
		fhStart := start - (start % v.headersPerFile)
		fhName := fmt.Sprintf(
			FilterHeaderFileNamePattern, headerDir, fhStart,
			fhStart+v.headersPerFile-1,
		)
		err := v.regenerate(
			fhName, fhStart, fhStart+v.headersPerFile-1,
			v.writeFilterHeaderRecord, "filter headers",
		)
		if err != nil {
			return err
		}
		v.fHeaderCacheStart = -1
	}

	filters, err = v.readFilterFile(name)
	if err == nil {
		err = v.checkFilterChain(filters, start, end)
	}
	if err != nil {
		return fmt.Errorf("%s still corrupt after fix: %w", name, err)
	}
	fmt.Printf("filters %d-%d: fixed\n", start, end)

	return nil
}

// checkFilterChain recomputes the BIP157 commitment chain over the given
// filters and requires it to run from the stored filter header before the
// range to the stored filter header at its end.
func (v *verifier) checkFilterChain(filters [][]byte, start,
	end int32) error {

	var prev chainhash.Hash
	if start > 0 {
		stored, err := v.storedFilterHeader(start - 1)
		if err != nil {
			return err
		}
		prev = stored
	}
	want, err := v.storedFilterHeader(end)
	if err != nil {
		return err
	}

	for _, filter := range filters {
		filterHash := chainhash.DoubleHashH(filter)
		prev = chainhash.DoubleHashH(
			append(filterHash[:], prev[:]...),
		)
	}
	if prev != want {
		return fmt.Errorf("filter chain does not close against the " +
			"stored filter headers")
	}

	return nil
}

// diagnoseRange compares every height of a failing range against the
// backend and reports whether the filters file and/or the filter-header
// entries diverge.
func (v *verifier) diagnoseRange(filters [][]byte, start, end int32) (bool,
	bool, error) {

	var badFilters, badFHeaders bool
	for height := start; height <= end; height++ {
		hash, err := v.chain.GetBlockHash(int64(height))
		if err != nil {
			return false, false, err
		}
		auth, err := v.chain.GetBlockFilter(*hash, &filterBasic)
		if err != nil {
			return false, false, err
		}

		authFilter, err := hex.DecodeString(auth.Filter)
		if err != nil {
			return false, false, err
		}
		if !bytes.Equal(filters[height-start], authFilter) {
			fmt.Printf("  filter at height %d diverges from "+
				"the backend\n", height)
			badFilters = true
		}

		authHeader, err := chainhash.NewHashFromStr(auth.Header)
		if err != nil {
			return false, false, err
		}
		stored, err := v.storedFilterHeader(height)
		if err != nil {
			return false, false, err
		}
		if stored != *authHeader {
			fmt.Printf("  filter header at height %d diverges "+
				"from the backend\n", height)
			badFHeaders = true
		}
	}

	return badFilters, badFHeaders, nil
}

// storedFilterHeader reads the filter header at the given height from the
// sealed filter-header files, caching the most recently loaded file.
func (v *verifier) storedFilterHeader(height int32) (chainhash.Hash, error) {
	var result chainhash.Hash

	fileStart := height - (height % v.headersPerFile)
	if v.fHeaderCacheStart != fileStart {
		headerDir := filepath.Join(v.baseDir, HeaderFileDir)
		name := fmt.Sprintf(
			FilterHeaderFileNamePattern, headerDir, fileStart,
			fileStart+v.headersPerFile-1,
		)
		data, err := os.ReadFile(name)
		if err != nil {
			return result, fmt.Errorf("error reading filter "+
				"headers for height %d: %w", height, err)
		}
		v.fHeaderCache = data
		v.fHeaderCacheStart = fileStart
	}

	offset := int(height-fileStart) * chainhash.HashSize
	if offset+chainhash.HashSize > len(v.fHeaderCache) {
		return result, fmt.Errorf("filter header file for height %d "+
			"is truncated", height)
	}
	copy(result[:], v.fHeaderCache[offset:])

	return result, nil
}

// readFilterFile parses a sealed var-int prefixed filter file.
func (v *verifier) readFilterFile(name string) ([][]byte, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("error reading: %w", err)
	}

	reader := bytes.NewReader(data)
	filters := make([][]byte, 0, v.filtersPerFile)
	for i := range v.filtersPerFile {
		filter, err := wire.ReadVarBytes(
			reader, 0, wire.MaxBlockPayload, "filter",
		)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		filters = append(filters, filter)
	}
	if reader.Len() != 0 {
		return nil, fmt.Errorf("%d trailing bytes", reader.Len())
	}

	return filters, nil
}

// regenerate rewrites one sealed file from the backend, writing every
// height's record via writeRecord into a temp file that atomically replaces
// the corrupt one.
func (v *verifier) regenerate(name string, start, end int32,
	writeRecord func(w *os.File, height int32) error,
	kind string) error {

	fmt.Printf("  regenerating %s %d-%d from the backend...\n", kind,
		start, end)

	tmp, err := os.CreateTemp(filepath.Dir(name), "verify-fix-*")
	if err != nil {
		return fmt.Errorf("error creating temp file: %w", err)
	}
	defer func() {
		_ = os.Remove(tmp.Name())
	}()

	for height := start; height <= end; height++ {
		if err := writeRecord(tmp, height); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("error writing height %d: %w",
				height, err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("error closing temp file: %w", err)
	}

	if err := os.Rename(tmp.Name(), name); err != nil {
		return fmt.Errorf("error replacing %s: %w", name, err)
	}
	v.fixedFiles = append(v.fixedFiles, name)

	return nil
}

// writeHeaderRecord fetches and serializes one block header.
func (v *verifier) writeHeaderRecord(w *os.File, height int32) error {
	hash, err := v.chain.GetBlockHash(int64(height))
	if err != nil {
		return err
	}
	header, err := v.chain.GetBlockHeader(hash)
	if err != nil {
		return err
	}

	return header.Serialize(w)
}

// writeFilterHeaderRecord fetches and writes one 32-byte filter header.
func (v *verifier) writeFilterHeaderRecord(w *os.File, height int32) error {
	header, err := v.authFilterHeader(height)
	if err != nil {
		return err
	}
	_, err = w.Write(header[:])

	return err
}

// writeFilterRecord fetches and writes one var-int prefixed filter.
func (v *verifier) writeFilterRecord(w *os.File, height int32) error {
	hash, err := v.chain.GetBlockHash(int64(height))
	if err != nil {
		return err
	}
	filter, err := v.chain.GetBlockFilter(*hash, &filterBasic)
	if err != nil {
		return err
	}
	filterBytes, err := hex.DecodeString(filter.Filter)
	if err != nil {
		return err
	}

	return wire.WriteVarBytes(w, 0, filterBytes)
}

// authFilterHeader returns the backend's authoritative filter header at the
// given height.
func (v *verifier) authFilterHeader(height int32) (chainhash.Hash, error) {
	var result chainhash.Hash

	hash, err := v.chain.GetBlockHash(int64(height))
	if err != nil {
		return result, err
	}
	filter, err := v.chain.GetBlockFilter(*hash, &filterBasic)
	if err != nil {
		return result, err
	}
	parsed, err := chainhash.NewHashFromStr(filter.Header)
	if err != nil {
		return result, err
	}

	return *parsed, nil
}
