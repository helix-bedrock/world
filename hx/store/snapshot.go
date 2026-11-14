package store

import (
	"bytes"
	"slices"
	"sync"
	"sync/atomic"
)

// A Snapshot is the store as of one save round, across every file, for as
// long as it is held: for live backups and exports while saves go on. Taking
// one costs no I/O and copies no index. It pins the round's generation, so no
// record it can see is overwritten, and every later round notes, before it
// changes a key, what the key held: a snapshot read checks those notes, then
// the current index. Memory grows with the keys changed while it is held, and
// files keep the space of what was replaced until it is released.
//
// Reads are safe from any goroutine. Release it when done.
type Snapshot struct {
	s     *Store
	g     *generation
	round uint64
	done  atomic.Bool

	mu sync.RWMutex
	// old holds, per file, what each key changed since the snapshot was taken
	// held then: its entry, or nothing.
	old map[*hxFile]map[string]oldEntry
}

type oldEntry struct {
	e  entry
	ok bool
}

// Snapshot takes a snapshot of the newest committed round. It waits for a
// commit running at the time.
func (s *Store) Snapshot() (*Snapshot, error) {
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if err := s.usable(); err != nil {
		return nil, err
	}
	g := s.pin()
	sn := &Snapshot{s: s, g: g, round: g.round, old: map[*hxFile]map[string]oldEntry{}}
	s.snaps = append(s.snaps, sn)
	return sn, nil
}

// Round is the save round the snapshot shows.
func (sn *Snapshot) Round() uint64 { return sn.round }

// Release ends the snapshot. Space it kept is reused from the next round.
func (sn *Snapshot) Release() {
	if sn.done.Swap(true) {
		return
	}
	s := sn.s
	s.commitMu.Lock()
	defer s.commitMu.Unlock()
	if i := slices.Index(s.snaps, sn); i >= 0 {
		s.snaps = slices.Delete(s.snaps, i, i+1)
	}
	s.unpin(sn.g)
	if s.failed == nil && !s.closed.Load() {
		s.reclaim()
	}
	sn.mu.Lock()
	sn.old = nil
	sn.mu.Unlock()
}

// noteChanges records, for every open snapshot, what the keys a round is
// about to change hold now. The commit lock is held and hf's index is not
// changing.
func (s *Store) noteChanges(hf *hxFile, chg []change) {
	for _, sn := range s.snaps {
		sn.mu.Lock()
		m := sn.old[hf]
		if m == nil {
			m = map[string]oldEntry{}
			sn.old[hf] = m
		}
		for i := range chg {
			k := chg[i].key
			if _, seen := m[string(k)]; seen {
				continue
			}
			e, ok := hf.ix.get(k)
			m[string(k)] = oldEntry{e: e, ok: ok}
		}
		sn.mu.Unlock()
	}
}

// noted returns what key held when the snapshot was taken, if a round has
// changed it since.
func (sn *Snapshot) noted(hf *hxFile, key []byte) (oldEntry, bool) {
	sn.mu.RLock()
	o, seen := sn.old[hf][string(key)]
	sn.mu.RUnlock()
	return o, seen
}

func (sn *Snapshot) usable() error {
	if sn.done.Load() {
		return ErrReleased
	}
	if sn.s.closed.Load() {
		return ErrClosed
	}
	return nil
}

// Get returns key's body as of the snapshot in dst[:0], as Store.Get does.
func (sn *Snapshot) Get(id FileID, key, dst []byte) ([]byte, error) {
	if err := sn.usable(); err != nil {
		return dst[:0], err
	}
	s := sn.s
	hf, err := s.fileOf(id)
	if err != nil {
		return dst[:0], err
	}
	if err := hf.acquire(s, s.cur.Load().round); err != nil {
		return dst[:0], err
	}
	defer hf.releaseRead()
	// The current index first, then the notes: a round notes a key before it
	// changes it, so a change made between the two is in the notes.
	e, f, ok := sn.entry(hf, key)
	if !ok {
		return dst[:0], ErrNotFound
	}
	return s.readRecord(hf, f, e, key, dst)
}

// Range calls fn for every key a file held as of the snapshot, in key order.
func (sn *Snapshot) Range(id FileID, fn func(key []byte, info Info) bool) error {
	if err := sn.usable(); err != nil {
		return err
	}
	s := sn.s
	hf, err := s.fileOf(id)
	if err != nil {
		return err
	}
	if err := hf.acquire(s, s.cur.Load().round); err != nil {
		return err
	}
	hf.mu.RLock()
	keys, ents := slices.Clone(hf.ix.keys), slices.Clone(hf.ix.ents)
	hf.mu.RUnlock()
	hf.releaseRead()

	sn.mu.RLock()
	old := sn.old[hf]
	items := make([]snapItem, 0, len(ents)+len(old))
	for _, e := range ents {
		k := keys[e.keyOff : e.keyOff+uint32(e.keyLen)]
		if _, changed := old[string(k)]; !changed {
			items = append(items, snapItem{key: k, info: e.info()})
		}
	}
	for k, o := range old {
		if o.ok {
			items = append(items, snapItem{key: []byte(k), info: o.e.info()})
		}
	}
	sn.mu.RUnlock()
	slices.SortFunc(items, func(a, b snapItem) int { return bytes.Compare(a.key, b.key) })
	for _, it := range items {
		if !fn(it.key, it.info) {
			return nil
		}
	}
	return nil
}

type snapItem struct {
	key  []byte
	info Info
}
