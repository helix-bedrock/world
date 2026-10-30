package mcdb

import (
	"encoding/binary"

	"github.com/helix-bedrock/world/chunk"
)

// heightMapColumns is the number of columns in a chunk's height map.
const heightMapColumns = 256

// appendHeightMap appends the 16x16 height map BDS 1.26.60 stores in front of
// the 3D biomes: per column, one above the highest block that dampens light
// at all (FilteringBlock > 0), counted from the bottom of the dimension, as
// LE int16, z-major (index z*16+x). Plants, vines, fences, stairs and other
// blocks that let light through are skipped; leaves and water count. An
// empty column stores 0.
//
// BDS does not lower a column when blocks below it are carved away during
// generation, so a stored height can sit above the blocks. saved, the height
// map the chunk was loaded with, keeps those values: each column gets the
// higher of saved and computed. Checked against every finished chunk of a
// world BDS saved: unchanged chunks come out byte for byte.
func appendHeightMap(b []byte, c *chunk.Chunk, saved []int16, blocks chunk.BlockRegistry) []byte {
	subs := c.Sub()
	for z := uint8(0); z < 16; z++ {
		for x := uint8(0); x < 16; x++ {
			h := columnHeight(subs, x, z, blocks)
			if i := int(z)<<4 | int(x); i < len(saved) {
				h = max(h, saved[i])
			}
			b = binary.LittleEndian.AppendUint16(b, uint16(h)) //nolint:gosec // raw int16 bits
		}
	}
	return b
}

// decodeHeightMap reads the height map in front of the biomes.
func decodeHeightMap(b []byte) []int16 {
	h := make([]int16, heightMapColumns)
	for i := range h {
		h[i] = int16(binary.LittleEndian.Uint16(b[2*i:])) //nolint:gosec // raw int16 bits
	}
	return h
}

// columnHeight returns the computed height of one column.
func columnHeight(subs []*chunk.SubChunk, x, z uint8, blocks chunk.BlockRegistry) int16 {
	for i := len(subs) - 1; i >= 0; i-- {
		sub := subs[i]
		if sub == nil || sub.Empty() {
			continue
		}
		for y := 15; y >= 0; y-- {
			if blocks.FilteringBlock(sub.Block(x, uint8(y), z, 0)) > 0 { //nolint:gosec // 0..15
				return int16(i<<4 + y + 1) //nolint:gosec // at most the dimension height
			}
		}
	}
	return 0
}
