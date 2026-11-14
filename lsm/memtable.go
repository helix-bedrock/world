package lsm

import (
	"bytes"
	"sync"
)

// The memtable is a skiplist kept in two flat slices: an arena of key and value bytes and
// an array of nodes linked by index. Nothing in it is a pointer, so the garbage collector
// never scans it. Bytes in the arena are never changed once written: a slice of an older
// arena stays valid after the arena grows, so readers may keep keys and values without
// copying while the memtable lives.

const memMaxHeight = 12

// memNode is one entry. Node 0 is the head and holds no entry.
type memNode struct {
	off    uint32 // the internal key, then the value, in the arena
	klen   uint32
	vlen   uint32
	height uint8
	next   [memMaxHeight]uint32
}

type memtable struct {
	mu     sync.RWMutex
	arena  []byte
	nodes  []memNode
	height int
	rnd    uint64
	prev   [memMaxHeight]uint32
}

func newMemtable(capacity int) *memtable {
	m := &memtable{arena: make([]byte, 0, capacity), nodes: make([]memNode, 1, capacity/64+1), height: 1, rnd: 0x2545F4914F6CDD1D}
	m.nodes[0].height = memMaxHeight
	return m
}

// size is about how much memory the memtable takes.
func (m *memtable) size() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.arena) + len(m.nodes)*int(sizeofMemNode)
}

const sizeofMemNode = 4*3 + 1 + 3 + 4*memMaxHeight

// empty reports whether the memtable holds no entry.
func (m *memtable) empty() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.nodes) == 1
}

// add inserts an entry. The caller holds no lock; sequence numbers are unique, so no two
// entries ever have the same internal key.
func (m *memtable) add(seq uint64, k kind, ukey, value []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	off := len(m.arena)
	m.arena = appendInternalKey(m.arena, ukey, seq, k)
	klen := len(m.arena) - off
	m.arena = append(m.arena, value...)
	ikey := m.arena[off : off+klen]
	m.findPrev(ikey)
	h := m.randomHeight()
	if h > m.height {
		for i := m.height; i < h; i++ {
			m.prev[i] = 0
		}
		m.height = h
	}
	n := memNode{off: uint32(off), klen: uint32(klen), vlen: uint32(len(value)), height: uint8(h)} //nolint:gosec // memtables stay far under 4 GB
	idx := uint32(len(m.nodes))                                                                     //nolint:gosec // as above
	for i := range h {
		n.next[i] = m.nodes[m.prev[i]].next[i]
	}
	m.nodes = append(m.nodes, n)
	for i := range h {
		m.nodes[m.prev[i]].next[i] = idx
	}
}

// findPrev fills m.prev with, per level, the last node before ikey. It returns the node
// at or after ikey on level 0 (0 if none).
func (m *memtable) findPrev(ikey []byte) uint32 {
	x := uint32(0)
	for level := m.height - 1; level >= 0; level-- {
		for {
			nx := m.nodes[x].next[level]
			if nx == 0 || compareInternal(m.key(nx), ikey) >= 0 {
				break
			}
			x = nx
		}
		m.prev[level] = x
	}
	return m.nodes[x].next[0]
}

// seekNode returns the first node at or after ikey, without touching m.prev. The caller
// holds the read lock.
func (m *memtable) seekNode(ikey []byte) uint32 {
	x := uint32(0)
	for level := m.height - 1; level >= 0; level-- {
		for {
			nx := m.nodes[x].next[level]
			if nx == 0 || compareInternal(m.key(nx), ikey) >= 0 {
				break
			}
			x = nx
		}
	}
	return m.nodes[x].next[0]
}

func (m *memtable) randomHeight() int {
	h := 1
	for h < memMaxHeight {
		m.rnd ^= m.rnd << 13
		m.rnd ^= m.rnd >> 7
		m.rnd ^= m.rnd << 17
		if m.rnd%4 != 0 {
			break
		}
		h++
	}
	return h
}

func (m *memtable) key(n uint32) []byte {
	nd := &m.nodes[n]
	return m.arena[nd.off : nd.off+nd.klen : nd.off+nd.klen]
}

func (m *memtable) value(n uint32) []byte {
	nd := &m.nodes[n]
	start := nd.off + nd.klen
	return m.arena[start : start+nd.vlen : start+nd.vlen]
}

// get returns the newest entry of ikey's user key at or below ikey's sequence number.
func (m *memtable) get(ikey []byte) (key, value []byte, found bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := m.seekNode(ikey)
	if n == 0 || !bytes.Equal(userKey(m.key(n)), userKey(ikey)) {
		return nil, nil, false
	}
	return m.key(n), m.value(n), true
}

// memIter walks a memtable. Keys and values stay valid while the memtable lives.
type memIter struct {
	m   *memtable
	n   uint32
	key []byte
	val []byte
}

func (m *memtable) iter() *memIter { return &memIter{m: m} }

func (it *memIter) load() bool {
	if it.n == 0 {
		it.key, it.val = nil, nil
		return false
	}
	it.key, it.val = it.m.key(it.n), it.m.value(it.n)
	return true
}

func (it *memIter) First() bool {
	it.m.mu.RLock()
	defer it.m.mu.RUnlock()
	it.n = it.m.nodes[0].next[0]
	return it.load()
}

func (it *memIter) Seek(ikey []byte) bool {
	it.m.mu.RLock()
	defer it.m.mu.RUnlock()
	it.n = it.m.seekNode(ikey)
	return it.load()
}

func (it *memIter) Next() bool {
	if it.n == 0 {
		return false
	}
	it.m.mu.RLock()
	defer it.m.mu.RUnlock()
	it.n = it.m.nodes[it.n].next[0]
	return it.load()
}

func (it *memIter) Key() []byte   { return it.key }
func (it *memIter) Value() []byte { return it.val }
func (it *memIter) Err() error    { return nil }
func (it *memIter) Close() error  { return nil }
