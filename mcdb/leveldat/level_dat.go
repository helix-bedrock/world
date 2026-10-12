// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/world/mcdb/leveldat/level_dat.go
//
// Changed for Helix: the file keeps every field, including ones Data does not
// know, so saving never drops data written by newer versions; the header
// length is checked; WriteFile reports every error and replaces the file
// atomically.

package leveldat

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/helix-bedrock/protocol/nbt"
)

// maxFileLength bounds the NBT length a header may claim. Real level.dat
// files are a few KiB; the limit only stops corrupt headers from causing
// huge allocations.
const maxFileLength = 16 << 20

// LevelDat is a level.dat file: an 8-byte header and little-endian NBT.
type LevelDat struct {
	hdr header
	// fields holds every top-level field read from the file, so fields that
	// Data does not know survive a save.
	fields map[string]any
}

type header struct {
	StorageVersion int32
	FileLength     int32
}

// ReadFile reads a level.dat at a path and returns it.
func ReadFile(name string) (*LevelDat, error) {
	b, err := os.ReadFile(name) //nolint:gosec // the caller chooses which world to open
	if err != nil {
		return nil, fmt.Errorf("level.dat: read file: %w", err)
	}
	return Read(bytes.NewReader(b))
}

// Read reads a level.dat from r and returns it.
func Read(r io.Reader) (*LevelDat, error) {
	var ldat LevelDat
	if err := binary.Read(r, binary.LittleEndian, &ldat.hdr); err != nil {
		return nil, fmt.Errorf("level.dat: read header: %w", err)
	}
	if ldat.hdr.FileLength < 0 || ldat.hdr.FileLength > maxFileLength {
		return nil, fmt.Errorf("level.dat: invalid data length %d", ldat.hdr.FileLength)
	}
	data := make([]byte, ldat.hdr.FileLength)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("level.dat: read data: %w", err)
	}
	if err := nbt.UnmarshalEncoding(data, &ldat.fields, nbt.LittleEndian); err != nil {
		return nil, fmt.Errorf("level.dat: decode nbt: %w", err)
	}
	return &ldat, nil
}

// Unmarshal decodes the fields of the level.dat that dst knows into dst, a
// pointer to a struct such as Data. Fields dst does not have are kept in ld
// but not decoded.
func (ld *LevelDat) Unmarshal(dst any) error {
	t := reflect.TypeOf(dst)
	if t == nil || t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return errors.New("level.dat: Unmarshal needs a pointer to a struct")
	}
	known := filterKnown(ld.fields, t.Elem())
	b, err := nbt.MarshalEncoding(known, nbt.LittleEndian)
	if err != nil {
		return fmt.Errorf("level.dat: encode known fields: %w", err)
	}
	if err := nbt.UnmarshalEncoding(b, dst, nbt.LittleEndian); err != nil {
		return fmt.Errorf("level.dat: decode nbt: %w", err)
	}
	return nil
}

// Ver returns the storage version of the level.dat, or 0 if ld is the empty
// value.
func (ld *LevelDat) Ver() int {
	return int(ld.hdr.StorageVersion)
}

// Marshal stores the fields of src, a struct or map, in the level.dat. Fields
// already present that src does not set are kept. Marshal updates the storage
// version to the latest.
func (ld *LevelDat) Marshal(src any) error {
	b, err := nbt.MarshalEncoding(src, nbt.LittleEndian)
	if err != nil {
		return fmt.Errorf("level.dat: encode nbt: %w", err)
	}
	var m map[string]any
	if err := nbt.UnmarshalEncoding(b, &m, nbt.LittleEndian); err != nil {
		return fmt.Errorf("level.dat: decode encoded fields: %w", err)
	}
	if ld.fields == nil {
		ld.fields = make(map[string]any, len(m))
	}
	merge(ld.fields, m)
	ld.hdr.StorageVersion = Version
	return nil
}

// Write writes ld to w.
func (ld *LevelDat) Write(w io.Writer) error {
	fields := ld.fields
	if fields == nil {
		fields = map[string]any{}
	}
	data, err := nbt.MarshalEncoding(fields, nbt.LittleEndian)
	if err != nil {
		return fmt.Errorf("level.dat: encode nbt: %w", err)
	}
	hdr := header{StorageVersion: ld.hdr.StorageVersion, FileLength: int32(len(data))} //nolint:gosec // bounded by the fields we hold
	if err := binary.Write(w, binary.LittleEndian, hdr); err != nil {
		return fmt.Errorf("level.dat: write header: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("level.dat: write data: %w", err)
	}
	return nil
}

// WriteFile writes ld to a file at name. It writes a temporary file next to
// it and renames it into place, so a crash never leaves a half-written
// level.dat.
func (ld *LevelDat) WriteFile(name string) error {
	var buf bytes.Buffer
	if err := ld.Write(&buf); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".tmp*")
	if err != nil {
		return fmt.Errorf("level.dat: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("level.dat: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("level.dat: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("level.dat: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, name); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("level.dat: replace file: %w", err)
	}
	return nil
}

// filterKnown returns the entries of m that struct type t has a field for,
// recursing into nested structs, so a strict NBT decode into t succeeds.
func filterKnown(m map[string]any, t reflect.Type) map[string]any {
	out := make(map[string]any, len(m))
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if tag, ok := f.Tag.Lookup("nbt"); ok {
			tagName, _, _ := strings.Cut(tag, ",")
			if tagName == "-" {
				continue
			}
			if tagName != "" {
				name = tagName
			}
		}
		v, ok := m[name]
		if !ok {
			continue
		}
		if f.Type.Kind() == reflect.Struct {
			nested, ok := v.(map[string]any)
			if !ok {
				continue
			}
			v = filterKnown(nested, f.Type)
		}
		out[name] = v
	}
	return out
}

// merge copies src into dst, recursing into nested compounds so unknown
// nested fields in dst survive.
func merge(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				merge(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
}
