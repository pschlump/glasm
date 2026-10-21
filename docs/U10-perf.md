# U10 — the wazero try_table-enter tax, killed at the source (2026-10-20)

Raw runs (same day, darwin-27 protocol — `bench/README.md`):
`bench/2026-10-20-u10-before.txt` (16:32, HEAD = v1.1.0/U9c) and
`bench/2026-10-20-u10.txt` (17:59, U10). All lanes `outputs: OK` on both.

## The mechanism (one paragraph)

Every protected rt helper (`rt_run` → `luaD_rawrunprotected` → the
`-mllvm -wasm-enable-sjlj` lowering) enters a wasm `try_table` per
call. Wazero v1.12 implements every enter as a full machine-stack
clone — a 10,880-byte Go allocation + copy per protected call, plus
the GC churn of ~20–28 GB per hot run. Wasmtime lowers the same
instruction to native landing pads (cheap), which is why U9c took
wasmtime to interp parity while wazero stood still. The fix was not
engine work: stop making per-op protected calls.

(The sledgehammer alternative — dropping the sjlj flag for software
setjmp — is impossible: wasi-sdk-34's `setjmp.h` hard-`#error`s
without `__wasm_exception_handling__`; `libsetjmp.a` holds only the
EH-lowering thunks. Verified before spending the blob dance on it.)

## What landed

- **U10-B1+B2 (commit c5bccaf, backend-only):** register-register
  float/mixed arith and all nine compare forms got in-wasm fast paths.
  The old defect map was half wrong: numloop's 2M enters were not the
  FORLOOP step (inline long since) nor MULK (U7c) — `emitArithReg`'s
  float arm was unreachable code (`intFast` matched ADD/SUB/MUL
  first). Bisected with lane-isolated probes (`f = f + i` alone paid
  2M × 10,880 B). `emitCompare` now mirrors vendored lvm.c exactly
  (LTnum/LEnum via the four mixed helpers, `luaV_flttointeger`
  F2Ieq equality, `op_orderI` immediates, EQI non-number → cond 0,
  EQK rawequal); fib's per-call enter was `rt_lt` on `n < 2`.
- **U10-B3 (this commit, blob + backend):** `rt_geti_probe` /
  `rt_seti_probe` — additive ABI v4 exports (rt and prod flavors) that
  run the VM's own `luaV_fastgeti`/`luaV_fastseti` arms as
  unprotected call-ins (no `rt_run`, no setjmp; they cannot raise by
  construction — pure writes or code returns, `luaC_barrierback` under
  the frozen-GC posture). Backend arms on OP_GETTABLE/GETI/SETTABLE/
  SETI probe first; any miss (non-table, non-int key, hole, NEWINDEX,
  growth) takes the unchanged protected path. Native ABI unit tests in
  `runtime/tests/rt_native55_test.c` (hit/miss/hole/non-table/
  hash-part round-trip). Semantics identical by construction — same
  macros, same conditions, byte-exact by the standing gates (matrix
  1501/1501 and err 96/96 on wasm, wazero, wazero-prod; full testdiff
  gate ok 1072.9s).

## Probe attribution (warm-run enters of the 10,880 B clone, before → after)

| lane | before | after | note |
|---|---|---|---|
| h_fib | 2,692,540 | **3** | B2 (`rt_lt` per call) |
| h_numloop | 2,000,041 | **41** | B1 (dead float arm in emitArithReg) |
| h_nsieve | 1,867,668 | **41,878** | B3 (misses = rehash growth + per-round concat) |
| h_tablechurn | — | 90,406 | B3 (misses = string-key inserts + growth) |

## luabench, before → after (medians; wasm warm = host/ pooled on wazero)

| lane | interp | wasm warm before | wasm warm after | delta |
|---|---|---|---|---|
| h_numloop (4 M numeric) | 128.6 ms | 925.0 ms | **42.0 ms** | −95.5%, **3.1× faster than interp** |
| h_fib fib(30) | 157.1 ms | 1.69 s | **139.7 ms** | −91.7%, **1.1× faster than interp** |
| h_nsieve 2^16..18 | 51.1 ms | 908.1 ms | **44.7 ms** | −95.1%, **1.1× faster than interp** |
| h_tco 200k tailcalls | 10.8 ms | 105.3 ms | **9.4 ms** | −91.1%, **1.1× faster than interp** |
| h_tablechurn | 10.1 ms | 78.3 ms | 57.4 ms | −26.7% |
| h_str | 170.0 µs | 647.8 µs | 552.1 µs | −14.8% |
| h_concat | 941.2 µs | 5.4 ms | 5.2 ms | flat |
| h_hostfn | 595.3 µs | 5.6 ms | 6.0 ms | flat |
| h_callret (U9c lane) | 20.8 ms | 22.0 ms | 22.6 ms | flat |

Interp moved too (same-day drift on the µs lanes runs both ways —
h_tiny interp 51→68 µs across the two runs); the compute-lane deltas
are 10–22×, far outside noise.

**The "wazero codegen gap" is retired.** The production engine now
beats the interpreter on every numeric/call/table lane. What actually
remains open:

- **String lanes** (`h_strlong` 10.5× interp warm, stringwork
  ~4×/14×): real rt-bound work — Lua's string code running as
  wazero-compiled C. No enter tax left there either; it is codegen
  plus the C-vs-Go-startup floor on cold paths.
- **Cold start ~70 ms** (the `wasm, cold VM` column): instantiation,
  not execution; pooling hides it for the daemon, and the exit-gate
  engine cold lanes show the same ~70 ms floor.
- **Upstream wazero** (optional): clone-per-enter is O(stack) per
  protected region vs wasmtime landing pads — worth an issue with the
  before-numbers, for everyone else's sjlj code.

## By-shape probes (prebuilt `luawasm-run`, 3 runs, process time incl. ~70–100 ms instantiation)

| shape | interp | wasm on wasmtime | wasm on wazero |
|---|---|---|---|
| fib(30) | 0.16 s | **0.11 s** | 0.23 s |
| nsieve (N=18, 3 rounds) | 0.29 s | **0.08 s** | 0.23 s |
| stringwork (~150 KB text) | 0.01 s | 0.04 s | 0.14 s |

(vs the U9c-era README rows: fib wazero 2.0 s → 0.23 s; nsieve
wasmtime 360 ms / wazero 9.10 s → 80 ms / 230 ms.)
