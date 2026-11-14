package store

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path/filepath"
)

// maxRun is how many bytes of small records one file's write may gather
// before they are written.
const maxRun = 8 << 20

// writer is the commit goroutine's scratch, kept between rounds so a round in
// the steady state allocates almost nothing.
type writer struct {
	run    []byte
	recs   []pendingRec
	allocs []allocation
	files  []*hxFile // touched by this round, in file order
	dirs   []string  // created by this round
	jbuf   []byte
	chg    []change
	old    []entry
	freed  []freedExtent
	// spareFreed is a reclaimed generation's slice, reused for the next round.
	spareFreed []freedExtent
	// dirty are files with changes not checkpointed.
	dirty []*hxFile
	ibuf  []byte
	irec  []byte
	hdr   [recHeaderLen + MaxKeyLen]byte
}

// pendingRec is one written record (or deletion) waiting for the journal.
type pendingRec struct {
	file *hxFile
	key  []byte
	e    entry
	del  bool
	// inRun is set while the record's offset is relative to the run being built.
	inRun bool
}

type allocation struct {
	file *hxFile
	ext  extent
}

var errStreamSize = errors.New("store: stream body differs from its declared size")

// Commit writes the batch as one save round and returns the round's number.
// When it returns nil, the round is durable (unless NoSync) and visible to
// every read; when it returns an error, nothing of it counts. The batch is
// left sorted; Reset it before reuse.
func (s *Store) Commit(b *Batch) (uint64, error) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if err := s.usable(); err != nil {
		return 0, err
	}
	if err := s.validate(b); err != nil {
		return 0, err
	}
	if b.Len() == 0 {
		return s.round, nil
	}
	round := s.round + 1
	if mr := s.maxRound.Load(); round <= mr {
		round = mr + 1
	}
	b.sort()
	s.w.reset()
	if err := s.writeRound(b, round); err != nil {
		s.rollback()
		return 0, err
	}
	if err := s.journalCommit(round); err != nil {
		// The commit record may or may not be on disk: nothing more is written.
		s.failed = err
		return 0, err
	}
	s.applyRound(round)
	s.round = round
	s.advance(round, s.w.freed)
	s.w.freed = nil
	s.reclaim()
	if err := s.maintain(); err != nil {
		return round, err
	}
	return round, nil
}

func (s *Store) validate(b *Batch) error {
	for i := range b.ops {
		o := &b.ops[i]
		switch {
		case o.keyLen > MaxKeyLen:
			return fmt.Errorf("store: key of %d bytes is longer than %d", o.keyLen, MaxKeyLen)
		case !o.del && o.kind == kindIndex:
			return errors.New("store: record kind 0 is reserved")
		case o.open != nil && o.size < 0:
			return errors.New("store: negative stream size")
		}
		if _, err := s.fileOf(o.file); err != nil {
			return err
		}
	}
	return nil
}

func (w *writer) reset() {
	clear(w.recs)
	clear(w.allocs)
	clear(w.files)
	w.recs, w.allocs, w.files, w.dirs = w.recs[:0], w.allocs[:0], w.files[:0], w.dirs[:0]
	w.freed = w.spareFreed
	w.spareFreed = nil
}

// writeRound writes every record of the batch to free space and forces the
// files to disk.
func (s *Store) writeRound(b *Batch, round uint64) error {
	for start := 0; start < len(b.ops); {
		end := start + 1
		for end < len(b.ops) && b.ops[end].file == b.ops[start].file {
			end++
		}
		hf, err := s.writable(b.ops[start].file)
		if err != nil {
			return err
		}
		if err := s.writeFile(hf, b, b.ops[start:end], round); err != nil {
			return err
		}
		start = end
	}
	return s.syncRound()
}

// writable loads a file for writing and creates it if needed.
func (s *Store) writable(id FileID) (*hxFile, error) {
	hf, err := s.fileOf(id)
	if err != nil {
		return nil, err
	}
	if err := hf.load(s); err != nil {
		return nil, err
	}
	if err := hf.create(s); err != nil {
		return nil, err
	}
	s.w.files = append(s.w.files, hf)
	return hf, nil
}

// writeFile writes one file's records: small ones gathered into runs written
// with one call each, streams on their own.
func (s *Store) writeFile(hf *hxFile, b *Batch, ops []op, round uint64) error {
	w := &s.w
	w.run = w.run[:0]
	runFrom := len(w.recs)
	for i := range ops {
		o := &ops[i]
		key := b.key(o)
		if i+1 < len(ops) && bytes.Equal(key, b.key(&ops[i+1])) {
			continue // a later write to the same key wins
		}
		switch {
		case o.del:
			w.recs = append(w.recs, pendingRec{file: hf, key: key, del: true})
		case o.open != nil:
			e, err := s.writeStream(hf, o, key, round)
			if err != nil {
				return err
			}
			w.recs = append(w.recs, pendingRec{file: hf, key: key, e: e})
		default:
			var e entry
			w.run, e = s.appendRecord(w.run, o.kind, key, b.value(o), round)
			w.recs = append(w.recs, pendingRec{file: hf, key: key, e: e, inRun: true})
			if len(w.run) >= maxRun {
				if err := s.flushRun(hf, runFrom); err != nil {
					return err
				}
				runFrom = len(w.recs)
			}
		}
	}
	return s.flushRun(hf, runFrom)
}

// appendRecord appends a record, padded to recordAlign, and returns its entry
// with the offset relative to where it starts in dst.
func (s *Store) appendRecord(dst []byte, kind byte, key, body []byte, round uint64) ([]byte, entry) {
	start := len(dst)
	dst = append(dst, make([]byte, recHeaderLen)...)
	dst = append(dst, key...)
	bodyStart := len(dst)
	dst, comp := s.codec.compressAppend(dst, body)
	h := recHeader{
		kind:    kind,
		comp:    comp,
		keyLen:  len(key),
		round:   round,
		stored:  uint64(len(dst) - bodyStart), //nolint:gosec // lengths
		raw:     uint64(len(body)),
		bodyCRC: crc32.Checksum(dst[bodyStart:], castagnoli()),
	}
	h.put(dst[start:], key)
	size := h.size()
	dst = append(dst, make([]byte, alignUp(size)-size)...)
	return dst, entry{off: uint64(start), size: size, raw: h.raw, round: round, kind: kind} //nolint:gosec // lengths
}

// flushRun writes the gathered run to one free extent and fixes the offsets of
// its records.
func (s *Store) flushRun(hf *hxFile, from int) error {
	w := &s.w
	if len(w.run) == 0 {
		return nil
	}
	size := uint64(len(w.run))
	base := hf.sp.alloc(size)
	w.allocs = append(w.allocs, allocation{file: hf, ext: extent{off: base, size: size}})
	if _, err := hf.f.WriteAt(w.run, int64(base)); err != nil { //nolint:gosec // file offsets
		return fmt.Errorf("store: write %s: %w", hf.path, err)
	}
	for i := from; i < len(w.recs); i++ {
		if w.recs[i].inRun {
			w.recs[i].e.off += base
			w.recs[i].inRun = false
		}
	}
	w.run = w.run[:0]
	return nil
}

// writeStream compresses a streamed body straight into free space: room for the
// worst case is taken, and what the body did not need is given back.
func (s *Store) writeStream(hf *hxFile, o *op, key []byte, round uint64) (entry, error) {
	rc, err := o.open()
	if err != nil {
		return entry{}, fmt.Errorf("store: open stream for %s: %w", hf.path, err)
	}
	defer func() { _ = rc.Close() }()
	head := uint64(recHeaderLen + len(key))
	bound := head + uint64(s.codec.enc.MaxEncodedSize(int(o.size))) //nolint:gosec // sizes
	off := hf.sp.alloc(alignUp(bound))
	w := &s.w
	w.allocs = append(w.allocs, allocation{file: hf, ext: extent{off: off, size: alignUp(bound)}})
	bw := &bodyWriter{f: hf.f, off: int64(off + head), limit: int64(bound - head)} //nolint:gosec // file offsets
	if err := s.compressStream(bw, rc, o.size); err != nil {
		return entry{}, fmt.Errorf("store: stream to %s: %w", hf.path, err)
	}
	h := recHeader{kind: o.kind, comp: compZstd, keyLen: len(key), round: round, stored: uint64(bw.n), raw: uint64(o.size), bodyCRC: bw.crc} //nolint:gosec // sizes
	hdr := w.hdr[:head]
	copy(hdr[recHeaderLen:], key)
	h.put(hdr, key)
	if _, err := hf.f.WriteAt(hdr, int64(off)); err != nil { //nolint:gosec // file offsets
		return entry{}, fmt.Errorf("store: write %s: %w", hf.path, err)
	}
	used := alignUp(h.size())
	hf.sp.release(off+used, alignUp(bound)-used)
	w.allocs[len(w.allocs)-1].ext.size = used
	return entry{off: off, size: h.size(), raw: h.raw, round: round, kind: o.kind}, nil
}

func (s *Store) compressStream(bw *bodyWriter, r io.Reader, size int64) error {
	enc, err := s.codec.streamEncoder(bw, size)
	if err != nil {
		return err
	}
	defer s.codec.putStreamEncoder(enc)
	n, err := enc.ReadFrom(io.LimitReader(r, size+1))
	if err != nil {
		_ = enc.Close()
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if n != size {
		return errStreamSize
	}
	return nil
}

// bodyWriter writes a streamed body at increasing offsets, never past limit,
// checksumming it on the way.
type bodyWriter struct {
	f     file
	off   int64
	limit int64
	n     int64
	crc   uint32
}

func (bw *bodyWriter) Write(p []byte) (int, error) {
	if bw.n+int64(len(p)) > bw.limit {
		return 0, errors.New("store: compressed stream larger than its bound")
	}
	n, err := bw.f.WriteAt(p, bw.off+bw.n)
	bw.crc = crc32.Update(bw.crc, castagnoli(), p[:n])
	bw.n += int64(n)
	return n, err
}

// syncRound forces every file written this round, and new directories, to disk.
func (s *Store) syncRound() error {
	if s.opts.NoSync {
		return nil
	}
	for _, hf := range s.w.files {
		if err := s.syncFile(hf); err != nil {
			return err
		}
	}
	for _, d := range s.w.dirs {
		if err := s.syncDirs(d); err != nil {
			return err
		}
	}
	return nil
}

// syncDirs forces dir and each parent up to the store's directory.
func (s *Store) syncDirs(dir string) error {
	for {
		if err := s.fs.SyncDir(dir); err != nil {
			return fmt.Errorf("store: sync %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if dir == s.dir || parent == dir || len(parent) < len(s.dir) {
			return nil
		}
		dir = parent
	}
}

// journalCommit appends the round's commit record: per file, every new
// location and deletion, in key order.
func (s *Store) journalCommit(round uint64) error {
	w := &s.w
	jb := append(w.jbuf[:0], make([]byte, commitHeaderLen)...)
	jb = binary.AppendUvarint(jb, round)
	jb = binary.AppendUvarint(jb, uint64(len(w.files)))
	for i := 0; i < len(w.recs); {
		hf := w.recs[i].file
		end := i + 1
		for end < len(w.recs) && w.recs[end].file == hf {
			end++
		}
		jb = appendCommitFile(jb, hf.path, w.recs[i:end])
		i = end
	}
	w.jbuf = jb
	return s.j.append(jb)
}

func appendCommitFile(jb []byte, path string, recs []pendingRec) []byte {
	jb = binary.AppendUvarint(jb, uint64(len(path)))
	jb = append(jb, path...)
	jb = binary.AppendUvarint(jb, uint64(len(recs)))
	for _, r := range recs {
		if r.del {
			jb = append(jb, opDel)
		} else {
			jb = append(jb, opPut)
		}
		jb = binary.AppendUvarint(jb, uint64(len(r.key)))
		jb = append(jb, r.key...)
		if !r.del {
			jb = append(jb, r.e.kind)
			jb = binary.AppendUvarint(jb, r.e.off)
			jb = binary.AppendUvarint(jb, r.e.size)
			jb = binary.AppendUvarint(jb, r.e.raw)
		}
	}
	return jb
}

// applyRound puts the round's entries into the indexes; what they replace goes
// to the round's freed extents.
func (s *Store) applyRound(round uint64) {
	w := &s.w
	for i := 0; i < len(w.recs); {
		hf := w.recs[i].file
		end := i + 1
		for end < len(w.recs) && w.recs[end].file == hf {
			end++
		}
		s.applyFile(hf, w.recs[i:end], round)
		i = end
	}
}

func (s *Store) applyFile(hf *hxFile, recs []pendingRec, round uint64) {
	w := &s.w
	w.chg = w.chg[:0]
	for _, r := range recs {
		w.chg = append(w.chg, change{key: r.key, e: r.e, del: r.del})
	}
	if len(s.snaps) > 0 {
		s.noteChanges(hf, w.chg)
	}
	hf.mu.Lock()
	w.old = hf.ix.apply(w.chg, w.old[:0])
	hf.mu.Unlock()
	for _, o := range w.old {
		w.freed = append(w.freed, freedExtent{file: hf, ext: o.extent()})
	}
	hf.limbo += len(w.old)
	if !hf.dirty {
		hf.dirty = true
		w.dirty = append(w.dirty, hf)
	}
	hf.round = round
	hf.updateMem(s)
	clear(w.chg)
}

// rollback gives back the space a failed round took. Nothing points at it.
func (s *Store) rollback() {
	w := &s.w
	for i := len(w.allocs) - 1; i >= 0; i-- {
		a := w.allocs[i]
		a.file.sp.release(a.ext.off, a.ext.size)
	}
	w.spareFreed, w.freed = w.freed[:0], nil
}

// maintain checkpoints when the journal is full and keeps the index cache
// within budget.
func (s *Store) maintain() error {
	if s.j.size >= s.opts.JournalLimit {
		if err := s.checkpointAll(); err != nil {
			return err
		}
	}
	s.evict()
	return nil
}
