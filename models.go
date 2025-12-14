package main

import "fmt"

type Status struct {
	ChainGenesisHash      string `json:"chain_genesis_hash"`
	ChainName             string `json:"chain_name"`
	BestBlockHeight       int32  `json:"best_block_height"`
	BestBlockHash         string `json:"best_block_hash"`
	BestFilterHeader      string `json:"best_filter_header"`
	BestFilterHeight      int32  `json:"best_filter_height"`
	BestSPTweakHeight     int32  `json:"best_sptweak_height"`
	AllFilesSynced        bool   `json:"all_files_synced"`
	EntriesPerHeaderFile  int32  `json:"entries_per_header_file"`
	EntriesPerFilterFile  int32  `json:"entries_per_filter_file"`
	EntriesPerSPTweakFile int32  `json:"entries_per_sptweak_file"`
}

type SPTweakBlock map[int32]string

type SPTweakFile struct {
	StartHeight int32          `json:"start_height"`
	NumBlocks   int32          `json:"num_blocks"`
	Blocks      []SPTweakBlock `json:"blocks"`
}

func (f *SPTweakFile) TweakAtHeight(height int32) (SPTweakBlock, error) {
	var empty SPTweakBlock
	if height < 0 {
		return empty, fmt.Errorf("height must be non-negative")
	}

	if height < f.StartHeight {
		return empty, fmt.Errorf("height %d out of range (%d to %d)",
			height, f.StartHeight, f.StartHeight+f.NumBlocks-1)
	}

	if height >= f.StartHeight+f.NumBlocks {
		return empty, fmt.Errorf("height %d out of range (%d to %d)",
			height, f.StartHeight, f.StartHeight+f.NumBlocks-1)
	}

	if int(f.NumBlocks) != len(f.Blocks) {
		return empty, fmt.Errorf("internal error: NumBlocks %d does "+
			"not match length of Blocks slice %d", f.NumBlocks,
			len(f.Blocks))
	}

	blockIndex := height - f.StartHeight
	return f.Blocks[blockIndex], nil
}
