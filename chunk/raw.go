package chunk

import (
	"bytes"
	"fmt"
)

// Raw access for storage formats outside this package, such as the Helix
// world format: block storages in the packed form BDS and the client use,
// read and built without NBT.

// Raw returns the storage's bits per index and its packed indices, laid out as
// BDS and the client store them. The words belong to the storage: never change
// them.
func (storage *PalettedStorage) Raw() (bitsPerIndex int, words []uint32) {
	return int(storage.bitsPerIndex), storage.indices
}

// WordCount returns how many packed words a storage of bitsPerIndex holds, or
// -1 if no storage has that many bits per index.
func WordCount(bitsPerIndex int) int {
	if bitsPerIndex < 0 || bitsPerIndex > 16 || !paletteSize(bitsPerIndex).valid() {
		return -1
	}
	return paletteSize(bitsPerIndex).uint32s()
}

// NewRawStorage returns a storage of bitsPerIndex whose packed indices are
// words and whose palette holds values; it keeps both slices. It checks that
// words has the right length and that every index points into the palette.
func NewRawStorage(bitsPerIndex int, words, values []uint32) (*PalettedStorage, error) {
	n := WordCount(bitsPerIndex)
	switch {
	case n < 0:
		return nil, fmt.Errorf("invalid storage of %d bits per index", bitsPerIndex)
	case len(words) != n:
		return nil, fmt.Errorf("storage of %d bits per index needs %d words, got %d", bitsPerIndex, n, len(words))
	case len(values) == 0 || len(values) > maxPaletteEntries:
		return nil, fmt.Errorf("invalid palette of %d entries", len(values))
	}
	if bitsPerIndex > 0 && len(values) < 1<<bitsPerIndex && !indicesBelow(bitsPerIndex, words, uint32(len(values))) { //nolint:gosec // at most 4096
		return nil, fmt.Errorf("palette index out of range (palette has %d entries)", len(values))
	}
	return newPalettedStorage(words, newPalette(paletteSize(bitsPerIndex), values)), nil
}

// indicesBelow reports whether every index packed in words is below n. Each
// word holds 32/bits indices from its low bits up; the bits above them, in
// the padded sizes, are unused.
func indicesBelow(bits int, words []uint32, n uint32) bool {
	per := 32 / bits
	mask := uint32(1)<<bits - 1
	for _, w := range words {
		for range per {
			if w&mask >= n {
				return false
			}
			w >>= bits
		}
	}
	return true
}

// NewSubChunkWith returns a sub chunk holding layers, which it keeps.
func NewSubChunkWith(air uint32, layers []*PalettedStorage) *SubChunk {
	return &SubChunk{air: air, storages: layers}
}

// SetSubChunk puts sub at index, as when building a chunk from a stored form.
// It reports false if index is outside the chunk.
func (chunk *Chunk) SetSubChunk(index int, sub *SubChunk) bool {
	if index < 0 || index >= len(chunk.sub) || sub == nil {
		return false
	}
	chunk.sub[index] = sub
	if chunk.shared != nil {
		chunk.shared[index] &^= sharedSub
	}
	chunk.recalculateHeightMap = true
	return true
}

// DecodeDiskBiomes reads biome storages as BDS stores them after the height
// map into c, replacing its biomes.
func DecodeDiskBiomes(c *Chunk, data []byte) error {
	return decodeBiomes(bytes.NewBuffer(data), c, DiskEncoding)
}

// DecodeDiskSubChunk decodes one sub chunk as BDS stores it, with block
// palettes in NBT, for chunk c. index is the sub chunk's index in c, which a
// version 9 sub chunk replaces with the one it holds. It returns the index
// and the sub chunk.
func DecodeDiskSubChunk(c *Chunk, up *Upgrader, data []byte, index int) (int, *SubChunk, error) {
	if index < 0 || index >= len(c.sub) {
		return 0, nil, fmt.Errorf("invalid sub-chunk index %d: chunk range has %d sub-chunks", index, len(c.sub))
	}
	i := uint8(index) //nolint:gosec // checked above
	sub, err := decodeSubChunk(bytes.NewBuffer(data), c, &i, DiskEncoding, up)
	if err != nil {
		return 0, nil, err
	}
	if int(i) >= len(c.sub) {
		return 0, nil, fmt.Errorf("invalid sub-chunk index %d: chunk range has %d sub-chunks", i, len(c.sub))
	}
	return int(i), sub, nil
}
