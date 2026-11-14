package hx

import (
	"fmt"

	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/hx/store"
)

// Round is one save round: every change in it lands on disk together or not
// at all. Fill it, then World.Commit it. A Round keeps its memory between
// rounds. Not safe for concurrent use.
type Round struct {
	w   *World
	b   store.Batch
	buf []byte
	// newPlayers are players given lookup records in this round.
	newPlayers map[PlayerIDs]playerLookup
}

// NewRound returns an empty round for w.
func (w *World) NewRound() *Round { return &Round{w: w} }

// Len is the number of changes in the round.
func (r *Round) Len() int { return r.b.Len() }

// PutChunk encodes c as the chunk's terrain record. keys are the chunk's
// other small BDS keys; savedData3D is the Data3D the chunk was loaded with,
// or nil (see AppendChunk).
func (r *Round) PutChunk(dim Dimension, pos ChunkPos, c *chunk.Chunk, keys []ChunkKey, savedData3D []byte) error {
	var err error
	if r.buf, err = AppendChunk(r.buf[:0], c, r.w.table, keys, savedData3D); err != nil {
		return fmt.Errorf("hx: chunk %v in %s: %w", pos, dim.Dir(), err)
	}
	return r.PutTerrainRecord(dim, pos, r.buf)
}

// PutTerrainRecord stores an encoded terrain record. Its palettes must use
// IDs of the world's block table.
func (r *Round) PutTerrainRecord(dim Dimension, pos ChunkPos, rec []byte) error {
	return r.putPart(dim, pos, partTerrain, KindTerrain, rec)
}

// PutObjects encodes and stores a chunk's objects record.
func (r *Round) PutObjects(dim Dimension, pos ChunkPos, o *Objects) error {
	var err error
	if r.buf, err = AppendObjects(r.buf[:0], o); err != nil {
		return fmt.Errorf("hx: objects of chunk %v in %s: %w", pos, dim.Dir(), err)
	}
	return r.putPart(dim, pos, partObjects, KindObjects, r.buf)
}

// DeleteObjects removes a chunk's objects record.
func (r *Round) DeleteObjects(dim Dimension, pos ChunkPos) error {
	f, err := r.w.region(dim, pos)
	if err != nil {
		return err
	}
	k := chunkKey(pos, partObjects)
	r.b.Delete(f, k[:])
	return nil
}

// DeleteChunk removes every part of a chunk.
func (r *Round) DeleteChunk(dim Dimension, pos ChunkPos) error {
	f, err := r.w.region(dim, pos)
	if err != nil {
		return err
	}
	for _, part := range [...]byte{partTerrain, partObjects} {
		k := chunkKey(pos, part)
		r.b.Delete(f, k[:])
	}
	return nil
}

func (r *Round) putPart(dim Dimension, pos ChunkPos, part, kind byte, rec []byte) error {
	f, err := r.w.region(dim, pos)
	if err != nil {
		return err
	}
	k := chunkKey(pos, part)
	r.b.Put(f, kind, k[:], rec)
	return nil
}

func (r *Round) reset() {
	r.b.Reset()
	clear(r.newPlayers)
}

// PutLevel sets a world-wide key to value, as BDS stores it.
func (r *Round) PutLevel(key string, value []byte) {
	r.b.Put(r.w.level, KindLevel, []byte(key), value)
}

// DeleteLevel removes a world-wide key.
func (r *Round) DeleteLevel(key string) {
	r.b.Delete(r.w.level, []byte(key))
}

// Commit writes a round, with the block table entries its chunks added, and
// returns its round number. The round is empty afterwards, ready for reuse,
// whether or not the commit worked. It may be called from any goroutine;
// commits run one at a time.
func (w *World) Commit(r *Round) (uint64, error) {
	defer r.reset()
	n := w.table.PutNew(&r.b, w.blocks)
	round, err := w.s.Commit(&r.b)
	if err != nil {
		return 0, fmt.Errorf("hx: commit: %w", err)
	}
	w.table.MarkSaved(n)
	return round, nil
}
