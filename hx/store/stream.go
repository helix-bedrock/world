package store

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/klauspost/compress/zstd"
)

// GetStream returns key's body as a stream and its length, for records too
// big to hold whole. The body's checksum is checked when it has been read to
// the end. The stream keeps the record's space from being reused until it is
// closed: close it.
func (s *Store) GetStream(id FileID, key []byte) (io.ReadCloser, int64, error) {
	if s.closed.Load() {
		return nil, 0, ErrClosed
	}
	hf, err := s.fileOf(id)
	if err != nil {
		return nil, 0, err
	}
	g := s.pin()
	if err := hf.acquire(s, g.round); err != nil {
		s.unpin(g)
		return nil, 0, err
	}
	e, f, ok := hf.find(key)
	if !ok {
		hf.releaseRead()
		s.unpin(g)
		return nil, 0, ErrNotFound
	}
	rs := &recordStream{s: s, hf: hf, g: g}
	if err := rs.open(f, e, key); err != nil {
		_ = rs.Close()
		return nil, 0, err
	}
	return rs, int64(e.raw), nil //nolint:gosec // record sizes
}

// recordStream reads one record's body, decompressing as it goes.
type recordStream struct {
	s    *Store
	hf   *hxFile
	g    *generation
	body *crcReader
	dec  *zstd.Decoder
	r    io.Reader
	h    recHeader
	read uint64
	done bool
}

// open reads and checks the record's header and key, then sets up the body.
func (rs *recordStream) open(f file, e entry, key []byte) error {
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
	switch h.comp {
	case compNone:
		rs.r = rs.body
	case compZstd:
		dec, err := rs.s.codec.streamDecoder(rs.body)
		if err != nil {
			return err
		}
		rs.dec, rs.r = dec, dec
	default:
		return errCorrupt(rs.hf.path, "unknown compression")
	}
	return nil
}

func (rs *recordStream) Read(p []byte) (int, error) {
	if rs.r == nil {
		return 0, ErrClosed
	}
	if rs.read == rs.h.raw {
		return 0, rs.finish()
	}
	if left := rs.h.raw - rs.read; uint64(len(p)) > left {
		p = p[:left]
	}
	n, err := rs.r.Read(p)
	rs.read += uint64(n) //nolint:gosec // n is non-negative
	if errors.Is(err, io.EOF) {
		if rs.read != rs.h.raw {
			return n, errCorrupt(rs.hf.path, "record body cut short")
		}
		err = nil
	}
	if err != nil {
		return n, fmt.Errorf("store: read %s: %w", rs.hf.path, err)
	}
	return n, nil
}

// finish checks, once the body has been read, that every stored byte was
// read and matches the checksum.
func (rs *recordStream) finish() error {
	if rs.done {
		return io.EOF
	}
	rs.done = true
	if _, err := io.Copy(io.Discard, rs.body); err != nil {
		return fmt.Errorf("store: read %s: %w", rs.hf.path, err)
	}
	if rs.body.n != rs.h.stored || rs.body.crc != rs.h.bodyCRC {
		return errCorrupt(rs.hf.path, "record body checksum")
	}
	return io.EOF
}

// Close releases the stream and the space it kept.
func (rs *recordStream) Close() error {
	if rs.g == nil {
		return nil
	}
	if rs.dec != nil {
		rs.s.codec.putStreamDecoder(rs.dec)
		rs.dec = nil
	}
	rs.r = nil
	rs.hf.releaseRead()
	rs.s.unpin(rs.g)
	rs.g = nil
	return nil
}

// crcReader checksums what it reads.
type crcReader struct {
	r   io.Reader
	crc uint32
	n   uint64
}

func (c *crcReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.crc = crc32.Update(c.crc, castagnoli(), p[:n])
	c.n += uint64(n) //nolint:gosec // n is non-negative
	return n, err
}
