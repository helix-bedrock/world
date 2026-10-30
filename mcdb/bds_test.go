package mcdb

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/leveldb"
)

// Values in these tests come from worlds BDS 1.26.60 saved.

func TestActorKey(t *testing.T) {
	// UniqueID -25769803771 is stored under actorprefix 00000006 00000005.
	const id = int64(-25769803771)
	k := appendActorKey(nil, id)
	if got := hex.EncodeToString(k); got != "0000000600000005" {
		t.Fatalf("key %s", got)
	}
	if got := actorIDFromKey(k); got != id {
		t.Fatalf("round trip %d", got)
	}
}

func TestXXH64(t *testing.T) {
	if got := xxh64(nil); got != 0xef46db3751d8e999 {
		t.Fatalf("empty input: %016x", got)
	}
}

// bdsSeed and the hashes below are from one BDS world: the metadata of a
// chunk generated in each dimension, then the form it is saved with.
const bdsSeed = int64(8860345708872713875)

func TestMetaHashes(t *testing.T) {
	cases := []struct {
		dim          Dimension
		fresh, saved uint64
	}{
		{Overworld, 0xaab0acdfde71ec3b, 0x73c0b1e56238b2e2},
		{Nether, 0xada226ba418f61c6, 0x42b373b0e0f6443b},
		{End, 0x764e16a274cb4465, 0x5d57c9d593bd1eb6},
	}
	d := newMetaDictionary()
	for _, c := range cases {
		s, err := d.savedHash(0, c.dim, bdsSeed, 1)
		if err != nil {
			t.Fatal(err)
		}
		if s != c.saved {
			t.Errorf("%v: saved hash %016x, want %016x", c.dim, s, c.saved)
		}
		if _, ok := d.entries[c.fresh]; !ok {
			t.Errorf("%v: fresh entry %016x not registered", c.dim, c.fresh)
		}
	}
	b, changed, err := d.encode()
	if err != nil || !changed {
		t.Fatalf("encode: %v %v", changed, err)
	}
	back, err := decodeMetaDictionary(b)
	if err != nil || len(back.entries) != 6 {
		t.Fatalf("decode: %d entries, %v", len(back.entries), err)
	}
	// Entries are sorted by hash.
	if first := binary.LittleEndian.Uint64(b[4:]); first != 0x42b373b0e0f6443b {
		t.Fatalf("first entry %016x", first)
	}
	// A loaded chunk keeps its original fields and gets the saved form.
	if s, _ := back.savedHash(0xada226ba418f61c6, Nether, 0, 0); s != 0x42b373b0e0f6443b {
		t.Fatalf("loaded nether chunk saved as %016x", s)
	}
}

func TestStoredKeys(t *testing.T) {
	db, err := Config{Blocks: newTestRegistry(), FlushInterval: -1}.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.LevelDat().RandomSeed, db.LevelDat().Generator = bdsSeed, 1

	col := testColumn(Overworld, 0)
	col.Entities, col.BlockEntities = nil, nil
	col.Finalisation = chunk.DefaultFinalisation()
	k := dbKey{dim: Overworld}
	if err := db.StoreColumn(k.pos, k.dim, col); err != nil {
		t.Fatal(err)
	}
	// Nothing reaches LevelDB before a flush, but reads see it.
	if _, err := db.kv.Get(k.Sum(keyVersion)); !errors.Is(err, leveldb.ErrNotFound) {
		t.Fatalf("written before flush: %v", err)
	}
	if _, err := db.LoadColumn(k.pos, k.dim); err != nil {
		t.Fatalf("buffered column: %v", err)
	}
	if err := db.Flush(); err != nil {
		t.Fatal(err)
	}

	want := map[byte][]byte{
		keyVersion:            {42},
		keyMetaDataHash:       binary.LittleEndian.AppendUint64(nil, 0x73c0b1e56238b2e2),
		keyBlendingData:       {0, 11},
		keyFinalisation:       {2, 0, 0, 0},
		keyActorDigestVersion: {0},
	}
	for tag, v := range want {
		got, err := db.kv.Get(k.Sum(tag))
		if err != nil || !bytes.Equal(got, v) {
			t.Errorf("tag %q: %x, %v; want %x", tag, got, err, v)
		}
	}
	for _, tag := range []byte{keyBlockEntities, keyLegacyStates} {
		if _, err := db.kv.Get(k.Sum(tag)); !errors.Is(err, leveldb.ErrNotFound) {
			t.Errorf("tag %q written", tag)
		}
	}
	// BDS keeps an empty digp record for a chunk without entities.
	if v, err := db.kv.Get(append([]byte(keyEntityIdentifiers), index(k.pos, k.dim)...)); err != nil || len(v) != 0 {
		t.Errorf("digp: %x, %v", v, err)
	}
	if _, err := db.kv.Get([]byte(keyChunkMetaData)); err != nil {
		t.Errorf("metadata dictionary: %v", err)
	}
}

func FuzzMetaDictionary(f *testing.F) {
	d := newMetaDictionary()
	_, _ = d.savedHash(0, Overworld, 1, 1)
	b, _, _ := d.encode()
	f.Add(b)
	f.Fuzz(func(_ *testing.T, b []byte) {
		_, _ = decodeMetaDictionary(b)
	})
}

func TestHeightMapAndBiomeMarkers(t *testing.T) {
	r, _ := Overworld.Range()
	c := chunk.New(newTestRegistry(), r)
	c.SetBlock(3, 10, 5, 0, 1) // sub chunk index 4 (y 0..15)
	b := appendHeightMap(nil, c, nil, newTestRegistry())
	if got := int16(binary.LittleEndian.Uint16(b[2*(5<<4|3):])); got != 10+1-int16(r.Min()) {
		t.Fatalf("height at 3,5: %d", got)
	}
	if got := binary.LittleEndian.Uint16(b[2*(3<<4|5):]); got != 0 {
		t.Fatalf("empty column: %d", got)
	}
	// A loaded height above the blocks is kept.
	saved := make([]int16, heightMapColumns)
	saved[5<<4|3] = 200
	b = appendHeightMap(nil, c, saved, newTestRegistry())
	if got := binary.LittleEndian.Uint16(b[2*(5<<4|3):]); got != 200 {
		t.Fatalf("saved height lost: %d", got)
	}

	// Biomes: in full up to the highest non-empty sub chunk, markers above.
	enc := chunk.EncodeBiomes(c, chunk.DiskEncoding)
	markers := 0
	for i := len(enc) - 1; i >= 0 && enc[i] == 0xff; i-- {
		markers++
	}
	if want := len(c.Sub()) - 5; markers != want {
		t.Fatalf("%d copy markers, want %d", markers, want)
	}
}

func TestExtraTags(t *testing.T) {
	db := openTest(t, t.TempDir())
	defer func() { _ = db.Close() }()
	col := testColumn(Overworld, 0)
	col.Extra = map[byte][]byte{keyRandomTicks: {1, 2, 3}, keyTag3C: {4, 0, 0, 0}, keyVersion: {9}}
	if err := db.StoreColumn(ChunkPos{}, Overworld, col); err != nil {
		t.Fatal(err)
	}
	got, err := db.LoadColumn(ChunkPos{}, Overworld)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Extra[keyRandomTicks], []byte{1, 2, 3}) || len(got.Extra[keyTag3C]) != 4 {
		t.Fatalf("extra: %v", got.Extra)
	}
	if _, ok := got.Extra[keyVersion]; ok {
		t.Fatal("a decoded tag went through Extra")
	}
	// Random ticks are deleted once gone; the unknown int32 is left alone.
	got.Extra = nil
	if err := db.StoreColumn(ChunkPos{}, Overworld, got); err != nil {
		t.Fatal(err)
	}
	k := dbKey{}
	if ok, _ := db.buf.has(k.Sum(keyRandomTicks)); ok {
		t.Fatal("random ticks not deleted")
	}
	if ok, _ := db.buf.has(k.Sum(keyTag3C)); !ok {
		t.Fatal("unknown tag deleted")
	}
}

func TestPlayers(t *testing.T) {
	dir := t.TempDir()
	db := openTest(t, dir)
	ids := PlayerIDs{MsaID: "f6fd757b-28a5-3909-95c5-68755bae7c36", SelfSignedID: "1f021143-32a7-3bcd-83eb-292470ab454f"}
	if _, ok, err := db.LoadPlayer(ids); ok || err != nil {
		t.Fatalf("unknown player: %v %v", ok, err)
	}
	if err := db.StorePlayer(ids, map[string]any{"SpawnX": int32(5)}); err != nil {
		t.Fatal(err)
	}
	if err := db.StorePlayer(ids, map[string]any{"SpawnX": int32(6)}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openTest(t, dir)
	defer func() { _ = db.Close() }()
	// Either ID finds the player.
	for _, id := range []PlayerIDs{{SelfSignedID: ids.SelfSignedID}, {MsaID: ids.MsaID}} {
		data, ok, err := db.LoadPlayer(id)
		if err != nil || !ok || data["SpawnX"] != int32(6) {
			t.Fatalf("load %+v: %v %v %v", id, data, ok, err)
		}
	}
	// Both lookup records point at one random player_server_ key.
	a, _ := db.LoadLevel("player_" + ids.MsaID)
	b, _ := db.LoadLevel("player_" + ids.SelfSignedID)
	if !bytes.Equal(a, b) || bytes.Contains(a, []byte("player_server_"+ids.MsaID)) {
		t.Fatalf("lookup records: %q %q", a, b)
	}
	n := 0
	if err := db.ForEachLevel("player_server_", func(string, []byte) bool { n++; return true }); err != nil || n != 1 {
		t.Fatalf("%d player records, %v", n, err)
	}
}

func TestLevelKeys(t *testing.T) {
	db := openTest(t, t.TempDir())
	defer func() { _ = db.Close() }()
	if err := db.StoreLevel(LevelKeyScoreboard, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if v, err := db.LoadLevel(LevelKeyScoreboard); err != nil || !bytes.Equal(v, []byte{1}) {
		t.Fatalf("load: %x %v", v, err)
	}
	if err := db.DeleteLevel(LevelKeyScoreboard); err != nil {
		t.Fatal(err)
	}
	if _, err := db.LoadLevel(LevelKeyScoreboard); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted key: %v", err)
	}
	if err := db.StoreLevel("abcdefgh,", nil); err == nil {
		t.Fatal("chunk-shaped key accepted")
	}
}

func TestNamespace(t *testing.T) {
	db := openTest(t, t.TempDir())
	defer func() { _ = db.Close() }()
	a, b := db.Namespace("a"), db.Namespace("b")
	if err := a.Set("k", []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get("k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("namespaces collide: %v", err)
	}
	var keys []string
	if err := a.ForEach("", func(k string, _ []byte) bool { keys = append(keys, k); return true }); err != nil || len(keys) != 1 || keys[0] != "k" {
		t.Fatalf("for each: %v %v", keys, err)
	}
}
