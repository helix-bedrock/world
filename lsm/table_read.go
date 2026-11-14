package lsm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
)

// table is an open table file.
type table struct {
	num   uint64
	f     io.ReaderAt
	size  uint64
	index block
	// filter is the table's bloom filter; ok is false for a table written without one.
	filter    filterReader
	hasFilter bool
	cache     *blockCache
	// bigBlock is the stored size from which a block is never cached and is streamed when
	// a single value is read from it.
	bigBlock  uint64
	inflaters sync.Pool // *inflater
}

// openTable reads a table's footer, index and filter.
func openTable(num uint64, f io.ReaderAt, size uint64, cache *blockCache, bigBlock uint64) (*table, error) {
	if size < footerLen {
		return nil, errCorrupt(fmt.Sprintf("table %d: too short", num))
	}
	footer := make([]byte, footerLen)
	if _, err := f.ReadAt(footer, int64(size-footerLen)); err != nil { //nolint:gosec // file size
		return nil, fmt.Errorf("lsm: read table %d footer: %w", num, err)
	}
	if binary.LittleEndian.Uint64(footer[footerLen-8:]) != tableMagic {
		return nil, errCorrupt(fmt.Sprintf("table %d: bad magic", num))
	}
	metaHandle, rest, err := decodeHandle(footer)
	if err != nil {
		return nil, err
	}
	indexHandle, _, err := decodeHandle(rest)
	if err != nil {
		return nil, err
	}
	t := &table{num: num, f: f, size: size, cache: cache, bigBlock: bigBlock}
	raw, err := t.readBlockUncached(indexHandle)
	if err != nil {
		return nil, fmt.Errorf("lsm: table %d index: %w", num, err)
	}
	if t.index, err = parseBlock(raw); err != nil {
		return nil, err
	}
	t.readFilter(metaHandle)
	return t, nil
}

// readFilter loads the table's bloom filter, if it has one this package reads. A table
// without one, or with a broken one, is read without it.
func (t *table) readFilter(metaHandle blockHandle) {
	raw, err := t.readBlockUncached(metaHandle)
	if err != nil {
		return
	}
	meta, err := parseBlock(raw)
	if err != nil {
		return
	}
	it := newBlockIter(meta, bytes.Compare)
	for ok := it.first(); ok; ok = it.next() {
		name := string(it.key)
		if name != "filter."+filterName && name != "filter.leveldb.BuiltinBloomFilter" {
			continue
		}
		h, _, err := decodeHandle(it.value)
		if err != nil {
			return
		}
		fb, err := t.readBlockUncached(h)
		if err != nil {
			return
		}
		t.filter, t.hasFilter = newFilterReader(fb)
		return
	}
}

func (t *table) getInflater() *inflater {
	if in, ok := t.inflaters.Get().(*inflater); ok {
		return in
	}
	return new(inflater)
}

// readBlockUncached reads, checks and decompresses a block.
func (t *table) readBlockUncached(h blockHandle) ([]byte, error) {
	if h.offset > t.size || h.size > t.size-h.offset || t.size-h.offset-h.size < blockTrailerLen {
		return nil, errCorrupt(fmt.Sprintf("table %d: block handle out of range", t.num))
	}
	buf := make([]byte, h.size+blockTrailerLen)
	if _, err := t.f.ReadAt(buf, int64(h.offset)); err != nil { //nolint:gosec // in range
		return nil, fmt.Errorf("lsm: read table %d: %w", t.num, err)
	}
	data, trailer := buf[:h.size], buf[h.size:]
	c := crc32.Update(crc32.Checksum(data, castagnoli()), castagnoli(), trailer[:1])
	if unmaskCRC(binary.LittleEndian.Uint32(trailer[1:])) != c {
		return nil, errCorrupt(fmt.Sprintf("table %d: block checksum", t.num))
	}
	in := t.getInflater()
	defer t.inflaters.Put(in)
	return in.decompress(data, trailer[0])
}

// readBlock returns a decoded block, through the cache for blocks under bigBlock.
func (t *table) readBlock(h blockHandle) (block, error) {
	small := h.size < t.bigBlock
	if small {
		if raw, ok := t.cache.get(t.num, h.offset); ok {
			return parseBlock(raw)
		}
	}
	raw, err := t.readBlockUncached(h)
	if err != nil {
		return block{}, err
	}
	if small {
		t.cache.put(t.num, h.offset, raw)
	}
	return parseBlock(raw)
}

// find returns the handle of the data block that may hold ikey, and whether there is one.
func (t *table) find(ikey []byte) (blockHandle, bool, error) {
	it := newBlockIter(t.index, compareInternal)
	if !it.seek(ikey) {
		return blockHandle{}, false, it.err
	}
	h, _, err := decodeHandle(it.value)
	return h, err == nil, err
}

// get finds the first entry at or after ikey whose user key is ikey's, returning its
// internal key and value; found is false if there is none in this table. The value is
// valid as long as the block is: callers copy it.
func (t *table) get(ikey []byte) (key, value []byte, found bool, err error) {
	h, ok, err := t.find(ikey)
	if !ok || err != nil {
		return nil, nil, false, err
	}
	ukey := userKey(ikey)
	if t.hasFilter && !t.filter.mayContain(h.offset, ukey) {
		return nil, nil, false, nil
	}
	if h.size >= t.bigBlock {
		// Streamed: only the entries up to ikey's are inflated, and only its value kept.
		b, found, err := t.seekBig(h, ikey)
		if !found || err != nil {
			return nil, nil, false, err
		}
		defer b.Close()
		key = append([]byte(nil), b.key...)
		value, err = b.value(nil)
		return key, value, err == nil, err
	}
	b, err := t.readBlock(h)
	if err != nil {
		return nil, nil, false, err
	}
	it := newBlockIter(b, compareInternal)
	if !it.seek(ikey) {
		return nil, nil, false, it.err
	}
	if !bytes.Equal(userKey(it.key), ukey) {
		return nil, nil, false, nil
	}
	return it.key, it.value, true, nil
}

// getStream is get for a value that may be big: a value of bigBlock bytes or more is
// returned as a stream inflating its block as it is read, never held whole. small is set
// instead for a smaller value.
func (t *table) getStream(ikey []byte) (key []byte, small []byte, stream *valueStream, found bool, err error) {
	h, ok, err := t.find(ikey)
	if !ok || err != nil {
		return nil, nil, nil, false, err
	}
	ukey := userKey(ikey)
	if t.hasFilter && !t.filter.mayContain(h.offset, ukey) {
		return nil, nil, nil, false, nil
	}
	if h.size < t.bigBlock {
		key, value, found, err := t.get(ikey)
		return key, value, nil, found, err
	}
	b, found, err := t.seekBig(h, ikey)
	if !found || err != nil {
		return nil, nil, nil, false, err
	}
	key = append([]byte(nil), b.key...)
	if b.vlen < int64(t.bigBlock) { //nolint:gosec // small
		small, err = b.value(nil)
		b.Close()
		return key, small, nil, err == nil, err
	}
	return key, nil, &valueStream{b: b, size: b.vlen}, true, nil
}

// seekBig opens a big block at the first entry whose user key is ikey's, at or after
// ikey. When found is false the block is closed.
func (t *table) seekBig(h blockHandle, ikey []byte) (b *bigReader, found bool, err error) {
	b, ok, err := t.openBig(h)
	if err != nil {
		return nil, false, err
	}
	for ok && compareInternal(b.key, ikey) < 0 {
		ok, err = b.next()
	}
	if err != nil || !ok || !bytes.Equal(userKey(b.key), userKey(ikey)) {
		b.Close()
		return nil, false, err
	}
	return b, true, nil
}
