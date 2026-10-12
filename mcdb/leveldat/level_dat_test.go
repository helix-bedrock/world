package leveldat

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"slices"
	"testing"

	"github.com/helix-bedrock/protocol/nbt"
)

// TestUnknownFieldsSurvive checks that fields Data does not know, including
// nested ones, are written back unchanged after a load and save.
func TestUnknownFieldsSurvive(t *testing.T) {
	src := map[string]any{
		"LevelName":             "Test",
		"futureField":           int32(7),
		"abilities":             map[string]any{"flySpeed": float32(0.05), "futureAbility": uint8(1)},
		"experiments":           map[string]any{"gametest": uint8(1)},
		"NetworkVersion":        int32(2193),
		"lastOpenedWithVersion": []int32{1, 26, 50, 0, 0},
	}
	data, err := nbt.MarshalEncoding(src, nbt.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, header{StorageVersion: Version, FileLength: int32(len(data))})
	buf.Write(data)

	ld, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	var d Data
	if err := ld.Unmarshal(&d); err != nil {
		t.Fatal(err)
	}
	if d.LevelName != "Test" || d.NetworkVersion != 2193 || d.Abilities.FlySpeed != 0.05 {
		t.Fatalf("decoded %q %d %v", d.LevelName, d.NetworkVersion, d.Abilities.FlySpeed)
	}

	d.LevelName = "Renamed"
	if err := ld.Marshal(d); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "level.dat")
	if err := ld.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	back, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f := back.fields
	if f["LevelName"] != "Renamed" {
		t.Errorf("LevelName = %v", f["LevelName"])
	}
	if f["futureField"] != int32(7) {
		t.Errorf("futureField = %v", f["futureField"])
	}
	ab, _ := f["abilities"].(map[string]any)
	if ab["futureAbility"] != uint8(1) {
		t.Errorf("nested unknown field lost: %v", ab)
	}
	if v, _ := f["lastOpenedWithVersion"].([]int32); !slices.Equal(v, []int32{1, 26, 50, 0, 0}) {
		t.Errorf("lastOpenedWithVersion = %v", f["lastOpenedWithVersion"])
	}
}

func TestReadRejectsBadLength(t *testing.T) {
	for _, n := range []int32{-1, maxFileLength + 1, 100} {
		var buf bytes.Buffer
		_ = binary.Write(&buf, binary.LittleEndian, header{StorageVersion: Version, FileLength: n})
		if _, err := Read(&buf); err == nil {
			t.Errorf("length %d: no error", n)
		}
	}
}

func TestVersionArray(t *testing.T) {
	for in, want := range map[string][]int32{
		"1.26.50":     {1, 26, 50, 0, 0},
		"1":           {1, 0, 0, 0, 0},
		"1.2.3.4.5.6": {1, 2, 3, 4, 0},
		"":            {0, 0, 0, 0, 0},
	} {
		if got := versionArray(in); !slices.Equal(got, want) {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}

func FuzzRead(f *testing.F) {
	var d Data
	d.FillDefault()
	var ld LevelDat
	if err := ld.Marshal(d); err != nil {
		f.Fatal(err)
	}
	var buf bytes.Buffer
	if err := ld.Write(&buf); err != nil {
		f.Fatal(err)
	}
	f.Add(buf.Bytes())
	f.Fuzz(func(t *testing.T, b []byte) {
		ld, err := Read(bytes.NewReader(b))
		if err != nil {
			return
		}
		var d Data
		_ = ld.Unmarshal(&d)
	})
}
