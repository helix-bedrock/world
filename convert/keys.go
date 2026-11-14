// Package convert turns BDS worlds into Helix worlds (package hx) and back,
// without loss: a world converted to Helix and back, without being played,
// has every LevelDB key and value it had, byte for byte. BDS's side is read
// and written with package lsm.
//
// Every key goes somewhere: chunk keys into their chunk's terrain or objects
// record, sub chunks translated to block table palettes (or kept in BDS's form
// when that would not give the same bytes back), block entities and entities
// as shells and payloads, players into players.hx, and everything else, keys
// this package does not know included, into level.hx under its own name.
package convert

import (
	"encoding/binary"

	"github.com/helix-bedrock/world/hx"
)

// Key prefixes of BDS 1.26.60 that are not chunk keys.
const (
	digpPrefix   = "digp"
	actorPrefix  = "actorprefix"
	playerPrefix = "player_"
)

// tagSubChunk is the chunk tag of a sub chunk, followed by its Y index.
const tagSubChunk = '/'

// tagData3D holds the height map and 3D biomes.
const tagData3D = '+'

// maxDimension bounds the dimension IDs taken as chunk keys. Higher ones are
// kept as level keys, which is just as lossless.
const maxDimension = 1024

// chunkTags are the chunk tags BDS 1.26.60 reads or writes (mcdb/keys.go),
// plus Helix's '|' (legacy states). Keys with other tags are level keys.
var chunkTags = [256]bool{
	',': true, 'v': true, '+': true, '-': true, '.': true, '0': true, '1': true,
	'2': true, '3': true, '4': true, '5': true, '6': true, '7': true, '8': true,
	'9': true, ':': true, ';': true, '<': true, '=': true, '?': true, '@': true,
	'A': true, 'w': true, 'y': true, 'z': true, '|': true,
}

// chunkID is a chunk in a dimension.
type chunkID struct {
	dim hx.Dimension
	pos hx.ChunkPos
}

// parseChunkKey reads a BDS chunk key: x, z (LE int32), the dimension (LE
// int32, left out for the Overworld), the tag, and for a sub chunk its index.
func parseChunkKey(k []byte) (id chunkID, tag byte, sub int8, ok bool) {
	var dim int32
	switch len(k) {
	case 9, 10:
	case 13, 14:
		dim = int32(binary.LittleEndian.Uint32(k[8:])) //nolint:gosec // raw int32 bits
		if dim <= 0 || dim >= maxDimension {
			return chunkID{}, 0, 0, false
		}
	default:
		return chunkID{}, 0, 0, false
	}
	tagAt := 8
	if dim != 0 {
		tagAt = 12
	}
	tag = k[tagAt]
	switch len(k) - tagAt {
	case 1:
		if tag == tagSubChunk || !chunkTags[tag] {
			return chunkID{}, 0, 0, false
		}
	case 2:
		if tag != tagSubChunk {
			return chunkID{}, 0, 0, false
		}
		sub = int8(k[tagAt+1]) //nolint:gosec // raw byte
	}
	id = chunkID{
		dim: hx.Dimension(dim),
		pos: hx.ChunkPos{int32(binary.LittleEndian.Uint32(k)), int32(binary.LittleEndian.Uint32(k[4:]))}, //nolint:gosec // raw int32 bits
	}
	return id, tag, sub, true
}

// appendChunkPrefix appends a chunk's key prefix: x, z and, outside the
// Overworld, the dimension.
func appendChunkPrefix(b []byte, id chunkID) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(id.pos[0])) //nolint:gosec // raw int32 bits
	b = binary.LittleEndian.AppendUint32(b, uint32(id.pos[1])) //nolint:gosec // raw int32 bits
	if id.dim != hx.Overworld {
		b = binary.LittleEndian.AppendUint32(b, uint32(id.dim)) //nolint:gosec // raw int32 bits
	}
	return b
}

// digpKey returns the digp key of a chunk.
func digpKey(id chunkID) []byte {
	return appendChunkPrefix([]byte(digpPrefix), id)
}

// parseDigpKey reads a digp key's chunk.
func parseDigpKey(k []byte) (chunkID, bool) {
	rest := k[len(digpPrefix):]
	if len(rest) != 8 && len(rest) != 12 {
		return chunkID{}, false
	}
	id := chunkID{pos: hx.ChunkPos{int32(binary.LittleEndian.Uint32(rest)), int32(binary.LittleEndian.Uint32(rest[4:]))}} //nolint:gosec // raw int32 bits
	if len(rest) == 12 {
		d := int32(binary.LittleEndian.Uint32(rest[8:])) //nolint:gosec // raw int32 bits
		if d <= 0 || d >= maxDimension {
			return chunkID{}, false
		}
		id.dim = hx.Dimension(d)
	}
	return id, true
}
