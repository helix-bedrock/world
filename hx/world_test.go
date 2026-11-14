package hx

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/hx/store"
	"github.com/helix-bedrock/world/mcdb/leveldat"
)

func openTestWorld(t testing.TB, dir string) *World {
	t.Helper()
	w, err := Open(dir, Config{Blocks: newTestRegistry(), Store: store.Options{NoSync: true}})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// bigChest is a block entity whose Items field is far over the shell limit.
func bigChest(t testing.TB, fill byte) []byte {
	t.Helper()
	items := make([]any, 40)
	for i := range items {
		items[i] = map[string]any{"Name": "minecraft:written_book", "Pages": string(bytes.Repeat([]byte{fill}, 500))}
	}
	b, err := nbt.MarshalEncoding(map[string]any{"id": "Chest", "x": int32(1), "y": int32(2), "z": int32(3), "Items": items, "CustomName": string(bytes.Repeat([]byte{'n'}, 600))}, nbt.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func smallCompound(t testing.TB, id string) []byte {
	t.Helper()
	b, err := nbt.MarshalEncoding(map[string]any{"id": id}, nbt.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testObjects(t testing.TB) *ObjectsSource {
	return &ObjectsSource{
		Keys:          []ChunkKey{{Tag: TagPendingTicks, Value: []byte{1, 2}}},
		BlockEntities: append(smallCompound(t, "Sign"), bigChest(t, 'a')...),
		Entities: []EntityData{
			{ID: []byte{0, 0, 0, 1, 0, 0, 0, 7}, Data: smallCompound(t, "minecraft:cow")},
			{ID: []byte{0, 0, 0, 1, 0, 0, 0, 9}},
		},
	}
}

// sameObjects checks a stored objects record expands to src.
func sameObjects(t *testing.T, v view, dim Dimension, pos ChunkPos, src *ObjectsSource) {
	t.Helper()
	rec, err := v.ObjectsRecord(dim, pos, nil)
	if err != nil {
		t.Fatal(err)
	}
	var o Objects
	if err := DecodeObjects(rec, &o); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.Keys, src.Keys) || o.Digp != (src.Digp || len(src.Entities) > 0) {
		t.Fatalf("keys %v digp %v", o.Keys, o.Digp)
	}
	var bes []byte
	for _, s := range o.BlockEntities {
		if bes, err = v.ExpandShell(dim, pos, s, bes); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(bes, src.BlockEntities) {
		t.Fatal("block entities differ")
	}
	if len(o.Entities) != len(src.Entities) {
		t.Fatalf("%d entities", len(o.Entities))
	}
	for i, e := range o.Entities {
		data, err := v.ExpandShell(dim, pos, e.Shell, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(e.ID, src.Entities[i].ID) || !bytes.Equal(data, src.Entities[i].Data) {
			t.Fatalf("entity %d differs", i)
		}
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
	if err := r.PutObjects(Overworld, ChunkPos{-1, -33}, testObjects(t)); err != nil {
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

	sameObjects(t, w.view, Overworld, ChunkPos{-1, -33}, testObjects(t))
	rec, _ := w.ObjectsRecord(Overworld, ChunkPos{-1, -33}, nil)
	var o Objects
	if err := DecodeObjects(rec, &o); err != nil || o.Payloads == 0 || o.BlockEntities[0].Cuts() != 0 || o.BlockEntities[1].Cuts() == 0 {
		t.Fatalf("big chest not cut: %d payloads, %v", o.Payloads, err)
	}
	if len(rec) > 2*shellLimit {
		t.Fatalf("objects record of %d bytes", len(rec))
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

// TestObjectsResave checks a smaller re-save drops old payloads, and that a
// snapshot taken before still reads the old objects.
func TestObjectsResave(t *testing.T) {
	w := openTestWorld(t, t.TempDir())
	defer w.Close()
	pos := ChunkPos{2, 2}
	src := testObjects(t)
	src.BlockEntities = append(append([]byte(nil), src.BlockEntities...), bigChest(t, 'b')...)
	r := w.NewRound()
	if err := r.PutObjects(Overworld, pos, src); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(r); err != nil {
		t.Fatal(err)
	}
	snap, err := w.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Release()

	small := testObjects(t)
	if err := r.PutObjects(Overworld, pos, small); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(r); err != nil {
		t.Fatal(err)
	}
	sameObjects(t, w.view, Overworld, pos, small)
	sameObjects(t, snap.view, Overworld, pos, src)
	rec, _ := w.ObjectsRecord(Overworld, pos, nil)
	var o Objects
	if err := DecodeObjects(rec, &o); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Payload(Overworld, pos, o.Payloads, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("payload past the count still stored: %v", err)
	}
	if _, err := snap.Payload(Overworld, pos, o.Payloads, nil); err != nil {
		t.Fatalf("snapshot lost a payload: %v", err)
	}

	if err := r.DeleteChunk(Overworld, pos); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(r); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Payload(Overworld, pos, 0, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("payload after delete: %v", err)
	}
}

func TestLevelDat(t *testing.T) {
	dir := t.TempDir()
	w := openTestWorld(t, dir)
	if _, err := w.LevelDatBytes(nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("new world level.dat: %v", err)
	}
	ld := &leveldat.LevelDat{}
	var d leveldat.Data
	d.FillDefault()
	d.LevelName = "hx test"
	if err := ld.Marshal(d); err != nil {
		t.Fatal(err)
	}
	r := w.NewRound()
	if err := r.PutLevelDat(ld); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(r); err != nil {
		t.Fatal(err)
	}
	stored, _ := w.LevelDatBytes(nil)
	file, err := os.ReadFile(filepath.Join(dir, LevelDat))
	if err != nil || !bytes.Equal(stored, file) {
		t.Fatalf("level.dat copy differs: %v", err)
	}
	w.Close()
	w = openTestWorld(t, dir)
	defer w.Close()
	got, err := w.LevelDat()
	if err != nil {
		t.Fatal(err)
	}
	var back leveldat.Data
	if err := got.Unmarshal(&back); err != nil || back.LevelName != "hx test" {
		t.Fatalf("level name %q, %v", back.LevelName, err)
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
