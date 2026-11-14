package hx

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/helix-bedrock/world/hx/store"
)

const (
	// BackupInfoFile is the BackupInfo of a backup folder, written last: a
	// folder without it is not a finished backup.
	BackupInfoFile = "backup.json"
	// BackupKeysFile holds an incremental backup's key lists, one record per
	// world file, keyed by its path.
	BackupKeysFile = "backup.hx"
	// KindBackupKeys is a key list record in BackupKeysFile.
	KindBackupKeys byte = 8
)

// defaultBackupRound is how many bytes a DirTarget gathers per commit.
const defaultBackupRound = 64 << 20

// DirTarget writes a backup to a folder: a full backup is a Helix world that
// opens as one (read-only, Config.Store.ReadOnly, for rollback tools).
type DirTarget struct {
	dir string
	s   *store.Store
	b   store.Batch
	ids map[string]store.FileID
	buf []byte
	// sync forces the folder to disk at Finish.
	sync bool
}

// NewDirTarget makes a target writing to dir, which must be empty or not
// exist. opts configures the folder's record store.
func NewDirTarget(dir string, opts store.Options) (*DirTarget, error) {
	ents, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("hx: backup folder: %w", err)
	}
	if len(ents) > 0 {
		return nil, fmt.Errorf("hx: backup folder %s is not empty", dir)
	}
	opts.ReadOnly = false
	s, err := store.Open(dir, opts)
	if err != nil {
		return nil, fmt.Errorf("hx: backup folder: %w", err)
	}
	return &DirTarget{dir: dir, s: s, ids: map[string]store.FileID{}, sync: !opts.NoSync}, nil
}

func (d *DirTarget) file(path string) (store.FileID, error) {
	if id, ok := d.ids[path]; ok {
		return id, nil
	}
	id, err := d.s.File(path)
	if err != nil {
		return 0, err
	}
	d.ids[path] = id
	return id, nil
}

// Record implements BackupTarget.
func (d *DirTarget) Record(file string, key []byte, h store.RawHeader, body io.Reader) error {
	id, err := d.file(file)
	if err != nil {
		return err
	}
	if h.Stored >= streamRecordAt {
		d.b.PutRawStream(id, key, h, func() (io.ReadCloser, error) { return io.NopCloser(body), nil })
		return d.commit()
	}
	if int64(cap(d.buf)) < h.Stored {
		d.buf = make([]byte, h.Stored)
	}
	d.buf = d.buf[:h.Stored]
	if _, err := io.ReadFull(body, d.buf); err != nil {
		return fmt.Errorf("hx: read record: %w", err)
	}
	d.b.PutRaw(id, key, h, d.buf)
	if d.b.Size() >= defaultBackupRound {
		return d.commit()
	}
	return nil
}

// Keys implements BackupTarget.
func (d *DirTarget) Keys(file string, keys []byte) error {
	id, err := d.file(BackupKeysFile)
	if err != nil {
		return err
	}
	d.b.Put(id, KindBackupKeys, []byte(file), keys)
	if d.b.Size() >= defaultBackupRound {
		return d.commit()
	}
	return nil
}

// File implements BackupTarget.
func (d *DirTarget) File(name string, r io.Reader, size int64) error {
	if !validPlainName(name) {
		return fmt.Errorf("hx: backup file name %q", name)
	}
	return copyToFile(filepath.Join(d.dir, filepath.FromSlash(name)), r, size, d.sync)
}

// Finish implements BackupTarget: the records are committed, the store
// closed and the BackupInfo written.
func (d *DirTarget) Finish(info BackupInfo) error {
	if err := d.commit(); err != nil {
		return err
	}
	if err := d.Close(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(d.dir, BackupInfoFile), b, d.sync)
}

// Close closes the folder's store; after a failed backup, the folder may be
// deleted.
func (d *DirTarget) Close() error {
	if d.s == nil {
		return nil
	}
	err := d.s.Close()
	d.s = nil
	return err
}

func (d *DirTarget) commit() error {
	if d.b.Len() == 0 {
		return nil
	}
	_, err := d.s.Commit(&d.b)
	d.b.Reset()
	if err != nil {
		return fmt.Errorf("hx: write backup: %w", err)
	}
	return nil
}

// validPlainName reports whether a plain file's name, which may come from an
// archive, stays inside the folder and is not one of the store's own files.
func validPlainName(name string) bool {
	return filepath.IsLocal(name) && !strings.Contains(name, "\\") && !strings.HasSuffix(name, ".hx") &&
		name != BackupInfoFile && !strings.HasPrefix(name, store.JournalDir+"/")
}

// copyToFile writes size bytes from r to name, through a temporary file.
func copyToFile(name string, r io.Reader, size int64, sync bool) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	n, err := io.Copy(tmp, io.LimitReader(r, size))
	if err == nil && n != size {
		err = fmt.Errorf("hx: %s: got %d bytes of %d", name, n, size)
	}
	if err == nil && sync {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), name)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// ReadBackupInfo reads the BackupInfo of a backup folder; a folder without
// one is not a finished backup.
func ReadBackupInfo(dir string) (BackupInfo, error) {
	b, err := os.ReadFile(filepath.Join(dir, BackupInfoFile))
	if err != nil {
		return BackupInfo{}, fmt.Errorf("hx: %s is not a finished backup: %w", dir, err)
	}
	var info BackupInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return BackupInfo{}, fmt.Errorf("hx: %s: %w", BackupInfoFile, err)
	}
	return info, nil
}

// Restore builds a world in dst, which must be empty or not exist, from a
// full backup folder and the incremental ones made after it, in order: each
// one's Since must be the round of the one before. The world is offline
// while this runs; to put chunks back into a running world, use
// Round.RestoreChunk.
func Restore(dst string, opts store.Options, backups ...string) error {
	if len(backups) == 0 {
		return errors.New("hx: restore: no backups given")
	}
	infos := make([]BackupInfo, len(backups))
	for i, dir := range backups {
		info, err := ReadBackupInfo(dir)
		if err != nil {
			return err
		}
		switch {
		case i == 0 && info.Since != 0:
			return fmt.Errorf("hx: restore: %s is incremental; the first backup must be full", dir)
		case i > 0 && info.Since != infos[i-1].Round:
			return fmt.Errorf("hx: restore: %s builds on round %d, not %d", dir, info.Since, infos[i-1].Round)
		}
		infos[i] = info
	}
	d, err := NewDirTarget(dst, opts)
	if err != nil {
		return err
	}
	defer d.Close()
	written := map[string]bool{}
	for i, dir := range backups {
		if err := d.apply(dir, infos[i], written, opts); err != nil {
			return fmt.Errorf("hx: restore %s: %w", dir, err)
		}
	}
	return d.Close()
}

// apply copies one backup's records and files into the target; for an
// incremental backup it then removes the keys its lists leave out.
func (d *DirTarget) apply(dir string, info BackupInfo, written map[string]bool, opts store.Options) error {
	opts.ReadOnly = true
	src, err := store.Open(dir, opts)
	if err != nil {
		return err
	}
	defer src.Close()
	sn, err := src.Snapshot()
	if err != nil {
		return err
	}
	defer sn.Release()
	files, plain, err := worldFiles(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if f == BackupKeysFile {
			continue
		}
		if err := d.copyFile(sn, src, f); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		written[f] = true
	}
	if err := d.commit(); err != nil {
		return err
	}
	if info.Since > 0 {
		if err := d.dropDeleted(sn, src, written); err != nil {
			return err
		}
	}
	for _, name := range plain {
		if name == BackupInfoFile {
			continue
		}
		if err := d.copyPlain(dir, name); err != nil {
			return err
		}
	}
	return nil
}

func (d *DirTarget) copyFile(sn *store.Snapshot, src *store.Store, path string) error {
	id, err := src.File(path)
	if err != nil {
		return err
	}
	var buf []byte
	var ferr error
	err = sn.Range(id, func(key []byte, info store.Info) bool {
		if info.Stored >= streamRecordAt {
			h, rc, err := sn.OpenRaw(id, key)
			if err != nil {
				ferr = err
				return false
			}
			ferr = d.Record(path, key, h, rc)
			_ = rc.Close()
			return ferr == nil
		}
		var h store.RawHeader
		h, buf, ferr = sn.ReadRaw(id, key, buf)
		if ferr == nil {
			ferr = d.putRaw(path, key, h, buf)
		}
		return ferr == nil
	})
	if err != nil {
		return err
	}
	return ferr
}

// putRaw is Record for a body already in memory.
func (d *DirTarget) putRaw(file string, key []byte, h store.RawHeader, body []byte) error {
	id, err := d.file(file)
	if err != nil {
		return err
	}
	d.b.PutRaw(id, key, h, body)
	if d.b.Size() >= defaultBackupRound {
		return d.commit()
	}
	return nil
}

// dropDeleted removes from every file restored so far the keys the
// incremental backup's lists say it no longer held.
func (d *DirTarget) dropDeleted(sn *store.Snapshot, src *store.Store, written map[string]bool) error {
	kf, err := src.File(BackupKeysFile)
	if err != nil {
		return err
	}
	var list []byte
	for path := range written {
		list, err = sn.Get(kf, []byte(path), list)
		if errors.Is(err, store.ErrNotFound) {
			list, err = list[:0], nil
		}
		if err != nil {
			return err
		}
		keep, err := keySet(list)
		if err != nil {
			return fmt.Errorf("key list of %s: %w", path, err)
		}
		id, err := d.file(path)
		if err != nil {
			return err
		}
		err = d.s.Range(id, func(key []byte, _ store.Info) bool {
			if !keep[string(key)] {
				d.b.Delete(id, key)
			}
			return true
		})
		if err != nil {
			return err
		}
	}
	return d.commit()
}

// keySet reads a key list (uvarint length, bytes, repeated).
func keySet(list []byte) (map[string]bool, error) {
	set := map[string]bool{}
	for len(list) > 0 {
		n, k := binary.Uvarint(list)
		if k <= 0 || n > uint64(len(list)-k) {
			return nil, errors.New("corrupt key list")
		}
		set[string(list[k:k+int(n)])] = true //nolint:gosec // checked against len
		list = list[k+int(n):]               //nolint:gosec // checked against len
	}
	return set, nil
}

func (d *DirTarget) copyPlain(dir, name string) error {
	f, err := os.Open(filepath.Join(dir, filepath.FromSlash(name)))
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return d.File(name, f, st.Size())
}

// RestoreChunk puts a chunk back as from shows it (a snapshot of a backup
// world, or of this world): its terrain, objects and payloads, copied as
// stored; a chunk from does not hold is deleted. from must stay held until the
// round is committed. Its block table must be this world's or an older copy of
// it (a backup of this world), so block IDs mean the same.
func (r *Round) RestoreChunk(from *Snapshot, dim Dimension, pos ChunkPos) error {
	if err := r.checkRestoreSource(from.w); err != nil {
		return err
	}
	src, err := from.w.region(dim, pos)
	if err != nil {
		return err
	}
	dst, err := r.w.region(dim, pos)
	if err != nil {
		return err
	}
	tk, ok := chunkKey(pos, partTerrain), chunkKey(pos, partObjects)
	if err := r.copyRaw(from, src, dst, tk[:]); err != nil {
		return err
	}
	if err := r.copyRaw(from, src, dst, ok[:]); err != nil {
		return err
	}
	n, err := from.storedPayloads(dim, pos, &r.scratch)
	if err != nil {
		return err
	}
	old, err := r.w.storedPayloads(dim, pos, &r.scratch)
	if err != nil {
		return err
	}
	for i := uint32(0); i < max(n, old); i++ {
		pk := payloadKey(pos, i)
		if i >= n {
			r.b.Delete(dst, pk[:])
			continue
		}
		if err := r.copyRaw(from, src, dst, pk[:]); err != nil {
			return err
		}
	}
	return nil
}

// checkRestoreSource checks once per source world that its block table is a
// prefix of this world's.
func (r *Round) checkRestoreSource(src *World) error {
	if r.restoreFrom == src {
		return nil
	}
	a, b := src.table, r.w.table
	if a.Len() > b.Len() {
		return fmt.Errorf("hx: restore: the source's block table has %d entries, this world's %d: not a backup of this world", a.Len(), b.Len())
	}
	for i := range uint32(a.Len()) { //nolint:gosec // table sizes
		x, _ := a.Bytes(i)
		y, _ := b.Bytes(i)
		if !bytes.Equal(x, y) {
			return fmt.Errorf("hx: restore: block %d differs: not a backup of this world", i)
		}
	}
	r.restoreFrom = src
	return nil
}

// copyRaw copies one record as stored, or deletes it in dst if from has none.
func (r *Round) copyRaw(from *Snapshot, src, dst store.FileID, key []byte) error {
	h, rc, err := from.sn.OpenRaw(src, key)
	if errors.Is(err, store.ErrNotFound) {
		r.b.Delete(dst, key)
		return nil
	}
	if err != nil {
		return err
	}
	if h.Stored >= streamRecordAt {
		_ = rc.Close()
		k := append([]byte(nil), key...)
		r.b.PutRawStream(dst, key, h, func() (io.ReadCloser, error) {
			_, rc, err := from.sn.OpenRaw(src, k)
			return rc, err
		})
		return nil
	}
	if int64(cap(r.buf)) < h.Stored {
		r.buf = make([]byte, h.Stored)
	}
	r.buf = r.buf[:h.Stored]
	_, err = io.ReadFull(rc, r.buf)
	if err == nil {
		// Read to the end so the checksum is checked.
		_, err = rc.Read(nil)
		if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	_ = rc.Close()
	if err != nil {
		return err
	}
	r.b.PutRaw(dst, key, h, r.buf)
	return nil
}
