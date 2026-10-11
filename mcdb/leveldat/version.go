// Ported from Dragonfly (MIT):
// https://github.com/df-mc/dragonfly/blob/1793369709ceed934675d69e86ff6c828e4216c8/server/world/mcdb/leveldat/version.go
//
// Changed for Helix: the version array is computed by a function instead of
// a package variable set in init.

package leveldat

import (
	"strconv"
	"strings"
)

// Version is the level.dat storage version written by Helix.
const Version = 10

// versionArray turns a version such as "1.26.50" into the five-number form
// level.dat uses, padding with zeros: [1 26 50 0 0].
func versionArray(v string) []int32 {
	out := make([]int32, 5)
	for i, p := range strings.SplitN(v, ".", 5) {
		n, _ := strconv.ParseInt(p, 10, 32)
		out[i] = int32(n)
	}
	return out
}
