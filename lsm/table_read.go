package lsm

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"hash"
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

// getStream is get for a value that may be big: a value in a block over bigBlock is
// returned as a stream inflating the block as it is read, never held whole. small is set
// instead when the block is small.
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
	s, err := t.openStream(h)
	if err != nil {
		return nil, nil, nil, false, err
	}
	if !bytes.Equal(userKey(s.key), ukey) || compareInternal(s.key, ikey) < 0 {
		// A big block holds one entry; if it is not this key, the key is not here.
		_ = s.Close()
		return nil, nil, nil, false, nil
	}
	return s.key, nil, s, true, nil
}

// valueStream reads the value of the one entry of a big block, inflating as it goes and
// checking the block's checksum when the value has been read to its end.
type valueStream struct {
	t     *table
	in    *inflater
	crc   hash.Hash32
	typ   byte
	h     blockHandle
	raw   *bufio.Reader
	r     io.Reader // the decompressed block, positioned at the value
	key     []byte
	size    int64
	left    int64
	done    bool
	wantCRC uint32
}

// openStream reads the entry header of a big block and returns a stream of its value.
func (t *table) openStream(h blockHandle) (*valueStream, error) {
	if h.offset > t.size || h.size > t.size-h.offset || t.size-h.offset-h.size < blockTrailerLen {
		return nil, errCorrupt(fmt.Sprintf("table %d: block handle out of range", t.num))
	}
	var trailer [blockTrailerLen]byte
	if _, err := t.f.ReadAt(trailer[:], int64(h.offset+h.size)); err != nil { //nolint:gosec // in range
		return nil, fmt.Errorf("lsm: read table %d: %w", t.num, err)
	}
	s := &valueStream{t: t, in: t.getInflater(), crc: crc32.New(castagnoli()), typ: trailer[0], h: h}
	sec := io.NewSectionReader(t.f, int64(h.offset), int64(h.size)) //nolint:gosec // in range
	s.raw = bufio.NewReaderSize(io.TeeReader(sec, s.crc), 64<<10)
	r, err := s.in.reader(s.raw, s.typ)
	if err != nil {
		s.release()
		return nil, err
	}
	s.r = r
	if err := s.readHeader(); err != nil {
		s.release()
		return nil, err
	}
	s.wantCRC = unmaskCRC(binary.LittleEndian.Uint32(trailer[1:]))
	return s, nil
}

// readHeader reads the entry's lengths and key.
func (s *valueStream) readHeader() error {
	br := byteReader{r: s.r}
	shared, err := binary.ReadUvarint(&br)
	if err != nil || shared != 0 {
		return errCorrupt("big block entry")
	}
	klen, err := binary.ReadUvarint(&br)
	if err != nil || klen > 1<<20 {
		return errCorrupt("big block entry")
	}
	vlen, err := binary.ReadUvarint(&br)
	if err != nil || vlen > 1<<62 {
		return errCorrupt("big block entry")
	}
	s.key = make([]byte, klen)
	if _, err := io.ReadFull(s.r, s.key); err != nil {
		return errCorrupt("big block key")
	}
	s.size, s.left = int64(vlen), int64(vlen) //nolint:gosec // checked above
	return nil
}

// byteReader reads single bytes from a reader for varints.
type byteReader struct {
	r   io.Reader
	one [1]byte
}

func (b *byteReader) ReadByte() (byte, error) {
	_, err := io.ReadFull(b.r, b.one[:])
	return b.one[0], err
}

// Size is the value's length.
func (s *valueStream) Size() int64 { return s.size }

// Read reads the value.
func (s *valueStream) Read(p []byte) (int, error) {
	if s.left == 0 {
		if err := s.finish(); err != nil {
			return 0, err
		}
		return 0, io.EOF
	}
	if int64(len(p)) > s.left {
		p = p[:s.left]
	}
	n, err := s.r.Read(p)
	s.left -= int64(n)
	if err == io.EOF {
		if s.left > 0 {
			return n, errCorrupt("big block cut short")
		}
		err = nil
	}
	return n, err
}

// finish checks the block's checksum once its value has been read: what is left of the
// block (its restart points) is read through so every stored byte is checksummed.
func (s *valueStream) finish() error {
	if s.done {
		return nil
	}
	s.done = true
	if _, err := io.Copy(io.Discard, s.raw); err != nil {
		return fmt.Errorf("lsm: read table %d: %w", s.t.num, err)
	}
	c := crc32.Update(s.crc.Sum32(), castagnoli(), []byte{s.typ})
	if c != s.wantCRC {
		return errCorrupt(fmt.Sprintf("table %d: big block checksum", s.t.num))
	}
	return nil
}

// Close releases the stream.
func (s *valueStream) Close() error {
	s.release()
	return nil
}

func (s *valueStream) release() {
	if s.in != nil {
		s.t.inflaters.Put(s.in)
		s.in = nil
	}
}
