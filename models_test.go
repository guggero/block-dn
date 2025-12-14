package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSPTweakFileTweakAtHeight exercises happy and edge cases for SPTweakFile's
// TweakAtHeight method.
func TestSPTweakFileTweakAtHeight(t *testing.T) {
	tests := []struct {
		name      string
		file      SPTweakFile
		height    int32
		want      SPTweakBlock
		expectErr bool
	}{
		{
			name: "negative height returns error",
			file: SPTweakFile{
				StartHeight: 10,
				NumBlocks:   2,
				Blocks: []SPTweakBlock{
					{10: "a"},
					{11: "b"},
				},
			},
			height:    -1,
			expectErr: true,
		},

		{
			name: "height before start errors",
			file: SPTweakFile{
				StartHeight: 10,
				NumBlocks:   2,
				Blocks: []SPTweakBlock{
					{10: "a"},
					{11: "b"},
				},
			},
			height:    9,
			expectErr: true,
		},

		{
			name: "height equal start returns first block",
			file: SPTweakFile{
				StartHeight: 10,
				NumBlocks:   3,
				Blocks: []SPTweakBlock{
					{10: "aa"},
					{11: "bb"},
					{12: "cc"},
				},
			},
			height: 10,
			want:   SPTweakBlock{10: "aa"},
		},

		{
			name: "middle height returns middle block",
			file: SPTweakFile{
				StartHeight: 10,
				NumBlocks:   3,
				Blocks: []SPTweakBlock{
					{10: "aa"},
					{11: "bb"},
					{12: "cc"},
				},
			},
			height: 11,
			want:   SPTweakBlock{11: "bb"},
		},

		{
			name: "last valid height returns last block",
			file: SPTweakFile{
				StartHeight: 100,
				NumBlocks:   2,
				Blocks: []SPTweakBlock{
					{100: "x"},
					{101: "y"},
				},
			},
			height: 101,
			want:   SPTweakBlock{101: "y"},
		},

		{
			name: "height equal start+num triggers out of range " +
				"error",
			file: SPTweakFile{
				StartHeight: 50,
				NumBlocks:   2,
				Blocks: []SPTweakBlock{
					{50: "m"},
					{51: "n"},
				},
			},
			height:    52,
			expectErr: true,
		},

		{
			name: "blocks shorter than NumBlocks errors",
			file: SPTweakFile{
				StartHeight: 0,
				NumBlocks:   2,
				Blocks: []SPTweakBlock{
					{0: "only"},
				},
			},
			height:    1,
			expectErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.file.TweakAtHeight(tc.height)
			if tc.expectErr {
				require.Error(t, err)

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
