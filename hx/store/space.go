package store

import (
	"cmp"
	"slices"
)

// extent is a range of a file, aligned to recordAlign.
type extent struct{ off, size uint64 }

func (e extent) end() uint64 { return e.off + e.size }

// space tracks a file's free space: the gaps below end, and everything from end.
// Only the commit goroutine uses it.
type space struct {
	free []extent // sorted by offset, no two touching
	end  uint64
}

func emptySpace() space { return space{end: dataStart} }

// newSpace builds the free space of a file whose records use used. It reports
// false if two of them overlap or one lies in the header.
func newSpace(used []extent) (space, bool) {
	slices.SortFunc(used, func(a, b extent) int { return cmp.Compare(a.off, b.off) })
	sp := emptySpace()
	for _, u := range used {
		if u.off < sp.end {
			// In the header, or over the record before it.
			return space{}, false
		}
		if u.off > sp.end {
			sp.free = append(sp.free, extent{off: sp.end, size: u.off - sp.end})
		}
		sp.end = u.end()
	}
	return sp, true
}

// alloc returns the offset of size free bytes: the smallest gap that fits, or
// the end of the file.
func (sp *space) alloc(size uint64) uint64 {
	best := -1
	for i, e := range sp.free {
		if e.size < size || (best >= 0 && e.size >= sp.free[best].size) {
			continue
		}
		best = i
		if e.size == size {
			break
		}
	}
	if best < 0 {
		off := sp.end
		sp.end += size
		return off
	}
	e := &sp.free[best]
	off := e.off
	if e.size == size {
		sp.free = slices.Delete(sp.free, best, best+1)
	} else {
		e.off += size
		e.size -= size
	}
	return off
}

// release frees an extent, merging it with free neighbours.
func (sp *space) release(off, size uint64) {
	if size == 0 {
		return
	}
	if off+size == sp.end {
		sp.end = off
		if n := len(sp.free); n > 0 && sp.free[n-1].end() == sp.end {
			sp.end = sp.free[n-1].off
			sp.free = sp.free[:n-1]
		}
		return
	}
	i, _ := slices.BinarySearchFunc(sp.free, off, func(e extent, t uint64) int { return cmp.Compare(e.off, t) })
	prev := i > 0 && sp.free[i-1].end() == off
	next := i < len(sp.free) && off+size == sp.free[i].off
	switch {
	case prev && next:
		sp.free[i-1].size += size + sp.free[i].size
		sp.free = slices.Delete(sp.free, i, i+1)
	case prev:
		sp.free[i-1].size += size
	case next:
		sp.free[i].off = off
		sp.free[i].size += size
	default:
		sp.free = slices.Insert(sp.free, i, extent{off: off, size: size})
	}
}

// take marks an extent used that the journal says a round wrote: it must be
// free. It reports false if it is not.
func (sp *space) take(x extent) bool {
	if x.off < dataStart {
		return false
	}
	if x.off >= sp.end {
		if x.off > sp.end {
			sp.free = append(sp.free, extent{off: sp.end, size: x.off - sp.end})
		}
		sp.end = x.end()
		return true
	}
	i, _ := slices.BinarySearchFunc(sp.free, x.off, func(e extent, t uint64) int { return cmp.Compare(e.off, t) })
	if i == len(sp.free) || sp.free[i].off != x.off {
		i--
	}
	if i < 0 || sp.free[i].off > x.off || sp.free[i].end() < x.end() {
		return false
	}
	f := sp.free[i]
	before := extent{off: f.off, size: x.off - f.off}
	after := extent{off: x.end(), size: f.end() - x.end()}
	switch {
	case before.size == 0 && after.size == 0:
		sp.free = slices.Delete(sp.free, i, i+1)
	case before.size == 0:
		sp.free[i] = after
	case after.size == 0:
		sp.free[i] = before
	default:
		sp.free[i] = before
		sp.free = slices.Insert(sp.free, i+1, after)
	}
	return true
}
