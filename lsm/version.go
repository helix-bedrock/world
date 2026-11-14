package lsm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
)

// numLevels is LevelDB's level count.
const numLevels = 7

// comparatorName is the comparator LevelDB and Bedrock record in the manifest.
const comparatorName = "leveldb.BytewiseComparator"

// fileMeta describes one table.
type fileMeta struct {
	num               uint64
	size              uint64
	smallest, largest []byte // internal keys
}

// versionEdit is one manifest record (LevelDB db/version_edit.cc).
type versionEdit struct {
	comparator   string
	logNum       uint64
	prevLogNum   uint64
	nextFile     uint64
	lastSeq      uint64
	hasLogNum    bool
	hasPrevLog   bool
	hasNextFile  bool
	hasLastSeq   bool
	compactPtrs  []levelKey
	deleted      []levelFile
	added        []levelMeta
}

type levelKey struct {
	level int
	key   []byte
}

type levelFile struct {
	level int
	num   uint64
}

type levelMeta struct {
	level int
	meta  fileMeta
}

// Manifest record tags.
const (
	tagComparator     = 1
	tagLogNumber      = 2
	tagNextFileNumber = 3
	tagLastSequence   = 4
	tagCompactPointer = 5
	tagDeletedFile    = 6
	tagNewFile        = 7
	tagPrevLogNumber  = 9
)

func (e *versionEdit) setLogNum(n uint64)   { e.logNum, e.hasLogNum = n, true }
func (e *versionEdit) setNextFile(n uint64) { e.nextFile, e.hasNextFile = n, true }
func (e *versionEdit) setLastSeq(n uint64)  { e.lastSeq, e.hasLastSeq = n, true }

func (e *versionEdit) addFile(level int, m fileMeta) {
	e.added = append(e.added, levelMeta{level: level, meta: m})
}

func (e *versionEdit) deleteFile(level int, num uint64) {
	e.deleted = append(e.deleted, levelFile{level: level, num: num})
}

// encode appends the edit as a manifest record.
func (e *versionEdit) encode(dst []byte) []byte {
	if e.comparator != "" {
		dst = binary.AppendUvarint(dst, tagComparator)
		dst = appendUvarintBytes(dst, []byte(e.comparator))
	}
	if e.hasLogNum {
		dst = binary.AppendUvarint(dst, tagLogNumber)
		dst = binary.AppendUvarint(dst, e.logNum)
	}
	if e.hasPrevLog {
		dst = binary.AppendUvarint(dst, tagPrevLogNumber)
		dst = binary.AppendUvarint(dst, e.prevLogNum)
	}
	if e.hasNextFile {
		dst = binary.AppendUvarint(dst, tagNextFileNumber)
		dst = binary.AppendUvarint(dst, e.nextFile)
	}
	if e.hasLastSeq {
		dst = binary.AppendUvarint(dst, tagLastSequence)
		dst = binary.AppendUvarint(dst, e.lastSeq)
	}
	for _, p := range e.compactPtrs {
		dst = binary.AppendUvarint(dst, tagCompactPointer)
		dst = binary.AppendUvarint(dst, uint64(p.level)) //nolint:gosec // small
		dst = appendUvarintBytes(dst, p.key)
	}
	for _, d := range e.deleted {
		dst = binary.AppendUvarint(dst, tagDeletedFile)
		dst = binary.AppendUvarint(dst, uint64(d.level)) //nolint:gosec // small
		dst = binary.AppendUvarint(dst, d.num)
	}
	for _, a := range e.added {
		dst = binary.AppendUvarint(dst, tagNewFile)
		dst = binary.AppendUvarint(dst, uint64(a.level)) //nolint:gosec // small
		dst = binary.AppendUvarint(dst, a.meta.num)
		dst = binary.AppendUvarint(dst, a.meta.size)
		dst = appendUvarintBytes(dst, a.meta.smallest)
		dst = appendUvarintBytes(dst, a.meta.largest)
	}
	return dst
}

// decode reads a manifest record.
func (e *versionEdit) decode(b []byte) error {
	for len(b) > 0 {
		tag, rest, err := readUvarint(b)
		if err != nil {
			return err
		}
		if b, err = e.decodeField(tag, rest); err != nil {
			return err
		}
	}
	return nil
}

func (e *versionEdit) decodeField(tag uint64, b []byte) ([]byte, error) {
	var err error
	switch tag {
	case tagComparator:
		var v []byte
		v, b, err = readUvarintBytes(b)
		e.comparator = string(v)
	case tagLogNumber:
		e.logNum, b, err = readUvarint(b)
		e.hasLogNum = true
	case tagPrevLogNumber:
		e.prevLogNum, b, err = readUvarint(b)
		e.hasPrevLog = true
	case tagNextFileNumber:
		e.nextFile, b, err = readUvarint(b)
		e.hasNextFile = true
	case tagLastSequence:
		e.lastSeq, b, err = readUvarint(b)
		e.hasLastSeq = true
	case tagCompactPointer:
		var p levelKey
		if p.level, b, err = readLevel(b); err == nil {
			p.key, b, err = readUvarintBytes(b)
			p.key = bytes.Clone(p.key)
			e.compactPtrs = append(e.compactPtrs, p)
		}
	case tagDeletedFile:
		var d levelFile
		if d.level, b, err = readLevel(b); err == nil {
			d.num, b, err = readUvarint(b)
			e.deleted = append(e.deleted, d)
		}
	case tagNewFile:
		b, err = e.decodeNewFile(b)
	default:
		return nil, errCorrupt(fmt.Sprintf("manifest tag %d", tag))
	}
	return b, err
}

func (e *versionEdit) decodeNewFile(b []byte) ([]byte, error) {
	var a levelMeta
	var err error
	if a.level, b, err = readLevel(b); err != nil {
		return nil, err
	}
	if a.meta.num, b, err = readUvarint(b); err != nil {
		return nil, err
	}
	if a.meta.size, b, err = readUvarint(b); err != nil {
		return nil, err
	}
	var k []byte
	if k, b, err = readUvarintBytes(b); err != nil {
		return nil, err
	}
	a.meta.smallest = bytes.Clone(k)
	if k, b, err = readUvarintBytes(b); err != nil {
		return nil, err
	}
	a.meta.largest = bytes.Clone(k)
	e.added = append(e.added, a)
	return b, nil
}

func readLevel(b []byte) (int, []byte, error) {
	l, rest, err := readUvarint(b)
	if err != nil || l >= numLevels {
		return 0, nil, errCorrupt("manifest level")
	}
	return int(l), rest, nil
}

// version is one immutable set of tables: L0 newest first, deeper levels sorted by key.
type version struct {
	levels [numLevels][]*fileMeta
	refs   int // guarded by the versionSet's lock
}

// apply returns a new version: v with the edit's files deleted and added.
func (v *version) apply(e *versionEdit) *version {
	nv := &version{}
	for level := range numLevels {
		nv.levels[level] = slices.DeleteFunc(slices.Clone(v.levels[level]), func(f *fileMeta) bool {
			return slices.Contains(e.deleted, levelFile{level: level, num: f.num})
		})
	}
	for _, a := range e.added {
		m := a.meta
		nv.levels[a.level] = append(nv.levels[a.level], &m)
	}
	nv.sortLevels()
	return nv
}

// sortLevels orders L0 newest (highest file number) first and other levels by smallest key.
func (v *version) sortLevels() {
	slices.SortFunc(v.levels[0], func(a, b *fileMeta) int {
		switch {
		case a.num > b.num:
			return -1
		case a.num < b.num:
			return 1
		}
		return 0
	})
	for level := 1; level < numLevels; level++ {
		slices.SortFunc(v.levels[level], func(a, b *fileMeta) int { return compareInternal(a.smallest, b.smallest) })
	}
}

// overlapping returns the files of level whose user key range overlaps [smallest, largest]
// (user keys; nil means unbounded).
func (v *version) overlapping(level int, smallest, largest []byte) []*fileMeta {
	var out []*fileMeta
	for _, f := range v.levels[level] {
		if largest != nil && bytes.Compare(userKey(f.smallest), largest) > 0 {
			continue
		}
		if smallest != nil && bytes.Compare(userKey(f.largest), smallest) < 0 {
			continue
		}
		out = append(out, f)
	}
	return out
}

// filesFor returns the files that may hold ukey, in the order to search them.
func (v *version) filesFor(ukey []byte, dst []*fileMeta) []*fileMeta {
	for _, f := range v.levels[0] {
		if bytes.Compare(ukey, userKey(f.smallest)) >= 0 && bytes.Compare(ukey, userKey(f.largest)) <= 0 {
			dst = append(dst, f)
		}
	}
	for level := 1; level < numLevels; level++ {
		files := v.levels[level]
		i, _ := slices.BinarySearchFunc(files, ukey, func(f *fileMeta, k []byte) int {
			return bytes.Compare(userKey(f.largest), k)
		})
		if i < len(files) && bytes.Compare(ukey, userKey(files[i].smallest)) >= 0 {
			dst = append(dst, files[i])
		}
	}
	return dst
}

// levelBytes is the total size of a level's tables.
func (v *version) levelBytes(level int) uint64 {
	var n uint64
	for _, f := range v.levels[level] {
		n += f.size
	}
	return n
}

// files returns every table number in the version.
func (v *version) files(dst map[uint64]bool) {
	for level := range numLevels {
		for _, f := range v.levels[level] {
			dst[f.num] = true
		}
	}
}
