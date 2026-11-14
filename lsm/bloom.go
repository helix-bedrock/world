package lsm

import "encoding/binary"

// The bloom filter is LevelDB's built-in one (util/bloom.cc, "leveldb.BuiltinBloomFilter2"),
// over user keys, with LevelDB's hash. A table's filter block holds one filter per 2 KB of
// data block offsets (filter_block.cc), so a lookup checks the filter of the one block the
// index points at.

// filterName is the filter's name; the metaindex block stores it as "filter." + name.
const filterName = "leveldb.BuiltinBloomFilter2"

// bitsPerKey is what Mojang's LevelDB example configures (NewBloomFilterPolicy(10)): about
// a 1% false positive rate.
const bitsPerKey = 10

// filterBaseLg is log2 of the data block offset range one filter covers.
const filterBaseLg = 11

// leveldbHash is LevelDB's Hash (util/hash.cc) over unsigned bytes.
func leveldbHash(data []byte, seed uint32) uint32 {
	const m = 0xc6a4a793
	h := seed ^ uint32(len(data))*m //nolint:gosec // wraps as LevelDB's does
	for ; len(data) >= 4; data = data[4:] {
		h += binary.LittleEndian.Uint32(data)
		h *= m
		h ^= h >> 16
	}
	switch len(data) {
	case 3:
		h += uint32(data[2]) << 16
		fallthrough
	case 2:
		h += uint32(data[1]) << 8
		fallthrough
	case 1:
		h += uint32(data[0])
		h *= m
		h ^= h >> 24
	}
	return h
}

func bloomHash(key []byte) uint32 { return leveldbHash(key, 0xbc9f1d34) }

// probes is how many bits each key sets: bitsPerKey * ln 2, between 1 and 30.
func probes() int { return min(max(bitsPerKey*69/100, 1), 30) }

// appendBloom appends the filter of the keys whose hashes are given.
func appendBloom(dst []byte, hashes []uint32) []byte {
	bits := max(len(hashes)*bitsPerKey, 64)
	n := (bits + 7) / 8
	bits = n * 8
	start := len(dst)
	dst = append(dst, make([]byte, n)...)
	array := dst[start:]
	k := probes()
	for _, h := range hashes {
		delta := h>>17 | h<<15
		for range k {
			pos := h % uint32(bits) //nolint:gosec // bits fits uint32
			array[pos/8] |= 1 << (pos % 8)
			h += delta
		}
	}
	return append(dst, byte(k))
}

// bloomMayContain reports whether a filter may hold the key.
func bloomMayContain(filter, key []byte) bool {
	if len(filter) < 2 {
		return false
	}
	k := int(filter[len(filter)-1])
	if k > 30 {
		return true // reserved for other encodings: treat as a match
	}
	array := filter[:len(filter)-1]
	bits := uint32(len(array) * 8) //nolint:gosec // filters are small
	h := bloomHash(key)
	delta := h>>17 | h<<15
	for range k {
		pos := h % bits
		if array[pos/8]&(1<<(pos%8)) == 0 {
			return false
		}
		h += delta
	}
	return true
}

// filterBuilder builds a table's filter block.
type filterBuilder struct {
	hashes  []uint32
	result  []byte
	offsets []uint32
}

// startBlock is called with the offset of each data block before its keys are added.
func (f *filterBuilder) startBlock(offset uint64) {
	index := offset >> filterBaseLg
	for uint64(len(f.offsets)) < index {
		f.generate()
	}
}

// addKey adds a user key to the current filter.
func (f *filterBuilder) addKey(ukey []byte) { f.hashes = append(f.hashes, bloomHash(ukey)) }

func (f *filterBuilder) generate() {
	f.offsets = append(f.offsets, uint32(len(f.result))) //nolint:gosec // small
	if len(f.hashes) == 0 {
		return
	}
	f.result = appendBloom(f.result, f.hashes)
	f.hashes = f.hashes[:0]
}

// finish returns the filter block.
func (f *filterBuilder) finish() []byte {
	if len(f.hashes) > 0 {
		f.generate()
	}
	arrayOffset := uint32(len(f.result)) //nolint:gosec // small
	for _, o := range f.offsets {
		f.result = binary.LittleEndian.AppendUint32(f.result, o)
	}
	f.result = binary.LittleEndian.AppendUint32(f.result, arrayOffset)
	return append(f.result, filterBaseLg)
}

// filterReader answers lookups from a filter block.
type filterReader struct {
	data        []byte
	arrayOffset int
	num         int
	baseLg      uint
}

func newFilterReader(b []byte) (filterReader, bool) {
	if len(b) < 5 {
		return filterReader{}, false
	}
	baseLg := uint(b[len(b)-1])
	arrayOffset := int(binary.LittleEndian.Uint32(b[len(b)-5:]))
	if arrayOffset > len(b)-5 || baseLg > 63 {
		return filterReader{}, false
	}
	return filterReader{data: b, arrayOffset: arrayOffset, num: (len(b) - 5 - arrayOffset) / 4, baseLg: baseLg}, true
}

// mayContain reports whether the data block at blockOffset may hold the user key.
func (f filterReader) mayContain(blockOffset uint64, ukey []byte) bool {
	index := blockOffset >> f.baseLg
	if index >= uint64(f.num) {
		return true
	}
	at := f.arrayOffset + int(index)*4
	start := int(binary.LittleEndian.Uint32(f.data[at:]))
	limit := int(binary.LittleEndian.Uint32(f.data[at+4:]))
	switch {
	case start == limit:
		return false
	case start < limit && limit <= f.arrayOffset:
		return bloomMayContain(f.data[start:limit], ukey)
	}
	return true
}
