package hx

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/helix-bedrock/world/hx/store"
)

// dumpWorld reads every record of a world folder, decoded, by file and key,
// opening it read-only (so also while it is open elsewhere).
func dumpWorld(t *testing.T, dir string) map[string]map[string]string {
	t.Helper()
	s, err := store.Open(dir, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	files, _, err := worldFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]string{}
	for _, f := range files {
		if f == BackupKeysFile {
			continue
		}
		id, err := s.File(f)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		err = s.Range(id, func(key []byte, _ store.Info) bool {
			v, err := s.Get(id, key, nil)
			if err != nil {
				t.Fatal(err)
			}
			m[string(key)] = string(v)
			return true
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(m) > 0 {
			out[f] = m
		}
	}
	return out
}

func sameDump(t *testing.T, what string, a, b map[string]map[string]string) {
	t.Helper()
	if maps.EqualFunc(a, b, maps.Equal) {
		return
	}
	for f := range a {
		if !maps.Equal(a[f], b[f]) {
			t.Fatalf("%s: %s differs: %d keys vs %d", what, f, len(a[f]), len(b[f]))
		}
	}
	t.Fatalf("%s: files differ: %d vs %d", what, len(a), len(b))
}

func sameFile(t *testing.T, a, b string) {
	t.Helper()
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	if err1 != nil || err2 != nil || !bytes.Equal(x, y) {
		t.Fatalf("%s and %s differ (%v, %v)", a, b, err1, err2)
	}
}

func TestBackup(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "world")
	w := openTestWorld(t, dir)
	defer w.Close()
	if err := os.MkdirAll(filepath.Join(dir, "bds"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bds", "levelname.txt"), []byte("Test"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := filledChunk(newTestRegistry())
	a, b, gone := ChunkPos{0, 0}, ChunkPos{-1, -33}, ChunkPos{31, 32}
	r := w.NewRound()
	for _, pos := range []ChunkPos{a, b, gone} {
		if err := r.PutChunk(Overworld, pos, c, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.PutChunk(Nether, a, c, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.PutObjects(Overworld, a, testObjects(t)); err != nil {
		t.Fatal(err)
	}
	r.PutLevel("scoreboard", []byte("board"))
	r.PutLevel("portals", []byte("p"))
	r.PutLevelDatBytes([]byte("level.dat 1"))
	if _, err := w.Commit(r); err != nil {
		t.Fatal(err)
	}
	first := dumpWorld(t, dir)

	// The snapshot is taken before the next round; the backup made from it
	// after that round must still show round 1.
	sn, err := w.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	c2 := filledChunk(newTestRegistry())
	c2.SetBlock(1, 1, 1, 0, 5)
	if err := r.PutChunk(Overworld, b, c2, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteChunk(Overworld, gone); err != nil {
		t.Fatal(err)
	}
	if err := r.PutObjects(Overworld, a, &ObjectsSource{BlockEntities: smallCompound(t, "Sign")}); err != nil {
		t.Fatal(err)
	}
	r.DeleteLevel("portals")
	r.PutLevel("villages", []byte("v"))
	r.PutLevelDatBytes([]byte("level.dat 2"))
	if _, err := w.Commit(r); err != nil {
		t.Fatal(err)
	}

	full := filepath.Join(root, "full")
	d, err := NewDirTarget(full, store.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	var progress int
	info1, err := sn.Backup(context.Background(), d, BackupOptions{Progress: func(BackupProgress) { progress++ }})
	sn.Release()
	if err != nil {
		t.Fatal(err)
	}
	if info1.Since != 0 || info1.Records == 0 || progress == 0 {
		t.Fatalf("full backup: %+v, %d progress calls", info1, progress)
	}
	sameDump(t, "full backup", first, dumpWorld(t, full))
	if got, _ := os.ReadFile(filepath.Join(full, LevelDat)); string(got) != "level.dat 1" {
		t.Fatalf("backup level.dat %q", got)
	}
	sameFile(t, filepath.Join(dir, "bds", "levelname.txt"), filepath.Join(full, "bds", "levelname.txt"))

	// Incremental, to a folder and to an archive.
	inc := filepath.Join(root, "inc")
	d, err = NewDirTarget(inc, store.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	info2, err := w.Backup(context.Background(), d, BackupOptions{Since: info1.Round})
	if err != nil {
		t.Fatal(err)
	}
	if info2.Records >= info1.Records+5 || info2.Since != info1.Round {
		t.Fatalf("incremental backup copied %d records (full %d)", info2.Records, info1.Records)
	}
	var arch bytes.Buffer
	if _, err := w.Backup(context.Background(), NewArchiveWriter(&arch), BackupOptions{Since: info1.Round, Rate: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	unpacked := filepath.Join(root, "unpacked")
	d, err = NewDirTarget(unpacked, store.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReadArchive(bytes.NewReader(arch.Bytes()), d); err != nil {
		t.Fatal(err)
	}

	now := dumpWorld(t, dir)
	for i, chain := range [][]string{{full}, {full, inc}, {full, unpacked}} {
		dst := filepath.Join(root, "restored", string(rune('a'+i)))
		if err := Restore(dst, store.Options{NoSync: true}, chain...); err != nil {
			t.Fatal(err)
		}
		want := now
		if len(chain) == 1 {
			want = first
		}
		sameDump(t, "restore "+filepath.Base(chain[len(chain)-1]), want, dumpWorld(t, dst))
	}
	sameFile(t, filepath.Join(dir, LevelDat), filepath.Join(root, "restored", "b", LevelDat))
	if err := Restore(filepath.Join(root, "bad"), store.Options{}, inc); err == nil {
		t.Fatal("restore from an incremental backup alone worked")
	}

	// A damaged archive is refused.
	bad := bytes.Clone(arch.Bytes())
	bad[len(bad)/2] ^= 0xff
	d, err = NewDirTarget(filepath.Join(root, "bad-archive"), store.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := ReadArchive(bytes.NewReader(bad), d); err == nil {
		t.Fatal("damaged archive read without error")
	}
	_ = d.Close()

	// The full backup opens read-only as a world, and chunks go back into the
	// running world from it.
	bw, err := Open(full, Config{Blocks: newTestRegistry(), Store: store.Options{ReadOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer bw.Close()
	if _, _, err := bw.LoadChunk(Overworld, gone, nil, &Terrain{}); err != nil {
		t.Fatal(err)
	}
	r2 := bw.NewRound()
	r2.PutLevel("x", nil)
	if _, err := bw.Commit(r2); !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("commit to a read-only world: %v", err)
	}
	bsn, err := bw.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer bsn.Release()
	for _, pos := range []ChunkPos{a, b, gone} {
		if err := r.RestoreChunk(bsn, Overworld, pos); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Commit(r); err != nil {
		t.Fatal(err)
	}
	got := dumpWorld(t, dir)
	for _, f := range []string{"overworld/r.0.0.hx", "overworld/r.-1.-2.hx", "overworld/r.0.1.hx"} {
		if !maps.Equal(got[f], first[f]) {
			t.Fatalf("%s after RestoreChunk: %d keys, backup %d", f, len(got[f]), len(first[f]))
		}
	}
	sameObjects(t, w.view, Overworld, a, testObjects(t))
}
