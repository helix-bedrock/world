package hx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/cube"
	"github.com/helix-bedrock/world/hx/store"
)

// Files of a Helix world, next to BDS's level.dat (kept unchanged).
const (
	// MetaFile holds the world's format version and settings.
	MetaFile = "world.hx"
	// LevelFile holds every world-wide key (maps, scoreboard, villages,
	// portals...), under the BDS key name, with the value as BDS stores it.
	LevelFile = "level.hx"
	// PlayersFile holds players under BDS's keys (players.go).
	PlayersFile = "players.hx"
	// LevelDat is BDS's level.dat.
	LevelDat = "level.dat"
)

// Record kinds besides KindBlock and KindTerrain.
const (
	KindObjects byte = 3
	KindLevel   byte = 4
	KindPlayer  byte = 5
	KindMeta    byte = 6
)

// formatVersion is the world format version in world.hx.
const formatVersion = 1

var metaFormatKey = []byte("format")

// ErrNotFound is returned for a record that does not exist.
var ErrNotFound = store.ErrNotFound

// Dimension is a dimension's ID, as in BDS's keys.
type Dimension int32

// The vanilla dimensions.
const (
	Overworld Dimension = 0
	Nether    Dimension = 1
	End       Dimension = 2
)

// Range returns the dimension's height range, and false for an unknown ID.
// Same ranges as mcdb's.
func (d Dimension) Range() (cube.Range, bool) {
	switch d {
	case Overworld:
		return cube.Range{-64, 319}, true
	case Nether:
		return cube.Range{0, 127}, true
	case End:
		return cube.Range{0, 255}, true
	}
	return cube.Range{}, false
}

// Dir returns the folder of the dimension's region files.
func (d Dimension) Dir() string {
	switch d {
	case Overworld:
		return "overworld"
	case Nether:
		return "nether"
	case End:
		return "end"
	}
	return "dim" + strconv.Itoa(int(d))
}

// ChunkPos is a chunk's position in chunks: X, Z.
type ChunkPos [2]int32

// Chunk parts, the last byte of a chunk's key in its region file.
const (
	partTerrain byte = 1
	partObjects byte = 2
)

// regionShift makes a region 32x32 chunks.
const regionShift = 5

// chunkKey is a chunk part's key in its region: the chunk's index in the
// region (big-endian u16, z*32+x) and the part.
func chunkKey(pos ChunkPos, part byte) [3]byte {
	i := uint16(pos[1]&31)<<regionShift | uint16(pos[0]&31) //nolint:gosec // 10 bits
	return [3]byte{byte(i >> 8), byte(i), part}
}

// Config configures a World.
type Config struct {
	// Blocks is the server's block registry.
	Blocks chunk.BlockRegistry
	// Upgrader upgrades block states saved by older versions. Optional.
	Upgrader *chunk.Upgrader
	// Store configures the record store.
	Store store.Options
}

// World is a Helix world on disk. Reads may run from any goroutine at any
// time, also during a Commit; Commits are serialised.
type World struct {
	dir   string
	conf  Config
	s     *store.Store
	table *BlockTable

	meta, level, players, blocks store.FileID

	regionMu sync.RWMutex
	regions  map[regionKey]store.FileID
}

type regionKey struct {
	dim  Dimension
	x, z int32
}

// Open opens the Helix world in dir, creating it if the folder holds none.
func Open(dir string, conf Config) (*World, error) {
	if conf.Blocks == nil {
		return nil, errors.New("hx: open world: no block registry")
	}
	s, err := store.Open(dir, conf.Store)
	if err != nil {
		return nil, fmt.Errorf("hx: open world: %w", err)
	}
	w := &World{dir: dir, conf: conf, s: s, regions: map[regionKey]store.FileID{}}
	if err := w.openFiles(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return w, nil
}

func (w *World) openFiles() error {
	ids := [...]*store.FileID{&w.meta, &w.level, &w.players, &w.blocks}
	for i, name := range [...]string{MetaFile, LevelFile, PlayersFile, BlocksFile} {
		id, err := w.s.File(name)
		if err != nil {
			return fmt.Errorf("hx: open world: %w", err)
		}
		*ids[i] = id
	}
	if err := w.checkFormat(); err != nil {
		return err
	}
	t, err := LoadBlockTable(w.s, w.blocks, w.conf.Blocks, w.conf.Upgrader)
	if err != nil {
		return fmt.Errorf("hx: open world: %w", err)
	}
	w.table = t
	return nil
}

// checkFormat reads the format version, writing it to a new world.
func (w *World) checkFormat() error {
	v, err := w.s.Get(w.meta, metaFormatKey, nil)
	if errors.Is(err, store.ErrNotFound) {
		var b store.Batch
		b.Put(w.meta, KindMeta, metaFormatKey, binary.LittleEndian.AppendUint32(nil, formatVersion))
		if _, err := w.s.Commit(&b); err != nil {
			return fmt.Errorf("hx: create world: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("hx: read format version: %w", err)
	}
	if len(v) != 4 {
		return errors.New("hx: corrupt format version")
	}
	if got := binary.LittleEndian.Uint32(v); got > formatVersion {
		return fmt.Errorf("hx: world format %d is newer than this version reads (%d)", got, formatVersion)
	}
	return nil
}

// Dir returns the world's folder.
func (w *World) Dir() string { return w.dir }

// LevelDatPath returns the path of the world's level.dat.
func (w *World) LevelDatPath() string { return filepath.Join(w.dir, LevelDat) }

// Blocks returns the world's block table.
func (w *World) Blocks() *BlockTable { return w.table }

// Store returns the record store under the world, for backups and tools.
func (w *World) Store() *store.Store { return w.s }

// Close checkpoints and closes the world. Rounds not committed are lost.
func (w *World) Close() error {
	if err := w.s.Close(); err != nil {
		return fmt.Errorf("hx: close world: %w", err)
	}
	return nil
}

// region returns the file of the region holding pos. Known regions cost one
// map lookup and no allocation.
func (w *World) region(dim Dimension, pos ChunkPos) (store.FileID, error) {
	k := regionKey{dim: dim, x: pos[0] >> regionShift, z: pos[1] >> regionShift}
	w.regionMu.RLock()
	id, ok := w.regions[k]
	w.regionMu.RUnlock()
	if ok {
		return id, nil
	}
	id, err := w.s.File(regionPath(k))
	if err != nil {
		return 0, fmt.Errorf("hx: region %v: %w", k, err)
	}
	w.regionMu.Lock()
	w.regions[k] = id
	w.regionMu.Unlock()
	return id, nil
}

func regionPath(k regionKey) string {
	return k.dim.Dir() + "/r." + strconv.Itoa(int(k.x)) + "." + strconv.Itoa(int(k.z)) + ".hx"
}

// TerrainRecord reads the terrain record of a chunk into dst[:0], growing it
// only if too small, or returns ErrNotFound. With a big enough dst it does not
// allocate.
func (w *World) TerrainRecord(dim Dimension, pos ChunkPos, dst []byte) ([]byte, error) {
	return w.part(dim, pos, partTerrain, dst)
}

// ObjectsRecord reads the objects record of a chunk, as TerrainRecord does.
func (w *World) ObjectsRecord(dim Dimension, pos ChunkPos, dst []byte) ([]byte, error) {
	return w.part(dim, pos, partObjects, dst)
}

func (w *World) part(dim Dimension, pos ChunkPos, part byte, dst []byte) ([]byte, error) {
	f, err := w.region(dim, pos)
	if err != nil {
		return dst[:0], err
	}
	k := chunkKey(pos, part)
	return w.s.Get(f, k[:], dst)
}

// LoadChunk reads and decodes a chunk's terrain, using buf and t as scratch
// space; it returns buf for reuse. The chunk is ErrNotFound if not stored.
func (w *World) LoadChunk(dim Dimension, pos ChunkPos, buf []byte, t *Terrain) (*chunk.Chunk, []byte, error) {
	r, ok := dim.Range()
	if !ok {
		return nil, buf, fmt.Errorf("hx: unknown dimension %d", dim)
	}
	buf, err := w.TerrainRecord(dim, pos, buf)
	if err != nil {
		return nil, buf, err
	}
	if err := DecodeTerrain(buf, t); err != nil {
		return nil, buf, fmt.Errorf("hx: chunk %v in %s: %w", pos, dim.Dir(), err)
	}
	c, err := DecodeChunk(t, w.table, w.conf.Upgrader, r)
	if err != nil {
		return nil, buf, fmt.Errorf("hx: chunk %v in %s: %w", pos, dim.Dir(), err)
	}
	return c, buf, nil
}

// Level reads a world-wide key into dst[:0], or returns ErrNotFound.
func (w *World) Level(key string, dst []byte) ([]byte, error) {
	return w.s.Get(w.level, []byte(key), dst)
}

// EachLevel calls fn for every world-wide key, in key order, until fn returns
// false. The key is valid only during the call.
func (w *World) EachLevel(fn func(key []byte) bool) error {
	return w.s.Range(w.level, func(key []byte, _ store.Info) bool { return fn(key) })
}

// EachChunk calls fn for every chunk of dim with a terrain record, region by
// region, until fn returns false.
func (w *World) EachChunk(dim Dimension, fn func(pos ChunkPos) bool) error {
	ents, err := os.ReadDir(filepath.Join(w.dir, dim.Dir()))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("hx: list %s: %w", dim.Dir(), err)
	}
	for _, e := range ents {
		rx, rz, ok := parseRegion(e.Name())
		if !ok {
			continue
		}
		more, err := w.eachRegionChunk(dim, rx, rz, fn)
		if err != nil || !more {
			return err
		}
	}
	return nil
}

func (w *World) eachRegionChunk(dim Dimension, rx, rz int32, fn func(ChunkPos) bool) (bool, error) {
	f, err := w.region(dim, ChunkPos{rx << regionShift, rz << regionShift})
	if err != nil {
		return false, err
	}
	more := true
	err = w.s.Range(f, func(key []byte, _ store.Info) bool {
		if len(key) != 3 || key[2] != partTerrain {
			return true
		}
		i := int32(key[0])<<8 | int32(key[1])
		more = fn(ChunkPos{rx<<regionShift | i&31, rz<<regionShift | i>>regionShift})
		return more
	})
	if err != nil {
		return false, fmt.Errorf("hx: region %d,%d of %s: %w", rx, rz, dim.Dir(), err)
	}
	return more, nil
}

// parseRegion reads "r.<x>.<z>.hx".
func parseRegion(name string) (x, z int32, ok bool) {
	rest, ok := strings.CutPrefix(name, "r.")
	if !ok {
		return 0, 0, false
	}
	if rest, ok = strings.CutSuffix(rest, ".hx"); !ok {
		return 0, 0, false
	}
	xs, zs, ok := strings.Cut(rest, ".")
	if !ok {
		return 0, 0, false
	}
	xi, err1 := strconv.ParseInt(xs, 10, 32)
	zi, err2 := strconv.ParseInt(zs, 10, 32)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return int32(xi), int32(zi), true
}
