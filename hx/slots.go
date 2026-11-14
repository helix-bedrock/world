package hx

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/helix-bedrock/protocol/nbt"
)

// A container's items (a chest's, a barrel's, a hopper's...) can be kept out of its block
// entity as one slot record per occupied slot plus a summary record, so a change of one slot
// writes one slot and a reader that only needs counts never reads the items. The summary's
// presence marks the container as split: readers take the items from the slot records, also
// over an Items list the shell may still hold. Both live in the chunk's region file.
const (
	// KindSlot is one slot of a split container: the item as BDS stores it in Items, Slot
	// byte included.
	KindSlot byte = 9
	// KindSummary is a split container's summary: an LE NBT list of its items without their
	// "tag" compounds, in slot order.
	KindSummary byte = 10

	partSlot    byte = 4
	partSummary byte = 5
)

// LocalPos is a block's position inside its chunk.
type LocalPos struct {
	X, Z uint8
	Y    int16
}

// Local is the position of block x, y, z inside its chunk.
func Local(x, y, z int) LocalPos {
	return LocalPos{X: uint8(x & 15), Z: uint8(z & 15), Y: int16(y)} //nolint:gosec // in range
}

// containerKey is the start of a container's slot keys, or its summary key with part
// partSummary: the chunk's index, the part, the block's packed x|z<<4, its y (big-endian).
func containerKey(pos ChunkPos, at LocalPos, part byte) [6]byte {
	c := chunkKey(pos, part)
	var k [6]byte
	copy(k[:], c[:])
	k[3] = at.X&15 | at.Z<<4
	binary.BigEndian.PutUint16(k[4:], uint16(at.Y)) //nolint:gosec // raw bits
	return k
}

func slotKey(pos ChunkPos, at LocalPos, slot uint8) [7]byte {
	c := containerKey(pos, at, partSlot)
	var k [7]byte
	copy(k[:], c[:])
	k[6] = slot
	return k
}

// PutSlot sets one slot of the split container at at.
func (r *Round) PutSlot(dim Dimension, pos ChunkPos, at LocalPos, slot uint8, item []byte) error {
	f, err := r.w.region(dim, pos)
	if err != nil {
		return err
	}
	k := slotKey(pos, at, slot)
	r.b.Put(f, KindSlot, k[:], item)
	return nil
}

// SlotHeld reports whether the slot already holds exactly item on disk, as far as the store
// knows, so its write can be left out.
func (r *Round) SlotHeld(dim Dimension, pos ChunkPos, at LocalPos, slot uint8, item []byte) bool {
	f, err := r.w.region(dim, pos)
	if err != nil {
		return false
	}
	k := slotKey(pos, at, slot)
	return r.w.s.Holds(f, KindSlot, k[:], item)
}

// DeleteSlot empties one slot of the split container at at.
func (r *Round) DeleteSlot(dim Dimension, pos ChunkPos, at LocalPos, slot uint8) error {
	f, err := r.w.region(dim, pos)
	if err != nil {
		return err
	}
	k := slotKey(pos, at, slot)
	r.b.Delete(f, k[:])
	return nil
}

// PutSummary sets the summary of the split container at at, which marks it split.
func (r *Round) PutSummary(dim Dimension, pos ChunkPos, at LocalPos, summary []byte) error {
	f, err := r.w.region(dim, pos)
	if err != nil {
		return err
	}
	k := containerKey(pos, at, partSummary)
	if r.w.s.Holds(f, KindSummary, k[:], summary) {
		return nil
	}
	r.b.Put(f, KindSummary, k[:], summary)
	return nil
}

// DeleteSummary removes the summary of the container at at: it is no longer split.
func (r *Round) DeleteSummary(dim Dimension, pos ChunkPos, at LocalPos) error {
	f, err := r.w.region(dim, pos)
	if err != nil {
		return err
	}
	k := containerKey(pos, at, partSummary)
	r.b.Delete(f, k[:])
	return nil
}

// Summary reads the summary of the container at at into dst[:0], or returns ErrNotFound
// when the container is not split.
func (v view) Summary(dim Dimension, pos ChunkPos, at LocalPos, dst []byte) ([]byte, error) {
	f, err := v.w.region(dim, pos)
	if err != nil {
		return dst, err
	}
	k := containerKey(pos, at, partSummary)
	b, err := v.get(f, k[:], dst)
	if err != nil {
		return b, err
	}
	return b, nil
}

// Slot reads one slot of the split container at at into dst[:0], or returns ErrNotFound for
// an empty slot.
func (v view) Slot(dim Dimension, pos ChunkPos, at LocalPos, slot uint8, dst []byte) ([]byte, error) {
	f, err := v.w.region(dim, pos)
	if err != nil {
		return dst, err
	}
	k := slotKey(pos, at, slot)
	b, err := v.get(f, k[:], dst)
	if err != nil {
		return b, fmt.Errorf("hx: slot %d of the container at %v in chunk %v: %w", slot, at, pos, err)
	}
	return b, nil
}

// A summary record is: the slot count n (u8), the n slot numbers, then the light Items field
// (tag, name, list of compounds) of the items without their "tag" compounds, ready to splice
// into a compound for a reader that only needs counts.

// AppendSummary appends a summary of slots, whose light Items field is lightItems.
func AppendSummary(dst []byte, slots []uint8, lightItems []byte) []byte {
	dst = append(dst, uint8(len(slots))) //nolint:gosec // callers keep to 255
	dst = append(dst, slots...)
	return append(dst, lightItems...)
}

var errSummary = errors.New("hx: corrupt container summary")

// SummarySlots are the slot numbers a summary lists.
func SummarySlots(summary []byte) ([]uint8, error) {
	if len(summary) < 1 || len(summary) < 1+int(summary[0]) {
		return nil, errSummary
	}
	return summary[1 : 1+int(summary[0])], nil
}

// SummaryLight is the light Items field a summary holds.
func SummaryLight(summary []byte) ([]byte, error) {
	if len(summary) < 1 || len(summary) < 1+int(summary[0]) {
		return nil, errSummary
	}
	return summary[1+int(summary[0]):], nil
}

// ItemScratch is the buffers putting a container's items back works in. The zero value is
// ready to use.
type ItemScratch struct {
	summary, slot []byte
}

// LightMarker is the byte field WithItems adds to a compound whose Items are light (without
// their tags). A store given such a compound back must keep the container's stored slots.
const LightMarker = "helix:light_items"

// lightMarkerField is LightMarker as an encoded byte field set to 1.
var lightMarkerField = append(append([]byte{1, byte(len(LightMarker)), 0}, LightMarker...), 1)

const (
	itemsField     = "Items"
	nbtTagList     = 9
	nbtTagCompound = 10
)

// WithItems appends to dst the block entity compound shell (whole, as ExpandShell gives it)
// of the block at at with its container's items put back if it is split: the light ones when
// light reports true for its id, else every slot as stored. A block entity that is not split
// is appended as it is.
func (v view) WithItems(dim Dimension, pos ChunkPos, at LocalPos, shell []byte, light func(id string) bool, sc *ItemScratch, dst []byte) ([]byte, error) {
	var err error
	sc.summary, err = v.Summary(dim, pos, at, sc.summary)
	if errors.Is(err, ErrNotFound) {
		return append(dst, shell...), nil
	}
	if err != nil {
		return dst, err
	}
	if len(shell) < 1 {
		return dst, errors.New("hx: empty block entity")
	}
	dst = append(dst, shell[:len(shell)-1]...)
	if light != nil && light(compoundID(shell)) {
		field, err := SummaryLight(sc.summary)
		if err != nil {
			return dst, err
		}
		dst = append(dst, field...)
		dst = append(dst, lightMarkerField...)
		return append(dst, 0), nil
	}
	slots, err := SummarySlots(sc.summary)
	if err != nil {
		return dst, err
	}
	dst = append(dst, nbtTagList)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(itemsField)))
	dst = append(dst, itemsField...)
	dst = append(dst, nbtTagCompound)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(slots)))
	for _, slot := range slots {
		if sc.slot, err = v.Slot(dim, pos, at, slot, sc.slot); err != nil {
			return dst, err
		}
		dst = append(dst, sc.slot...)
	}
	return append(dst, 0), nil
}

// ExpandBlockEntity is ExpandShell for a block entity, with its split container's items put
// back whole (WithItems). tmp holds the shell meanwhile.
func (v view) ExpandBlockEntity(dim Dimension, pos ChunkPos, s Shell, sc *ItemScratch, tmp *[]byte, dst []byte) ([]byte, error) {
	var err error
	if *tmp, err = v.ExpandShell(dim, pos, s, (*tmp)[:0]); err != nil {
		return dst, err
	}
	m, err := nbt.DecodeWithout(*tmp, nbt.LittleEndian, notBlockPos)
	if err != nil {
		return dst, fmt.Errorf("hx: block entity position: %w", err)
	}
	x, _ := m["x"].(int32)
	y, _ := m["y"].(int32)
	z, _ := m["z"].(int32)
	return v.WithItems(dim, pos, Local(int(x), int(y), int(z)), *tmp, nil, sc, dst)
}

func notBlockPos(name string) bool { return name != "x" && name != "y" && name != "z" }

// compoundID is a block entity's id, or "".
func compoundID(compound []byte) string {
	m, err := nbt.DecodeWithout(compound, nbt.LittleEndian, notIDField)
	if err != nil {
		return ""
	}
	id, _ := m["id"].(string)
	return id
}

func notIDField(name string) bool { return name != "id" }
