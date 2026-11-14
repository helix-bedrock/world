package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/rand/v2"
	"slices"
	"testing"
)

var testPaths = []string{"overworld/r.0.0.hx", "overworld/r.-1.0.hx", "players.hx"}

// state is what a store holds: path, then key, then body.
type state map[string]map[string][]byte

func (st state) clone() state {
	c := state{}
	for p, m := range st {
		c[p] = maps.Clone(m)
	}
	return c
}

func (st state) put(p, k string, v []byte) {
	if st[p] == nil {
		st[p] = map[string][]byte{}
	}
	st[p][k] = v
}

func (st state) equal(o state) bool {
	for _, p := range testPaths {
		if !maps.EqualFunc(st[p], o[p], bytes.Equal) {
			return false
		}
	}
	return true
}

// testValue returns a body of n bytes, compressible or not.
func testValue(rng *rand.Rand, n int) []byte {
	v := make([]byte, n)
	if rng.IntN(2) == 0 {
		for i := range v {
			v[i] = byte(rng.IntN(256))
		}
		return v
	}
	for i := range v {
		v[i] = "helix world "[i%12]
	}
	if n > 0 {
		v[rng.IntN(n)] = byte(rng.IntN(256))
	}
	return v
}

// randomBatch fills b with writes and applies them to st.
func randomBatch(t *testing.T, s *Store, rng *rand.Rand, b *Batch, st state, streams bool) {
	t.Helper()
	b.Reset()
	for range 1 + rng.IntN(30) {
		p := testPaths[rng.IntN(len(testPaths))]
		id, err := s.File(p)
		if err != nil {
			t.Fatal(err)
		}
		k := fmt.Sprintf("key-%03d", rng.IntN(120))
		switch r := rng.IntN(20); {
		case r < 4:
			b.Delete(id, []byte(k))
			delete(st[p], k)
		case r < 5 && streams:
			v := testValue(rng, 50_000+rng.IntN(400_000))
			b.PutStream(id, 2, []byte(k), int64(len(v)), func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(v)), nil
			})
			st.put(p, k, v)
		default:
			v := testValue(rng, rng.IntN(3000))
			b.Put(id, 1, []byte(k), v)
			st.put(p, k, v)
		}
	}
}

// readState reads everything a store holds, checking Get, GetStream and Range
// agree.
func readState(t *testing.T, s *Store) state {
	t.Helper()
	st := state{}
	for _, p := range testPaths {
		id, err := s.File(p)
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		if err := s.Range(id, func(k []byte, _ Info) bool {
			keys = append(keys, string(k))
			return true
		}); err != nil {
			t.Fatalf("range %s: %v", p, err)
		}
		for _, k := range keys {
			v, err := s.Get(id, []byte(k), nil)
			if err != nil {
				t.Fatalf("get %s %s: %v", p, k, err)
			}
			st.put(p, k, v)
			checkStream(t, s, id, k, v)
		}
	}
	return st
}

func checkStream(t *testing.T, s *Store, id FileID, k string, want []byte) {
	t.Helper()
	rc, n, err := s.GetStream(id, []byte(k))
	if err != nil {
		t.Fatalf("stream %s: %v", k, err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || n != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("stream %s: %d bytes, err %v; want %d bytes", k, len(got), err, len(want))
	}
}

func TestModel(t *testing.T) {
	dir := t.TempDir()
	opts := Options{NoSync: true, JournalLimit: 8 << 10, IndexCache: 16 << 10}
	s, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // test randomness
	want := state{}
	var b Batch
	for round := range 200 {
		randomBatch(t, s, rng, &b, want, true)
		if _, err := s.Commit(&b); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if round%10 == 9 {
			if got := readState(t, s); !got.equal(want) {
				t.Fatalf("round %d: store differs from the model", round)
			}
		}
		if round%40 == 39 {
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if s, err = Open(dir, opts); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := readState(t, s); !got.equal(want) {
		t.Fatal("store differs from the model at the end")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCrash stops the machine at random moments: during rounds, checkpoints
// and journal rotations, with writes not forced to disk lost or torn. After
// reopening, the store must hold exactly the last round that counted.
func TestCrash(t *testing.T) {
	iters := 1000
	if testing.Short() {
		iters = 100
	}
	for iter := range iters {
		crashOnce(t, uint64(iter)) //nolint:gosec // small
	}
}

func crashOnce(t *testing.T, seed uint64) {
	t.Helper()
	mfs := newMemFS(seed)
	rng := rand.New(rand.NewPCG(seed, 99)) //nolint:gosec // test randomness
	// Small journals checkpoint (and rotate) after most rounds, so crashes land
	// in checkpoints as often as in rounds.
	opts := Options{JournalLimit: int64(256 << rng.IntN(6)), IndexCache: 8 << 10, fs: mfs}
	s, err := Open("w", opts)
	if err != nil {
		t.Fatal(err)
	}
	committed := state{}
	var b Batch
	for range rng.IntN(20) {
		randomBatch(t, s, rng, &b, committed, false)
		if _, err := s.Commit(&b); err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
	}
	mfs.mu.Lock()
	mfs.failAfter = mfs.ops + rng.IntN(80)
	mfs.mu.Unlock()
	var pending state
	for range 30 {
		next := committed.clone()
		randomBatch(t, s, rng, &b, next, rng.IntN(4) == 0)
		round, err := s.Commit(&b)
		if err == nil {
			committed = next
			continue
		}
		if !errors.Is(err, errDied) {
			t.Fatalf("seed %d: unexpected error: %v", seed, err)
		}
		if round > 0 {
			committed = next // counted; only the checkpoint after it failed
		} else {
			pending = next
		}
		break
	}
	mfs.crash()
	s2, err := Open("w", Options{JournalLimit: 4 << 10, fs: mfs})
	if err != nil {
		t.Fatalf("seed %d: reopen: %v", seed, err)
	}
	got := readState(t, s2)
	if !got.equal(committed) && (pending == nil || !got.equal(pending)) {
		t.Fatalf("seed %d: after the crash the store holds neither the last counted round nor the one in progress", seed)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("seed %d: close: %v", seed, err)
	}
	// Opening again after a clean close changes nothing.
	s3, err := Open("w", Options{fs: mfs})
	if err != nil {
		t.Fatalf("seed %d: second reopen: %v", seed, err)
	}
	if !readState(t, s3).equal(got) {
		t.Fatalf("seed %d: second reopen differs", seed)
	}
	_ = s3.Close()
}

func TestReadsDuringCommits(t *testing.T) {
	s, err := Open(t.TempDir(), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	id, _ := s.File("r.0.0.hx")
	var b Batch
	val := func(i, r int) []byte { return bytes.Repeat([]byte{byte(i), byte(r)}, 500+i) }
	for i := range 50 {
		b.Put(id, 1, []byte{byte(i)}, val(i, 0))
	}
	if _, err := s.Commit(&b); err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() {
		var buf []byte
		for n := 0; n < 20000; n++ {
			i := n % 50
			v, err := s.Get(id, []byte{byte(i)}, buf)
			if err != nil {
				done <- err
				return
			}
			if len(v) != len(val(i, 0)) || v[0] != byte(i) {
				done <- fmt.Errorf("key %d: wrong body", i)
				return
			}
			buf = v
		}
		done <- nil
	}()
	for r := 1; r < 100; r++ {
		b.Reset()
		for i := range 50 {
			b.Put(id, 1, []byte{byte(i)}, val(i, r))
		}
		if _, err := s.Commit(&b); err != nil {
			t.Fatal(err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSpace(t *testing.T) {
	sp := emptySpace()
	a := sp.alloc(128)
	b := sp.alloc(64)
	c := sp.alloc(256)
	sp.release(b, 64)
	if got := sp.alloc(64); got != b {
		t.Fatalf("reuse: got %d want %d", got, b)
	}
	sp.release(a, 128)
	sp.release(b, 64)
	if len(sp.free) != 1 || sp.free[0] != (extent{off: a, size: 192}) {
		t.Fatalf("merge: %v", sp.free)
	}
	sp.release(c, 256)
	if sp.end != dataStart || len(sp.free) != 0 {
		t.Fatalf("end: %d %v", sp.end, sp.free)
	}
	if !sp.take(extent{off: dataStart + 64, size: 64}) || sp.end != dataStart+128 || len(sp.free) != 1 {
		t.Fatalf("take: %d %v", sp.end, sp.free)
	}
	if sp.take(extent{off: dataStart + 64, size: 64}) {
		t.Fatal("take of a used extent succeeded")
	}
}

func TestIndexApply(t *testing.T) {
	var ix index
	keys := []string{"d", "b", "f", "a", "c", "e"}
	var ch []change
	for i, k := range slices.Sorted(slices.Values(keys)) {
		ch = append(ch, change{key: []byte(k), e: entry{off: dataStart + uint64(i)*64, size: 41, round: 1, kind: 1}})
	}
	ix.apply(ch, nil)
	old := ix.apply([]change{
		{key: []byte("a"), del: true},
		{key: []byte("b"), e: entry{off: 1 << 20, size: 41, round: 2, kind: 1}},
		{key: []byte("bb"), e: entry{off: 2 << 20, size: 42, round: 2, kind: 1}},
		{key: []byte("f"), del: true},
		{key: []byte("g"), e: entry{off: 3 << 20, size: 41, round: 2, kind: 1}},
	}, nil)
	if len(old) != 3 {
		t.Fatalf("old entries: %d", len(old))
	}
	var got []string
	for _, e := range ix.ents {
		got = append(got, string(ix.key(e)))
	}
	if want := []string{"b", "bb", "c", "d", "e", "g"}; !slices.Equal(got, want) {
		t.Fatalf("keys %v, want %v", got, want)
	}
	enc := ix.encode(nil)
	dec, err := decodeIndex(enc, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec.encode(nil), enc) {
		t.Fatal("index does not round-trip")
	}
}
