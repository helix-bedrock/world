# The Helix world format (`.hx`)

Byte-level layout of Helix worlds. The design and the reasons for it are in
the `.github` repo's `API_DESIGN.md`, "World data redesign", sections 4 to 6.
All integers are little-endian; varints are unsigned LEB128 (Go's
`binary.AppendUvarint`). Checksums are CRC-32C (Castagnoli).

This file grows with the code: the record store (`hx/store`) is described
here; the chunk parts, block table, players and level data come next.

## Record store files

Every `.hx` file in a world is a record store file:

| Offset | Size | Content |
| --- | --- | --- |
| 0 | 4 KB | file header |
| 4096 | 4 KB | superblock slot A |
| 8192 | 4 KB | superblock slot B |
| 12288 | rest | records, each starting at a multiple of 64 |

Each slot sits in its own 4 KB sector, so a torn write of one never reaches
the other.

**File header** (12 bytes used): `"HXST"`, format version (u16, 1), two zero
bytes, CRC of the first 8 bytes (u32).

**Superblock** (48 bytes used): `"HXSB"`, checkpoint count `seq` (u64), round
(u64), index record offset (u64), index record size (u64), end of used space
(u64), CRC of the first 44 bytes (u32). Odd checkpoints are written to slot A,
even ones to B; the valid superblock with the higher `seq` is current. A file
with no valid superblock has had no checkpoint: its records come from the
journal only.

**Record**: a 40-byte header, the key, the body, then zero padding to the next
multiple of 64.

| Offset | Size | Content |
| --- | --- | --- |
| 0 | 4 | `"HXR"` 0x01 |
| 4 | 1 | kind (0 = index record, 1 to 255 = the caller's) |
| 5 | 1 | compression: 0 none, 1 zstd (one frame, no zstd checksum) |
| 6 | 2 | key length (at most 4096) |
| 8 | 8 | round that wrote it |
| 16 | 8 | stored body length |
| 24 | 8 | body length decompressed |
| 32 | 4 | CRC of the stored body |
| 36 | 4 | CRC of bytes 0 to 35, then the key |

Records describe themselves, so a file whose index is lost can be rebuilt by
scanning every 64-byte boundary for a valid header.

**Index record**: kind 0, empty key, written by a checkpoint. Its body, before
compression: count (varint), then per key in ascending byte order: key length
(varint), key, kind (byte), offset (varint), record size before padding
(varint), body length decompressed (varint), round (varint).

Saved records are never written over: a new version goes to free space, and
the space of the old one is reused only after the round that replaced it has
committed and no reader can still see it.

## Journal

`journal/<seq as 16 hex digits>.hxj`, oldest first. A segment is a 32-byte
header, `"HXJ"` 0x01, `seq` (u64), the round before its first commit (u64),
8 zero bytes, CRC of the first 28 bytes (u32); then commit records.

**Commit record**: body length (u32), CRC of the body (u32), body:

- round (varint), number of files (varint);
- per file: path length (varint), path (relative, `/`-separated, ends in
  `.hx`), number of changes (varint);
- per change, in ascending key order: 1 = put or 2 = delete (byte), key
  length (varint), key; for a put: kind (byte), offset (varint), record size
  before padding (varint), body length decompressed (varint).

A round counts once its commit record is complete on disk. Opening a store
replays every commit newer than each file's superblock round, checkpoints
every file and starts a new segment. A torn record can only end the newest
segment; anywhere else it is corruption.

## Save round order

1. Write the new records into free space.
2. Force the written files (and new directories) to disk.
3. Append the commit record and force the journal to disk.

A checkpoint writes the index record and forces it, then writes the
superblock slot not in use and forces it. The journal is emptied (a new
segment, older ones removed) only once every file changed since is
checkpointed.
