<div align="center">

<img src=".github/helix-mark.svg" alt="Helix" width="120">

<p>
  <b>Minecraft: Bedrock Edition worlds in Go</b><br>
  Read and written byte for byte as BDS does, fast, low memory
</p>

[![Minecraft](https://img.shields.io/badge/minecraft-v1.26.60_%7C_1.26.62_(Bedrock)-black)](https://feedback.minecraft.net/hc/en-us/sections/360001186971-Release-Changelogs)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue)](LICENSE)
[![Status](https://img.shields.io/badge/status-in_development-orange)](STATUS.md)

</div>

## Why world?

Helix needs its worlds to be BDS's worlds. world reads and writes the LevelDB database, chunks and
`level.dat` byte for byte the way the Bedrock Dedicated Server (BDS) does, so the same world works
in BDS and in Helix: move it from one to the other and back, and nothing is lost or changed.

Existing Go world readers aim to load and save worlds the game accepts, not to match BDS byte for
byte, so we wrote our own. Owning the code also lets us optimize reading and writing for Helix
(copy-on-write snapshots, batched writes, pluggable stores) without ever changing what ends up on
disk.

world is part of [Helix](https://github.com/helix-bedrock), an open-source Bedrock server engine in
Go, but it does not depend on the engine and is a plain library that any Go project can use.

## How it is checked

The storage format comes from BDS 1.26.60: every LevelDB key it uses, the chunk tags and the
encoding of each value. Worlds BDS saved are loaded and saved back, and the bytes are compared with
BDS's own.

## Quick Start

```bash
go get github.com/helix-bedrock/world
```

`mcdb` needs a `chunk.BlockRegistry` that maps block states to runtime IDs to read chunks; the
engine provides one, tests use a small fake. Level keys, players and `level.dat` open without one.

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

## Packages

| Package | What it has |
|---|---|
| [`leveldb`](leveldb) | LevelDB with Bedrock's zlib and raw deflate compression (goleveldb with fixes, no panics) |
| [`chunk`](chunk) | Chunks and sub chunks in memory, palettes, block storage, disk and network encoding |
| [`blockupgrader`](blockupgrader) | Upgrades block states saved by older Bedrock versions (pmmp's upgrade schemas) |
| [`mcdb`](mcdb) | The world provider, one-to-one with BDS 1.26.60: chunks, entities, block entities, chunk metadata, level keys, players, plugin namespaces, 5-second batched writes; pluggable through `KV`, `WorldStore` and `PlayerStore` |
| [`mcdb/leveldat`](mcdb/leveldat) | Reading and writing `level.dat`, keeping fields it does not know |
| [`cube`](cube) | Block positions, height ranges and faces |

## Supported versions

Worlds are read and written as BDS 1.26.60 and 1.26.62 do. Chunks use chunk version 41 and sub chunk
version 9, as Minecraft has written them since 1.26.52. Block states from older worlds are upgraded
up to 1.26.50.

## Roadmap

What works today and what is left is in [STATUS.md](STATUS.md).

| Part | State |
|---|---|
| LevelDB with Bedrock's compression | ✅ done |
| Chunks, entities, block entities, chunk metadata | ✅ byte for byte with BDS 1.26.60 |
| Level keys, players, plugin namespaces, `level.dat` | ✅ byte for byte with BDS 1.26.60 |
| Pluggable stores | ✅ done |
| Block state upgrading from older worlds | ✅ up to 1.26.50 |

**The goal is to match the latest Minecraft version exactly.** After that, every time Minecraft
updates, world will be updated to the new version. The project is maintained.

## Contributing

- **Found a bug or a difference from BDS?** Open an [issue](https://github.com/helix-bedrock/world/issues)
- **Want to contribute code?** Read the [contributing guide](https://github.com/helix-bedrock/.github/blob/main/CONTRIBUTING.md)
  and the [AI policy](https://github.com/helix-bedrock/.github/blob/main/AI_POLICY.md)

## License

world is licensed under [MIT](LICENSE). Ported code and its sources (goleveldb, Dragonfly,
worldupgrader, pmmp's schemas) are listed in [CREDITS.md](CREDITS.md); their licenses are in
[`licenses/`](licenses). API docs: <https://pkg.go.dev/github.com/helix-bedrock/world>
