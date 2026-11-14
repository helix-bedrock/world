package hx

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/helix-bedrock/world/hx/store"
)

func openTestWorld(t testing.TB, dir string) *World {
	t.Helper()
	w, err := Open(dir, Config{Blocks: newTestRegistry(), Store: store.Options{NoSync: true}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func testObjects() *Objects {
	return &Objects{
		Keys: []ChunkKey{{Tag: TagBlockEntities, Value: []byte("chest nbt")}, {Tag: TagPendingTicks, Value: []byte{1, 2}}},
		Entities: []Entity{
			{ID: []byte{0, 0, 0, 1, 0, 0, 0, 7}, Data: []byte("cow")},
			{ID: []byte{0, 0, 0, 1, 0, 0, 0, 9}, Data: []byte("pig")},
		},
	}
}

func TestWorld(t *testing.T) {
	dir := t.TempDir()
	w := openTestWorld(t, dir)
	reg := newTestRegistry()
	c := filledChunk(reg)
	positions := []ChunkPos{{0, 0}, {-1, -33}, {31, 32}}

	r := w.NewRound()
	for _, pos := range positions {
		if err := r.PutChunk(Overworld, pos, c, []ChunkKey{{Tag: ',', Value: []byte{42}}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.PutObjects(Overworld, ChunkPos{-1, -33}, testObjects()); err != nil {
		t.Fatal(err)
	}
	r.PutLevel("scoreboard", []byte("board"))
	ids := PlayerIDs{MsaID: "msa", SelfSignedID: "self"}
	if err := r.PutPlayer(ids, map[string]any{"a": int32(1)}); err != nil {
		t.Fatal(err)
	}
	if err := r.PutPlayer(ids, map[string]any{"a": int32(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(r); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	w = openTestWorld(t, dir)
	defer w.Close()
	var (
		buf []byte
		tr  Terrain
	)
	for _, pos := range positions {
		got, b, err := w.LoadChunk(Overworld, pos, buf, &tr)
		if err != nil {
			t.Fatalf("chunk %v: %v", pos, err)
		}
		buf = b
		sameChunk(t, c, got)
	}
	if _, _, err := w.LoadChunk(Overworld, ChunkPos{5, 5}, buf, &tr); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing chunk: %v", err)
	}
	if _, _, err := w.LoadChunk(Nether, ChunkPos{0, 0}, buf, &tr); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing nether chunk: %v", err)
	}

	rec, err := w.ObjectsRecord(Overworld, ChunkPos{-1, -33}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var o Objects
	if err := DecodeObjects(rec, &o); err != nil {
		t.Fatal(err)
	}
	if want := testObjects(); !reflect.DeepEqual(o.Keys, want.Keys) || !reflect.DeepEqual(o.Entities, want.Entities) {
		t.Fatalf("objects %+v", o)
	}

	if v, err := w.Level("scoreboard", nil); err != nil || string(v) != "board" {
		t.Fatalf("level key: %q, %v", v, err)
	}
	data, ok, err := w.LoadPlayer(PlayerIDs{SelfSignedID: "self"})
	if err != nil || !ok || data["a"] != int32(2) {
		t.Fatalf("player: %v, %v, %v", data, ok, err)
	}
	servers := 0
	if err := w.EachPlayerRecord(func(key []byte) bool {
		if bytes.HasPrefix(key, []byte(playerServerPrefix)) {
			servers++
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if servers != 1 {
		t.Fatalf("%d player records for one player", servers)
	}

	var found []ChunkPos
	if err := w.EachChunk(Overworld, func(pos ChunkPos) bool {
		found = append(found, pos)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(found, func(a, b ChunkPos) int { return int(a[0]-b[0])*1000 + int(a[1]-b[1]) })
	want := slices.Clone(positions)
	slices.SortFunc(want, func(a, b ChunkPos) int { return int(a[0]-b[0])*1000 + int(a[1]-b[1]) })
	if !slices.Equal(found, want) {
		t.Fatalf("EachChunk found %v, want %v", found, want)
	}
}

func TestObjectsRoundTrip(t *testing.T) {
	rec, err := AppendObjects(nil, testObjects())
	if err != nil {
		t.Fatal(err)
	}
	var o Objects
	if err := DecodeObjects(rec, &o); err != nil {
		t.Fatal(err)
	}
	again, _ := AppendObjects(nil, &o)
	if !bytes.Equal(rec, again) {
		t.Fatal("re-encoded record differs")
	}
	for n := range len(rec) {
		_ = DecodeObjects(rec[:n], &o)
	}
	if _, err := AppendObjects(nil, &Objects{Entities: []Entity{{ID: []byte{1}}}}); err == nil {
		t.Fatal("short entity ID accepted")
	}
}

func TestParseRegion(t *testing.T) {
	k := regionKey{dim: Nether, x: -3, z: 12}
	x, z, ok := parseRegion(regionPath(k)[len("nether/"):])
	if !ok || x != -3 || z != 12 {
		t.Fatalf("parsed %d, %d, %v", x, z, ok)
	}
}

// BenchmarkLoadTerrain is a warm chunk read: region lookup, record read into
// a reused buffer, terrain decode.
func BenchmarkLoadTerrain(b *testing.B) {
	w := openTestWorld(b, b.TempDir())
	defer w.Close()
	r := w.NewRound()
	if err := r.PutChunk(Overworld, ChunkPos{3, -4}, filledChunk(newTestRegistry()), nil, nil); err != nil {
		b.Fatal(err)
	}
	if _, err := w.Commit(r); err != nil {
		b.Fatal(err)
	}
	var (
		buf []byte
		tr  Terrain
		err error
	)
	b.ReportAllocs()
	for b.Loop() {
		if buf, err = w.TerrainRecord(Overworld, ChunkPos{3, -4}, buf); err != nil {
			b.Fatal(err)
		}
		if err := DecodeTerrain(buf, &tr); err != nil {
			b.Fatal(err)
		}
	}
}
