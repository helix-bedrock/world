package store

import (
	"errors"
	"fmt"
	"io"
)

// Raw records are records as stored: compressed body and checksum, copied
// from one store to another without being decompressed or compressed again.
// Backups use them: a copy costs one read and one write of the stored bytes.

// RawHeader describes a stored record apart from its key.
type RawHeader struct {
	// Kind is what the writer said the record is.
	Kind byte
	// Comp is how the body is compressed.
	Comp byte
	// Size is the body's length decompressed, Stored its length on disk.
	Size   int64
	Stored int64
	// CRC is the CRC-32C of the stored body.
	CRC uint32
	// Round is the save round that wrote it, in the store it came from. A copy
	// gets the round of the commit that writes it.
	Round uint64
}

func (h recHeader) rawHeader() RawHeader {
	return RawHeader{Kind: h.kind, Comp: h.comp, Size: int64(h.raw), Stored: int64(h.stored), CRC: h.bodyCRC, Round: h.round} //nolint:gosec // record sizes
}

var errRawHeader = errors.New("store: raw record header is not valid")

func (h RawHeader) valid() bool {
	switch {
	case h.Kind == kindIndex, h.Size < 0, h.Stored < 0:
		return false
	case h.Comp == compNone:
		return h.Size == h.Stored
	case h.Comp == compZstd:
		return true
	}
	return false
}

// entry returns key's entry as of the snapshot and the open file holding it.
// The caller has acquired hf.
func (sn *Snapshot) entry(hf *hxFile, key []byte) (entry, file, bool) {
	e, f, ok := hf.find(key)
	if o, seen := sn.noted(hf, key); seen {
		e, ok = o.e, o.ok
		hf.mu.RLock()
		f = hf.f
		hf.mu.RUnlock()
	}
	return e, f, ok
}

// ReadRaw reads key's record as stored, as of the snapshot: its header, and
// its stored body in dst[:0]. The body's checksum is checked. For records of
// megabytes use OpenRaw.
func (sn *Snapshot) ReadRaw(id FileID, key, dst []byte) (RawHeader, []byte, error) {
	if err := sn.usable(); err != nil {
		return RawHeader{}, dst[:0], err
	}
	s := sn.s
	hf, err := s.fileOf(id)
	if err != nil {
		return RawHeader{}, dst[:0], err
	}
	if err := hf.acquire(s, s.cur.Load().round); err != nil {
		return RawHeader{}, dst[:0], err
	}
	defer hf.releaseRead()
	e, f, ok := sn.entry(hf, key)
	if !ok {
		return RawHeader{}, dst[:0], ErrNotFound
	}
	if e.size > maxDecoded {
		return RawHeader{}, dst[:0], fmt.Errorf("store: record of %d bytes is too big to read whole", e.size)
	}
	if uint64(cap(dst)) < e.size {
		dst = make([]byte, e.size)
	}
	buf := dst[:e.size]
	if _, err := f.ReadAt(buf, int64(e.off)); err != nil { //nolint:gosec // file offsets
		return RawHeader{}, dst[:0], fmt.Errorf("store: read %s: %w", hf.path, err)
	}
	h, body, err := checkRecord(buf, key, hf.path)
	if err != nil {
		return RawHeader{}, dst[:0], err
	}
	// The body sits after the header and key: move it to the front.
	n := copy(buf, body)
	return h.rawHeader(), buf[:n], nil
}

// OpenRaw opens key's stored body as a stream, as of the snapshot. Its
// checksum is checked when it has been read to the end. Close it.
func (sn *Snapshot) OpenRaw(id FileID, key []byte) (RawHeader, io.ReadCloser, error) {
	if err := sn.usable(); err != nil {
		return RawHeader{}, nil, err
	}
	s := sn.s
	hf, err := s.fileOf(id)
	if err != nil {
		return RawHeader{}, nil, err
	}
	if err := hf.acquire(s, s.cur.Load().round); err != nil {
		return RawHeader{}, nil, err
	}
	e, f, ok := sn.entry(hf, key)
	if !ok {
		hf.releaseRead()
		return RawHeader{}, nil, ErrNotFound
	}
	rs := &rawStream{hf: hf}
	if err := rs.open(f, e, key); err != nil {
		hf.releaseRead()
		return RawHeader{}, nil, err
	}
	return rs.h.rawHeader(), rs, nil
}

// rawStream reads one record's stored body.
type rawStream struct {
	hf     *hxFile
	body   *crcReader
	h      recHeader
	closed bool
}

func (rs *rawStream) open(f file, e entry, key []byte) error {
	head := uint64(recHeaderLen + len(key))
	if e.size < head {
		return errCorrupt(rs.hf.path, "record does not match its index entry")
	}
	buf := make([]byte, head)
	if _, err := f.ReadAt(buf, int64(e.off)); err != nil { //nolint:gosec // file offsets
		return fmt.Errorf("store: read %s: %w", rs.hf.path, err)
	}
	h, ok := parseHeader(buf)
	if !ok || h.size() != e.size || string(buf[recHeaderLen:]) != string(key) {
		return errCorrupt(rs.hf.path, "record does not match its index entry")
	}
	rs.h = h
	rs.body = &crcReader{r: io.NewSectionReader(f, int64(e.off+head), int64(h.stored))} //nolint:gosec // file offsets
	return nil
}

func (rs *rawStream) Read(p []byte) (int, error) {
	if rs.closed {
		return 0, ErrClosed
	}
	n, err := rs.body.Read(p)
	if errors.Is(err, io.EOF) {
		if rs.body.n != rs.h.stored || rs.body.crc != rs.h.bodyCRC {
			return n, errCorrupt(rs.hf.path, "record body checksum")
		}
		return n, io.EOF
	}
	if err != nil {
		return n, fmt.Errorf("store: read %s: %w", rs.hf.path, err)
	}
	return n, nil
}

func (rs *rawStream) Close() error {
	if !rs.closed {
		rs.closed = true
		rs.hf.releaseRead()
	}
	return nil
}

// PutRaw stores a record copied as stored: h from ReadRaw or OpenRaw, body its
// stored bytes. key and body are copied. The commit checks the body against
// h.CRC.
func (b *Batch) PutRaw(f FileID, key []byte, h RawHeader, body []byte) {
	o := b.newOp(f, h.Kind, key)
	o.valOff, o.valLen = len(b.arena), len(body)
	b.arena = append(b.arena, body...)
	o.raw, o.rawh = true, h
	b.ops = append(b.ops, o)
}

// PutRawStream stores a record copied as stored whose h.Stored bytes open
// reads during the commit.
func (b *Batch) PutRawStream(f FileID, key []byte, h RawHeader, open OpenFunc) {
	o := b.newOp(f, h.Kind, key)
	o.open, o.size = open, h.Stored
	o.raw, o.rawh = true, h
	b.ops = append(b.ops, o)
}

// appendRawRecord appends a record whose body is already stored, padded to
// recordAlign.
func appendRawRecord(dst []byte, key []byte, rh RawHeader, body []byte, round uint64) ([]byte, entry, error) {
	if int64(len(body)) != rh.Stored || crc32Of(body) != rh.CRC {
		return dst, entry{}, fmt.Errorf("%w: raw record body does not match its header", ErrCorrupt)
	}
	start := len(dst)
	dst = append(dst, make([]byte, recHeaderLen)...)
	dst = append(dst, key...)
	dst = append(dst, body...)
	h := recHeader{kind: rh.Kind, comp: rh.Comp, keyLen: len(key), round: round, stored: uint64(rh.Stored), raw: uint64(rh.Size), bodyCRC: rh.CRC} //nolint:gosec // checked sizes
	h.put(dst[start:], key)
	size := h.size()
	dst = append(dst, make([]byte, alignUp(size)-size)...)
	return dst, entry{off: uint64(start), size: size, raw: h.raw, round: round, kind: h.kind}, nil //nolint:gosec // lengths
}

// writeRawStream copies a stored body straight into free space.
func (s *Store) writeRawStream(hf *hxFile, o *op, key []byte, round uint64) (entry, error) {
	rc, err := o.open()
	if err != nil {
		return entry{}, fmt.Errorf("store: open stream for %s: %w", hf.path, err)
	}
	defer func() { _ = rc.Close() }()
	rh := o.rawh
	h := recHeader{kind: rh.Kind, comp: rh.Comp, keyLen: len(key), round: round, stored: uint64(rh.Stored), raw: uint64(rh.Size), bodyCRC: rh.CRC} //nolint:gosec // checked sizes
	size := alignUp(h.size())
	off := hf.sp.alloc(size)
	w := &s.w
	w.allocs = append(w.allocs, allocation{file: hf, ext: extent{off: off, size: size}})
	head := uint64(recHeaderLen + len(key))
	bw := &bodyWriter{f: hf.f, off: int64(off + head), limit: rh.Stored} //nolint:gosec // file offsets
	n, err := io.Copy(bw, io.LimitReader(rc, rh.Stored+1))
	if err != nil {
		return entry{}, fmt.Errorf("store: stream to %s: %w", hf.path, err)
	}
	if n != rh.Stored || bw.crc != rh.CRC {
		return entry{}, fmt.Errorf("%w: raw record body does not match its header", ErrCorrupt)
	}
	hdr := w.hdr[:head]
	copy(hdr[recHeaderLen:], key)
	h.put(hdr, key)
	if _, err := hf.f.WriteAt(hdr, int64(off)); err != nil { //nolint:gosec // file offsets
		return entry{}, fmt.Errorf("store: write %s: %w", hf.path, err)
	}
	return entry{off: off, size: h.size(), raw: h.raw, round: round, kind: h.kind}, nil
}
