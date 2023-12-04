package main

type Status struct {
	ChainGenesisHash string `json:"chain_genesis_hash"`
	ChainName        string `json:"chain_name"`
	BestBlockHeight  int32  `json:"best_block_height"`
	BestBlockHash    string `json:"best_block_hash"`
	BestFilterHeader string `json:"best_filter_header"`
	EntriesPerHeader int32  `json:"entries_per_header"`
	EntriesPerFilter int32  `json:"entries_per_filter"`
}
