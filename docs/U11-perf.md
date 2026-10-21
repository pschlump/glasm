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

**U11c — the concat fold (`rt_concat_probe`):** the allocating `..`
itself. One unprotected pass over OP_CONCAT's operand cells: strings
read directly, numbers formatted into a C stack buffer via stock's own
`luaO_tostringbuff` (allocation-free — and the cells stay unconverted,
which is what the protected path already did to them: `concat_body`
copies to the Lua stack and converts the *copies*), then exactly one
result allocation like stock's last pass — `luaS_newlstr` (≤40,
interned; a repeat returns the existing object) or
`luaS_createlngstrobj` (>40, never interned). Numbers formatted to
buffers instead of interned temps is strictly fewer allocations, same
bytes; the frozen-GC posture makes the difference invisible.

The design's crux is the **two-sided memory pre-flight**, because an
allocation inside an unprotected probe can raise (OOM → `luaM_realloc_`
longjmps), and a raise with no catch is a wasm trap:

- **The real allocator side** — a `malloc(need)` round trip (give it
  back immediately): if the worst-case contiguous block is servable, a
  following request of at most `need` bytes cannot fail; a NULL is
  exactly the real wall (linear-memory max, dlmalloc fragmentation),
  which no accounting predicts — and the *only* check there is under
  `limit == 0`. Sum-contiguity is stricter than the 1–2 separate
  requests ever need, so it only declines early.
- **The cap side** — `rt_alloc`'s own refusal arithmetic
  (`rt_mem_used + need > rt_mem_limit`) when a budget is set, plus the
  exact `growstrtab` prediction (`nuse >= size` → the doubled array's
  peak) for the short-interning path.

Two lessons the lane itself caught:

- **v1 gated on the rt memlimit and was completely inert**: the host's
  default posture is `maxMem == 0` = unlimited, so the probe declined
  every concat on every default-configured engine — the enter
  re-measure showed h_concat unchanged at 6,059 until the pre-flight
  was made two-sided. Any probe that conditions on the budget must be
  measured under the *default* configuration, not just the budgeted
  test surfaces.
- **The seam's probe families disagree on return codes**: the U10/U11a
  table probes return **1 = hit** (`I32Eqz → protected`), the U11b
  stdcall probe **0 = hit** (`== 1 → miss`). The concat probe follows
  its opcode neighbors (1 = hit); writing the arm against the wrong
  convention silently runs the protected path on every hit — the first
  differential smoke catches it (the fold never accumulates), but only
  if you look at content, not just exit codes.

h_concat's residual 139 enters are its `string.format` calls
(h_strlong's unchanged ~14.4k are format + gmatch — the next open
items). No ledger row: no engine diverged; every miss shape takes the
byte-identical protected path, proven by the matrix, error suite,
oracles, and host gates.

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

| lane | U10 | U11 | U11c | residual |
|---|---|---|---|---|
| h_strlong | 27,439 | 14,371 | 14,366 | string.format + the gmatch iterator |
| h_concat | 12,028 | 6,059 | **139** | the 139 = the lane's string.format calls |
| h_str | 1,329 | 533 | 533 | format/misc |

U11's h_concat drop (−5,920) matched the U10-era decomposition exactly;
U11c's (−5,920) takes the `..` fold itself off the protected path —
under the DEFAULT host posture (no budget), where the v1 pre-flight
was measuring 6,059 because it gated on the memlimit (see U11c above).

## Benchmarks (same-day, quiet machine — `bench/2026-10-20-u11.txt`)

Medians, warm = pooled VM rerun on the production path (`host/` on
wazero); the U10/U11 columns are the same-day runs (`bench/
2026-10-20-u10.txt`, `bench/2026-10-20-u11.txt`), the U11c column and
the interp re-measure are `bench/2026-10-20-u11c.txt`:

| lane | interp | U10 warm | U11 warm | U11c warm | U11c / interp |
|---|---|---|---|---|---|
| h_str | 181 µs | 552 µs | 297 µs | **282 µs** | 1.6× (was 3.1×) |
| h_strlong | 1.54 ms | 15.2 ms | 6.01 ms | **5.98 ms** | 4.2× (was ~10×) |
| h_concat | 946 µs | 5.2 ms | 3.20 ms | **1.55 ms** | 1.66× (was 5.5×) |

Non-string lanes unchanged within noise (h_numloop 41.7 ms,
h_tablechurn 40.3 ms, h_hostfn 5.2 ms warm — same-day re-measures of
the U10 numbers). U11c halves h_concat again (3.20 → 1.55 ms warm, 1.66× interp);
h_str/h_strlong are unchanged within noise, as the probe predicts.
The residual string gap is now only the C-call shapes:
format/gmatch (h_strlong, h_str).

## Open items

- `string.format` and the gmatch iterator as stdcall targets
  (format allocates its result — U11c's two-sided pre-flight is the
  template; h_strlong's ~14.4k residual enters are format + gmatch).
- GETTABUP/SETTABUP: globals reads/writes still pay the protected
  `_ENV` route (the `t["k"..i]` shape rides the probes once the concat
  is constant-folded).
- wazero upstream: the per-enter stack clone is an engine cost the
  probes route around, not fix.
