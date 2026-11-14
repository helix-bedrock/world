package convert

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/hx"
)

// maxLayers matches hx's limit on layers per sub chunk.
const maxLayers = 8

// subTranslator turns BDS sub chunks into hx sub chunks, reusing its memory.
type subTranslator struct {
	table *hx.BlockTable
	ids   []uint32
	enc   []byte
}

// translate returns the hx form of a BDS sub chunk value stored under index.
// Values it cannot give back byte for byte are kept raw. The layers point
// into value and into the translator's memory, valid until the next reset.
func (st *subTranslator) translate(value []byte, index int8) (hx.Sub, error) {
	raw := hx.Sub{Index: index, Raw: value}
	layers, ok, err := st.parse(value, index)
	if err != nil || !ok {
		return raw, err
	}
	s := hx.Sub{Index: index, Version: value[0], Layers: layers}
	st.enc, err = appendBDSSub(st.enc[:0], &s, st.table)
	if err != nil || !bytes.Equal(st.enc, value) {
		return raw, nil
	}
	return s, nil
}

// reset forgets the palettes of earlier sub chunks.
func (st *subTranslator) reset() { st.ids = st.ids[:0] }

// parse reads version 8 and 9 sub chunks with disk palettes. ok is false for
// anything else, which stays raw.
func (st *subTranslator) parse(v []byte, index int8) ([]hx.Layer, bool, error) {
	if len(v) < 2 || v[0] != 8 && v[0] != 9 {
		return nil, false, nil
	}
	count, p := int(v[1]), 2
	if v[0] == 9 {
		if len(v) < 3 || int8(v[2]) != index { //nolint:gosec // raw byte
			return nil, false, nil
		}
		p = 3
	}
	if count > maxLayers {
		return nil, false, nil
	}
	layers := make([]hx.Layer, 0, count)
	for range count {
		l, next, ok, err := st.parseLayer(v, p)
		if err != nil || !ok {
			return nil, ok, err
		}
		layers = append(layers, l)
		p = next
	}
	return layers, p == len(v), nil
}

func (st *subTranslator) parseLayer(v []byte, p int) (hx.Layer, int, bool, error) {
	if p >= len(v) || v[p]&1 != 0 {
		return hx.Layer{}, 0, false, nil
	}
	bits := int(v[p] >> 1)
	words := chunk.WordCount(bits)
	p++
	if words < 0 || len(v)-p < words*4 {
		return hx.Layer{}, 0, false, nil
	}
	l := hx.Layer{Bits: bits, Words: v[p : p+words*4]}
	p += words * 4
	n := 1
	if bits != 0 {
		if len(v)-p < 4 {
			return hx.Layer{}, 0, false, nil
		}
		n = int(binary.LittleEndian.Uint32(v[p:]))
		p += 4
	}
	if n < 1 || n > 4096 {
		return hx.Layer{}, 0, false, nil
	}
	start := len(st.ids)
	for range n {
		size, err := nbt.EachField(v[p:], nbt.LittleEndian, func(string, int, int) bool { return true })
		if err != nil {
			return hx.Layer{}, 0, false, nil //nolint:nilerr // not a palette this package reads: kept raw
		}
		id, err := st.table.Add(v[p : p+size])
		if err != nil {
			return hx.Layer{}, 0, false, err
		}
		st.ids = append(st.ids, id)
		p += size
	}
	l.Palette = st.ids[start:len(st.ids):len(st.ids)]
	return l, p, true, nil
}

// appendBDSSub appends a sub chunk as BDS stores it.
func appendBDSSub(dst []byte, s *hx.Sub, table *hx.BlockTable) ([]byte, error) {
	if s.Raw != nil {
		return append(dst, s.Raw...), nil
	}
	dst = append(dst, s.Version, byte(len(s.Layers))) //nolint:gosec // at most maxLayers
	if s.Version == 9 {
		dst = append(dst, byte(s.Index))
	}
	for _, l := range s.Layers {
		dst = append(dst, byte(l.Bits<<1)) //nolint:gosec // at most 16
		dst = append(dst, l.Words...)
		if l.Bits != 0 {
			dst = binary.LittleEndian.AppendUint32(dst, uint32(len(l.Palette))) //nolint:gosec // at most 4096
		}
		for _, id := range l.Palette {
			b, ok := table.Bytes(id)
			if !ok {
				return dst, fmt.Errorf("sub chunk %d: block table has no entry %d", s.Index, id)
			}
			dst = append(dst, b...)
		}
	}
	return dst, nil
}
