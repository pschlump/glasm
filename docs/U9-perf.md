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
real divergence, not a script error (bisected live; regression rows
land in `host/host_test.go` TestEvalBasics with the U9a fix):

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
(the original frame is still allocated on every caller path). Ledger row
+ corpus case (main-chunk tailcall return, incl. vararg/tbc shapes) +
host test needed. U9 item 0 — correctness before perf.
