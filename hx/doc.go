// Package hx is the Helix world format: worlds stored in record store files
// (package hx/store) instead of BDS's LevelDB, converted to and from BDS
// worlds without loss. The byte layout is in FORMAT.md.
//
// A chunk is stored as parts, each its own record, read and written on its
// own. The terrain part holds the sub chunks, the biomes and height map, and
// every other small chunk key BDS writes. Its block palettes are numbers into
// the world's block table, which keeps every block state once, as the exact
// bytes BDS stores for it: loading a chunk is a table lookup per palette
// entry, never an NBT decode, and an export writes BDS's bytes back.
package hx
