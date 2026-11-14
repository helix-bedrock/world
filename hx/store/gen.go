package store

import "sync/atomic"

// A generation is the store between two save rounds. A read pins the current
// generation for as long as it uses what it looked up. The extents a round
// frees are attached to the generation before it and reused only once that
// generation and every older one has no reader left: a read never sees a
// record overwritten under it.
type generation struct {
	round uint64
	refs  atomic.Int64
	// freed are the extents the round ending this generation replaced.
	freed []freedExtent
	// next is the generation after this one; set under the commit lock.
	next *generation
}

type freedExtent struct {
	file *hxFile
	ext  extent
}

// pin returns the current generation, pinned.
func (s *Store) pin() *generation {
	for {
		g := s.cur.Load()
		g.refs.Add(1)
		if s.cur.Load() == g {
			return g
		}
		// A round ended meanwhile; nothing was read through g.
		g.refs.Add(-1)
	}
}

func (s *Store) unpin(g *generation) { g.refs.Add(-1) }

// advance ends the current generation with the extents its round freed. The
// commit lock is held.
func (s *Store) advance(round uint64, freed []freedExtent) {
	old := s.cur.Load()
	old.freed = freed
	g := &generation{round: round}
	old.next = g
	s.cur.Store(g)
}

// reclaim releases the freed extents of every finished generation no reader
// still pins, oldest first. The commit lock is held.
func (s *Store) reclaim() {
	for g := s.oldest; g != s.cur.Load() && g.refs.Load() == 0; g = s.oldest {
		for _, fe := range g.freed {
			fe.file.sp.release(fe.ext.off, fe.ext.size)
			fe.file.limbo--
		}
		if s.w.spareFreed == nil {
			s.w.spareFreed = g.freed[:0]
		}
		s.oldest, g.freed, g.next = g.next, nil, nil
	}
}
