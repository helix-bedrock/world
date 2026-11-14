package convert

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/helix-bedrock/world/hx"
	"github.com/helix-bedrock/world/lsm"
)

// streamAt is the block entity value size from which an export streams the
// value into the database instead of building it in memory.
const streamAt = 1 << 20

// Export converts the Helix world in hxDir into a new BDS world in bdsDir,
// which must not hold a database. The world must not be open elsewhere; for
// a world a server has open, use ExportWorld.
func Export(hxDir, bdsDir string, opts Options) error {
	opts = opts.withDefaults()
	w, err := hx.Open(hxDir, hx.Config{Store: opts.Store})
	if err != nil {
		return fmt.Errorf("convert: open Helix world: %w", err)
	}
	defer w.Close()
	return ExportWorld(w, bdsDir, opts)
}

// ExportWorld converts an open Helix world into a new BDS world in bdsDir,
// which must not hold a database. It reads one snapshot of the world, so it
// may run while a server plays it: the export shows the newest round saved
// when it began.
func ExportWorld(w *hx.World, bdsDir string, opts Options) error {
	opts = opts.withDefaults()
	if _, err := os.Stat(filepath.Join(bdsDir, "db", "CURRENT")); err == nil {
		return fmt.Errorf("convert: %s already holds a database", bdsDir)
	}
	snap, err := w.Snapshot()
	if err != nil {
		return fmt.Errorf("convert: %w", err)
	}
	defer snap.Release()
	db, err := lsm.Open(filepath.Join(bdsDir, "db"), lsm.Options{})
	if err != nil {
		return fmt.Errorf("convert: create BDS world: %w", err)
	}
	ex := &exporter{snap: snap, db: db, opts: opts, table: w.Blocks()}
	err = ex.run(w.Dir(), bdsDir)
	if cerr := db.Close(); err == nil {
		err = cerr
	}
	return err
}

type exporter struct {
	snap  *hx.Snapshot
	db    *lsm.DB
	opts  Options
	table *hx.BlockTable
	b     lsm.Batch
	size  int
	prog  progress

	rec, objRec, key, val []byte
	terrain               hx.Terrain
	objects               hx.Objects
}

func (ex *exporter) run(hxDir, bdsDir string) error {
	ex.prog = newProgress(ex.opts)
	ex.prog.phase(PhaseFiles)
	if err := copyFiles(filepath.Join(hxDir, BDSFilesDir), bdsDir, nil); err != nil {
		return err
	}
	ld, err := ex.snap.LevelDatBytes(nil)
	if err == nil {
		if err := os.WriteFile(filepath.Join(bdsDir, hx.LevelDat), ld, 0o644); err != nil { //nolint:gosec // a world file
			return fmt.Errorf("convert: write level.dat: %w", err)
		}
	} else if !errors.Is(err, hx.ErrNotFound) {
		return fmt.Errorf("convert: %w", err)
	}
	dims, err := dimensions(hxDir)
	if err != nil {
		return err
	}
	ex.prog.phase(PhaseChunks)
	for _, dim := range dims {
		if err := ex.dimension(dim); err != nil {
			return err
		}
	}
	ex.prog.phase(PhaseRecords)
	if err := ex.records(); err != nil {
		return err
	}
	if err := ex.flush(); err != nil {
		return err
	}
	ex.prog.phase(PhaseDone)
	return nil
}

// dimensions lists the dimensions with a region folder.
func dimensions(hxDir string) ([]hx.Dimension, error) {
	ents, err := os.ReadDir(hxDir)
	if err != nil {
		return nil, fmt.Errorf("convert: %w", err)
	}
	var dims []hx.Dimension
	for _, e := range ents {
		if d, ok := hx.ParseDimensionDir(e.Name()); ok && e.IsDir() {
			dims = append(dims, d)
		}
	}
	return dims, nil
}

func (ex *exporter) dimension(dim hx.Dimension) error {
	var cerr error
	err := ex.snap.EachChunk(dim, func(pos hx.ChunkPos) bool {
		cerr = ex.chunk(chunkID{dim: dim, pos: pos})
		return cerr == nil
	})
	if cerr != nil {
		return cerr
	}
	if err != nil {
		return fmt.Errorf("convert: %w", err)
	}
	return nil
}

// chunk writes one chunk's keys.
func (ex *exporter) chunk(id chunkID) error {
	ex.prog.chunk(id)
	ex.prog.p.Chunks++
	var err error
	if ex.rec, err = ex.snap.TerrainRecord(id.dim, id.pos, ex.rec); err != nil {
		return fmt.Errorf("convert: chunk %v: %w", id, err)
	}
	t := &ex.terrain
	if err := hx.DecodeTerrain(ex.rec, t); err != nil {
		return fmt.Errorf("convert: chunk %v: %w", id, err)
	}
	for _, k := range t.Keys {
		ex.put(ex.chunkKey(id, k.Tag), k.Value)
	}
	if t.Data3D != nil {
		ex.put(ex.chunkKey(id, tagData3D), t.Data3D)
	}
	for i := range t.Subs {
		s := &t.Subs[i]
		if ex.val, err = appendBDSSub(ex.val[:0], s, ex.table); err != nil {
			return fmt.Errorf("convert: chunk %v: %w", id, err)
		}
		ex.put(append(ex.chunkKey(id, tagSubChunk), byte(s.Index)), ex.val)
	}
	if err := ex.chunkObjects(id); err != nil {
		return err
	}
	return ex.maybeFlush()
}

func (ex *exporter) chunkKey(id chunkID, tag byte) []byte {
	ex.key = append(appendChunkPrefix(ex.key[:0], id), tag)
	return ex.key
}

func (ex *exporter) chunkObjects(id chunkID) error {
	var err error
	ex.objRec, err = ex.snap.ObjectsRecord(id.dim, id.pos, ex.objRec)
	if errors.Is(err, hx.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("convert: objects of chunk %v: %w", id, err)
	}
	o := &ex.objects
	if err := hx.DecodeObjects(ex.objRec, o); err != nil {
		return fmt.Errorf("convert: objects of chunk %v: %w", id, err)
	}
	for _, k := range o.Keys {
		ex.put(ex.chunkKey(id, k.Tag), k.Value)
	}
	if len(o.BlockEntities) > 0 {
		if err := ex.blockEntities(id, o.BlockEntities); err != nil {
			return err
		}
	}
	if o.Digp {
		return ex.entities(id, o.Entities)
	}
	return nil
}

// blockEntities writes the chunk's block entity value: built in memory when
// small, streamed one shell at a time when big.
func (ex *exporter) blockEntities(id chunkID, shells []hx.Shell) error {
	size := 0
	for _, s := range shells {
		size += s.Size()
	}
	key := ex.chunkKey(id, hx.TagBlockEntities)
	if size < streamAt {
		ex.val = ex.val[:0]
		for _, s := range shells {
			var err error
			if ex.val, err = ex.snap.ExpandShell(id.dim, id.pos, s, ex.val); err != nil {
				return fmt.Errorf("convert: block entities of chunk %v: %w", id, err)
			}
		}
		ex.put(key, ex.val)
		ex.prog.p.BlockEntities += int64(len(shells))
		return nil
	}
	ex.prog.p.Keys++
	ex.prog.p.Bytes += int64(size)
	ex.prog.p.BlockEntities += int64(len(shells))
	// The shells point into objRec: write the batch before it is reused.
	sr := &shellReader{snap: ex.snap, id: id, shells: shells}
	ex.b.PutStream(key, func() (io.ReadCloser, int64, error) { return sr, int64(size), nil })
	if err := ex.flush(); err != nil {
		return err
	}
	if sr.err != nil {
		return fmt.Errorf("convert: block entities of chunk %v: %w", id, sr.err)
	}
	return nil
}

// entities writes the chunk's digp list and actor records.
func (ex *exporter) entities(id chunkID, ents []hx.Entity) error {
	ex.val = ex.val[:0]
	for _, e := range ents {
		ex.val = append(ex.val, e.ID...)
	}
	ex.key = appendChunkPrefix(append(ex.key[:0], digpPrefix...), id)
	ex.put(ex.key, ex.val)
	for _, e := range ents {
		if e.Shell.Size() == 0 {
			continue
		}
		var err error
		if ex.val, err = ex.snap.ExpandShell(id.dim, id.pos, e.Shell, ex.val[:0]); err != nil {
			return fmt.Errorf("convert: entity of chunk %v: %w", id, err)
		}
		ex.key = append(append(ex.key[:0], actorPrefix...), e.ID...)
		ex.put(ex.key, ex.val)
		ex.prog.p.Entities++
		if err := ex.maybeFlush(); err != nil {
			return err
		}
	}
	return nil
}

// records writes every level key and player record as stored.
func (ex *exporter) records() error {
	type each func(fn func(key []byte) bool) error
	type get func(key string, dst []byte) ([]byte, error)
	for _, src := range [...]struct {
		each each
		get  get
	}{{ex.snap.EachLevel, ex.snap.Level}, {ex.snap.EachPlayerRecord, ex.snap.PlayerRecord}} {
		var gerr error
		err := src.each(func(key []byte) bool {
			if ex.val, gerr = src.get(string(key), ex.val); gerr != nil {
				return false
			}
			ex.put(key, ex.val)
			gerr = ex.maybeFlush()
			return gerr == nil
		})
		if gerr != nil {
			return fmt.Errorf("convert: %w", gerr)
		}
		if err != nil {
			return fmt.Errorf("convert: %w", err)
		}
	}
	return nil
}

func (ex *exporter) put(key, value []byte) {
	ex.b.Put(key, value)
	ex.size += len(key) + len(value)
	ex.prog.p.Keys++
	ex.prog.p.Bytes += int64(len(value))
}

func (ex *exporter) maybeFlush() error {
	ex.prog.tick()
	if ex.size < ex.opts.RoundSize {
		return nil
	}
	return ex.flush()
}

func (ex *exporter) flush() error {
	if ex.b.Len() == 0 {
		return nil
	}
	if err := ex.db.Write(&ex.b); err != nil {
		return fmt.Errorf("convert: write BDS world: %w", err)
	}
	ex.b.Reset()
	ex.size = 0
	return nil
}

// shellReader reads a chunk's block entity value, expanding one shell at a
// time.
type shellReader struct {
	snap   *hx.Snapshot
	id     chunkID
	shells []hx.Shell
	cur    []byte
	off    int
	err    error
}

func (sr *shellReader) Read(p []byte) (int, error) {
	for sr.off == len(sr.cur) {
		if len(sr.shells) == 0 {
			return 0, io.EOF
		}
		var err error
		if sr.cur, err = sr.snap.ExpandShell(sr.id.dim, sr.id.pos, sr.shells[0], sr.cur[:0]); err != nil {
			sr.err = err
			return 0, err
		}
		sr.shells, sr.off = sr.shells[1:], 0
	}
	n := copy(p, sr.cur[sr.off:])
	sr.off += n
	return n, nil
}

func (sr *shellReader) Close() error { return nil }

// bdsSkip leaves out what Import stores itself: the database and level.dat.
func bdsSkip(rel string) bool {
	return rel == "db" || rel == hx.LevelDat
}

// copyFiles copies the files under src to dst, leaving out paths (relative,
// slash-separated) skip reports true for. A missing src copies nothing.
func copyFiles(src, dst string, skip func(rel string) bool) error {
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil || rel == "." {
			return err
		}
		if skip != nil && skip(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		return copyFile(path, out)
	})
	if err != nil {
		return fmt.Errorf("convert: copy world files: %w", err)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
