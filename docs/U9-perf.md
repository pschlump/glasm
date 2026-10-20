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

## U9c — direct in-module dispatch at OP_CALL/TFORCALL

Same-day before/after (2026-10-20 06:42/06:46, back-to-back, quiet
machine; BEFORE = HEAD 0427c66 in a separate worktree, AFTER = the
working tree): `bench/2026-10-20-u9c-{before,after}.txt`.

The change (ledger row 104): a non-tail OP_CALL/TFORCALL whose callee
is one of the module's own compiled closures no longer crosses the host
seam — probe (`rt_dd_callee`) → `rt_dd_enter` → a plain module-internal
call to `lua_dispatch` → `rt_dd_ret`/`rt_dd_err`. C callees, `__call`
chains, and foreign-registry closures decline to the unchanged
`rt_call_addr` fallback, and `return f(x)` still rides the M5c staged
tailcall (OP_TAILCALL) — the arm is the OP_CALL family: call statements,
non-tail recursion, `fib(n-1)+fib(n-2)` (two direct calls per
invocation). Riding with it: the direct frame's stack presence (a real
CallInfo + sea entry per level, so getinfo/where/level walks are
byte-exact — see row 104's (c) for the retract rule and headroom gate
that ovfl01 forced).

h_callret / h_fib medians (the two direct-arm lanes; interp flat,
h_nsieve flat at 1.16→1.12s, h_tco flat at 129→116ms):

| lane                    | before   | after    | Δ        |
|-------------------------|----------|----------|----------|
| h_callret interp TOTAL  | 22.94ms  | 21.12ms  | flat     |
| h_callret wasm warm     | 525.81ms | 22.87ms  | **−95.7%** |
| h_callret wasm cold     | 569.52ms | 93.64ms  | −83.6%   |
| h_callret prod cold     | 592.32ms | 94.86ms  | −84.0%   |
| h_fib interp TOTAL      | 160.13ms | 157.89ms | flat     |
| h_fib wasm warm         | 5.95s    | 2.00s    | **−66.4%** |
| h_fib wasm cold         | 5.79s    | 2.04s    | −64.8%   |
| h_fib prod cold         | 6.33s    | 2.05s    | −67.6%   |

Reading: leaf-call churn is at interpreter parity warm (22.87ms vs the
interp's 21.12ms — the per-call host-seam cost is gone; U9b had already
taken it 382→526ms-equivalent, U9c removes the crossing itself). fib(30)
warm drops 3×; the residual 12.7× vs interp is NOT the seam anymore —
it is the known wazero-vs-wasmtime codegen gap over the rt table/arith
helpers (the nsieve-family item, top open perf work). On wasmtime the
same fib(30) now runs in **0.15s** (prebuilt `luawasm-run`, 3 runs,
0.47s first-touch) — at interp parity (interp 158ms), from the 14.2s
the README had pinned.

Budget-depth probe (budget scratch-lifted to 1e6, isolated blobs,
tree SHA-restored): pure-direct recursion completes 2000 frames on
every engine; wasmtime raises a catchable position-less "stack
overflow" by 3000 (pcall catches — no engine trap), wazero/wazero-prod
complete 32000+ (wasm frames ride the goroutine stack). Worst mixed
case ≈ 150 trampolines (≥2 direct-equivalents each) + 1024 budgeted
direct levels ≈ 1324 < 2000 — RT_DD_MAX_DEPTH=1024 fires first on every
engine with ≥1.5× margin (row 104).

Gates (all on the final blobs, prod SHA 94ce38f1…, rt SHA ac453d77…):
vet + `go test -short ./...` green; full `testdiff` heavy suite
`ok 1135.720s` (GODEBUG=asyncpreemptoff=1, -timeout 40m); runtime/tests
run.sh ×{plain,ASan,UBSan}; host plain (8.39s) + `host -race -count=2`
(177.02s); `_cli-tests` ×{wasmtime, WAZERO=1} PASS; `_u9` corpus 7/7
(interp↔wasm, zero skips) + TestU9Gate + TestU9DeadlineMidDirect.
