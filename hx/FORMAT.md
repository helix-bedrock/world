# The Helix world format (`.hx`)

Byte-level layout of Helix worlds. The design and the reasons for it are in
the `.github` repo's `API_DESIGN.md`, "World data redesign", sections 4 to 6.
All integers are little-endian; varints are unsigned LEB128 (Go's
`binary.AppendUvarint`). Checksums are CRC-32C (Castagnoli).

This file grows with the code. Described so far: the record store
(`hx/store`), the world's files, the block table, the terrain and objects
records, players and level keys. Shells and payloads come next.

## Files of a world

```
level.dat              a copy of world.hx's, for BDS tools
world.hx               key "format": format version (LE u32, 1);
                       key "level.dat": level.dat as BDS stores it
bds/                   the BDS world's other files (convert package)
blocks.hx              the block table
level.hx               world-wide keys
players.hx             players
journal/               the record store's journal
overworld/r.<x>.<z>.hx regions; also nether/, end/, dim<id>/ for others
```

A region holds 32x32 chunks: region X = chunk X >> 5 (rounding down, also
for negative chunks). A chunk part's key in its region is 3 bytes: the
chunk's index in the region as a big-endian u16, `(z & 31) * 32 + (x & 31)`,
then the part: 1 terrain, 2 objects (payloads add their number, see below).
A chunk exists when it has a terrain
record.

## Record kinds

| Kind | Record |
| --- | --- |
| 1 | block table entry (`blocks.hx`) |
| 2 | chunk terrain |
| 3 | chunk objects |
| 4 | world-wide key (`level.hx`) |
| 5 | player record (`players.hx`) |
| 6 | world metadata (`world.hx`) |
| 7 | payload: a field cut out of a shell |
| 8 | key list of an incremental backup (`backup.hx`) |

## Block table (`blocks.hx`)

Every block state the world's chunks use, once. Key: the entry's ID as a
big-endian u32; IDs run from 0 with none missing and never change. Body: the
block state exactly as BDS stores it in a sub chunk palette, one
little-endian NBT compound (`name`, `states`, `version`). Entries converted
from a BDS world keep BDS's bytes, whatever version wrote them; entries Helix
adds are written in the current form (states sorted by name, version
`chunk.CurrentBlockVersion`). A state the server does not know is kept, so it
exports unchanged. The table is written in the same save round as the chunks
that first use its new entries.

## Terrain record

A format version byte (1), then sections: a tag byte, the body length
(varint), the body. A reader skips tags it does not know.

| Tag | Section | Body |
| --- | --- | --- |
| 1 | chunk key | the BDS key's tag byte (such as `,` for the chunk version), then its value as BDS stores it |
| 2 | Data3D | BDS's height map (256 LE int16) and 3D biome storages, as BDS stores them |
| 3 | sub chunk | see below |
| 4 | raw sub chunk | the sub chunk's Y index (i8), then the sub chunk as BDS stores it, NBT palettes and all |

**Sub chunk** (tag 3): Y index (i8, -4 for the Overworld's lowest), the BDS
sub chunk version it is written back with (8 or 9), the layer count (at most
8), then per layer: bits per index (0, 1, 2, 3, 4, 5, 6, 8 or 16), the packed
indices as BDS stores them (LE u32 words, as many as the bits need: none for
0 bits), the palette length (varint, 1 to 4096), then each palette entry as a
block table ID (varint). Every index must point into the palette.

Raw sub chunks hold what the converter could not translate exactly. Sub
chunks with no layers are not written.

## Objects record

A format version byte (1), the number of payload records the chunk has
(varint), then sections as in the terrain record:

| Tag | Section | Body |
| --- | --- | --- |
| 1 | chunk key | a BDS chunk key that belongs with the objects: `2` entities (old worlds), `3` pending ticks, `:` random ticks; tag byte then value as BDS stores it |
| 2 | entity | the 8-byte actor ID as in BDS's `actorprefix` key, then the entity's shell; in the order of BDS's `digp` list. An empty shell is an ID in `digp` with no `actorprefix` record |
| 3 | block entity | a shell, in the order of BDS's `1` value (compounds back to back) |
| 4 | digp | empty: the chunk has a `digp` key, also with no entities |

Every other BDS chunk key goes in the terrain record.

**Shell**: cut count (varint); per cut, in order: where the field starts in
the whole NBT (varint), its length (varint), its payload number (varint);
then the NBT with the cut fields left out. A field is cut when the compound
is over 4 KB: fields of 512 bytes or more, largest first, until at most 4 KB
is left. A cut is a whole top-level field, type byte and name included, so
putting each payload back at its offset gives BDS's bytes unchanged.

**Payload record** (kind 7): key = the chunk's 2-byte index in the region,
part 3, payload number (big-endian u32); body = the cut field's bytes.
Payloads are numbered from 0 per chunk; a save with fewer deletes the rest.

## Level keys and players

`level.hx` keeps every world-wide BDS key under its BDS name, value as BDS
stores it. `players.hx` keeps BDS's player keys: `player_<id>` lookup records
(LE NBT `MsaId`, `SelfSignedId`, `ServerId`), one per login ID, and the
`player_server_<uuid>` record each points at, holding the player's NBT.

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

## Snapshots

A snapshot (`Store.Snapshot`) is the store as of one round, kept in memory
only: it pins that round's generation, so no record it can see is reused,
and each later round notes, before changing a key, what the key held. A
snapshot read looks at those notes, then at the current index.

A store opened `ReadOnly` replays its journal in memory and writes nothing.

## Backups

A backup copies records as stored (`Snapshot.ReadRaw`/`OpenRaw` into
`Batch.PutRaw`/`PutRawStream`): compressed body and body checksum unchanged,
the header rewritten with the round of the commit that writes the copy. The
commit checks every copied body against its checksum.

**Backup folder** (`DirTarget`): the world's store files with the records
copied, its plain files (`level.dat` from the snapshot, `bds/...`), and last
`backup.json` (`BackupInfo`: format, round shown, round it builds on, time,
record and byte counts). A full backup (`since` 0) is a world and opens as
one. An incremental backup holds only records written after round `since`,
plus `backup.hx`: one record (kind 8) per store file of the world, keyed by
the file's path, whose body is every key the file held (uvarint length and
bytes, repeated); a file without one held no keys. `Restore` applies a full
backup, then each incremental one, deleting keys its lists leave out.

**Archive** (`ArchiveWriter`, `ReadArchive`): `HXBK\x01`, then frames, each
a type byte. Strings and byte fields are a uvarint length and the bytes.

| Type | Frame | Fields |
| --- | --- | --- |
| 1 | record | file, key, kind (byte), compression (byte), size decompressed (uvarint), stored size (uvarint), body CRC-32C (u32 LE), stored body |
| 2 | keys | file, key list (as in `backup.hx`) |
| 3 | plain file | name, size (uvarint), bytes |
| 4 | finish | `BackupInfo` JSON, then CRC-32C (u32 LE) of every byte of the stream before it |
