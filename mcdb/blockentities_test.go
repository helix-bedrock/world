package mcdb

import (
	"strings"
	"testing"

	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/cube"
)

// TestKeptBlockEntitiesStream stores a chunk whose block entities are bigger than bigValue,
// then saves it again keeping most of them as stored, so they are copied as a stream, and
// reads them back one by one, before and after the flush and after reopening.
func TestKeptBlockEntitiesStream(t *testing.T) {
	dir := t.TempDir()
	db := openTest(t, dir)
	col := testColumn(Overworld, 0)
	page := strings.Repeat("x", 30000)
	col.BlockEntities = nil
	for i := range 60 {
		col.BlockEntities = append(col.BlockEntities, chunk.BlockEntity{
			Pos:  cube.Pos{i % 16, -64, i / 16},
			Data: map[string]any{"id": "Chest", "n": int32(i), "text": page},
		})
	}
	if err := db.StoreColumn(ChunkPos{}, Overworld, col); err != nil {
		t.Fatal(err)
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	// Keep all but the first; give that one new data.
	keep := make([]cube.Pos, 0, 59)
	for _, be := range col.BlockEntities[1:] {
		keep = append(keep, be.Pos)
	}
	col.BlockEntities = []chunk.BlockEntity{{Pos: cube.Pos{0, -64, 0}, Data: map[string]any{"id": "Chest", "n": int32(-1)}}}
	col.KeepBlockEntitiesAt = keep
	if err := db.StoreColumn(ChunkPos{}, Overworld, col); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		seen := map[cube.Pos]int32{}
		err := db.EachBlockEntity(ChunkPos{}, Overworld, func(pos cube.Pos, raw []byte) error {
			m, err := db.LoadBlockEntity(ChunkPos{}, Overworld, pos)
			if err != nil {
				return err
			}
			n, _ := m["n"].(int32)
			seen[pos] = n
			return nil
		})
		if err != nil || len(seen) != 60 {
			t.Fatalf("%s: %d block entities, %v", when, len(seen), err)
		}
		for i := range 60 {
			want := int32(i)
			if i == 0 {
				want = -1
			}
			if got := seen[cube.Pos{i % 16, -64, i / 16}]; got != want {
				t.Fatalf("%s: block entity %d has n %d", when, i, got)
			}
		}
	}
	check("buffered")
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}
	check("flushed")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, dir)
	defer func() { _ = db.Close() }()
	check("reopened")

	// A kept block entity that is not stored fails the save.
	col.BlockEntities, col.KeepBlockEntitiesAt = nil, []cube.Pos{{9, 9, 9}}
	if err := db.StoreColumn(ChunkPos{}, Overworld, col); err == nil {
		t.Fatal("save keeping a missing block entity went on")
	}
}
