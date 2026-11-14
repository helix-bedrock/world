package hx

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/helix-bedrock/world/hx/store"
)

// Live backups (API_DESIGN "World data redesign", section 5). A backup reads
// a snapshot, so the server keeps playing and saving while it runs; records
// are copied as stored, checksums and all, never decoded. A full backup is
// itself a Helix world. An incremental one holds the records written since an
// earlier backup's round, plus, per file, the list of keys the file held, so
// a restore also removes what was deleted.

// BackupInfo describes a finished backup. A target stores it with the backup.
type BackupInfo struct {
	// Format is the world format version.
	Format int `json:"format"`
	// Round is the save round the backup shows.
	Round uint64 `json:"round"`
	// Since is the round of the backup this one builds on, 0 for a full one.
	Since uint64 `json:"since"`
	// Time is when the snapshot was taken.
	Time time.Time `json:"time"`
	// Records and Bytes count the records copied and their stored bytes.
	Records int64 `json:"records"`
	Bytes   int64 `json:"bytes"`
}

// BackupOptions configures a backup. The zero value makes a full backup as
// fast as the disk allows.
type BackupOptions struct {
	// Since makes the backup incremental: only records written after this
	// round (an earlier backup's BackupInfo.Round) are copied.
	Since uint64
	// Rate caps the stored bytes copied per second, so a backup never slows
	// chunk loading. 0 is no cap.
	Rate int64
	// Progress, if set, is called after each file and every progressEvery
	// records, from the backup's goroutine.
	Progress func(BackupProgress)
}

// BackupProgress is how far a backup has got.
type BackupProgress struct {
	Files, FilesDone int
	Records, Bytes   int64
}

const progressEvery = 4096

// BackupTarget receives a backup: a folder (DirTarget), an archive
// (ArchiveWriter), or a plugin's own (cloud storage). Its methods are called
// from one goroutine: Record for each record of a file, then, for an
// incremental backup, Keys for that file; then File for each plain file; then
// Finish. If the backup fails, Finish is not called: the target should not
// count as a backup.
type BackupTarget interface {
	// Record receives one record of file (a store path such as
	// "overworld/r.0.-1.hx"): body reads exactly h.Stored stored bytes, valid
	// until Record returns.
	Record(file string, key []byte, h store.RawHeader, body io.Reader) error
	// Keys receives, for an incremental backup, every key file held, each as
	// a uvarint length and the key's bytes. A file with no Keys call held no
	// keys.
	Keys(file string, keys []byte) error
	// File receives a plain file of the world folder (level.dat, BDS's own
	// files), name slash-separated: size bytes from r.
	File(name string, r io.Reader, size int64) error
	// Finish completes the backup.
	Finish(info BackupInfo) error
}

// Backup takes a snapshot and backs it up to t. It may run from any
// goroutine while the world is played and saved; ctx cancels it.
func (w *World) Backup(ctx context.Context, t BackupTarget, opts BackupOptions) (BackupInfo, error) {
	sn, err := w.Snapshot()
	if err != nil {
		return BackupInfo{}, err
	}
	defer sn.Release()
	return sn.Backup(ctx, t, opts)
}

// Backup backs the snapshot up to t.
func (s *Snapshot) Backup(ctx context.Context, t BackupTarget, opts BackupOptions) (BackupInfo, error) {
	if opts.Since > s.Round() {
		return BackupInfo{}, fmt.Errorf("hx: backup since round %d, after the snapshot's %d", opts.Since, s.Round())
	}
	files, plain, err := worldFiles(s.w.dir)
	if err != nil {
		return BackupInfo{}, err
	}
	b := backuper{
		s: s, t: t, opts: opts, ctx: ctx, start: time.Now(),
		info:  BackupInfo{Format: formatVersion, Round: s.Round(), Since: opts.Since, Time: time.Now().UTC()},
		files: len(files),
	}
	for _, f := range files {
		if err := b.file(f); err != nil {
			return BackupInfo{}, err
		}
	}
	if err := b.plainFiles(plain); err != nil {
		return BackupInfo{}, err
	}
	if err := t.Finish(b.info); err != nil {
		return BackupInfo{}, fmt.Errorf("hx: finish backup: %w", err)
	}
	return b.info, nil
}

// worldFiles lists a world folder's store files and its plain files (both
// slash-separated, sorted), leaving out the journal and temporary files.
func worldFiles(dir string) (stores, plain []string, err error) {
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == store.JournalDir {
				return filepath.SkipDir
			}
			return nil
		}
		switch {
		case !d.Type().IsRegular(), strings.HasPrefix(d.Name(), "."):
		case strings.HasSuffix(rel, ".hx"):
			stores = append(stores, rel)
		default:
			plain = append(plain, rel)
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("hx: list world files: %w", err)
	}
	slices.Sort(stores)
	slices.Sort(plain)
	return stores, plain, nil
}

type backuper struct {
	s     *Snapshot
	t     BackupTarget
	opts  BackupOptions
	ctx   context.Context //nolint:containedctx // one backup's lifetime
	start time.Time
	info  BackupInfo
	files int
	done  int
	buf   []byte
	keys  []byte
	rd    bytes.Reader
}

// file copies one store file's records.
func (b *backuper) file(path string) error {
	id, err := b.s.w.s.File(path)
	if err != nil {
		return fmt.Errorf("hx: backup %s: %w", path, err)
	}
	b.keys = b.keys[:0]
	var ferr error
	err = b.s.sn.Range(id, func(key []byte, info store.Info) bool {
		if b.opts.Since > 0 {
			b.keys = binary.AppendUvarint(b.keys, uint64(len(key)))
			b.keys = append(b.keys, key...)
		}
		if info.Round > b.opts.Since {
			ferr = b.record(id, path, key, info)
		}
		return ferr == nil
	})
	if err == nil {
		err = ferr
	}
	if err == nil && b.opts.Since > 0 && len(b.keys) > 0 {
		err = b.t.Keys(path, b.keys)
	}
	if err != nil {
		return fmt.Errorf("hx: backup %s: %w", path, err)
	}
	b.done++
	b.progress()
	return nil
}

// record copies one record, streamed if it is big.
func (b *backuper) record(id store.FileID, path string, key []byte, info store.Info) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	var stored int64
	if info.Stored < streamRecordAt {
		h, body, err := b.s.sn.ReadRaw(id, key, b.buf)
		b.buf = body
		if err != nil {
			return err
		}
		b.rd.Reset(body)
		if err := b.t.Record(path, key, h, &b.rd); err != nil {
			return err
		}
		stored = h.Stored
	} else {
		h, rc, err := b.s.sn.OpenRaw(id, key)
		if err != nil {
			return err
		}
		err = b.t.Record(path, key, h, rc)
		_ = rc.Close()
		if err != nil {
			return err
		}
		stored = h.Stored
	}
	b.info.Records++
	b.info.Bytes += stored
	if b.info.Records%progressEvery == 0 {
		b.progress()
	}
	return b.pace()
}

// streamRecordAt is the stored size from which records are streamed rather
// than read whole.
const streamRecordAt = 1 << 20

// pace sleeps while the backup is ahead of its rate.
func (b *backuper) pace() error {
	if b.opts.Rate <= 0 {
		return nil
	}
	want := time.Duration(float64(b.info.Bytes) / float64(b.opts.Rate) * float64(time.Second))
	ahead := want - time.Since(b.start)
	if ahead <= time.Millisecond {
		return nil
	}
	t := time.NewTimer(ahead)
	defer t.Stop()
	select {
	case <-b.ctx.Done():
		return b.ctx.Err()
	case <-t.C:
		return nil
	}
}

func (b *backuper) progress() {
	if b.opts.Progress != nil {
		b.opts.Progress(BackupProgress{Files: b.files, FilesDone: b.done, Records: b.info.Records, Bytes: b.info.Bytes})
	}
}

// plainFiles copies the files that are not record stores. level.dat comes
// from the snapshot, so it matches the records.
func (b *backuper) plainFiles(names []string) error {
	ld, err := b.s.LevelDatBytes(nil)
	switch {
	case err == nil:
		b.rd.Reset(ld)
		if err := b.t.File(LevelDat, &b.rd, int64(len(ld))); err != nil {
			return fmt.Errorf("hx: backup level.dat: %w", err)
		}
	case !errors.Is(err, ErrNotFound):
		return fmt.Errorf("hx: backup level.dat: %w", err)
	}
	for _, name := range names {
		if name == LevelDat || name == BackupInfoFile {
			continue
		}
		if err := b.plainFile(name); err != nil {
			return fmt.Errorf("hx: backup %s: %w", name, err)
		}
	}
	return nil
}

func (b *backuper) plainFile(name string) error {
	f, err := os.Open(filepath.Join(b.s.w.dir, filepath.FromSlash(name)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	return b.t.File(name, io.LimitReader(f, st.Size()), st.Size())
}
