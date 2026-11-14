package lsm

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
)

// Big blocks (stored size of bigBlock or more) are never decompressed whole: they are
// streamed entry by entry. A block is a run of entries (shared key length, unshared key
// length, value length, unshared key bytes, value), then its restart points (uint32s)
// and their count (uint32).
//
// LevelDB's table builder (BDS's, goleveldb's and ours) closes a block as soon as it
// reaches its size target, so a value of bigBlock bytes or more is always the last entry
// of its block, but small entries may come before it: BDS writes blocks of a few small
// sub chunks followed by a block entity value of hundreds of megabytes. Our own writer
// gives every big value a block of its own.

// trailerPeek is how far ahead the reader looks for the end of the entries. The restart
// points of a block take 4 bytes per restart (one per 16 entries by default), far less
// than this for any block a writer makes.
const trailerPeek = 64 << 10

// bigReader walks the entries of a big block, inflating as it goes. Each value is read
// only if asked for; skipped values are inflated and dropped. The block's checksum is
// checked once its end is reached.
type bigReader struct {
	t       *table
	in      *inflater
	crc     hash.Hash32
	typ     byte
	raw     *bufio.Reader // stored bytes, checksummed as they are read
	br      *bufio.Reader // the decompressed block
	wantCRC uint32

	key   []byte
	vlen  int64
	vleft int64
	// index counts the entries read: 1 on the first.
	index int
	end   bool
	err   error
}

// openBig opens a big block and moves to its first entry. ok is false for a block with
// no entries.
func (t *table) openBig(h blockHandle) (b *bigReader, ok bool, err error) {
	if h.offset > t.size || h.size > t.size-h.offset || t.size-h.offset-h.size < blockTrailerLen {
		return nil, false, errCorrupt(fmt.Sprintf("table %d: block handle out of range", t.num))
	}
	var trailer [blockTrailerLen]byte
	if _, err := t.f.ReadAt(trailer[:], int64(h.offset+h.size)); err != nil { //nolint:gosec // in range
		return nil, false, fmt.Errorf("lsm: read table %d: %w", t.num, err)
	}
	b = &bigReader{t: t, in: t.getInflater(), crc: crc32.New(castagnoli()), typ: trailer[0]}
	b.wantCRC = unmaskCRC(binary.LittleEndian.Uint32(trailer[1:]))
	sec := io.NewSectionReader(t.f, int64(h.offset), int64(h.size)) //nolint:gosec // in range
	b.raw = bufio.NewReaderSize(io.TeeReader(sec, b.crc), 64<<10)
	r, err := b.in.reader(b.raw, b.typ)
	if err != nil {
		b.Close()
		return nil, false, err
	}
	b.br = bufio.NewReaderSize(r, 2*trailerPeek)
	ok, err = b.next()
	if err != nil {
		b.Close()
		return nil, false, err
	}
	return b, ok, nil
}

// next moves to the next entry, skipping what is left of the current value. It returns
// false at the end of the entries, once the block's checksum has been checked.
func (b *bigReader) next() (bool, error) {
	if b.err != nil || b.end {
		return false, b.err
	}
	if b.vleft > 0 {
		if _, err := b.br.Discard(int(b.vleft)); err != nil {
			return b.fail(errCorrupt(fmt.Sprintf("table %d: big block cut short", b.t.num)))
		}
		b.vleft = 0
	}
	if b.atTrailer() {
		b.end = true
		if err := b.finish(); err != nil {
			return b.fail(err)
		}
		return false, nil
	}
	shared, err1 := binary.ReadUvarint(b.br)
	unshared, err2 := binary.ReadUvarint(b.br)
	vlen, err3 := binary.ReadUvarint(b.br)
	if err := errors.Join(err1, err2, err3); err != nil || shared > uint64(len(b.key)) || unshared > 1<<20 || vlen > 1<<62 {
		return b.fail(errCorrupt(fmt.Sprintf("table %d: big block entry", b.t.num)))
	}
	b.key = b.key[:shared]
	for range unshared {
		c, err := b.br.ReadByte()
		if err != nil {
			return b.fail(errCorrupt(fmt.Sprintf("table %d: big block key", b.t.num)))
		}
		b.key = append(b.key, c)
	}
	b.vlen, b.vleft = int64(vlen), int64(vlen) //nolint:gosec // checked above
	b.index++
	return true, nil
}

// atTrailer reports whether what is left of the block is its restart points: the bytes
// up to the end are exactly 4 per restart plus the count.
func (b *bigReader) atTrailer() bool {
	p, _ := b.br.Peek(trailerPeek)
	if len(p) >= trailerPeek || len(p) < 4 {
		return false
	}
	n := uint64(binary.LittleEndian.Uint32(p[len(p)-4:]))
	return uint64(len(p)) == 4*(n+1)
}

// value reads the current entry's value into dst, which it grows if needed.
func (b *bigReader) value(dst []byte) ([]byte, error) {
	if b.vleft != b.vlen {
		return dst[:0], errors.New("lsm: big block value already read")
	}
	if int64(cap(dst)) < b.vlen {
		dst = make([]byte, b.vlen)
	}
	dst = dst[:b.vlen]
	if _, err := io.ReadFull(b.br, dst); err != nil {
		_, err = b.fail(errCorrupt(fmt.Sprintf("table %d: big block cut short", b.t.num)))
		return dst[:0], err
	}
	b.vleft = 0
	return dst, nil
}

// single reports whether the current entry is alone in its block, so the block can be
// copied as stored: it is the first entry and its value is big, so it is also the last.
func (b *bigReader) single() bool {
	return b.index == 1 && b.vlen >= int64(b.t.bigBlock) //nolint:gosec // small
}

// finish reads the rest of the block, so every stored byte is checksummed, and checks it.
func (b *bigReader) finish() error {
	if _, err := io.Copy(io.Discard, b.br); err != nil {
		return fmt.Errorf("lsm: read table %d: %w", b.t.num, err)
	}
	if _, err := io.Copy(io.Discard, b.raw); err != nil {
		return fmt.Errorf("lsm: read table %d: %w", b.t.num, err)
	}
	if crc32.Update(b.crc.Sum32(), castagnoli(), []byte{b.typ}) != b.wantCRC {
		return errCorrupt(fmt.Sprintf("table %d: big block checksum", b.t.num))
	}
	return nil
}

func (b *bigReader) fail(err error) (bool, error) {
	b.err = err
	return false, err
}

// Close releases the reader.
func (b *bigReader) Close() {
	if b.in != nil {
		b.t.inflaters.Put(b.in)
		b.in = nil
	}
}

// valueStream reads one value of a big block, inflating as it goes. When the value has
// been read to its end, the rest of the block is read through and its checksum checked.
type valueStream struct {
	b    *bigReader
	size int64
	done bool
}

// Size is the value's length.
func (s *valueStream) Size() int64 { return s.size }

// Read reads the value.
func (s *valueStream) Read(p []byte) (int, error) {
	b := s.b
	if b.vleft == 0 {
		if !s.done {
			s.done = true
			if _, err := b.next(); err != nil {
				return 0, err
			}
			// Entries after a big value cannot exist; the block is read to its end.
			for !b.end {
				if _, err := b.next(); err != nil {
					return 0, err
				}
			}
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.vleft {
		p = p[:b.vleft]
	}
	n, err := b.br.Read(p)
	b.vleft -= int64(n)
	if errors.Is(err, io.EOF) {
		if b.vleft > 0 {
			return n, errCorrupt("big block cut short")
		}
		err = nil
	}
	return n, err
}

// Close releases the stream.
func (s *valueStream) Close() error {
	s.b.Close()
	return nil
}
