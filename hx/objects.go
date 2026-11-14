package hx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/hx/store"
)

// The objects record: a format version byte, the number of payload records
// the chunk has (varint), then sections as in the terrain record (tag byte,
// varint length, body).
//
// Block entities and entities are stored as shells: their NBT as BDS stores
// it with the biggest fields cut out into payload records of their own,
// largest first, until the shell is at most shellLimit bytes. A normal chest
// stays whole; a chest of written books keeps its small fields in the shell
// and its Items in a payload, read only when the chest is used or exported.
// Cutting goes by size, not by type, so any block entity, mob or plugin data
// works the same. Splicing the payloads back gives BDS's bytes unchanged.
const (
	objectsVersion = 1

	// secObjKey is a chunk key BDS writes for a chunk's objects (see
	// ObjectTag): tag byte, then the value as stored.
	secObjKey byte = 1
	// secEntity is one entity: its 8-byte actor ID as in BDS's actorprefix
	// key, then its shell. An empty shell is an ID in BDS's digp list with
	// no actorprefix record.
	secEntity byte = 2
	// secBlockEntity is one block entity's shell, in the order of BDS's
	// block entity value.
	secBlockEntity byte = 3
	// secDigp is present when the chunk has a digp key, even an empty one.
	secDigp byte = 4
)

const (
	// shellLimit is the size fields are cut until a shell fits.
	shellLimit = 4 << 10
	// minCut is the smallest field cut into a payload.
	minCut = 512
)

// BDS chunk key tags that belong in the objects record. Block entities ('1')
// are stored as shells, not as a key. Every other chunk key goes in the
// terrain record.
const (
	TagBlockEntities  byte = '1'
	TagLegacyEntities byte = '2'
	TagPendingTicks   byte = '3'
	TagRandomTicks    byte = ':'
)

// ObjectTag reports whether chunk key tag belongs in the objects record.
func ObjectTag(tag byte) bool {
	switch tag {
	case TagBlockEntities, TagLegacyEntities, TagPendingTicks, TagRandomTicks:
		return true
	}
	return false
}

// ActorIDLen is the length of the actor ID in BDS's actorprefix keys.
const ActorIDLen = 8

var errObjects = errors.New("hx: corrupt objects record")

// Objects is a chunk's objects record, decoded. Its slices point into the
// record it was decoded from, and are reused by the next DecodeObjects into it.
type Objects struct {
	// Payloads is how many payload records the chunk has.
	Payloads uint32
	Keys     []ChunkKey
	// BlockEntities are in the order of BDS's block entity value.
	BlockEntities []Shell
	// Entities are in the order of BDS's digp list.
	Entities []Entity
	// Digp is set when the chunk had a digp key, even with no entities.
	Digp bool
}

// Entity is one stored entity.
type Entity struct {
	// ID is the actor ID as in BDS's actorprefix key: 8 bytes.
	ID    []byte
	Shell Shell
}

// Shell is a block entity's or entity's NBT with its big fields cut out.
type Shell struct {
	// Data is the NBT with the cut fields left out.
	Data []byte
	cuts []byte // encoded: per cut, offset, length, payload number
	n    int    // number of cuts
	size int    // length of the whole NBT
}

// Whole reports whether no field was cut, so Data is the whole NBT.
func (s Shell) Whole() bool { return s.n == 0 }

// Size is the length of the whole NBT, payloads included.
func (s Shell) Size() int { return s.size }

// Cuts is the number of fields cut into payloads.
func (s Shell) Cuts() int { return s.n }

// EachCut calls fn for every cut, in order: where the field starts in the
// whole NBT, its length, and its payload number.
func (s Shell) EachCut(fn func(offset, length int, payload uint32)) {
	b := s.cuts
	for range s.n {
		off, k1 := binary.Uvarint(b)
		l, k2 := binary.Uvarint(b[k1:])
		p, k3 := binary.Uvarint(b[k1+k2:])
		b = b[k1+k2+k3:]
		fn(int(off), int(l), uint32(p)) //nolint:gosec // checked by decodeShell
	}
}

// Key returns the value of the chunk key with tag, if the record has one.
func (o *Objects) Key(tag byte) ([]byte, bool) {
	for _, k := range o.Keys {
		if k.Tag == tag {
			return k.Value, true
		}
	}
	return nil, false
}

// DecodeObjects decodes an objects record into o, reusing o's memory.
// Sections it does not know are skipped.
func DecodeObjects(rec []byte, o *Objects) error {
	o.Keys, o.BlockEntities, o.Entities, o.Digp = o.Keys[:0], o.BlockEntities[:0], o.Entities[:0], false
	if len(rec) == 0 || rec[0] != objectsVersion {
		return fmt.Errorf("%w: unknown version", errObjects)
	}
	n, k := binary.Uvarint(rec[1:])
	if k <= 0 || n > 1<<32-1 {
		return fmt.Errorf("%w: payload count", errObjects)
	}
	o.Payloads = uint32(n)
	return eachSection(rec[1+k:], errObjects, o.section)
}

func (o *Objects) section(tag byte, body []byte) error {
	switch tag {
	case secObjKey:
		if len(body) == 0 {
			return fmt.Errorf("%w: empty chunk key", errObjects)
		}
		o.Keys = append(o.Keys, ChunkKey{Tag: body[0], Value: body[1:]})
	case secEntity:
		if len(body) < ActorIDLen {
			return fmt.Errorf("%w: entity", errObjects)
		}
		s, err := decodeShell(body[ActorIDLen:], o.Payloads)
		if err != nil {
			return err
		}
		o.Entities = append(o.Entities, Entity{ID: body[:ActorIDLen], Shell: s})
	case secBlockEntity:
		s, err := decodeShell(body, o.Payloads)
		if err != nil {
			return err
		}
		o.BlockEntities = append(o.BlockEntities, s)
	case secDigp:
		o.Digp = true
	}
	return nil
}

// decodeShell reads a shell: the cut count, the cuts (offset, length,
// payload, each a varint) and the shell's bytes. Cuts must be in order, not
// overlap and name payloads the chunk has.
func decodeShell(b []byte, payloads uint32) (Shell, error) {
	n, k := binary.Uvarint(b)
	if k <= 0 || n > uint64(len(b)) {
		return Shell{}, fmt.Errorf("%w: shell cut count", errObjects)
	}
	start := k
	pos, removed := 0, 0
	for range n {
		off, k1 := binary.Uvarint(b[k:])
		if k1 <= 0 {
			return Shell{}, fmt.Errorf("%w: shell cut", errObjects)
		}
		l, k2 := binary.Uvarint(b[k+k1:])
		if k2 <= 0 {
			return Shell{}, fmt.Errorf("%w: shell cut", errObjects)
		}
		p, k3 := binary.Uvarint(b[k+k1+k2:])
		if k3 <= 0 || p >= uint64(payloads) || off < uint64(pos) || l == 0 || l > 1<<40 || off > 1<<40 {
			return Shell{}, fmt.Errorf("%w: shell cut", errObjects)
		}
		k += k1 + k2 + k3
		pos, removed = int(off+l), removed+int(l) //nolint:gosec // bounded above
	}
	s := Shell{cuts: b[start:k], n: int(n), Data: b[k:]} //nolint:gosec // n <= len(b)
	s.size = len(s.Data) + removed
	if pos > s.size {
		return Shell{}, fmt.Errorf("%w: shell cut past the end", errObjects)
	}
	return s, nil
}

// field is a field of a compound being cut.
type field struct{ start, end int }

// planCuts appends to cuts the fields of compound to cut out, in order.
func planCuts(compound []byte, fields []field, cuts []field) ([]field, []field, error) {
	fields, cuts = fields[:0], cuts[:0]
	if len(compound) <= shellLimit {
		return fields, cuts, nil
	}
	n, err := nbt.EachField(compound, nbt.LittleEndian, func(_ string, start, end int) bool {
		if end-start >= minCut {
			fields = append(fields, field{start, end})
		}
		return true
	})
	if err != nil {
		return fields, cuts, fmt.Errorf("hx: compound: %w", err)
	}
	if n != len(compound) {
		return fields, cuts, fmt.Errorf("hx: compound of %d bytes followed by %d more", n, len(compound)-n)
	}
	slices.SortFunc(fields, func(a, b field) int { return (b.end - b.start) - (a.end - a.start) })
	size := len(compound)
	for _, f := range fields {
		if size <= shellLimit {
			break
		}
		cuts = append(cuts, f)
		size -= f.end - f.start
	}
	slices.SortFunc(cuts, func(a, b field) int { return a.start - b.start })
	return fields, cuts, nil
}

// payloadKey is a payload record's key in its region: the chunk's index, the
// part, and the payload number (big-endian u32).
func payloadKey(pos ChunkPos, n uint32) [7]byte {
	c := chunkKey(pos, partPayload)
	var k [7]byte
	copy(k[:], c[:])
	binary.BigEndian.PutUint32(k[3:], n)
	return k
}

// ObjectsWriter writes a chunk's objects record: chunk keys, block entities
// and entities, each cut into a shell and payloads as it is added. Payloads go
// into the round at once (copied), so the caller may reuse its buffers, and a
// converter may commit the round while a huge chunk is being written: the
// objects record itself is put by Close. One writer per Round at a time.
type ObjectsWriter struct {
	r    *Round
	dim  Dimension
	pos  ChunkPos
	f    store.FileID
	body []byte
	// payloads is the payload count the record will give: one past the
	// highest number in use.
	payloads uint32
	// next is where the search for a free payload number starts; every
	// number below it is kept or newly written.
	next uint32
	// kept marks the payload numbers of kept shells.
	kept   []bool
	nums   []uint32
	empty  bool
	fields []field
	cuts   []field
}

// Objects starts writing the objects record of a chunk, replacing the one
// stored.
func (r *Round) Objects(dim Dimension, pos ChunkPos) (*ObjectsWriter, error) {
	f, err := r.w.region(dim, pos)
	if err != nil {
		return nil, err
	}
	ow := &r.ow
	*ow = ObjectsWriter{r: r, dim: dim, pos: pos, f: f, body: ow.body[:0], empty: true, fields: ow.fields, cuts: ow.cuts,
		kept: ow.kept[:0], nums: ow.nums[:0]}
	return ow, nil
}

// Key adds a chunk key BDS stores with the chunk's objects (see ObjectTag).
func (ow *ObjectsWriter) Key(tag byte, value []byte) {
	ow.body = appendSection(ow.body, secObjKey, 1+len(value))
	ow.body = append(ow.body, tag)
	ow.body = append(ow.body, value...)
	ow.empty = false
}

// Digp records that the chunk has a digp key, also when it lists no entity.
func (ow *ObjectsWriter) Digp() {
	ow.body = appendSection(ow.body, secDigp, 0)
	ow.empty = false
}

// BlockEntity adds one block entity: one LE NBT compound as BDS stores it.
func (ow *ObjectsWriter) BlockEntity(compound []byte) error {
	return ow.shell(secBlockEntity, nil, compound)
}

// Entity adds one entity: its 8-byte actor ID and its LE NBT as BDS stores
// it, or nil for an ID in digp with no actorprefix record.
func (ow *ObjectsWriter) Entity(id, data []byte) error {
	if len(id) != ActorIDLen {
		return fmt.Errorf("hx: entity ID of %d bytes", len(id))
	}
	return ow.shell(secEntity, id, data)
}

// KeepBlockEntity adds a block entity shell decoded from the chunk's stored
// objects record, with its payloads left on disk as they are: a save in which
// a chest did not change neither reads nor writes its contents again. Kept
// shells must come before any new block entity or entity.
func (ow *ObjectsWriter) KeepBlockEntity(s Shell) error {
	if ow.next > 0 {
		return errors.New("hx: shell kept after new ones")
	}
	var err error
	s.EachCut(func(_, _ int, payload uint32) {
		if int(payload) >= len(ow.kept) {
			ow.kept = append(ow.kept, make([]bool, int(payload)+1-len(ow.kept))...)
		}
		if ow.kept[payload] {
			err = fmt.Errorf("hx: payload %d kept twice", payload)
		}
		ow.kept[payload] = true
	})
	if err != nil {
		return err
	}
	ow.payloads = max(ow.payloads, uint32(len(ow.kept)))                                              //nolint:gosec // payload numbers are u32
	ow.body = appendSection(ow.body, secBlockEntity, uvarintLen(uint64(s.n))+len(s.cuts)+len(s.Data)) //nolint:gosec // small
	ow.body = binary.AppendUvarint(ow.body, uint64(s.n))                                              //nolint:gosec // small
	ow.body = append(ow.body, s.cuts...)
	ow.body = append(ow.body, s.Data...)
	ow.empty = false
	return nil
}

// allocPayload returns the lowest payload number neither kept nor written.
func (ow *ObjectsWriter) allocPayload() uint32 {
	n := ow.next
	for int(n) < len(ow.kept) && ow.kept[n] {
		n++
	}
	ow.next = n + 1
	ow.payloads = max(ow.payloads, ow.next)
	return n
}

// shell cuts compound and appends its section; prefix (an entity ID) comes
// before the shell.
func (ow *ObjectsWriter) shell(tag byte, prefix, compound []byte) error {
	var err error
	if len(compound) > 0 {
		if ow.fields, ow.cuts, err = planCuts(compound, ow.fields, ow.cuts); err != nil {
			return err
		}
	} else {
		ow.cuts = ow.cuts[:0]
	}
	ow.nums = ow.nums[:0]
	for range ow.cuts {
		ow.nums = append(ow.nums, ow.allocPayload())
	}
	n := len(prefix) + uvarintLen(uint64(len(ow.cuts))) + len(compound)
	for i, c := range ow.cuts {
		n += uvarintLen(uint64(c.start)) + uvarintLen(uint64(c.end-c.start)) + uvarintLen(uint64(ow.nums[i])) //nolint:gosec // small
		n -= c.end - c.start
	}
	ow.body = appendSection(ow.body, tag, n)
	ow.body = append(ow.body, prefix...)
	ow.body = binary.AppendUvarint(ow.body, uint64(len(ow.cuts)))
	for i, c := range ow.cuts {
		k := payloadKey(ow.pos, ow.nums[i])
		ow.r.b.Put(ow.f, KindPayload, k[:], compound[c.start:c.end])
		ow.body = binary.AppendUvarint(ow.body, uint64(c.start))       //nolint:gosec // offsets
		ow.body = binary.AppendUvarint(ow.body, uint64(c.end-c.start)) //nolint:gosec // lengths
		ow.body = binary.AppendUvarint(ow.body, uint64(ow.nums[i]))
	}
	last := 0
	for _, c := range ow.cuts {
		ow.body = append(ow.body, compound[last:c.start]...)
		last = c.end
	}
	ow.body = append(ow.body, compound[last:]...)
	ow.empty = false
	return nil
}

// Close puts the objects record, or deletes it if nothing was added, and
// deletes the payloads of the old record that are neither kept nor written
// again.
func (ow *ObjectsWriter) Close() error {
	r := ow.r
	old, err := r.w.storedPayloads(ow.dim, ow.pos, &r.scratch)
	if err != nil {
		return err
	}
	for n := ow.next; n < old; n++ {
		if int(n) < len(ow.kept) && ow.kept[n] {
			continue
		}
		k := payloadKey(ow.pos, n)
		r.b.Delete(ow.f, k[:])
	}
	k := chunkKey(ow.pos, partObjects)
	if ow.empty {
		r.b.Delete(ow.f, k[:])
		return nil
	}
	r.buf = append(r.buf[:0], objectsVersion)
	r.buf = binary.AppendUvarint(r.buf, uint64(ow.payloads))
	r.buf = append(r.buf, ow.body...)
	r.b.Put(ow.f, KindObjects, k[:], r.buf)
	return nil
}

// storedPayloads returns how many payloads the committed objects record of a
// chunk has, reading it into *scratch.
func (v view) storedPayloads(dim Dimension, pos ChunkPos, scratch *[]byte) (uint32, error) {
	rec, err := v.ObjectsRecord(dim, pos, *scratch)
	*scratch = rec
	if errors.Is(err, store.ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("hx: objects of chunk %v in %s: %w", pos, dim.Dir(), err)
	}
	if len(rec) == 0 || rec[0] != objectsVersion {
		return 0, fmt.Errorf("hx: objects of chunk %v in %s: %w", pos, dim.Dir(), errObjects)
	}
	n, k := binary.Uvarint(rec[1:])
	if k <= 0 || n > 1<<32-1 {
		return 0, fmt.Errorf("hx: objects of chunk %v in %s: %w", pos, dim.Dir(), errObjects)
	}
	return uint32(n), nil
}

// ExpandShell appends the whole NBT of s, a shell of chunk pos in dim, to
// dst, reading its payloads.
func (v view) ExpandShell(dim Dimension, pos ChunkPos, s Shell, dst []byte) ([]byte, error) {
	if s.Whole() {
		return append(dst, s.Data...), nil
	}
	f, err := v.w.region(dim, pos)
	if err != nil {
		return dst, err
	}
	last, removed := 0, 0
	s.EachCut(func(offset, length int, payload uint32) {
		if err != nil {
			return
		}
		at := offset - removed
		dst = append(dst, s.Data[last:at]...)
		last, removed = at, removed+length
		k := payloadKey(pos, payload)
		start := len(dst)
		if dst, err = v.appendGet(f, k[:], dst); err != nil {
			err = fmt.Errorf("hx: payload %d of chunk %v in %s: %w", payload, pos, dim.Dir(), err)
			return
		}
		if len(dst)-start != length {
			err = fmt.Errorf("hx: payload %d of chunk %v in %s: %d bytes, want %d", payload, pos, dim.Dir(), len(dst)-start, length)
		}
	})
	if err != nil {
		return dst, err
	}
	if last > len(s.Data) {
		return dst, fmt.Errorf("hx: shell of chunk %v in %s: %w", pos, dim.Dir(), errObjects)
	}
	return append(dst, s.Data[last:]...), nil
}

// appendGet appends key's body to dst, reading straight into dst's spare
// capacity when it fits.
func (v view) appendGet(f store.FileID, key, dst []byte) ([]byte, error) {
	got, err := v.get(f, key, dst[len(dst):])
	if err != nil {
		return dst, err
	}
	if len(got) > 0 && cap(dst)-len(dst) >= len(got) && &dst[:len(dst)+1][len(dst)] == &got[0] {
		return dst[:len(dst)+len(got)], nil
	}
	return append(dst, got...), nil
}

// Payload reads payload n of a chunk into dst[:0], or returns ErrNotFound.
func (v view) Payload(dim Dimension, pos ChunkPos, n uint32, dst []byte) ([]byte, error) {
	f, err := v.w.region(dim, pos)
	if err != nil {
		return dst[:0], err
	}
	k := payloadKey(pos, n)
	return v.get(f, k[:], dst)
}
