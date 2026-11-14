package store

import (
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// memFS is a file system in memory that, like a disk, keeps only what was
// forced to it when the machine stops: crash keeps each write made since the
// last Sync whole, torn or not at all, at random. After failAfter mutating
// calls every call fails, as if the program had died there.
type memFS struct {
	mu        sync.Mutex
	files     map[string]*memFile
	rng       *rand.Rand
	ops       int
	failAfter int // -1: never
}

type memFile struct {
	data    []byte
	synced  []byte
	pending []memWrite
	// durable is set once the file's name survives a crash.
	durable bool
}

// memWrite is a write or, with truncate set, a truncation since the last Sync.
type memWrite struct {
	off      int64
	data     []byte
	truncate bool
}

var errDied = errors.New("memfs: the program died here")

func newMemFS(seed uint64) *memFS {
	return &memFS{files: map[string]*memFile{}, rng: rand.New(rand.NewPCG(seed, seed^0x9e37)), failAfter: -1} //nolint:gosec // test randomness
}

// step counts a mutating call and reports whether it fails.
func (m *memFS) step() error {
	m.ops++
	if m.failAfter >= 0 && m.ops > m.failAfter {
		return errDied
	}
	return nil
}

func (m *memFS) OpenFile(name string, flag int) (file, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[name]
	if !ok {
		if flag&os.O_CREATE == 0 {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		if err := m.step(); err != nil {
			return nil, err
		}
		f = &memFile{}
		m.files[name] = f
	}
	if flag&os.O_TRUNC != 0 {
		if err := m.step(); err != nil {
			return nil, err
		}
		f.data = f.data[:0]
		f.pending = append(f.pending, memWrite{truncate: true})
	}
	return &memHandle{m: m, f: f}, nil
}

func (m *memFS) MkdirAll(string) error { return nil }

func (m *memFS) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.step(); err != nil {
		return err
	}
	delete(m.files, name)
	return nil
}

func (m *memFS) List(dir string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var names []string
	for n := range m.files {
		if filepath.Dir(n) == dir {
			names = append(names, filepath.Base(n))
		}
	}
	slices.Sort(names)
	return names, nil
}

func (m *memFS) SyncDir(dir string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.step(); err != nil {
		return err
	}
	for n, f := range m.files {
		if filepath.Dir(n) == dir {
			f.durable = true
		}
	}
	return nil
}

// crash stops the machine: every file keeps what was synced plus a random
// part of what was not, and files whose names were never made durable go.
func (m *memFS) crash() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for n, f := range m.files {
		if !f.durable {
			delete(m.files, n)
			continue
		}
		data := slices.Clone(f.synced)
		for _, w := range f.pending {
			data = m.replayWrite(data, w)
		}
		f.data, f.synced, f.pending = data, slices.Clone(data), nil
	}
	m.ops, m.failAfter = 0, -1
}

func (m *memFS) replayWrite(data []byte, w memWrite) []byte {
	if w.truncate {
		if m.rng.IntN(2) == 0 {
			return data // lost
		}
		return data[:min(int64(len(data)), w.off)]
	}
	switch m.rng.IntN(3) {
	case 0:
		return data // lost
	case 1:
		return writeAt(data, w.data[:m.rng.IntN(len(w.data)+1)], w.off) // torn
	default:
		return writeAt(data, w.data, w.off)
	}
}

func writeAt(data, p []byte, off int64) []byte {
	if end := int(off) + len(p); end > len(data) {
		data = append(data, make([]byte, end-len(data))...)
	}
	copy(data[off:], p)
	return data
}

type memHandle struct {
	m *memFS
	f *memFile
}

func (h *memHandle) ReadAt(p []byte, off int64) (int, error) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if off >= int64(len(h.f.data)) {
		return 0, io.EOF
	}
	n := copy(p, h.f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (h *memHandle) WriteAt(p []byte, off int64) (int, error) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if err := h.m.step(); err != nil {
		return 0, err
	}
	h.f.data = writeAt(h.f.data, p, off)
	h.f.pending = append(h.f.pending, memWrite{off: off, data: slices.Clone(p)})
	return len(p), nil
}

func (h *memHandle) Sync() error {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if err := h.m.step(); err != nil {
		return err
	}
	h.f.synced, h.f.pending = slices.Clone(h.f.data), nil
	return nil
}

func (h *memHandle) Truncate(size int64) error {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if err := h.m.step(); err != nil {
		return err
	}
	if size < int64(len(h.f.data)) {
		h.f.data = h.f.data[:size]
	}
	h.f.pending = append(h.f.pending, memWrite{off: size, truncate: true})
	return nil
}

func (h *memHandle) Size() (int64, error) {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return int64(len(h.f.data)), nil
}

func (h *memHandle) Close() error { return nil }
