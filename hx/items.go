package hx

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/helix-bedrock/protocol/nbt"
	"github.com/helix-bedrock/world/hx/store"
)

// Items held inside an item (a shulker box item's "Items") may live in their own record
// instead of inside the item: Helix's own format, beyond BDS. An item on the ground, in a
// player's inventory or in a chest then holds only a reference, so a world of dropped boxes
// of books costs memory and save time by count, not by size. The records are keyed by the
// SHA-256 of their bytes, so equal contents are stored once and a reference can be copied
// with the item freely. Export writes the items back in place (InlineItems).
const (
	// ItemsFile holds item contents records.
	ItemsFile = "items.hx"
	// KindItems is an item contents record: an NBT list (Little Endian), as nbt.Raw's
	// Bytes gives it.
	KindItems byte = 11
	// ItemsRefTag is the tag, in an item's "tag" compound, that refers to an item contents
	// record in place of an "Items" list: a compound of ItemsRefSum, ItemsRefSize and
	// ItemsRefShown.
	ItemsRefTag = "helix:items"
	// ItemsRefSum is the record's key ([32]byte), ItemsRefSize its size in bytes (int32),
	// ItemsRefShown the items as a client is shown them (a list, without book pages), so
	// showing or measuring the item reads no record.
	ItemsRefSum   = "sum"
	ItemsRefSize  = "size"
	ItemsRefShown = "shown"
	// PagesRefTag is the same for a book's "pages" list: the record holds the pages.
	PagesRefTag = "helix:pages"
	// itemsListTag and pagesListTag are the tags references stand for.
	itemsListTag = "Items"
	pagesListTag = "pages"
)

// refTags are the reference tags and the list tags each stands for.
var refTags = [...][2]string{{ItemsRefTag, itemsListTag}, {PagesRefTag, pagesListTag}}

// holdsRef reports whether b, NBT bytes, may hold a reference.
func holdsRef(b []byte) bool {
	return bytes.Contains(b, []byte(ItemsRefTag)) || bytes.Contains(b, []byte(PagesRefTag))
}

// ItemsSum is an item contents record's key: the SHA-256 of its bytes.
type ItemsSum [32]byte

// SumItems is the key of an item contents record holding b.
func SumItems(b []byte) ItemsSum { return sha256.Sum256(b) }

// ItemsRecord reads the item contents record of sum into dst[:0], or returns ErrNotFound.
func (v view) ItemsRecord(sum ItemsSum, dst []byte) ([]byte, error) {
	b, err := v.get(v.w.items, sum[:], dst)
	if err != nil {
		return nil, fmt.Errorf("hx: item contents %x: %w", sum[:8], err)
	}
	return b, nil
}

// CommitItems stores item contents records at once, in their own round, leaving out those
// already stored. A reference to one may be saved once this returns. sums[i] must be
// SumItems(list[i]). Runs: any goroutine.
func (w *World) CommitItems(list [][]byte, sums []ItemsSum) error {
	if len(list) != len(sums) {
		return errors.New("hx: commit items: a sum per record")
	}
	var b store.Batch
	for i, data := range list {
		_, err := w.s.Stat(w.items, sums[i][:])
		if err == nil {
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("hx: commit items: %w", err)
		}
		b.Put(w.items, KindItems, sums[i][:], data)
	}
	if b.Len() == 0 {
		return nil
	}
	if _, err := w.s.Commit(&b); err != nil {
		return fmt.Errorf("hx: commit items: %w", err)
	}
	return nil
}

// ItemsRef reads the reference an item tag holds: its record's key, size and the items as
// shown, or false if m is not one.
func ItemsRef(m map[string]any) (ItemsSum, int32, any, bool) {
	sum, ok := m[ItemsRefSum].([32]byte)
	if !ok {
		return ItemsSum{}, 0, nil, false
	}
	size, _ := m[ItemsRefSize].(int32)
	return sum, size, m[ItemsRefShown], true
}

// NewItemsRef is the compound saved under ItemsRefTag for a record of size bytes.
func NewItemsRef(sum ItemsSum, size int32, shown any) map[string]any {
	m := map[string]any{ItemsRefSum: [32]byte(sum), ItemsRefSize: size}
	if shown != nil {
		m[ItemsRefShown] = shown
	}
	return m
}

// InlineItemsNBT is src, a Little Endian NBT compound, with every item contents reference
// in it replaced by the items it refers to, appended to dst: what BDS reads. A compound with
// none is copied as it is, without decoding.
func (v view) InlineItemsNBT(src, dst []byte) ([]byte, error) {
	if !holdsRef(src) {
		return append(dst, src...), nil
	}
	var m map[string]any
	if err := nbt.UnmarshalEncoding(src, &m, nbt.LittleEndian); err != nil {
		return nil, fmt.Errorf("hx: inline items: %w", err)
	}
	if err := v.InlineItems(m); err != nil {
		return nil, err
	}
	b, err := nbt.MarshalEncoding(m, nbt.LittleEndian)
	if err != nil {
		return nil, fmt.Errorf("hx: inline items: %w", err)
	}
	return append(dst, b...), nil
}

// InlineItems replaces every item contents reference in m, at any depth, with the items (or
// pages) it refers to, and every reference inside those.
func (v view) InlineItems(m map[string]any) error {
	for _, t := range refTags {
		if err := v.inlineRef(m, t[0], t[1]); err != nil {
			return err
		}
	}
	for _, e := range m {
		if err := v.inlineIn(e); err != nil {
			return err
		}
	}
	return nil
}

// inlineRef replaces m's reference under refTag, if any, with the list under listTag. A
// record that holds references itself (a box's items holding a book whose pages are stored)
// is decoded, and inlined by the caller's walk over m; any other stays raw.
func (v view) inlineRef(m map[string]any, refTag, listTag string) error {
	ref, ok := m[refTag].(map[string]any)
	if !ok {
		return nil
	}
	sum, _, _, isRef := ItemsRef(ref)
	if !isRef {
		return errors.New("hx: inline items: bad reference")
	}
	b, err := v.ItemsRecord(sum, nil)
	if err != nil {
		return err
	}
	delete(m, refTag)
	raw := nbt.RawList(b, nbt.LittleEndian)
	if !holdsRef(b) {
		m[listTag] = raw
		return nil
	}
	list, err := raw.Value()
	if err != nil {
		return fmt.Errorf("hx: inline items: %w", err)
	}
	m[listTag] = list
	return nil
}

// inlineIn inlines references inside a value of a compound.
func (v view) inlineIn(e any) error {
	switch x := e.(type) {
	case map[string]any:
		return v.InlineItems(x)
	case []any:
		for _, el := range x {
			if err := v.inlineIn(el); err != nil {
				return err
			}
		}
	case []map[string]any:
		for _, el := range x {
			if err := v.InlineItems(el); err != nil {
				return err
			}
		}
	}
	return nil
}
