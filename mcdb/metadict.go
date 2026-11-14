package mcdb

import "errors"

// ChunkMetaData is BDS's chunk metadata dictionary (the
// LevelChunkMetaDataDictionary level key), for world stores other than DB that
// write chunks as BDS does. Safe for concurrent use.
type ChunkMetaData struct{ d *metaDictionary }

// NewChunkMetaData returns an empty dictionary, for a new world.
func NewChunkMetaData() *ChunkMetaData { return &ChunkMetaData{d: newMetaDictionary()} }

// DecodeChunkMetaData reads the dictionary from its level key's value. On an
// error the entries read before it are kept and returned too: chunks pointing
// at the rest keep their hash.
func DecodeChunkMetaData(b []byte) (*ChunkMetaData, error) {
	d, err := decodeMetaDictionary(b)
	return &ChunkMetaData{d: d}, err
}

// SavedHash returns the metadata hash a chunk loaded with hash h gets when it
// is saved now, adding the entry it needs. h is 0 for a chunk this server
// generated.
func (m *ChunkMetaData) SavedHash(h uint64, dim Dimension, seed int64, generator int32) (uint64, error) {
	if m == nil || m.d == nil {
		return 0, errors.New("mcdb: no chunk metadata dictionary")
	}
	return m.d.savedHash(h, dim, seed, generator)
}

// Encode returns the dictionary's level key value and whether it changed
// since the last Encode. After a failed save, call MarkDirty.
func (m *ChunkMetaData) Encode() ([]byte, bool, error) { return m.d.encode() }

// MarkDirty makes the next Encode return the dictionary again.
func (m *ChunkMetaData) MarkDirty() { m.d.markDirty() }

// ActorIDFromKey reads an actor ID as BDS stores it in digp and actorprefix
// keys (8 bytes).
func ActorIDFromKey(b []byte) int64 { return actorIDFromKey(b) }

// AppendActorKey appends an actor ID as BDS stores it in digp and actorprefix
// keys.
func AppendActorKey(b []byte, id int64) []byte { return appendActorKey(b, id) }
