package lsm

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Options configure a database. Zero values take the defaults.
type Options struct {
	// WriteBuffer is how big the memtable grows before it is written to a table.
	// Default 4 MiB, as Bedrock.
	WriteBuffer int
	// BlockSize is the uncompressed size data blocks are cut at. Default 16 KiB.
	BlockSize int
	// BigValue is the value size from which a value is written in a table of its own,
	// streamed and never cached. Default 1 MiB.
	BigValue int
	// BlockCache is the byte budget of the small-block cache. Default 32 MiB; negative
	// turns the cache off.
	BlockCache int
	// MaxOpenFiles is how many tables stay open. Default 500.
	MaxOpenFiles int
	// TableSize is the size compaction cuts its output tables at. Default 2 MiB.
	TableSize int
	// Sync makes every write wait until its log record is on disk.
	Sync bool
	// Pace, if set, is called by flushes and compactions after writing each block, with
	// its stored size; it may sleep to slow background work down.
	Pace func(n int)
	// ReadOnly opens an existing database without writing anything to it, so a
	// world BDS owns is never changed: logs are replayed into memory, no
	// manifest or log is started, and writes return ErrReadOnly.
	ReadOnly bool
}

func (o Options) withDefaults() Options {
	if o.WriteBuffer <= 0 {
		o.WriteBuffer = 4 << 20
	}
	if o.BlockSize <= 0 {
		o.BlockSize = 16 << 10
	}
	if o.BigValue <= 0 {
		o.BigValue = 1 << 20
	}
	if o.BlockCache == 0 {
		o.BlockCache = 32 << 20
	}
	if o.MaxOpenFiles <= 0 {
		o.MaxOpenFiles = 500
	}
	if o.TableSize <= 0 {
		o.TableSize = 2 << 20
	}
	return o
}

// tableOpts are how this database writes tables.
func (o Options) tableOpts() tableOptions {
	return tableOptions{blockSize: o.BlockSize, bigValue: o.BigValue}
}

// L0 limits, as LevelDB: compaction starts at l0Compact tables, writes wait at l0Stop.
const (
	l0Compact = 4
	l0Stop    = 12
)

// DB is an open database. It is safe for concurrent use: any number of readers and one
// writer at a time (Write serialises writers itself). Reads never wait for writes,
// flushes or compactions.
type DB struct {
	dir    string
	opts   Options
	tables *tableCache
	blocks *blockCache

	writeMu    sync.Mutex // one Write at a time
	manifestMu sync.Mutex // one version change at a time

	mu   sync.Mutex // guards the fields below
	cond *sync.Cond
	mem  *memtable
	imm  *memtable
	// immLog is the log holding imm's entries, deleted once imm is in a table.
	immLog  uint64
	current *version
	live    map[*version]bool
	seq     uint64
	next    uint64
	// pending are file numbers being written, not yet in a version.
	pending   map[uint64]bool
	snapshots map[uint64]int
	ptrs      [numLevels][]byte
	bgErr     error
	closed    bool

	logNum   uint64
	logFile  *os.File
	log      *logWriter
	manifest *os.File
	mlog     *logWriter

	bg sync.WaitGroup
}

// Open opens or creates the database in dir.
func Open(dir string, opts Options) (*DB, error) {
	opts = opts.withDefaults()
	if opts.ReadOnly {
		if _, err := os.Stat(filepath.Join(dir, currentName)); err != nil {
			return nil, fmt.Errorf("lsm: open %s read-only: %w", dir, err)
		}
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("lsm: create %s: %w", dir, err)
	}
	cacheBudget := max(opts.BlockCache, 0)
	d := &DB{
		dir:       dir,
		opts:      opts,
		blocks:    newBlockCache(cacheBudget),
		live:      make(map[*version]bool),
		pending:   make(map[uint64]bool),
		snapshots: make(map[uint64]int),
		current:   &version{},
		next:      2,
	}
	d.cond = sync.NewCond(&d.mu)
	d.tables = newTableCache(dir, opts.MaxOpenFiles, d.blocks, uint64(opts.BigValue)) //nolint:gosec // positive
	if err := d.recover(); err != nil {
		d.tables.close()
		return nil, err
	}
	if !opts.ReadOnly {
		d.bg.Add(1)
		go d.background()
	}
	return d, nil
}

// recover reads the manifest and logs, writes what the logs held to a table, and starts
// a new log and manifest.
func (d *DB) recover() error {
	edit := versionEdit{}
	current, err := os.ReadFile(filepath.Join(d.dir, currentName))
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return fmt.Errorf("lsm: read CURRENT: %w", err)
	default:
		if err := d.readManifest(strings.TrimSpace(string(current))); err != nil {
			return err
		}
	}
	if err := d.replayLogs(&edit); err != nil {
		return err
	}
	if d.opts.ReadOnly {
		return nil
	}
	return d.startFresh(&edit)
}

// readManifest builds the current version from a manifest.
func (d *DB) readManifest(name string) error {
	f, err := os.Open(filepath.Join(d.dir, name))
	if err != nil {
		return fmt.Errorf("lsm: open %s: %w", name, err)
	}
	defer f.Close()
	r := newLogReader(f)
	v := &version{}
	for {
		rec, err := r.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("lsm: read %s: %w", name, err)
		}
		var e versionEdit
		if err := e.decode(rec); err != nil {
			return fmt.Errorf("lsm: read %s: %w", name, err)
		}
		if e.comparator != "" && e.comparator != comparatorName {
			return fmt.Errorf("lsm: %s uses comparator %q", name, e.comparator)
		}
		d.noteEdit(&e)
		v = v.apply(&e)
	}
	d.current = v
	return nil
}

// noteEdit takes the counters and compaction pointers of a manifest record.
func (d *DB) noteEdit(e *versionEdit) {
	if e.hasLogNum {
		d.logNum = e.logNum
	}
	if e.hasNextFile {
		d.next = max(d.next, e.nextFile)
	}
	if e.hasLastSeq {
		d.seq = max(d.seq, e.lastSeq)
	}
	for _, p := range e.compactPtrs {
		d.ptrs[p.level] = p.key
	}
	for _, a := range e.added {
		d.next = max(d.next, a.meta.num+1)
	}
}

// replayLogs reads every log at or after the manifest's log number, in order, writing
// their entries to tables recorded in edit.
func (d *DB) replayLogs(edit *versionEdit) error {
	nums, err := d.filesOf(fileLog)
	if err != nil {
		return err
	}
	mem := newMemtable(d.opts.WriteBuffer)
	if d.opts.ReadOnly {
		// Nothing is written: what the logs hold stays in memory.
		d.mem = mem
	}
	for _, num := range nums {
		d.next = max(d.next, num+1)
		if num < d.logNum {
			continue
		}
		if err := d.replayLog(num, &mem, edit); err != nil {
			return err
		}
	}
	if !mem.empty() && !d.opts.ReadOnly {
		return d.writeLevel0(mem, edit)
	}
	return nil
}

// replayLog reads one log into mem, writing mem out whenever it fills.
func (d *DB) replayLog(num uint64, mem **memtable, edit *versionEdit) error {
	f, err := os.Open(filepath.Join(d.dir, logName(num)))
	if err != nil {
		return fmt.Errorf("lsm: open log %d: %w", num, err)
	}
	defer f.Close()
	r := newLogReader(f)
	for {
		rec, err := r.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("lsm: read log %d: %w", num, err)
		}
		if err := d.replayRecord(rec, *mem); err != nil {
			return fmt.Errorf("lsm: read log %d: %w", num, err)
		}
		if (*mem).size() >= d.opts.WriteBuffer && !d.opts.ReadOnly {
			if err := d.writeLevel0(*mem, edit); err != nil {
				return err
			}
			*mem = newMemtable(d.opts.WriteBuffer)
		}
	}
}

// replayRecord applies one log record to mem.
func (d *DB) replayRecord(rec []byte, mem *memtable) error {
	seq, count, entries, err := decodeLogRecord(rec)
	if err != nil {
		return err
	}
	i := uint64(0)
	err = eachEntry(entries, func(k kind, key, value []byte) error {
		mem.add(seq+i, k, key, value)
		i++
		return nil
	})
	if err != nil {
		return err
	}
	if int(i) != count { //nolint:gosec // small
		return errCorrupt("batch count")
	}
	if count > 0 {
		d.seq = max(d.seq, seq+uint64(count)-1) //nolint:gosec // positive
	}
	return nil
}

// writeLevel0 writes a memtable to a new table during recovery and records it in edit.
func (d *DB) writeLevel0(mem *memtable, edit *versionEdit) error {
	num := d.next
	d.next++
	meta, err := d.writeTable(num, mem.iter(), nil)
	if err != nil {
		return err
	}
	edit.addFile(0, meta)
	d.current = d.current.apply(&versionEdit{added: []levelMeta{{level: 0, meta: meta}}})
	return nil
}

// startFresh starts a new log and a new manifest holding the whole current state, points
// CURRENT at it and removes files no longer needed.
func (d *DB) startFresh(_ *versionEdit) error {
	if err := d.newLog(); err != nil {
		return err
	}
	num := d.next
	d.next++
	snap := versionEdit{comparator: comparatorName}
	snap.setLogNum(d.logNum)
	snap.setNextFile(d.next)
	snap.setLastSeq(d.seq)
	for level, p := range d.ptrs {
		if p != nil {
			snap.compactPtrs = append(snap.compactPtrs, levelKey{level: level, key: p})
		}
	}
	for level := range numLevels {
		for _, f := range d.current.levels[level] {
			snap.addFile(level, *f)
		}
	}
	if err := d.writeManifest(num, &snap); err != nil {
		return err
	}
	d.mem = newMemtable(d.opts.WriteBuffer)
	d.removeObsolete()
	return nil
}

// newLog opens a new log and makes it the current one.
func (d *DB) newLog() error {
	num := d.next
	d.next++
	f, err := os.OpenFile(filepath.Join(d.dir, logName(num)), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("lsm: create log: %w", err)
	}
	if d.logFile != nil {
		_ = d.logFile.Close()
	}
	d.logFile, d.log, d.logNum = f, newLogWriter(f, 0), num
	return nil
}

// writeManifest writes a new manifest holding snap and points CURRENT at it.
func (d *DB) writeManifest(num uint64, snap *versionEdit) error {
	name := manifestName(num)
	f, err := os.OpenFile(filepath.Join(d.dir, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("lsm: create manifest: %w", err)
	}
	w := newLogWriter(f, 0)
	if err := w.add(snap.encode(nil)); err != nil {
		_ = f.Close()
		return fmt.Errorf("lsm: write manifest: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("lsm: sync manifest: %w", err)
	}
	if err := d.setCurrent(name); err != nil {
		_ = f.Close()
		return err
	}
	if d.manifest != nil {
		_ = d.manifest.Close()
	}
	d.manifest, d.mlog = f, w
	return nil
}

// setCurrent points CURRENT at a manifest, through a temporary file renamed over it.
func (d *DB) setCurrent(manifest string) error {
	tmp := filepath.Join(d.dir, fmt.Sprintf("%06d.dbtmp", d.next))
	if err := os.WriteFile(tmp, []byte(manifest+"\n"), 0o644); err != nil {
		return fmt.Errorf("lsm: write CURRENT: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(d.dir, currentName)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("lsm: write CURRENT: %w", err)
	}
	return nil
}

// filesOf lists the numbers of the database's files of type t, in order.
func (d *DB) filesOf(t fileType) ([]uint64, error) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return nil, fmt.Errorf("lsm: list %s: %w", d.dir, err)
	}
	var nums []uint64
	for _, e := range entries {
		if ft, num := parseFileName(e.Name()); ft == t {
			nums = append(nums, num)
		}
	}
	slices.Sort(nums)
	return nums, nil
}

// readState is what one read sees: the memtables, the tables and the sequence number of
// one moment.
type readState struct {
	mem, imm *memtable
	v        *version
	seq      uint64
}

// now asks acquire for the latest sequence number.
const now = -1

// acquire takes the read state at sequence number at (now for the latest); release must
// be called with it.
func (d *DB) acquire(at int64) (readState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return readState{}, ErrClosed
	}
	seq := d.seq
	if at >= 0 {
		seq = uint64(at)
	}
	d.current.refs++
	d.live[d.current] = true
	return readState{mem: d.mem, imm: d.imm, v: d.current, seq: seq}, nil
}

func (d *DB) release(st readState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st.v.refs--
	if st.v.refs == 0 && st.v != d.current {
		delete(d.live, st.v)
	}
}

// Get returns a copy of key's value, or ErrNotFound.
func (d *DB) Get(key []byte) ([]byte, error) { return d.get(key, now) }

func (d *DB) get(key []byte, at int64) ([]byte, error) {
	st, err := d.acquire(at)
	if err != nil {
		return nil, err
	}
	defer d.release(st)
	ikey := appendInternalKey(nil, key, st.seq, kindSeek)
	for _, m := range []*memtable{st.mem, st.imm} {
		if m == nil {
			continue
		}
		if k, v, found := m.get(ikey); found {
			return foundValue(k, v)
		}
	}
	var buf [numLevels + 8]*fileMeta
	for _, f := range st.v.filesFor(key, buf[:0]) {
		k, v, found, err := d.tableGet(f, ikey)
		if err != nil {
			return nil, err
		}
		if found {
			return foundValue(k, v)
		}
	}
	return nil, ErrNotFound
}

// foundValue turns a found entry into a result: a copy of the value, or ErrNotFound
// for a deletion.
func foundValue(ikey, value []byte) ([]byte, error) {
	_, _, k, ok := splitInternalKey(ikey)
	if !ok {
		return nil, errCorrupt("internal key")
	}
	if k == kindDelete {
		return nil, ErrNotFound
	}
	return bytes.Clone(value), nil
}

func (d *DB) tableGet(f *fileMeta, ikey []byte) ([]byte, []byte, bool, error) {
	ent, err := d.tables.acquire(f.num, f.size)
	if err != nil {
		return nil, nil, false, err
	}
	defer d.tables.release(ent)
	k, v, found, err := ent.t.get(ikey)
	if found {
		k = bytes.Clone(k) // the value is cloned by foundValue
	}
	return k, v, found, err
}

// GetStream returns key's value as a stream and its size, or ErrNotFound. A big value is
// read from disk as the stream is read, never held whole. The stream must be closed.
func (d *DB) GetStream(key []byte) (io.ReadCloser, int64, error) { return d.getStream(key, now) }

func (d *DB) getStream(key []byte, at int64) (io.ReadCloser, int64, error) {
	st, err := d.acquire(at)
	if err != nil {
		return nil, 0, err
	}
	ikey := appendInternalKey(nil, key, st.seq, kindSeek)
	for _, m := range []*memtable{st.mem, st.imm} {
		if m == nil {
			continue
		}
		if k, v, found := m.get(ikey); found {
			d.release(st)
			return smallStream(k, v)
		}
	}
	var buf [numLevels + 8]*fileMeta
	for _, f := range st.v.filesFor(key, buf[:0]) {
		rc, size, found, err := d.tableStream(f, ikey, st)
		if err != nil || found {
			return rc, size, err
		}
	}
	d.release(st)
	return nil, 0, ErrNotFound
}

func smallStream(ikey, value []byte) (io.ReadCloser, int64, error) {
	v, err := foundValue(ikey, value)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(v)), int64(len(v)), nil
}

// tableStream looks ikey up in one table for GetStream. When found, it hands st over to
// the returned stream, which releases it on Close; on an error it releases st itself.
func (d *DB) tableStream(f *fileMeta, ikey []byte, st readState) (io.ReadCloser, int64, bool, error) {
	ent, err := d.tables.acquire(f.num, f.size)
	if err != nil {
		d.release(st)
		return nil, 0, false, err
	}
	k, small, s, found, err := ent.t.getStream(ikey)
	if err != nil || !found {
		d.tables.release(ent)
		if err != nil {
			d.release(st)
		}
		return nil, 0, false, err
	}
	if s == nil {
		rc, size, err := smallStream(k, small)
		d.tables.release(ent)
		d.release(st)
		return rc, size, true, err
	}
	if _, _, kd, _ := splitInternalKey(k); kd == kindDelete {
		_ = s.Close()
		d.tables.release(ent)
		d.release(st)
		return nil, 0, true, ErrNotFound
	}
	return &heldStream{valueStream: s, done: func() { d.tables.release(ent); d.release(st) }}, s.Size(), true, nil
}

// heldStream keeps its table and read state until closed.
type heldStream struct {
	*valueStream
	once sync.Once
	done func()
}

func (h *heldStream) Close() error {
	err := h.valueStream.Close()
	h.once.Do(h.done)
	return err
}

// Has reports whether key is set.
func (d *DB) Has(key []byte) (bool, error) {
	_, err := d.Get(key)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotFound):
		return false, nil
	}
	return false, err
}

// NewIterator returns an iterator over the keys starting with prefix (all keys if empty),
// as of now. It must be closed.
func (d *DB) NewIterator(prefix []byte) (*Iterator, error) { return d.newIterator(prefix, now) }

func (d *DB) newIterator(prefix []byte, at int64) (*Iterator, error) {
	st, err := d.acquire(at)
	if err != nil {
		return nil, err
	}
	d.addSnapshot(st.seq)
	iters := []internalIter{st.mem.iter()}
	if st.imm != nil {
		iters = append(iters, st.imm.iter())
	}
	var held []*openTableEntry
	for _, f := range st.v.levels[0] {
		ent, err := d.tables.acquire(f.num, f.size)
		if err != nil {
			d.releaseIter(st, held)
			return nil, err
		}
		held = append(held, ent)
		iters = append(iters, newTableIter(ent.t))
	}
	for level := 1; level < numLevels; level++ {
		if len(st.v.levels[level]) > 0 {
			iters = append(iters, newLevelIter(st.v.levels[level], d.tables))
		}
	}
	it := &Iterator{m: newMergeIter(iters), seq: st.seq, prefix: bytes.Clone(prefix)}
	it.release = func() { d.releaseIter(st, held) }
	return it, nil
}

func (d *DB) releaseIter(st readState, held []*openTableEntry) {
	for _, ent := range held {
		d.tables.release(ent)
	}
	d.removeSnapshot(st.seq)
	d.release(st)
}

// Snapshot is a consistent view of the database at one moment.
type Snapshot struct {
	d    *DB
	seq  uint64
	once sync.Once
}

// Snapshot returns a view of the database as it is now. It must be released.
func (d *DB) Snapshot() (*Snapshot, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, ErrClosed
	}
	d.snapshots[d.seq]++
	return &Snapshot{d: d, seq: d.seq}, nil
}

// Get is DB.Get as of the snapshot.
func (s *Snapshot) Get(key []byte) ([]byte, error) { return s.d.get(key, s.at()) }

// GetStream is DB.GetStream as of the snapshot.
func (s *Snapshot) GetStream(key []byte) (io.ReadCloser, int64, error) {
	return s.d.getStream(key, s.at())
}

// NewIterator is DB.NewIterator as of the snapshot.
func (s *Snapshot) NewIterator(prefix []byte) (*Iterator, error) {
	return s.d.newIterator(prefix, s.at())
}

// at is the snapshot's sequence number.
func (s *Snapshot) at() int64 { return int64(s.seq) } //nolint:gosec // 56 bits

// Release frees the snapshot.
func (s *Snapshot) Release() { s.once.Do(func() { s.d.removeSnapshot(s.seq) }) }

func (d *DB) addSnapshot(seq uint64) {
	d.mu.Lock()
	d.snapshots[seq]++
	d.mu.Unlock()
}

func (d *DB) removeSnapshot(seq uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.snapshots[seq]--; d.snapshots[seq] <= 0 {
		delete(d.snapshots, seq)
	}
}

// oldestSnapshot is the lowest sequence number a reader may still read at.
func (d *DB) oldestSnapshot() uint64 {
	oldest := d.seq
	for seq := range d.snapshots {
		oldest = min(oldest, seq)
	}
	return oldest
}

// Close waits for background work, then closes every file.
func (d *DB) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return ErrClosed
	}
	d.closed = true
	d.cond.Broadcast()
	d.mu.Unlock()
	d.bg.Wait()
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	var errs []error
	if d.logFile != nil {
		errs = append(errs, d.logFile.Sync(), d.logFile.Close())
	}
	if d.manifest != nil {
		errs = append(errs, d.manifest.Close())
	}
	d.tables.close()
	d.mu.Lock()
	defer d.mu.Unlock()
	return errors.Join(append(errs, d.bgErr)...)
}
