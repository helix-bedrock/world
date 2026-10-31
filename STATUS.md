# What world does

Status as of 2026-10-31: **in development.** Storage is one-to-one with BDS 1.26.60: a world BDS
saved loads and saves back byte for byte.

## What works

- **Storage:** chunks, entities, block entities, chunk metadata, level keys, players, plugin
  namespaces and `level.dat`, as BDS 1.26.60 stores them, with BDS's 5-second batched writes.
- **Pluggable stores:** `KV`, `WorldStore` and `PlayerStore` replace any part of the storage.
- **No registry needed for level data:** a world opens without a block registry for level keys,
  players and level.dat. `chunk.BlockRegistry` asks only for what chunk code uses: states, light
  emission and dampening, and network hashes.
- **Snapshots:** `Chunk.Snapshot` hands a chunk to another goroutine to save or encode while its
  owner keeps changing it; only sections changed afterwards are copied.
- **Network encoding:** `NetworkHashEncoding` writes block palettes as network hashes, as BDS sends
  them.
- **Tick queues:** both of a chunk's tick queues are decoded: pending ticks (`ScheduledBlocks`) and
  BDS's random ticking queue (`RandomTicks`), each with its own clock.
- **Old worlds:** block states from older worlds are upgraded up to 1.26.50 (pmmp's upgrade
  schemas).

## What is left

- **The API is not stable yet.** Names and behaviour may still change between commits.
