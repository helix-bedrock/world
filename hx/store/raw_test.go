package store

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

// TestRawCopy copies records as stored, small and streamed, from a snapshot
// of one store into another, and opens the copy read-only.
func TestRawCopy(t *testing.T) {
	src, err := Open(t.TempDir(), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	sf, _ := src.File("a.hx")
	big := make([]byte, 3<<20)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range big {
		big[i] = byte(rng.Uint32())
	}
	small := bytes.Repeat([]byte("small "), 100)
	var b Batch
	b.Put(sf, 1, []byte("small"), small)
	b.PutStream(sf, 2, []byte("big"), int64(len(big)), func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(big)), nil })
	if _, err := src.Commit(&b); err != nil {
		t.Fatal(err)
	}
	sn, err := src.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer sn.Release()

	dir := t.TempDir()
	dst, err := Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	df, _ := dst.File("b.hx")
	h, body, err := sn.ReadRaw(sf, []byte("small"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.Kind != 1 || h.Size != int64(len(small)) || h.Stored != int64(len(body)) {
		t.Fatalf("raw header %+v, body %d bytes", h, len(body))
	}
	b.Reset()
	b.PutRaw(df, []byte("small"), h, body)
	bh, rc, err := sn.OpenRaw(sf, []byte("big"))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	b.PutRawStream(df, []byte("big"), bh, func() (io.ReadCloser, error) {
		_, rc, err := sn.OpenRaw(sf, []byte("big"))
		return rc, err
	})
	if _, err := dst.Commit(&b); err != nil {
		t.Fatal(err)
	}

	// A body that does not match its checksum is refused.
	b.Reset()
	bad := bytes.Clone(body)
	bad[0] ^= 1
	b.PutRaw(df, []byte("bad"), h, bad)
	if _, err := dst.Commit(&b); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("commit of a damaged raw record: %v", err)
	}
	b.Reset()
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := Open(dir, Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rf, _ := ro.File("b.hx")
	if got, err := ro.Get(rf, []byte("small"), nil); err != nil || !bytes.Equal(got, small) {
		t.Fatalf("small copy: %v", err)
	}
	rs, n, err := ro.GetStream(rf, []byte("big"))
	if err != nil || n != int64(len(big)) {
		t.Fatalf("big copy: %v, %d bytes", err, n)
	}
	got, err := io.ReadAll(rs)
	_ = rs.Close()
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("big copy differs: %v", err)
	}
	b.Put(rf, 1, []byte("x"), nil)
	if _, err := ro.Commit(&b); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("commit to a read-only store: %v", err)
	}
}
