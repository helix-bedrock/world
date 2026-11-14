package lsm

import (
	"bytes"
	"container/heap"
)

// internalIter walks internal keys in order, forward only.
type internalIter interface {
	First() bool
	Seek(ikey []byte) bool
	Next() bool
	Key() []byte
	Value() []byte
	Err() error
	Close() error
}

// tableIter walks a table: its index, then each data block in turn. A big block (one
// value of at least the table's bigBlock) is not read when reached: only its key is, and
// its value is read whole only if Value is called. Compaction copies such blocks as
// stored instead (bigBlock).
type tableIter struct {
	t     *table
	index *blockIter
	data  *blockIter
	err   error
	valid bool

	// big is set while on a big block, streamed by br entry by entry (bigblock.go):
	// bigValue holds the current entry's value once read (bigLoad).
	big      bool
	bigH     blockHandle
	br       *bigReader
	bigOK    bool
	bigValue []byte
	bigLoad  bool
}

func newTableIter(t *table) *tableIter {
	return &tableIter{t: t, index: newBlockIter(t.index, compareInternal), data: newBlockIter(block{}, compareInternal)}
}

// loadBlock points the data iterator at the block the index iterator is on.
func (it *tableIter) loadBlock() bool {
	h, _, err := decodeHandle(it.index.value)
	if err != nil {
		it.err = err
		return false
	}
	it.closeBig()
	it.big = h.size >= it.t.bigBlock
	if it.big {
		return it.loadBig(h)
	}
	b, err := it.t.readBlock(h)
	if err != nil {
		it.err = err
		return false
	}
	it.data.reset(b)
	return true
}

// loadBig opens a big block at its first entry.
func (it *tableIter) loadBig(h blockHandle) bool {
	br, ok, err := it.t.openBig(h)
	if err != nil {
		it.err = err
		return false
	}
	it.bigH, it.br, it.bigOK, it.bigLoad = h, br, ok, false
	// The data iterator is unused: an empty block keeps it from reporting entries.
	it.data.reset(block{})
	return true
}

// closeBig releases the big block reader, if any, and a value buffer grown for a big
// value.
func (it *tableIter) closeBig() {
	if it.br != nil {
		it.br.Close()
		it.br = nil
	}
	if cap(it.bigValue) > int(it.t.bigBlock) { //nolint:gosec // small
		it.bigValue = nil
	}
}

// bigSource reports the big block the iterator is on, for compaction to copy.
func (it *tableIter) bigSource() (*table, blockHandle, bool) {
	if !it.valid || !it.big || it.br == nil || !it.br.single() {
		return nil, blockHandle{}, false
	}
	return it.t, it.bigH, true
}

// blockFirst moves to the first entry of the current block.
func (it *tableIter) blockFirst() bool {
	if it.big {
		return it.bigOK
	}
	return it.data.first()
}

// blockSeek moves to the first entry of the current block at or after ikey.
func (it *tableIter) blockSeek(ikey []byte) bool {
	if it.big {
		for it.bigOK && compareInternal(it.br.key, ikey) < 0 {
			it.bigNext()
		}
		return it.bigOK
	}
	return it.data.seek(ikey)
}

// blockNext moves to the next entry of the current block.
func (it *tableIter) blockNext() bool {
	if it.big {
		return it.bigNext()
	}
	return it.data.next()
}

// bigNext moves to the next entry of a big block. After a value alone in its block,
// the block is left without inflating the value.
func (it *tableIter) bigNext() bool {
	it.bigLoad = false
	if it.br.single() {
		it.bigOK = false
		return false
	}
	ok, err := it.br.next()
	if err != nil {
		it.err = err
	}
	it.bigOK = ok
	return ok
}

// skipEmpty moves to the next block while the current one is exhausted.
func (it *tableIter) skipEmpty(ok bool) bool {
	for !ok {
		if it.data.err != nil {
			it.err = it.data.err
		}
		if it.err != nil || !it.index.next() || !it.loadBlock() {
			if it.index.err != nil {
				it.err = it.index.err
			}
			it.valid = false
			return false
		}
		ok = it.blockFirst()
	}
	it.valid = true
	return true
}

func (it *tableIter) First() bool {
	if !it.index.first() {
		it.err, it.valid = it.index.err, false
		return false
	}
	if !it.loadBlock() {
		it.valid = false
		return false
	}
	return it.skipEmpty(it.blockFirst())
}

func (it *tableIter) Seek(ikey []byte) bool {
	if !it.index.seek(ikey) {
		it.err, it.valid = it.index.err, false
		return false
	}
	if !it.loadBlock() {
		it.valid = false
		return false
	}
	return it.skipEmpty(it.blockSeek(ikey))
}

func (it *tableIter) Next() bool {
	if !it.valid {
		return false
	}
	return it.skipEmpty(it.blockNext())
}

func (it *tableIter) Key() []byte {
	if it.big {
		return it.br.key
	}
	return it.data.key
}

// Value is the current value. A big block's value is read whole here, the first time.
func (it *tableIter) Value() []byte {
	if !it.big {
		return it.data.value
	}
	if !it.bigLoad {
		it.bigLoad = true
		v, err := it.br.value(it.bigValue)
		if err != nil {
			it.err = err
		}
		it.bigValue = v
	}
	return it.bigValue
}

func (it *tableIter) Err() error { return it.err }
func (it *tableIter) Close() error {
	it.closeBig()
	return nil
}

// bigSourcer is an iterator that may stand on a big block compaction can copy as stored.
type bigSourcer interface {
	bigSource() (*table, blockHandle, bool)
}

func (it *levelIter) bigSource() (*table, blockHandle, bool) {
	if it.cur == nil {
		return nil, blockHandle{}, false
	}
	return it.cur.bigSource()
}

func (m *mergeIter) bigSource() (*table, blockHandle, bool) {
	if len(m.h) == 0 {
		return nil, blockHandle{}, false
	}
	if s, ok := m.h[0].(bigSourcer); ok {
		return s.bigSource()
	}
	return nil, blockHandle{}, false
}

// levelIter walks the non-overlapping tables of one level in key order, opening each
// table only when reached.
type levelIter struct {
	files []*fileMeta
	tc    *tableCache
	i     int
	ent   *openTableEntry
	cur   *tableIter
	err   error
}

func newLevelIter(files []*fileMeta, tc *tableCache) *levelIter {
	return &levelIter{files: files, tc: tc, i: -1}
}

// open moves to file i.
func (it *levelIter) open(i int) bool {
	it.closeCur()
	it.i = i
	if i >= len(it.files) {
		return false
	}
	f := it.files[i]
	ent, err := it.tc.acquire(f.num, f.size)
	if err != nil {
		it.err = err
		return false
	}
	it.ent, it.cur = ent, newTableIter(ent.t)
	return true
}

func (it *levelIter) closeCur() {
	if it.cur != nil {
		_ = it.cur.Close()
	}
	if it.ent != nil {
		it.tc.release(it.ent)
		it.ent, it.cur = nil, nil
	}
}

// advance moves through files until one has an entry.
func (it *levelIter) advance(ok bool) bool {
	for !ok {
		if it.cur != nil && it.cur.err != nil {
			it.err = it.cur.err
		}
		if it.err != nil || !it.open(it.i+1) {
			return false
		}
		ok = it.cur.First()
	}
	return true
}

func (it *levelIter) First() bool {
	if !it.open(0) {
		return false
	}
	return it.advance(it.cur.First())
}

func (it *levelIter) Seek(ikey []byte) bool {
	i := 0
	for i < len(it.files) && compareInternal(it.files[i].largest, ikey) < 0 {
		i++
	}
	if !it.open(i) {
		return false
	}
	return it.advance(it.cur.Seek(ikey))
}

func (it *levelIter) Next() bool {
	if it.cur == nil {
		return false
	}
	return it.advance(it.cur.Next())
}

func (it *levelIter) Key() []byte   { return it.cur.Key() }
func (it *levelIter) Value() []byte { return it.cur.Value() }
func (it *levelIter) Err() error    { return it.err }

func (it *levelIter) Close() error {
	it.closeCur()
	return nil
}

// mergeIter merges iterators by internal key. Ties never happen: every entry has its
// own sequence number.
type mergeIter struct {
	iters []internalIter
	h     iterHeap
	err   error
}

func newMergeIter(iters []internalIter) *mergeIter { return &mergeIter{iters: iters} }

type iterHeap []internalIter

func (h iterHeap) Len() int           { return len(h) }
func (h iterHeap) Less(i, j int) bool { return compareInternal(h[i].Key(), h[j].Key()) < 0 }
func (h iterHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *iterHeap) Push(x any) {
	if it, ok := x.(internalIter); ok {
		*h = append(*h, it)
	}
}

func (h *iterHeap) Pop() any {
	old := *h
	it := old[len(old)-1]
	*h = old[:len(old)-1]
	return it
}

// build fills the heap with the iterators positioned by pos.
func (m *mergeIter) build(pos func(internalIter) bool) bool {
	m.h = m.h[:0]
	for _, it := range m.iters {
		if pos(it) {
			m.h = append(m.h, it)
		} else if err := it.Err(); err != nil {
			m.err = err
		}
	}
	heap.Init(&m.h)
	return m.err == nil && len(m.h) > 0
}

func (m *mergeIter) First() bool { return m.build(internalIter.First) }

func (m *mergeIter) Seek(ikey []byte) bool {
	return m.build(func(it internalIter) bool { return it.Seek(ikey) })
}

func (m *mergeIter) Next() bool {
	if len(m.h) == 0 || m.err != nil {
		return false
	}
	top := m.h[0]
	if top.Next() {
		heap.Fix(&m.h, 0)
	} else {
		if err := top.Err(); err != nil {
			m.err = err
			return false
		}
		heap.Pop(&m.h)
	}
	return len(m.h) > 0
}

func (m *mergeIter) Key() []byte   { return m.h[0].Key() }
func (m *mergeIter) Value() []byte { return m.h[0].Value() }
func (m *mergeIter) Err() error    { return m.err }

func (m *mergeIter) Close() error {
	for _, it := range m.iters {
		_ = it.Close()
	}
	return nil
}

// Iterator walks the user keys of a database as of one snapshot, in order, forward only.
// Key and Value are valid until the next call to Next or Seek.
type Iterator struct {
	m       *mergeIter
	seq     uint64
	prefix  []byte
	key     []byte
	valid   bool
	release func()
	err     error
}

// First moves to the first key; false if there is none.
func (it *Iterator) First() bool {
	if len(it.prefix) > 0 {
		return it.Seek(it.prefix)
	}
	return it.settle(it.m.First())
}

// Seek moves to the first key at or after key.
func (it *Iterator) Seek(key []byte) bool {
	return it.settle(it.m.Seek(appendInternalKey(nil, key, it.seq, kindSeek)))
}

// Next moves to the next key.
func (it *Iterator) Next() bool {
	if !it.valid {
		return false
	}
	return it.settle(it.skipKey(it.key))
}

// skipKey moves past every remaining entry of ukey.
func (it *Iterator) skipKey(ukey []byte) bool {
	for {
		if !it.m.Next() {
			return false
		}
		if !bytes.Equal(userKey(it.m.Key()), ukey) {
			return true
		}
	}
}

// settle moves from the merged position to the next visible user key: the newest entry
// at or below the snapshot that is not a deletion.
func (it *Iterator) settle(ok bool) bool {
	for ok {
		ukey, seq, k, valid := splitInternalKey(it.m.Key())
		if !valid {
			it.err = errCorrupt("internal key")
			break
		}
		if len(it.prefix) > 0 && !bytes.HasPrefix(ukey, it.prefix) {
			break
		}
		if seq > it.seq {
			ok = it.m.Next()
			continue
		}
		if k == kindDelete {
			ok = it.skipKey(append(it.key[:0], ukey...))
			continue
		}
		it.key = append(it.key[:0], ukey...)
		it.valid = true
		return true
	}
	if it.err == nil {
		it.err = it.m.Err()
	}
	it.valid = false
	return false
}

// Key is the current user key.
func (it *Iterator) Key() []byte { return it.key }

// Value is the current value. A big value is read whole only when asked for.
func (it *Iterator) Value() []byte {
	if !it.valid {
		return nil
	}
	return it.m.Value()
}

// Err is the first error met.
func (it *Iterator) Err() error { return it.err }

// Close releases the iterator.
func (it *Iterator) Close() error {
	_ = it.m.Close()
	if it.release != nil {
		it.release()
		it.release = nil
	}
	return nil
}
