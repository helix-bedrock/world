package leveldb

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/helix-bedrock/world/leveldb/opt"
	"github.com/helix-bedrock/world/leveldb/util"
)

// TestFlateRoundTrip writes with Minecraft's compression (zlib, block type 4),
// reopens the database and reads everything back.
func TestFlateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	o := &opt.Options{Compression: opt.FlateCompression, BlockSize: 16 * opt.KiB}
	db, err := OpenFile(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	value := bytes.Repeat([]byte("helix chunk data "), 200)
	for i := range 2000 {
		if err := db.Put(fmt.Appendf(nil, "key-%05d", i), value, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CompactRange(util.Range{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = OpenFile(dir, o)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, i := range []int{0, 999, 1999} {
		got, err := db.Get(fmt.Appendf(nil, "key-%05d", i), nil)
		if err != nil || !bytes.Equal(got, value) {
			t.Fatalf("key %d: err %v, %d bytes", i, err, len(got))
		}
	}
}
