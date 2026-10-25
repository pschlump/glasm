# Glasm vs native C Lua 5.5.1 — runtime and memory

First recorded run of `cmd/perfcmp` (tree at `732fc8e`, v1.1.2): the glasm
engines against **native stock C Lua 5.5.1** on the luabench script set,
measuring wall time **and** process memory. Raw log of the recorded run:
`note/perfcmp-run.txt` (gitignored).

Headline (steady state, per run, median):

- The **wazero production engine is 0.7×–6.8× native C** across lanes, and
  **faster than native C** on the tiny EVAL shape (9.2 µs vs 14 µs — a
  pooled VM rerun skips parse + state build, which the native CLI pays
  per process by design).
- The **interpreter is 1.3×–16× native C** — at parity-ish on the
  string/alloc-churn lanes (`h_concat` 1.3×), worst on tight numeric
  loops (`h_numloop` 16×).
- Native C's whole-process footprint is **2.0–2.8 MB** (11.6 MB peak on
  `h_nsieve`, ~2.3 MB of it Lua heap). The Go engines carry a runtime
  floor: interp 8–14 MB cold, wazero ~52 MB steady, wasmtime ~63 MB cold.

## The comparison target

"Original C" = the vendored stock 5.5.1 tree (`runtime/lua55/src`, zero
patches except the `loadlib.c` `readable()` guard, `VENDORED.md`) plus the
**stock `lua.c`** standalone driver, extracted from U0's saved upstream
tarball (`note/conv-from-5.2-to-5.5/lua-5.5.1.tar.gz`) — the vendored tree
ships no `lua.c` (the blob is a reactor). Built by
`runtime/build-native55.sh`:

```
clang -O2 -Wall -DLUA_USE_MACOSX   # Apple clang 21.0.0, arm64
→ note/native-lua55/lua            # 313,648 B, stock runtime defaults
                                    # (time-based seed — build55.sh's
                                    #  determinism pins bind the wasm
                                    #  oracle, not native C)
```

## Reproduce

```sh
runtime/build-native55.sh     # native stock C Lua 5.5.1 → note/native-lua55/lua
go run ./cmd/perfcmp           # from the repo root; ~35 s
go run ./cmd/perfcmp -only fib # one script
```

## Environment

| | |
|---|---|
| date | 2026-10-25 06:49 (single session, all engines back-to-back) |
| host | Apple M4 Max, arm64, macOS darwin 27.0.0, 36 GB |
| Go | go1.27.0, GOMAXPROCS=14 |
| C | Apple clang 21.0.0 (-O2 -DLUA_USE_MACOSX) |
| children | `GODEBUG=asyncpreemptoff=1` (the darwin-27 wedge-family mitigation) |

Caveats that bound these numbers:

- **Same-session rule.** This machine's perf shifts 2.2–3.3× across days
  (note/u7-perf.md R3 context) — compare only numbers from this one run.
- **Cold numbers carry a ~3.7 ms harness floor** (`/usr/bin/time -l
  /usr/bin/true`): fork+exec of `time` itself. Native C's 4.8 ms cold on
  `h_tiny` is ~1.1 ms above the floor; every engine pays the same floor.
- maxRSS is macOS `/usr/bin/time -l` (bytes). It is a **peak** resident
  figure, so Go heap high-water ratchets show up fully (see warm table).

## Methodology

Four engines, two postures:

| engine | what it is | cold (one-shot) | warm (steady state) |
|---|---|---|---|
| `clua` | native stock C Lua 5.5.1 | fresh `lua` process per rep | `warm55.lua`: loadfile+call reruns in ONE state (the CLI has no pooling surface; parse stays in the timed region — matching the interp leg) |
| `interp` | the glasm interpreter | child process, one run | fresh `LState` per run (the fork-style EVAL posture) |
| `wazero` | wasm backend on wazero via `host/` — **the production posture** | child process, one `e.Run` (fresh VM) | pooled `host.VM` bound to the script, rerun |
| `wasmtime` | wasm backend on wasmtime — the differential oracle host | child process, one `Run` | shared wasmtime Engine, **fresh Store per run** |

- Every leg consumes the **same source file**: script + `KEYS={}ARGV={}`
  prelude (the luabench engine prelude). Return values are cross-checked
  across all four engines on every run — **0 mismatches** in the recorded
  run (this is a timing harness; semantic parity is testdiff's job, but a
  leg that errors would fail loudly).
- Cold = whole process under `/usr/bin/time -l`, median over 3–12 reps.
  Warm = per-run median from inside the loop process (clua via `os.clock`
  per iteration), 2 warmups, `runtime.GC()` between Go iterations; RSS of
  the loop process from `/usr/bin/time -l`.
- `h_hostfn` is excluded (needs the row-96 hostfn seam; the wasmtime
  engine leg has no surface — same skip as luabench).
- wasmtime warm loops cap at 40 runs/child: each `Run` static-maps the
  blob's 256 MiB memory + guard into a fresh Store and Go finalizers
  reclaim Stores slowly (the M6e cliff) — uncapped, the loop would
  measure finalizer backlog, not run cost.

Reading the warm table honestly — three posture asymmetries are by
design, not accidents:

1. **wazero warm is pooled** (compile + blob instantiation amortized —
   exactly what the daemon does); clua/interp warm re-parse and re-build
   state every run because that is their only honest steady state.
2. **Pooled wazero guest GC is frozen** (ledger row 61) — per-run guest
   garbage accumulates in the 256 MiB linear memory. Visible on the
   churny lanes (`h_tablechurn`/`h_nsieve`: 270 MB RSS at 60 iterations);
   production bounds this with `WithMemoryBudgetBytes`.
3. **wasmtime per-run cost includes a fresh 256 MiB Store mapping** —
   its ~28 ms warm floor on every script is store setup + cgo, not Lua
   execution. It is the oracle host, not a production posture.

## Results — wall time

Cold (one-shot process, median; everyone pays the 3.7 ms harness floor):

| script | clua | interp | wazero | wasmtime |
|---|---|---|---|---|
| h_tiny | 4.89ms | 6.71ms | 87.02ms | 42.42ms |
| h_str | 4.81ms | 6.98ms | 88.91ms | 43.55ms |
| h_strlong | 5.74ms | 8.74ms | 91.77ms | 45.53ms |
| h_numloop | 13.76ms | 136.11ms | 128.40ms | 55.09ms |
| h_tablechurn | 9.40ms | 18.26ms | 111.67ms | 51.67ms |
| h_concat | 5.79ms | 7.86ms | 90.08ms | 44.73ms |
| h_fib | 25.64ms | 163.36ms | 224.94ms | 115.71ms |
| h_nsieve | 11.68ms | 71.04ms | 130.86ms | 54.21ms |
| h_callret | 7.23ms | 28.62ms | 110.04ms | 53.18ms |
| h_tco | 6.76ms | 18.41ms | 97.70ms | 46.52ms |

Warm (per run, steady state, median):

| script | clua | interp | wazero | wasmtime |
|---|---|---|---|---|
| h_tiny | 14.0µs | 62.8µs | 9.2µs | 28.02ms |
| h_str | 35.0µs | 135.8µs | 68.1µs | 30.33ms |
| h_strlong | 391.0µs | 1.56ms | 731.1µs | 34.18ms |
| h_numloop | 8.53ms | 136.77ms | 40.04ms | 45.60ms |
| h_tablechurn | 3.87ms | 10.90ms | 21.30ms | 36.42ms |
| h_concat | 609.0µs | 812.6µs | 1.46ms | 31.47ms |
| h_fib | 20.12ms | 155.79ms | 137.03ms | 104.27ms |
| h_nsieve | 6.14ms | 63.42ms | 40.53ms | 40.04ms |
| h_callret | 2.42ms | 21.61ms | 22.20ms | 38.66ms |
| h_tco | 1.70ms | 11.37ms | 9.64ms | 33.97ms |

Warm wall × native C:

| script | clua | interp | wazero | wasmtime |
|---|---|---|---|---|
| h_tiny | 1.0× | 4.5× | 0.7× | 2001.4× |
| h_str | 1.0× | 3.9× | 1.9× | 866.7× |
| h_strlong | 1.0× | 4.0× | 1.9× | 87.4× |
| h_numloop | 1.0× | 16.0× | 4.7× | 5.3× |
| h_tablechurn | 1.0× | 2.8× | 5.5× | 9.4× |
| h_concat | 1.0× | 1.3× | 2.4× | 51.7× |
| h_fib | 1.0× | 7.7× | 6.8× | 5.2× |
| h_nsieve | 1.0× | 10.3× | 6.6× | 6.5× |
| h_callret | 1.0× | 8.9× | 9.2× | 16.0× |
| h_tco | 1.0× | 6.7× | 5.7× | 20.0× |

## Results — memory

Cold maxRSS (one-shot process):

| script | clua | interp | wazero | wasmtime |
|---|---|---|---|---|
| h_tiny | 2.0MB | 8.5MB | 51.7MB | 62.9MB |
| h_str | 2.0MB | 8.6MB | 52.0MB | 63.2MB |
| h_strlong | 2.2MB | 11.7MB | 52.4MB | 63.5MB |
| h_numloop | 2.0MB | 14.3MB | 51.6MB | 62.7MB |
| h_tablechurn | 2.5MB | 13.4MB | 57.8MB | 62.9MB |
| h_concat | 2.4MB | 9.5MB | 51.8MB | 63.2MB |
| h_fib | 2.0MB | 8.8MB | 52.1MB | 64.0MB |
| h_nsieve | 6.7MB | 26.7MB | 82.6MB | 70.1MB |
| h_callret | 2.0MB | 12.7MB | 52.0MB | 62.1MB |
| h_tco | 2.0MB | 12.3MB | 51.8MB | 63.2MB |

Warm maxRSS (loop process — see the three posture asymmetries above
before quoting these):

| script | clua | interp | wazero | wasmtime |
|---|---|---|---|---|
| h_tiny | 2.2MB | 54.3MB | 52.3MB | 185.4MB |
| h_str | 2.4MB | 40.0MB | 52.4MB | 189.7MB |
| h_strlong | 2.8MB | 19.5MB | 68.3MB | 192.5MB |
| h_numloop | 2.2MB | 16.8MB | 52.6MB | 130.2MB |
| h_tablechurn | 2.8MB | 29.4MB | 270.7MB | 228.8MB |
| h_concat | 2.7MB | 20.1MB | 67.9MB | 203.1MB |
| h_fib | 2.1MB | 10.3MB | 51.8MB | 99.1MB |
| h_nsieve | 9.3MB | 33.0MB | 270.7MB | 204.3MB |
| h_callret | 2.2MB | 15.5MB | 52.2MB | 136.1MB |
| h_tco | 2.2MB | 15.5MB | 52.0MB | 131.4MB |

Memory detail (post-loop): clua reports `collectgarbage("count")` at exit
(Lua heap, KB); Go engines report `runtime.MemStats` after the loop —
heapsys / live = `HeapSys` / `HeapAlloc` after `runtime.GC()`
(`Sys` omitted here; it tracks heapsys + ~7 MB):

| script | clua lua-heap | interp live | wazero heapsys / live | wasmtime live |
|---|---|---|---|---|
| h_tiny (300 it) | 27.7KB | 44.1MB | 47.4MB / 12.8MB | 915KB |
| h_str (200 it) | 33.4KB | 29.7MB | 47.4MB / 12.8MB | 909KB |
| h_strlong (40 it) | 145.9KB | 6.5MB | 67.4MB / 15.9MB | 910KB |
| h_numloop (12 it) | 23.8KB | 2.4MB | 43.5MB / 12.8MB | 558KB |
| h_tablechurn (60 it) | 111.7KB | 9.4MB | 283.4MB / 72.3MB | 910KB |
| h_concat (60 it) | 163.7KB | 9.4MB | 63.4MB / 19.6MB | 909KB |
| h_fib (4 it) | 25.3KB | 1.2MB | 47.5MB / 12.8MB | 452KB |
| h_nsieve (10 it) | 2336.0KB | 2.1MB | 259.4MB / 72.3MB | 533KB |
| h_callret (12 it) | 25.8KB | 2.4MB | 47.5MB / 12.8MB | 557KB |
| h_tco (12 it) | 25.7KB | 2.4MB | 47.4MB / 12.8MB | 563KB |

Attribution notes:

- **clua**: Lua heap at exit is tens of KB except `h_nsieve` (2.3 MB of
  sieve tables); process RSS ≈ binary + runtime + heap, 2.0–2.8 MB.
- **interp**: warm RSS ratchets with **iteration count**, not script
  weight — live heap after explicit GC is ~150 KB **retained per
  fresh-state run** (44 MB at 300 `h_tiny` runs, 2.4 MB at 12). The
  cold-process figure (8–14 MB) is what a fork-style EVAL host actually
  pays; the retention is an open interp-side item (nothing in `Close`'s
  path obviously explains it — worth a heap profile before anyone quotes
  warm-loop interp RSS as a leak).
- **wazero**: steady 52 MB = Go runtime + wazero JIT code pages + blob;
  the guest linear memory is Go-heap-backed here, so frozen-GC guest
  garbage shows up inside `HeapSys` (283 MB on `h_tablechurn` — 52 base +
  ~230 MB accumulated guest garbage at 60 iterations, bounded by the
  256 MiB memory and by `WithMemoryBudgetBytes` in production).
- **wasmtime**: Go heap is tiny (~1 MB live) — its RSS is cgo-owned
  Store mappings: 256 MiB reservation + guard per Store, reclaimed on
  finalizer schedule (the M6e behavior), hence 99–229 MB across ≤42 runs.

## Findings

1. **Steady state, production engine:** wazero lands 0.7×–6.8× native C.
   It beats native C where the pooled posture amortizes what the native
   CLI cannot (tiny EVAL shape), sits ~2× on string lanes (consistent
   with the U11 string-lanes-at-parity work — vs interp, not vs C), and
   is farthest behind exactly where the rt seams still cross the host
   per operation: `h_callret` 9.2× and `h_fib` 6.8× (the call seam),
   `h_tablechurn` 5.5× (rt table code), `h_numloop` 4.7× (in-wasm arith
   is post-U10/U11e but every miss still folds to `rt_arith`).
2. **Interpreter:** 1.3×–16× native C. Best on alloc/string churn
   (`h_concat` 1.3×, `h_tablechurn` 2.8× — it even beats wazero there:
   10.9 ms vs 21.3 ms), worst on tight arithmetic (`h_numloop` 16×) —
   the classic dispatch-overhead profile of a register-VM interpreter
   against -O2 C.
3. **Cold start:** native C adds ~1.1 ms over the 3.7 ms harness floor;
   interp +3 ms of Go runtime; wazero ~87 ms (Go runtime + wazero
   runtime init + blob verify/instantiate + compile — the one-shot cost
   the pooled-VM posture exists to amortize); wasmtime ~42 ms.
4. **Memory:** native C is 2–3 MB total. Interp pays the Go runtime plus
   per-run retention (~150 KB/run in the warm loop — open item). Wazero
   pays a flat ~52 MB engine floor plus guest garbage growth bounded by
   the memory cap. Wasmtime (oracle only) maps a 256 MiB region per
   Store. For the daemon posture the relevant pair is wazero's flat
   floor + `WithMemoryBudgetBytes` vs C's ~2 MB — that is the price of
   the pure-Go/no-cgo constraint (A8), paid once per VM, not per script.

## Open items surfaced by this run

- interp fresh-state loop retains ~150 KB/run (see above) — heap-profile
  before labeling.
- wazero cold ~87 ms is dominated by engine init, not script compile;
  fine for the pooled posture, worth a look if one-shot CLI use matters.
- The `h_callret`/`h_fib` 9.2×/6.8× gap attribution (per-call seam
   residue after U9 direct dispatch) matches U9c's residue analysis;
   `h_nsieve`/`h_tablechurn` remain the rt-table-codegen lanes from
   note/u7-perf.md.
