package leveldb

import (
	"bytes"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/helix-bedrock/world/leveldb/opt"
	"github.com/helix-bedrock/world/leveldb/storage"
	"github.com/helix-bedrock/world/leveldb/util"
)

// TestStreamValue writes a big value with PutStream between small writes and reads it back
// streamed and whole, after compaction moved it and after reopening.
func TestStreamValue(t *testing.T) {
	stor := storage.NewMemStorage()
	o := &opt.Options{Compression: opt.FlateCompression}
	db, err := Open(stor, o)
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 5<<20)
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range big {
		big[i] = byte(rng.IntN(256))
	}
	if err := db.Put([]byte("k"), []byte("old"), nil); err != nil {
		t.Fatal(err)
	}
	tr, err := db.OpenTransaction()
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Put([]byte("a"), []byte("small"), nil); err != nil {
		t.Fatal(err)
	}
	if err := tr.PutStream([]byte("k"), int64(len(big)), bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	if err := tr.Put([]byte("z"), []byte("after"), nil); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit(); err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		rc, size, err := db.GetStream([]byte("k"), nil)
		if err != nil || size != int64(len(big)) {
			t.Fatalf("%s: get stream: %d %v", when, size, err)
		}
		got, err := io.ReadAll(rc)
		if err != nil || !bytes.Equal(got, big) || rc.Close() != nil {
			t.Fatalf("%s: streamed value differs: %v", when, err)
		}
		for k, want := range map[string]string{"a": "small", "z": "after"} {
			if v, err := db.Get([]byte(k), nil); err != nil || string(v) != want {
				t.Fatalf("%s: %s = %q %v", when, k, v, err)
			}
		}
		if _, _, err := db.GetStream([]byte("missing"), nil); err != ErrNotFound {
			t.Fatalf("%s: missing key: %v", when, err)
		}
	}
	check("written")
	// An overlapping table makes compaction merge, not move, the big one.
	if err := db.Put([]byte("b"), []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	if err := db.CompactRange(util.Range{}); err != nil {
		t.Fatal(err)
	}
	check("compacted")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if db, err = Open(stor, o); err != nil {
		t.Fatal(err)
	}
	check("reopened")
	if err := db.Delete([]byte("k"), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.GetStream([]byte("k"), nil); err != ErrNotFound {
		t.Fatalf("deleted key: %v", err)
	}
	_ = db.Close()
}
