package mcdb

import (
	"bytes"
	"fmt"
)

// chunkReader holds every key of one chunk, read from the store in one pass. Loading a
// chunk reads some 30 keys (version, biomes, each sub chunk, block entities, ticks...).
// Read one by one, each Get reads and inflates the whole LevelDB block its key is in,
// and a big value (a chunk of chests full of books: hundreds of megabytes) is a block of
// its own size holding the chunk's other keys too, larger than any block cache: one
// chunk load read it again for every key, tens of gigabytes for a few chunks. A prefix
// iterator reads each block once.
type chunkReader struct {
	buf  *writeBuffer
	vals map[string][]byte
}

// readChunk reads every key of the chunk at k. Flushes wait until release, so a value
// that moves from the write buffer to the store meanwhile is found in one or the other.
func (db *DB) readChunk(k dbKey) (*chunkReader, error) {
	db.buf.flushMu.RLock()
	rd := &chunkReader{buf: db.buf, vals: map[string][]byte{}}
	prefix := k.Sum()
	it := db.kv.NewIterator(prefix)
	for it.Next() {
		key := it.Key()
		// The chunk's own keys are its prefix and a tag, and a sub chunk index for
		// sub chunks. Longer ones under an Overworld prefix are another dimension's.
		if extra := len(key) - len(prefix); extra != 1 && extra != 2 {
			continue
		}
		rd.vals[string(key)] = bytes.Clone(it.Value())
	}
	it.Release()
	if err := it.Error(); err != nil {
		db.buf.flushMu.RUnlock()
		return nil, fmt.Errorf("iterate: %w", err)
	}
	return rd, nil
}

// release lets flushes run again.
func (rd *chunkReader) release() { rd.buf.flushMu.RUnlock() }

// get returns the value of key, buffered or read, or ErrNotFound.
func (rd *chunkReader) get(key []byte) ([]byte, error) {
	if v, ok := rd.buf.buffered(key); ok {
		if v.deleted {
			return nil, ErrNotFound
		}
		return v.data, nil
	}
	if v, ok := rd.vals[string(key)]; ok {
		return v, nil
	}
	return nil, ErrNotFound
}

// has reports whether key exists, buffered or read.
func (rd *chunkReader) has(key []byte) (bool, error) {
	_, err := rd.get(key)
	return err == nil, nil
}
