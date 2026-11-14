package convert

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"
	"testing"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/hx"
	"github.com/helix-bedrock/world/hx/store"
	"github.com/helix-bedrock/world/lsm"
)

func mustNBT(t *testing.T, v any) []byte {
	t.Helper()
	b, err := nbt.MarshalEncoding(v, nbt.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func blockState(t *testing.T, name string) []byte {
	return mustNBT(t, map[string]any{"name": name, "states": map[string]any{}, "version": int32(18168865)})
}

// bdsSub is a version 9 sub chunk with one 1-bit layer.
func bdsSub(t *testing.T, index int8) []byte {
	v := []byte{9, 1, byte(index), 1 << 1}
	words := make([]byte, 128*4)
	words[5] = 0x10
	v = append(v, words...)
	v = binary.LittleEndian.AppendUint32(v, 2)
	v = append(v, blockState(t, "minecraft:air")...)
	return append(v, blockState(t, "minecraft:stone")...)
}

func chunkKey(id chunkID, tag byte, extra ...byte) []byte {
	return append(append(appendChunkPrefix(nil, id), tag), extra...)
}

func bigChest(t *testing.T, fill byte) []byte {
	items := make([]any, 60)
	for i := range items {
		items[i] = map[string]any{"Name": "minecraft:written_book", "Pages": string(bytes.Repeat([]byte{fill}, 9000))}
	}
	return mustNBT(t, map[string]any{"id": "Chest", "x": int32(16), "y": int32(64), "z": int32(-32), "Items": items})
}

// testBDSWorld writes a small BDS world touching every kind of key.
func testBDSWorld(t *testing.T, dir string) {
	db, err := lsm.Open(filepath.Join(dir, "db"), lsm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ow := chunkID{pos: hx.ChunkPos{1, -2}}
	ne := chunkID{dim: hx.Nether, pos: hx.ChunkPos{1, -2}}
	far := chunkID{dim: hx.End, pos: hx.ChunkPos{-40, 7}}
	var b lsm.Batch
	put := func(k, v []byte) { b.Put(k, v) }
	for _, id := range []chunkID{ow, ne, far} {
		put(chunkKey(id, ','), []byte{42})
		put(chunkKey(id, '+'), bytes.Repeat([]byte{3}, 520))
		put(chunkKey(id, '/', 0), bdsSub(t, 0))
		put(chunkKey(id, '6'), []byte{2, 0, 0, 0})
	}
	put(chunkKey(ow, '/', 0xfc), bdsSub(t, -4))
	put(chunkKey(ow, '/', 1), []byte{1, 0, 7, 7})      // version 1: kept raw
	put(chunkKey(ow, '/', 2), append(bdsSub(t, 3), 0)) // index mismatch: raw
	put(chunkKey(ow, '3'), mustNBT(t, map[string]any{"tick": int64(5)}))
	put(chunkKey(ow, ':'), mustNBT(t, map[string]any{"tick": int64(6)}))
	put(chunkKey(ow, 'w'), []byte("structure starts"))
	var bes []byte
	bes = append(bes, mustNBT(t, map[string]any{"id": "Sign", "Text": "hi"})...)
	for _, f := range []byte("abc") {
		bes = append(bes, bigChest(t, f)...)
	}
	if len(bes) < streamAt {
		t.Fatalf("block entities of %d bytes do not test streaming", len(bes))
	}
	put(chunkKey(ow, '1'), bes)
	put(chunkKey(ne, '1'), mustNBT(t, map[string]any{"id": "Furnace"}))

	cow := []byte{0, 0, 0, 1, 0, 0, 0, 7}
	pig := []byte{0, 0, 0, 1, 0, 0, 0, 8}
	ghost := []byte{0, 0, 0, 1, 0, 0, 0, 9}  // in digp, no record
	orphan := []byte{0, 0, 0, 2, 0, 0, 0, 1} // record, no digp
	put(digpKey(ow), append(append(append([]byte{}, cow...), pig...), ghost...))
	put(digpKey(ne), nil)
	put(digpKey(chunkID{pos: hx.ChunkPos{100, 100}}), cow) // chunk with only digp
	put(append([]byte(actorPrefix), cow...), mustNBT(t, map[string]any{"identifier": "minecraft:cow"}))
	put(append([]byte(actorPrefix), pig...), mustNBT(t, map[string]any{"identifier": "minecraft:pig", "big": string(bytes.Repeat([]byte{'p'}, 9000))}))
	put(append([]byte(actorPrefix), orphan...), mustNBT(t, map[string]any{"identifier": "minecraft:bat"}))

	put([]byte("player_server_1234"), mustNBT(t, map[string]any{"a": int32(1)}))
	put([]byte("player_abc"), mustNBT(t, map[string]any{"ServerId": "player_server_1234"}))
	put([]byte("~local_player"), mustNBT(t, map[string]any{"b": int32(2)}))
	put([]byte("scoreboard"), []byte("board"))
	put([]byte("VILLAGE_Overworld_x_INFO"), []byte("v"))
	put(chunkKey(ow, 0x99), []byte("unknown tag")) // not a chunk tag: a level key
	if err := db.Write(&b); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "level.dat"), []byte("level.dat bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "levelname.txt"), []byte("Test"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// heapWatch samples the heap in use, keeping the highest value.
type heapWatch struct {
	max  atomic.Uint64
	done chan struct{}
}

func watchHeap() *heapWatch {
	h := &heapWatch{done: make(chan struct{})}
	go func() {
		var ms runtime.MemStats
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-h.done:
				return
			case <-tick.C:
				runtime.ReadMemStats(&ms)
				if ms.HeapInuse > h.max.Load() {
					h.max.Store(ms.HeapInuse)
				}
			}
		}
	}()
	return h
}

// take returns the peak since the last take.
func (h *heapWatch) take() uint64 { return h.max.Swap(0) }

func (h *heapWatch) stop() { close(h.done) }

// listDir returns the names, sizes and change times of a folder's files.
func listDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, e := range ents {
		fi, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		m[e.Name()] = fmt.Sprint(fi.Size(), fi.ModTime().UnixNano())
	}
	return m
}

// sameDB checks two databases hold the same keys and values.
func sameDB(t *testing.T, a, b string) int {
	t.Helper()
	da, err := lsm.Open(a, lsm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer da.Close()
	db, err := lsm.Open(b, lsm.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ia, _ := da.NewIterator(nil)
	defer ia.Close()
	ib, _ := db.NewIterator(nil)
	defer ib.Close()
	n := 0
	oka, okb := ia.First(), ib.First()
	for ; oka && okb; oka, okb = ia.Next(), ib.Next() {
		if !bytes.Equal(ia.Key(), ib.Key()) {
			t.Fatalf("key %d: %x vs %x", n, ia.Key(), ib.Key())
		}
		if !bytes.Equal(ia.Value(), ib.Value()) {
			t.Fatalf("key %x: values differ (%d vs %d bytes)", ia.Key(), len(ia.Value()), len(ib.Value()))
		}
		n++
	}
	if oka || okb {
		t.Fatalf("after %d keys: original has more %v, export has more %v", n, oka, okb)
	}
	if ia.Err() != nil || ib.Err() != nil {
		t.Fatal(ia.Err(), ib.Err())
	}
	return n
}

func TestRoundTrip(t *testing.T) {
	src, hxDir, out := t.TempDir(), t.TempDir(), t.TempDir()
	testBDSWorld(t, src)
	opts := Options{Store: store.Options{NoSync: true}, RoundSize: 256 << 10}
	before := listDir(t, filepath.Join(src, "db"))
	if err := Import(src, hxDir, opts); err != nil {
		t.Fatal(err)
	}
	if after := listDir(t, filepath.Join(src, "db")); !maps.Equal(before, after) {
		t.Fatalf("Import changed the BDS database: %v -> %v", before, after)
	}
	if err := Export(hxDir, out, opts); err != nil {
		t.Fatal(err)
	}
	n := sameDB(t, filepath.Join(src, "db"), filepath.Join(out, "db"))
	t.Logf("%d keys identical", n)
	for _, f := range []string{"level.dat", "levelname.txt"} {
		a, _ := os.ReadFile(filepath.Join(src, f))
		b, err := os.ReadFile(filepath.Join(out, f))
		if err != nil || !bytes.Equal(a, b) {
			t.Fatalf("%s differs: %v", f, err)
		}
	}

	// What went where: translated sub chunks, raw ones, shells.
	w, err := hx.Open(hxDir, hx.Config{Store: opts.Store})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	rec, err := w.TerrainRecord(hx.Overworld, hx.ChunkPos{1, -2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var tr hx.Terrain
	if err := hx.DecodeTerrain(rec, &tr); err != nil {
		t.Fatal(err)
	}
	raw := 0
	for _, s := range tr.Subs {
		if s.Raw != nil {
			raw++
		}
	}
	if len(tr.Subs) != 4 || raw != 2 {
		t.Fatalf("%d sub chunks, %d raw; want 4, 2", len(tr.Subs), raw)
	}
	orec, err := w.ObjectsRecord(hx.Overworld, hx.ChunkPos{1, -2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var o hx.Objects
	if err := hx.DecodeObjects(orec, &o); err != nil {
		t.Fatal(err)
	}
	if len(o.BlockEntities) != 4 || len(o.Entities) != 3 || o.Payloads < 4 || len(orec) > 64<<10 {
		t.Fatalf("objects: %d block entities, %d entities, %d payloads, %d bytes", len(o.BlockEntities), len(o.Entities), o.Payloads, len(orec))
	}
	if w.Blocks().Len() != 2 {
		t.Fatalf("block table has %d entries, want 2", w.Blocks().Len())
	}
}

// TestBDSWorld converts a world BDS saved there and back and compares every
// key. Set HX_BDS_WORLD to the world's folder (holding db/); it is copied
// first.
func TestBDSWorld(t *testing.T) {
	src := os.Getenv("HX_BDS_WORLD")
	if src == "" {
		t.Skip("HX_BDS_WORLD not set")
	}
	cp, hxDir, out := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.CopyFS(cp, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	opts := Options{Store: store.Options{NoSync: true}}
	peak := watchHeap()
	start := time.Now()
	if err := Import(cp, hxDir, opts); err != nil {
		t.Fatal(err)
	}
	t.Logf("import %v, peak heap %d MB", time.Since(start).Round(time.Millisecond), peak.take()>>20)
	start = time.Now()
	if err := Export(hxDir, out, opts); err != nil {
		t.Fatal(err)
	}
	t.Logf("export %v, peak heap %d MB", time.Since(start).Round(time.Millisecond), peak.take()>>20)
	start = time.Now()
	t.Logf("%d keys identical (compare %v, peak heap %d MB)", sameDB(t, filepath.Join(cp, "db"), filepath.Join(out, "db")), time.Since(start).Round(time.Millisecond), peak.take()>>20)
	peak.stop()
	w, err := hx.Open(hxDir, hx.Config{Store: opts.Store})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	chunks, raw, subs := 0, 0, 0
	var (
		rec []byte
		tr  hx.Terrain
	)
	for _, dim := range []hx.Dimension{hx.Overworld, hx.Nether, hx.End} {
		_ = w.EachChunk(dim, func(pos hx.ChunkPos) bool {
			chunks++
			rec, _ = w.TerrainRecord(dim, pos, rec)
			if hx.DecodeTerrain(rec, &tr) == nil {
				for _, s := range tr.Subs {
					subs++
					if s.Raw != nil {
						raw++
					}
				}
			}
			return true
		})
	}
	t.Logf("%d chunks, %d sub chunks, %d kept raw, %d block states", chunks, subs, raw, w.Blocks().Len())
}
