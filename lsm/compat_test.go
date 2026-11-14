package lsm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/helix-bedrock/world/leveldb"
	"github.com/helix-bedrock/world/leveldb/opt"
)

// The goleveldb fork reads and writes the same files as Bedrock (raw deflate blocks,
// Bedrock's manifest), so a database it reads back key for key is one BDS reads too.

// TestWrittenReadByLevelDB writes with lsm (through flushes, compactions and big values)
// and reads everything back with goleveldb.
func TestWrittenReadByLevelDB(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir, smallOpts())
	if err != nil {
		t.Fatal(err)
	}
	model := map[string][]byte{}
	rng := rand.New(rand.NewPCG(3, 4))
	for range 1500 {
		writeRandom(t, db, model, rng)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ldb, err := leveldb.OpenFile(dir, &opt.Options{Compression: opt.FlateCompression, ErrorIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ldb.Close()
	for k, want := range model {
		got, err := ldb.Get([]byte(k), nil)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("goleveldb Get %s: %d bytes, %v; want %d", k, len(got), err, len(want))
		}
	}
	it := ldb.NewIterator(nil, nil)
	defer it.Release()
	n := 0
	for it.Next() {
		if _, ok := model[string(it.Key())]; !ok {
			t.Fatalf("goleveldb has key %q lsm deleted", it.Key())
		}
		n++
	}
	if n != len(model) {
		t.Fatalf("goleveldb has %d keys, want %d", n, len(model))
	}
}

// TestReadsLevelDB writes with goleveldb, including values big enough for blocks of
// their own, and reads with lsm.
func TestReadsLevelDB(t *testing.T) {
	dir := t.TempDir()
	ldb, err := leveldb.OpenFile(dir, &opt.Options{Compression: opt.FlateCompression, WriteBuffer: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	model := map[string][]byte{}
	rng := rand.New(rand.NewPCG(5, 6))
	for i := range 3000 {
		key := fmt.Sprintf("key%05d", rng.IntN(2000))
		size := rng.IntN(300)
		if i%200 == 0 {
			size = 3 << 20
		}
		v := randomValue(rng, size)
		if err := ldb.Put([]byte(key), v, nil); err != nil {
			t.Fatal(err)
		}
		model[key] = v
	}
	if err := ldb.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	checkAgainst(t, db, model)
}

func checkAgainst(t *testing.T, db *DB, model map[string][]byte) {
	t.Helper()
	for k, want := range model {
		got, err := db.Get([]byte(k))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Get %s: %d bytes, %v; want %d", k, len(got), err, len(want))
		}
		checkStream(t, db, k, want)
	}
	checkIter(t, db, model)
}

// TestBDSWorld reads a world BDS saved (the folder holding its db, copied first: lsm
// writes a new manifest when it opens a database) with goleveldb and with lsm and
// compares every key. Set LSM_BDS_WORLD to the world's db folder.
func TestBDSWorld(t *testing.T) {
	src := os.Getenv("LSM_BDS_WORLD")
	if src == "" {
		t.Skip("LSM_BDS_WORLD not set")
	}
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
		t.Fatal(err)
	}
	ldb, err := leveldb.OpenFile(dir, &opt.Options{ReadOnly: true, ErrorIfMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	// goleveldb opens read-only without touching the files; lsm opens it after and
	// writes its own manifest. Both walk the keys in step, so no value is held twice.
	db, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lit := ldb.NewIterator(nil, nil)
	defer lit.Release()
	it, err := db.NewIterator(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	n, bytesRead := 0, 0
	ok := it.First()
	for lit.Next() {
		if !ok || !bytes.Equal(it.Key(), lit.Key()) {
			t.Fatalf("key %d: lsm %q, goleveldb %q (%v)", n, it.Key(), lit.Key(), it.Err())
		}
		if !bytes.Equal(it.Value(), lit.Value()) {
			t.Fatalf("value of %q differs", lit.Key())
		}
		checkGet(t, db, lit.Key(), lit.Value())
		n++
		bytesRead += len(lit.Value())
		ok = it.Next()
	}
	if ok || it.Err() != nil || lit.Error() != nil {
		t.Fatalf("after %d keys: lsm has more (%v) or failed: %v %v", n, ok, it.Err(), lit.Error())
	}
	t.Logf("%d keys, %d MB of values, identical", n, bytesRead>>20)
	_ = ldb.Close()
}

func checkGet(t *testing.T, db *DB, key, want []byte) {
	t.Helper()
	got, err := db.Get(key)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("Get %q: %d bytes, %v; want %d", key, len(got), err, len(want))
	}
	rc, size, err := db.GetStream(key)
	if err != nil {
		t.Fatalf("GetStream %q: %v", key, err)
	}
	defer rc.Close()
	got, err = io.ReadAll(rc)
	if err != nil || size != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("GetStream %q: %d bytes, %v; want %d", key, len(got), err, len(want))
	}
}

var _ = errors.Is
