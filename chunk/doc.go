// Package chunk holds Bedrock chunks in memory and encodes and decodes them
// for disk (LevelDB worlds) and network. A chunk is a column of 16x16x16 sub
// chunks, each with block storages made of a palette and packed indices.
//
// The package does not know any blocks itself: callers pass a BlockRegistry
// that maps block states to runtime IDs. Decoding saved worlds also takes an
// Upgrader, which converts block states saved by older versions.
//
// A Chunk is not safe for concurrent writes; callers that share one must
// synchronise. Encoding and decoding functions may run on any goroutine.
package chunk
