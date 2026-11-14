package table

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/helix-bedrock/world/leveldb/opt"
	"github.com/helix-bedrock/world/leveldb/storage"
	"github.com/helix-bedrock/world/leveldb/util"
)

// TestStreamedValue writes a value bigger than bigBlockMin as a stream between small ones and
// reads it back every way: iterating, seeking, backwards, Find and FindStream.
func TestStreamedValue(t *testing.T) {
	for _, c := range []opt.Compression{opt.FlateCompression, opt.NoCompression} {
		t.Run(fmt.Sprint(c), func(t *testing.T) { testStreamedValue(t, c) })
	}
}

func testStreamedValue(t *testing.T, c opt.Compression) {
	o := &opt.Options{Compression: c, BlockSize: 4096}
	big := make([]byte, 3<<20)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range big {
		big[i] = byte(rng.IntN(256))
	}
	var file bytes.Buffer
	w := NewWriter(&file, o, util.NewBufferPool(4096))
	want := map[string][]byte{}
	for i := range 50 {
		k, v := fmt.Sprintf("a%03d", i), bytes.Repeat([]byte{byte(i)}, 300)
		want[k] = v
		if err := w.Append([]byte(k), v); err != nil {
			t.Fatal(err)
		}
	}
	want["b"] = big
	if err := w.AppendStream([]byte("b"), int64(len(big)), bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	for i := range 50 {
		k := fmt.Sprintf("c%03d", i)
		v := []byte(k)
		want[k] = v
		if err := w.Append([]byte(k), v); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data := file.Bytes()
	r, err := NewReader(bytes.NewReader(data), int64(len(data)), storage.FileDesc{}, nil, util.NewBufferPool(4096), o)
	if err != nil {
		t.Fatal(err)
	}

	it := r.NewIterator(nil, nil)
	n := 0
	for it.Next() {
		if v := want[string(it.Key())]; !bytes.Equal(it.Value(), v) {
			t.Fatalf("iterate %q: wrong value", it.Key())
		}
		n++
	}
	if it.Error() != nil || n != len(want) {
		t.Fatalf("iterated %d of %d: %v", n, len(want), it.Error())
	}
	if !it.Seek([]byte("b")) || string(it.Key()) != "b" || !it.Prev() || string(it.Key()) != "a049" {
		t.Fatalf("seek and prev: %q %v", it.Key(), it.Error())
	}
	it.Release()

	it = r.NewIterator(nil, nil)
	if !it.Seek([]byte("a5")) || string(it.Key()) != "b" {
		t.Fatalf("seek a5 gave %q", it.Key())
	}
	vs, ok := it.(interface {
		ValueStream() (io.Reader, int64, bool)
	})
	if !ok {
		t.Fatal("iterator does not stream values")
	}
	rd, size, ok := vs.ValueStream()
	if !ok || size != int64(len(big)) {
		t.Fatalf("value stream: %v %d", ok, size)
	}
	got, err := io.ReadAll(rd)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("streamed value differs: %v", err)
	}
	if !it.Next() || string(it.Key()) != "c000" {
		t.Fatalf("after stream: %q", it.Key())
	}
	it.Release()

	if _, v, err := r.Find([]byte("b"), false, nil); err != nil || !bytes.Equal(v, big) {
		t.Fatalf("find: %v", err)
	}
	k, rc, size, err := r.FindStream([]byte("b"), false, nil)
	if err != nil || string(k) != "b" || size != int64(len(big)) {
		t.Fatalf("find stream: %q %d %v", k, size, err)
	}
	got, err = io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, big) || rc.Close() != nil {
		t.Fatalf("find stream value differs: %v", err)
	}
	k, rc, _, err = r.FindStream([]byte("a010"), false, nil)
	if err != nil || string(k) != "a010" {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(rc)
	if !bytes.Equal(got, want["a010"]) {
		t.Fatal("small find stream differs")
	}

	// A corrupt big block fails instead of giving wrong data.
	bad := bytes.Clone(data)
	bad[len(bad)/2] ^= 0xff
	r, err = NewReader(bytes.NewReader(bad), int64(len(bad)), storage.FileDesc{}, nil, util.NewBufferPool(4096), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Find([]byte("b"), false, nil); err == nil {
		t.Fatal("corrupt big block read without error")
	}
}
