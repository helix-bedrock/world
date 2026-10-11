// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/world/chunk/col.go

package chunk

import (
	"github.com/helix-bedrock/world/cube"
)

// Column is a chunk with everything saved alongside it: entities, block
// entities, its tick and pending scheduled block updates.
type Column struct {
	Chunk           *Chunk
	Entities        []Entity
	BlockEntities   []BlockEntity
	Tick            int64
	ScheduledBlocks []ScheduledBlockUpdate
}

// BlockEntity is the saved NBT data of a block entity at Pos.
type BlockEntity struct {
	Pos  cube.Pos
	Data map[string]any
}

// Entity is the saved NBT data of an entity with its unique ID.
type Entity struct {
	ID   int64
	Data map[string]any
}

// ScheduledBlockUpdate is a block update due at Tick for the block at Pos.
type ScheduledBlockUpdate struct {
	Pos   cube.Pos
	Block uint32
	Tick  int64
}
