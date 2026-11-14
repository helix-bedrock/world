package lsm

import (
	"encoding/binary"
)

// A block (LevelDB doc/table_format.md) is a run of entries, each sharing a key prefix with
// the one before it except at restart points, followed by the offsets of the restart
// points (uint32 each) and their count (uint32). An entry is: shared key bytes (varint),
// unshared key bytes (varint), value length (varint), the unshared key, the value.

const (
	// dataRestartInterval is how many data block entries share prefixes between restarts.
	dataRestartInterval = 16
	// indexRestartInterval is 1: index keys are looked up by binary search only.
	indexRestartInterval = 1
)

// blockBuilder builds one block.
type blockBuilder struct {
	interval int
	buf      []byte
	restarts []uint32
	last     []byte
	counter  int
	entries  int
}

func (b *blockBuilder) reset() {
	b.buf = b.buf[:0]
	b.restarts = b.restarts[:0]
	b.last = b.last[:0]
	b.counter = 0
	b.entries = 0
}

// add appends an entry; keys must come in order.
func (b *blockBuilder) add(key, value []byte) {
	shared := 0
	if b.counter < b.interval && b.entries > 0 {
		shared = sharedPrefix(b.last, key)
	} else {
		b.restarts = append(b.restarts, uint32(len(b.buf))) //nolint:gosec // blocks stay under 4 GB
		b.counter = 0
	}
	b.buf = binary.AppendUvarint(b.buf, uint64(shared))
	b.buf = binary.AppendUvarint(b.buf, uint64(len(key)-shared))
	b.buf = binary.AppendUvarint(b.buf, uint64(len(value)))
	b.buf = append(b.buf, key[shared:]...)
	b.buf = append(b.buf, value...)
	b.last = append(b.last[:0], key...)
	b.counter++
	b.entries++
}

// size is about how big the block is finished.
func (b *blockBuilder) size() int { return len(b.buf) + 4*len(b.restarts) + 4 }

// finish appends the restart points and returns the block.
func (b *blockBuilder) finish() []byte {
	if len(b.restarts) == 0 {
		b.restarts = append(b.restarts, 0)
	}
	for _, r := range b.restarts {
		b.buf = binary.LittleEndian.AppendUint32(b.buf, r)
	}
	b.buf = binary.LittleEndian.AppendUint32(b.buf, uint32(len(b.restarts))) //nolint:gosec // few
	return b.buf
}

func sharedPrefix(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// block is a decoded block.
type block struct {
	data     []byte // the entries
	restarts []byte // the restart offsets, 4 bytes each
}

// parseBlock splits a decompressed block into its entries and restart points.
func parseBlock(b []byte) (block, error) {
	if len(b) < 4 {
		return block{}, errCorrupt("block too short")
	}
	n := int(binary.LittleEndian.Uint32(b[len(b)-4:]))
	if n > (len(b)-4)/4 {
		return block{}, errCorrupt("block restart count")
	}
	end := len(b) - 4 - 4*n
	return block{data: b[:end], restarts: b[end : len(b)-4]}, nil
}

func (b block) restart(i int) int { return int(binary.LittleEndian.Uint32(b.restarts[4*i:])) }

func (b block) numRestarts() int { return len(b.restarts) / 4 }

// blockIter walks a block's entries in order.
type blockIter struct {
	b       block
	cmp     func(a, b []byte) int
	off     int // offset of the next entry
	key     []byte
	value   []byte
	valid   bool
	err     error
	restart int
}

func newBlockIter(b block, cmp func(a, b []byte) int) *blockIter {
	return &blockIter{b: b, cmp: cmp}
}

// reset points the iterator at another block.
func (it *blockIter) reset(b block) {
	it.b, it.off, it.key, it.value, it.valid, it.err = b, 0, it.key[:0], nil, false, nil
}

// first moves to the first entry.
func (it *blockIter) first() bool {
	it.off, it.key = 0, it.key[:0]
	return it.next()
}

// next moves to the next entry.
func (it *blockIter) next() bool {
	if it.err != nil || it.off >= len(it.b.data) {
		it.valid = false
		return false
	}
	shared, rest, err := readUvarint(it.b.data[it.off:])
	if err == nil {
		err = it.decode(shared, rest)
	}
	if err != nil {
		it.err, it.valid = err, false
		return false
	}
	it.valid = true
	return true
}

// decode reads the entry at it.off whose shared length was already read, rest being the
// data after it.
func (it *blockIter) decode(shared uint64, rest []byte) error {
	unshared, rest, err := readUvarint(rest)
	if err != nil {
		return err
	}
	vlen, rest, err := readUvarint(rest)
	if err != nil {
		return err
	}
	if shared > uint64(len(it.key)) || unshared > uint64(len(rest)) || vlen > uint64(len(rest))-unshared {
		return errCorrupt("block entry")
	}
	it.key = append(it.key[:shared], rest[:unshared]...)
	it.value = rest[unshared : unshared+vlen]
	it.off = len(it.b.data) - len(rest) + int(unshared+vlen)
	return nil
}

// seek moves to the first entry whose key is at or after target.
func (it *blockIter) seek(target []byte) bool {
	lo, hi := 0, it.b.numRestarts()-1
	for lo < hi {
		mid := (lo + hi + 1) / 2
		k, ok := it.restartKey(mid)
		if !ok {
			return false
		}
		if it.cmp(k, target) < 0 {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if it.b.numRestarts() == 0 {
		it.valid = false
		return false
	}
	it.off, it.key = it.b.restart(lo), it.key[:0]
	for it.next() {
		if it.cmp(it.key, target) >= 0 {
			return true
		}
	}
	return false
}

// restartKey returns the full key stored at restart point i.
func (it *blockIter) restartKey(i int) ([]byte, bool) {
	off := it.b.restart(i)
	if off >= len(it.b.data) {
		it.err, it.valid = errCorrupt("block restart offset"), false
		return nil, false
	}
	shared, rest, err := readUvarint(it.b.data[off:])
	if err != nil || shared != 0 {
		it.err, it.valid = errCorrupt("block restart entry"), false
		return nil, false
	}
	unshared, rest, err := readUvarint(rest)
	if err == nil {
		_, rest, err = readUvarint(rest)
	}
	if err != nil || unshared > uint64(len(rest)) {
		it.err, it.valid = errCorrupt("block restart entry"), false
		return nil, false
	}
	return rest[:unshared], true
}
