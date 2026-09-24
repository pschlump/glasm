# Results — fork vs baseline, 2026-09-23

Machine: macOS darwin 25.6, arm64 (Apple Silicon), GOMAXPROCS=14, go1.27.0.
All numbers are **medians** (see `out-*.txt` for min/mean/n per row; every
script printed identical output on orig / new-interp / wasm legs — `outputs: OK`).

Rows: `parse` = parse+compile only; `run` = interpreter on a precompiled proto,
fresh state; `TOTAL` = CLI equivalent (NewState + DoString + Close); `wasm run`
= cold VM (engine + rt blob + script instantiate + stdlib open + lua_main).

## Short scripts

### interp path — orig vs fork (wasmtime-env run; wazero-env interp rows agreed within noise)

| script | orig parse | new parse | orig run | new run | orig TOTAL | new TOTAL |
|---|---|---|---|---|---|---|
| s0_nop (startup only) | 3.6µs | 3.5µs | 25.8µs | 25.4µs | **27.5µs** | 27.2µs (0.99x) |
| s1_hello | 9.8µs | 9.8µs | 30.9µs | 30.9µs | **38.6µs** | 38.7µs (1.00x) |
| s2_string | 17.9µs | 17.3µs | 97.6µs | 99.4µs | **112.2µs** | 112.8µs (1.01x) |
| s3_table | 15.2µs | 15.3µs | 64.2µs | 64.4µs | **76.8µs** | 77.8µs (1.01x) |

Frontend and interpreter: **parity** on short scripts (≤ ±2%, within noise).

### wasm path on the same scripts

| script | wasm compile | wasm run wasmtime | wasm run wazero | vs orig TOTAL |
|---|---|---|---|---|
| s0_nop | 10.8µs | 18.3ms | ~102ms | 664x / 1511x |
| s1_hello | 22.3µs | 18.3ms | ~100ms | 474x / 947x |
| s2_string | 36.8µs | 18.9ms | ~118ms | 168x / 420x |
| s3_table | 29.8µs | 18.8ms | ~107ms | 245x / 517x |

`s0_nop` = pure VM startup: **~18ms on wasmtime, ~100ms on wazero** (wazero
re-instantiates rt blob + fresh runtime per run; wasmtime shares one JITed
engine across runs, so its number is a warm-engine cold-instance).

## Long scripts

### interp path — orig vs fork

| script | orig run | new run | orig TOTAL | new TOTAL |
|---|---|---|---|---|
| l1_fib (fib 30, call-heavy) | 154-160ms | 161-177ms (+5-10%) | **155-161ms** | 163-168ms (+2-8%) |
| l2_nsieve (2^18-2^20 sieve) | 458-536ms | 486-531ms (+3-6%) | **459-475ms** | 485-520ms (+7-9%) |
| l3_stringwork (~150KB text) | 16.5-17.9ms | 16.7-17.6ms | **16.5-17.3ms** | 15.4-17.3ms (0.94-1.00x) |

On compute-heavy long scripts the fork's interpreter is a consistent **~4-6%
slower** (min-of-N basis); stringwork and all short scripts are at parity.
(ranges = the two independent runs, wasmtime-env and wazero-env)

### wasm path vs orig TOTAL (median)

| script | orig TOTAL | wasm run wasmtime | wasm run wazero | wasmtime ratio | wazero ratio |
|---|---|---|---|---|---|
| l1_fib | 161ms | 14.20s | 7.46s | **88x slower** | **48x slower** |
| l2_nsieve | 459ms | 360ms | 9.10s | **0.78x (faster)** | 19x slower |
| l3_stringwork | 16.5ms | 67.1ms | 445.7ms | 4.1x slower | 26x slower |

Subtracting the ~18ms wasmtime startup, wasm *execution* is: fib ≈ 14.2s,
nsieve ≈ 0.34s, stringwork ≈ 49ms.

## Reading the wasm numbers

- **Call-heavy code dominates the cost.** Every Lua→Lua call in compiled wasm
  round-trips script → rt precall → host `wasm_dispatch` → script. fib(30)
  (~1.6M calls) pays that 1.6M times: 88x orig on wasmtime. On wazero the
  dispatch is a pure-Go callback (no cgo), so it halves to 48x — wazero is
  ~1.9x *faster* than wasmtime here.
- **Straight-line loop/table code runs faster than the Go interpreter** on
  wasmtime (nsieve 0.78x of orig TOTAL) — the C rt table ops beat golua55's
  Go table implementation.
- **String-heavy code is 4x slower** on wasmtime: every string op is an rt_* C
  call from wasm; golua55's native Go string library is fast.
- **wazero on darwin/arm64 loses badly on memory-heavy code** (nsieve 9.1s vs
  wasmtime 0.36s ≈ 25x; scales roughly linearly 2^14→2^20, slightly
  superlinear). wazevo *is* compiled in on darwin/arm64 (`isa_arm64.go` has no
  OS build tag), so the likely causes are wazero's per-access memory bounds
  checks and `memory.grow`-as-slice-copy vs wasmtime's mmap + guard pages —
  attribution plausible, not proven. Production (linux) wazero numbers may
  differ.
- wasm *compile* cost is noise (11µs-260µs, ≤2% of any run).
- The wasm engine stops Lua GC per run (v1 posture); interp legs run under the
  normal Go GC — alloc-heavy nsieve therefore does GC work only on interp legs.

## Warm (pooled-VM) posture — hostbench, through the production host/ pkg

`hostbench/` measures the two deployment postures for high-frequency
EVAL-shaped scripts on the wazero prod blob (`host/`, the daemon's actual
path): **cold** = `e.Run` (fresh VM per run — the v1 default) vs **warm** =
one pooled VM bound to the script (`vm.Run` reruns — guide R3, "deferred
until benchmarks justify it"). Interp leg stages KEYS/ARGV identically.

| script | interp TOTAL (fork) | wasm cold fresh-VM/run | wasm warm pooled rerun |
|---|---|---|---|
| h_tiny (minimal EVAL) | 70.6µs | 77.3ms | **31.5µs (min 16µs)** |
| h_str (short string-heavy) | 298µs | 93.6ms | **3.18ms (min 1.46ms)** |
| h_strlong (significant string work) | 3.07ms | 170.6ms | **62.7ms (min 32.6ms)** |

Reading:

- **Warm pooling removes the fixed cost entirely** — the per-run floor
  (rt_err_clear + KEYS/ARGV staging + lua_main) is ~16-31µs, at parity with
  the interpreter. High frequency is a solved *architecturally* (R3 pool;
  the one-script-per-VM law maps directly onto Redis's EVALSHA script
  cache, and the M8d globals lockdown is what makes reuse safe).
- **The per-op string tax remains**: ~10x interp for short string-heavy
  scripts, ~20x for significant string processing (every string op is an
  rt_* C call from wasm; golua55's native Go string library is fast).
  On darwin/arm64 wazero — the worst-case host (see nsieve note above);
  Linux numbers may be materially better and must be measured before any
  verdict.
- **Pooling has an unpriced cost: GC is stopped per run** (v1 ABI), so a
  pooled VM running allocation-heavy string scripts grows its linear
  memory without bound — pools need VM recycling every N runs or the M6
  arena lifecycle to be production-safe.
- Verdict shape: short + high-frequency + pooled = competitive (3ms vs
  0.3ms per invocation, both far below network latency). Heavy string
  *processing* = 10-20x interp, which is the real price of the sandbox on
  this platform.

## Incidental finding

`table.concat` of 3000 entries overflows golua55's value registry
("registry overflow", `RegistrySize = 256*20` in config.go) in **both** orig
and fork — the wasm/C engine runs it fine. Pre-existing upstream behavior, not
fork-related; l3_stringwork now block-concats by 128 (see script comment).
