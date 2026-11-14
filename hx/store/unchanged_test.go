package store

import (
	"bytes"
	"testing"
)

// TestUnchangedSkipped checks that writing what a key already holds writes
// nothing, and that a change, another kind or a reopened store writes again.
func TestUnchangedSkipped(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := s.File("a.hx")
	body := bytes.Repeat([]byte("chest "), 5000)
	put := func(kind byte, v []byte) uint64 {
		t.Helper()
		var b Batch
		b.Put(f, kind, []byte("k"), v)
		b.Put(f, 1, []byte("other"), []byte{byte(len(v))})
		if _, err := s.Commit(&b); err != nil {
			t.Fatal(err)
		}
		info, err := s.Stat(f, []byte("k"))
		if err != nil {
			t.Fatal(err)
		}
		return info.Round
	}
	first := put(1, body)
	if r := put(1, body); r != first {
		t.Fatalf("same bytes written again: round %d, was %d", r, first)
	}
	changed := append(bytes.Clone(body), '!')
	second := put(1, changed)
	if second == first {
		t.Fatal("changed bytes not written")
	}
	if r := put(2, changed); r == second {
		t.Fatal("same bytes of another kind not written")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, _ = s.File("a.hx")
	before, _ := s.Stat(f, []byte("k"))
	if r := put(2, changed); r == before.Round {
		t.Fatal("after a reopen the first write must go to disk: the index keeps no sums")
	}
	got, err := s.Get(f, []byte("k"), nil)
	if err != nil || !bytes.Equal(got, changed) {
		t.Fatalf("read back: %v", err)
	}
}
