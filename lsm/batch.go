package lsm

import (
	"bytes"
	"encoding/binary"
	"io"
)

// Batch is a set of writes applied together. Puts of values at or over Options.BigValue
// and PutStream values are written into tables of their own instead of the log.
type Batch struct {
	small   []byte // entries in log record form: kind, key, value
	count   int
	streams []streamPut
}

// streamPut is a big value written from a stream.
type streamPut struct {
	key  []byte
	open func() (io.ReadCloser, int64, error)
}

// Put sets key to value. Both are copied.
func (b *Batch) Put(key, value []byte) {
	b.small = append(b.small, byte(kindValue))
	b.small = appendUvarintBytes(b.small, key)
	b.small = appendUvarintBytes(b.small, value)
	b.count++
}

// Delete removes key. It is copied.
func (b *Batch) Delete(key []byte) {
	b.small = append(b.small, byte(kindDelete))
	b.small = appendUvarintBytes(b.small, key)
	b.count++
}

// PutStream sets key to a value read from the reader open returns, of the size it
// returns, when the batch is written. The value is never held whole.
func (b *Batch) PutStream(key []byte, open func() (io.ReadCloser, int64, error)) {
	b.streams = append(b.streams, streamPut{key: bytes.Clone(key), open: open})
}

// Len is the number of writes in the batch.
func (b *Batch) Len() int { return b.count + len(b.streams) }

// Reset empties the batch for reuse.
func (b *Batch) Reset() {
	b.small, b.count, b.streams = b.small[:0], 0, b.streams[:0]
}

// splitBig moves puts of values of at least big bytes to streams.
func (b *Batch) splitBig(big int) {
	if !b.hasBig(big) {
		return
	}
	var kept []byte
	count := 0
	_ = eachEntry(b.small, func(k kind, key, value []byte) error {
		if k == kindValue && len(value) >= big {
			v := bytes.Clone(value)
			b.streams = append(b.streams, streamPut{key: bytes.Clone(key), open: func() (io.ReadCloser, int64, error) {
				return io.NopCloser(bytes.NewReader(v)), int64(len(v)), nil
			}})
			return nil
		}
		kept = append(kept, byte(k))
		kept = appendUvarintBytes(kept, key)
		if k == kindValue {
			kept = appendUvarintBytes(kept, value)
		}
		count++
		return nil
	})
	b.small, b.count = kept, count
}

func (b *Batch) hasBig(big int) bool {
	found := false
	_ = eachEntry(b.small, func(k kind, _, value []byte) error {
		if k == kindValue && len(value) >= big {
			found = true
		}
		return nil
	})
	return found
}

// batchHeaderLen is the sequence number and count before a log record's entries.
const batchHeaderLen = 12

// logRecord encodes the batch's small entries as a log record starting at seq.
func (b *Batch) logRecord(dst []byte, seq uint64) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, seq)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(b.count)) //nolint:gosec // batch sizes are small
	return append(dst, b.small...)
}

// eachEntry decodes log record entries.
func eachEntry(b []byte, fn func(k kind, key, value []byte) error) error {
	for len(b) > 0 {
		k := kind(b[0])
		key, rest, err := readUvarintBytes(b[1:])
		if err != nil {
			return err
		}
		var value []byte
		switch k {
		case kindValue:
			if value, rest, err = readUvarintBytes(rest); err != nil {
				return err
			}
		case kindDelete:
		default:
			return errCorrupt("batch entry kind")
		}
		if err := fn(k, key, value); err != nil {
			return err
		}
		b = rest
	}
	return nil
}

// decodeLogRecord splits a log record into its first sequence number, count and entries.
func decodeLogRecord(rec []byte) (seq uint64, count int, entries []byte, err error) {
	if len(rec) < batchHeaderLen {
		return 0, 0, nil, errCorrupt("batch header")
	}
	return binary.LittleEndian.Uint64(rec), int(binary.LittleEndian.Uint32(rec[8:])), rec[batchHeaderLen:], nil
}
