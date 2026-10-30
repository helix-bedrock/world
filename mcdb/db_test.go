package mcdb

import (
	"errors"
	"testing"

	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/cube"
	"github.com/helix-bedrock/world/leveldb"
)

// testRegistry knows air, stone and dirt.
type testRegistry struct{ names []string }

func newTestRegistry() *testRegistry {
	return &testRegistry{names: []string{"minecraft:air", "minecraft:stone", "minecraft:dirt"}}
}

func (r *testRegistry) BlockCount() int      { return len(r.names) }
func (r *testRegistry) AirRuntimeID() uint32 { return 0 }
func (r *testRegistry) RuntimeIDToState(id uint32) (string, map[string]any, bool) {
	if int(id) >= len(r.names) {
		return "", nil, false
	}
	return r.names[id], map[string]any{}, true
}

func (r *testRegistry) StateToRuntimeID(name string, props map[string]any) (uint32, bool) {
	if len(props) != 0 {
		return 0, false
	}
	for i, n := range r.names {
		if n == name {
			return uint32(i), true
		}
	}
	return 0, false
}
func (r *testRegistry) FilteringBlock(uint32) uint8           { return 0 }
func (r *testRegistry) LightBlock(uint32) uint8               { return 0 }
func (r *testRegistry) RandomTickBlock(uint32) bool           { return false }
func (r *testRegistry) NBTBlock(uint32) bool                  { return false }
func (r *testRegistry) LiquidDisplacingBlock(uint32) bool     { return false }
func (r *testRegistry) LiquidBlock(uint32) bool               { return false }
func (r *testRegistry) HashToRuntimeID(uint32) (uint32, bool) { return 0, false }
func (r *testRegistry) RuntimeIDToHash(uint32) (uint32, bool) { return 0, false }

func openTest(t *testing.T, dir string) *DB {
	t.Helper()
	db, err := Open(dir, newTestRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func testColumn(dim Dimension, salt int) *chunk.Column {
	r, _ := dim.Range()
	c := chunk.New(newTestRegistry(), r)
	for x := range uint8(16) {
		for z := range uint8(16) {
			c.SetBlock(x, int16(r.Min()), z, 0, 1)
			c.SetBlock(x, int16(r.Min()+1+salt%5), z, 0, 2)
		}
	}
	return &chunk.Column{
		Chunk:         c,
		Entities:      []chunk.Entity{{ID: int64(100 + salt), Data: map[string]any{"identifier": "minecraft:pig"}}},
		BlockEntities: []chunk.BlockEntity{{Pos: cube.Pos{1, r.Min(), 2}, Data: map[string]any{"id": "Chest"}}},
		Tick:          int64(salt),
		ScheduledBlocks: []chunk.ScheduledBlockUpdate{
			{Pos: cube.Pos{3, r.Min(), 4}, Block: 2, Tick: 42},
		},
	}
}

func sameColumn(t *testing.T, want, got *chunk.Column) {
	t.Helper()
	r := want.Chunk.Range()
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := r.Min(); y <= r.Max(); y++ {
				if a, b := want.Chunk.Block(x, int16(y), z, 0), got.Chunk.Block(x, int16(y), z, 0); a != b {
					t.Fatalf("block %d,%d,%d: %d != %d", x, y, z, a, b)
				}
			}
		}
	}
	if len(got.Entities) != 1 || got.Entities[0].ID != want.Entities[0].ID || got.Entities[0].Data["identifier"] != "minecraft:pig" {
		t.Fatalf("entities: %+v", got.Entities)
	}
	if len(got.BlockEntities) != 1 || got.BlockEntities[0].Pos != want.BlockEntities[0].Pos {
		t.Fatalf("block entities: %+v", got.BlockEntities)
	}
	if len(got.ScheduledBlocks) != 1 || got.ScheduledBlocks[0] != want.ScheduledBlocks[0] || got.Tick != want.Tick {
		t.Fatalf("scheduled: %+v tick %d", got.ScheduledBlocks, got.Tick)
	}
}

func TestStoreLoadColumn(t *testing.T) {
	dir := t.TempDir()
	db := openTest(t, dir)
	cols := map[Dimension]*chunk.Column{}
	for i, dim := range []Dimension{Overworld, Nether, End} {
		cols[dim] = testColumn(dim, i)
		if err := db.StoreColumn(ChunkPos{int32(i) - 5, 7}, dim, cols[dim]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.LoadColumn(ChunkPos{1000, 1000}, Overworld); !errors.Is(err, leveldb.ErrNotFound) {
		t.Fatalf("missing column: err = %v", err)
	}
	if err := db.StoreColumn(ChunkPos{}, Dimension(9), cols[Overworld]); err == nil {
		t.Fatal("unknown dimension: no error")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: everything must come back from disk.
	db = openTest(t, dir)
	defer func() { _ = db.Close() }()
	for i, dim := range []Dimension{Overworld, Nether, End} {
		got, err := db.LoadColumn(ChunkPos{int32(i) - 5, 7}, dim)
		if err != nil {
			t.Fatal(err)
		}
		sameColumn(t, cols[dim], got)
	}

	// The iterator finds all three, once each.
	it := db.NewColumnIterator(nil)
	defer it.Release()
	seen := map[Dimension]int{}
	for it.Next() {
		seen[it.Dimension()]++
	}
	if err := it.Error(); err != nil {
		t.Fatal(err)
	}
	if seen[Overworld] != 1 || seen[Nether] != 1 || seen[End] != 1 {
		t.Fatalf("iterator saw %v", seen)
	}
	nether := Nether
	it2 := db.NewColumnIterator(&IteratorRange{Dimension: &nether})
	defer it2.Release()
	n := 0
	for it2.Next() {
		n++
		if it2.Dimension() != Nether {
			t.Fatalf("range returned %v", it2.Dimension())
		}
	}
	if n != 1 {
		t.Fatalf("nether range saw %d columns", n)
	}
}

func TestStoreReplacesEntities(t *testing.T) {
	db := openTest(t, t.TempDir())
	defer func() { _ = db.Close() }()
	col := testColumn(Overworld, 0)
	if err := db.StoreColumn(ChunkPos{}, Overworld, col); err != nil {
		t.Fatal(err)
	}
	col.Entities = nil
	if err := db.StoreColumn(ChunkPos{}, Overworld, col); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadColumn(ChunkPos{}, Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entities) != 0 {
		t.Fatalf("removed entity came back: %+v", got.Entities)
	}
	if _, err := db.buf.get(entityIndex(100)); !errors.Is(err, leveldb.ErrNotFound) {
		t.Fatalf("entity record not deleted: %v", err)
	}
}

func TestPlayerSpawn(t *testing.T) {
	dir := t.TempDir()
	db := openTest(t, dir)
	const id = "4a2f3b1c-0000-4000-8000-000000000001"
	if _, ok, err := db.LoadPlayerSpawnPosition(id); ok || err != nil {
		t.Fatalf("unknown player: ok=%v err=%v", ok, err)
	}
	for _, pos := range []cube.Pos{{1, 70, -3}, {5, 64, 9}} {
		if err := db.SavePlayerSpawnPosition(id, pos); err != nil {
			t.Fatal(err)
		}
		got, ok, err := db.LoadPlayerSpawnPosition(id)
		if err != nil || !ok || got != pos {
			t.Fatalf("got %v %v %v, want %v", got, ok, err, pos)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLevelDatSaved(t *testing.T) {
	dir := t.TempDir()
	db := openTest(t, dir)
	db.LevelDat().LevelName = "Helix Test"
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, dir)
	defer func() { _ = db.Close() }()
	if got := db.LevelDat().LevelName; got != "Helix Test" {
		t.Fatalf("LevelName = %q", got)
	}
}

func TestCorruptEntityIDs(t *testing.T) {
	db := openTest(t, t.TempDir())
	defer func() { _ = db.Close() }()
	if err := db.StoreColumn(ChunkPos{}, Overworld, testColumn(Overworld, 0)); err != nil {
		t.Fatal(err)
	}
	// An ID list that is not a multiple of 8 bytes used to crash loading.
	key := append([]byte(keyEntityIdentifiers), index(ChunkPos{}, Overworld)...)
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := db.ldb.Put(key, []byte{1, 2, 3}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LoadColumn(ChunkPos{}, Overworld); err == nil {
		t.Fatal("no error for a corrupt entity ID list")
	}
	// Storing over it must not crash either.
	if err := db.StoreColumn(ChunkPos{}, Overworld, testColumn(Overworld, 0)); err != nil {
		t.Fatal(err)
	}
}

func TestOpenNeedsBlocks(t *testing.T) {
	if _, err := (Config{}).Open(t.TempDir()); err == nil {
		t.Fatal("no error without a block registry")
	}
}
