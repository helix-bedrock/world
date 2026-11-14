package store

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// FileID names a file of a store; Store.File gives it. IDs stay valid until the
// store is closed.
type FileID uint32

// checkPath reports whether p is a file path a store accepts: relative,
// slash-separated, clean, ending in ".hx" and outside the journal.
func checkPath(p string) error {
	switch {
	case p == "" || len(p) > 1024:
		return fmt.Errorf("store: bad file path %q", p)
	case path.Clean(p) != p || path.IsAbs(p) || strings.HasPrefix(p, "../") || strings.Contains(p, "\\"):
		return fmt.Errorf("store: file path %q is not clean and relative", p)
	case !strings.HasSuffix(p, ".hx"):
		return fmt.Errorf("store: file path %q does not end in .hx", p)
	case strings.HasPrefix(p, journalDir+"/"):
		return fmt.Errorf("store: file path %q is in the journal", p)
	}
	return nil
}

// hxFile is one file of the store. Its index is loaded on first use and
// dropped again when the index cache is over budget.
type hxFile struct {
	id   FileID
	path string // as given to Store.File
	full string // on the operating system

	loadMu  sync.Mutex
	loaded  atomic.Bool
	readers atomic.Int64
	// lastUse is the round of the last read, for eviction.
	lastUse atomic.Uint64

	// mu guards ix and f: readers hold it to look a key up, a round holds it
	// to apply its changes.
	mu sync.RWMutex
	ix index
	f  file // nil while the file does not exist

	// Writer state, used only by the commit goroutine (and by load).
	sp        space
	super     superblock
	hasSuper  bool
	newHeader bool // the file exists without a valid header
	dirty     bool // changes applied since the last checkpoint
	round     uint64
	limbo     int   // extents waiting in generations
	mem       int64 // index bytes counted in Store.indexBytes
}

// acquire makes sure the index is loaded and counts a reader, which keeps it
// loaded until releaseRead.
func (hf *hxFile) acquire(s *Store, round uint64) error {
	for {
		hf.readers.Add(1)
		if hf.loaded.Load() {
			hf.lastUse.Store(round)
			return nil
		}
		hf.readers.Add(-1)
		if err := hf.load(s); err != nil {
			return err
		}
	}
}

func (hf *hxFile) releaseRead() { hf.readers.Add(-1) }

// find returns key's entry and the open file holding it.
func (hf *hxFile) find(key []byte) (entry, file, bool) {
	hf.mu.RLock()
	e, ok := hf.ix.get(key)
	f := hf.f
	hf.mu.RUnlock()
	return e, f, ok
}

// load reads the file's header, current superblock and index.
func (hf *hxFile) load(s *Store) error {
	hf.loadMu.Lock()
	defer hf.loadMu.Unlock()
	if hf.loaded.Load() {
		return nil
	}
	if err := hf.open(s); err != nil {
		if hf.f != nil {
			_ = hf.f.Close()
		}
		hf.reset()
		return err
	}
	hf.mem = hf.ix.memBytes()
	s.indexBytes.Add(hf.mem)
	s.trackLoaded(hf)
	hf.loaded.Store(true)
	return nil
}

func (hf *hxFile) open(s *Store) error {
	flag := os.O_RDWR
	if s.opts.ReadOnly {
		flag = os.O_RDONLY
	}
	f, err := s.fs.OpenFile(hf.full, flag)
	if errors.Is(err, fs.ErrNotExist) {
		hf.ix, hf.sp = index{}, emptySpace()
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: open %s: %w", hf.path, err)
	}
	hf.f = f
	return hf.readState(s)
}

// readState reads the header area and the newest valid superblock's index. A
// file without a valid header or superblock was created by a round that never
// counted, or has had no checkpoint yet: its records, if any, come from the
// journal.
func (hf *hxFile) readState(s *Store) error {
	size, err := hf.f.Size()
	if err != nil {
		return fmt.Errorf("store: stat %s: %w", hf.path, err)
	}
	head := make([]byte, min(size, dataStart))
	if _, err := hf.f.ReadAt(head, 0); err != nil {
		return fmt.Errorf("store: read %s: %w", hf.path, err)
	}
	hf.ix, hf.sp = index{}, emptySpace()
	if !checkFileHeader(head) {
		hf.newHeader = true
		return nil
	}
	sb, ok := newestSuper(head)
	if !ok {
		return nil
	}
	if sb.indexOff < dataStart || sb.indexOff+sb.indexSize > uint64(size) { //nolint:gosec // size is non-negative
		return errCorrupt(hf.path, "superblock points outside the file")
	}
	ix, err := hf.readIndex(s, sb)
	if err != nil {
		return err
	}
	used := make([]extent, 0, len(ix.ents)+1)
	for _, e := range ix.ents {
		used = append(used, e.extent())
	}
	used = append(used, extent{off: sb.indexOff, size: alignUp(sb.indexSize)})
	sp, ok := newSpace(used)
	if !ok {
		return errCorrupt(hf.path, "records overlap")
	}
	hf.ix, hf.sp, hf.super, hf.hasSuper, hf.round = ix, sp, sb, true, sb.round
	s.noteRound(sb.round)
	return nil
}

// newestSuper returns the valid superblock with the higher checkpoint count.
func newestSuper(head []byte) (superblock, bool) {
	var a, b superblock
	var okA, okB bool
	if len(head) >= superAOff+superLen {
		a, okA = decodeSuper(head[superAOff:])
	}
	if len(head) >= superBOff+superLen {
		b, okB = decodeSuper(head[superBOff:])
	}
	switch {
	case okA && (!okB || a.seq > b.seq):
		return a, true
	case okB:
		return b, true
	}
	return superblock{}, false
}

func (hf *hxFile) readIndex(s *Store, sb superblock) (index, error) {
	buf := make([]byte, sb.indexSize)
	if _, err := hf.f.ReadAt(buf, int64(sb.indexOff)); err != nil { //nolint:gosec // checked against the file size
		return index{}, fmt.Errorf("store: read %s index: %w", hf.path, err)
	}
	h, body, err := checkRecord(buf, nil, hf.path)
	if err != nil {
		return index{}, err
	}
	if h.kind != kindIndex {
		return index{}, errCorrupt(hf.path, "index record kind")
	}
	raw, err := s.codec.decode(nil, body, h.comp, h.raw)
	if err != nil {
		return index{}, fmt.Errorf("store: %s index: %w", hf.path, err)
	}
	return decodeIndex(raw, hf.path)
}

// checkRecord checks a whole record read into buf against its key and
// checksums, and returns its header and stored body.
func checkRecord(buf, key []byte, path string) (recHeader, []byte, error) {
	h, ok := parseHeader(buf)
	if !ok {
		return recHeader{}, nil, errCorrupt(path, "record header")
	}
	if h.size() != uint64(len(buf)) || string(buf[recHeaderLen:recHeaderLen+h.keyLen]) != string(key) {
		return recHeader{}, nil, errCorrupt(path, "record does not match its index entry")
	}
	body := buf[recHeaderLen+h.keyLen:]
	if crc32.Checksum(body, castagnoli()) != h.bodyCRC {
		return recHeader{}, nil, errCorrupt(path, "record body checksum")
	}
	return h, body, nil
}

// create makes the file on disk for its first write, or rewrites the header of
// one left without it.
func (hf *hxFile) create(s *Store) error {
	if hf.f != nil && !hf.newHeader {
		return nil
	}
	if hf.f == nil {
		dir := filepath.Dir(hf.full)
		if err := s.fs.MkdirAll(dir); err != nil {
			return fmt.Errorf("store: create %s: %w", hf.path, err)
		}
		f, err := s.fs.OpenFile(hf.full, os.O_RDWR|os.O_CREATE)
		if err != nil {
			return fmt.Errorf("store: create %s: %w", hf.path, err)
		}
		hf.mu.Lock()
		hf.f = f
		hf.mu.Unlock()
		s.w.dirs = append(s.w.dirs, dir)
	}
	var h [fileHeaderLen]byte
	encodeFileHeader(h[:])
	if _, err := hf.f.WriteAt(h[:], 0); err != nil {
		return fmt.Errorf("store: create %s: %w", hf.path, err)
	}
	hf.newHeader = false
	return nil
}

// checkpoint writes the index as a record, then the superblock slot not in use,
// each forced to disk, so the journal no longer needs this file's rounds.
func (hf *hxFile) checkpoint(s *Store) error {
	if !hf.dirty {
		return nil
	}
	if hf.f == nil {
		hf.dirty = false
		return nil
	}
	if err := hf.create(s); err != nil {
		return err
	}
	w := &s.w
	w.ibuf = hf.ix.encode(w.ibuf[:0])
	var e entry
	w.irec, e = s.appendRecord(w.irec[:0], kindIndex, nil, w.ibuf, hf.round)
	off := hf.sp.alloc(uint64(len(w.irec)))
	if err := hf.writeSynced(s, w.irec, off); err != nil {
		hf.sp.release(off, uint64(len(w.irec)))
		return err
	}
	sb := superblock{seq: hf.super.seq + 1, round: hf.round, indexOff: off, indexSize: e.size, end: hf.sp.end}
	var buf [superLen]byte
	sb.encode(buf[:])
	if err := hf.writeSynced(s, buf[:], uint64(sb.slot())); err != nil { //nolint:gosec // a slot offset
		return err
	}
	if hf.hasSuper {
		hf.sp.release(hf.super.indexOff, alignUp(hf.super.indexSize))
	}
	hf.super, hf.hasSuper, hf.dirty = sb, true, false
	return hf.trim()
}

func (hf *hxFile) writeSynced(s *Store, b []byte, off uint64) error {
	if _, err := hf.f.WriteAt(b, int64(off)); err != nil { //nolint:gosec // file offsets
		return fmt.Errorf("store: write %s: %w", hf.path, err)
	}
	return s.syncFile(hf)
}

// trim cuts free space off the end of the file.
func (hf *hxFile) trim() error {
	size, err := hf.f.Size()
	if err != nil {
		return fmt.Errorf("store: stat %s: %w", hf.path, err)
	}
	if end := int64(hf.sp.end); size > end { //nolint:gosec // file offsets
		if err := hf.f.Truncate(end); err != nil {
			return fmt.Errorf("store: trim %s: %w", hf.path, err)
		}
	}
	return nil
}

// tryEvict drops the index and closes the file, unless a reader uses it. Only
// the commit goroutine calls it, for clean files with nothing in limbo.
func (hf *hxFile) tryEvict(s *Store) bool {
	hf.loadMu.Lock()
	defer hf.loadMu.Unlock()
	if !hf.loaded.Load() {
		return true
	}
	hf.loaded.Store(false)
	if hf.readers.Load() != 0 {
		hf.loaded.Store(true)
		return false
	}
	hf.mu.Lock()
	if hf.f != nil {
		_ = hf.f.Close()
	}
	hf.reset()
	hf.mu.Unlock()
	s.indexBytes.Add(-hf.mem)
	hf.mem = 0
	return true
}

// reset forgets everything loaded.
func (hf *hxFile) reset() {
	hf.f, hf.ix, hf.sp = nil, index{}, space{}
	hf.super, hf.hasSuper, hf.newHeader, hf.dirty, hf.round = superblock{}, false, false, false, 0
}

// updateMem recounts the index's memory after a change.
func (hf *hxFile) updateMem(s *Store) {
	m := hf.ix.memBytes()
	s.indexBytes.Add(m - hf.mem)
	hf.mem = m
}
