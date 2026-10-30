// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/world/mcdb/keys.go
//
// Changed for Helix: the full set of chunk tags BDS 1.26.60 reads or writes,
// and actor keys in the byte order BDS uses.

package mcdb

import "encoding/binary"

// Keys on a per-sub chunk basis. These are prefixed by the chunk coordinates and subchunk ID.
const (
	keySubChunkData = '/' // 2f
)

// Keys on a per-chunk basis. These are prefixed by only the chunk coordinates.
const (
	// keyVersion holds a single byte with the chunk's format version.
	keyVersion = ',' // 2c
	// keyVersionOld was replaced by keyVersion. BDS reads it only when
	// keyVersion is missing and never writes it.
	keyVersionOld = 'v' // 76
	// key3DData holds a 16x16 int16 height map followed by 3D biomes.
	key3DData = '+' // 2b
	// key2DData is a height map with 2D biomes, from before the world height
	// change. BDS deletes it whenever it writes key3DData.
	key2DData = '-' // 2d
	// key2DDataLegacy is an older 2D format. BDS deletes it on every save.
	key2DDataLegacy = '.' // 2e
	// keyLegacyTerrain is the pre-sub-chunk terrain. BDS deletes it on every save.
	keyLegacyTerrain = '0' // 30
	// keyBlockEntities holds block entity compounds appended to each other
	// (not a list). Each compound holds its own position.
	keyBlockEntities = '1' // 31
	// keyEntitiesOld holds entity compounds appended to each other. BDS
	// only migrates them for chunk versions below 40 and deletes the key
	// on every save.
	keyEntitiesOld = '2' // 32
	// keyPendingScheduledTicks holds {currentTick, tickList} as NBT.
	keyPendingScheduledTicks = '3' // 33
	// keyBlockExtraData is legacy. BDS deletes it on every save.
	keyBlockExtraData = '4' // 34
	// keyFinalisation holds an LE int32 generation state (chunk.Finalisation).
	// A chunk without it loads as finished.
	keyFinalisation = '6' // 36
	// keyConversionData is legacy. BDS deletes it on every save.
	keyConversionData = '7' // 37
	// keyChecksums is legacy. BDS deletes it on every save.
	keyChecksums = ';' // 3b
	// keyMetaDataHash holds an LE uint64: the hash of the chunk's entry in
	// the LevelChunkMetaDataDictionary level key.
	keyMetaDataHash = '?' // 3f
	// keyBlendingData is written with keyMetaDataHash: two bytes (0, 11)
	// for a chunk with no blending, more for chunks that blend with old
	// terrain.
	keyBlendingData = '@' // 40
	// keyActorDigestVersion is one byte, 0, written with every digp record.
	keyActorDigestVersion = 'A' // 41
	// keyLegacyStates marks a chunk saved while its block states derived from surrounding blocks were still
	// the defaults of an upgrade, so that they are recalculated when it is next loaded. Helix-only: BDS
	// ignores tags it does not know.
	keyLegacyStates = '|' // 7c

	keyEntityIdentifiers = "digp"

	keyEntity = "actorprefix"
)

// legacyChunkTags are deleted every time BDS saves a chunk.
var legacyChunkTags = [...]byte{key2DData, key2DDataLegacy, keyLegacyTerrain, keyEntitiesOld, keyBlockExtraData, keyConversionData, keyChecksums}

// Keys on a per-world basis. These are found only once in a leveldb world save.
const (
	keyChunkMetaData = "LevelChunkMetaDataDictionary"
)

// actorKeySize is the length of an actor's id as stored in digp records
// and actorprefix keys.
const actorKeySize = 8

// appendActorKey appends id the way BDS stores it in keys: the high half
// negated, then the low half, both big-endian. A unique id 0xfffffffa00000005
// becomes 00000006 00000005.
func appendActorKey(b []byte, id int64) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(-int32(id>>32))) //nolint:gosec // raw int32 bits
	return binary.BigEndian.AppendUint32(b, uint32(id))          //nolint:gosec // low half
}

// actorIDFromKey reverses appendActorKey. b must hold actorKeySize bytes.
func actorIDFromKey(b []byte) int64 {
	hi := -int32(binary.BigEndian.Uint32(b)) //nolint:gosec // raw int32 bits
	return int64(hi)<<32 | int64(binary.BigEndian.Uint32(b[4:]))
}

// entityIndex returns the actorprefix key of an actor.
func entityIndex(id int64) []byte {
	return appendActorKey([]byte(keyEntity), id)
}
