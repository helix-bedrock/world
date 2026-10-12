// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/block/cube/pos.go
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/block/cube/range.go
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/block/cube/face.go

// Package cube holds the block positions, height ranges and faces used by the
// world format. It contains only plain values with no engine behaviour, so the
// engine can build on it later.
//
// All types are values and safe to use from any goroutine.
package cube

import "fmt"

// Pos is the position of a block: X, Y and Z.
type Pos [3]int

// String formats the Pos as (x,y,z).
func (p Pos) String() string {
	return fmt.Sprintf("(%v,%v,%v)", p[0], p[1], p[2])
}

// X returns the X coordinate.
func (p Pos) X() int { return p[0] }

// Y returns the Y coordinate.
func (p Pos) Y() int { return p[1] }

// Z returns the Z coordinate.
func (p Pos) Z() int { return p[2] }

// OutOfBounds reports whether the Y coordinate lies outside r.
func (p Pos) OutOfBounds(r Range) bool {
	return p[1] > r[1] || p[1] < r[0]
}

// Within reports whether p lies within the inclusive bounds min and max.
func (p Pos) Within(minPos, maxPos Pos) bool {
	return p[0] >= minPos[0] && p[0] <= maxPos[0] &&
		p[1] >= minPos[1] && p[1] <= maxPos[1] &&
		p[2] >= minPos[2] && p[2] <= maxPos[2]
}

// Add returns p plus pos.
func (p Pos) Add(pos Pos) Pos {
	return Pos{p[0] + pos[0], p[1] + pos[1], p[2] + pos[2]}
}

// Sub returns p minus pos.
func (p Pos) Sub(pos Pos) Pos {
	return Pos{p[0] - pos[0], p[1] - pos[1], p[2] - pos[2]}
}

// Side returns the position next to p on the given face. An invalid face
// returns p unchanged.
func (p Pos) Side(face Face) Pos {
	switch face {
	case FaceUp:
		p[1]++
	case FaceDown:
		p[1]--
	case FaceNorth:
		p[2]--
	case FaceSouth:
		p[2]++
	case FaceWest:
		p[0]--
	case FaceEast:
		p[0]++
	}
	return p
}

// Range is the vertical extent of a dimension in blocks: the minimum and
// maximum Y, both inclusive.
type Range [2]int

// Min returns the minimum Y.
func (r Range) Min() int { return r[0] }

// Max returns the maximum Y.
func (r Range) Max() int { return r[1] }

// Height returns Max minus Min.
func (r Range) Height() int { return r[1] - r[0] }

// Face is one of the six sides of a block. The values match Bedrock's.
type Face int

// The six faces, in Bedrock's order.
const (
	FaceDown Face = iota
	FaceUp
	FaceNorth
	FaceSouth
	FaceWest
	FaceEast
)

// Opposite returns the face on the other side of the block. An invalid face
// returns itself.
func (f Face) Opposite() Face {
	switch f {
	case FaceDown:
		return FaceUp
	case FaceUp:
		return FaceDown
	case FaceNorth:
		return FaceSouth
	case FaceSouth:
		return FaceNorth
	case FaceWest:
		return FaceEast
	case FaceEast:
		return FaceWest
	}
	return f
}

// String returns the face's name in lower case.
func (f Face) String() string {
	switch f {
	case FaceDown:
		return "down"
	case FaceUp:
		return "up"
	case FaceNorth:
		return "north"
	case FaceSouth:
		return "south"
	case FaceWest:
		return "west"
	case FaceEast:
		return "east"
	}
	return fmt.Sprintf("Face(%d)", int(f))
}
