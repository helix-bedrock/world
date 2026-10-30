package mcdb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"slices"
	"sync"

	"github.com/helix-bedrock/protocol/nbt"
)

// Chunk metadata, as BDS 1.26.60 keeps it: every chunk's '?' key holds the
// hash of a compound in the LevelChunkMetaDataDictionary level key. The hash
// is xxHash64 (seed 0) of the compound in network NBT encoding with its keys
// in sorted order, the order BDS's compounds always have. The dictionary
// value is a uint32 count, then per entry the hash and the compound as LE
// NBT, sorted by hash. Checked against every entry of worlds BDS saved.

// metaDictionary is the in-memory LevelChunkMetaDataDictionary. Safe for
// concurrent use.
type metaDictionary struct {
	mu      sync.Mutex
	entries map[uint64]map[string]any
	dirty   bool
	// saved maps a hash loaded from a chunk to the hash it gets when saved
	// now, so a save encodes and hashes a compound once, not per chunk.
	saved map[uint64]uint64
	// fresh holds the saved hash for new chunks, per dimension.
	fresh map[Dimension]uint64
}

func newMetaDictionary() *metaDictionary {
	return &metaDictionary{entries: map[uint64]map[string]any{}, saved: map[uint64]uint64{}, fresh: map[Dimension]uint64{}}
}

// decodeMetaDictionary reads the dictionary value. Entries that fail to
// decode end the read with an error; those before them are kept.
func decodeMetaDictionary(b []byte) (*metaDictionary, error) {
	d := newMetaDictionary()
	if len(b) < 4 {
		return d, fmt.Errorf("chunk metadata: %d bytes, need at least 4", len(b))
	}
	n := binary.LittleEndian.Uint32(b)
	buf := bytes.NewBuffer(b[4:])
	for i := range n {
		if buf.Len() < 8 {
			return d, fmt.Errorf("chunk metadata: entry %d: truncated hash", i)
		}
		h := binary.LittleEndian.Uint64(buf.Next(8))
		var m map[string]any
		if err := nbt.NewDecoderWithEncoding(buf, nbt.LittleEndian).Decode(&m); err != nil {
			return d, fmt.Errorf("chunk metadata: entry %d: %w", i, err)
		}
		d.entries[h] = m
	}
	return d, nil
}

// encode returns the dictionary value and whether it changed since the last
// call.
func (d *metaDictionary) encode() ([]byte, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.dirty {
		return nil, false, nil
	}
	hashes := make([]uint64, 0, len(d.entries))
	for h := range d.entries {
		hashes = append(hashes, h)
	}
	slices.Sort(hashes)
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(hashes))) //nolint:gosec // entry count
	for _, h := range hashes {
		b = binary.LittleEndian.AppendUint64(b, h)
		var err error
		if b, err = appendSortedNBT(b, d.entries[h], false); err != nil {
			return nil, false, fmt.Errorf("chunk metadata %016x: %w", h, err)
		}
	}
	d.dirty = false
	return b, true, nil
}

// markDirty makes the next encode write the dictionary again, after a
// failed flush.
func (d *metaDictionary) markDirty() {
	d.mu.Lock()
	d.dirty = true
	d.mu.Unlock()
}

// register adds m and returns its hash.
func (d *metaDictionary) register(m map[string]any) (uint64, error) {
	net, err := appendSortedNBT(nil, m, true)
	if err != nil {
		return 0, err
	}
	h := xxh64(net)
	if _, ok := d.entries[h]; !ok {
		d.entries[h] = m
		d.dirty = true
	}
	return h, nil
}

// metaGameVersion is the base game version written into chunk metadata.
const metaGameVersion = "1.26.60"

// savedHash returns the hash a chunk with metadata hash h gets when saved:
// its entry with the last saved version and height range set to now. A new
// chunk (h == 0) gets the metadata BDS gives a chunk it generates. A hash
// missing from the dictionary is kept as is.
func (d *metaDictionary) savedHash(h uint64, dim Dimension, seed int64, generator int32) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if h == 0 {
		if s, ok := d.fresh[dim]; ok {
			return s, nil
		}
		// BDS registers the generated chunk's metadata, then the saved form.
		m := freshMeta(dim, seed, generator)
		if _, err := d.register(m); err != nil {
			return 0, err
		}
		s, err := d.register(withLastSaved(m, dim))
		if err != nil {
			return 0, err
		}
		d.fresh[dim] = s
		return s, nil
	}
	if s, ok := d.saved[h]; ok {
		return s, nil
	}
	m, ok := d.entries[h]
	if !ok {
		return h, nil
	}
	s, err := d.register(withLastSaved(m, dim))
	if err != nil {
		return 0, err
	}
	d.saved[h] = s
	return s, nil
}

// dimensionName is the name BDS stores in chunk metadata.
func dimensionName(dim Dimension) string {
	switch dim {
	case Nether:
		return "Nether"
	case End:
		return "TheEnd"
	}
	return "Overworld"
}

// heightRange is the {max, min} compound of chunk metadata; max is
// exclusive.
func heightRange(dim Dimension) map[string]any {
	r, _ := dim.Range()
	return map[string]any{"max": int16(r.Max() + 1), "min": int16(r.Min())} //nolint:gosec // dimension heights fit in int16
}

// freshMeta is the metadata BDS 1.26.60 gives a chunk it generates.
func freshMeta(dim Dimension, seed int64, generator int32) map[string]any {
	m := map[string]any{
		"BiomeBaseGameVersion":             "1.18.0",
		"DimensionName":                    dimensionName(dim),
		"GenerationSeed":                   seed,
		"GeneratorType":                    generator,
		"NeighborAwareBlockUpgradeVersion": int32(6),
		"OriginalBaseGameVersion":          metaGameVersion,
		"OriginalDimensionHeightRange":     heightRange(dim),
		"SkullFlatteningPerformed":         int16(1),
	}
	if dim == Overworld {
		m["Overworld1_18HeightExtended"] = int16(1)
		m["UnderwaterLavaLakeFixed"] = int16(1)
		m["WorldGenBelowZeroFixed"] = int16(1)
	}
	return m
}

// withLastSaved returns a copy of m with the last saved fields set to now.
func withLastSaved(m map[string]any, dim Dimension) map[string]any {
	c := make(map[string]any, len(m)+2)
	for k, v := range m {
		c[k] = v
	}
	c["LastSavedBaseGameVersion"] = metaGameVersion
	c["LastSavedDimensionHeightRange"] = heightRange(dim)
	return c
}

// NBT tag types used by chunk metadata.
const (
	tagEnd      = 0
	tagByte     = 1
	tagShort    = 2
	tagInt      = 3
	tagLong     = 4
	tagFloat    = 5
	tagDouble   = 6
	tagString   = 8
	tagCompound = 10
)

// appendSortedNBT appends m as an unnamed root compound with its keys
// sorted, in network encoding if network is true and LE otherwise. The
// protocol NBT encoder writes map keys in random order, which would change
// the hash.
func appendSortedNBT(b []byte, m map[string]any, network bool) ([]byte, error) {
	b = append(b, tagCompound)
	b = appendNBTString(b, "", network)
	return appendCompound(b, m, network)
}

func appendCompound(b []byte, m map[string]any, network bool) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		var err error
		if b, err = appendTag(b, k, m[k], network); err != nil {
			return nil, err
		}
	}
	return append(b, tagEnd), nil
}

func appendTag(b []byte, name string, v any, network bool) ([]byte, error) {
	switch v := v.(type) {
	case uint8:
		b = appendNBTString(append(b, tagByte), name, network)
		return append(b, v), nil
	case int16:
		b = appendNBTString(append(b, tagShort), name, network)
		return binary.LittleEndian.AppendUint16(b, uint16(v)), nil //nolint:gosec // raw bits
	case int32:
		b = appendNBTString(append(b, tagInt), name, network)
		if network {
			return binary.AppendVarint(b, int64(v)), nil
		}
		return binary.LittleEndian.AppendUint32(b, uint32(v)), nil //nolint:gosec // raw bits
	case int64:
		b = appendNBTString(append(b, tagLong), name, network)
		if network {
			return binary.AppendVarint(b, v), nil
		}
		return binary.LittleEndian.AppendUint64(b, uint64(v)), nil //nolint:gosec // raw bits
	case float32:
		b = appendNBTString(append(b, tagFloat), name, network)
		return binary.LittleEndian.AppendUint32(b, math.Float32bits(v)), nil
	case float64:
		b = appendNBTString(append(b, tagDouble), name, network)
		return binary.LittleEndian.AppendUint64(b, math.Float64bits(v)), nil
	case string:
		b = appendNBTString(append(b, tagString), name, network)
		return appendNBTString(b, v, network), nil
	case map[string]any:
		b = appendNBTString(append(b, tagCompound), name, network)
		return appendCompound(b, v, network)
	}
	return nil, fmt.Errorf("tag %q: unsupported type %T", name, v)
}

func appendNBTString(b []byte, s string, network bool) []byte {
	if network {
		b = binary.AppendUvarint(b, uint64(len(s)))
	} else {
		b = binary.LittleEndian.AppendUint16(b, uint16(min(len(s), math.MaxUint16))) //nolint:gosec // clamped
		s = s[:min(len(s), math.MaxUint16)]
	}
	return append(b, s...)
}

// xxHash64 primes.
const (
	xxPrime1 uint64 = 11400714785074694791
	xxPrime2 uint64 = 14029467366897019727
	xxPrime3 uint64 = 1609587929392839161
	xxPrime4 uint64 = 9650029242287828579
	xxPrime5 uint64 = 2870177450012600261
)

func xxRound(acc, in uint64) uint64 {
	acc += in * xxPrime2
	return bits.RotateLeft64(acc, 31) * xxPrime1
}

func xxMerge(acc, v uint64) uint64 {
	acc ^= xxRound(0, v)
	return acc*xxPrime1 + xxPrime4
}

// xxh64 is xxHash64 with seed 0, the hash BDS uses for chunk metadata.
func xxh64(b []byte) uint64 {
	n := len(b)
	var h uint64
	if n >= 32 {
		p1, p2 := xxPrime1, xxPrime2 // wrapping arithmetic needs variables
		v1, v2, v3, v4 := p1+p2, p2, uint64(0), -p1
		for len(b) >= 32 {
			v1 = xxRound(v1, binary.LittleEndian.Uint64(b))
			v2 = xxRound(v2, binary.LittleEndian.Uint64(b[8:]))
			v3 = xxRound(v3, binary.LittleEndian.Uint64(b[16:]))
			v4 = xxRound(v4, binary.LittleEndian.Uint64(b[24:]))
			b = b[32:]
		}
		h = bits.RotateLeft64(v1, 1) + bits.RotateLeft64(v2, 7) + bits.RotateLeft64(v3, 12) + bits.RotateLeft64(v4, 18)
		h = xxMerge(h, v1)
		h = xxMerge(h, v2)
		h = xxMerge(h, v3)
		h = xxMerge(h, v4)
	} else {
		h = xxPrime5
	}
	h += uint64(n)
	for ; len(b) >= 8; b = b[8:] {
		h ^= xxRound(0, binary.LittleEndian.Uint64(b))
		h = bits.RotateLeft64(h, 27)*xxPrime1 + xxPrime4
	}
	if len(b) >= 4 {
		h ^= uint64(binary.LittleEndian.Uint32(b)) * xxPrime1
		h = bits.RotateLeft64(h, 23)*xxPrime2 + xxPrime3
		b = b[4:]
	}
	for ; len(b) > 0; b = b[1:] {
		h ^= uint64(b[0]) * xxPrime5
		h = bits.RotateLeft64(h, 11) * xxPrime1
	}
	h ^= h >> 33
	h *= xxPrime2
	h ^= h >> 29
	h *= xxPrime3
	h ^= h >> 32
	return h
}
