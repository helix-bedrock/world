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
	if _, err := db.ldb.Get(k.Sum(keyVersion), nil); !errors.Is(err, leveldb.ErrNotFound) {
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
		got, err := db.ldb.Get(k.Sum(tag), nil)
		if err != nil || !bytes.Equal(got, v) {
			t.Errorf("tag %q: %x, %v; want %x", tag, got, err, v)
		}
	}
	for _, tag := range []byte{keyBlockEntities, keyLegacyStates} {
		if _, err := db.ldb.Get(k.Sum(tag), nil); !errors.Is(err, leveldb.ErrNotFound) {
			t.Errorf("tag %q written", tag)
		}
	}
	// BDS keeps an empty digp record for a chunk without entities.
	if v, err := db.ldb.Get(append([]byte(keyEntityIdentifiers), index(k.pos, k.dim)...), nil); err != nil || len(v) != 0 {
		t.Errorf("digp: %x, %v", v, err)
	}
	if _, err := db.ldb.Get([]byte(keyChunkMetaData), nil); err != nil {
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
