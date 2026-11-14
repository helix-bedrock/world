package store

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

func TestSnapshot(t *testing.T) {
	s, err := Open(t.TempDir(), Options{NoSync: true, IndexCache: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, _ := s.File("a.hx")
	g, _ := s.File("g.hx")
	val := func(name string, i int) []byte {
		return bytes.Repeat([]byte(fmt.Sprintf("%s%d.", name, i)), 300)
	}
	var b Batch
	b.Put(f, 1, []byte("a"), val("a", 0))
	b.Put(f, 1, []byte("b"), val("b", 0))
	b.Put(f, 1, []byte("d"), val("d", 0))
	if _, err := s.Commit(&b); err != nil {
		t.Fatal(err)
	}
	sn, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	// Change everything many times, so freed space would be reused if the
	// snapshot did not keep it, with checkpoints between.
	for i := 1; i <= 50; i++ {
		b.Reset()
		b.Delete(f, []byte("a"))
		b.Put(f, 1, []byte("b"), val("b", i))
		b.Put(f, 1, []byte("c"), val("c", i))
		b.Put(g, 1, []byte("x"), val("x", i))
		if _, err := s.Commit(&b); err != nil {
			t.Fatal(err)
		}
		if i%10 == 0 {
			if err := s.Checkpoint(); err != nil {
				t.Fatal(err)
			}
		}
	}

	check := func(id FileID, key string, want []byte) {
		t.Helper()
		got, err := sn.Get(id, []byte(key), nil)
		if want == nil {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("snapshot %s: %v, want not found", key, err)
			}
			return
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("snapshot %s: %.20q, %v", key, got, err)
		}
	}
	check(f, "a", val("a", 0))
	check(f, "b", val("b", 0))
	check(f, "c", nil)
	check(f, "d", val("d", 0))
	check(g, "x", nil)
	var keys []string
	if err := sn.Range(f, func(k []byte, _ Info) bool { keys = append(keys, string(k)); return true }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(keys) != "[a b d]" {
		t.Fatalf("snapshot keys %v", keys)
	}
	if cur, err := s.Get(f, []byte("b"), nil); err != nil || !bytes.Equal(cur, val("b", 50)) {
		t.Fatalf("current b: %v", err)
	}

	sn.Release()
	if _, err := sn.Get(f, []byte("a"), nil); !errors.Is(err, ErrReleased) {
		t.Fatalf("read after release: %v", err)
	}
	// Space is reused again after the release.
	for i := range 5 {
		b.Reset()
		b.Put(f, 1, []byte("b"), val("b", 100+i))
		if _, err := s.Commit(&b); err != nil {
			t.Fatal(err)
		}
	}
	if cur, err := s.Get(f, []byte("b"), nil); err != nil || !bytes.Equal(cur, val("b", 104)) {
		t.Fatalf("b after release: %v", err)
	}
}
