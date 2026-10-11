//go:build race

package cache

// raceEnabled is true under -race, which slows code down several times and
// makes latency checks meaningless.
const raceEnabled = true
