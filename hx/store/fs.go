package store

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"runtime"
	"sync/atomic"
)

// fsys is the file system a store lives on: the operating system's, or one
// that loses unforced writes in tests.
type fsys interface {
	OpenFile(name string, flag int) (file, error)
	MkdirAll(name string) error
	Remove(name string) error
	// List returns the names of the files in dir, or none if dir is missing.
	List(dir string) ([]string, error)
	// SyncDir makes the names in dir durable.
	SyncDir(dir string) error
}

// file is an open file.
type file interface {
	io.ReaderAt
	io.WriterAt
	Sync() error
	Truncate(size int64) error
	Size() (int64, error)
	Close() error
}

type osFS struct{}

func (osFS) OpenFile(name string, flag int) (file, error) {
	f, err := os.OpenFile(name, flag, 0o644) //nolint:gosec // paths are checked by Store.File
	if err != nil {
		return nil, err
	}
	of := &osFile{File: f, name: name}
	if runtime.GOOS == "windows" {
		of.readers = make(chan *os.File, maxReadHandles)
	}
	return of, nil
}

func (osFS) MkdirAll(name string) error { return os.MkdirAll(name, 0o755) }

func (osFS) Remove(name string) error { return os.Remove(name) }

func (osFS) List(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.Type().IsRegular() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// SyncDir forces a directory's entries to disk. Windows has no such call: a
// file's name is durable with the file itself there.
func (osFS) SyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir) //nolint:gosec // the store's own directories
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}

// maxReadHandles is how many extra handles a file opens on Windows, where Go
// runs one positioned read at a time per handle (and seeks around each): with
// a handle per concurrent read, reads run in parallel and never queue behind
// a save's writes on the main handle.
const maxReadHandles = 8

// osFile is an open file. Writes, syncs and truncation use the main handle;
// on Windows, reads take one of up to maxReadHandles read-only handles.
type osFile struct {
	*os.File
	name    string
	readers chan *os.File // nil where one handle reads in parallel
	opened  atomic.Int32
}

func (f *osFile) ReadAt(p []byte, off int64) (int, error) {
	if f.readers == nil {
		return f.File.ReadAt(p, off)
	}
	h, pooled := f.reader()
	n, err := h.ReadAt(p, off)
	if pooled {
		f.readers <- h
	}
	return n, err
}

// reader returns a free read handle, opening one while fewer than
// maxReadHandles exist, else waits for one. If opening fails it falls back to
// the main handle (pooled false).
func (f *osFile) reader() (*os.File, bool) {
	select {
	case h := <-f.readers:
		return h, true
	default:
	}
	if f.opened.Add(1) <= maxReadHandles {
		if h, err := os.Open(f.name); err == nil {
			return h, true
		}
	}
	f.opened.Add(-1)
	if f.opened.Load() == 0 {
		return f.File, false
	}
	return <-f.readers, true
}

// Close closes every handle. No read may be running.
func (f *osFile) Close() error {
	if f.readers != nil {
		for f.opened.Load() > 0 {
			_ = (<-f.readers).Close()
			f.opened.Add(-1)
		}
	}
	return f.File.Close()
}

func (f *osFile) Size() (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}
