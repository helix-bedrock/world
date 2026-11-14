package store

import (
	"bytes"
	"cmp"
	"io"
	"slices"
)

// Batch is a set of writes committed as one save round. Its memory is kept
// between rounds: Reset it and fill it again. Not safe for concurrent use.
type Batch struct {
	ops   []op
	arena []byte
}

// OpenFunc opens the body of a streamed record. It is called once, during the
// commit.
type OpenFunc func() (io.ReadCloser, error)

type op struct {
	open   OpenFunc
	size   int64
	keyOff int
	keyLen int
	valOff int
	valLen int
	file   FileID
	kind   byte
	del    bool
	// raw is set for a record copied as stored (PutRaw), described by rawh.
	raw  bool
	rawh RawHeader
}

// Put stores value under key in file f, with kind (1 to 255) saying what it is.
// key and value are copied.
func (b *Batch) Put(f FileID, kind byte, key, value []byte) {
	o := b.newOp(f, kind, key)
	o.valOff, o.valLen = len(b.arena), len(value)
	b.arena = append(b.arena, value...)
	b.ops = append(b.ops, o)
}

// PutStream stores a body of exactly size bytes that open reads during the
// commit, never held whole in memory: for records of megabytes.
func (b *Batch) PutStream(f FileID, kind byte, key []byte, size int64, open OpenFunc) {
	o := b.newOp(f, kind, key)
	o.open, o.size = open, size
	b.ops = append(b.ops, o)
}

// Delete removes key from file f.
func (b *Batch) Delete(f FileID, key []byte) {
	o := b.newOp(f, 0, key)
	o.del = true
	b.ops = append(b.ops, o)
}

func (b *Batch) newOp(f FileID, kind byte, key []byte) op {
	o := op{file: f, kind: kind, keyOff: len(b.arena), keyLen: len(key)}
	b.arena = append(b.arena, key...)
	return o
}

// Size is about how many bytes the batch holds: its keys and values, not
// streamed bodies.
func (b *Batch) Size() int { return len(b.arena) }

// Len is the number of writes in the batch.
func (b *Batch) Len() int { return len(b.ops) }

// keepArena is the biggest buffer a batch keeps across Reset: a round that
// held a huge value does not pin that much memory for the rest of the run.
const keepArena = 32 << 20

// Reset empties the batch, keeping its memory unless it grew past keepArena.
func (b *Batch) Reset() {
	clear(b.ops) // drop the OpenFuncs
	b.ops, b.arena = b.ops[:0], b.arena[:0]
	if cap(b.arena) > keepArena {
		b.arena = nil
	}
}

func (b *Batch) key(o *op) []byte   { return b.arena[o.keyOff : o.keyOff+o.keyLen] }
func (b *Batch) value(o *op) []byte { return b.arena[o.valOff : o.valOff+o.valLen] }

// sort orders the writes by file and key, keeping the order of writes to the
// same key so the last one wins.
func (b *Batch) sort() {
	slices.SortStableFunc(b.ops, func(x, y op) int {
		if c := cmp.Compare(x.file, y.file); c != 0 {
			return c
		}
		return bytes.Compare(b.key(&x), b.key(&y))
	})
}
