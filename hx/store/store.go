package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
)

// Options configures a store. The zero value is ready to use.
type Options struct {
	// NoSync skips forcing writes to disk. A store then survives a crash of
	// the program but may lose recent rounds if the machine stops.
	NoSync bool
	// Level is how hard records are compressed.
	Level Level
	// IndexCache is how many bytes of file indexes stay in memory while not in
	// use (default 16 MB). Files with changes not yet checkpointed stay too.
	IndexCache int64
	// JournalLimit is how big the journal may grow before every file is
	// checkpointed and the journal emptied (default 64 MB).
	JournalLimit int64

	fs fsys
}

const (
	defaultIndexCache   = 16 << 20
	defaultJournalLimit = 64 << 20
)

func (o Options) withDefaults() Options {
	if o.IndexCache <= 0 {
		o.IndexCache = defaultIndexCache
	}
	if o.JournalLimit <= 0 {
		o.JournalLimit = defaultJournalLimit
	}
	if o.fs == nil {
		o.fs = osFS{}
	}
	return o
}

// Store is an open store. Reads are safe from any goroutine and never wait for
// a save's I/O; Commit, Checkpoint and Close run one at a time.
type Store struct {
	dir   string
	opts  Options
	fs    fsys
	codec *codec

	filesMu sync.RWMutex
	files   []*hxFile
	byPath  map[string]FileID

	loadedMu sync.Mutex
	loaded   []*hxFile

	indexBytes atomic.Int64
	// maxRound is the highest round any loaded file holds, in case the journal
	// was lost: new rounds always come after it.
	maxRound atomic.Uint64
	closed   atomic.Bool
	cur      atomic.Pointer[generation]
	scratch  sync.Pool // *[]byte

	// commitMu serialises Commit, Checkpoint and Close; the fields below it
	// belong to whoever holds it.
	commitMu sync.Mutex
	j        *journal
	round    uint64
	oldest   *generation
	failed   error
	w        writer
}

// Open opens the store in dir, creating it if needed, and replays its journal.
func Open(dir string, opts Options) (*Store, error) {
	opts = opts.withDefaults()
	s := &Store{dir: dir, opts: opts, fs: opts.fs, byPath: map[string]FileID{}}
	if err := s.fs.MkdirAll(filepath.Join(dir, journalDir)); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", dir, err)
	}
	c, err := newCodec(opts.Level)
	if err != nil {
		return nil, err
	}
	s.codec = c
	if err := s.recover(); err != nil {
		s.closeFiles()
		c.close()
		return nil, err
	}
	return s, nil
}

// recover replays the journal over the files, checkpoints them and starts an
// empty journal.
func (s *Store) recover() error {
	jdir := filepath.Join(s.dir, journalDir)
	rep, err := replayJournal(s.fs, jdir)
	if err != nil {
		return err
	}
	s.round = rep.round
	g := &generation{round: rep.round}
	s.cur.Store(g)
	s.oldest = g
	for _, c := range rep.commits {
		if err := s.replayCommit(c); err != nil {
			return err
		}
	}
	s.round = max(s.round, s.maxRound.Load())
	if err := s.checkpointDirty(); err != nil {
		return err
	}
	s.j = &journal{fs: s.fs, dir: jdir, sync: !s.opts.NoSync, seq: rep.lastSeq, old: rep.segs}
	return s.j.rotate(s.round)
}

// replayCommit applies one journal commit to the files it touched, unless
// their checkpoint already holds it.
func (s *Store) replayCommit(c replayCommit) error {
	for _, rf := range c.files {
		id, err := s.File(rf.path)
		if err != nil {
			return err
		}
		hf, err := s.fileOf(id)
		if err != nil {
			return err
		}
		if err := hf.load(s); err != nil {
			return err
		}
		if c.round <= hf.round {
			continue
		}
		if hf.f == nil {
			return errCorrupt(rf.path, "journal names a file that does not exist")
		}
		if err := hf.replay(s, rf.changes, c.round); err != nil {
			return err
		}
	}
	return nil
}

// replay applies a round's changes found in the journal: the space its new
// records use is taken, and what they replaced is freed at once (no reader
// exists yet).
func (hf *hxFile) replay(s *Store, changes []change, round uint64) error {
	old := hf.ix.apply(changes, nil)
	for _, o := range old {
		hf.sp.release(o.off, alignUp(o.size))
	}
	for _, c := range changes {
		if !c.del && !hf.sp.take(c.e.extent()) {
			return errCorrupt(hf.path, "journal record overlaps another")
		}
	}
	hf.round, hf.dirty = round, true
	hf.updateMem(s)
	s.w.dirty = append(s.w.dirty, hf)
	return nil
}

// File returns the ID of the file at path: relative, slash-separated, ending
// in ".hx", such as "overworld/r.0.-1.hx". The file need not exist; the first
// write creates it.
func (s *Store) File(path string) (FileID, error) {
	s.filesMu.RLock()
	id, ok := s.byPath[path]
	s.filesMu.RUnlock()
	if ok {
		return id, nil
	}
	if err := checkPath(path); err != nil {
		return 0, err
	}
	s.filesMu.Lock()
	defer s.filesMu.Unlock()
	if id, ok := s.byPath[path]; ok {
		return id, nil
	}
	id = FileID(len(s.files)) //nolint:gosec // far fewer than 4 billion files
	s.files = append(s.files, &hxFile{id: id, path: path, full: filepath.Join(s.dir, filepath.FromSlash(path))})
	s.byPath[path] = id
	return id, nil
}

func (s *Store) fileOf(id FileID) (*hxFile, error) {
	s.filesMu.RLock()
	defer s.filesMu.RUnlock()
	if int(id) >= len(s.files) {
		return nil, fmt.Errorf("store: unknown file ID %d", id)
	}
	return s.files[id], nil
}

// Info describes a stored record.
type Info struct {
	// Kind is what the caller said the record is.
	Kind byte
	// Size is the body's length.
	Size int64
	// Round is the save round that wrote it.
	Round uint64
}

func (e entry) info() Info { return Info{Kind: e.kind, Size: int64(e.raw), Round: e.round} } //nolint:gosec // record sizes

// Get returns key's body in dst[:0], growing dst only if it is too small, or
// ErrNotFound. With a big enough dst it allocates nothing.
func (s *Store) Get(id FileID, key, dst []byte) ([]byte, error) {
	if s.closed.Load() {
		return dst[:0], ErrClosed
	}
	hf, err := s.fileOf(id)
	if err != nil {
		return dst[:0], err
	}
	g := s.pin()
	defer s.unpin(g)
	if err := hf.acquire(s, g.round); err != nil {
		return dst[:0], err
	}
	defer hf.releaseRead()
	e, f, ok := hf.find(key)
	if !ok {
		return dst[:0], ErrNotFound
	}
	return s.readRecord(hf, f, e, key, dst)
}

// Stat returns what is stored under key without reading it.
func (s *Store) Stat(id FileID, key []byte) (Info, error) {
	if s.closed.Load() {
		return Info{}, ErrClosed
	}
	hf, err := s.fileOf(id)
	if err != nil {
		return Info{}, err
	}
	g := s.pin()
	defer s.unpin(g)
	if err := hf.acquire(s, g.round); err != nil {
		return Info{}, err
	}
	defer hf.releaseRead()
	e, _, ok := hf.find(key)
	if !ok {
		return Info{}, ErrNotFound
	}
	return e.info(), nil
}

// readRecord reads e's record with one positioned read and decodes its body.
func (s *Store) readRecord(hf *hxFile, f file, e entry, key, dst []byte) ([]byte, error) {
	bp := s.getScratch(e.size)
	defer s.putScratch(bp)
	buf := (*bp)[:e.size]
	if _, err := f.ReadAt(buf, int64(e.off)); err != nil { //nolint:gosec // file offsets
		return dst[:0], fmt.Errorf("store: read %s: %w", hf.path, err)
	}
	h, body, err := checkRecord(buf, key, hf.path)
	if err != nil {
		return dst[:0], err
	}
	out, err := s.codec.decode(dst, body, h.comp, h.raw)
	if err != nil {
		return dst[:0], fmt.Errorf("store: %s: %w", hf.path, err)
	}
	return out, nil
}

// maxScratch is the biggest read buffer kept for reuse.
const maxScratch = 1 << 20

func (s *Store) getScratch(n uint64) *[]byte {
	bp, ok := s.scratch.Get().(*[]byte)
	if !ok {
		b := make([]byte, 0, max(n, 4096))
		bp = &b
	}
	if uint64(cap(*bp)) < n {
		*bp = make([]byte, n)
	}
	return bp
}

func (s *Store) putScratch(bp *[]byte) {
	if cap(*bp) <= maxScratch {
		s.scratch.Put(bp)
	}
}

// Range calls fn for every key of a file, in key order, as of one moment.
func (s *Store) Range(id FileID, fn func(key []byte, info Info) bool) error {
	if s.closed.Load() {
		return ErrClosed
	}
	hf, err := s.fileOf(id)
	if err != nil {
		return err
	}
	g := s.pin()
	defer s.unpin(g)
	if err := hf.acquire(s, g.round); err != nil {
		return err
	}
	hf.mu.RLock()
	keys, ents := slices.Clone(hf.ix.keys), slices.Clone(hf.ix.ents)
	hf.mu.RUnlock()
	hf.releaseRead()
	for _, e := range ents {
		if !fn(keys[e.keyOff:e.keyOff+uint32(e.keyLen)], e.info()) {
			return nil
		}
	}
	return nil
}

// Round is the newest committed save round.
func (s *Store) Round() uint64 { return s.cur.Load().round }

func (s *Store) noteRound(r uint64) {
	for {
		cur := s.maxRound.Load()
		if r <= cur || s.maxRound.CompareAndSwap(cur, r) {
			return
		}
	}
}

func (s *Store) trackLoaded(hf *hxFile) {
	s.loadedMu.Lock()
	s.loaded = append(s.loaded, hf)
	s.loadedMu.Unlock()
}

func (s *Store) syncFile(hf *hxFile) error {
	if s.opts.NoSync {
		return nil
	}
	if err := hf.f.Sync(); err != nil {
		return fmt.Errorf("store: sync %s: %w", hf.path, err)
	}
	return nil
}

// Checkpoint writes every changed index to its file and empties the journal.
func (s *Store) Checkpoint() error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if err := s.usable(); err != nil {
		return err
	}
	return s.checkpointAll()
}

func (s *Store) checkpointAll() error {
	if err := s.checkpointDirty(); err != nil {
		s.failed = err
		return err
	}
	if err := s.j.rotate(s.round); err != nil {
		s.failed = err
		return err
	}
	return nil
}

func (s *Store) checkpointDirty() error {
	for _, hf := range s.w.dirty {
		if err := hf.checkpoint(s); err != nil {
			return err
		}
	}
	clear(s.w.dirty)
	s.w.dirty = s.w.dirty[:0]
	return nil
}

func (s *Store) usable() error {
	if s.closed.Load() {
		return ErrClosed
	}
	return s.failed
}

// evict drops the indexes of files least recently read until the cache is
// within budget. Files with changes not checkpointed, or with extents still
// in limbo, stay.
func (s *Store) evict() {
	for s.indexBytes.Load() > s.opts.IndexCache {
		victim := s.evictionVictim()
		if victim == nil || !victim.tryEvict(s) {
			return
		}
		s.untrackLoaded(victim)
	}
}

func (s *Store) evictionVictim() *hxFile {
	s.loadedMu.Lock()
	defer s.loadedMu.Unlock()
	var victim *hxFile
	for _, hf := range s.loaded {
		if hf.dirty || hf.limbo > 0 || hf.readers.Load() != 0 {
			continue
		}
		if victim == nil || hf.lastUse.Load() < victim.lastUse.Load() {
			victim = hf
		}
	}
	return victim
}

func (s *Store) untrackLoaded(hf *hxFile) {
	s.loadedMu.Lock()
	if i := slices.Index(s.loaded, hf); i >= 0 {
		s.loaded = slices.Delete(s.loaded, i, i+1)
	}
	s.loadedMu.Unlock()
}

// Close checkpoints every file and closes the store. Reads still running must
// have finished.
func (s *Store) Close() error {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if s.closed.Swap(true) {
		return nil
	}
	var errs []error
	if s.failed == nil {
		errs = append(errs, s.checkpointAll())
	}
	errs = append(errs, s.j.close())
	s.closeFiles()
	s.codec.close()
	return errors.Join(errs...)
}

func (s *Store) closeFiles() {
	s.loadedMu.Lock()
	defer s.loadedMu.Unlock()
	for _, hf := range s.loaded {
		if hf.f != nil {
			_ = hf.f.Close()
		}
	}
	s.loaded = nil
}
