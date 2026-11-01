# Glasm vs native C Lua 5.5.1 — embedded usage (EVAL shape)

This is the second C-Lua comparison suite. `cmd/perfcmp`
(docs/Perf-vs-C-Lua.md) races **long compute scripts** with the native
leg running the `lua` **CLI** — a leg that re-`loadfile`s per run
because the CLI has no pooling surface. That is a CLI limitation, not a
C limitation: an application embedding Lua (Redis EVAL) keeps **one**
`lua_State`, loads each chunk **once**, and calls it with fresh
KEYS/ARGV tables per run. This suite races *that* C shape — driven
through the **C embedding API** by a custom harness — against glasm's
matching daemon posture over `host/` with the **two-tier compile cache**
(`WithCompileCacheDir` + `WithCompileMemCache`, v1.3.3) on every glasm
leg. Short scripts, thousands of staged runs, wall time AND memory.

Recorded run: tree at `18e432c` (v1.3.3) plus this suite's new files
(`runtime/embedbench55.c`, `cmd/embedcmp`). Raw log:
`note/embedcmp-run1.txt` (gitignored, run 2026-11-01 05:04). Every
number below is from that one run; the same-session rule applies —
machine perf on this host shifts 2.2–3.3× across days, and e_\* numbers
are never comparable to perfcmp's h_\* numbers (different harness,
different corpus, different clock path).

## The comparison target

`runtime/embedbench55.c` — a custom C `main` against the same vendored
zero-patch `runtime/lua55/src` tree as perfcmp's leg (the one loadlib.c
`readable()` guard is semantically inert natively), built by
`runtime/build-embedbench55.sh` (no tarball — the harness main is
in-repo):

```sh
clang -O2 -Wall -DLUA_USE_MACOSX -I runtime/lua55/src \
  -o note/native-lua55/embedbench runtime/embedbench55.c \
  runtime/lua55/src/*.c -lreadline -lm
```

Stock runtime defaults (time-based seed, stock incremental GC) — what an
embedded app gets. Both harnesses stage identical per-run arguments:
`KEYS = {"key:1".."key:10"}`, `ARGV = {"17", "4", "alpha beta", "0"}`
(string tables; numbers via `tonumber()` in the scripts), and both
register a `bench.call` hostfn (one string down, its byte-length integer
back — luabench's `regBenchCall` contract) as a C closure / a
`host.RegisterGlobal` Go function.

## Reproduce

```sh
runtime/build-embedbench55.sh
go run ./cmd/embedcmp                  # full suite (~3 min) → 8 markdown tables
go run ./cmd/embedcmp -only call       # one script
go run ./cmd/embedcmp -cachedir /path  # pin the two-tier cache dir across runs
```

## Environment

|            |                                                                                                                                                                                                                                  |
|------------|----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| date       | 2026-11-01 (single session, one recorded run)                                                                                                                                                                                    |
| host       | Apple M4 Max, 36 GB, darwin 27.0.0                                                                                                                                                                                               |
| Go         | go1.27.0, GOMAXPROCS=14                                                                                                                                                                                                          |
| C compiler | Apple clang 21.0.0 (clang-2100.3.34.2), `-O2 -DLUA_USE_MACOSX`                                                                                                                                                                   |
| child env  | `GODEBUG=asyncpreemptoff=1` (the darwin-27 wedge-family mitigation)                                                                                                                                                              |
| timing     | C harness: `mach_absolute_time` (darwin's `clock_gettime(CLOCK_MONOTONIC)` ticks ~1 µs on this host and collapses sub-µs runs to bimodal 0/1000 ns samples — see `now_ns` in the harness); Go children: `time.Now` (same source) |

## Methodology

Engines × postures — every glasm leg carries
`WithCompileCacheDir(dir) + WithCompileMemCache(256)`:

| leg | shape | cold | warm |
|---|---|---|---|
| clua-reuse | ONE state, ONE `luaL_loadbuffer` (registry ref), N staged pcalls — the Redis-EVAL shape | fresh process, 1 run | N in-process runs |
| clua-fresh | naive embedder: newstate+openlibs+register+load+stage+pcall+close per run | — (process-identical to clua-reuse cold) | N runs, capped at 200 |
| glasm-cold | fresh process: `NewEngine(cache)` → `Compile` (disk-cache hit) → exactly 1 `e.Run` — the daemon restart | fresh process, 1 run (cache dir populated once untimed per script) | — |
| glasm-fresh | one process, `Compile` once, N `e.Run` **without** the VM pool (fresh VM per run) | — | N runs, capped at 200 |
| glasm-pool | `WithVMCacheSize(4)`: one untimed priming run, then N pool-hit `e.Run`s via the real daemon API | — | N runs |

Corpus: 7 short EVAL-shaped scripts (`cmd/embedcmp/scripts/e_*.lua`) —
arg arith, KEYS aggregation, reply-array build, string normalization,
40-field hash loop, 200 `bench.call` hostfn crossings, 500-element sort.
All stateless across runs (no persistent globals — every leg's returns
are identical) and integer/string-returns only. Per-script counts:
{coldReps, warmN} = 12×3000 (e_arg/e_sum/e_tag/e_norm), 8×2000
(e_hash/e_call), 8×500 (e_sort); the fresh legs cap at 200 runs (a
glasm-fresh run pays a ~2.6 ms NewVM, a clua-fresh run a full re-parse —
each side's own no-reuse tax; 3000 of those would measure nothing else).

Measurement honesty rules:

- **KEYS/ARGV staging is inside the timed region on BOTH sides** (Redis
  restages per run; `host` stages inside `e.Run`). Result collection too.
- **Cold "process wall" columns carry the ~3.7 ms `/usr/bin/time -l`
  exec floor** (both engines) — hence the separate "cold first run
  (in-process)" and "cold load/compile" tables.
- **maxRSS is a peak** — Go heap high-water ratchets show fully.
- **GC asymmetry**: C runs its stock incremental GC during the loop
  (what an app gets); glasm's guest GC is frozen per run (the v1 law),
  so guest allocation (`USED`) is monotone until the pool's 64 MiB
  recycle watermark. The C `lua-heap` column is **live** heap after a
  forced full collect; the glasm `USED` column is **total** guest bytes.
  They are labeled differently and are not comparable one-to-one — one
  is steady-state live set, the other is cumulative allocation.
- Every leg's return values are cross-checked against clua-reuse
  (`normRet`) — run1 had zero mismatches across all 7 scripts × 5 legs.

## Results — wall time

### Cold wall (one-shot process, median; includes the ~3.7 ms harness floor)

| script | clua-reuse | glasm-cold |
|--------|------------|------------|
| e_arg  | 3.65ms     | 12.08ms    |
| e_sum  | 3.95ms     | 12.71ms    |
| e_tag  | 3.63ms     | 12.34ms    |
| e_norm | 3.71ms     | 13.08ms    |
| e_hash | 3.71ms     | 12.47ms    |
| e_call | 3.78ms     | 13.14ms    |
| e_sort | 3.72ms     | 12.39ms    |

### Cold first run (in-process, excludes the /usr/bin/time floor)

| script | clua-reuse | glasm-cold |
|--------|------------|------------|
| e_arg  | 3.5µs      | 6.51ms     |
| e_sum  | 4.9µs      | 6.56ms     |
| e_tag  | 5.7µs      | 6.50ms     |
| e_norm | 4.0µs      | 7.33ms     |
| e_hash | 15.2µs     | 6.47ms     |
| e_call | 32.2µs     | 6.75ms     |
| e_sort | 51.3µs     | 6.57ms     |

### Cold script load/compile (in-process)

| script | clua-reuse (loadbuffer) | glasm-cold (Compile, disk-cache hit) |
|--------|-------------------------|--------------------------------------|
| e_arg  | 6.9µs                   | 16.0µs                               |
| e_sum  | 9.9µs                   | 14.1µs                               |
| e_tag  | 8.8µs                   | 13.4µs                               |
| e_norm | 8.9µs                   | 14.2µs                               |
| e_hash | 9.8µs                   | 13.2µs                               |
| e_call | 10.0µs                  | 13.8µs                               |
| e_sort | 9.7µs                   | 14.5µs                               |

### Warm wall (per run, steady state, median)

| script | clua-reuse | clua-fresh | glasm-fresh | glasm-pool |
|--------|------------|------------|-------------|------------|
| e_arg  | 0.3µs      | 15.0µs     | 2.63ms      | 20.7µs     |
| e_sum  | 0.8µs      | 16.4µs     | 2.67ms      | 25.5µs     |
| e_tag  | 1.3µs      | 17.1µs     | 2.65ms      | 30.2µs     |
| e_norm | 0.5µs      | 16.5µs     | 2.65ms      | 22.3µs     |
| e_hash | 5.1µs      | 23.1µs     | 2.69ms      | 44.9µs     |
| e_call | 14.2µs     | 38.0µs     | 2.80ms      | 189.7µs    |
| e_sort | 41.9µs     | 59.0µs     | 2.73ms      | 124.8µs    |

### Warm wall × native reuse (glasm legs ÷ clua-reuse)

| script | clua-fresh | glasm-fresh | glasm-pool |
|--------|------------|-------------|------------|
| e_arg  | 44.9×      | 7867.3×     | 62.0×      |
| e_sum  | 21.9×      | 3561.4×     | 34.1×      |
| e_tag  | 12.8×      | 1988.4×     | 22.6×      |
| e_norm | 36.1×      | 5792.8×     | 48.7×      |
| e_hash | 4.5×       | 524.7×      | 8.8×       |
| e_call | 2.7×       | 196.4×      | 13.3×      |
| e_sort | 1.4×       | 65.0×       | 3.0×       |

## Results — memory

### Cold maxRSS (one-shot process)

| script | clua-reuse | glasm-cold |
|--------|------------|------------|
| e_arg  | 2.0MB      | 34.9MB     |
| e_sum  | 2.0MB      | 34.8MB     |
| e_tag  | 2.0MB      | 34.8MB     |
| e_norm | 2.0MB      | 34.8MB     |
| e_hash | 2.0MB      | 34.9MB     |
| e_call | 2.1MB      | 36.3MB     |
| e_sort | 2.1MB      | 35.0MB     |

### Warm maxRSS (loop process)

| script | clua-reuse | clua-fresh | glasm-fresh | glasm-pool |
|--------|------------|------------|-------------|------------|
| e_arg  | 2.1MB      | 2.3MB      | 57.2MB      | 69.5MB     |
| e_sum  | 2.1MB      | 2.3MB      | 68.3MB      | 69.0MB     |
| e_tag  | 2.2MB      | 2.4MB      | 58.3MB      | 69.5MB     |
| e_norm | 2.1MB      | 2.2MB      | 58.1MB      | 69.1MB     |
| e_hash | 2.2MB      | 2.3MB      | 57.9MB      | 50.4MB     |
| e_call | 2.2MB      | 2.4MB      | 62.0MB      | 52.2MB     |
| e_sort | 2.3MB      | 2.4MB      | 57.8MB      | 50.3MB     |

### Memory detail (post-loop)

| script | clua lua-heap (live, post-collect) | glasm go-heap live | glasm guest USED 1→N (frozen GC) |
|--------|------------------------------------|--------------------|----------------------------------|
| e_arg  | 19.6KB                             | 20.0MB             | 13KB → 5.6MB                     |
| e_sum  | 19.8KB                             | 20.0MB             | 13KB → 5.6MB                     |
| e_tag  | 19.8KB                             | 20.0MB             | 13KB → 6.4MB                     |
| e_norm | 19.7KB                             | 20.0MB             | 13KB → 5.6MB                     |
| e_hash | 21.9KB                             | 20.0MB             | 15KB → 6.7MB                     |
| e_call | 21.8KB                             | 16.2MB             | 18KB → 3.8MB                     |
| e_sort | 19.8KB                             | 16.2MB             | 17KB → 3.2MB                     |

(glasm `USED` = `vm.UsedBytes` on a manually pinned VM — the pool's
image is private to `host.Engine`, and a pinned VM accumulates
identically; N = warmN runs.)

## Findings

1. **Against a real C embedder, native C wins every warm comparison.**
   The Redis-shape C leg runs a staged chunk in 0.3–14 µs (sub-µs for
   the trivial scripts); glasm's daemon shape (pool + two-tier cache,
   the best posture glasm has) is 20.7–189.7 µs — **13–62× slower**.
   The earlier "pooled wasm at/near native parity" framing (README,
   perfcmp) was measured against the `lua` CLI's re-loadfile-per-run
   loop; against an embedder that keeps its state, the wasm boundary is
   the story. The one qualification: the ratio compresses fast as the
   script does real work (e_sort 3.0×, e_hash 8.8×) — the ~20 µs is
   mostly a fixed per-run dispatch + staging cost, not per-instruction
   slowdown.
2. **The fixed per-run cost decomposes roughly as: dispatch+staging
   ~20 µs, hostfn crossing ~0.85 µs each.** e_arg (minimal work) at
   20.7 µs vs e_call at 189.7 µs with 200 crossings: (189.7−20.7)/200 ≈
   0.85 µs per `bench.call` round-trip, vs ~70 ns for the C closure
   ((14.2−0.3)/200). A Redis-style script doing dozens of `redis.call`s
   per EVAL pays this per call.
3. **Daemon restart: glasm pays ~12 ms process wall / ~35 MB RSS vs C's
   ~3.7 ms / ~2 MB.** In-process the first glasm run is 6.5–7.6 ms
   (wasm module instantiation + seeding) — but the two-tier cache does
   its job: the script `Compile` from disk is 13–18 µs, same order as
   C's 7–10 µs source parse. Restart cost is instantiation + Go/wazero
   floor, not compilation.
4. **No-reuse postures: C's floor is ~15 µs (newstate+openlibs+parse),
   glasm's is ~2.6 ms (NewVM = wasm instantiation).** A host that
   cannot keep warm images pays ~100× more per run on glasm — the VM
   pool isn't an optimization, it's load-bearing.
5. **Memory: ~25–35× the RSS.** C: 2.0–2.4 MB process, 19–22 KB live
   Lua heap. glasm: 35 MB one-shot restart, 50–70 MB warm loop process,
   16–20 MB live Go heap. Guest allocation on the frozen-GC pooled
   image grows ~1.9 KB/run (13 KB → 5.6 MB over 3000 e_arg runs) —
   bounded by the 64 MiB recycle watermark (which closes, not pools,
   past it), fine for EVAL-rate traffic, worth watching for hot loops.
6. **Darwin clock gotcha (recorded for the next harness):**
   `clock_gettime(CLOCK_MONOTONIC)` ticks at ~1 µs on this host and made
   sub-µs C runs read as bimodal 0/1000 ns samples; the harness uses
   `mach_absolute_time` (`now_ns` in embedbench55.c).

## Open items

- The ~20 µs pool-hit floor (dispatch + wire + KEYS/ARGV staging) is
  the top per-run cost; a U-series target if EVAL-rate latency matters.
- C-side bytecode-cache fresh mode (`lua_dump` once, load bytecode per
  fresh state) unmeasured — would make clua-fresh's parse line free.
- e_\* vs h_\* corpora share no clock-path or shape — never cross-quote.
