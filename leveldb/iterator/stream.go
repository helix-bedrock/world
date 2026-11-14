package iterator

import "io"

// ValueStreamer is an iterator that can hand out the value at its position as a stream,
// valid until the iterator moves, with its size; false means use Value. Table iterators do
// for values in big blocks, and the indexed and merged iterators pass it on (Helix: a value
// of hundreds of megabytes is then never held whole).
type ValueStreamer interface {
	ValueStream() (io.Reader, int64, bool)
}

// ValueStream returns the value at it's position as a stream if it can give one.
func ValueStream(it Iterator) (io.Reader, int64, bool) {
	if vs, ok := it.(ValueStreamer); ok {
		return vs.ValueStream()
	}
	return nil, 0, false
}

// ValueStream passes on the data iterator's stream (ValueStreamer).
func (i *indexedIterator) ValueStream() (io.Reader, int64, bool) {
	if i.data == nil {
		return nil, 0, false
	}
	return ValueStream(i.data)
}

// ValueStream passes on the current iterator's stream (ValueStreamer).
func (i *mergedIterator) ValueStream() (io.Reader, int64, bool) {
	if i.err != nil || i.dir <= dirEOI {
		return nil, 0, false
	}
	return ValueStream(i.iters[i.index])
}

// ValueSizer is an iterator that can tell the size of the value at its position without
// reading it. Table iterators do for values in big blocks.
type ValueSizer interface {
	ValueSize() (int64, bool)
}

// ValueSize returns the size of the value at it's position if it can tell without reading
// the value.
func ValueSize(it Iterator) (int64, bool) {
	if vs, ok := it.(ValueSizer); ok {
		return vs.ValueSize()
	}
	return 0, false
}

// ValueSize passes on the data iterator's size (ValueSizer).
func (i *indexedIterator) ValueSize() (int64, bool) {
	if i.data == nil {
		return 0, false
	}
	return ValueSize(i.data)
}

// ValueSize passes on the current iterator's size (ValueSizer).
func (i *mergedIterator) ValueSize() (int64, bool) {
	if i.err != nil || i.dir <= dirEOI {
		return 0, false
	}
	return ValueSize(i.iters[i.index])
}
