// Package lsm is Helix's own key-value engine for Minecraft: Bedrock Edition
// worlds. It reads and writes the LevelDB files BDS uses (log, manifest,
// CURRENT, sorted tables with Bedrock's raw deflate blocks), so a world it
// writes opens in BDS and the client and the other way round, but it is built
// for what a server does with a world rather than as a general database:
//
//   - Reads never wait for writes. A write builds the new state and swaps it
//     in; a reader works on a snapshot (sequence number plus the memtables and
//     table set of that moment) with no lock held while it reads.
//   - Big values are never held whole. A value of hundreds of megabytes is
//     written from a stream into a table of its own and read back as a
//     stream, and its blocks never enter the block cache.
//   - Memory is flat. The memtable is a pointer-free arena skiplist; the block
//     cache holds small blocks only, under a byte budget the caller sets.
//   - Missing keys are cheap. New tables carry LevelDB's bloom filter
//     ("leveldb.BuiltinBloomFilter2", the one Mojang's LevelDB example
//     configures), so a key that is not there costs no data block reads.
//   - Background work is the caller's. Flushes and compactions run on a
//     goroutine the DB owns but can be paced by the caller (Options.Pace), so
//     an engine can give foreground reads priority over compaction.
//
// The formats are LevelDB's, documented in Google's LevelDB repository
// (doc/log_format.md, doc/table_format.md, doc/impl.md); no LevelDB code was
// copied. Compression types are LevelDB's 0 (none) and 1 (snappy, read only)
// and Bedrock's 2 (zlib, read only) and 4 (raw deflate, what Bedrock writes).
package lsm
