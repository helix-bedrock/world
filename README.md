# world

Minecraft: Bedrock Edition worlds for Go: the LevelDB database, chunks, block
state upgrading and `level.dat`. Worlds it saves open in BDS and in the game.

**Status as of 2026-10-29:** in development. Storage is one-to-one with BDS 1.26.60: a world BDS saved loads and saves back byte for byte, and stores are pluggable (`KV`, `WorldStore`, `PlayerStore`). A world opens without a block registry for level keys, players and level.dat. `chunk.BlockRegistry` asks only for what chunk code uses: states, light emission and dampening, and network hashes.

Part of [Helix](https://github.com/helix-bedrock), an open-source Minecraft: Bedrock
Edition server engine in Go. It does not depend on the engine and can be used on
its own.

## Packages

| Package | What it has |
|---|---|
| `leveldb` | LevelDB with Bedrock's zlib and raw deflate compression (goleveldb with fixes, no panics) |
| `chunk` | Chunks and sub chunks in memory, palettes, block storage, disk and network encoding |
| `blockupgrader` | Upgrades block states saved by older Bedrock versions (pmmp's upgrade schemas) |
| `mcdb` | The world provider, one-to-one with BDS 1.26.60: chunks, entities, block entities, chunk metadata, level keys, players, plugin namespaces, 5-second batched writes; pluggable through `KV`, `WorldStore` and `PlayerStore` |
| `mcdb/leveldat` | Reading and writing `level.dat`, keeping fields it does not know |
| `cube` | Block positions, height ranges and faces |

## Install

```bash
go get github.com/helix-bedrock/world
```

## Example

`mcdb` needs a `chunk.BlockRegistry` that maps block states to runtime IDs; the
engine provides one, tests use a small fake.

```go
db, err := mcdb.Open("worlds/My World", blocks)
if err != nil {
	return err
}
defer db.Close()

it := db.NewColumnIterator(nil) // nil: every dimension
defer it.Release()
for it.Next() {
	pos, col := it.Position(), it.Column()
	_ = pos
	_ = col.Chunk // blocks, biomes; col.Entities, col.BlockEntities
}
return it.Error()
```

## Supported versions

Chunks are written as Minecraft 1.26.52 writes them (chunk version 41, sub chunk
version 9). Block states from older worlds are upgraded up to 1.26.50.

## Documentation

API docs: <https://pkg.go.dev/github.com/helix-bedrock/world>

## Contributing

See the [contributing guide](https://github.com/helix-bedrock/.github/blob/main/CONTRIBUTING.md).
AI-assisted contributions follow the
[AI policy](https://github.com/helix-bedrock/.github/blob/main/AI_POLICY.md).

## License

MIT. See [`LICENSE`](LICENSE). Ported code and its sources (goleveldb, Dragonfly,
worldupgrader, pmmp's schemas) are listed in [`CREDITS.md`](CREDITS.md); their
licenses are in [`licenses/`](licenses).
