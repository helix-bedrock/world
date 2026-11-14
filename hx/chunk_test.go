package hx

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/cube"
	"github.com/helix-bedrock/world/hx/store"
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

func TestChunkRoundTrip(t *testing.T) {
	reg := newTestRegistry()
	table := NewBlockTable(reg, nil)
	c := filledChunk(reg)
	keys := []ChunkKey{{Tag: ',', Value: []byte{42}}, {Tag: '6', Value: []byte{2, 0, 0, 0}}}

	rec, err := AppendChunk(nil, c, table, keys, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tr Terrain
	if err := DecodeTerrain(rec, &tr); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tr.Keys, keys) {
		t.Fatalf("keys %v, want %v", tr.Keys, keys)
	}
	got, err := DecodeChunk(&tr, table, nil, testRange)
	if err != nil {
		t.Fatal(err)
	}
	sameChunk(t, c, got)
	if got.LegacyStates() {
		t.Fatal("current states marked legacy")
	}

	again, err := AppendChunk(nil, got, table, tr.Keys, tr.Data3D)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rec, again) {
		t.Fatal("second save differs from the first")
	}
}

// TestRawSub checks a sub chunk kept in BDS's form decodes into the chunk.
func TestRawSub(t *testing.T) {
	reg := newTestRegistry()
	table := NewBlockTable(reg, nil)
	c := filledChunk(reg)
	rec, err := AppendChunk(nil, c, table, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tr Terrain
	if err := DecodeTerrain(rec, &tr); err != nil {
		t.Fatal(err)
	}
	bottom := testRange[0] >> 4
	for i := range tr.Subs {
		s := &tr.Subs[i]
		s.Raw, s.Layers = chunk.EncodeSubChunk(c, chunk.DiskEncoding, int(s.Index)-bottom), nil
	}
	got, err := DecodeChunk(&tr, table, nil, testRange)
	if err != nil {
		t.Fatal(err)
	}
	sameChunk(t, c, got)
}

func TestTerrainRoundTrip(t *testing.T) {
	in := Terrain{
		Keys:   []ChunkKey{{Tag: ',', Value: []byte{42}}},
		Data3D: bytes.Repeat([]byte{7}, 600),
		Subs: []Sub{
			{Index: -4, Version: 9, Layers: []Layer{
				{Bits: 0, Words: []byte{}, Palette: []uint32{5}},
				{Bits: 1, Words: make([]byte, 128*4), Palette: []uint32{0, 300}},
			}},
			{Index: 2, Raw: []byte{8, 1, 0}},
			{Index: 3, Version: 8, Layers: []Layer{{Bits: 4, Words: bytes.Repeat([]byte{0x21}, 512*4), Palette: []uint32{1, 2, 3}}}},
		},
	}
	rec := AppendTerrain(nil, &in)
	var out Terrain
	if err := DecodeTerrain(rec, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in.Keys, out.Keys) || !bytes.Equal(in.Data3D, out.Data3D) || !reflect.DeepEqual(in.Subs, out.Subs) {
		t.Fatalf("terrain differs:\n%+v\n%+v", in.Subs, out.Subs)
	}
	if !bytes.Equal(rec, AppendTerrain(nil, &out)) {
		t.Fatal("re-encoded record differs")
	}
	// Every cut of the record fails or decodes, never panics.
	for n := range len(rec) {
		_ = DecodeTerrain(rec[:n], &out)
	}
}

func TestBlockTable(t *testing.T) {
	reg := newTestRegistry()
	table := NewBlockTable(reg, nil)
	for rid := range uint32(reg.BlockCount()) {
		id, err := table.ID(rid)
		if err != nil {
			t.Fatal(err)
		}
		if again, _ := table.ID(rid); again != id {
			t.Fatalf("ID(%d) = %d then %d", rid, id, again)
		}
		if got, legacy, err := table.RuntimeID(id); err != nil || got != rid || legacy {
			t.Fatalf("RuntimeID(%d) = %d, %v, %v; want %d", id, got, legacy, err, rid)
		}
	}
	// An old and an unknown state, kept as given.
	old, _ := nbt.MarshalEncoding(map[string]any{"name": "minecraft:stone", "states": map[string]any{}, "version": int32(17959425)}, nbt.LittleEndian)
	unknown, _ := nbt.MarshalEncoding(map[string]any{"name": "mod:thing", "states": map[string]any{}, "version": chunk.CurrentBlockVersion}, nbt.LittleEndian)
	oldID, _ := table.Add(old)
	unknownID, _ := table.Add(unknown)
	if rid, legacy, err := table.RuntimeID(oldID); err != nil || rid != 1 || !legacy {
		t.Fatalf("old stone: %d, %v, %v", rid, legacy, err)
	}
	if _, _, err := table.RuntimeID(unknownID); err == nil {
		t.Fatal("unknown state resolved")
	}

	dir := t.TempDir()
	s, err := store.Open(dir, store.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.File(BlocksFile)
	if err != nil {
		t.Fatal(err)
	}
	var b store.Batch
	n := table.PutNew(&b, f)
	if _, err := s.Commit(&b); err != nil {
		t.Fatal(err)
	}
	table.MarkSaved(n)
	b.Reset()
	if table.PutNew(&b, f); b.Len() != 0 {
		t.Fatalf("PutNew after save wrote %d entries", b.Len())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = store.Open(dir, store.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if f, err = s.File(BlocksFile); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBlockTable(s, f, reg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Len() != table.Len() {
		t.Fatalf("loaded %d entries, want %d", loaded.Len(), table.Len())
	}
	for id := range uint32(table.Len()) {
		a, _ := table.Bytes(id)
		b, _ := loaded.Bytes(id)
		if !bytes.Equal(a, b) {
			t.Fatalf("entry %d differs", id)
		}
	}
	if id, _ := loaded.ID(2); id != 2 {
		t.Fatalf("loaded ID(2) = %d", id)
	}
}

func BenchmarkDecodeTerrain(b *testing.B) {
	reg := newTestRegistry()
	rec, err := AppendChunk(nil, filledChunk(reg), NewBlockTable(reg, nil), nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	var tr Terrain
	b.ReportAllocs()
	b.SetBytes(int64(len(rec)))
	for b.Loop() {
		if err := DecodeTerrain(rec, &tr); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeChunk(b *testing.B) {
	reg := newTestRegistry()
	table := NewBlockTable(reg, nil)
	rec, err := AppendChunk(nil, filledChunk(reg), table, nil, nil)
	if err != nil {
		b.Fatal(err)
	}
	var tr Terrain
	b.ReportAllocs()
	for b.Loop() {
		if err := DecodeTerrain(rec, &tr); err != nil {
			b.Fatal(err)
		}
		if _, err := DecodeChunk(&tr, table, nil, testRange); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendChunk(b *testing.B) {
	reg := newTestRegistry()
	table := NewBlockTable(reg, nil)
	c := filledChunk(reg)
	var rec []byte
	b.ReportAllocs()
	for b.Loop() {
		var err error
		if rec, err = AppendChunk(rec[:0], c, table, nil, nil); err != nil {
			b.Fatal(err)
		}
	}
}
