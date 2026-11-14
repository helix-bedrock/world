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

// A table (LevelDB doc/table_format.md): data blocks, a filter block, a metaindex block
// naming the filter, an index block with one entry per data block (a key at or after the
// block's last key, and the block's handle) and a 48 byte footer with the metaindex and
// index handles and a magic number. Every block is followed by a 5 byte trailer: its
// compression type and a masked CRC-32C of the block and the type.

const (
	blockTrailerLen = 5
	footerLen       = 48
	tableMagic      = 0xdb4775248b80fb57
	// maxHandleLen is the most two varints of a block handle take.
	maxHandleLen = 2 * binary.MaxVarintLen64
)

// blockHandle is where a block sits in its table, its trailer not counted.
type blockHandle struct{ offset, size uint64 }

func (h blockHandle) append(dst []byte) []byte {
	dst = binary.AppendUvarint(dst, h.offset)
	return binary.AppendUvarint(dst, h.size)
}

func decodeHandle(b []byte) (blockHandle, []byte, error) {
	off, rest, err := readUvarint(b)
	if err != nil {
		return blockHandle{}, nil, errCorrupt("block handle")
	}
	size, rest, err := readUvarint(rest)
	if err != nil {
		return blockHandle{}, nil, errCorrupt("block handle")
	}
	return blockHandle{offset: off, size: size}, rest, nil
}

// tableOptions are how a table is written.
type tableOptions struct {
	// blockSize is the uncompressed size a data block is cut at.
	blockSize int
	// bigValue is the value size from which a value gets a data block of its own, so it is
	// streamed when read and never cached.
	bigValue int
}

// tableWriter writes one table.
type tableWriter struct {
	w      *bufio.Writer
	opts   tableOptions
	offset uint64

	data, index blockBuilder
	filter      filterBuilder
	deflate     deflater

	pendingIndex  bool
	pendingHandle blockHandle
	last          []byte
	smallest      []byte
	entries       int
	handleBuf     []byte
	trailer       [blockTrailerLen]byte
	err           error
}

func newTableWriter(w io.Writer, opts tableOptions) *tableWriter {
	t := &tableWriter{w: bufio.NewWriterSize(w, 64<<10), opts: opts}
	t.data.interval = dataRestartInterval
	t.index.interval = indexRestartInterval
	t.filter.startBlock(0)
	return t
}

// add appends an entry. Internal keys must come in order.
func (t *tableWriter) add(ikey, value []byte) error {
	if t.err != nil {
		return t.err
	}
	if len(value) >= t.opts.bigValue {
		return t.addOwnBlock(ikey, value)
	}
	t.addIndexEntry(ikey)
	t.filter.addKey(userKey(ikey))
	t.data.add(ikey, value)
	t.noteKey(ikey)
	if t.data.size() >= t.opts.blockSize {
		t.flushBlock()
	}
	return t.err
}

// addOwnBlock writes a big value in a data block of its own.
func (t *tableWriter) addOwnBlock(ikey, value []byte) error {
	t.flushBlock()
	t.addIndexEntry(ikey)
	t.filter.addKey(userKey(ikey))
	t.data.add(ikey, value)
	t.noteKey(ikey)
	t.flushBlock()
	return t.err
}

// addStream appends a value read from src, size bytes, in a data block of its own,
// compressing it as it is read so it is never held whole.
func (t *tableWriter) addStream(ikey []byte, src io.Reader, size int64) error {
	if t.err != nil {
		return t.err
	}
	t.flushBlock()
	t.addIndexEntry(ikey)
	t.filter.addKey(userKey(ikey))
	t.noteKey(ikey)
	h, err := t.writeStreamBlock(ikey, src, size)
	if err != nil {
		t.err = err
		return err
	}
	t.finishBlock(h)
	return nil
}

// writeStreamBlock writes a block of the one entry (ikey, value from src) as raw deflate.
func (t *tableWriter) writeStreamBlock(ikey []byte, src io.Reader, size int64) (blockHandle, error) {
	cw := &crcWriter{w: t.w, crc: crc32.New(castagnoli())}
	if err := t.deflate.reset(cw); err != nil {
		return blockHandle{}, err
	}
	head := binary.AppendUvarint(nil, 0)
	head = binary.AppendUvarint(head, uint64(len(ikey)))
	head = binary.AppendUvarint(head, uint64(size)) //nolint:gosec // size is never negative here
	head = append(head, ikey...)
	if _, err := t.deflate.w.Write(head); err != nil {
		return blockHandle{}, fmt.Errorf("lsm: write stream block: %w", err)
	}
	n, err := io.CopyN(t.deflate.w, src, size)
	if err != nil {
		return blockHandle{}, fmt.Errorf("lsm: stream value: %d of %d bytes: %w", n, size, err)
	}
	var tail [8]byte
	binary.LittleEndian.PutUint32(tail[0:], 0) // one restart point at 0
	binary.LittleEndian.PutUint32(tail[4:], 1)
	if _, err := t.deflate.w.Write(tail[:]); err != nil {
		return blockHandle{}, fmt.Errorf("lsm: write stream block: %w", err)
	}
	if err := t.deflate.w.Close(); err != nil {
		return blockHandle{}, fmt.Errorf("lsm: write stream block: %w", err)
	}
	h := blockHandle{offset: t.offset, size: uint64(cw.n)} //nolint:gosec // count
	if err := t.writeTrailer(blockDeflate, cw.crc.Sum32()); err != nil {
		return blockHandle{}, err
	}
	t.offset += uint64(cw.n) + blockTrailerLen //nolint:gosec // count
	return h, nil
}

// crcWriter counts and checksums what goes through it.
type crcWriter struct {
	w   io.Writer
	crc hash.Hash32
	n   int64
}

func (c *crcWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.crc.Write(p[:n])
	c.n += int64(n)
	return n, err
}

// addIndexEntry adds the index entry of the block before ikey, if one is pending.
func (t *tableWriter) addIndexEntry(ikey []byte) {
	if !t.pendingIndex {
		return
	}
	t.handleBuf = t.pendingHandle.append(t.handleBuf[:0])
	t.index.add(t.last, t.handleBuf)
	t.pendingIndex = false
}

func (t *tableWriter) noteKey(ikey []byte) {
	if t.entries == 0 {
		t.smallest = append(t.smallest[:0], ikey...)
	}
	t.last = append(t.last[:0], ikey...)
	t.entries++
}

// flushBlock writes the data block being built, if it holds anything.
func (t *tableWriter) flushBlock() {
	if t.err != nil || t.data.entries == 0 {
		return
	}
	h, err := t.writeBlock(t.data.finish(), true)
	t.data.reset()
	if err != nil {
		t.err = err
		return
	}
	t.finishBlock(h)
}

// finishBlock records a written data block: its index entry is added with the next key.
func (t *tableWriter) finishBlock(h blockHandle) {
	t.pendingIndex, t.pendingHandle = true, h
	t.filter.startBlock(t.offset)
}

// writeBlock writes a block with its trailer, compressed when that saves at least an
// eighth, as LevelDB does.
func (t *tableWriter) writeBlock(raw []byte, compress bool) (blockHandle, error) {
	data, typ := raw, byte(blockNone)
	if compress {
		c, err := t.deflate.compress(raw)
		if err != nil {
			return blockHandle{}, err
		}
		if len(c) < len(raw)-len(raw)/8 {
			data, typ = c, blockDeflate
		}
	}
	if _, err := t.w.Write(data); err != nil {
		return blockHandle{}, fmt.Errorf("lsm: write table: %w", err)
	}
	c := crc32.Checksum(data, castagnoli())
	if err := t.writeTrailer(typ, c); err != nil {
		return blockHandle{}, err
	}
	h := blockHandle{offset: t.offset, size: uint64(len(data))}
	t.offset += uint64(len(data)) + blockTrailerLen
	return h, nil
}

// writeTrailer writes a block trailer; c is the checksum of the block data so far.
func (t *tableWriter) writeTrailer(typ byte, c uint32) error {
	t.trailer[0] = typ
	c = crc32.Update(c, castagnoli(), t.trailer[:1])
	binary.LittleEndian.PutUint32(t.trailer[1:], maskCRC(c))
	if _, err := t.w.Write(t.trailer[:]); err != nil {
		return fmt.Errorf("lsm: write table: %w", err)
	}
	return nil
}

// finish writes the filter, metaindex and index blocks and the footer, and flushes. It
// returns the table's size.
func (t *tableWriter) finish() (uint64, error) {
	t.flushBlock()
	if t.err != nil {
		return 0, t.err
	}
	if t.entries == 0 {
		return 0, errors.New("lsm: empty table")
	}
	filterHandle, err := t.writeBlock(t.filter.finish(), false)
	if err != nil {
		return 0, err
	}
	var meta blockBuilder
	meta.interval = indexRestartInterval
	meta.add([]byte("filter."+filterName), filterHandle.append(nil))
	metaHandle, err := t.writeBlock(meta.finish(), false)
	if err != nil {
		return 0, err
	}
	if t.pendingIndex {
		t.index.add(t.last, t.pendingHandle.append(t.handleBuf[:0]))
		t.pendingIndex = false
	}
	indexHandle, err := t.writeBlock(t.index.finish(), false)
	if err != nil {
		return 0, err
	}
	if err := t.writeFooter(metaHandle, indexHandle); err != nil {
		return 0, err
	}
	if err := t.w.Flush(); err != nil {
		return 0, fmt.Errorf("lsm: write table: %w", err)
	}
	return t.offset, nil
}

func (t *tableWriter) writeFooter(meta, index blockHandle) error {
	footer := make([]byte, footerLen)
	b := index.append(meta.append(footer[:0]))
	_ = b // the handles are written in place; the rest stays zero padding
	binary.LittleEndian.PutUint64(footer[footerLen-8:], tableMagic)
	if _, err := t.w.Write(footer); err != nil {
		return fmt.Errorf("lsm: write table: %w", err)
	}
	t.offset += footerLen
	return nil
}
