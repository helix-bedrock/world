package hxdb

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/cube"
	"github.com/helix-bedrock/world/hx/store"
	"github.com/helix-bedrock/world/mcdb"
)

// testRegistry is a small BlockRegistry: air plus a few named blocks, some
// with properties. Copied from chunk/chunk_test.go.
type testRegistry struct {
	names []string
	props []map[string]any
	ids   map[string]uint32
}

func stateKey(name string, props map[string]any) string {
	ks := make([]string, 0, len(props))
	for k, v := range props {
		ks = append(ks, fmt.Sprintf("%s=%T:%v", k, v, v))
	}
	sort.Strings(ks)
	return name + "{" + strings.Join(ks, ",") + "}"
}

func newTestRegistry() *testRegistry {
	r := &testRegistry{ids: map[string]uint32{}}
	add := func(name string, props map[string]any) {
		if props == nil {
			props = map[string]any{}
		}
		r.ids[stateKey(name, props)] = uint32(len(r.names))
		r.names = append(r.names, name)
		r.props = append(r.props, props)
	}
	add("minecraft:air", nil)
	add("minecraft:stone", nil)
	add("minecraft:dirt", map[string]any{"dirt_type": "normal"})
	add("minecraft:water", map[string]any{"liquid_depth": int32(0)})
	for i := range 40 {
		add("test:block", map[string]any{"n": int32(i)})
	}
	return r
}

func (r *testRegistry) BlockCount() int      { return len(r.names) }
func (r *testRegistry) AirRuntimeID() uint32 { return 0 }
func (r *testRegistry) RuntimeIDToState(id uint32) (string, map[string]any, bool) {
	if int(id) >= len(r.names) {
		return "", nil, false
	}
	return r.names[id], r.props[id], true
}

func (r *testRegistry) StateToRuntimeID(name string, props map[string]any) (uint32, bool) {
	id, ok := r.ids[stateKey(name, props)]
	return id, ok
}
func (r *testRegistry) FilteringBlock(id uint32) uint8 {
	if id == 0 {
		return 0
	}
	return 15
}
func (r *testRegistry) LightBlock(uint32) uint8                  { return 0 }
func (r *testRegistry) HashToRuntimeID(h uint32) (uint32, bool)  { return h, int(h) < len(r.names) }
func (r *testRegistry) RuntimeIDToHash(id uint32) (uint32, bool) { return id, true }

var testRange = cube.Range{-64, 319}

// filledChunk sets blocks on both layers, across sub chunks, with more
// distinct values than small palettes hold, plus some biomes.
func filledChunk(br chunk.BlockRegistry) *chunk.Chunk {
	c := chunk.New(br, testRange)
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y < 80; y += 3 {
				c.SetBlock(x, y, z, 0, uint32(int(x)+int(z)+int(y+64))%uint32(br.BlockCount()))
			}
			c.SetBlock(x, 10, z, 1, 3)
			c.SetBiome(x, 0, z, uint32(x))
		}
	}
	return c
}

func sameChunk(t *testing.T, a, b *chunk.Chunk) {
	t.Helper()
	// Not Chunk.Equals: PalettedStorage.Equal is false for any palette
	// starting with air.
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(testRange[0]); y <= int16(testRange[1]); y++ {
				for layer := range uint8(2) {
					if a.Block(x, y, z, layer) != b.Block(x, y, z, layer) {
						t.Fatalf("block %d,%d,%d layer %d: %d != %d", x, y, z, layer, a.Block(x, y, z, layer), b.Block(x, y, z, layer))
					}
				}
			}
			for y := int16(testRange[0]); y <= int16(testRange[1]); y += 5 {
				if a.Biome(x, y, z) != b.Biome(x, y, z) {
					t.Fatalf("biome %d,%d,%d: %d != %d", x, y, z, a.Biome(x, y, z), b.Biome(x, y, z))
				}
			}
		}
	}
}

// bigChest is a block entity with an Items field big enough to be cut into a
// payload.
func bigChest(pos cube.Pos, n int) map[string]any {
	items := make([]any, 0, 27)
	for i := range 27 {
		items = append(items, map[string]any{"Name": "minecraft:written_book", "Slot": byte(i), "Count": byte(1),
			"tag": map[string]any{"pages": strings.Repeat(fmt.Sprintf("page %d ", n), 200)}})
	}
	return map[string]any{"id": "Chest", "x": int32(pos[0]), "y": int32(pos[1]), "z": int32(pos[2]), "Items": items}
}

func openTest(t *testing.T, dir string, reg chunk.BlockRegistry) *DB {
	t.Helper()
	db, err := Config{Blocks: reg, FlushInterval: -1, Store: store.Options{NoSync: true}}.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestColumnRoundTrip(t *testing.T) {
	reg := newTestRegistry()
	dir := t.TempDir()
	db := openTest(t, dir, reg)
	pos, dim := mcdb.ChunkPos{3, -7}, mcdb.Overworld
	chest := cube.Pos{50, 10, -110}
	col := &chunk.Column{
		Chunk:           filledChunk(reg),
		Finalisation:    chunk.FinalisationNeedsPopulation,
		Entities:        []chunk.Entity{{ID: -5, Data: map[string]any{"identifier": "minecraft:pig"}}, {ID: 1 << 40, Data: map[string]any{"identifier": "minecraft:cow"}}},
		BlockEntities:   []chunk.BlockEntity{{Pos: chest, Data: bigChest(chest, 1)}, {Pos: cube.Pos{49, 11, -110}, Data: map[string]any{"id": "Sign", "x": int32(49), "y": int32(11), "z": int32(-110)}}},
		Tick:            77,
		ScheduledBlocks: []chunk.ScheduledBlockUpdate{{Pos: cube.Pos{48, 5, -112}, Block: 2, Tick: 80}},
		Extra:           map[byte][]byte{'w': {1, 2, 3}},
	}
	if err := db.StoreColumn(pos, dim, col); err != nil {
		t.Fatal(err)
	}
	// Read before the round is committed: the store commits it first.
	got, err := db.LoadColumn(pos, dim)
	if err != nil {
		t.Fatal(err)
	}
	checkColumn(t, col, got, 2)
	if err := db.StoreLevel("portals", []byte{9}); err != nil {
		t.Fatal(err)
	}
	if err := db.StorePlayer(mcdb.PlayerIDs{SelfSignedID: "abc"}, map[string]any{"x": int32(1)}); err != nil {
		t.Fatal(err)
	}
	if v, err := db.LoadLevel("portals"); err != nil || !bytes.Equal(v, []byte{9}) {
		t.Fatalf("level key before commit: %v %v", v, err)
	}
	db.LevelDat().LevelName = "round trip"
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = openTest(t, dir, reg)
	defer db.Close()
	if db.LevelDat().LevelName != "round trip" {
		t.Fatalf("level name %q", db.LevelDat().LevelName)
	}
	got, err = db.LoadColumn(pos, dim)
	if err != nil {
		t.Fatal(err)
	}
	checkColumn(t, col, got, 2)
	if p, ok, err := db.LoadPlayer(mcdb.PlayerIDs{SelfSignedID: "abc"}); err != nil || !ok || p["x"] != int32(1) {
		t.Fatalf("player: %v %v %v", p, ok, err)
	}
	if _, err := db.LoadColumn(mcdb.ChunkPos{100, 100}, dim); !errors.Is(err, mcdb.ErrNotFound) {
		t.Fatalf("missing column: %v", err)
	}
	m, err := db.LoadBlockEntity(pos, dim, chest)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(m) != fmt.Sprint(bigChest(chest, 1)) {
		t.Fatal("chest changed")
	}

	// A save that keeps the block entities leaves the chest's payload as it
	// is; a new chest beside it gets payload numbers of its own.
	before := payloadRecords(t, db)
	if before == 0 {
		t.Fatal("the chest was not cut into payloads")
	}
	got.KeepBlockEntities = true
	got.Entities = got.Entities[:1]
	if err := db.StoreColumn(pos, dim, got); err != nil {
		t.Fatal(err)
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	if after := payloadRecords(t, db); after != before {
		t.Fatalf("payload records %d, then %d", before, after)
	}
	chest2 := cube.Pos{51, 10, -110}
	got.KeepBlockEntities = false
	got.KeepBlockEntitiesAt = []cube.Pos{chest}
	got.BlockEntities = []chunk.BlockEntity{{Pos: chest2, Data: bigChest(chest2, 2)}}
	if err := db.StoreColumn(pos, dim, got); err != nil {
		t.Fatal(err)
	}
	var seen []string
	if err := db.EachBlockEntity(pos, dim, func(at cube.Pos, raw []byte) error {
		seen = append(seen, fmt.Sprint(at))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != fmt.Sprint([]string{fmt.Sprint(chest), fmt.Sprint(chest2)}) {
		t.Fatalf("block entities %v", seen)
	}
	for i, at := range []cube.Pos{chest, chest2} {
		m, err := db.LoadBlockEntity(pos, dim, at)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(m) != fmt.Sprint(bigChest(at, i+1)) {
			t.Fatalf("chest %d changed", i+1)
		}
	}
	if _, err := db.LoadBlockEntity(pos, dim, cube.Pos{49, 11, -110}); !errors.Is(err, mcdb.ErrNotFound) {
		t.Fatalf("dropped sign: %v", err)
	}
}

func checkColumn(t *testing.T, want, got *chunk.Column, blockEntities int) {
	t.Helper()
	sameChunk(t, want.Chunk, got.Chunk)
	if got.Finalisation != want.Finalisation || got.Tick != want.Tick || len(got.ScheduledBlocks) != 1 ||
		got.ScheduledBlocks[0] != want.ScheduledBlocks[0] || !bytes.Equal(got.Extra['w'], want.Extra['w']) {
		t.Fatalf("column fields: %+v", got)
	}
	if len(got.Entities) != len(want.Entities) {
		t.Fatalf("%d entities", len(got.Entities))
	}
	for i, e := range got.Entities {
		if e.ID != want.Entities[i].ID || e.Data["identifier"] != want.Entities[i].Data["identifier"] {
			t.Fatalf("entity %d: %+v", i, e)
		}
	}
	if len(got.BlockEntities) != blockEntities {
		t.Fatalf("%d block entities", len(got.BlockEntities))
	}
	if got.MetaDataHash == 0 || len(got.HeightMap) != 256 {
		t.Fatal("no metadata hash or height map")
	}
}

// payloadRecords counts the stored records of kind payload.
func payloadRecords(t *testing.T, db *DB) int {
	t.Helper()
	sn, err := db.World().Store().Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Release()
	n := 0
	f, err := db.World().Store().File("overworld/r.0.-1.hx")
	if err != nil {
		t.Fatal(err)
	}
	if err := sn.Range(f, func(key []byte, _ store.Info) bool {
		if len(key) == 7 {
			n++
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestStagedLoad checks the calls for chunks only shown: terrain with no
// objects, and block entities without the fields skipped.
func TestStagedLoad(t *testing.T) {
	reg := newTestRegistry()
	db := openTest(t, t.TempDir(), reg)
	defer db.Close()
	pos, dim := mcdb.ChunkPos{-1, 4}, mcdb.Overworld
	chest, sign := cube.Pos{-10, 20, 70}, cube.Pos{-11, 20, 70}
	col := &chunk.Column{
		Chunk:    filledChunk(reg),
		Entities: []chunk.Entity{{ID: 7, Data: map[string]any{"identifier": "minecraft:pig"}}},
		BlockEntities: []chunk.BlockEntity{{Pos: chest, Data: bigChest(chest, 3)},
			{Pos: sign, Data: map[string]any{"id": "Sign", "x": int32(-11), "y": int32(20), "z": int32(70), "Text": "hello"}}},
	}
	if err := db.StoreColumn(pos, dim, col); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadTerrain(pos, dim)
	if err != nil {
		t.Fatal(err)
	}
	sameChunk(t, col.Chunk, got.Chunk)
	if len(got.Entities) != 0 || len(got.BlockEntities) != 0 {
		t.Fatal("terrain load read objects")
	}
	seen := map[cube.Pos]map[string]any{}
	if err := db.EachBlockEntityView(pos, dim, func(name string) bool { return name == "Items" }, func(at cube.Pos, raw []byte) error {
		m, err := nbt.DecodeWithout(raw, nbt.LittleEndian, nil)
		seen[at] = m
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := seen[chest]["Items"]; ok || seen[chest]["id"] != "Chest" || seen[chest]["x"] != int32(-10) {
		t.Fatalf("chest seen as %v", seen[chest])
	}
	if seen[sign]["Text"] != "hello" {
		t.Fatalf("sign seen as %v", seen[sign])
	}
}
