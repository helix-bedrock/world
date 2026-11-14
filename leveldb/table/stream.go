package table

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/helix-bedrock/world/leveldb/errors"
	"github.com/helix-bedrock/world/leveldb/iterator"
	"github.com/helix-bedrock/world/leveldb/opt"
	"github.com/helix-bedrock/world/leveldb/util"
)

// Helix: big data blocks are streamed, never read whole. A Bedrock chunk keeps all its block
// entities in one value, and a chunk of chests full of books makes that value hundreds of
// megabytes: one block that size. Read whole, it took its compressed size, its inflated size
// and a copy of the value at once. A streamed block is inflated from the file as its
// entries are walked, and a value is only read into memory when a caller asks for it whole
// (Value); ValueStream and FindStream hand it out as a stream instead.

// bigBlockMin is the smallest stored data block that is streamed. Smaller ones are read
// whole and may be cached, as upstream does.
const bigBlockMin = 1 << 20

// streamBufSize is the read buffer of a streamed block, on both sides of the inflater.
const streamBufSize = 64 << 10

// bigBlock is the checked layout of a big data block.
type bigBlock struct {
	blockType byte
	// entriesEnd is where its entries end in the inflated block: the restart points follow.
	entriesEnd int64
}

// bigBlockOf returns the layout of the big block at bh, reading it through once the first
// time to check its checksum and measure it. Snappy blocks cannot be streamed: false.
func (r *Reader) bigBlockOf(bh blockHandle) (bigBlock, bool, error) {
	if v, ok := r.bigBlocks.Load(bh.offset); ok {
		b, _ := v.(bigBlock)
		return b, true, nil
	}
	var trailer [blockTrailerLen]byte
	if _, err := r.reader.ReadAt(trailer[:], int64(bh.offset+bh.length)); err != nil && err != io.EOF {
		return bigBlock{}, false, err
	}
	b := bigBlock{blockType: trailer[0]}
	switch b.blockType {
	case blockTypeNoCompression, blockTypeZlibCompression, blockTypeZlibRawCompression:
	default:
		return bigBlock{}, false, nil
	}
	crc := &crcReader{r: io.NewSectionReader(r.reader, int64(bh.offset), int64(bh.length))}
	src, done, err := r.inflateStream(bufio.NewReaderSize(crc, streamBufSize), b.blockType)
	if err != nil {
		return bigBlock{}, false, r.newErrCorruptedBH(bh, err.Error())
	}
	size, last, err := measure(src)
	done()
	if err != nil {
		return bigBlock{}, false, r.newErrCorruptedBH(bh, err.Error())
	}
	if _, err := io.Copy(io.Discard, crc); err != nil {
		return bigBlock{}, false, err
	}
	if want, got := binary.LittleEndian.Uint32(trailer[1:]), crc.crc.Update(trailer[:1]).Value(); want != got {
		return bigBlock{}, false, r.newErrCorruptedBH(bh, fmt.Sprintf("checksum mismatch, want=%#x got=%#x", want, got))
	}
	restarts := int64(binary.LittleEndian.Uint32(last[:]))
	b.entriesEnd = size - (restarts+1)*4
	if size < 4 || b.entriesEnd < 0 {
		return bigBlock{}, false, r.newErrCorruptedBH(bh, "bad restart points")
	}
	r.bigBlocks.Store(bh.offset, b)
	return b, true, nil
}

// measure reads src to its end and returns its size and last four bytes.
func measure(src io.Reader) (int64, [4]byte, error) {
	var scratch [32 << 10]byte
	var last [4]byte
	var n int64
	for {
		m, err := src.Read(scratch[:])
		if m >= 4 {
			copy(last[:], scratch[m-4:m])
		} else if m > 0 {
			copy(last[:], append(last[m:], scratch[:m]...))
		}
		n += int64(m)
		if err == io.EOF {
			return n, last, nil
		}
		if err != nil {
			return 0, last, err
		}
	}
}

// inflateStream returns src inflated as blockType, and a func that gives the inflater back.
func (r *Reader) inflateStream(src *bufio.Reader, blockType byte) (io.Reader, func(), error) {
	if blockType == blockTypeNoCompression {
		return src, func() {}, nil
	}
	inf, ok := r.inflaters.Get().(*inflater)
	if !ok {
		inf = new(inflater)
	}
	rd, err := inf.resetReader(src, blockType)
	if err != nil {
		r.inflaters.Put(inf)
		return nil, nil, err
	}
	return rd, func() { r.inflaters.Put(inf) }, nil
}

// crcReader computes the block checksum of what it reads.
type crcReader struct {
	r   io.Reader
	crc util.CRC
}

func (c *crcReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.crc = c.crc.Update(p[:n])
	return n, err
}

// streamIter walks a big data block's entries while inflating it from the file. It only goes
// forward; Last and Prev read the block whole and go on from there.
type streamIter struct {
	tr       *Reader
	bh       blockHandle
	layout   bigBlock
	slice    *util.Range
	releaser util.Releaser

	src  *bufio.Reader // the block's entries, inflated
	done func()        // closes the inflater of src

	key           []byte
	valLen, left  int64 // the value's size and how much of it is still to read
	value         []byte
	dir           dir
	err           error
	whole         iterator.Iterator // the block read whole, after Last or Prev
	streamedValue bool
}

// newStreamIter returns an iterator streaming the big block at bh, or nil if it cannot be
// streamed.
func (r *Reader) newStreamIter(bh blockHandle, slice *util.Range) iterator.Iterator {
	layout, ok, err := r.bigBlockOf(bh)
	if err != nil {
		return iterator.NewEmptyIterator(err)
	}
	if !ok {
		return nil
	}
	return &streamIter{tr: r, bh: bh, layout: layout, slice: slice, dir: dirSOI}
}

// open starts reading the block's entries from the beginning.
func (i *streamIter) open() bool {
	i.closeStream()
	sec := io.NewSectionReader(i.tr.reader, int64(i.bh.offset), int64(i.bh.length))
	rd, done, err := i.tr.inflateStream(bufio.NewReaderSize(sec, streamBufSize), i.layout.blockType)
	if err != nil {
		i.fail(i.tr.newErrCorruptedBH(i.bh, err.Error()))
		return false
	}
	i.src = bufio.NewReaderSize(io.LimitReader(rd, i.layout.entriesEnd), streamBufSize)
	i.done = done
	i.key, i.value, i.left, i.valLen = i.key[:0], nil, 0, 0
	return true
}

func (i *streamIter) closeStream() {
	if i.done != nil {
		i.done()
		i.done = nil
	}
	i.src = nil
}

func (i *streamIter) fail(err error) {
	i.err = err
	i.closeStream()
	i.key, i.value = nil, nil
}

// next reads the next entry's key, skipping what is left of the current value.
func (i *streamIter) next() bool {
	if i.left > 0 {
		if _, err := io.CopyN(io.Discard, i.src, i.left); err != nil {
			i.fail(i.tr.newErrCorruptedBH(i.bh, "value cut short"))
			return false
		}
	}
	i.value, i.left, i.streamedValue = nil, 0, false
	if _, err := i.src.Peek(1); err == io.EOF {
		i.dir = dirEOI
		i.closeStream()
		return false
	}
	shared, err1 := binary.ReadUvarint(i.src)
	unshared, err2 := binary.ReadUvarint(i.src)
	valLen, err3 := binary.ReadUvarint(i.src)
	if err1 != nil || err2 != nil || err3 != nil || shared > uint64(len(i.key)) || unshared > uint64(i.layout.entriesEnd) || valLen > uint64(i.layout.entriesEnd) {
		i.fail(i.tr.newErrCorruptedBH(i.bh, "bad entry in block"))
		return false
	}
	n := int(shared)
	i.key = append(i.key[:n], make([]byte, unshared)...)
	if _, err := io.ReadFull(i.src, i.key[n:]); err != nil {
		i.fail(i.tr.newErrCorruptedBH(i.bh, "bad entry in block"))
		return false
	}
	i.valLen, i.left = int64(valLen), int64(valLen)
	i.dir = dirForward
	if i.slice != nil && i.slice.Limit != nil && i.tr.cmp.Compare(i.key, i.slice.Limit) >= 0 {
		i.dir = dirEOI
		i.closeStream()
		return false
	}
	return true
}

func (i *streamIter) ok() bool {
	if i.err != nil {
		return false
	}
	if i.dir == dirReleased {
		i.err = ErrIterReleased
		return false
	}
	return true
}

func (i *streamIter) First() bool {
	if i.whole != nil {
		return i.whole.First()
	}
	if !i.ok() || !i.open() {
		return false
	}
	if i.slice != nil && i.slice.Start != nil {
		return i.seekForward(i.slice.Start)
	}
	return i.next()
}

func (i *streamIter) Seek(key []byte) bool {
	if i.whole != nil {
		return i.whole.Seek(key)
	}
	if !i.ok() {
		return false
	}
	if i.slice != nil && i.slice.Start != nil && i.tr.cmp.Compare(key, i.slice.Start) < 0 {
		key = i.slice.Start
	}
	// Going on from here is only right while the current key is before key.
	if i.dir != dirForward || i.tr.cmp.Compare(i.key, key) >= 0 {
		if !i.open() {
			return false
		}
	}
	return i.seekForward(key)
}

// seekForward moves to the first entry at or after key from where the stream is.
func (i *streamIter) seekForward(key []byte) bool {
	for i.next() {
		if i.tr.cmp.Compare(i.key, key) >= 0 {
			return true
		}
	}
	return false
}

func (i *streamIter) Next() bool {
	if i.whole != nil {
		return i.whole.Next()
	}
	if !i.ok() {
		return false
	}
	switch i.dir {
	case dirSOI:
		return i.First()
	case dirEOI:
		return false
	}
	return i.next()
}

// readWhole switches to the block read whole, for going backwards.
func (i *streamIter) readWhole() bool {
	if !i.ok() {
		return false
	}
	i.closeStream()
	b, err := i.tr.readBlock(i.bh, i.tr.verifyChecksum)
	if err != nil {
		i.fail(err)
		return false
	}
	i.whole = i.tr.newBlockIter(b, b, i.slice, false)
	return true
}

func (i *streamIter) Last() bool {
	if i.whole == nil && !i.readWhole() {
		return false
	}
	return i.whole.Last()
}

func (i *streamIter) Prev() bool {
	if i.whole != nil {
		return i.whole.Prev()
	}
	if !i.ok() {
		return false
	}
	var at []byte
	if i.dir == dirForward {
		at = bytes.Clone(i.key)
	}
	if !i.readWhole() {
		return false
	}
	if at == nil {
		return i.whole.Last()
	}
	if !i.whole.Seek(at) {
		return i.whole.Last()
	}
	return i.whole.Prev()
}

func (i *streamIter) Key() []byte {
	if i.whole != nil {
		return i.whole.Key()
	}
	if i.err != nil || i.dir != dirForward {
		return nil
	}
	return i.key
}

// Value reads the value whole, into a buffer of its own that the caller may keep.
func (i *streamIter) Value() []byte {
	if i.whole != nil {
		return i.whole.Value()
	}
	if i.err != nil || i.dir != dirForward {
		return nil
	}
	if i.value != nil {
		return i.value
	}
	if i.streamedValue {
		i.fail(errors.New("leveldb/table: value already streamed"))
		return nil
	}
	v := make([]byte, i.valLen)
	if _, err := io.ReadFull(i.src, v); err != nil {
		i.fail(i.tr.newErrCorruptedBH(i.bh, "value cut short"))
		return nil
	}
	i.value, i.left = v, 0
	return v
}

// ValueStream hands out the value at the position as a stream (iterator.ValueStreamer).
func (i *streamIter) ValueStream() (io.Reader, int64, bool) {
	if i.whole != nil || i.err != nil || i.dir != dirForward || i.value != nil || i.left != i.valLen {
		return nil, 0, false
	}
	i.streamedValue = true
	return &valueReader{i: i}, i.valLen, true
}

// ValueSize is the size of the value at the position (iterator.ValueSizer).
func (i *streamIter) ValueSize() (int64, bool) {
	if i.whole != nil || i.err != nil || i.dir != dirForward {
		return 0, false
	}
	return i.valLen, true
}

// valueReader reads the value at a streamIter's position.
type valueReader struct{ i *streamIter }

func (v *valueReader) Read(p []byte) (int, error) {
	i := v.i
	if i.src == nil || i.dir != dirForward {
		return 0, errors.New("leveldb/table: iterator moved")
	}
	if i.left == 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > i.left {
		p = p[:i.left]
	}
	n, err := i.src.Read(p)
	i.left -= int64(n)
	if err == io.EOF && i.left > 0 {
		err = io.ErrUnexpectedEOF
	} else if err == io.EOF {
		err = nil
	}
	return n, err
}

func (i *streamIter) Valid() bool {
	if i.whole != nil {
		return i.whole.Valid()
	}
	return i.err == nil && i.dir == dirForward
}

func (i *streamIter) Error() error {
	if i.whole != nil {
		if err := i.whole.Error(); err != nil {
			return err
		}
	}
	return i.err
}

func (i *streamIter) SetReleaser(releaser util.Releaser) {
	if i.dir == dirReleased {
		return
	}
	i.releaser = releaser
}

func (i *streamIter) Release() {
	if i.dir == dirReleased {
		return
	}
	i.closeStream()
	if i.whole != nil {
		i.whole.Release()
		i.whole = nil
	}
	if i.releaser != nil {
		i.releaser.Release()
		i.releaser = nil
	}
	i.dir = dirReleased
	i.key, i.value = nil, nil
}

// FindStream is Find with the value as a stream: a big value is inflated from the file as it
// is read rather than held whole. The reader must be closed, and is valid while the Reader
// is.
func (r *Reader) FindStream(key []byte, filtered bool, ro *opt.ReadOptions) (rkey []byte, value io.ReadCloser, size int64, err error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.err != nil {
		return nil, nil, 0, r.err
	}
	indexBlock, rel, err := r.getIndexBlock(true)
	if err != nil {
		return nil, nil, 0, err
	}
	defer rel.Release()
	index := r.newBlockIter(indexBlock, nil, nil, true)
	defer index.Release()
	if !index.Seek(key) {
		if err = index.Error(); err == nil {
			err = ErrNotFound
		}
		return nil, nil, 0, err
	}
	dataBH, n := decodeBlockHandle(index.Value())
	if n == 0 {
		return nil, nil, 0, r.newErrCorruptedBH(r.indexBH, "bad data block handle")
	}
	if filtered && r.filter != nil {
		if fb, frel, ferr := r.getFilterBlock(true); ferr == nil {
			ok := fb.contains(r.filter, dataBH.offset, key)
			frel.Release()
			if !ok {
				return nil, nil, 0, ErrNotFound
			}
		} else if !errors.IsCorrupted(ferr) {
			return nil, nil, 0, ferr
		}
	}
	data := r.getDataIter(dataBH, nil, r.verifyChecksum, !ro.GetDontFillCache())
	if !data.Seek(key) {
		data.Release()
		if err = data.Error(); err != nil {
			return nil, nil, 0, err
		}
		// The nearest greater key is the first of the next block.
		if !index.Next() {
			if err = index.Error(); err == nil {
				err = ErrNotFound
			}
			return nil, nil, 0, err
		}
		if dataBH, n = decodeBlockHandle(index.Value()); n == 0 {
			return nil, nil, 0, r.newErrCorruptedBH(r.indexBH, "bad data block handle")
		}
		data = r.getDataIter(dataBH, nil, r.verifyChecksum, !ro.GetDontFillCache())
		if !data.Next() {
			data.Release()
			if err = data.Error(); err == nil {
				err = ErrNotFound
			}
			return nil, nil, 0, err
		}
	}
	rkey = bytes.Clone(data.Key())
	if rd, size, ok := iterator.ValueStream(data); ok {
		return rkey, &iterReadCloser{Reader: rd, it: data}, size, nil
	}
	v := bytes.Clone(data.Value())
	data.Release()
	return rkey, io.NopCloser(bytes.NewReader(v)), int64(len(v)), nil
}

// iterReadCloser releases the iterator its value streams from when closed.
type iterReadCloser struct {
	io.Reader
	it iterator.Iterator
}

func (c *iterReadCloser) Close() error {
	err := c.it.Error()
	c.it.Release()
	return err
}
