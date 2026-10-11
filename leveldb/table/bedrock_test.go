package table

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/flate"
	"github.com/klauspost/compress/zlib"

	"github.com/helix-bedrock/world/leveldb/util"
)

func newTestReader() *Reader {
	return &Reader{bpool: util.NewBufferPool(4096)}
}

func compressRaw(t testing.TB, data []byte) []byte {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func compressZlib(t testing.TB, data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestInflate checks both of Bedrock's zlib block types, including data that
// compresses far more than the 4x the output buffer starts at.
func TestInflate(t *testing.T) {
	inputs := map[string][]byte{
		"empty":    {},
		"text":     []byte("helix chunk data"),
		"repeated": bytes.Repeat([]byte{0}, 1<<20),
	}
	r := newTestReader()
	for name, want := range inputs {
		for _, tc := range []struct {
			typ  byte
			data []byte
		}{
			{blockTypeZlibRawCompression, compressRaw(t, want)},
			{blockTypeZlibCompression, compressZlib(t, want)},
		} {
			// Twice, so the second run reuses a pooled decompressor.
			for range 2 {
				got, err := r.inflate(tc.data, tc.typ)
				if err != nil {
					t.Fatalf("%s type %d: %v", name, tc.typ, err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("%s type %d: got %d bytes, want %d", name, tc.typ, len(got), len(want))
				}
			}
		}
	}
}

func TestInflateCorrupt(t *testing.T) {
	r := newTestReader()
	bad := []byte{0xff, 0xff, 0xff, 0xff}
	for _, typ := range []byte{blockTypeZlibRawCompression, blockTypeZlibCompression} {
		if _, err := r.inflate(bad, typ); err == nil {
			t.Fatalf("type %d: corrupt input decoded without error", typ)
		}
	}
	// A truncated stream must fail, not return partial data.
	full := compressRaw(t, bytes.Repeat([]byte("abc"), 1000))
	if _, err := r.inflate(full[:len(full)/2], blockTypeZlibRawCompression); err == nil {
		t.Fatal("truncated stream decoded without error")
	}
}

func FuzzInflate(f *testing.F) {
	f.Add(compressRaw(f, []byte("seed")), true)
	f.Add(compressZlib(f, []byte("seed")), false)
	f.Fuzz(func(t *testing.T, data []byte, raw bool) {
		typ := byte(blockTypeZlibCompression)
		if raw {
			typ = blockTypeZlibRawCompression
		}
		_, _ = newTestReader().inflate(data, typ)
	})
}
