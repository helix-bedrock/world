package hx

import (
	"encoding/binary"
	"fmt"

	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/cube"
)

// heightMapSize is the 16x16 int16 height map in front of the biomes in
// Data3D, as BDS stores it.
const heightMapSize = 512

// subChunkVersion is the BDS sub chunk version AppendChunk records.
const subChunkVersion = chunk.SubChunkVersion

// DecodeChunk builds a chunk of range r from a decoded terrain record. Block
// palettes resolve through table; raw sub chunks, kept in BDS's form, decode
// with up, which may be nil if every state in them is current. The chunk is
// marked as holding legacy states if any entry it uses is older than
// chunk.CurrentBlockVersion.
func DecodeChunk(t *Terrain, table *BlockTable, up *chunk.Upgrader, r cube.Range) (*chunk.Chunk, error) {
	c := chunk.New(table.reg, r)
	if len(t.Data3D) > heightMapSize {
		if err := chunk.DecodeDiskBiomes(c, t.Data3D[heightMapSize:]); err != nil {
			return nil, fmt.Errorf("hx: decode biomes: %w", err)
		}
	}
	bottom := r[0] >> 4
	legacy := false
	for i := range t.Subs {
		s := &t.Subs[i]
		index := int(s.Index) - bottom
		if s.Raw != nil {
			at, sub, err := chunk.DecodeDiskSubChunk(c, up, s.Raw, index)
			if err != nil {
				return nil, fmt.Errorf("hx: decode raw sub chunk %d: %w", s.Index, err)
			}
			c.SetSubChunk(at, sub)
			continue
		}
		sub, old, err := decodeSub(s, table)
		if err != nil {
			return nil, fmt.Errorf("hx: sub chunk %d: %w", s.Index, err)
		}
		legacy = legacy || old
		if !c.SetSubChunk(index, sub) {
			return nil, fmt.Errorf("hx: sub chunk %d outside the chunk range", s.Index)
		}
	}
	if legacy {
		c.MarkLegacyStates()
	}
	return c, nil
}

// decodeSub builds a sub chunk from its layers. All the words and palettes of
// the sub chunk share one allocation.
func decodeSub(s *Sub, table *BlockTable) (*chunk.SubChunk, bool, error) {
	n := 0
	for _, l := range s.Layers {
		n += len(l.Words)/4 + len(l.Palette)
	}
	mem := make([]uint32, n)
	layers := make([]*chunk.PalettedStorage, len(s.Layers))
	legacy := false
	for i, l := range s.Layers {
		words := mem[: len(l.Words)/4 : len(l.Words)/4]
		mem = mem[len(words):]
		for j := range words {
			words[j] = binary.LittleEndian.Uint32(l.Words[4*j:])
		}
		values := mem[:len(l.Palette):len(l.Palette)]
		mem = mem[len(values):]
		for j, id := range l.Palette {
			rid, old, err := table.RuntimeID(id)
			if err != nil {
				return nil, false, err
			}
			values[j] = rid
			legacy = legacy || old
		}
		storage, err := chunk.NewRawStorage(l.Bits, words, values)
		if err != nil {
			return nil, false, fmt.Errorf("layer %d: %w", i, err)
		}
		layers[i] = storage
	}
	return chunk.NewSubChunkWith(table.reg.AirRuntimeID(), layers), legacy, nil
}

// AppendChunk appends the terrain record of c to dst. keys are the chunk's
// other small BDS keys, written as given. savedData3D is the Data3D the chunk
// was loaded with, or nil: its height map keeps columns BDS left higher than
// the blocks (see appendHeightMap). Palette entries the table does not have
// yet are added to it.
func AppendChunk(dst []byte, c *chunk.Chunk, table *BlockTable, keys []ChunkKey, savedData3D []byte) ([]byte, error) {
	dst = append(dst, terrainVersion)
	for _, k := range keys {
		dst = appendSection(dst, secKey, 1+len(k.Value))
		dst = append(dst, k.Tag)
		dst = append(dst, k.Value...)
	}
	biomes := chunk.EncodeBiomes(c, chunk.DiskEncoding)
	dst = appendSection(dst, secData3D, heightMapSize+len(biomes))
	dst = appendHeightMap(dst, c, savedData3D, table.reg)
	dst = append(dst, biomes...)

	bottom := c.Range()[0] >> 4
	for i, sub := range c.Sub() {
		if sub == nil || len(sub.Layers()) == 0 {
			continue
		}
		var err error
		if dst, err = appendChunkSub(dst, int8(i+bottom), sub, table); err != nil { //nolint:gosec // sub chunk Y fits int8
			return dst, fmt.Errorf("hx: sub chunk %d: %w", i+bottom, err)
		}
	}
	return dst, nil
}

// appendChunkSub writes one sub chunk section: a pass for its length, then one
// for its bytes.
func appendChunkSub(dst []byte, index int8, sub *chunk.SubChunk, table *BlockTable) ([]byte, error) {
	layers := sub.Layers()
	if len(layers) > maxLayers {
		return dst, fmt.Errorf("%d layers", len(layers))
	}
	n := 3
	for _, l := range layers {
		_, words := l.Raw()
		p := l.Palette()
		n += 1 + 4*len(words) + uvarintLen(uint64(p.Len())) //nolint:gosec // palette length
		for j := range p.Len() {
			id, err := table.ID(p.Value(uint16(j))) //nolint:gosec // at most 4096
			if err != nil {
				return dst, err
			}
			n += uvarintLen(uint64(id))
		}
	}
	dst = appendSection(dst, secSub, n)
	dst = append(dst, byte(index), subChunkVersion, byte(len(layers))) //nolint:gosec // at most maxLayers
	for _, l := range layers {
		bits, words := l.Raw()
		dst = append(dst, byte(bits)) //nolint:gosec // at most 16
		for _, w := range words {
			dst = binary.LittleEndian.AppendUint32(dst, w)
		}
		p := l.Palette()
		dst = binary.AppendUvarint(dst, uint64(p.Len())) //nolint:gosec // palette length
		for j := range p.Len() {
			id, _ := table.ID(p.Value(uint16(j))) //nolint:gosec // found in the first pass
			dst = binary.AppendUvarint(dst, uint64(id))
		}
	}
	return dst, nil
}
