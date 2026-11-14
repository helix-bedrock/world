package lsm

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// TestSharedBigBlock reads tables whose big blocks hold small entries before the big
// value, as BDS writes them (a chunk's sub chunks, then its block entities of hundreds
// of megabytes): every key must be seen. The tables are written with own-block big
// values turned off, which gives BDS's layout, and read with the defaults.
func TestSharedBigBlock(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{BigValue: 1 << 30, WriteBuffer: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(7, 8))
	model := map[string][]byte{}
	for i := range 40 {
		size := 100 + rng.IntN(200)
		if i%10 == 7 {
			size = 3 << 20 // after seven small entries in the same block
		}
		k := fmt.Sprintf("k%03d", i)
		model[k] = make([]byte, size)
		for j := range model[k] {
			model[k][j] = byte(rng.Uint32()) // incompressible: the block stays big on disk
		}
		if err := w.Put([]byte(k), model[k]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening writes the log into a table, still with BDS's layout.
	w, err = Open(dir, Options{BigValue: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for _, ro := range []bool{true, false} {
		db, err := Open(dir, Options{ReadOnly: ro})
		if err != nil {
			t.Fatal(err)
		}
		checkAgainst(t, db, model)
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
