package hxdb

import (
	"bytes"
	"errors"
	"fmt"
	"maps"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/chunk"
	"github.com/helix-bedrock/world/cube"
	"github.com/helix-bedrock/world/hx"
	"github.com/helix-bedrock/world/mcdb"
)

// Containers are stored split (hx/slots.go): a block entity's Items list goes out as one
// record per slot plus a summary, and its shell keeps the rest. A save writes only the slots
// whose bytes changed, each encoded alone, so neither a save nor a read of a chest ever holds
// all its items as one value; slot records are committed in rounds of their own as they
// grow, so a save's memory stays within the round size whatever the chests hold. Reads give
// the compound back whole, with Items, as BDS stores it.
//
// The summary record is: the slot count n (u8), the n slot numbers, then the light Items
// field (tag, name, list) of the items without their "tag" compounds, ready to splice into a
// compound for a reader that only needs counts.

const (
	itemsField = "Items"
	itemTag    = "tag"
	slotField  = "Slot"
	// nbtRootHead is an encoded root compound's tag and empty name.
	nbtRootHead = 3
	// maxSplitSlots is the most slots a summary can list.
	maxSplitSlots = 255
)

// slotItem is one item of a container being split.
type slotItem struct {
	slot uint8
	item map[string]any
}

// splitItems returns the items of a block entity's Items list by slot, and false when it
// has none or one the split cannot keep apart (no Slot byte, a slot twice).
func splitItems(data map[string]any, dst []slotItem) ([]slotItem, bool) {
	dst = dst[:0]
	raw, ok := data[itemsField]
	if !ok {
		return dst, false
	}
	var seen [256]bool
	add := func(m map[string]any) bool {
		slot, ok := m[slotField].(uint8)
		if !ok || seen[slot] {
			return false
		}
		seen[slot] = true
		dst = append(dst, slotItem{slot: slot, item: m})
		return true
	}
	switch l := raw.(type) {
	case []any:
		for _, v := range l {
			m, ok := v.(map[string]any)
			if !ok || !add(m) {
				return dst, false
			}
		}
	case []map[string]any:
		for _, m := range l {
			if !add(m) {
				return dst, false
			}
		}
	default:
		return dst, false
	}
	return dst, true
}

// splitScratch is what splitting a column's containers works in.
type splitScratch struct {
	items   []slotItem
	enc     bytes.Buffer
	light   []any
	summary []byte
	old     []byte
	slots   []uint8
	// slotNums are the slots being written.
	slotNums []uint8
	kept     map[cube.Pos]bool
	bes      []chunk.BlockEntity
	// side is the round records go into: own, or a linked store's.
	side, own *hx.Round
	linked    bool
}

// splitContainers writes the slot and summary records of the column's containers in side
// rounds, removes those of containers gone or no longer split, and returns the block
// entities to store, with the split ones' Items left out. The caller's maps are not changed.
//
// With into set (db.mu held), every record goes into that round instead and nothing is
// committed here: a linked store commits them together with what else it writes.
func (db *DB) splitContainers(sc *splitScratch, dim hx.Dimension, pos hx.ChunkPos, col *chunk.Column, into *hx.Round) ([]chunk.BlockEntity, error) {
	sc.linked = into != nil
	if sc.linked {
		sc.side = into
	} else {
		if sc.own == nil {
			sc.own = db.w.NewRound()
		}
		sc.side = sc.own
	}
	if sc.kept == nil {
		sc.kept = map[cube.Pos]bool{}
	}
	clear(sc.kept)
	sc.bes = sc.bes[:0]
	if !col.KeepBlockEntities {
		for _, at := range col.KeepBlockEntitiesAt {
			sc.kept[at] = true
		}
		for _, be := range col.BlockEntities {
			out, err := db.splitOne(sc, dim, pos, be)
			if err != nil {
				return nil, err
			}
			sc.kept[be.Pos] = true
			sc.bes = append(sc.bes, out)
		}
		if err := db.dropGoneContainers(sc, dim, pos); err != nil {
			return nil, err
		}
	}
	if err := db.commitSide(sc); err != nil {
		return nil, err
	}
	return sc.bes, nil
}

// splitOne splits one block entity if it holds items, else makes sure no split records of
// an older one at its position are left to override it.
func (db *DB) splitOne(sc *splitScratch, dim hx.Dimension, pos hx.ChunkPos, be chunk.BlockEntity) (chunk.BlockEntity, error) {
	at := hx.Local(be.Pos[0], be.Pos[1], be.Pos[2])
	var ok bool
	if be.Raw == nil {
		sc.items, ok = splitItems(be.Data, sc.items)
	}
	if !ok || len(sc.items) > maxSplitSlots {
		return be, db.unsplit(sc, dim, pos, at)
	}
	if err := db.readOldSlots(sc, dim, pos, at); err != nil {
		return be, err
	}
	sc.light = sc.light[:0]
	sc.slotNums = sc.slotNums[:0]
	for _, it := range sc.items {
		if err := db.putSlot(sc, dim, pos, at, it); err != nil {
			return be, err
		}
		sc.slotNums = append(sc.slotNums, it.slot)
		light := it.item
		if _, has := light[itemTag]; has {
			light = maps.Clone(light)
			delete(light, itemTag)
		}
		sc.light = append(sc.light, light)
	}
	for _, slot := range sc.slots {
		if !hasSlot(sc.items, slot) {
			if err := sc.side.DeleteSlot(dim, pos, at, slot); err != nil {
				return be, err
			}
		}
	}
	field, err := encodeItemsField(&sc.enc, sc.light)
	if err != nil {
		return be, err
	}
	sc.summary = hx.AppendSummary(sc.summary[:0], sc.slotNums, field)
	if err := sc.side.PutSummary(dim, pos, at, sc.summary); err != nil {
		return be, err
	}
	rest := maps.Clone(be.Data)
	delete(rest, itemsField)
	return chunk.BlockEntity{Pos: be.Pos, Data: rest}, db.maybeCommitSide(sc)
}

func hasSlot(items []slotItem, slot uint8) bool {
	for _, it := range items {
		if it.slot == slot {
			return true
		}
	}
	return false
}

// putSlot writes one slot, unless the store already holds its bytes.
func (db *DB) putSlot(sc *splitScratch, dim hx.Dimension, pos hx.ChunkPos, at hx.LocalPos, it slotItem) error {
	sc.enc.Reset()
	if err := nbt.NewEncoderWithEncoding(&sc.enc, nbt.LittleEndian).Encode(it.item); err != nil {
		return fmt.Errorf("slot %d: %w", it.slot, err)
	}
	b := sc.enc.Bytes()
	if len(b) < nbtRootHead {
		return fmt.Errorf("slot %d: short compound", it.slot)
	}
	elem := b[nbtRootHead:]
	if sc.side.SlotHeld(dim, pos, at, it.slot, elem) {
		return nil
	}
	if err := sc.side.PutSlot(dim, pos, at, it.slot, elem); err != nil {
		return err
	}
	return db.maybeCommitSide(sc)
}

// encodeItemsField encodes the Items field (tag, name and list of compounds) of items.
func encodeItemsField(enc *bytes.Buffer, items []any) ([]byte, error) {
	enc.Reset()
	if err := nbt.NewEncoderWithEncoding(enc, nbt.LittleEndian).Encode(map[string]any{itemsField: items}); err != nil {
		return nil, err
	}
	b := enc.Bytes()
	if len(b) < nbtRootHead+1 {
		return nil, errors.New("hxdb: short items compound")
	}
	return b[nbtRootHead : len(b)-1], nil
}

// readOldSlots reads which slots the container at at had stored, if it was split.
func (db *DB) readOldSlots(sc *splitScratch, dim hx.Dimension, pos hx.ChunkPos, at hx.LocalPos) error {
	sc.slots = sc.slots[:0]
	var err error
	sc.old, err = db.w.Summary(dim, pos, at, sc.old)
	if errors.Is(err, hx.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	slots, err := hx.SummarySlots(sc.old)
	if err != nil {
		return err
	}
	sc.slots = append(sc.slots, slots...)
	return nil
}

// unsplit removes the split records of the container at at, if any.
func (db *DB) unsplit(sc *splitScratch, dim hx.Dimension, pos hx.ChunkPos, at hx.LocalPos) error {
	if err := db.readOldSlots(sc, dim, pos, at); err != nil {
		return err
	}
	if len(sc.slots) == 0 && len(sc.old) == 0 {
		return nil
	}
	for _, slot := range sc.slots {
		if err := sc.side.DeleteSlot(dim, pos, at, slot); err != nil {
			return err
		}
	}
	sc.old = sc.old[:0]
	return sc.side.DeleteSummary(dim, pos, at)
}

// dropGoneContainers removes the split records of the stored block entities the column no
// longer has.
func (db *DB) dropGoneContainers(sc *splitScratch, dim hx.Dimension, pos hx.ChunkPos) error {
	s := db.getScratch()
	defer db.putScratch(s)
	o, err := db.objects(s, dim, pos)
	if err != nil || o == nil {
		return err
	}
	for _, sh := range o.BlockEntities {
		p, err := shellPos(sh)
		if err != nil {
			return err
		}
		if sc.kept[p] {
			continue
		}
		if err := db.unsplit(sc, dim, pos, hx.Local(p[0], p[1], p[2])); err != nil {
			return err
		}
	}
	return nil
}

// maybeCommitSide commits the side round once it holds a round's worth.
func (db *DB) maybeCommitSide(sc *splitScratch) error {
	if sc.linked || sc.side.Size() < db.conf.RoundSize {
		return nil
	}
	return db.commitSide(sc)
}

func (db *DB) commitSide(sc *splitScratch) error {
	if sc.linked || sc.side.Len() == 0 {
		return nil
	}
	if _, err := db.w.Commit(sc.side); err != nil {
		return fmt.Errorf("hxdb: container slots: %w", err)
	}
	return nil
}

// withItems is hx's WithItems for the block at at.
func (db *DB) withItems(s *scratch, dim hx.Dimension, pos hx.ChunkPos, at cube.Pos, shell []byte, light func(id string) bool, dst []byte) ([]byte, error) {
	return db.w.WithItems(dim, pos, hx.Local(at[0], at[1], at[2]), shell, light, &s.items, dst)
}

// decodeWithItems decodes a stored block entity compound, with its items put back if its
// container is split.
func (db *DB) decodeWithItems(s *scratch, dim hx.Dimension, pos hx.ChunkPos, c []byte) (map[string]any, error) {
	m, err := nbt.DecodeWithout(c, nbt.LittleEndian, nil)
	if err != nil {
		return nil, err
	}
	whole, err := db.withItems(s, dim, pos, blockPosFromNBT(m), c, nil, s.whole[:0])
	s.whole = whole
	if err != nil {
		return nil, err
	}
	if len(whole) == len(c) {
		return m, nil
	}
	return nbt.DecodeWithout(whole, nbt.LittleEndian, nil)
}

// StoreLinked stores a chunk column and players in one commit, so they land on disk together
// or not at all: items a player moved into or out of a container in that chunk are then never
// on disk in both places, or in neither, whatever stops the server. Everything stored before
// is committed first, so nothing older lands after it.
func (db *DB) StoreLinked(pos mcdb.ChunkPos, dim mcdb.Dimension, col *chunk.Column, players []mcdb.LinkedPlayer) error {
	if db.conf.Blocks == nil {
		return fmt.Errorf("hxdb: store linked %v: no block registry", pos)
	}
	if _, ok := dim.Range(); !ok {
		return fmt.Errorf("hxdb: store linked %v: unknown dimension %v", pos, dim)
	}
	if col == nil || col.Chunk == nil {
		return fmt.Errorf("hxdb: store linked %v (%v): no chunk", pos, dim)
	}
	if err := db.Flush(); err != nil {
		return err
	}
	s := db.getScratch()
	defer db.putScratch(s)
	if err := db.terrainKeys(s, dim, col); err != nil {
		return fmt.Errorf("hxdb: store linked %v (%v): %w", pos, dim, err)
	}
	if err := db.storeLinked(s, pos, dim, col, players); err != nil {
		return fmt.Errorf("hxdb: store linked %v (%v): %w", pos, dim, err)
	}
	return db.Flush()
}

// storeLinked puts the column and the players in the shared round under one hold of db.mu,
// so no flush splits them.
func (db *DB) storeLinked(s *scratch, pos mcdb.ChunkPos, dim mcdb.Dimension, col *chunk.Column, players []mcdb.LinkedPlayer) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	bes, err := db.splitContainers(&s.split, hx.Dimension(dim), hx.ChunkPos(pos), col, db.round)
	if err != nil {
		return err
	}
	split := *col
	split.BlockEntities = bes
	if err := db.storeColumn(s, hx.Dimension(dim), hx.ChunkPos(pos), &split); err != nil {
		return err
	}
	db.held.chunks[chunkID{dim, pos}] = struct{}{}
	for _, p := range players {
		if err := db.round.PutPlayer(hx.PlayerIDs(p.IDs), p.Data); err != nil {
			return err
		}
		db.held.players[p.IDs] = struct{}{}
	}
	return nil
}
