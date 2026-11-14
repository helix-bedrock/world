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
	w       *World
	b       store.Batch
	buf     []byte
	scratch []byte
	ow      ObjectsWriter
	// newPlayers are players given lookup records in this round.
	newPlayers map[PlayerIDs]playerLookup
	// levelDat is set when the round writes level.dat.
	levelDat []byte
	// restoreFrom is the last world RestoreChunk checked the block table of.
	restoreFrom *World
}

// NewRound returns an empty round for w.
func (w *World) NewRound() *Round { return &Round{w: w} }

// Len is the number of changes in the round.
func (r *Round) Len() int { return r.b.Len() }

// Size is about how many bytes the round holds.
func (r *Round) Size() int { return r.b.Size() }

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
	f, err := r.w.region(dim, pos)
	if err != nil {
		return err
	}
	k := chunkKey(pos, partTerrain)
	r.b.Put(f, KindTerrain, k[:], rec)
	return nil
}

// ObjectsSource is a chunk's objects as BDS stores them, for PutObjects.
type ObjectsSource struct {
	// Keys are chunk keys that belong with the objects (ObjectTag), except
	// block entities.
	Keys []ChunkKey
	// BlockEntities is BDS's block entity value: LE NBT compounds back to
	// back.
	BlockEntities []byte
	Entities      []EntityData
	// Digp is set to write a digp key even with no entities.
	Digp bool
}

// EntityData is an entity as BDS stores it: actor ID and NBT.
type EntityData struct {
	ID, Data []byte
}

// PutObjects writes a chunk's objects record and payloads from src. For
// chunks too big to hold whole, use Objects.
func (r *Round) PutObjects(dim Dimension, pos ChunkPos, src *ObjectsSource) error {
	ow, err := r.Objects(dim, pos)
	if err != nil {
		return err
	}
	for _, k := range src.Keys {
		ow.Key(k.Tag, k.Value)
	}
	if err := eachCompound(src.BlockEntities, ow.BlockEntity); err != nil {
		return fmt.Errorf("hx: block entities of chunk %v in %s: %w", pos, dim.Dir(), err)
	}
	if src.Digp || len(src.Entities) > 0 {
		ow.Digp()
	}
	for _, e := range src.Entities {
		if err := ow.Entity(e.ID, e.Data); err != nil {
			return fmt.Errorf("hx: entity of chunk %v in %s: %w", pos, dim.Dir(), err)
		}
	}
	return ow.Close()
}

// DeleteObjects removes a chunk's objects record and payloads.
func (r *Round) DeleteObjects(dim Dimension, pos ChunkPos) error {
	ow, err := r.Objects(dim, pos)
	if err != nil {
		return err
	}
	return ow.Close()
}

// DeleteChunk removes every part of a chunk.
func (r *Round) DeleteChunk(dim Dimension, pos ChunkPos) error {
	if err := r.DeleteObjects(dim, pos); err != nil {
		return err
	}
	f, err := r.w.region(dim, pos)
	if err != nil {
		return err
	}
	k := chunkKey(pos, partTerrain)
	r.b.Delete(f, k[:])
	return nil
}

func (r *Round) reset() {
	r.b.Reset()
	clear(r.newPlayers)
	r.levelDat = nil
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
// commits run one at a time. A round that wrote level.dat then copies it to
// the world's level.dat file for BDS tools; the copy in world.hx is the one
// Helix reads.
func (w *World) Commit(r *Round) (uint64, error) {
	defer r.reset()
	n := w.table.PutNew(&r.b, w.blocks)
	round, err := w.s.Commit(&r.b)
	if err != nil {
		return 0, fmt.Errorf("hx: commit: %w", err)
	}
	w.table.MarkSaved(n)
	if r.levelDat != nil {
		if err := writeFileAtomic(w.LevelDatPath(), r.levelDat, !w.conf.Store.NoSync); err != nil {
			return round, fmt.Errorf("hx: copy level.dat: %w", err)
		}
	}
	return round, nil
}
