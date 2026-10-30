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
go test -race -short ./...                 # quick run
go test -race ./...                        # full run; leveldb alone takes minutes
go test -run=^$ -fuzz=FuzzDiskDecode -fuzztime=30s ./chunk
```

## Layout

- `leveldb`: goleveldb (syndtr `126854a`) plus open upstream PRs, Bedrock
  compression types 2 (zlib) and 4 (raw deflate), and no panics. See
  `CREDITS.md` for the full list.
- `chunk`: chunks, sub chunks, palettes and paletted storage; disk and network
  encoding; `Upgrader` for legacy (pre-1.13) IDs and old block states.
- `blockupgrader`: applies pmmp's block upgrade schemas in `schemas/`.
- `mcdb`: the world provider. Keys in `keys.go`, chunk/entity/block entity
  storage in `db.go`, the column iterator, players.
- `mcdb/leveldat`: `level.dat` reading and writing.
- `cube`: `Pos`, `Range` and `Face`, shared by the packages above.

## Invariants: do not break these

1. A world saved by this repo opens in the current Minecraft client and in BDS.
   The client is stricter than BDS: it crashed on Dragonfly's format (chunk
   version 42, biome copy markers, zero-layer sub chunks, `6` keys).
2. Disk output matches a world saved by the game. When you change what is
   written, save a world in the game, write it with this repo, and compare the
   keys and value sizes one by one.
3. Never panic, including on corrupt worlds. Decoders check every length, index
   and palette size; bad data returns an error.
4. No package-level mutable state. Schemas, legacy tables and buffer pools live
   in values the caller creates (`chunk.NewUpgrader`, `blockupgrader.New`).
5. Unknown `level.dat` fields are kept and written back unchanged.
6. Never import the engine or `helix-api`.
7. Storage is one-to-one with BDS (currently 1.26.60): every key BDS writes,
   its format and how BDS reads it. Next work (Helix build step 1, see the
   "Engine design" section of `../.github/API_DESIGN.md`): a full key
   inventory checked against BDS, and pluggable world and player stores with
   LevelDB as the default.

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
