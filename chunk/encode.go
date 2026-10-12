// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/world/chunk/encode.go

package chunk

import (
	"bytes"
	"sync"
)

const (
	// SubChunkVersion is the current version of the written sub chunks, specifying the format they are
	// written on disk and over network.
	SubChunkVersion = 9
	// CurrentBlockVersion is the current version of blocks (states) of the game. This version is composed
	// of 4 bytes indicating a version, interpreted as a big endian int. The current version represents
	// 1.21.60.33 {1, 21, 60, 33}.
	CurrentBlockVersion int32 = 18168865
)

// pool reuses scratch buffers for encoding. It is package-level on purpose:
// it holds no data between calls (every result is copied out and the buffer
// reset), sync.Pool is safe for concurrent use, and encoding runs per chunk
// sent or saved, where allocating a fresh buffer each time adds up.
var pool = sync.Pool{
	New: func() any {
		return bytes.NewBuffer(make([]byte, 0, 1024))
	},
}

func getBuffer() *bytes.Buffer {
	if buf, ok := pool.Get().(*bytes.Buffer); ok {
		return buf
	}
	return bytes.NewBuffer(make([]byte, 0, 1024))
}

type (
	// SerialisedData holds the serialised data of a chunk. It consists of the chunk's block data itself, a height
	// map, the biomes and entities and block entities.
	SerialisedData struct {
		// sub holds the data of the serialised sub chunks in a chunk. Sub chunks that are empty or that otherwise
		// don't exist are represented as an empty slice (or technically, nil).
		SubChunks [][]byte
		// Biomes is the biome data of the chunk, which is composed of a biome storage for each sub-chunk.
		Biomes []byte
	}
	// blockEntry represents a block as found in a disk save of a world.
	blockEntry struct {
		Name    string         `nbt:"name"`
		State   map[string]any `nbt:"states"`
		Version int32          `nbt:"version"`
	}
)

// Encode encodes Chunk to an intermediate representation SerialisedData. An Encoding may be passed to encode either for
// network or disk purposed, the most notable difference being that the network encoding generally uses varints and no
// NBT.
func Encode(c *Chunk, e Encoding) SerialisedData {
	d := SerialisedData{SubChunks: make([][]byte, len(c.sub))}
	for i, sub := range c.sub {
		// The game saves all-air sub chunks (one layer of air) but never
		// one with no layers, which a never-touched SubChunk has. Leave
		// those nil on disk; DiskDecode treats nil as absent.
		if e.network() == 0 && (sub == nil || len(sub.Layers()) == 0) {
			continue
		}
		d.SubChunks[i] = EncodeSubChunk(c, e, i)
	}
	d.Biomes = EncodeBiomes(c, e)
	return d
}

// EncodeSubChunk encodes a sub-chunk from a chunk into bytes. An Encoding may be passed to encode either for network or
// disk purposed, the most notable difference being that the network encoding generally uses varints and no NBT.
func EncodeSubChunk(c *Chunk, e Encoding, ind int) []byte {
	buf := getBuffer()
	defer func() {
		buf.Reset()
		pool.Put(buf)
	}()

	s := c.sub[ind]
	_, _ = buf.Write([]byte{SubChunkVersion, byte(len(s.storages)), uint8(ind + (c.r[0] >> 4))})
	for _, storage := range s.storages {
		encodePalettedStorage(buf, storage, nil, e, BlockPaletteEncoding{Blocks: c.br})
	}
	sub := make([]byte, buf.Len())
	_, _ = buf.Read(sub)
	return sub
}

// EncodeBiomes encodes the biomes of a chunk into bytes. An Encoding may be passed to encode either for network or
// disk purposed, the most notable difference being that the network encoding generally uses varints and no NBT.
func EncodeBiomes(c *Chunk, e Encoding) []byte {
	buf := getBuffer()
	defer func() {
		buf.Reset()
		pool.Put(buf)
	}()

	if e.network() == 0 {
		// On disk the game writes every biome storage in full, with the
		// palette-type bit set (biome palettes hold plain IDs, not NBT),
		// and never the "same as previous" marker. Match it exactly.
		for _, b := range c.biomes {
			encodeStorage(buf, b, 1)
			DiskEncoding.encodePalette(buf, b.palette, BiomePaletteEncoding)
		}
	} else {
		var previous *PalettedStorage
		for _, b := range c.biomes {
			encodePalettedStorage(buf, b, previous, e, BiomePaletteEncoding)
			previous = b
		}
	}
	biomes := make([]byte, buf.Len())
	_, _ = buf.Read(biomes)
	return biomes
}

// encodePalettedStorage encodes a PalettedStorage into a bytes.Buffer. The Encoding passed is used to write the Palette
// of the PalettedStorage.
func encodePalettedStorage(buf *bytes.Buffer, storage, previous *PalettedStorage, e Encoding, pe paletteEncoding) {
	if storage.Equal(previous) {
		_, _ = buf.Write([]byte{0x7f<<1 | e.network()})
		return
	}
	encodeStorage(buf, storage, e.network())
	e.encodePalette(buf, storage.palette, pe)
}

// encodeStorage writes a storage's header byte (bits per index and the
// palette-type flag) and its packed indices.
func encodeStorage(buf *bytes.Buffer, storage *PalettedStorage, flag byte) {
	b := make([]byte, len(storage.indices)*4+1)
	b[0] = byte(storage.bitsPerIndex<<1) | flag

	for i, v := range storage.indices {
		// Explicitly don't use the binary package to greatly improve performance of writing the uint32s.
		b[i*4+1], b[i*4+2], b[i*4+3], b[i*4+4] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	}
	_, _ = buf.Write(b)
}
