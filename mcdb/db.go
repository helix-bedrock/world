// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/world/mcdb/db.go
//
// Changed for Helix: plain ChunkPos/Dimension types instead of engine ones,
// level.dat data exposed directly, player IDs as strings, no panics, and
// length checks on data read from disk.

package mcdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/cube"
	"github.com/helix-bedrock/world/leveldb"
	"github.com/helix-bedrock/world/mcdb/leveldat"
)

// DB reads and writes a Bedrock world: the LevelDB database in its db
// folder and its level.dat.
//
// LoadColumn, StoreColumn and the player methods may be called from any
// goroutine; LevelDB orders concurrent access. Callers must not store the
// same column from two goroutines at once. LevelDat and Close must not run
// concurrently with anything else.
type DB struct {
	conf Config
	ldb  *leveldb.DB
	dir  string
	ldat *leveldat.Data
	// ldatFile keeps every level.dat field, including ones Data does not
	// know, so they are written back unchanged.
	ldatFile *leveldat.LevelDat
}

// Open opens the world in dir with the block registry passed and default
// options. See Config.Open.
func Open(dir string, blocks chunk.BlockRegistry) (*DB, error) {
	return Config{Blocks: blocks}.Open(dir)
}

// LevelDat returns the world's level.dat values. Changes are saved by Close.
func (db *DB) LevelDat() *leveldat.Data {
	return db.ldat
}

// playerData holds the fields that indicate where player data is stored for a player with a specific UUID.
type playerData struct {
	UUID         string `nbt:"MsaId"`
	ServerID     string `nbt:"ServerId"`
	SelfSignedID string `nbt:"SelfSignedId"`
}

// LoadPlayerSpawnPosition loads the players spawn position stored in the level.dat from their UUID.
// The id is the player's UUID in its usual text form.
func (db *DB) LoadPlayerSpawnPosition(id string) (pos cube.Pos, exists bool, err error) {
	serverData, _, exists, err := db.loadPlayerData(id)
	if !exists || err != nil {
		return cube.Pos{}, exists, err
	}
	x, okX := serverData["SpawnX"].(int32)
	y, okY := serverData["SpawnY"].(int32)
	z, okZ := serverData["SpawnZ"].(int32)
	if !okX || !okY || !okZ {
		return cube.Pos{}, true, fmt.Errorf("error reading spawn fields from server data for player %v", id)
	}
	return cube.Pos{int(x), int(y), int(z)}, true, nil
}

// loadPlayerData loads the data stored in a LevelDB database for a specific UUID.
func (db *DB) loadPlayerData(id string) (serverData map[string]any, key string, exists bool, err error) {
	data, err := db.ldb.Get([]byte("player_"+id), nil)
	if errors.Is(err, leveldb.ErrNotFound) {
		return nil, "", false, nil
	} else if err != nil {
		return nil, "", true, fmt.Errorf("error reading player data for uuid %v: %w", id, err)
	}

	var d playerData
	if err := nbt.UnmarshalEncoding(data, &d, nbt.LittleEndian); err != nil {
		return nil, "", true, fmt.Errorf("error decoding player data for uuid %v: %w", id, err)
	}
	if d.UUID != id || d.ServerID == "" {
		return nil, d.ServerID, true, fmt.Errorf("invalid player data for uuid %v: %v", id, d)
	}
	serverDB, err := db.ldb.Get([]byte(d.ServerID), nil)
	if err != nil {
		return nil, d.ServerID, true, fmt.Errorf("error reading server data for player %v (%v): %w", id, d.ServerID, err)
	}

	if err := nbt.UnmarshalEncoding(serverDB, &serverData, nbt.LittleEndian); err != nil {
		return nil, d.ServerID, true, fmt.Errorf("error decoding server data for player %v", id)
	}
	return serverData, d.ServerID, true, nil
}

// SavePlayerSpawnPosition saves the player spawn position passed to the levelDB database.
func (db *DB) SavePlayerSpawnPosition(id string, pos cube.Pos) error {
	_, err := db.ldb.Get([]byte("player_"+id), nil)
	d := make(map[string]any)
	k := "player_server_" + id

	if errors.Is(err, leveldb.ErrNotFound) {
		data, err := nbt.MarshalEncoding(playerData{UUID: id, ServerID: k}, nbt.LittleEndian)
		if err != nil {
			return fmt.Errorf("encode player data (uuid=%v): %w", id, err)
		}
		if err := db.ldb.Put([]byte("player_"+id), data, nil); err != nil {
			return fmt.Errorf("write player data (uuid=%v): %w", id, err)
		}
	} else if err != nil {
		return fmt.Errorf("read player data (uuid=%v): %w", id, err)
	} else if d, k, _, err = db.loadPlayerData(id); err != nil {
		return err
	}
	d["SpawnX"], d["SpawnY"], d["SpawnZ"] = int32(pos.X()), int32(pos.Y()), int32(pos.Z())

	data, err := nbt.MarshalEncoding(d, nbt.LittleEndian)
	if err != nil {
		return fmt.Errorf("encode server data for player %v: %w", id, err)
	}
	if err = db.ldb.Put([]byte(k), data, nil); err != nil {
		return fmt.Errorf("write server data for player %v: %w", id, err)
	}
	return nil
}

// LoadColumn reads the column at a position in a dimension. If no column
// exists there, errors.Is(err, leveldb.ErrNotFound) is true.
func (db *DB) LoadColumn(pos ChunkPos, dim Dimension) (*chunk.Column, error) {
	if _, ok := dim.Range(); !ok {
		return nil, fmt.Errorf("load column %v: unknown dimension %v", pos, dim)
	}
	k := dbKey{pos: pos, dim: dim}
	col, err := db.column(k)
	if err != nil {
		return nil, fmt.Errorf("load column %v (%v): %w", pos, dim, err)
	}
	return col, nil
}

const chunkVersion = 42

func (db *DB) column(k dbKey) (*chunk.Column, error) {
	var cdata chunk.SerialisedData
	col := new(chunk.Column)

	ver, err := db.version(k)
	if err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	if ver != chunkVersion {
		db.conf.Log.Debug("column: unsupported chunk version, trying to load anyway", "X", k.pos[0], "Z", k.pos[1], "dimension", fmt.Sprint(k.dim), "ver", ver)
	}
	cdata.Biomes, err = db.biomes(k)
	if err != nil && !errors.Is(err, leveldb.ErrNotFound) {
		// Some chunks still use 2D chunk data and might not have this field, in
		// which case we can just move on.
		return nil, fmt.Errorf("read biomes: %w", err)
	}
	cdata.SubChunks, err = db.subChunks(k)
	if err != nil {
		return nil, fmt.Errorf("read sub chunks: %w", err)
	}
	r, _ := k.dim.Range()
	col.Chunk, err = chunk.DiskDecode(db.conf.Blocks, db.conf.Upgrader, cdata, r)
	if err != nil {
		return nil, fmt.Errorf("decode chunk data: %w", err)
	}
	legacy, err := db.legacyStates(k)
	if err != nil {
		return nil, fmt.Errorf("read legacy states: %w", err)
	}
	if legacy {
		col.Chunk.MarkLegacyStates()
	}
	col.Entities, err = db.entities(k)
	if err != nil && !errors.Is(err, leveldb.ErrNotFound) {
		// Not all chunks need to have entities, so an ErrNotFound is fine here.
		return nil, fmt.Errorf("read entities: %w", err)
	}
	col.BlockEntities, err = db.blockEntities(k)
	if err != nil && !errors.Is(err, leveldb.ErrNotFound) {
		// Same as with entities, an ErrNotFound is fine here.
		return nil, fmt.Errorf("read block entities: %w", err)
	}
	col.ScheduledBlocks, col.Tick, err = db.scheduledUpdates(k)
	if err != nil && !errors.Is(err, leveldb.ErrNotFound) {
		return nil, fmt.Errorf("read scheduled updates: %w", err)
	}
	return col, nil
}

// legacyStates reports whether the chunk was saved with keyLegacyStates present.
func (db *DB) legacyStates(k dbKey) (bool, error) {
	_, err := db.ldb.Get(k.Sum(keyLegacyStates), nil)
	if errors.Is(err, leveldb.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (db *DB) version(k dbKey) (byte, error) {
	p, err := db.ldb.Get(k.Sum(keyVersion), nil)
	if errors.Is(err, leveldb.ErrNotFound) {
		// Although the version at `keyVersion` may not be found, there is
		// another `keyVersionOld` where the version may be found.
		if p, err = db.ldb.Get(k.Sum(keyVersionOld), nil); err != nil {
			return 0, err
		}
	}
	if err != nil {
		return 0, err
	}
	if n := len(p); n != 1 {
		return 0, fmt.Errorf("expected 1 version byte, got %v", n)
	}
	return p[0], nil
}

func (db *DB) biomes(k dbKey) ([]byte, error) {
	biomes, err := db.ldb.Get(k.Sum(key3DData), nil)
	if err != nil {
		return nil, err
	}
	// The first 512 bytes is a heightmap (16*16 int16s), the biomes follow. We
	// calculate a heightmap on startup so the heightmap can be discarded.
	if n := len(biomes); n <= 512 {
		return nil, fmt.Errorf("expected at least 513 bytes for 3D data, got %v", n)
	}
	return biomes[512:], nil
}

func (db *DB) subChunks(k dbKey) ([][]byte, error) {
	r, _ := k.dim.Range()
	sub := make([][]byte, (r.Height()>>4)+1)

	var err error
	for i := range sub {
		y := uint8(i + (r[0] >> 4))
		sub[i], err = db.ldb.Get(k.Sum(keySubChunkData, y), nil)
		if errors.Is(err, leveldb.ErrNotFound) {
			// No sub chunk present at this Y level. We skip this one and move
			// to the next, which might still be present.
			continue
		} else if err != nil {
			return nil, fmt.Errorf("sub chunk %v: %w", int8(i), err)
		}
	}
	return sub, nil
}

func (db *DB) entities(k dbKey) ([]chunk.Entity, error) {
	// https://learn.microsoft.com/en-us/minecraft/creator/documents/actorstorage
	ids, err := db.ldb.Get(append([]byte(keyEntityIdentifiers), index(k.pos, k.dim)...), nil)
	if err != nil {
		// Key not found, try old method of loading entities.
		return db.entitiesOld(k)
	}
	if len(ids)%8 != 0 {
		return nil, fmt.Errorf("entity ID list has %d bytes, not a multiple of 8", len(ids))
	}
	entities := make([]chunk.Entity, 0, len(ids)/8)
	for i := 0; i < len(ids); i += 8 {
		id := int64(binary.LittleEndian.Uint64(ids[i : i+8]))
		data, err := db.ldb.Get(entityIndex(id), nil)
		if err != nil {
			db.conf.Log.Error("read entity: "+err.Error(), "ID", id)
			return nil, err
		}
		ent := chunk.Entity{ID: id, Data: make(map[string]any)}
		if err = nbt.UnmarshalEncoding(data, &ent.Data, nbt.LittleEndian); err != nil {
			db.conf.Log.Error("decode entity nbt: "+err.Error(), "ID", id)
		}
		entities = append(entities, ent)
	}
	return entities, nil
}

func (db *DB) entitiesOld(k dbKey) ([]chunk.Entity, error) {
	data, err := db.ldb.Get(k.Sum(keyEntitiesOld), nil)
	if err != nil {
		return nil, err
	}
	var entities []chunk.Entity

	buf := bytes.NewBuffer(data)
	dec, ok := nbt.NewDecoderWithEncoding(buf, nbt.LittleEndian), false

	for buf.Len() != 0 {
		ent := chunk.Entity{Data: make(map[string]any)}
		if err := dec.Decode(&ent.Data); err != nil {
			return nil, fmt.Errorf("decode entity nbt: %w", err)
		}
		ent.ID, ok = ent.Data["UniqueID"].(int64)
		if !ok {
			db.conf.Log.Error("missing unique ID field, generating random", "data", fmt.Sprint(ent.Data))
			ent.ID = rand.Int64() //nolint:gosec // an entity ID, not a secret
		}
		entities = append(entities, ent)
	}
	return entities, nil
}

func (db *DB) blockEntities(k dbKey) ([]chunk.BlockEntity, error) {
	var blockEntities []chunk.BlockEntity

	data, err := db.ldb.Get(k.Sum(keyBlockEntities), nil)
	if err != nil {
		return blockEntities, err
	}

	buf := bytes.NewBuffer(data)
	dec := nbt.NewDecoderWithEncoding(buf, nbt.LittleEndian)

	for buf.Len() != 0 {
		be := chunk.BlockEntity{Data: make(map[string]any)}
		if err := dec.Decode(&be.Data); err != nil {
			return blockEntities, fmt.Errorf("decode nbt: %w", err)
		}
		be.Pos = blockPosFromNBT(be.Data)
		blockEntities = append(blockEntities, be)
	}
	return blockEntities, nil
}

func (db *DB) scheduledUpdates(k dbKey) ([]chunk.ScheduledBlockUpdate, int64, error) {
	data, err := db.ldb.Get(k.Sum(keyPendingScheduledTicks), nil)
	if err != nil {
		return nil, 0, err
	}
	var m scheduledUpdates
	if err := nbt.UnmarshalEncoding(data, &m, nbt.LittleEndian); err != nil {
		return nil, 0, fmt.Errorf("read nbt: %w", err)
	}
	updates := make([]chunk.ScheduledBlockUpdate, 0, len(m.TickList))
	for _, tick := range m.TickList {
		t, _ := tick["time"].(int64)
		bl, _ := tick["blockState"].(map[string]any)
		bpe := chunk.BlockPaletteEncoding{Blocks: db.conf.Blocks, Upgrader: db.conf.Upgrader}
		block, err := bpe.DecodeBlockState(bl)
		if err != nil {
			db.conf.Log.Error("read scheduled updates: decode block state: " + err.Error())
			continue
		}
		updates = append(updates, chunk.ScheduledBlockUpdate{Pos: blockPosFromNBT(tick), Block: block, Tick: t})
	}
	return updates, int64(m.CurrentTick), nil
}

// StoreColumn stores a column at a position in a dimension.
func (db *DB) StoreColumn(pos ChunkPos, dim Dimension, col *chunk.Column) error {
	if _, ok := dim.Range(); !ok {
		return fmt.Errorf("store column %v: unknown dimension %v", pos, dim)
	}
	if col == nil || col.Chunk == nil {
		return fmt.Errorf("store column %v (%v): no chunk", pos, dim)
	}
	k := dbKey{pos: pos, dim: dim}
	if err := db.storeColumn(k, col); err != nil {
		return fmt.Errorf("store column %v (%v): %w", pos, dim, err)
	}
	return nil
}

func (db *DB) storeColumn(k dbKey, col *chunk.Column) error {
	data := chunk.Encode(col.Chunk, chunk.DiskEncoding)
	n := 8 + len(data.SubChunks) + len(col.Entities)
	batch := leveldb.MakeBatch(n)

	db.storeVersion(batch, k, chunkVersion)
	db.storeBiomes(batch, k, data.Biomes)
	db.storeSubChunks(batch, k, data.SubChunks, col.Chunk.Range())
	db.storeFinalisation(batch, k, finalisationPopulated)
	db.storeLegacyStates(batch, k, col.Chunk.LegacyStates())
	db.storeEntities(batch, k, col.Entities)
	db.storeBlockEntities(batch, k, col.BlockEntities)
	db.storeScheduledUpdates(batch, k, col.Tick, col.ScheduledBlocks)

	return db.ldb.Write(batch, nil)
}

func (db *DB) storeVersion(batch *leveldb.Batch, k dbKey, ver uint8) {
	batch.Put(k.Sum(keyVersion), []byte{ver})
}

// heightMapSize is the 16x16 int16 height map in front of the biomes. It is
// written as zeros; the height map is recalculated on load.
const heightMapSize = 512

func (db *DB) storeBiomes(batch *leveldb.Batch, k dbKey, biomes []byte) {
	data := make([]byte, heightMapSize, heightMapSize+len(biomes))
	batch.Put(k.Sum(key3DData), append(data, biomes...))
}

func (db *DB) storeSubChunks(batch *leveldb.Batch, k dbKey, subChunks [][]byte, r cube.Range) {
	for i, sub := range subChunks {
		batch.Put(k.Sum(keySubChunkData, byte(i+(r[0]>>4))), sub)
	}
}

func (db *DB) storeFinalisation(batch *leveldb.Batch, k dbKey, finalisation uint32) {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, finalisation)
	batch.Put(k.Sum(keyFinalisation), p)
}

// storeLegacyStates keeps keyLegacyStates while the chunk's derived states are still pending and removes
// it once they were recalculated, so a chunk saved with defaulted states is not mistaken for an upgraded one.
func (db *DB) storeLegacyStates(batch *leveldb.Batch, k dbKey, legacy bool) {
	key := k.Sum(keyLegacyStates)
	if !legacy {
		batch.Delete(key)
		return
	}
	batch.Put(key, []byte{1})
}

func (db *DB) storeEntities(batch *leveldb.Batch, k dbKey, entities []chunk.Entity) {
	idsKey := append([]byte(keyEntityIdentifiers), index(k.pos, k.dim)...)

	// load the ids of the previous entities
	var previousIDs []int64
	digpPrev, err := db.ldb.Get(idsKey, nil)
	if err != nil && !errors.Is(err, leveldb.ErrNotFound) {
		db.conf.Log.Error("store entities: read chunk entity IDs: " + err.Error())
	}
	if err == nil {
		for i := 0; i+8 <= len(digpPrev); i += 8 {
			previousIDs = append(previousIDs, int64(binary.LittleEndian.Uint64(digpPrev[i:])))
		}
	}

	newIDs := make([]int64, 0, len(entities))
	for _, e := range entities {
		e.Data["UniqueID"] = e.ID
		b, err := nbt.MarshalEncoding(e.Data, nbt.LittleEndian)
		if err != nil {
			db.conf.Log.Error("store entities: encode NBT: " + err.Error())
			continue
		}
		batch.Put(entityIndex(e.ID), b)
		newIDs = append(newIDs, e.ID)
	}

	// Remove entities that are no longer referenced.
	for _, uniqueID := range previousIDs {
		if !slices.Contains(newIDs, uniqueID) {
			batch.Delete(entityIndex(uniqueID))
		}
	}
	if len(entities) == 0 {
		batch.Delete(idsKey)
	} else {
		// Save the index of entities in the chunk.
		ids := make([]byte, 0, 8*len(newIDs))
		for _, uniqueID := range newIDs {
			ids = binary.LittleEndian.AppendUint64(ids, uint64(uniqueID))
		}
		batch.Put(idsKey, ids)
	}

	// Remove old entity data for this chunk.
	batch.Delete(k.Sum(keyEntitiesOld))
}

func entityIndex(id int64) []byte {
	return binary.LittleEndian.AppendUint64([]byte(keyEntity), uint64(id))
}

func (db *DB) storeBlockEntities(batch *leveldb.Batch, k dbKey, blockEntities []chunk.BlockEntity) {
	if len(blockEntities) == 0 {
		batch.Delete(k.Sum(keyBlockEntities))
		return
	}

	buf := bytes.NewBuffer(nil)
	enc := nbt.NewEncoderWithEncoding(buf, nbt.LittleEndian)
	for _, b := range blockEntities {
		b.Data["x"], b.Data["y"], b.Data["z"] = int32(b.Pos[0]), int32(b.Pos[1]), int32(b.Pos[2])
		if err := enc.Encode(b.Data); err != nil {
			db.conf.Log.Error("store block entities: encode nbt: " + err.Error())
		}
	}
	batch.Put(k.Sum(keyBlockEntities), buf.Bytes())
}

func (db *DB) storeScheduledUpdates(batch *leveldb.Batch, k dbKey, tick int64, updates []chunk.ScheduledBlockUpdate) {
	if len(updates) == 0 {
		batch.Delete(k.Sum(keyPendingScheduledTicks))
		return
	}
	list := make([]map[string]any, len(updates))
	bpe := chunk.BlockPaletteEncoding{Blocks: db.conf.Blocks, Upgrader: db.conf.Upgrader}
	for i, update := range updates {
		list[i] = map[string]any{
			"x": int32(update.Pos[0]), "y": int32(update.Pos[1]), "z": int32(update.Pos[2]),
			"time": update.Tick, "blockState": bpe.EncodeBlockState(update.Block),
		}
	}
	b, err := nbt.MarshalEncoding(scheduledUpdates{CurrentTick: int32(tick), TickList: list}, nbt.LittleEndian)
	if err != nil {
		db.conf.Log.Error("store scheduled updates: encode nbt: " + err.Error())
		return
	}
	batch.Put(k.Sum(keyPendingScheduledTicks), b)
}

type scheduledUpdates struct {
	CurrentTick int32            `nbt:"currentTick"`
	TickList    []map[string]any `nbt:"tickList"`
}

// NewColumnIterator returns a ColumnIterator that may be used to iterate over all
// position/chunk pairs in a database.
// An IteratorRange r may be passed to specify limits in terms of what chunks
// should be read. r may be set to nil to read all chunks from the DB.
func (db *DB) NewColumnIterator(r *IteratorRange) *ColumnIterator {
	if r == nil {
		r = &IteratorRange{}
	}
	return newColumnIterator(db, r)
}

// Close closes the provider, saving any file that might need to be saved, such as the level.dat.
func (db *DB) Close() error {
	db.ldat.LastPlayed = time.Now().Unix()

	var errs []error
	if err := db.ldatFile.Marshal(*db.ldat); err != nil {
		errs = append(errs, fmt.Errorf("close: %w", err))
	} else if err := db.ldatFile.WriteFile(filepath.Join(db.dir, "level.dat")); err != nil {
		errs = append(errs, fmt.Errorf("close: %w", err))
	}
	if err := os.WriteFile(filepath.Join(db.dir, "levelname.txt"), []byte(db.ldat.LevelName), 0o644); err != nil { //nolint:gosec // world files are meant to be readable
		errs = append(errs, fmt.Errorf("close: write levelname.txt: %w", err))
	}
	// Always close the database, even if level.dat could not be saved.
	if err := db.ldb.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close: leveldb: %w", err))
	}
	return errors.Join(errs...)
}

// dbKey holds a position and dimension.
type dbKey struct {
	pos ChunkPos
	dim Dimension
}

// Sum converts k to its []byte representation and appends p.
func (k dbKey) Sum(p ...byte) []byte {
	return append(index(k.pos, k.dim), p...)
}

// index returns the key prefix for a chunk: X and Z, followed by the
// dimension ID for every dimension except the Overworld.
func index(position ChunkPos, dim Dimension) []byte {
	x, z := uint32(position[0]), uint32(position[1])
	b := make([]byte, 12)

	binary.LittleEndian.PutUint32(b, x)
	binary.LittleEndian.PutUint32(b[4:], z)
	if dim == 0 {
		return b[:8]
	}
	binary.LittleEndian.PutUint32(b[8:], uint32(dim))
	return b
}

// blockPosFromNBT returns a position from the X, Y and Z components stored in the NBT data map passed. The
// map is assumed to have an 'x', 'y' and 'z' key.
func blockPosFromNBT(data map[string]any) cube.Pos {
	x, _ := data["x"].(int32)
	y, _ := data["y"].(int32)
	z, _ := data["z"].(int32)
	return cube.Pos{int(x), int(y), int(z)}
}
