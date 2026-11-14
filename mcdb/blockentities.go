package mcdb

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/cube"
)

// A chunk's block entities are one value, as BDS stores them, and chests full of books make
// it hundreds of megabytes. Helix never holds it whole: it is read as a stream one compound
// at a time (EachBlockEntity, LoadBlockEntity), and a save that keeps compounds the caller
// left on disk copies them from the stored value as a stream into the new one (streamValue),
// which a KVStreamer writes without holding it either.

// bigValue is the size from which a block entity value that keeps stored compounds is
// written as a stream instead of being put together in memory.
const bigValue = 1 << 20

// EachBlockEntity calls fn with the position and stored compound of each block entity of the
// chunk, as stored, in order. It sees buffered writes. The compound is only valid during the
// call: fn copies what it keeps. Only the biggest single compound is held at a time. An
// error from fn stops it and is returned. Safe for concurrent use.
func (db *DB) EachBlockEntity(chunkPos ChunkPos, dim Dimension, fn func(pos cube.Pos, raw []byte) error) error {
	db.buf.flushMu.RLock()
	defer db.buf.flushMu.RUnlock()
	err := db.eachBlockEntity(dbKey{pos: chunkPos, dim: dim}, fn)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("block entities of %v (%v): %w", chunkPos, dim, err)
	}
	return nil
}

// eachBlockEntity is EachBlockEntity with flushes already held off. A chunk with no block
// entities gives ErrNotFound.
func (db *DB) eachBlockEntity(k dbKey, fn func(pos cube.Pos, raw []byte) error) error {
	rc, size, err := db.blockEntityStream(k)
	if err != nil {
		return err
	}
	defer rc.Close()
	cr := nbt.NewCompoundReader(rc, size, nbt.LittleEndian)
	for {
		c, err := cr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode nbt: %w", err)
		}
		pos, err := rawBlockPos(c)
		if err != nil {
			return fmt.Errorf("decode nbt: %w", err)
		}
		if err := fn(pos, c); err != nil {
			return err
		}
	}
}

// errFound stops a walk once the block entity looked for is found.
var errFound = errors.New("found")

// LoadBlockEntity returns the stored compound of the block entity at pos in the chunk at
// chunkPos, decoding only that one and holding no more of the chunk's block entities than
// the biggest one. It sees buffered writes. Safe for concurrent use.
func (db *DB) LoadBlockEntity(chunkPos ChunkPos, dim Dimension, pos cube.Pos) (map[string]any, error) {
	var m map[string]any
	db.buf.flushMu.RLock()
	err := db.eachBlockEntity(dbKey{pos: chunkPos, dim: dim}, func(at cube.Pos, c []byte) error {
		if at != pos {
			return nil
		}
		var err error
		if m, err = nbt.DecodeWithout(c, nbt.LittleEndian, nil); err != nil {
			return err
		}
		return errFound
	})
	db.buf.flushMu.RUnlock()
	if errors.Is(err, errFound) {
		return m, nil
	}
	if err == nil {
		err = ErrNotFound
	}
	return nil, fmt.Errorf("load block entity %v: %w", pos, err)
}

// blockEntityStream reads the chunk's block entity value, buffered or stored, as a stream
// with its size.
func (db *DB) blockEntityStream(k dbKey) (io.ReadCloser, int64, error) {
	key := k.Sum(keyBlockEntities)
	if v, ok := db.buf.buffered(key); ok {
		return v.open()
	}
	return db.kvStream(key)
}

// kvStream reads a stored value as a stream.
func (db *DB) kvStream(key []byte) (io.ReadCloser, int64, error) {
	if s, ok := db.kv.(KVStreamer); ok {
		return s.GetStream(key)
	}
	v, err := db.kv.Get(key)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(v)), int64(len(v)), nil
}

// storeBlockEntities writes the chunk's block entities, or deletes the key when there are
// none, as BDS does. The compounds of those at keep are copied from the value stored now:
// in memory for a small one, as a stream written at the flush for a big one.
func (db *DB) storeBlockEntities(t *txn, k dbKey, blockEntities []chunk.BlockEntity, keep []cube.Pos) error {
	live := db.encodeBlockEntities(blockEntities)
	key := k.Sum(keyBlockEntities)
	if len(keep) == 0 {
		if len(live) == 0 {
			t.Delete(key)
		} else {
			t.Put(key, live)
		}
		return nil
	}
	src := db.source(key)
	s := &streamValue{src: src, keep: keep, live: live}
	size, err := s.count()
	if err != nil {
		return fmt.Errorf("read kept: %w", err)
	}
	if _, ok := db.kv.(KVStreamer); ok && size >= bigValue {
		s.size = size
		t.PutStream(key, s)
		return nil
	}
	rc, _, err := s.open()
	if err != nil {
		return fmt.Errorf("read kept: %w", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("read kept: %w", err)
	}
	t.Put(key, data)
	return nil
}

// source is the value at key as it stands now, to read later: the buffered write, or the
// store's value, which only a flush changes, and the flush that writes over it writes the
// value read from it.
func (db *DB) source(key []byte) func() (io.ReadCloser, int64, error) {
	if v, ok := db.buf.buffered(key); ok {
		return v.open
	}
	return func() (io.ReadCloser, int64, error) { return db.kvStream(key) }
}

// encodeBlockEntities encodes the block entities in memory, the compounds of those given
// raw as they are.
func (db *DB) encodeBlockEntities(blockEntities []chunk.BlockEntity) []byte {
	buf := bytes.NewBuffer(nil)
	enc := nbt.NewEncoderWithEncoding(buf, nbt.LittleEndian)
	for _, b := range blockEntities {
		if b.Raw != nil {
			buf.Write(b.Raw)
			continue
		}
		b.Data["x"], b.Data["y"], b.Data["z"] = int32(b.Pos[0]), int32(b.Pos[1]), int32(b.Pos[2]) //nolint:gosec // block coordinates fit in int32
		if err := enc.Encode(b.Data); err != nil {
			db.conf.Log.Error("store block entities: encode nbt: " + err.Error())
		}
	}
	return buf.Bytes()
}

// streamValue is a block entity value put together as it is read: the compounds at keep
// copied from src, then live. Kept compounds missing from src are an error: the caller holds
// nothing else of them, so the save must not go on as if they were gone.
type streamValue struct {
	src  func() (io.ReadCloser, int64, error)
	keep []cube.Pos
	live []byte
	// keepSet is keep as a set, made on the first read.
	keepSet map[cube.Pos]struct{}
	// size is the value's size, counted when it was stored.
	size int64
}

// count reads the value through once to count its size, checking every kept compound is
// there.
func (s *streamValue) count() (int64, error) {
	rc, _, err := s.open()
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	return io.Copy(io.Discard, rc)
}

// open starts reading the value.
func (s *streamValue) open() (io.ReadCloser, int64, error) {
	src, size, err := s.src()
	if err != nil {
		return nil, 0, err
	}
	if s.keepSet == nil {
		s.keepSet = make(map[cube.Pos]struct{}, len(s.keep))
		for _, p := range s.keep {
			s.keepSet[p] = struct{}{}
		}
	}
	return &keptReader{v: s, src: src, cr: nbt.NewCompoundReader(src, size, nbt.LittleEndian)}, s.size, nil
}

// keptReader reads a streamValue.
type keptReader struct {
	v     *streamValue
	src   io.Closer
	cr    *nbt.CompoundReader
	cur   []byte // what is left of the compound being read
	found int
	live  int // how much of the live part is read, once the kept part is done
	err   error
}

func (r *keptReader) Read(p []byte) (int, error) {
	for len(r.cur) == 0 && r.err == nil {
		r.err = r.nextKept()
	}
	if len(r.cur) > 0 {
		n := copy(p, r.cur)
		r.cur = r.cur[n:]
		return n, nil
	}
	if r.err != io.EOF {
		return 0, r.err
	}
	if r.live >= len(r.v.live) {
		return 0, io.EOF
	}
	n := copy(p, r.v.live[r.live:])
	r.live += n
	return n, nil
}

// nextKept moves to the next kept compound of the source, or gives io.EOF once all are read.
func (r *keptReader) nextKept() error {
	for {
		c, err := r.cr.Next()
		if err == io.EOF {
			if r.found != len(r.v.keep) {
				return fmt.Errorf("%d of %d kept block entities stored", r.found, len(r.v.keep))
			}
			return io.EOF
		}
		if err != nil {
			return fmt.Errorf("decode nbt: %w", err)
		}
		pos, err := rawBlockPos(c)
		if err != nil {
			return fmt.Errorf("decode nbt: %w", err)
		}
		if _, ok := r.v.keepSet[pos]; ok {
			r.found++
			r.cur = c
			return nil
		}
	}
}

func (r *keptReader) Close() error { return r.src.Close() }

// rawBlockPos reads the position of a stored block entity compound, reading past everything
// else.
func rawBlockPos(c []byte) (cube.Pos, error) {
	m, err := nbt.DecodeWithout(c, nbt.LittleEndian, notBlockPos)
	if err != nil {
		return cube.Pos{}, err
	}
	return blockPosFromNBT(m), nil
}

// notBlockPos is every tag name but a block entity's x, y and z.
func notBlockPos(name string) bool { return name != "x" && name != "y" && name != "z" }
