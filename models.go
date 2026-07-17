package main

type Status struct {
	Version                string `json:"version"`
	Commit                 string `json:"commit"`
	ChainGenesisHash       string `json:"chain_genesis_hash"`
	ChainName              string `json:"chain_name"`
	BestBlockHeight        int32  `json:"best_block_height"`
	BestBlockHash          string `json:"best_block_hash"`
	BestFilterHeader       string `json:"best_filter_header"`
	BestFilterHeight       int32  `json:"best_filter_height"`
	BestSPTweakHeight      int32  `json:"best_sptweak_height"`
	CustomFiltersAvailable bool   `json:"custom_filters_available"`
	BestCustomFilterHeight int32  `json:"best_custom_filter_height"`
	AllFilesSynced         bool   `json:"all_files_synced"`
	EntriesPerHeaderFile   int32  `json:"entries_per_header_file"`
	EntriesPerFilterFile   int32  `json:"entries_per_filter_file"`
	EntriesPerSPTweakFile  int32  `json:"entries_per_sptweak_file"`
}

type FeeRate struct {
	FeeSatPerKVByte  int64 `json:"fee_sat_per_kvbyte"`
	FeeSatPerKWeight int64 `json:"fee_sat_per_kweight"`
	FeeSatPerVByte   int64 `json:"fee_sat_per_vbyte"`
}
