package hxdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/cube"
	"github.com/helix-bedrock/world/hx"
	"github.com/helix-bedrock/world/mcdb"
)

// BDS 1.26.60 chunk key tags this provider reads or writes; the bytes are
// mcdb's (keys.go), which documents each.
const (
	tagVersion       = ','
	tagFinalisation  = '6'
	tagMetaDataHash  = '?'
	tagBlendingData  = '@'
	tagDigestVersion = 'A'
	// tagLegacyStates is Helix's own: states derived from neighbours still
	// hold an upgrade's defaults.
	tagLegacyStates = '|'
)

// extraTags are the chunk keys kept raw in chunk.Column.Extra.
var extraTags = [...]byte{'5', '8', '9', '<', '=', 'w', 'y', 'z'}

const (
	// chunkVersion is the chunk format version BDS 1.26.60 writes.
	chunkVersion = 42
	// legacyEntitiesBelow is the chunk version below which BDS still reads
	// entities from the old '2' key.
	legacyEntitiesBelow = 40
	// heightMapSize is the 16x16 int16 height map in front of the biomes.
	heightMapSize = 512
)

// blendingNone is the blending data BDS writes for a chunk it generates.
var blendingNone = []byte{0, 11}

// scratch is the memory one load or store works in, pooled.
type scratch struct {
	rec, obj, shell []byte
	terrain         hx.Terrain
	objects         hx.Objects
	keys            []hx.ChunkKey
	data3D          []byte
	head, ids       []byte
	enc             bytes.Buffer
}

func (db *DB) getScratch() *scratch  { return db.scratch.Get().(*scratch) } //nolint:forcetypeassert // only scratches go in
func (db *DB) putScratch(s *scratch) { db.scratch.Put(s) }

// LoadColumn reads the column at pos in dim, or returns an error matching
// mcdb.ErrNotFound if there is none.
func (db *DB) LoadColumn(pos mcdb.ChunkPos, dim mcdb.Dimension) (*chunk.Column, error) {
	if db.conf.Blocks == nil {
		return nil, fmt.Errorf("hxdb: load column %v: no block registry", pos)
	}
	if err := db.settleChunk(chunkID{dim, pos}); err != nil {
		return nil, err
	}
	s := db.getScratch()
	defer db.putScratch(s)
	col, err := db.loadColumn(s, hx.Dimension(dim), hx.ChunkPos(pos))
	if err != nil {
		return nil, fmt.Errorf("hxdb: load column %v (%v): %w", pos, dim, notFound(err))
	}
	return col, nil
}

func (db *DB) loadColumn(s *scratch, dim hx.Dimension, pos hx.ChunkPos) (*chunk.Column, error) {
	c, rec, err := db.w.LoadChunk(dim, pos, s.rec, &s.terrain)
	s.rec = rec
	if err != nil {
		return nil, err
	}
	t := &s.terrain
	col := &chunk.Column{Chunk: c, Finalisation: chunk.DefaultFinalisation()}
	if len(t.Data3D) >= heightMapSize {
		col.HeightMap = make([]int16, heightMapSize/2)
		for i := range col.HeightMap {
			col.HeightMap[i] = int16(binary.LittleEndian.Uint16(t.Data3D[2*i:])) //nolint:gosec // raw int16 bits
		}
	}
	ver := byte(chunkVersion)
	if v, ok := t.Key(tagVersion); ok && len(v) == 1 {
		ver = v[0]
	}
	if err := readTerrainKeys(t, col); err != nil {
		return nil, err
	}
	if err := db.loadObjects(s, dim, pos, col, ver); err != nil {
		return nil, err
	}
	return col, nil
}

// readTerrainKeys fills the column fields kept in the terrain record's chunk
// keys.
func readTerrainKeys(t *hx.Terrain, col *chunk.Column) error {
	if _, ok := t.Key(tagLegacyStates); ok {
		col.Chunk.MarkLegacyStates()
	}
	if v, ok := t.Key(tagFinalisation); ok {
		if len(v) != 4 {
			return fmt.Errorf("finalisation of %d bytes", len(v))
		}
		col.Finalisation = chunk.Finalisation(int32(binary.LittleEndian.Uint32(v))) //nolint:gosec // raw int32 bits
	}
	if v, ok := t.Key(tagMetaDataHash); ok {
		if len(v) != 8 {
			return fmt.Errorf("metadata hash of %d bytes", len(v))
		}
		col.MetaDataHash = binary.LittleEndian.Uint64(v)
	}
	if v, ok := t.Key(tagBlendingData); ok {
		col.BlendingData = slices.Clone(v)
	}
	for _, tag := range extraTags {
		if v, ok := t.Key(tag); ok {
			if col.Extra == nil {
				col.Extra = map[byte][]byte{}
			}
			col.Extra[tag] = slices.Clone(v)
		}
	}
	return nil
}

// loadObjects fills the column's tick queues, entities and (unless skipped)
// block entities from its objects record.
func (db *DB) loadObjects(s *scratch, dim hx.Dimension, pos hx.ChunkPos, col *chunk.Column, ver byte) error {
	rec, err := db.w.ObjectsRecord(dim, pos, s.obj)
	s.obj = rec
	if errors.Is(err, hx.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	o := &s.objects
	if err := hx.DecodeObjects(rec, o); err != nil {
		return err
	}
	if v, ok := o.Key(hx.TagPendingTicks); ok {
		if col.ScheduledBlocks, col.Tick, err = db.decodeTicks(v); err != nil {
			return fmt.Errorf("scheduled updates: %w", err)
		}
	}
	if v, ok := o.Key(hx.TagRandomTicks); ok {
		if col.RandomTicks, col.RandomTick, err = db.decodeTicks(v); err != nil {
			return fmt.Errorf("random ticks: %w", err)
		}
	}
	if !db.conf.SkipBlockEntities {
		if err := db.eachShell(s, dim, pos, o.BlockEntities, func(c []byte) error {
			m, err := nbt.DecodeWithout(c, nbt.LittleEndian, nil)
			if err != nil {
				return err
			}
			col.BlockEntities = append(col.BlockEntities, chunk.BlockEntity{Pos: blockPosFromNBT(m), Data: m})
			return nil
		}); err != nil {
			return fmt.Errorf("block entities: %w", err)
		}
	}
	if !o.Digp && ver < legacyEntitiesBelow {
		if v, ok := o.Key(hx.TagLegacyEntities); ok {
			col.Entities, err = db.legacyEntities(v)
			return err
		}
	}
	col.Entities = make([]chunk.Entity, 0, len(o.Entities))
	for _, e := range o.Entities {
		if e.Shell.Size() == 0 {
			// An ID in BDS's digp list with no record.
			continue
		}
		if s.shell, err = db.w.ExpandShell(dim, pos, e.Shell, s.shell[:0]); err != nil {
			return fmt.Errorf("entity: %w", err)
		}
		ent := chunk.Entity{ID: mcdb.ActorIDFromKey(e.ID), Data: map[string]any{}}
		if err := nbt.UnmarshalEncoding(s.shell, &ent.Data, nbt.LittleEndian); err != nil {
			db.conf.Log.Error("hxdb: decode entity: "+err.Error(), "ID", ent.ID)
			continue
		}
		col.Entities = append(col.Entities, ent)
	}
	return nil
}

// eachShell calls fn with the whole NBT of each shell, expanded into the
// scratch buffer; it is valid only during the call.
func (db *DB) eachShell(s *scratch, dim hx.Dimension, pos hx.ChunkPos, shells []hx.Shell, fn func(c []byte) error) error {
	var err error
	for _, sh := range shells {
		if s.shell, err = db.w.ExpandShell(dim, pos, sh, s.shell[:0]); err != nil {
			return err
		}
		if err := fn(s.shell); err != nil {
			return err
		}
	}
	return nil
}

// legacyEntities reads the old '2' key: entity compounds back to back.
func (db *DB) legacyEntities(data []byte) ([]chunk.Entity, error) {
	var entities []chunk.Entity
	buf := bytes.NewBuffer(data)
	dec := nbt.NewDecoderWithEncoding(buf, nbt.LittleEndian)
	for buf.Len() != 0 {
		ent := chunk.Entity{Data: map[string]any{}}
		if err := dec.Decode(&ent.Data); err != nil {
			return nil, fmt.Errorf("legacy entities: %w", err)
		}
		id, ok := ent.Data["UniqueID"].(int64)
		if !ok {
			id = rand.Int64() //nolint:gosec // an entity ID, not a secret
		}
		ent.ID = id
		entities = append(entities, ent)
	}
	return entities, nil
}

// ticks is a tick queue as BDS stores it.
type ticks struct {
	CurrentTick int32            `nbt:"currentTick"`
	TickList    []map[string]any `nbt:"tickList"`
}

func (db *DB) decodeTicks(data []byte) ([]chunk.ScheduledBlockUpdate, int64, error) {
	var m ticks
	if err := nbt.UnmarshalEncoding(data, &m, nbt.LittleEndian); err != nil {
		return nil, 0, err
	}
	bpe := chunk.BlockPaletteEncoding{Blocks: db.conf.Blocks, Upgrader: db.conf.Upgrader}
	updates := make([]chunk.ScheduledBlockUpdate, 0, len(m.TickList))
	for _, tick := range m.TickList {
		t, _ := tick["time"].(int64)
		bl, _ := tick["blockState"].(map[string]any)
		block, err := bpe.DecodeBlockState(bl)
		if err != nil {
			db.conf.Log.Error("hxdb: tick queue: decode block state: " + err.Error())
			continue
		}
		updates = append(updates, chunk.ScheduledBlockUpdate{Pos: blockPosFromNBT(tick), Block: block, Tick: t})
	}
	return updates, int64(m.CurrentTick), nil
}

func (db *DB) encodeTicks(tick int64, updates []chunk.ScheduledBlockUpdate) ([]byte, error) {
	list := make([]map[string]any, len(updates))
	bpe := chunk.BlockPaletteEncoding{Blocks: db.conf.Blocks, Upgrader: db.conf.Upgrader}
	for i, u := range updates {
		list[i] = map[string]any{
			"x": int32(u.Pos[0]), "y": int32(u.Pos[1]), "z": int32(u.Pos[2]), //nolint:gosec // block coordinates fit in int32
			"time": u.Tick, "blockState": bpe.EncodeBlockState(u.Block),
		}
	}
	return nbt.MarshalEncoding(ticks{CurrentTick: int32(tick), TickList: list}, nbt.LittleEndian) //nolint:gosec // BDS stores the tick as int32
}

// blockPosFromNBT reads a block entity's or tick's x, y and z.
func blockPosFromNBT(m map[string]any) cube.Pos {
	x, _ := m["x"].(int32)
	y, _ := m["y"].(int32)
	z, _ := m["z"].(int32)
	return cube.Pos{int(x), int(y), int(z)}
}

// StoreColumn saves a column in the round being filled. A column stored again
// before the round holding it is committed commits that round first: the
// objects of a chunk are written against what is on disk. The column's data
// is encoded before StoreColumn returns.
func (db *DB) StoreColumn(pos mcdb.ChunkPos, dim mcdb.Dimension, col *chunk.Column) error {
	if db.conf.Blocks == nil {
		return fmt.Errorf("hxdb: store column %v: no block registry", pos)
	}
	if _, ok := dim.Range(); !ok {
		return fmt.Errorf("hxdb: store column %v: unknown dimension %v", pos, dim)
	}
	if col == nil || col.Chunk == nil {
		return fmt.Errorf("hxdb: store column %v (%v): no chunk", pos, dim)
	}
	id := chunkID{dim, pos}
	if err := db.settleChunk(id); err != nil {
		return err
	}
	s := db.getScratch()
	defer db.putScratch(s)
	if err := db.terrainKeys(s, dim, col); err != nil {
		return fmt.Errorf("hxdb: store column %v (%v): %w", pos, dim, err)
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.storeColumn(s, hx.Dimension(dim), hx.ChunkPos(pos), col); err != nil {
		return fmt.Errorf("hxdb: store column %v (%v): %w", pos, dim, err)
	}
	db.held.chunks[id] = struct{}{}
	db.grown()
	return nil
}

// terrainKeys builds the small chunk keys of the terrain record and the saved
// height map, as BDS writes them on a save.
func (db *DB) terrainKeys(s *scratch, dim mcdb.Dimension, col *chunk.Column) error {
	hash, err := db.meta.SavedHash(col.MetaDataHash, dim, db.ldat.RandomSeed, db.ldat.Generator)
	if err != nil {
		return fmt.Errorf("chunk metadata: %w", err)
	}
	blending := col.BlendingData
	if blending == nil {
		blending = blendingNone
	}
	s.head = s.head[:0]
	s.head = append(s.head, chunkVersion)
	s.head = binary.LittleEndian.AppendUint64(s.head, hash)
	s.head = binary.LittleEndian.AppendUint32(s.head, uint32(col.Finalisation)) //nolint:gosec // raw int32 bits
	s.keys = append(s.keys[:0],
		hx.ChunkKey{Tag: tagVersion, Value: s.head[0:1]},
		hx.ChunkKey{Tag: tagMetaDataHash, Value: s.head[1:9]},
		hx.ChunkKey{Tag: tagBlendingData, Value: blending},
		hx.ChunkKey{Tag: tagFinalisation, Value: s.head[9:13]},
		hx.ChunkKey{Tag: tagDigestVersion, Value: []byte{0}},
	)
	if col.Chunk.LegacyStates() {
		s.keys = append(s.keys, hx.ChunkKey{Tag: tagLegacyStates, Value: []byte{1}})
	}
	for _, tag := range extraTags {
		if v, ok := col.Extra[tag]; ok {
			s.keys = append(s.keys, hx.ChunkKey{Tag: tag, Value: v})
		}
	}
	s.data3D = s.data3D[:0]
	if len(col.HeightMap) == heightMapSize/2 {
		for _, h := range col.HeightMap {
			s.data3D = binary.LittleEndian.AppendUint16(s.data3D, uint16(h)) //nolint:gosec // raw int16 bits
		}
	}
	return nil
}

// storeColumn puts the column's terrain and objects records in the round. mu
// must be held.
func (db *DB) storeColumn(s *scratch, dim hx.Dimension, pos hx.ChunkPos, col *chunk.Column) error {
	var saved []byte
	if len(s.data3D) > 0 {
		saved = s.data3D
	}
	if err := db.round.PutChunk(dim, pos, col.Chunk, s.keys, saved); err != nil {
		return err
	}
	ow, err := db.round.Objects(dim, pos)
	if err != nil {
		return err
	}
	if err := db.keepBlockEntities(s, dim, pos, ow, col); err != nil {
		return fmt.Errorf("block entities: %w", err)
	}
	if !col.KeepBlockEntities {
		for _, be := range col.BlockEntities {
			if err := db.putBlockEntity(s, ow, be); err != nil {
				return fmt.Errorf("block entity at %v: %w", be.Pos, err)
			}
		}
	}
	if err := db.putTicks(ow, hx.TagPendingTicks, col.Tick, col.ScheduledBlocks); err != nil {
		return fmt.Errorf("scheduled updates: %w", err)
	}
	if err := db.putTicks(ow, hx.TagRandomTicks, col.RandomTick, col.RandomTicks); err != nil {
		return fmt.Errorf("random ticks: %w", err)
	}
	// BDS writes a digp key with every save, also an empty one.
	ow.Digp()
	for _, e := range col.Entities {
		if err := db.putEntity(s, ow, e); err != nil {
			db.conf.Log.Error("hxdb: store entity: "+err.Error(), "ID", e.ID)
		}
	}
	return ow.Close()
}

// keepBlockEntities copies the stored shells of the block entities the column
// keeps (all with KeepBlockEntities, else those at KeepBlockEntitiesAt),
// leaving their payloads on disk.
func (db *DB) keepBlockEntities(s *scratch, dim hx.Dimension, pos hx.ChunkPos, ow *hx.ObjectsWriter, col *chunk.Column) error {
	if !col.KeepBlockEntities && len(col.KeepBlockEntitiesAt) == 0 {
		return nil
	}
	rec, err := db.w.ObjectsRecord(dim, pos, s.obj)
	s.obj = rec
	if errors.Is(err, hx.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	o := &s.objects
	if err := hx.DecodeObjects(rec, o); err != nil {
		return err
	}
	for _, sh := range o.BlockEntities {
		if !col.KeepBlockEntities {
			at, err := shellPos(sh)
			if err != nil {
				return err
			}
			if !slices.Contains(col.KeepBlockEntitiesAt, at) {
				continue
			}
		}
		if err := ow.KeepBlockEntity(sh); err != nil {
			return err
		}
	}
	return nil
}

// shellPos reads a block entity's position from its shell: the coordinates
// are small fields, never cut.
func shellPos(sh hx.Shell) (cube.Pos, error) {
	m, err := nbt.DecodeWithout(sh.Data, nbt.LittleEndian, notBlockPos)
	if err != nil {
		return cube.Pos{}, err
	}
	return blockPosFromNBT(m), nil
}

// notBlockPos is every tag name but a block entity's x, y and z.
func notBlockPos(name string) bool { return name != "x" && name != "y" && name != "z" }

func (db *DB) putBlockEntity(s *scratch, ow *hx.ObjectsWriter, be chunk.BlockEntity) error {
	if be.Raw != nil {
		return ow.BlockEntity(be.Raw)
	}
	s.enc.Reset()
	if err := nbt.NewEncoderWithEncoding(&s.enc, nbt.LittleEndian).Encode(be.Data); err != nil {
		return err
	}
	return ow.BlockEntity(s.enc.Bytes())
}

func (db *DB) putEntity(s *scratch, ow *hx.ObjectsWriter, e chunk.Entity) error {
	if e.Data == nil {
		e.Data = map[string]any{}
	}
	e.Data["UniqueID"] = e.ID
	s.enc.Reset()
	if err := nbt.NewEncoderWithEncoding(&s.enc, nbt.LittleEndian).Encode(e.Data); err != nil {
		return err
	}
	s.ids = mcdb.AppendActorKey(s.ids[:0], e.ID)
	return ow.Entity(s.ids, s.enc.Bytes())
}

// putTicks writes a tick queue, or nothing when it is empty, as BDS does.
func (db *DB) putTicks(ow *hx.ObjectsWriter, tag byte, tick int64, updates []chunk.ScheduledBlockUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	b, err := db.encodeTicks(tick, updates)
	if err != nil {
		return err
	}
	ow.Key(tag, b)
	return nil
}
