package lsm

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Level sizes, as LevelDB: L1 holds 10 MiB, each deeper level ten times more.
const (
	level1Bytes = 10 << 20
	levelFactor = 10
	// maxPushLevel is the deepest level a fresh table goes to when nothing overlaps it.
	maxPushLevel = 2
)

func maxBytesFor(level int) float64 {
	b := float64(level1Bytes)
	for l := 1; l < level; l++ {
		b *= levelFactor
	}
	return b
}

// background writes immutable memtables to tables and compacts, until the database
// closes.
func (d *DB) background() {
	defer d.bg.Done()
	for {
		job, ok := d.nextJob()
		if !ok {
			return
		}
		if err := job(); err != nil {
			d.mu.Lock()
			if d.bgErr == nil {
				d.bgErr = err
			}
			d.cond.Broadcast()
			d.mu.Unlock()
			return
		}
	}
}

// nextJob waits for background work: an immutable memtable first, then a compaction.
func (d *DB) nextJob() (func() error, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for {
		if d.closed || d.bgErr != nil {
			return nil, false
		}
		if d.imm != nil {
			return d.flushImm, true
		}
		if c := d.pickCompaction(); c != nil {
			return func() error { return d.compact(c) }, true
		}
		d.cond.Wait()
	}
}

// flushImm writes the immutable memtable to a table.
func (d *DB) flushImm() error {
	d.mu.Lock()
	imm, logNum := d.imm, d.logNum
	d.mu.Unlock()
	num := d.newFileNum()
	defer d.unpend([]uint64{num})
	meta, err := d.writeTable(num, imm.iter(), nil)
	if err != nil {
		return err
	}
	d.mu.Lock()
	level := d.pickLevel(d.current, meta)
	d.mu.Unlock()
	edit := versionEdit{}
	edit.addFile(level, meta)
	edit.setLogNum(logNum)
	if err := d.logAndApply(&edit); err != nil {
		return err
	}
	d.mu.Lock()
	d.imm = nil
	d.cond.Broadcast()
	d.mu.Unlock()
	d.removeObsolete()
	return nil
}

// writeTable writes every entry of it to table num. keep, if set, filters entries.
func (d *DB) writeTable(num uint64, it internalIter, keep func(ikey []byte) bool) (fileMeta, error) {
	path := filepath.Join(d.dir, tableName(num))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fileMeta{}, fmt.Errorf("lsm: create table: %w", err)
	}
	tw := newTableWriter(f, d.opts.tableOpts())
	meta, err := d.fillTable(tw, it, keep)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return fileMeta{}, err
	}
	meta.num = num
	return meta, nil
}

func (d *DB) fillTable(tw *tableWriter, it internalIter, keep func(ikey []byte) bool) (fileMeta, error) {
	for ok := it.First(); ok; ok = it.Next() {
		if keep != nil && !keep(it.Key()) {
			continue
		}
		if err := tw.add(it.Key(), it.Value()); err != nil {
			return fileMeta{}, err
		}
	}
	if err := it.Err(); err != nil {
		return fileMeta{}, err
	}
	n, err := tw.finish()
	if err != nil {
		return fileMeta{}, err
	}
	return fileMeta{size: n, smallest: bytes.Clone(tw.smallest), largest: bytes.Clone(tw.last)}, nil
}

// pickLevel is the level a fresh table goes to: L0, or deeper (up to maxPushLevel) while
// it overlaps nothing there, so tables that never overlap skip L0's compactions.
// d.mu is held.
func (d *DB) pickLevel(v *version, m fileMeta) int {
	lo, hi := userKey(m.smallest), userKey(m.largest)
	if len(v.overlapping(0, lo, hi)) > 0 {
		return 0
	}
	level := 0
	for level < maxPushLevel && len(v.overlapping(level+1, lo, hi)) == 0 {
		level++
	}
	return level
}

// logAndApply records an edit in the manifest and makes the resulting version current.
func (d *DB) logAndApply(e *versionEdit) error {
	d.manifestMu.Lock()
	defer d.manifestMu.Unlock()
	d.mu.Lock()
	e.setNextFile(d.next)
	if !e.hasLastSeq || e.lastSeq < d.seq {
		e.setLastSeq(d.seq)
	}
	nv := d.current.apply(e)
	d.mu.Unlock()
	if err := d.mlog.add(e.encode(nil)); err != nil {
		return fmt.Errorf("lsm: write manifest: %w", err)
	}
	if err := d.manifest.Sync(); err != nil {
		return fmt.Errorf("lsm: sync manifest: %w", err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.current.refs == 0 {
		delete(d.live, d.current)
	}
	d.current = nv
	for _, p := range e.compactPtrs {
		d.ptrs[p.level] = p.key
	}
	d.cond.Broadcast()
	return nil
}

// removeObsolete deletes logs, manifests and tables nothing uses any more. A table still
// open by a reader on Windows cannot be deleted; it is tried again next time.
func (d *DB) removeObsolete() {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	d.mu.Lock()
	keep := make(map[uint64]bool)
	d.current.files(keep)
	for v := range d.live {
		v.files(keep)
	}
	for num := range d.pending {
		keep[num] = true
	}
	logNum, immLog, next := d.logNum, d.immLog, d.next
	hasImm := d.imm != nil
	d.mu.Unlock()
	manifest := ""
	if d.manifest != nil {
		manifest = filepath.Base(d.manifest.Name())
	}
	for _, e := range entries {
		t, num := parseFileName(e.Name())
		if num >= next {
			continue
		}
		drop := false
		switch t {
		case fileLog:
			drop = num < logNum && (!hasImm || num != immLog)
		case fileTable:
			drop = !keep[num]
			if drop {
				d.tables.evict(num)
			}
		case fileManifest:
			drop = e.Name() != manifest
		case fileTemp:
			drop = true
		}
		if drop {
			_ = os.Remove(filepath.Join(d.dir, e.Name()))
		}
	}
}

// compaction is one compaction: the inputs from level and level+1.
type compaction struct {
	level  int
	inputs [2][]*fileMeta
	v      *version
}

// pickCompaction chooses the level most over its size (or L0 over its table count) and
// the tables to compact. d.mu is held. It returns nil if nothing needs compacting.
func (d *DB) pickCompaction() *compaction {
	v := d.current
	best, bestScore := -1, 1.0
	if s := float64(len(v.levels[0])) / l0Compact; s >= bestScore {
		best, bestScore = 0, s
	}
	for level := 1; level < numLevels-1; level++ {
		if s := float64(v.levelBytes(level)) / maxBytesFor(level); s >= bestScore {
			best, bestScore = level, s
		}
	}
	if best < 0 {
		return nil
	}
	c := &compaction{level: best, v: v}
	if best == 0 {
		c.inputs[0] = append(c.inputs[0], v.levels[0]...)
	} else {
		c.inputs[0] = []*fileMeta{d.nextFileAfterPointer(v, best)}
	}
	lo, hi := keyRange(c.inputs[0])
	c.inputs[1] = v.overlapping(best+1, lo, hi)
	v.refs++
	d.live[v] = true
	return c
}

// nextFileAfterPointer is the first table of level past its compaction pointer, wrapping.
func (d *DB) nextFileAfterPointer(v *version, level int) *fileMeta {
	files := v.levels[level]
	for _, f := range files {
		if d.ptrs[level] == nil || compareInternal(f.largest, d.ptrs[level]) > 0 {
			return f
		}
	}
	return files[0]
}

// keyRange is the user key range of tables.
func keyRange(files []*fileMeta) (lo, hi []byte) {
	for _, f := range files {
		if lo == nil || bytes.Compare(userKey(f.smallest), lo) < 0 {
			lo = userKey(f.smallest)
		}
		if hi == nil || bytes.Compare(userKey(f.largest), hi) > 0 {
			hi = userKey(f.largest)
		}
	}
	return lo, hi
}

// compact runs a compaction.
func (d *DB) compact(c *compaction) error {
	defer d.release(readState{v: c.v})
	edit := versionEdit{}
	for i, files := range c.inputs {
		for _, f := range files {
			edit.deleteFile(c.level+i, f.num)
		}
	}
	edit.compactPtrs = append(edit.compactPtrs, levelKey{level: c.level, key: bytes.Clone(c.inputs[0][len(c.inputs[0])-1].largest)})
	if c.level > 0 && len(c.inputs[0]) == 1 && len(c.inputs[1]) == 0 {
		// Nothing to merge with: move the table down as it is.
		edit.addFile(c.level+1, *c.inputs[0][0])
		return d.logAndApply(&edit)
	}
	outputs, err := d.mergeInputs(c)
	defer d.unpend(fileNums(outputs))
	if err != nil {
		return err
	}
	for _, m := range outputs {
		edit.addFile(c.level+1, m)
	}
	if err := d.logAndApply(&edit); err != nil {
		return err
	}
	d.removeObsolete()
	return nil
}

func fileNums(files []fileMeta) []uint64 {
	nums := make([]uint64, len(files))
	for i, f := range files {
		nums[i] = f.num
	}
	return nums
}

// mergeInputs merges a compaction's tables into new tables at level+1, dropping entries
// no reader can see any more.
func (d *DB) mergeInputs(c *compaction) ([]fileMeta, error) {
	it, release, err := d.compactionIter(c)
	if err != nil {
		return nil, err
	}
	defer release()
	d.mu.Lock()
	oldest := d.oldestSnapshot()
	d.mu.Unlock()
	m := merger{d: d, c: c, oldest: oldest, lastSeq: maxSeq}
	for ok := it.First(); ok; ok = it.Next() {
		if err := m.add(it); err != nil {
			return m.outputs, err
		}
	}
	if err := it.Err(); err != nil {
		return m.outputs, err
	}
	return m.outputs, m.finishOutput()
}

// compactionIter merges a compaction's input tables.
func (d *DB) compactionIter(c *compaction) (*mergeIter, func(), error) {
	var iters []internalIter
	var held []*openTableEntry
	release := func() {
		for _, ent := range held {
			d.tables.release(ent)
		}
	}
	for _, f := range c.inputs[0] {
		ent, err := d.tables.acquire(f.num, f.size)
		if err != nil {
			release()
			return nil, nil, err
		}
		held = append(held, ent)
		iters = append(iters, newTableIter(ent.t))
	}
	if len(c.inputs[1]) > 0 {
		iters = append(iters, newLevelIter(c.inputs[1], d.tables))
	}
	m := newMergeIter(iters)
	return m, func() { _ = m.Close(); release() }, nil
}

// merger writes a compaction's output tables.
type merger struct {
	d       *DB
	c       *compaction
	oldest  uint64
	outputs []fileMeta

	cur     []byte // user key of the entries being read
	hasCur  bool
	lastSeq uint64 // sequence number of the last entry kept or seen of cur

	f    *os.File
	tw   *tableWriter
	num  uint64
	path string
	full bool // the output reached its size and is cut at the next user key
}

// add handles one input entry.
func (m *merger) add(it *mergeIter) error {
	ikey := it.Key()
	ukey, seq, k, ok := splitInternalKey(ikey)
	if !ok {
		return errCorrupt("internal key in compaction")
	}
	newKey := !m.hasCur || !bytes.Equal(ukey, m.cur)
	if newKey {
		m.cur, m.hasCur, m.lastSeq = append(m.cur[:0], ukey...), true, maxSeq
	}
	drop := m.lastSeq <= m.oldest || (k == kindDelete && seq <= m.oldest && m.isBaseLevel(ukey))
	m.lastSeq = seq
	if drop {
		return nil
	}
	if newKey && m.full {
		if err := m.finishOutput(); err != nil {
			return err
		}
	}
	if err := m.ensureOutput(); err != nil {
		return err
	}
	if t, h, big := it.bigSource(); big {
		return m.tw.addRawBlock(ikey, t, h)
	}
	if err := m.tw.add(ikey, it.Value()); err != nil {
		return err
	}
	m.full = m.tw.offset >= uint64(m.d.opts.TableSize) //nolint:gosec // positive
	return nil
}

// isBaseLevel reports whether no level below the output level may hold ukey, so a
// deletion of it can be dropped.
func (m *merger) isBaseLevel(ukey []byte) bool {
	for level := m.c.level + 2; level < numLevels; level++ {
		if len(m.c.v.overlapping(level, ukey, ukey)) > 0 {
			return false
		}
	}
	return true
}

func (m *merger) ensureOutput() error {
	if m.tw != nil {
		return nil
	}
	m.num = m.d.newFileNum()
	m.path = filepath.Join(m.d.dir, tableName(m.num))
	f, err := os.OpenFile(m.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		m.d.unpend([]uint64{m.num})
		return fmt.Errorf("lsm: create table: %w", err)
	}
	m.f, m.tw, m.full = f, newTableWriter(&pacedWriter{w: f, pace: m.d.opts.Pace}, m.d.opts.tableOpts()), false
	return nil
}

// finishOutput completes the current output table.
func (m *merger) finishOutput() error {
	if m.tw == nil {
		return nil
	}
	n, err := m.tw.finish()
	if err == nil {
		err = m.f.Sync()
	}
	if cerr := m.f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	meta := fileMeta{num: m.num, size: n, smallest: bytes.Clone(m.tw.smallest), largest: bytes.Clone(m.tw.last)}
	m.tw, m.f = nil, nil
	if err != nil {
		_ = os.Remove(m.path)
		m.d.unpend([]uint64{m.num})
		return err
	}
	m.outputs = append(m.outputs, meta)
	return nil
}

// pacedWriter calls pace after each write, so the caller can slow background work.
type pacedWriter struct {
	w    io.Writer
	pace func(n int)
}

func (p *pacedWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	if p.pace != nil {
		p.pace(n)
	}
	return n, err
}

// addRawBlock copies a big block of another table as stored: no decompression, no
// recompression, the same checksum.
func (t *tableWriter) addRawBlock(ikey []byte, src *table, h blockHandle) error {
	if t.err != nil {
		return t.err
	}
	t.flushBlock()
	t.addIndexEntry(ikey)
	t.filter.addKey(userKey(ikey))
	t.noteKey(ikey)
	sec := io.NewSectionReader(src.f, int64(h.offset), int64(h.size+blockTrailerLen)) //nolint:gosec // in range
	n, err := io.Copy(t.w, sec)
	if err != nil {
		t.err = fmt.Errorf("lsm: copy big block: %w", err)
		return t.err
	}
	if uint64(n) != h.size+blockTrailerLen { //nolint:gosec // count
		t.err = errCorrupt("big block cut short")
		return t.err
	}
	nh := blockHandle{offset: t.offset, size: h.size}
	t.offset += h.size + blockTrailerLen
	t.finishBlock(nh)
	return nil
}
