package chunk

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/helix-bedrock/world/cube"
)

// testRegistry is a small BlockRegistry: air plus a few named blocks, some
// with properties.
type testRegistry struct {
	names []string
	props []map[string]any
	ids   map[string]uint32
}

func stateKey(name string, props map[string]any) string {
	ks := make([]string, 0, len(props))
	for k, v := range props {
		ks = append(ks, fmt.Sprintf("%s=%T:%v", k, v, v))
	}
	sort.Strings(ks)
	return name + "{" + strings.Join(ks, ",") + "}"
}

func newTestRegistry() *testRegistry {
	r := &testRegistry{ids: map[string]uint32{}}
	add := func(name string, props map[string]any) {
		if props == nil {
			props = map[string]any{}
		}
		r.ids[stateKey(name, props)] = uint32(len(r.names))
		r.names = append(r.names, name)
		r.props = append(r.props, props)
	}
	add("minecraft:air", nil)
	add("minecraft:stone", nil)
	add("minecraft:dirt", map[string]any{"dirt_type": "normal"})
	add("minecraft:water", map[string]any{"liquid_depth": int32(0)})
	for i := range 40 {
		add("test:block", map[string]any{"n": int32(i)})
	}
	return r
}

func (r *testRegistry) BlockCount() int      { return len(r.names) }
func (r *testRegistry) AirRuntimeID() uint32 { return 0 }
func (r *testRegistry) RuntimeIDToState(id uint32) (string, map[string]any, bool) {
	if int(id) >= len(r.names) {
		return "", nil, false
	}
	return r.names[id], r.props[id], true
}

func (r *testRegistry) StateToRuntimeID(name string, props map[string]any) (uint32, bool) {
	id, ok := r.ids[stateKey(name, props)]
	return id, ok
}
func (r *testRegistry) FilteringBlock(uint32) uint8           { return 0 }
func (r *testRegistry) LightBlock(uint32) uint8               { return 0 }
func (r *testRegistry) RandomTickBlock(uint32) bool           { return false }
func (r *testRegistry) NBTBlock(uint32) bool                  { return false }
func (r *testRegistry) LiquidDisplacingBlock(uint32) bool     { return false }
func (r *testRegistry) LiquidBlock(uint32) bool               { return false }
func (r *testRegistry) HashToRuntimeID(uint32) (uint32, bool) { return 0, false }
func (r *testRegistry) RuntimeIDToHash(uint32) (uint32, bool) { return 0, false }

var testRange = cube.Range{-64, 319}

// filledChunk sets blocks on both layers, across sub chunks, with more
// distinct values than small palettes hold, plus some biomes.
func filledChunk(br BlockRegistry) *Chunk {
	c := New(br, testRange)
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(-64); y < 80; y += 3 {
				c.SetBlock(x, y, z, 0, uint32(int(x)+int(z)+int(y+64))%uint32(br.BlockCount()))
			}
			c.SetBlock(x, 10, z, 1, 3)
			c.SetBiome(x, 0, z, uint32(x))
		}
	}
	return c
}

func sameBlocks(t *testing.T, a, b *Chunk) {
	t.Helper()
	for x := range uint8(16) {
		for z := range uint8(16) {
			for y := int16(testRange[0]); y <= int16(testRange[1]); y++ {
				for layer := range uint8(2) {
					if a.Block(x, y, z, layer) != b.Block(x, y, z, layer) {
						t.Fatalf("block %d,%d,%d layer %d: %d != %d", x, y, z, layer, a.Block(x, y, z, layer), b.Block(x, y, z, layer))
					}
				}
			}
			if a.Biome(x, 0, z) != b.Biome(x, 0, z) {
				t.Fatalf("biome %d,%d differs", x, z)
			}
		}
	}
}

func newTestUpgrader(t testing.TB) *Upgrader {
	t.Helper()
	up, err := NewUpgrader()
	if err != nil {
		t.Fatal(err)
	}
	return up
}

func TestDiskRoundTrip(t *testing.T) {
	br := newTestRegistry()
	c := filledChunk(br)
	got, err := DiskDecode(br, newTestUpgrader(t), Encode(c, DiskEncoding), testRange)
	if err != nil {
		t.Fatal(err)
	}
	sameBlocks(t, c, got)
	// Nil Upgrader: states written now are current, so it must still work.
	if got, err = DiskDecode(br, nil, Encode(c, DiskEncoding), testRange); err != nil {
		t.Fatal(err)
	}
	sameBlocks(t, c, got)
}

func TestNetworkRoundTrip(t *testing.T) {
	br := newTestRegistry()
	c := filledChunk(br)
	data := Encode(c, NetworkEncoding)
	var buf bytes.Buffer
	for _, sub := range data.SubChunks {
		buf.Write(sub)
	}
	buf.Write(data.Biomes)
	got, err := NetworkDecode(br, buf.Bytes(), len(data.SubChunks), testRange)
	if err != nil {
		t.Fatal(err)
	}
	sameBlocks(t, c, got)
}

func TestUpgraderLegacyTable(t *testing.T) {
	up := newTestUpgrader(t)
	if len(up.legacy) == 0 {
		t.Fatal("legacy table is empty")
	}
	if _, ok := up.upgradeLegacyEntry("minecraft:stone", 0); !ok {
		t.Fatal("minecraft:stone meta 0 not in the legacy table")
	}
}

func TestLightAreaNotSquare(t *testing.T) {
	br := newTestRegistry()
	for _, n := range []int{0, 2, 3} {
		cs := make([]*Chunk, n)
		for i := range cs {
			cs[i] = New(br, testRange)
		}
		if _, err := LightArea(cs, 0, 0); err == nil {
			t.Errorf("%d chunks: no error", n)
		}
	}
}

// subChunk builds a disk sub chunk with one storage of the given bits per
// block, raw index words and palette entry count.
func subChunk(bits byte, words []uint32, paletteCount uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte{SubChunkVersion, 1, 0})
	b.WriteByte(bits << 1)
	for _, w := range words {
		_ = binary.Write(&b, binary.LittleEndian, w)
	}
	_ = binary.Write(&b, binary.LittleEndian, paletteCount)
	return b.Bytes()
}

func TestDecodeRejectsCorrupt(t *testing.T) {
	br := newTestRegistry()
	decode := func(sub []byte) error {
		data := SerialisedData{SubChunks: make([][]byte, (testRange.Height()>>4)+1)}
		data.SubChunks[4] = sub
		_, err := DiskDecode(br, nil, data, testRange)
		return err
	}
	// 1 bit per block: 128 words. Every index is 1, but the palette count
	// says 1 entry, and no entries follow.
	ones := make([]uint32, 128)
	for i := range ones {
		ones[i] = 0xffffffff
	}
	cases := map[string][]byte{
		"invalid size":         {SubChunkVersion, 1, 0, 7 << 1},
		"huge palette count":   subChunk(1, make([]uint32, 128), 1<<30),
		"index past palette":   subChunk(1, ones, 1),
		"truncated index data": {SubChunkVersion, 1, 0, 1 << 1, 0, 0},
		// Found by FuzzDiskDecodeSubChunk: the Y in the header was used as
		// an index without a bounds check.
		"sub chunk Y out of range": {SubChunkVersion, 0, 52},
	}
	for name, sub := range cases {
		if err := decode(sub); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func FuzzDiskDecodeSubChunk(f *testing.F) {
	br := newTestRegistry()
	c := filledChunk(br)
	enc := Encode(c, DiskEncoding)
	for _, sub := range enc.SubChunks {
		if len(sub) > 0 {
			f.Add(sub, enc.Biomes)
		}
	}
	f.Fuzz(func(t *testing.T, sub, biomes []byte) {
		data := SerialisedData{SubChunks: make([][]byte, (testRange.Height()>>4)+1), Biomes: biomes}
		data.SubChunks[4] = sub
		_, _ = DiskDecode(br, nil, data, testRange)
	})
}

func FuzzNetworkDecode(f *testing.F) {
	br := newTestRegistry()
	data := Encode(filledChunk(br), NetworkEncoding)
	var buf bytes.Buffer
	for _, sub := range data.SubChunks {
		buf.Write(sub)
	}
	buf.Write(data.Biomes)
	f.Add(buf.Bytes(), len(data.SubChunks))
	f.Fuzz(func(t *testing.T, b []byte, count int) {
		if count < 0 || count > 24 {
			return
		}
		_, _ = NetworkDecode(br, b, count, testRange)
	})
}
