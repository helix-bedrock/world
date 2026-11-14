package hx

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/helix-bedrock/world/chunk"
)

// The terrain record: a format version byte, then sections, each a tag byte,
// a varint length and the section's bytes. A reader skips sections it does not
// know, so sections can be added without converting worlds.
const (
	terrainVersion = 1

	// secKey is a chunk key BDS writes, kept as stored: its tag byte, then
	// its value (version, finalisation, border blocks, blending data...).
	secKey byte = 1
	// secData3D is BDS's height map and 3D biomes, as stored.
	secData3D byte = 2
	// secSub is a sub chunk with block table palettes.
	secSub byte = 3
	// secSubRaw is a sub chunk kept in BDS's form, NBT palettes and all: old
	// versions, or any the converter could not translate exactly.
	secSubRaw byte = 4
)

// maxLayers is the most block layers a sub chunk may have.
const maxLayers = 8

var errTerrain = errors.New("hx: corrupt terrain record")

// ChunkKey is one of the small chunk keys BDS writes, kept as stored.
type ChunkKey struct {
	// Tag is the key's tag byte, such as ',' (0x2c) for the chunk version.
	Tag   byte
	Value []byte
}

// Terrain is a chunk's terrain record, decoded. Its slices point into the
// record it was decoded from, and are reused by the next DecodeTerrain into it.
type Terrain struct {
	Keys   []ChunkKey
	Data3D []byte
	Subs   []Sub

	layers   []Layer
	ids      []uint32
	palSpans []span // per entry of layers
	subSpans []span // per sub chunk with layers, in order
}

// Sub is one sub chunk.
type Sub struct {
	// Index is the sub chunk's Y index, as in BDS's key: -4 for the lowest
	// sub chunk of the Overworld.
	Index int8
	// Version is the BDS sub chunk version it was stored with (8 or 9).
	Version byte
	// Raw, when set, is the sub chunk in BDS's form, and Layers is empty.
	Raw    []byte
	Layers []Layer
}

// Layer is one block storage of a sub chunk.
type Layer struct {
	// Bits is the bits per index: 0, 1, 2, 3, 4, 5, 6, 8 or 16.
	Bits int
	// Words is the packed indices, little-endian uint32s, laid out as BDS and
	// the client store them.
	Words []byte
	// Palette is block table IDs.
	Palette []uint32
}

// Key returns the value of the chunk key with tag, if the record has one.
func (t *Terrain) Key(tag byte) ([]byte, bool) {
	for _, k := range t.Keys {
		if k.Tag == tag {
			return k.Value, true
		}
	}
	return nil, false
}

// AppendTerrain appends t's record to dst.
func AppendTerrain(dst []byte, t *Terrain) []byte {
	dst = append(dst, terrainVersion)
	for _, k := range t.Keys {
		dst = appendSection(dst, secKey, 1+len(k.Value))
		dst = append(dst, k.Tag)
		dst = append(dst, k.Value...)
	}
	if t.Data3D != nil {
		dst = appendSection(dst, secData3D, len(t.Data3D))
		dst = append(dst, t.Data3D...)
	}
	for i := range t.Subs {
		dst = appendSub(dst, &t.Subs[i])
	}
	return dst
}

func appendSection(dst []byte, tag byte, n int) []byte {
	dst = append(dst, tag)
	return binary.AppendUvarint(dst, uint64(n)) //nolint:gosec // lengths
}

func appendSub(dst []byte, s *Sub) []byte {
	if s.Raw != nil {
		dst = appendSection(dst, secSubRaw, 1+len(s.Raw))
		dst = append(dst, byte(s.Index))
		return append(dst, s.Raw...)
	}
	n := 3
	for _, l := range s.Layers {
		n += 1 + len(l.Words) + uvarintLen(uint64(len(l.Palette)))
		for _, id := range l.Palette {
			n += uvarintLen(uint64(id))
		}
	}
	dst = appendSection(dst, secSub, n)
	dst = append(dst, byte(s.Index), s.Version, byte(len(s.Layers))) //nolint:gosec // at most maxLayers
	for _, l := range s.Layers {
		dst = append(dst, byte(l.Bits)) //nolint:gosec // at most 16
		dst = append(dst, l.Words...)
		dst = binary.AppendUvarint(dst, uint64(len(l.Palette)))
		for _, id := range l.Palette {
			dst = binary.AppendUvarint(dst, uint64(id))
		}
	}
	return dst
}

func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// DecodeTerrain decodes a terrain record into t, reusing t's memory. Sections
// it does not know are skipped.
func DecodeTerrain(rec []byte, t *Terrain) error {
	t.Keys, t.Data3D, t.Subs = t.Keys[:0], nil, t.Subs[:0]
	t.layers, t.ids = t.layers[:0], t.ids[:0]
	t.palSpans, t.subSpans = t.palSpans[:0], t.subSpans[:0]
	if len(rec) == 0 || rec[0] != terrainVersion {
		return fmt.Errorf("%w: unknown version", errTerrain)
	}
	rest := rec[1:]
	for len(rest) > 0 {
		tag := rest[0]
		n, k := binary.Uvarint(rest[1:])
		if k <= 0 || n > uint64(len(rest)-1-k) {
			return fmt.Errorf("%w: section length", errTerrain)
		}
		body := rest[1+k : 1+k+int(n)] //nolint:gosec // checked above
		rest = rest[1+k+int(n):]       //nolint:gosec // checked above
		if err := t.section(tag, body); err != nil {
			return err
		}
	}
	t.linkLayers()
	return nil
}

func (t *Terrain) section(tag byte, body []byte) error {
	switch tag {
	case secKey:
		if len(body) == 0 {
			return fmt.Errorf("%w: empty chunk key", errTerrain)
		}
		t.Keys = append(t.Keys, ChunkKey{Tag: body[0], Value: body[1:]})
	case secData3D:
		t.Data3D = body
	case secSubRaw:
		if len(body) == 0 {
			return fmt.Errorf("%w: empty sub chunk", errTerrain)
		}
		t.Subs = append(t.Subs, Sub{Index: int8(body[0]), Raw: body[1:]}) //nolint:gosec // raw byte
	case secSub:
		return t.decodeSub(body)
	}
	return nil
}

// decodeSub reads a sub chunk section. Its layers go to t.layers and their
// palettes to t.ids, which may move while they grow: spans record where each
// sub's layers and each layer's palette are, and linkLayers points the slices
// at them once every section is read.
func (t *Terrain) decodeSub(b []byte) error {
	if len(b) < 3 || b[2] > maxLayers {
		return fmt.Errorf("%w: sub chunk header", errTerrain)
	}
	t.subSpans = append(t.subSpans, span{start: len(t.layers), n: int(b[2])})
	t.Subs = append(t.Subs, Sub{Index: int8(b[0]), Version: b[1]}) //nolint:gosec // raw byte
	count := int(b[2])
	b = b[3:]
	for range count {
		var err error
		if b, err = t.decodeLayer(b); err != nil {
			return err
		}
	}
	if len(b) != 0 {
		return fmt.Errorf("%w: sub chunk trailing bytes", errTerrain)
	}
	return nil
}

// decodeLayer reads one layer and returns what follows it.
func (t *Terrain) decodeLayer(b []byte) ([]byte, error) {
	if len(b) == 0 {
		return nil, fmt.Errorf("%w: layer header", errTerrain)
	}
	bits := int(b[0])
	words := chunk.WordCount(bits)
	if words < 0 || len(b) < 1+words*4 {
		return nil, fmt.Errorf("%w: layer of %d bits", errTerrain, bits)
	}
	l := Layer{Bits: bits, Words: b[1 : 1+words*4]}
	b = b[1+words*4:]
	n, k := binary.Uvarint(b)
	if k <= 0 || n == 0 || n > 4096 {
		return nil, fmt.Errorf("%w: palette length", errTerrain)
	}
	b = b[k:]
	start := len(t.ids)
	for range n {
		id, k := binary.Uvarint(b)
		if k <= 0 || id > 1<<32-1 {
			return nil, fmt.Errorf("%w: palette entry", errTerrain)
		}
		t.ids = append(t.ids, uint32(id))
		b = b[k:]
	}
	t.layers = append(t.layers, l)
	t.palSpans = append(t.palSpans, span{start: start, n: int(n)}) //nolint:gosec // at most 4096
	return b, nil
}

// span is a range of t.layers or t.ids.
type span struct{ start, n int }

// linkLayers points every sub at its layers and every layer at its palette.
func (t *Terrain) linkLayers() {
	for i, sp := range t.palSpans {
		t.layers[i].Palette = t.ids[sp.start : sp.start+sp.n]
	}
	j := 0
	for i := range t.Subs {
		if t.Subs[i].Raw != nil {
			continue
		}
		sp := t.subSpans[j]
		t.Subs[i].Layers = t.layers[sp.start : sp.start+sp.n]
		j++
	}
}
