# U11 — the string lanes (2026-10-20)

U10 retired the numeric/call/table try_table-enter tax; the residual gap
was strings: h_strlong ran ~10× interp warm, h_concat ~2×, and the
probe decomposition attributed it to the same per-protected-call enter
(wazero v1.12 clones a 10,880-byte machine stack per `try_table` enter),
not to C codegen. U11 lifts the same posture onto the string-shaped
opcodes and two pure stdlib callees. Every probe is the stock macro arm
or stock function body verbatim, unprotected, miss = the unchanged
protected path — semantics identical by construction, proven byte-exact
by the standing gates (no ledger row: no engine ever diverged).

## What landed

**U11a — short-string keys and `#`:**
- `rt_gets_probe` / `rt_sets_probe` — `luaV_fastget`/`fastset` over
  `luaH_getshortstr`/`luaH_psetshortstr` (GETFIELD/SETFIELD arms; nested
  after the U10 int probes in GETTABLE/SETTABLE). Long-string keys and
  every grow path stay on the protected route.
- `rt_len_probe` — `luaV_objlen`'s primitive arms: string shrlen/lnglen
  and table `luaH_getn` with no `__len` (OP_LEN).

**U11b — the pure stdlib callee probe (`rt_stdcall_probe`):**
`string.byte` and `string.find`'s plain search (explicit `plain` flag
or `nospecials` pattern) run inline at non-tail OP_CALL — no precall,
no trampoline, no host crossing. The callee must be the stock function
itself: both targets are pinned **once per process** at the first
`rt_set_state` by a raw registry walk (globals pristine there; light C
functions carry their pointer inline, so the pins are
state-independent). A script that swaps `string.byte` — closure or C
function — stops matching and takes the protected call; a saved
original restored under the name matches because it is the original.
Result staging mirrors `rt_call_addr` exactly: want-truncated,
nil-padded, its success encoding (`0` fixed / `-(nret+1)` multret,
nret capped at 8), so the wasm top-adjust is reused verbatim. Miss = 1
(the probe never errors); the raising shapes — non-string subjects
(stock coerces numbers), non-integer indices, the INT_MAX slice guard,
pattern specials — all decline to the protected call, which raises the
stock texts. Two tree facts the port had to match: `svalue()` no longer
exists in 5.5 (`getstr(tsvalue(o))`), and `luaL_pushfail` is **nil**
in this build (`LUA_FAILISFALSE` unset) — find's miss value is nil.

## The guard-seam hazard (caught by the host gate before anything shipped)

The blob's `luaH_pset{,str,shortstr,int}` are not stock: they are the
U6c write-protection guards (`rt_seam55.c`, row 94), which
`luaG_runerror("Attempt to modify a readonly table")` — a real longjmp.
An "unprotected probe cannot raise by construction" claim built on
STOCK table semantics is therefore wrong for the set probes: under
globals protection, `string.foo = 1` / `_G.pairs = nil` (SETFIELD,
string constant key) longjmp'd out of `rt_sets_probe` with no catch and
surfaces as a wasm `unreachable` trap instead of the clean
ScriptError. Both set probes (`rt_sets_probe`, and `rt_seti_probe`
from U10, latent — no protect test used a dynamic integer key) now
decline `rt_prot_isprotected` tables; the protected path raises
properly under `rt_run`. The native battery never links the seam
object, so only the Go protect surfaces can see this class —
`go test ./host/` and TestM6cProtectGlobals are the detectors that
matter for probe work.

## Enter attribution (wazero, warm; 10,880-byte allocations = enters)

| lane | U10 | U11 | residual |
|---|---|---|---|
| h_strlong | 27,439 | 14,371 | string.format ~600, gmatch-iterator ~600, misc |
| h_concat | 12,028 | 6,059 | the `..` concats (~5,920: 1,920 `acc..i` + 4,000 `"k"..i`) |
| h_str | 1,329 | 533 | format/misc |

h_concat's drop (−5,920) matches the U10-era decomposition exactly.

## Benchmarks (same-day, quiet machine — `bench/2026-10-20-u11.txt`)

Medians, warm = pooled VM rerun on the production path (`host/` on
wazero); the U10 column is the same-day U10 run (`bench/
2026-10-20-u10.txt`):

| lane | interp | U10 warm | U11 warm | U11 / interp |
|---|---|---|---|---|
| h_str | 181 µs | 552 µs | **297 µs** | 1.6× (was 3.1×) |
| h_strlong | 1.54 ms | 15.2 ms | **6.01 ms** | 3.9× (was ~10×) |
| h_concat | 946 µs | 5.2 ms | **3.20 ms** | 3.4× (was 5.5×) |

Non-string lanes unchanged within noise (h_numloop 41.7 ms,
h_tablechurn 40.3 ms, h_hostfn 5.2 ms warm — same-day re-measures of
the U10 numbers). The residual string gap is now the two shapes the
probes deliberately leave alone: allocating `..` (h_concat, U11c) and
format/gmatch C calls (h_strlong).

## Open items

- U11c (stretch, not started): concat memory pre-flight — the
  allocating `..` itself. Needs an rt memlimit-headroom check so the
  probe can decline the OOM/raise shapes; h_concat's ~5,920 residual
  enters are all this.
- `string.format` and the gmatch iterator as stdcall targets
  (format allocates — it needs U11c's pre-flight first).
- GETTABUP/SETTABUP: globals reads/writes still pay the protected
  `_ENV` route (the `t["k"..i]` shape now rides the probes only once
  the concat is constant-folded).
- wazero upstream: the per-enter stack clone is an engine cost the
  probes route around, not fix.
