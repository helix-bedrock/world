package store

import (
	"encoding/binary"
	"math/rand/v2"
	"testing"
)

// benchStore holds 1024 chunk-sized records in one region file.
func benchStore(b *testing.B, size int) (*Store, FileID, [][]byte) {
	b.Helper()
	s, err := Open(b.TempDir(), Options{NoSync: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	id, _ := s.File("overworld/r.0.0.hx")
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test randomness
	keys := make([][]byte, 1024)
	var batch Batch
	for i := range keys {
		keys[i] = binary.BigEndian.AppendUint64(nil, uint64(i)<<8|1) //nolint:gosec // small
		batch.Put(id, 1, keys[i], testValue(rng, size))
	}
	if _, err := s.Commit(&batch); err != nil {
		b.Fatal(err)
	}
	return s, id, keys
}

func BenchmarkGet(b *testing.B) {
	s, id, keys := benchStore(b, 16<<10)
	dst := make([]byte, 0, 32<<10)
	var err error
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if dst, err = s.Get(id, keys[i%len(keys)], dst); err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(16 << 10)
}

func BenchmarkGetParallel(b *testing.B) {
	s, id, keys := benchStore(b, 16<<10)
	b.ReportAllocs()
	b.SetBytes(16 << 10)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		dst := make([]byte, 0, 32<<10)
		var err error
		for i := 0; pb.Next(); i++ {
			if dst, err = s.Get(id, keys[i%len(keys)], dst); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// BenchmarkCommit saves 64 changed chunk parts per round, as a busy area does.
func BenchmarkCommit(b *testing.B) {
	s, id, keys := benchStore(b, 16<<10)
	rng := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // test randomness
	vals := make([][]byte, 64)
	for i := range vals {
		vals[i] = testValue(rng, 16<<10)
	}
	var batch Batch
	b.ReportAllocs()
	b.SetBytes(64 * 16 << 10)
	b.ResetTimer()
	for i := range b.N {
		batch.Reset()
		for j := range vals {
			batch.Put(id, 1, keys[(i*64+j)%len(keys)], vals[j])
		}
		if _, err := s.Commit(&batch); err != nil {
			b.Fatal(err)
		}
	}
}
