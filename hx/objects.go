package hx

import (
	"errors"
	"fmt"
)

// The objects record: a format version byte, then sections as in the terrain
// record (tag byte, varint length, body). Shells and payloads (section 1 of the
// design) come later; for now block entities are kept as BDS stores them.
const (
	objectsVersion = 1

	// secObjKey is a chunk key BDS writes for a chunk's objects: block
	// entities, pending and random ticks. Tag byte, then the value as stored.
	secObjKey byte = 1
	// secEntity is one entity: its 8-byte actor ID as in BDS's actorprefix
	// key, then its NBT as stored.
	secEntity byte = 2
)

// BDS chunk key tags that belong in the objects record. Every other chunk key
// goes in the terrain record.
const (
	TagBlockEntities  byte = '1'
	TagLegacyEntities byte = '2'
	TagPendingTicks   byte = '3'
	TagRandomTicks    byte = ';'
)

// ObjectTag reports whether chunk key tag belongs in the objects record.
func ObjectTag(tag byte) bool {
	switch tag {
	case TagBlockEntities, TagLegacyEntities, TagPendingTicks, TagRandomTicks:
		return true
	}
	return false
}

// actorIDLen is the length of the actor ID in BDS's actorprefix keys.
const actorIDLen = 8

var errObjects = errors.New("hx: corrupt objects record")

// Objects is a chunk's objects record, decoded. Its slices point into the
// record it was decoded from, and are reused by the next DecodeObjects into it.
type Objects struct {
	Keys []ChunkKey
	// Entities are in the order of BDS's digp list.
	Entities []Entity
}

// Entity is one stored entity.
type Entity struct {
	// ID is the actor ID as in BDS's actorprefix key: 8 bytes.
	ID   []byte
	Data []byte
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

// AppendObjects appends o's record to dst. It fails for an entity whose ID is
// not 8 bytes.
func AppendObjects(dst []byte, o *Objects) ([]byte, error) {
	dst = append(dst, objectsVersion)
	for _, k := range o.Keys {
		dst = appendSection(dst, secObjKey, 1+len(k.Value))
		dst = append(dst, k.Tag)
		dst = append(dst, k.Value...)
	}
	for _, e := range o.Entities {
		if len(e.ID) != actorIDLen {
			return dst, fmt.Errorf("hx: entity ID of %d bytes", len(e.ID))
		}
		dst = appendSection(dst, secEntity, actorIDLen+len(e.Data))
		dst = append(dst, e.ID...)
		dst = append(dst, e.Data...)
	}
	return dst, nil
}

// DecodeObjects decodes an objects record into o, reusing o's memory.
// Sections it does not know are skipped.
func DecodeObjects(rec []byte, o *Objects) error {
	o.Keys, o.Entities = o.Keys[:0], o.Entities[:0]
	if len(rec) == 0 || rec[0] != objectsVersion {
		return fmt.Errorf("%w: unknown version", errObjects)
	}
	return eachSection(rec[1:], errObjects, func(tag byte, body []byte) error {
		switch tag {
		case secObjKey:
			if len(body) == 0 {
				return fmt.Errorf("%w: empty chunk key", errObjects)
			}
			o.Keys = append(o.Keys, ChunkKey{Tag: body[0], Value: body[1:]})
		case secEntity:
			if len(body) < actorIDLen {
				return fmt.Errorf("%w: entity", errObjects)
			}
			o.Entities = append(o.Entities, Entity{ID: body[:actorIDLen], Data: body[actorIDLen:]})
		}
		return nil
	})
}
