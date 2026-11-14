package convert

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/hx"
	"github.com/helix-bedrock/world/hx/store"
	"github.com/helix-bedrock/world/lsm"
)

// BDSFilesDir is the folder of a Helix world holding the files of the BDS
// world it was converted from, other than db/ and level.dat (levelname.txt,
// world_icon.jpeg, packs...), copied back on export.
const BDSFilesDir = "bds"

// Options configures a conversion.
type Options struct {
	// Store configures the Helix world's record store.
	Store store.Options
	// RoundSize is how many bytes a save round collects before it is
	// committed. Default 64 MB.
	RoundSize int
}

func (o Options) withDefaults() Options {
	if o.RoundSize <= 0 {
		o.RoundSize = 64 << 20
	}
	return o
}

// Import converts the BDS world in bdsDir (the folder holding db/ and
// level.dat) into a new Helix world in hxDir. hxDir must not hold a world.
//
// The BDS world's data is not changed, but opening its database writes a
// new manifest, as LevelDB does; convert a copy if BDS must not see that.
func Import(bdsDir, hxDir string, opts Options) error {
	opts = opts.withDefaults()
	if _, err := os.Stat(filepath.Join(hxDir, hx.MetaFile)); err == nil {
		return fmt.Errorf("convert: %s already holds a Helix world", hxDir)
	}
	db, err := lsm.Open(filepath.Join(bdsDir, "db"), lsm.Options{})
	if err != nil {
		return fmt.Errorf("convert: open BDS world: %w", err)
	}
	defer db.Close()
	w, err := hx.Open(hxDir, hx.Config{Store: opts.Store})
	if err != nil {
		return fmt.Errorf("convert: create Helix world: %w", err)
	}
	im := &importer{db: db, w: w, r: w.NewRound(), opts: opts, st: subTranslator{table: w.Blocks()}}
	err = im.run(bdsDir, hxDir)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

type importer struct {
	db   *lsm.DB
	w    *hx.World
	r    *hx.Round
	opts Options
	st   subTranslator

	// group holds the keys of the chunks sharing one x, z, whose keys BDS
	// sorts together, until the next x, z begins.
	prefix []byte
	group  []pendingKey
	// referenced are the actor IDs some digp list holds, as uint64 for a
	// compact sorted set.
	referenced []uint64

	terrain hx.Terrain
	rec     []byte
	digp    []byte
	actor   []byte
}

// pendingKey is a chunk key waiting for its chunk to be written.
type pendingKey struct {
	id    chunkID
	tag   byte
	sub   int8
	key   []byte
	value []byte // nil for block entities, which are streamed
}

func (im *importer) run(bdsDir, hxDir string) error {
	if err := im.levelDat(bdsDir); err != nil {
		return err
	}
	if err := copyFiles(bdsDir, filepath.Join(hxDir, BDSFilesDir), bdsSkip); err != nil {
		return err
	}
	if err := im.keys(); err != nil {
		return err
	}
	if err := im.commit(); err != nil {
		return err
	}
	slices.Sort(im.referenced)
	if err := im.orphans(); err != nil {
		return err
	}
	return im.commit()
}

func (im *importer) levelDat(bdsDir string) error {
	b, err := os.ReadFile(filepath.Join(bdsDir, hx.LevelDat))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("convert: read level.dat: %w", err)
	}
	im.r.PutLevelDatBytes(b)
	return nil
}

// keys walks every key in order: chunk keys gather by x, z; digp and actor
// keys are read with their chunks; players and everything else go as they are.
func (im *importer) keys() error {
	it, err := im.db.NewIterator(nil)
	if err != nil {
		return fmt.Errorf("convert: %w", err)
	}
	defer it.Close()
	for ok := it.First(); ok; ok = it.Next() {
		if err := im.key(it); err != nil {
			return err
		}
	}
	if err := it.Err(); err != nil {
		return fmt.Errorf("convert: read BDS world: %w", err)
	}
	return im.flushGroup()
}

func (im *importer) key(it *lsm.Iterator) error {
	k := it.Key()
	if id, tag, sub, ok := parseChunkKey(k); ok {
		if !bytes.Equal(k[:8], im.prefix) {
			if err := im.flushGroup(); err != nil {
				return err
			}
			im.prefix = append(im.prefix[:0], k[:8]...)
		}
		pk := pendingKey{id: id, tag: tag, sub: sub, key: bytes.Clone(k)}
		if tag != hx.TagBlockEntities {
			pk.value = bytes.Clone(it.Value())
		}
		im.group = append(im.group, pk)
		return nil
	}
	switch {
	case bytes.HasPrefix(k, []byte(digpPrefix)), bytes.HasPrefix(k, []byte(actorPrefix)):
		// Read with their chunks; the ones no chunk took, after.
		return nil
	case bytes.HasPrefix(k, []byte(playerPrefix)):
		im.r.PutPlayerRecord(string(k), it.Value())
	default:
		im.r.PutLevel(string(k), it.Value())
	}
	return im.maybeCommit()
}

// flushGroup writes every chunk of the gathered x, z.
func (im *importer) flushGroup() error {
	g := im.group
	slices.SortStableFunc(g, func(a, b pendingKey) int { return int(a.id.dim - b.id.dim) })
	for i := 0; i < len(g); {
		j := i + 1
		for j < len(g) && g[j].id == g[i].id {
			j++
		}
		if err := im.chunk(g[i:j]); err != nil {
			return err
		}
		i = j
	}
	clear(im.group)
	im.group = im.group[:0]
	return nil
}

// chunk writes one chunk's terrain and objects.
func (im *importer) chunk(keys []pendingKey) error {
	id := keys[0].id
	t := &im.terrain
	*t = hx.Terrain{Keys: t.Keys[:0], Subs: t.Subs[:0]}
	im.st.reset()
	ow, err := im.r.Objects(id.dim, id.pos)
	if err != nil {
		return err
	}
	hasObjects := false
	for i := range keys {
		pk := &keys[i]
		switch {
		case pk.tag == tagSubChunk:
			s, err := im.st.translate(pk.value, pk.sub)
			if err != nil {
				return fmt.Errorf("convert: chunk %v: %w", id, err)
			}
			t.Subs = append(t.Subs, s)
		case pk.tag == tagData3D:
			t.Data3D = pk.value
		case pk.tag == hx.TagBlockEntities:
			if err := im.blockEntities(ow, pk.key); err != nil {
				return fmt.Errorf("convert: block entities of chunk %v: %w", id, err)
			}
			hasObjects = true
		case hx.ObjectTag(pk.tag):
			ow.Key(pk.tag, pk.value)
			hasObjects = true
		default:
			t.Keys = append(t.Keys, hx.ChunkKey{Tag: pk.tag, Value: pk.value})
		}
	}
	entities, err := im.entities(ow, id)
	if err != nil {
		return err
	}
	if hasObjects || entities {
		if err := ow.Close(); err != nil {
			return err
		}
	}
	im.rec = hx.AppendTerrain(im.rec[:0], t)
	if err := im.r.PutTerrainRecord(id.dim, id.pos, im.rec); err != nil {
		return err
	}
	return im.maybeCommit()
}

// blockEntities streams a chunk's block entity value into shells, one
// compound at a time, so a value of any size is never held whole.
func (im *importer) blockEntities(ow *hx.ObjectsWriter, key []byte) error {
	rc, size, err := im.db.GetStream(key)
	if err != nil {
		return err
	}
	defer rc.Close()
	cr := nbt.NewCompoundReader(rc, size, nbt.LittleEndian)
	for {
		c, err := cr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := ow.BlockEntity(c); err != nil {
			return err
		}
		if err := im.maybeCommit(); err != nil {
			return err
		}
	}
}

// entities adds the entities of the chunk's digp list. A list that is not a
// whole number of IDs is left for the level keys, as are its entities.
func (im *importer) entities(ow *hx.ObjectsWriter, id chunkID) (bool, error) {
	var err error
	im.digp, err = im.db.Get(digpKey(id))
	if errors.Is(err, lsm.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("convert: digp of chunk %v: %w", id, err)
	}
	if len(im.digp)%hx.ActorIDLen != 0 {
		return false, nil
	}
	ow.Digp()
	for b := im.digp; len(b) > 0; b = b[hx.ActorIDLen:] {
		aid := b[:hx.ActorIDLen]
		data, err := im.db.Get(append([]byte(actorPrefix), aid...))
		if errors.Is(err, lsm.ErrNotFound) {
			data = nil
		} else if err != nil {
			return false, fmt.Errorf("convert: entity of chunk %v: %w", id, err)
		}
		if err := ow.Entity(aid, data); err != nil {
			return false, err
		}
		if data != nil {
			im.referenced = append(im.referenced, binary.BigEndian.Uint64(aid))
		}
		if err := im.maybeCommit(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// orphans keeps, as level keys, digp lists of chunks with no other key and
// entities no list took: lossless, and found by an export.
func (im *importer) orphans() error {
	for _, prefix := range [...]string{digpPrefix, actorPrefix} {
		it, err := im.db.NewIterator([]byte(prefix))
		if err != nil {
			return fmt.Errorf("convert: %w", err)
		}
		for ok := it.First(); ok; ok = it.Next() {
			if im.taken(prefix, it.Key()) {
				continue
			}
			im.r.PutLevel(string(it.Key()), it.Value())
			if err := im.maybeCommit(); err != nil {
				_ = it.Close()
				return err
			}
		}
		err = it.Err()
		if cerr := it.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("convert: read BDS world: %w", err)
		}
	}
	return nil
}

// taken reports whether a digp or actor key went into a chunk.
func (im *importer) taken(prefix string, k []byte) bool {
	if prefix == digpPrefix {
		id, ok := parseDigpKey(k)
		return ok && im.hasChunk(id) && im.digpWhole(id)
	}
	aid := k[len(actorPrefix):]
	if len(aid) != hx.ActorIDLen {
		return false
	}
	_, found := slices.BinarySearch(im.referenced, binary.BigEndian.Uint64(aid))
	return found
}

// hasChunk reports whether a chunk was written: every chunk with a chunk key
// gets a terrain record.
func (im *importer) hasChunk(id chunkID) bool {
	var err error
	im.rec, err = im.w.TerrainRecord(id.dim, id.pos, im.rec)
	return err == nil
}

// digpWhole reports whether the chunk's digp list was taken into it, which
// entities only does for whole lists.
func (im *importer) digpWhole(id chunkID) bool {
	b, err := im.db.Get(digpKey(id))
	return err == nil && len(b)%hx.ActorIDLen == 0
}

func (im *importer) maybeCommit() error {
	if im.r.Size() < im.opts.RoundSize {
		return nil
	}
	return im.commit()
}

func (im *importer) commit() error {
	if im.r.Len() == 0 {
		return nil
	}
	if _, err := im.w.Commit(im.r); err != nil {
		return fmt.Errorf("convert: %w", err)
	}
	return nil
}
