package mcdb

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/helix-bedrock/world/leveldb"
)

// DefaultFlushInterval returns how often pending writes reach LevelDB: BDS
// 1.26.60 flushes one batch every 5 seconds, plus a last one on stop.
func DefaultFlushInterval() time.Duration {
	return 5 * time.Second
}

// pendingValue is a buffered Put, or a Delete if deleted is set.
type pendingValue struct {
	data    []byte
	deleted bool
}

// writeBuffer collects writes, like BDS's pending batch, and flushes them as
// one LevelDB batch. Reads check it first, so a value is visible as soon as
// it is stored. Safe for concurrent use.
type writeBuffer struct {
	ldb *leveldb.DB
	// flushMu lets one flush run at a time.
	flushMu sync.Mutex
	mu      sync.Mutex
	pending map[string]pendingValue
	// flushing holds the batch being written, still visible to reads until
	// LevelDB has it.
	flushing map[string]pendingValue
}

func newWriteBuffer(ldb *leveldb.DB) *writeBuffer {
	return &writeBuffer{ldb: ldb, pending: map[string]pendingValue{}}
}

// txn is a set of writes applied to the buffer at once, so a flush never
// holds half of a chunk.
type txn struct {
	keys [][]byte
	vals []pendingValue
}

func (t *txn) Put(key, value []byte) {
	t.keys = append(t.keys, key)
	t.vals = append(t.vals, pendingValue{data: value})
}

func (t *txn) Delete(key []byte) {
	t.keys = append(t.keys, key)
	t.vals = append(t.vals, pendingValue{deleted: true})
}

// apply adds t's writes to the next flush.
func (w *writeBuffer) apply(t *txn) {
	w.mu.Lock()
	for i, k := range t.keys {
		w.pending[string(k)] = t.vals[i]
	}
	w.mu.Unlock()
}

// get returns the value of key, buffered or on disk. A missing key gives
// leveldb.ErrNotFound.
func (w *writeBuffer) get(key []byte) ([]byte, error) {
	w.mu.Lock()
	v, ok := w.pending[string(key)]
	if !ok {
		v, ok = w.flushing[string(key)]
	}
	w.mu.Unlock()
	if ok {
		if v.deleted {
			return nil, leveldb.ErrNotFound
		}
		return v.data, nil
	}
	return w.ldb.Get(key, nil)
}

// has reports whether key exists, buffered or on disk.
func (w *writeBuffer) has(key []byte) (bool, error) {
	_, err := w.get(key)
	if errors.Is(err, leveldb.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// flush writes everything pending as one batch, with extra written last.
// On failure the writes stay pending for the next flush.
func (w *writeBuffer) flush(extra *txn) error {
	w.flushMu.Lock()
	defer w.flushMu.Unlock()

	w.mu.Lock()
	if extra != nil {
		for i, k := range extra.keys {
			w.pending[string(k)] = extra.vals[i]
		}
	}
	if len(w.pending) == 0 {
		w.mu.Unlock()
		return nil
	}
	w.flushing, w.pending = w.pending, map[string]pendingValue{}
	w.mu.Unlock()

	batch := leveldb.MakeBatch(len(w.flushing))
	for k, v := range w.flushing {
		if v.deleted {
			batch.Delete([]byte(k))
		} else {
			batch.Put([]byte(k), v.data)
		}
	}
	err := w.ldb.Write(batch, nil)

	w.mu.Lock()
	if err != nil {
		// Writes stored since the swap are newer and win.
		for k, v := range w.flushing {
			if _, ok := w.pending[k]; !ok {
				w.pending[k] = v
			}
		}
	}
	w.flushing = nil
	w.mu.Unlock()
	if err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}
