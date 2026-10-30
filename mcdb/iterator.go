// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/world/mcdb/iterator.go

package mcdb

import (
	"encoding/binary"
	"fmt"

	"github.com/helix-bedrock/world/chunk"
)

// ColumnIterator iterates over a DB's position/column pairs in key order.
//
// When an error is encountered, any call to Next will return false and will
// yield no position/chunk pairs. The error can be queried by calling the Error
// method. Calling Release is still necessary.
//
// An iterator must be released after use, but it is not necessary to read
// an iterator until exhaustion.
// Also, an iterator is not necessarily safe for concurrent use, but it is
// safe to use multiple iterators concurrently, with each in a dedicated
// goroutine.
type ColumnIterator struct {
	dbIter KVIterator
	db     *DB
	r      *IteratorRange

	err error

	current *chunk.Column
	pos     ChunkPos
	dim     Dimension
	seen    map[dbKey]struct{}
}

func newColumnIterator(db *DB, r *IteratorRange) *ColumnIterator {
	return &ColumnIterator{
		db:     db,
		dbIter: db.kv.NewIterator(nil),
		seen:   make(map[dbKey]struct{}),
		r:      r,
	}
}

// Next moves the iterator to the next key/value pair.
// It returns false if the iterator is exhausted.
func (iter *ColumnIterator) Next() bool {
	// A loop, not recursion: worlds hold long runs of non-chunk keys.
	for iter.err == nil && iter.dbIter.Next() {
		k := iter.dbIter.Key()
		kLen := len(k)
		if (kLen != 9 && kLen != 13) || (k[kLen-1] != keyVersion && k[kLen-1] != keyVersionOld) {
			continue
		}
		dim := Overworld
		if kLen > 9 {
			dim = Dimension(int32(binary.LittleEndian.Uint32(k[8:12]))) //nolint:gosec // dimension IDs are stored as raw int32 bits
			if _, ok := dim.Range(); !ok {
				// Not a vanilla dimension (or not a chunk key); skip it.
				continue
			}
		}
		pos := ChunkPos{
			int32(binary.LittleEndian.Uint32(k[:4])),  //nolint:gosec // raw int32 bits
			int32(binary.LittleEndian.Uint32(k[4:8])), //nolint:gosec // raw int32 bits
		}
		if !iter.r.within(pos, dim) {
			continue
		}
		key := dbKey{dim: dim, pos: pos}
		if _, ok := iter.seen[key]; ok {
			// Already loaded: a chunk can have both version keys.
			continue
		}
		iter.pos, iter.dim = pos, dim
		iter.current, iter.err = iter.db.LoadColumn(pos, dim)
		if iter.err != nil {
			iter.err = fmt.Errorf("load chunk %v: %w", pos, iter.err)
			break
		}
		iter.seen[key] = struct{}{}
		return true
	}
	if iter.err == nil {
		iter.err = iter.dbIter.Error()
	}
	iter.current = nil
	return false
}

// Column returns the value of the current position/column pair, or nil if none.
func (iter *ColumnIterator) Column() *chunk.Column {
	return iter.current
}

// Position returns the position of the current position/column pair.
func (iter *ColumnIterator) Position() ChunkPos {
	return iter.pos
}

// Dimension returns the dimension of the current position/column pair.
func (iter *ColumnIterator) Dimension() Dimension {
	return iter.dim
}

// Release releases associated resources. Release should always success
// and can be called multiple times without causing error.
func (iter *ColumnIterator) Release() {
	iter.dbIter.Release()
}

// Error returns any accumulated error. Exhausting all the key/value pairs
// is not considered to be an error.
func (iter *ColumnIterator) Error() error {
	return iter.err
}

// IteratorRange is a range used to limit what columns are accumulated by a
// ColumnIterator.
type IteratorRange struct {
	// Min and Max limit what chunk positions are returned by a ColumnIterator.
	// A zero value for both Min and Max causes all positions to be within the
	// range.
	Min, Max ChunkPos
	// Dimension limits the columns to one dimension. If nil, every dimension
	// is read.
	Dimension *Dimension
}

// within checks if a position and dimension is within the IteratorRange.
func (r *IteratorRange) within(pos ChunkPos, dim Dimension) bool {
	if r.Dimension != nil && dim != *r.Dimension {
		return false
	}
	return ((r.Min == ChunkPos{}) && (r.Max == ChunkPos{})) ||
		pos[0] >= r.Min[0] && pos[0] < r.Max[0] && pos[1] >= r.Min[1] && pos[1] < r.Max[1]
}
