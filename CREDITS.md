# Credits

Code in this repo that was ported from other projects, as required by the
[porting rules](https://github.com/helix-bedrock/.github/blob/main/PORTING.md).

| Helix path | Source project | Source path | Commit | License |
|---|---|---|---|---|
| `leveldb/` | [syndtr/goleveldb](https://github.com/syndtr/goleveldb) by Suryandaru Triandana | `leveldb/` | [`126854a`](https://github.com/syndtr/goleveldb/commit/126854af5e6d8295ef8e8bee3040dd8380ae72e8) | BSD-2-Clause (`leveldb/LICENSE`) |
| `leveldb/opt/options.go`, `leveldb/table/` (flate compression) | [df-mc/goleveldb](https://github.com/df-mc/goleveldb) by Dragonfly Tech | `leveldb/table/reader.go`, `writer.go`, `table.go`, `leveldb/opt/options.go` | [`e85415c`](https://github.com/df-mc/goleveldb/commit/e85415c04dab8522f9f67f66e1d25511478c1769) | BSD-2-Clause; reworked to remove package-level state |

## Unmerged upstream pull requests

goleveldb has not been maintained since 2022. These open pull requests against
`syndtr/goleveldb` are applied on top of `126854a`. Each was contributed to
that BSD-2-Clause repo.

| PR | Author | Head commit | What it does |
|---|---|---|---|
| [#457](https://github.com/syndtr/goleveldb/pull/457) | SashaMIT | `974b21a105c8` | Reject batch sequence numbers that overflow an internal key instead of panicking |
| [#455](https://github.com/syndtr/goleveldb/pull/455) | k2s | `00b3281ca8ba` | Recover journal batches below the manifest's sequence, as C++ LevelDB (and so BDS) writes them; previously they were silently dropped |
| [#456](https://github.com/syndtr/goleveldb/pull/456) | k2s | `bdc96830fb75` | `cache.GetStats` no longer panics after `Close` |
| [#300](https://github.com/syndtr/goleveldb/pull/300) | WangXiangUSTC | `ea514d43ecec` | Fix a slice-bounds panic in `tFiles.newIndexIterator` |
| [#414](https://github.com/syndtr/goleveldb/pull/414) | qianbin | `bbf4482c72bd` | Throttle manifest rebuilding |
| [#376](https://github.com/syndtr/goleveldb/pull/376) | kingeasternsun | `589e7bac6811` | Simpler pointer update in `memdb.Delete` |
| [#384](https://github.com/syndtr/goleveldb/pull/384) | holiman | `c612a5c66d20` | Skip repeated comparisons in the memdb skiplist |
| [#416](https://github.com/syndtr/goleveldb/pull/416) | qianbin | `f4fac812c77e` | Store memdb nodes and data in one slice (faster) |
| [#421](https://github.com/syndtr/goleveldb/pull/421) | qianbin | `31a63ab4c23a` | Faster merged iterator |
| [#407](https://github.com/syndtr/goleveldb/pull/407) | qianbin | `9b667debf2a1` | `BufferPool` without unexpected allocations |
| [#420](https://github.com/syndtr/goleveldb/pull/420) | qianbin | `3284c3537567` | Skip target-level tables that no source key overlaps during compaction |

Not applied: #362 (treats "no space left on device" as recoverable by matching
error text), #379 (overwrites memdb values in place, which corrupts slices that
iterators still hold), #419 (`WaitGroup.Add` racing with `Wait` in `Close`),
and PRs that only change CI, docs or formatting.

## Helix changes

- Bedrock block types 2 (zlib) and 4 (raw deflate) are read; type 4 is
  written. The type numbers come from Mojang's LevelDB fork as published in
  [pmmp/leveldb](https://github.com/pmmp/leveldb/blob/aba9e148149bda96c5212905c67275a53e701b1c/include/leveldb/options.h)
  (BSD-3-Clause, branch `mojang-compatible`); no code was copied.
- Compaction reply channels are buffered and never closed, fixing a data race
  that upstream hid with `recover()` (`TestDB_TransientError` under `-race`).
