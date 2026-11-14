package store

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The journal is a directory of segments. A segment is a header, then commit
// records: a 4-byte length, a 4-byte CRC-32C of the body, and the body.
const (
	journalDir      = "journal"
	segExt          = ".hxj"
	segMagic        = "HXJ\x01"
	segHeaderLen    = 32
	commitHeaderLen = 8
	maxCommitLen    = 1 << 30
)

// Operations in a commit body.
const (
	opPut byte = 1
	opDel byte = 2
)

// journal appends commit records to its newest segment. Only the commit
// goroutine uses it.
type journal struct {
	fs   fsys
	dir  string
	sync bool
	seg  file
	seq  uint64
	size int64
	// old are older segments, removed at the next rotation.
	old []uint64
}

func segName(seq uint64) string { return fmt.Sprintf("%016x%s", seq, segExt) }

func parseSegName(name string) (uint64, bool) {
	hex, ok := strings.CutSuffix(name, segExt)
	if !ok {
		return 0, false
	}
	seq, err := strconv.ParseUint(hex, 16, 64)
	return seq, err == nil
}

// append adds one commit record, built with room for its header at the front.
func (j *journal) append(rec []byte) error {
	body := rec[commitHeaderLen:]
	binary.LittleEndian.PutUint32(rec[0:4], uint32(len(body))) //nolint:gosec // bounded by maxCommitLen
	binary.LittleEndian.PutUint32(rec[4:8], crc32.Checksum(body, castagnoli()))
	if _, err := j.seg.WriteAt(rec, j.size); err != nil {
		return fmt.Errorf("store: journal write: %w", err)
	}
	j.size += int64(len(rec))
	if j.sync {
		if err := j.seg.Sync(); err != nil {
			return fmt.Errorf("store: journal sync: %w", err)
		}
	}
	return nil
}

// rotate starts a new, empty segment beginning after round, and removes every
// older one. Every file must be checkpointed up to round first.
func (j *journal) rotate(round uint64) error {
	seq := j.seq + 1
	f, err := j.fs.OpenFile(filepath.Join(j.dir, segName(seq)), os.O_RDWR|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("store: journal create: %w", err)
	}
	var h [segHeaderLen]byte
	copy(h[0:4], segMagic)
	binary.LittleEndian.PutUint64(h[4:12], seq)
	binary.LittleEndian.PutUint64(h[12:20], round)
	binary.LittleEndian.PutUint32(h[28:32], crc32.Checksum(h[:28], castagnoli()))
	if err := j.writeHeader(f, h[:]); err != nil {
		_ = f.Close()
		return err
	}
	if j.seg != nil {
		_ = j.seg.Close()
		j.old = append(j.old, j.seq)
	}
	j.seg, j.seq, j.size = f, seq, segHeaderLen
	for _, s := range j.old {
		_ = j.fs.Remove(filepath.Join(j.dir, segName(s)))
	}
	j.old = j.old[:0]
	return nil
}

func (j *journal) writeHeader(f file, h []byte) error {
	if _, err := f.WriteAt(h, 0); err != nil {
		return fmt.Errorf("store: journal header: %w", err)
	}
	if !j.sync {
		return nil
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("store: journal sync: %w", err)
	}
	if err := j.fs.SyncDir(j.dir); err != nil {
		return fmt.Errorf("store: journal dir sync: %w", err)
	}
	return nil
}

func (j *journal) close() error {
	if j.seg == nil {
		return nil
	}
	err := j.seg.Close()
	j.seg = nil
	return err
}

// replayed is what the journal held when the store was opened.
type replayed struct {
	round   uint64
	lastSeq uint64
	segs    []uint64
	commits []replayCommit
}

type replayCommit struct {
	round uint64
	files []replayFile
}

type replayFile struct {
	path    string
	changes []change // sorted by key, each key once
}

// replayJournal reads every segment, oldest first. A torn commit record ends
// the newest segment (the round it held never counted); anywhere else it is
// corruption.
func replayJournal(fs fsys, dir string) (replayed, error) {
	names, err := fs.List(dir)
	if err != nil {
		return replayed{}, fmt.Errorf("store: list journal: %w", err)
	}
	var rep replayed
	for _, n := range names {
		if seq, ok := parseSegName(n); ok {
			rep.segs = append(rep.segs, seq)
		}
	}
	slices.Sort(rep.segs)
	for i, seq := range rep.segs {
		last := i == len(rep.segs)-1
		if err := rep.readSegment(fs, filepath.Join(dir, segName(seq)), last); err != nil {
			return replayed{}, err
		}
		rep.lastSeq = seq
	}
	return rep, nil
}

func (rep *replayed) readSegment(fs fsys, path string, last bool) error {
	data, err := readWhole(fs, path)
	if err != nil {
		return fmt.Errorf("store: read journal: %w", err)
	}
	base, ok := parseSegHeader(data)
	if !ok {
		if last {
			// Created when the server stopped, before its header was durable.
			return nil
		}
		return errCorrupt(path, "journal segment header")
	}
	rep.round = max(rep.round, base)
	rest := data[segHeaderLen:]
	for len(rest) > 0 {
		body, n, ok := nextCommit(rest)
		if !ok {
			if last {
				return nil
			}
			return errCorrupt(path, "torn commit record")
		}
		c, err := parseCommit(body, path)
		if err != nil {
			return err
		}
		if c.round <= rep.round {
			return errCorrupt(path, "commit rounds out of order")
		}
		rep.round = c.round
		rep.commits = append(rep.commits, c)
		rest = rest[n:]
	}
	return nil
}

func parseSegHeader(b []byte) (base uint64, ok bool) {
	if len(b) < segHeaderLen || string(b[0:4]) != segMagic {
		return 0, false
	}
	if crc32.Checksum(b[:28], castagnoli()) != binary.LittleEndian.Uint32(b[28:32]) {
		return 0, false
	}
	return binary.LittleEndian.Uint64(b[12:20]), true
}

// nextCommit returns the body of the commit record at the start of b and the
// record's length, or false if it is cut short or its checksum fails.
func nextCommit(b []byte) (body []byte, n int, ok bool) {
	if len(b) < commitHeaderLen {
		return nil, 0, false
	}
	size := uint64(binary.LittleEndian.Uint32(b[0:4]))
	if size > maxCommitLen || size > uint64(len(b)-commitHeaderLen) {
		return nil, 0, false
	}
	body = b[commitHeaderLen : commitHeaderLen+size]
	if crc32.Checksum(body, castagnoli()) != binary.LittleEndian.Uint32(b[4:8]) {
		return nil, 0, false
	}
	return body, commitHeaderLen + int(size), true //nolint:gosec // bounded above
}

func parseCommit(body []byte, path string) (replayCommit, error) {
	d := decoder{b: body}
	c := replayCommit{round: d.uvarint()}
	nfiles := d.uvarint()
	if d.bad || nfiles > uint64(len(body)) {
		return replayCommit{}, errCorrupt(path, "commit record")
	}
	for range nfiles {
		f, err := parseCommitFile(&d, c.round, path)
		if err != nil {
			return replayCommit{}, err
		}
		c.files = append(c.files, f)
	}
	if d.bad || len(d.b) != 0 {
		return replayCommit{}, errCorrupt(path, "commit record")
	}
	return c, nil
}

func parseCommitFile(d *decoder, round uint64, path string) (replayFile, error) {
	f := replayFile{path: string(d.bytes(d.uvarint()))}
	if d.bad || checkPath(f.path) != nil {
		return replayFile{}, errCorrupt(path, "commit file path")
	}
	nops := d.uvarint()
	if d.bad || nops > uint64(len(d.b)) {
		return replayFile{}, errCorrupt(path, "commit record")
	}
	for range nops {
		op := d.byte()
		c := change{key: slices.Clone(d.bytes(d.uvarint()))}
		switch op {
		case opDel:
			c.del = true
		case opPut:
			c.e = entry{kind: d.byte(), off: d.uvarint(), size: d.uvarint(), raw: d.uvarint(), round: round}
		default:
			d.bad = true
		}
		if d.bad || len(c.key) > MaxKeyLen {
			return replayFile{}, errCorrupt(path, "commit record")
		}
		if n := len(f.changes); n > 0 && string(f.changes[n-1].key) >= string(c.key) {
			return replayFile{}, errCorrupt(path, "commit key order")
		}
		f.changes = append(f.changes, c)
	}
	return f, nil
}

// readWhole reads a whole file into memory.
func readWhole(fs fsys, path string) ([]byte, error) {
	f, err := fs.OpenFile(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	size, err := f.Size()
	if err != nil {
		return nil, err
	}
	data := make([]byte, size)
	if _, err := f.ReadAt(data, 0); err != nil && size > 0 {
		return nil, err
	}
	return data, nil
}
