package store

import (
	"bytes"
	"encoding/binary"
	"slices"
)

// entry says where a key's record is. Keys live in the index's key arena, so
// an index holds no pointers for the collector to scan.
type entry struct {
	off  uint64
	size uint64 // the record's length on disk, before alignment
	raw  uint64 // the body's length decompressed
	// round is the save round that wrote the record.
	round  uint64
	keyOff uint32
	keyLen uint16
	kind   byte
}

func (e entry) extent() extent { return extent{off: e.off, size: alignUp(e.size)} }

// change is one key's new entry, or its deletion, in a save round.
type change struct {
	key []byte
	e   entry
	del bool
}

// index maps a file's keys to entries, sorted by key.
type index struct {
	keys []byte
	ents []entry
	// garbage counts arena bytes no entry uses any more.
	garbage int
	// ins and dels are scratch for apply.
	ins  []entry
	dels []int
}

const (
	entryMem = 40
	// compactAt is how much garbage the key arena may hold before it is rebuilt.
	compactAt = 64 << 10
)

func (ix *index) key(e entry) []byte { return ix.keys[e.keyOff : e.keyOff+uint32(e.keyLen)] }

// search returns the position of key, or where it would go.
func (ix *index) search(key []byte) (int, bool) {
	lo, hi := 0, len(ix.ents)
	for lo < hi {
		m := int(uint(lo+hi) >> 1) //nolint:gosec // non-negative
		if bytes.Compare(ix.key(ix.ents[m]), key) < 0 {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo, lo < len(ix.ents) && bytes.Equal(ix.key(ix.ents[lo]), key)
}

func (ix *index) get(key []byte) (entry, bool) {
	i, ok := ix.search(key)
	if !ok {
		return entry{}, false
	}
	return ix.ents[i], true
}

// memBytes is about how much memory the index holds.
func (ix *index) memBytes() int64 {
	return int64(cap(ix.keys) + (cap(ix.ents)+cap(ix.ins))*entryMem + cap(ix.dels)*8)
}

// apply makes changes, sorted by key with each key once, and appends every
// entry they replace or delete to old.
func (ix *index) apply(changes []change, old []entry) []entry {
	ix.ins, ix.dels = ix.ins[:0], ix.dels[:0]
	for i := range changes {
		c := &changes[i]
		pos, found := ix.search(c.key)
		switch {
		case found && c.del:
			old = append(old, ix.ents[pos])
			ix.dels = append(ix.dels, pos)
		case found:
			old = append(old, ix.ents[pos])
			e := c.e
			e.keyOff, e.keyLen = ix.ents[pos].keyOff, ix.ents[pos].keyLen
			ix.ents[pos] = e
		case !c.del:
			e := c.e
			e.keyOff, e.keyLen = uint32(len(ix.keys)), uint16(len(c.key)) //nolint:gosec // keys are bounded
			ix.keys = append(ix.keys, c.key...)
			ix.ins = append(ix.ins, e)
		}
	}
	ix.removeAt(ix.dels)
	ix.insertSorted(ix.ins)
	if ix.garbage > compactAt && ix.garbage > len(ix.keys)/2 {
		ix.compactKeys()
	}
	return old
}

// removeAt removes the entries at the ascending positions pos.
func (ix *index) removeAt(pos []int) {
	if len(pos) == 0 {
		return
	}
	w, next := pos[0], 0
	for r := pos[0]; r < len(ix.ents); r++ {
		if next < len(pos) && pos[next] == r {
			ix.garbage += int(ix.ents[r].keyLen)
			next++
			continue
		}
		ix.ents[w] = ix.ents[r]
		w++
	}
	ix.ents = ix.ents[:w]
}

// insertSorted merges ins, sorted by key and holding no key already present,
// into the entries, from the back so nothing moves twice.
func (ix *index) insertSorted(ins []entry) {
	if len(ins) == 0 {
		return
	}
	n := len(ix.ents)
	ix.ents = slices.Grow(ix.ents, len(ins))[:n+len(ins)]
	i, j := n-1, len(ins)-1
	for k := len(ix.ents) - 1; j >= 0; k-- {
		if i >= 0 && bytes.Compare(ix.key(ix.ents[i]), ix.key(ins[j])) > 0 {
			ix.ents[k] = ix.ents[i]
			i--
		} else {
			ix.ents[k] = ins[j]
			j--
		}
	}
}

// compactKeys rebuilds the key arena without the keys of removed entries.
func (ix *index) compactKeys() {
	keys := make([]byte, 0, len(ix.keys)-ix.garbage)
	for i := range ix.ents {
		k := ix.key(ix.ents[i])
		ix.ents[i].keyOff = uint32(len(keys)) //nolint:gosec // bounded by the old arena
		keys = append(keys, k...)
	}
	ix.keys, ix.garbage = keys, 0
}

// encode appends the index in its on-disk form.
func (ix *index) encode(dst []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(ix.ents)))
	for _, e := range ix.ents {
		k := ix.key(e)
		dst = binary.AppendUvarint(dst, uint64(len(k)))
		dst = append(dst, k...)
		dst = append(dst, e.kind)
		dst = binary.AppendUvarint(dst, e.off)
		dst = binary.AppendUvarint(dst, e.size)
		dst = binary.AppendUvarint(dst, e.raw)
		dst = binary.AppendUvarint(dst, e.round)
	}
	return dst
}

// decodeIndex reads an index written by encode, checking that its keys are
// sorted and its records lie in the data area.
func decodeIndex(b []byte, path string) (index, error) {
	d := decoder{b: b}
	n := d.uvarint()
	if d.bad || n > uint64(len(b)) {
		return index{}, errCorrupt(path, "index length")
	}
	ix := index{ents: make([]entry, 0, n)}
	var prev []byte
	for range n {
		k := d.bytes(d.uvarint())
		e := entry{kind: d.byte(), off: d.uvarint(), size: d.uvarint(), raw: d.uvarint(), round: d.uvarint()}
		if d.bad || len(k) > MaxKeyLen || e.kind == kindIndex {
			return index{}, errCorrupt(path, "index entry")
		}
		if prev != nil && bytes.Compare(prev, k) >= 0 {
			return index{}, errCorrupt(path, "index order")
		}
		if e.off < dataStart || e.size < recHeaderLen+uint64(len(k)) || e.off+alignUp(e.size) < e.off {
			return index{}, errCorrupt(path, "index entry range")
		}
		e.keyOff, e.keyLen = uint32(len(ix.keys)), uint16(len(k)) //nolint:gosec // checked above
		ix.keys = append(ix.keys, k...)
		ix.ents = append(ix.ents, e)
		prev = k
	}
	if len(d.b) != 0 {
		return index{}, errCorrupt(path, "index trailing bytes")
	}
	return ix, nil
}

// decoder reads varints and byte strings, remembering the first failure.
type decoder struct {
	b   []byte
	bad bool
}

func (d *decoder) uvarint() uint64 {
	if d.bad {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.bad = true
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) byte() byte {
	if d.bad || len(d.b) == 0 {
		d.bad = true
		return 0
	}
	v := d.b[0]
	d.b = d.b[1:]
	return v
}

func (d *decoder) bytes(n uint64) []byte {
	if d.bad || n > uint64(len(d.b)) {
		d.bad = true
		return nil
	}
	v := d.b[:n]
	d.b = d.b[n:]
	return v
}
