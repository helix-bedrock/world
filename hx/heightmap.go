package hx

import (
	"encoding/binary"

	"github.com/helix-bedrock/world/chunk"
)

// appendHeightMap appends the 16x16 height map BDS 1.26.60 stores in front of
// the 3D biomes: per column, one above the highest block that dampens light
// at all (FilteringBlock > 0), counted from the bottom of the dimension, as
// LE int16, z-major (index z*16+x). An empty column stores 0.
//
// BDS does not lower a column when blocks below it are carved away during
// generation, so a stored height can sit above the blocks. saved, the Data3D
// the chunk was loaded with, keeps those: each column gets the higher of the
// saved and the computed height. Same rule as mcdb's, checked there against
// a world BDS saved.
func appendHeightMap(b []byte, c *chunk.Chunk, saved []byte, blocks chunk.BlockRegistry) []byte {
	subs := c.Sub()
	for z := range uint8(16) {
		for x := range uint8(16) {
			h := columnHeight(subs, x, z, blocks)
			if i := 2 * (int(z)<<4 | int(x)); i+2 <= len(saved) && i+2 <= heightMapSize {
				h = max(h, int16(binary.LittleEndian.Uint16(saved[i:]))) //nolint:gosec // raw int16 bits
			}
			b = binary.LittleEndian.AppendUint16(b, uint16(h)) //nolint:gosec // raw int16 bits
		}
	}
	return b
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
