package hx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/hx/store"
)

// BlocksFile is the record store file holding a world's block table.
const BlocksFile = "blocks.hx"

// Record kinds of Helix world files.
const (
	// KindBlock is a block table entry: key = table ID (big-endian u32),
	// body = the block state as BDS stores it in a palette (LE NBT).
	KindBlock byte = 1
	// KindTerrain is a chunk's terrain record (terrain.go).
	KindTerrain byte = 2
)

// Entry flags.
const (
	// flagResolved is set when the entry has a runtime ID in the registry.
	flagResolved uint8 = 1 << iota
	// flagLegacy is set when the entry was saved before chunk.CurrentBlockVersion,
	// so states added since were defaulted by the upgrade.
	flagLegacy
)

// noID marks a runtime ID with no table entry yet.
const noID = ^uint32(0)

// maxEntryBytes is the largest block state a table entry may hold.
const maxEntryBytes = 1 << 16

var errUnknownBlock = errors.New("hx: unknown block table ID")

// BlockTable is a world's block table: every block state the world's chunks
// use, each stored once as the exact bytes BDS keeps in a palette, numbered
// from 0 in the order they were added. Chunk palettes hold these numbers, so
// loading a chunk resolves each palette entry with a slice lookup, and a
// world exported to BDS gets back the bytes it was converted from.
//
// IDs never change once given and entries are never removed. RuntimeID, ID
// and Bytes may be called from any goroutine and do not allocate once the
// entries they meet exist; adding entries takes a lock.
type BlockTable struct {
	reg chunk.BlockRegistry
	enc chunk.BlockPaletteEncoding

	// snap is the entries readers see. Writers append past every published
	// length under mu, then publish a new snapshot, so a reader never sees a
	// slot being written.
	snap atomic.Pointer[tableSnap]
	// byRID maps a runtime ID to the entry holding its current form, or noID.
	byRID []atomic.Uint32

	mu      sync.Mutex
	byBytes map[string]uint32
	saved   int // entries already in blocks.hx
}

// tableSnap is one published state of the table. data holds every entry's
// bytes back to back; entry i is data[ends[i-1]:ends[i]].
type tableSnap struct {
	data  []byte
	ends  []uint32
	rids  []uint32
	flags []uint8
}

// NewBlockTable returns an empty table resolving states against reg. up
// upgrades states saved by older versions; with nil, only current states
// resolve. With a nil reg the table only keeps bytes, as a converter needs:
// no entry resolves and ID fails.
func NewBlockTable(reg chunk.BlockRegistry, up *chunk.Upgrader) *BlockTable {
	n := 0
	if reg != nil {
		n = reg.BlockCount()
	}
	t := &BlockTable{
		reg:     reg,
		enc:     chunk.BlockPaletteEncoding{Blocks: reg, Upgrader: up},
		byRID:   make([]atomic.Uint32, n),
		byBytes: map[string]uint32{},
	}
	for i := range t.byRID {
		t.byRID[i].Store(noID)
	}
	t.snap.Store(&tableSnap{})
	return t
}

// Len returns the number of entries.
func (t *BlockTable) Len() int { return len(t.snap.Load().ends) }

// RuntimeID returns the runtime ID of entry id, and whether the entry was
// saved before chunk.CurrentBlockVersion. It fails for IDs the table does not
// have and for states the registry does not know.
func (t *BlockTable) RuntimeID(id uint32) (rid uint32, legacy bool, err error) {
	s := t.snap.Load()
	if uint64(id) >= uint64(len(s.rids)) {
		return 0, false, errUnknownBlock
	}
	f := s.flags[id]
	if f&flagResolved == 0 {
		return 0, false, fmt.Errorf("hx: block table entry %d is a state the registry does not know", id)
	}
	return s.rids[id], f&flagLegacy != 0, nil
}

// Bytes returns entry id as BDS stores it in a palette. The bytes belong to
// the table: never change them.
func (t *BlockTable) Bytes(id uint32) ([]byte, bool) {
	s := t.snap.Load()
	if uint64(id) >= uint64(len(s.ends)) {
		return nil, false
	}
	start := uint32(0)
	if id > 0 {
		start = s.ends[id-1]
	}
	return s.data[start:s.ends[id]], true
}

// ID returns the entry for runtime ID rid in its current form, adding one if
// the table has none.
func (t *BlockTable) ID(rid uint32) (uint32, error) {
	if uint64(rid) >= uint64(len(t.byRID)) {
		return 0, fmt.Errorf("hx: runtime ID %d outside the registry", rid)
	}
	if id := t.byRID[rid].Load(); id != noID {
		return id, nil
	}
	b, err := nbt.MarshalEncoding(t.enc.EncodeBlockState(rid), nbt.LittleEndian)
	if err != nil {
		return 0, fmt.Errorf("hx: encode block state %d: %w", rid, err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	id, err := t.addLocked(b)
	if err != nil {
		return 0, err
	}
	t.byRID[rid].Store(id)
	return id, nil
}

// Add returns the entry holding state, a block state as BDS stores it in a
// palette, adding one if the table has none. A state the registry does not
// know is still added, so it can be exported unchanged; RuntimeID fails for it.
func (t *BlockTable) Add(state []byte) (uint32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.addLocked(state)
}

func (t *BlockTable) addLocked(state []byte) (uint32, error) {
	if id, ok := t.byBytes[string(state)]; ok {
		return id, nil
	}
	if len(state) == 0 || len(state) > maxEntryBytes {
		return 0, fmt.Errorf("hx: block state of %d bytes", len(state))
	}
	old := t.snap.Load()
	if len(old.ends) >= int(noID) {
		return 0, errors.New("hx: block table full")
	}
	id := uint32(len(old.ends)) //nolint:gosec // checked above
	rid, flags := t.resolve(state)
	s := &tableSnap{
		data:  append(old.data, state...),
		rids:  append(old.rids, rid),
		flags: append(old.flags, flags),
	}
	s.ends = append(old.ends, uint32(len(s.data))) //nolint:gosec // entries are small and few
	t.byBytes[string(state)] = id
	t.snap.Store(s)
	return id, nil
}

// resolve finds the runtime ID of a stored state, upgrading it if it is old.
func (t *BlockTable) resolve(state []byte) (rid uint32, flags uint8) {
	if t.reg == nil {
		return 0, 0
	}
	var m map[string]any
	if err := nbt.UnmarshalEncoding(state, &m, nbt.LittleEndian); err != nil {
		return 0, 0
	}
	rid, err := t.enc.DecodeBlockState(m)
	if err != nil {
		return 0, 0
	}
	flags = flagResolved
	if v, _ := m["version"].(int32); v < chunk.CurrentBlockVersion {
		flags |= flagLegacy
	}
	return rid, flags
}

// blockKey returns the store key of entry id.
func blockKey(id uint32) [4]byte {
	var k [4]byte
	binary.BigEndian.PutUint32(k[:], id)
	return k
}

// LoadBlockTable reads the table from file f of s. Entries must be numbered
// 0 to n-1 with none missing.
func LoadBlockTable(s *store.Store, f store.FileID, reg chunk.BlockRegistry, up *chunk.Upgrader) (*BlockTable, error) {
	t := NewBlockTable(reg, up)
	var (
		buf  []byte
		want uint32
		err  error
	)
	rangeErr := s.Range(f, func(key []byte, _ store.Info) bool {
		if len(key) != 4 || binary.BigEndian.Uint32(key) != want {
			err = fmt.Errorf("hx: block table: entry %d missing", want)
			return false
		}
		if buf, err = s.Get(f, key, buf); err != nil {
			err = fmt.Errorf("hx: block table: read entry %d: %w", want, err)
			return false
		}
		var id uint32
		if id, err = t.Add(buf); err != nil {
			return false
		}
		if id != want {
			err = fmt.Errorf("hx: block table: entry %d repeats entry %d", want, id)
			return false
		}
		want++
		return true
	})
	if rangeErr != nil {
		return nil, fmt.Errorf("hx: block table: %w", rangeErr)
	}
	if err != nil {
		return nil, err
	}
	t.saved = int(want)
	return t, nil
}

// PutNew adds every entry not yet saved to b, for file f, and returns how many
// entries the table has with them. Commit b in the same round as the chunks
// that use the entries, then call MarkSaved with the count.
func (t *BlockTable) PutNew(b *store.Batch, f store.FileID) int {
	t.mu.Lock()
	from := t.saved
	t.mu.Unlock()
	s := t.snap.Load()
	for id := from; id < len(s.ends); id++ {
		k := blockKey(uint32(id))   //nolint:gosec // table IDs are u32
		v, _ := t.Bytes(uint32(id)) //nolint:gosec // table IDs are u32
		b.Put(f, KindBlock, k[:], v)
	}
	return len(s.ends)
}

// MarkSaved records that the first n entries are in the store.
func (t *BlockTable) MarkSaved(n int) {
	t.mu.Lock()
	t.saved = max(t.saved, n)
	t.mu.Unlock()
}
