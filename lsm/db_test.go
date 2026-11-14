package lsm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"testing"
)

// smallOpts make flushes and compactions happen with little data.
func smallOpts() Options {
	return Options{WriteBuffer: 16 << 10, BlockSize: 1 << 10, BigValue: 8 << 10, TableSize: 32 << 10, BlockCache: 64 << 10}
}

// TestModel applies random writes to the database and to a map, reopening now and then,
// and checks every read against the map.
func TestModel(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, smallOpts())
	if err != nil {
		t.Fatal(err)
	}
	model := map[string][]byte{}
	rng := rand.New(rand.NewPCG(1, 2))
	for round := range 6 {
		for range 400 {
			writeRandom(t, db, model, rng)
		}
		checkModel(t, db, model)
		if err := db.Close(); err != nil {
			t.Fatalf("round %d: close: %v", round, err)
		}
		if db, err = Open(dir, smallOpts()); err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		checkModel(t, db, model)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeRandom(t *testing.T, db *DB, model map[string][]byte, rng *rand.Rand) {
	t.Helper()
	var b Batch
	for range 1 + rng.IntN(8) {
		key := []byte(fmt.Sprintf("k%04d", rng.IntN(600)))
		switch r := rng.IntN(20); {
		case r < 3:
			b.Delete(key)
			delete(model, string(key))
		case r == 3:
			v := randomValue(rng, 20<<10+rng.IntN(40<<10))
			b.PutStream(key, func() (io.ReadCloser, int64, error) {
				return io.NopCloser(bytes.NewReader(v)), int64(len(v)), nil
			})
			model[string(key)] = v
		default:
			v := randomValue(rng, rng.IntN(600))
			if r == 4 {
				v = randomValue(rng, 10<<10) // over BigValue as a plain Put
			}
			b.Put(key, v)
			model[string(key)] = v
		}
	}
	// Streams are applied before small writes in one batch: keep the model in step by
	// never mixing a key in both within a batch.
	if err := db.Write(&b); err != nil {
		t.Fatal(err)
	}
	fixModelOrder(model, &b)
}

// fixModelOrder makes the model match the write order Write uses for a batch: streams
// first, then small entries.
func fixModelOrder(model map[string][]byte, b *Batch) {
	_ = eachEntry(b.small, func(k kind, key, value []byte) error {
		if k == kindDelete {
			delete(model, string(key))
		} else {
			model[string(key)] = bytes.Clone(value)
		}
		return nil
	})
}

func randomValue(rng *rand.Rand, n int) []byte {
	v := make([]byte, n)
	for i := range v {
		v[i] = byte('a' + rng.IntN(4)) // compressible, like world data
	}
	return v
}

func checkModel(t *testing.T, db *DB, model map[string][]byte) {
	t.Helper()
	for i := range 600 {
		key := fmt.Sprintf("k%04d", i)
		want, ok := model[key]
		got, err := db.Get([]byte(key))
		switch {
		case !ok && !errors.Is(err, ErrNotFound):
			t.Fatalf("Get %s: want not found, got %d bytes, %v", key, len(got), err)
		case ok && err != nil:
			t.Fatalf("Get %s: %v", key, err)
		case ok && !bytes.Equal(got, want):
			t.Fatalf("Get %s: %d bytes, want %d", key, len(got), len(want))
		}
		if ok {
			checkStream(t, db, key, want)
		}
	}
	checkIter(t, db, model)
}

func checkStream(t *testing.T, db *DB, key string, want []byte) {
	t.Helper()
	rc, size, err := db.GetStream([]byte(key))
	if err != nil {
		t.Fatalf("GetStream %s: %v", key, err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || size != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("GetStream %s: %d bytes (size %d), %v; want %d", key, len(got), size, err, len(want))
	}
}

func checkIter(t *testing.T, db *DB, model map[string][]byte) {
	t.Helper()
	keys := make([]string, 0, len(model))
	for k := range model {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	it, err := db.NewIterator(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	i := 0
	for ok := it.First(); ok; ok = it.Next() {
		if i >= len(keys) || string(it.Key()) != keys[i] {
			t.Fatalf("iterator key %d: %q, want %v", i, it.Key(), keys[min(i, len(keys)-1):min(i+1, len(keys))])
		}
		if !bytes.Equal(it.Value(), model[keys[i]]) {
			t.Fatalf("iterator value of %s differs", keys[i])
		}
		i++
	}
	if it.Err() != nil || i != len(keys) {
		t.Fatalf("iterator: %d keys, want %d, %v", i, len(keys), it.Err())
	}
}

// TestSnapshot checks that a snapshot keeps seeing old values across writes, flushes and
// compactions.
func TestSnapshot(t *testing.T) {
	db, err := Open(t.TempDir(), smallOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := range 200 {
		if err := db.Put([]byte(fmt.Sprintf("k%03d", i)), []byte("old")); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := db.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Release()
	for round := range 20 {
		for i := range 200 {
			if err := db.Put([]byte(fmt.Sprintf("k%03d", i)), bytes.Repeat([]byte{byte(round)}, 200)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range 200 {
		v, err := snap.Get([]byte(fmt.Sprintf("k%03d", i)))
		if err != nil || string(v) != "old" {
			t.Fatalf("snapshot k%03d: %q, %v", i, v, err)
		}
	}
}

// TestPrefixIterator checks prefix iteration and Seek.
func TestPrefixIterator(t *testing.T) {
	db, err := Open(t.TempDir(), smallOpts())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, k := range []string{"a1", "b1", "b2", "b3", "c1"} {
		if err := db.Put([]byte(k), []byte(k)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Delete([]byte("b2")); err != nil {
		t.Fatal(err)
	}
	it, err := db.NewIterator([]byte("b"))
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	var got []string
	for ok := it.First(); ok; ok = it.Next() {
		got = append(got, string(it.Key()))
	}
	if !slices.Equal(got, []string{"b1", "b3"}) {
		t.Fatalf("prefix b: %v", got)
	}
}
