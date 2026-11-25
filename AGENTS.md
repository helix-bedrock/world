# AGENTS.md

Rules for AI coding agents working in this repo. Also read the org-wide rules in
`../.github/AGENT_RULES.md` (online:
<https://github.com/helix-bedrock/.github/blob/main/AGENT_RULES.md>). Where they
disagree, this file wins for this repo.

## What this repo is

Reading and writing Minecraft: Bedrock Edition worlds: the LevelDB database,
chunk encoding, block state upgrading and `level.dat`. It is a standalone
library: it never imports the engine and knows nothing about blocks beyond the
`chunk.BlockRegistry` the caller passes in. It does not generate terrain, tick
blocks or run entities. NBT and varints come from `helix-bedrock/protocol`.

## Commands

```bash
go build ./...
go vet ./...
golangci-lint run
go test -short ./...                 # quick run
go test ./...                        # full run; leveldb alone takes minutes
go test -run=^$ -fuzz=FuzzDiskDecode -fuzztime=30s ./chunk
```

## Layout

Detail per package is in [`LAYOUT.md`](LAYOUT.md); read the entry for the
package you touch and update it when behaviour changes.

- `leveldb`: goleveldb fork with Bedrock compression, no panics, streamed big values.
- `lsm`: Helix's own engine for BDS LevelDB files (converter's BDS side, `ReadOnly`).
- `hx/store`: record store under the `.hx` format (journal, snapshots, 0 allocs; `TestCrash`).
- `hx`: Helix world format (block table, terrain, objects, rounds, players, live backups).
- `hxdb`: `hx` behind mcdb's store interfaces; split containers, linked stores, staged loads.
- `convert`: lossless BDS <-> Helix world conversion (`TestRoundTrip`, `TestBDSWorld`).
- `chunk`, `blockupgrader`: chunk encoding, palettes, legacy and pmmp state upgrades.
- `mcdb`, `mcdb/leveldat`: BDS-format world provider and `level.dat`.
- `cube`: `Pos`, `Range`, `Face`.

## Invariants: do not break these

1. A world saved by this repo opens in the current Minecraft client and in BDS.
   The client is stricter than BDS: it crashed on Dragonfly's format (biome
   copy markers, zero-layer sub chunks). Chunk version 42 and `6` keys are
   what BDS 1.26.60 writes, so mcdb writes them again; check a world in the
   client after changing chunk output.
2. Disk output matches a world saved by the game. When you change what is
   written, save a world in the game, write it with this repo, and compare the
   keys and value sizes one by one.
3. Never panic, including on corrupt worlds. Decoders check every length, index
   and palette size; bad data returns an error.
4. No package-level mutable state. Schemas, legacy tables and buffer pools live
   in values the caller creates (`chunk.NewUpgrader`, `blockupgrader.New`).
5. Unknown `level.dat` fields are kept and written back unchanged.
6. Never import the engine or `helix-api`.
7. The BDS side (`lsm`, `leveldb`, `mcdb`) is one-to-one with BDS (currently
   1.26.60); Helix's own format (`hx`) must convert to and from it without
   loss. For the BDS side: every key BDS writes,
   its format and how BDS reads it. Check changes by round-tripping a world
   BDS saved (load every column, store it, compare every key byte for byte)
   and regenerate the key map with the `dump-storage-keys` skill on a new
   BDS version.

## Gotchas

- The full `leveldb` test suite is slow under `-race`; use `-short` or `-run`
  while working. `TestLRUCache_GetLatency` is skipped under `-race`.
- `leveldb` compares many errors with `==` on purpose: its sentinel errors are
  never wrapped inside the package. Keep it that way, or switch the comparison
  to `errors.Is` in the same change.
- `blockupgrader/schemas/0351_1.26.40_to_1.26.50.json` is not from pmmp. It
  comes from Dragonfly's worldupgrader and its default values for new
  properties were not checked against BDS. Replace it when pmmp publishes one.
- New block states use version `18168865` (1.21.60.33) in the 1.26.50 palette,
  while worlds saved by 1.26.40+ store `18491392`. Do not "fix" one to match
  the other.
- `chunk/legacy_states.nbt` and `blockupgrader/schemas/` are data files; update
  them from their sources and record the commit in `CREDITS.md`.
- Test worlds are not in the repo. Use your own; never commit worlds or paths
  to them.
