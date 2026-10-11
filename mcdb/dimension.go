package mcdb

import (
	"fmt"

	"github.com/helix-bedrock/world/cube"
)

// ChunkPos is the position of a chunk column: X and Z in chunks, not blocks.
type ChunkPos [2]int32

// Dimension is a dimension's ID as stored in Bedrock world keys.
type Dimension int32

// The vanilla dimensions.
const (
	Overworld Dimension = 0
	Nether    Dimension = 1
	End       Dimension = 2
)

// Range returns the dimension's height range, and false for an unknown ID.
// The ranges match Dragonfly's (server/world/dimension.go at 1793369).
func (d Dimension) Range() (cube.Range, bool) {
	switch d {
	case Overworld:
		return cube.Range{-64, 319}, true
	case Nether:
		return cube.Range{0, 127}, true
	case End:
		return cube.Range{0, 255}, true
	}
	return cube.Range{}, false
}

// String returns the dimension's name.
func (d Dimension) String() string {
	switch d {
	case Overworld:
		return "overworld"
	case Nether:
		return "nether"
	case End:
		return "end"
	}
	return fmt.Sprintf("Dimension(%d)", int32(d))
}
