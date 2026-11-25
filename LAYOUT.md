# Code layout

Package-by-package map of the library. Not loaded into agent context; search it for the package you are about to touch.

- `leveldb`: goleveldb (syndtr `126854a`) plus open upstream PRs, Bedrock
  compression types 2 (zlib) and 4 (raw deflate), no panics, and big values
  streamed instead of held whole (`GetStream`, `Transaction.PutStream`,
  `table/stream.go`). See `CREDITS.md` for the full list.
- `lsm`: Helix's own engine for the same files (replacing `leveldb` step by
  step): log, manifest, tables with raw deflate blocks and LevelDB's bloom
  filter, a pointer-free arena memtable, a `ReadOnly` open that writes
  nothing (the converter reads BDS worlds with it), snapshot reads that never wait for
  writes, big values streamed into tables of their own and copied as stored
  by compaction (iterators stream blocks over 64 KB stored, so a value
  alone in its block is copied without inflating it), a small-block cache. Checked against `leveldb` both ways and
  on BDS worlds (`TestBDSWorld`, `LSM_BDS_WORLD`).
- `hx/store`: the record store under Helix's own world format (`.hx`, design
  in `.github/API_DESIGN.md` "World data redesign", bytes in `hx/FORMAT.md`):
  checksummed records never written over, a flat in-memory index per file
  within a byte budget, save rounds made atomic across files by one journal,
  checkpoints to A/B superblocks, generations so freed space is reused only
  when no read can see it, streamed big records, snapshots of one round
  across every file (`snapshot.go`), records copied as stored (`raw.go`),
  writes of the bytes a key already holds skipped (CRC-32C of the body kept
  in memory per index entry for records this run wrote; `TestUnchangedSkipped`),
  `ReadOnly` opens. Reads and steady-state
  rounds must stay at 0 allocations (`bench_test.go`). `TestCrash` stops a
  fake disk (`memfs_test.go`) at random moments, losing or tearing unforced
  writes; any change to write order must keep it passing.
- `hx`: the Helix world format on top of `hx/store`: the block table
  (`blocks.go`, every block state once as BDS's palette bytes, IDs resolved
  to runtime IDs) and the chunk terrain record (`terrain.go`, `chunk.go`:
  packed words as BDS stores them, palettes of block table IDs, the small
  BDS chunk keys kept byte for byte), the objects record (`objects.go`:
  block entities, ticks and entities as BDS stores them), and the world
  provider (`world.go`: regions of 32x32 chunks, reads shared by World and
  Snapshot through `view`; `round.go`: save rounds, atomic across files;
  `players.go`: BDS's player keys; `leveldat.go`: level.dat in world.hx)
  and live backups (`backup.go`: from a snapshot to a `BackupTarget`, full
  or incremental, paced; `backup_dir.go`: backup folders, `Restore`,
  `Round.RestoreChunk` into a running world; `backup_archive.go`: one-stream
  archives).
  Block entities and entities are shells with fields over 512 bytes cut
  into payload records (`objects.go`) until 4 KB is left; splicing them
  back must give BDS's bytes. Decoding a terrain record and a warm terrain
  read must stay at 0 allocations (`BenchmarkDecodeTerrain`,
  `BenchmarkLoadTerrain`).
- `hxdb`: the provider engines run on: an `hx` world behind mcdb's
  `WorldStore`, `PlayerStore` and `BlockEntityLoader`, so Helix uses the
  same calls it used on LevelDB. Chunk columns are turned into hx's terrain
  and objects records with BDS's chunk keys (version, metadata hash through
  `mcdb.ChunkMetaData`, finalisation, blending, Extra), one save round
  committed every 5 s or at 64 MB; a chunk or player stored again before its
  round is committed commits that round first, level keys are read from the
  round. Containers are stored split (`containers.go`, `hx/slots.go`): a
  block entity's `Items` go out as one slot record per item plus a summary, each slot encoded
  alone and written only when its bytes changed, in side rounds committed as they reach
  `RoundSize` (so a save of chests of gigabytes holds about one round in memory); reads put
  `Items` back (whole, or light from the summary for `Config.LightItems` ids in
  `EachBlockEntity`); export puts them back, `RestoreChunk` copies them. A store waits while
  the shared round is over twice `RoundSize` (`waitForRoom`). `StoreLinked` (mcdb's optional
  `LinkedStore`) stores a column and players in one commit, after committing everything older,
  so items moved between a player and a container never land on one side only. Item contents (`hx/items.go`, `hxdb/items.go`): the items held
  inside an item (a shulker box's `Items`) may live in `items.hx`, keyed by their SHA-256,
  committed on their own (`StoreItems`) before any `helix:items` reference to them is saved;
  a book's `pages` the same way under `helix:pages`; export puts `Items` and `pages` back, at
  any depth (`InlineItemsNBT`); records no reference uses are not removed yet.
  `KeepBlockEntities` keeps shells and their payloads as stored
  (`ObjectsWriter.KeepBlockEntity`). Chunks only shown load in a stage of
  their own: `LoadTerrain` (no objects) and `EachBlockEntityView` (block
  entities without the fields named, a cut field's payload read only for
  its name, `hx` `ExpandShellWithout`). `TestColumnRoundTrip`,
  `TestStagedLoad`.
- `convert`: BDS world <-> Helix world, lossless (`Import`, `Export`;
  `ExportWorld` exports a world a server has open, from a snapshot). Every
  key goes somewhere; sub chunks that would not re-encode to the same bytes
  stay raw. `TestRoundTrip` covers every key family; `TestBDSWorld`
  (`HX_BDS_WORLD` = a BDS world folder) round-trips a real world and
  compares every key.
- `chunk`: chunks, sub chunks, palettes and paletted storage; disk and network
  encoding; `Upgrader` for legacy (pre-1.13) IDs and old block states.
- `blockupgrader`: applies pmmp's block upgrade schemas in `schemas/`.
- `mcdb`: the world provider. Chunk tags and actor keys in `keys.go`, level
  keys in `levelkeys.go`, chunk/entity/block entity storage in `db.go` (a
  chunk's keys are read with one prefix iterator, `chunkread.go`, so each
  LevelDB block is read once; loads of chunks over 1 MB share
  `Config.LoadBudget`, `loadgate.go`), the
  5-second write buffer in `buffer.go`, block entities read and written one
  compound at a time in `blockentities.go` (`EachBlockEntity`,
  `LoadBlockEntity`, `Config.SkipBlockEntities`; a big value, kept stored
  compounds and new ones alike, is written as a stream, each compound
  encoded as the flush writes it), the chunk metadata in `meta.go`, height
  maps in `heightmap.go`, players in `player.go`, the `KV`/`WorldStore`/
  `PlayerStore` interfaces and plugin namespaces in `store.go`.
- `mcdb/leveldat`: `level.dat` reading and writing.
- `cube`: `Pos`, `Range` and `Face`, shared by the packages above.
