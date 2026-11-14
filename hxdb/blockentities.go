package hxdb

import (
	"errors"
	"fmt"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/cube"
	"github.com/helix-bedrock/world/hx"
	"github.com/helix-bedrock/world/mcdb"
)

// EachBlockEntity calls fn with the position and whole stored compound of each
// block entity of the chunk, in order. The compound is valid only during the
// call. Only one block entity is held at a time. An error from fn stops it and
// is returned. Safe for concurrent use.
func (db *DB) EachBlockEntity(chunkPos mcdb.ChunkPos, dim mcdb.Dimension, fn func(pos cube.Pos, raw []byte) error) error {
	if err := db.settleChunk(chunkID{dim, chunkPos}); err != nil {
		return err
	}
	s := db.getScratch()
	defer db.putScratch(s)
	d, p := hx.Dimension(dim), hx.ChunkPos(chunkPos)
	o, err := db.objects(s, d, p)
	if err != nil || o == nil {
		return err
	}
	for _, sh := range o.BlockEntities {
		at, err := shellPos(sh)
		if err != nil {
			return fmt.Errorf("hxdb: block entities of %v (%v): %w", chunkPos, dim, err)
		}
		if s.shell, err = db.w.ExpandShell(d, p, sh, s.shell[:0]); err != nil {
			return fmt.Errorf("hxdb: block entities of %v (%v): %w", chunkPos, dim, err)
		}
		if err := fn(at, s.shell); err != nil {
			return err
		}
	}
	return nil
}

// LoadBlockEntity returns the stored compound of the block entity at pos in
// the chunk at chunkPos, reading the payloads of that one only, or an error
// matching mcdb.ErrNotFound. Safe for concurrent use.
func (db *DB) LoadBlockEntity(chunkPos mcdb.ChunkPos, dim mcdb.Dimension, pos cube.Pos) (map[string]any, error) {
	if err := db.settleChunk(chunkID{dim, chunkPos}); err != nil {
		return nil, err
	}
	s := db.getScratch()
	defer db.putScratch(s)
	d, p := hx.Dimension(dim), hx.ChunkPos(chunkPos)
	o, err := db.objects(s, d, p)
	if err != nil {
		return nil, err
	}
	if o != nil {
		for _, sh := range o.BlockEntities {
			at, err := shellPos(sh)
			if err != nil {
				return nil, fmt.Errorf("hxdb: load block entity %v: %w", pos, err)
			}
			if at != pos {
				continue
			}
			if s.shell, err = db.w.ExpandShell(d, p, sh, s.shell[:0]); err != nil {
				return nil, fmt.Errorf("hxdb: load block entity %v: %w", pos, err)
			}
			m, err := nbt.DecodeWithout(s.shell, nbt.LittleEndian, nil)
			if err != nil {
				return nil, fmt.Errorf("hxdb: load block entity %v: %w", pos, err)
			}
			return m, nil
		}
	}
	return nil, fmt.Errorf("hxdb: load block entity %v: %w", pos, mcdb.ErrNotFound)
}

// objects reads and decodes a chunk's objects record into the scratch, or
// returns nil if it has none.
func (db *DB) objects(s *scratch, dim hx.Dimension, pos hx.ChunkPos) (*hx.Objects, error) {
	rec, err := db.w.ObjectsRecord(dim, pos, s.obj)
	s.obj = rec
	if errors.Is(err, hx.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("hxdb: objects of %v in %s: %w", pos, dim.Dir(), err)
	}
	if err := hx.DecodeObjects(rec, &s.objects); err != nil {
		return nil, fmt.Errorf("hxdb: objects of %v in %s: %w", pos, dim.Dir(), err)
	}
	return &s.objects, nil
}

// EachBlockEntityView is EachBlockEntity for a chunk only shown, not run: each
// compound comes without the fields skip names, and a big field skipped (a
// chest's Items, cut into a payload) is never read. Safe for concurrent use.
func (db *DB) EachBlockEntityView(chunkPos mcdb.ChunkPos, dim mcdb.Dimension, skip func(name string) bool, fn func(pos cube.Pos, raw []byte) error) error {
	if err := db.settleChunk(chunkID{dim, chunkPos}); err != nil {
		return err
	}
	s := db.getScratch()
	defer db.putScratch(s)
	d, p := hx.Dimension(dim), hx.ChunkPos(chunkPos)
	o, err := db.objects(s, d, p)
	if err != nil || o == nil {
		return err
	}
	for _, sh := range o.BlockEntities {
		at, err := shellPos(sh)
		if err != nil {
			return fmt.Errorf("hxdb: block entities of %v (%v): %w", chunkPos, dim, err)
		}
		if s.shell, err = db.w.ExpandShellWithout(d, p, sh, s.shell[:0], skip); err != nil {
			return fmt.Errorf("hxdb: block entities of %v (%v): %w", chunkPos, dim, err)
		}
		if err := fn(at, s.shell); err != nil {
			return err
		}
	}
	return nil
}
