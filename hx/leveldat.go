package hx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/hx/store"
	"github.com/helix-bedrock/world/mcdb/leveldat"
)

// level.dat is kept in world.hx, so it is saved in the same round as the
// chunks and players it goes with; each round that changes it also copies it
// to the world's level.dat file, which BDS tools read. A world without the
// record (just converted, or made by hand) is read from the file.

// LevelDatBytes reads level.dat as stored, header included, into dst[:0].
func (v view) LevelDatBytes(dst []byte) ([]byte, error) {
	b, err := v.get(v.w.meta, metaLevelDatKey, dst)
	if !errors.Is(err, store.ErrNotFound) {
		return b, err
	}
	f, err := os.ReadFile(v.w.LevelDatPath())
	if errors.Is(err, os.ErrNotExist) {
		return dst[:0], ErrNotFound
	}
	if err != nil {
		return dst[:0], fmt.Errorf("hx: read level.dat: %w", err)
	}
	return append(dst[:0], f...), nil
}

// LevelDat reads and parses level.dat.
func (v view) LevelDat() (*leveldat.LevelDat, error) {
	b, err := v.LevelDatBytes(nil)
	if err != nil {
		return nil, err
	}
	ld, err := leveldat.Read(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("hx: level.dat: %w", err)
	}
	return ld, nil
}

// PutLevelDat saves level.dat in the round.
func (r *Round) PutLevelDat(ld *leveldat.LevelDat) error {
	var buf bytes.Buffer
	if err := ld.Write(&buf); err != nil {
		return fmt.Errorf("hx: write level.dat: %w", err)
	}
	r.PutLevelDatBytes(buf.Bytes())
	return nil
}

// PutLevelDatBytes saves level.dat as given, header included.
func (r *Round) PutLevelDatBytes(b []byte) {
	r.b.Put(r.w.meta, KindMeta, metaLevelDatKey, b)
	r.levelDat = append(r.levelDat[:0], b...)
}

// writeFileAtomic replaces name with data: written to a temporary file in the
// same folder, forced to disk if sync, then renamed over name.
func writeFileAtomic(name string, data []byte, sync bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
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

// eachCompound calls fn with every LE NBT compound stored back to back in
// data, stopping at fn's first error.
func eachCompound(data []byte, fn func([]byte) error) error {
	var ferr error
	err := nbt.EachCompound(data, nbt.LittleEndian, func(c []byte) bool {
		ferr = fn(c)
		return ferr == nil
	})
	if ferr != nil {
		return ferr
	}
	return err
}

// Snapshot is a world as of one save round: every read method of World, on
// data that does not change while saves go on. For live backups and exports.
// Release it when done; files keep the space of replaced records until then.
type Snapshot struct {
	view
	sn *store.Snapshot
}

// Snapshot takes a snapshot of the newest committed round.
func (w *World) Snapshot() (*Snapshot, error) {
	sn, err := w.s.Snapshot()
	if err != nil {
		return nil, fmt.Errorf("hx: snapshot: %w", err)
	}
	return &Snapshot{view: view{w: w, snap: sn}, sn: sn}, nil
}

// Round is the save round the snapshot shows.
func (s *Snapshot) Round() uint64 { return s.sn.Round() }

// Release ends the snapshot.
func (s *Snapshot) Release() { s.sn.Release() }
