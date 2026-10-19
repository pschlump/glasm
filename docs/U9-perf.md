# U9 luabench baseline — 2026-10-19 (same-day, darwin-27)

Raw run: `bench/2026-10-19-luabench-baseline.txt` (2026-10-19 12:00,
go1.27.0, GOMAXPROCS=14). Machine protocol applies: never compare
across days — see `bench/README.md`.

Four new lanes (this commit): h_fib, h_nsieve, h_callret, h_tco.

| lane | interp | wasm warm (wazero) | ratio | note |
|---|---|---|---|---|
| h_fib fib(30) | 149.9ms | 7.12s | ~47× | call seam × 1.6M |
| h_callret 300k leaf calls | 20.7ms | 533.0ms | ~26× | seam w/o recursion |
| h_tco 200k tailcalls | 10.8ms | 103.4ms | ~9.6× | M5c restage path |
| h_nsieve 2^16..18 | 49.6ms | 906.7ms | ~18× | rt table code (wazero gap) |

Per-call seam cost (wazero, warm): h_callret 533ms/300k ≈ **1.8µs/call**;
h_fib 7.12s/1.69M ≈ **4.2µs/call** (recursion adds frame depth). The M5c
staged tailcall is already ~5× cheaper than a plain call — direct dispatch
should bring non-tail OP_CALL into that class or better.

## FINDING: h_fib output MISMATCH = pre-existing main-tailcall bug

`outputs: MISMATCH! interp="832040" wasm="function"` — the lane caught a
real divergence, not a script error (bisected live; **FIXED at U9a —
ledger row 103**, regression rows in `host/host_test.go` TestEvalBasics;
`-only fib` now answers `outputs: OK`):

- `return f(21)` / `return (function(x)...end)(41)` / recursive fib /
  vararg f — single-expression call return → host reads the CLOSURE in
  R(A-1), kind 6 "function"
- `return f(21), f(22)` (fixed wants), `return tostring(f(21))` (tailcall
  to a C function declines the restage), literals, C-call results — all
  correct

Root cause: main-chunk `return f(x)` lowers to OP_TAILCALL; a wasm-closure
callee accepts the M5c staged restage, so results stage at the restaged
successor frame while the host reads `vm.frame + i*16` (host/vm.go:515) —
lua_main has no rt_disp_frame bracket (U4g direct dispatch; the trampoline
path is immune via rtw_dispatch_body's adopt). The 1501-case matrix never
caught it: no corpus case ends the chunk in a tailcall whose result the
engines read back (events are PRINT/GLOBALS, and every prior luabench lane
ends in a C-function call returning a string).

Fix shape (backend-only, no rt/blob change): emitDispatch saves its entry
frame param, and when the M5c loop actually restaged, copies nret results
from the final restaged frame back to the entry frame before returning
(the original frame is still allocated on every caller path) — landed as
U9a. A differential corpus case (main-chunk tailcall return, incl.
vararg/tbc shapes) becomes writable with U9a2's RESULTS event, which is
the chunk-returns-into-the-differential-surface work and prerequisite for
U9c. U9 item 0 — correctness before perf.

## U9b — dispatch-seam micro-fixes (host + testdiff engines)

Before-numbers (2026-10-19 14:53, quiet machine, same protocol):
`bench/2026-10-19-u9b-callret-before.txt` — h_callret wasm cold 599–602ms,
warm 535–538ms, wazero-prod cold 603–605ms (interp TOTAL 20.4ms).

Two hoists at the `host.wasm_dispatch` seam:

1. **Pool the `lua_dispatch` handles instead of looking up per call.** In
   wazero v1.12 (wazevo), every `ExportedFunction` call runs
   `moduleEngine.NewFunction`, which builds a fresh `callEngine` — private
   `execCtx` AND a fresh machine stack (`ce.init()`, `make([]byte,
   stackSize)`) — so the old per-call lookup was the seam's dominant
   allocation, paid on every compiled call.
2. **`CallWithStack` over pooled 5-slot arg slices** instead of `Call`'s
   variadic + result allocations.

**The wazero reentrancy trap (found the hard way):** a single cached
handle is NOT shareable across nesting levels. A `callEngine` is
single-flight mutable state (`execCtx` exit codes, stack pointers,
try-handlers), and the dispatch seam reenters on the same goroutine
(guest → host.wasm_dispatch → lua_dispatch → C precall →
host.wasm_dispatch → …). Sharing one handle deterministic-SIGBUSes in
JIT code (first script-running host test). Likewise the arg slice:
wazevo passes the generated entrypoint a raw pointer into the caller's
slice (`paramResultPtr`) that stays live for the whole execution, so a
nested call overwriting it corrupts the outer call's parameter area.
Both pools are therefore **depth-indexed**: nesting level *d* takes
pool[d] (grown on demand), level 0 serves the overwhelmingly common
non-nested call, and sequential reuse of one handle is the documented
`CallWithStack` pattern. Steady state: zero allocations per dispatch.
This constraint binds U9c too — the trampoline fallback path must keep
per-depth (not per-call, not shared) engines.

wasmtime (`testdiff/wasmengine.go`): the `*wt.Func` handle is cached
per run (wasmtime-go `Func.Call` builds a fresh call frame each time —
no shared mutable engine state, so a single cache is safe there).

After-numbers (2026-10-19 15:25, same protocol, quiet machine):
`bench/2026-10-19-u9b-callret-after.txt`. h_callret medians:

| posture                 | before  | after   | Δ       |
|-------------------------|---------|---------|---------|
| interp TOTAL (fork)     | 20.36ms | 20.40ms | flat    |
| wasm cold (host/ VM)    | 600.66ms| 458.65ms| −23.7%  |
| wasm warm (pooled VM)   | 538.20ms| 382.13ms| −29.0%  |
| wazero-prod engine cold | 603.82ms| 454.50ms| −24.7%  |

Interp flat (untouched path). Warm −29% is the cleanest seam-cost
signal (no instantiation in the measurement); cold wins the same ~24%
with instantiation constant on both sides. Outputs byte-checked OK on
every posture. Gates: host plain (7.56s) + `host -race -count=2`
(170.0s) + full `testdiff` suite (`ok 1024.954s`, GODEBUG=asyncpreemptoff=1,
-timeout 40m, zero FAILs) — all green on the committed tree.
