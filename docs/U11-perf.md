# U11 — the string lanes (2026-10-20, U11c + U11d; U11e 2026-10-21)

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

**U11d — `string.format` as a stdcall probe target (2026-10-21):** the
top open item above. `str_format`'s fold rides `rt_stdcall_probe`
(the U11b pin/dispatch/staging are reused verbatim — pin `"format"`
at first `rt_set_state`, `sc_format_arm` in `rt_abi55.c`), so no new
export, no backend change, no blob-interface change. Pass 1 walks the
format string with stock's own parser (`getformat`/`checkformat`/
`get2digits`/`addlenmod`, verbatim), screening each arg cell and
formatting numeric pieces into per-piece C buffers so `total` is
exact; pass 2 emits into the single result (`luaS_newlstr` ≤ 40 /
`luaS_createlngstrobj` > 40) behind the shared two-sided pre-flight
(`rt_probe_mem_ok`, factored out of the concat probe at U11d so the
two cannot drift). The hex-float formatter is the tree's own fallback
(`num2straux` — this build has no system `%a`), ported verbatim for
`%a`/`%A` and `%q`-float; `addquoted` became count/emit twins with a
`lua_assert` that they agree. Misses: every raising shape (bad
conversion, missing arg, modifiers on `%q`), numeral-string coercion
(stock coerces `"42"` in `%d`; the probe declines string cells), `%p`,
formats beyond 24 pieces, and — the re-entrancy guard — any
string/number/boolean/nil type whose metatable defines `__tostring`
(stock's `luaL_tolstring` calls it; the probe declines and the
protected path runs it). Two 5.5.1 facts the guard work surfaced:

- **5.5.1 has no `TM_TOSTRING` slot** — the TM enum ends at
  `TM_CLOSE`; `__tostring` is `luaL_getmetafield`'s rawget of the
  string on the *real* metatable. The probe pins the `"__tostring"`
  short string at `sc_resolve` and rawgets exactly that.
- **`luaH_getshortstr` writes its out-cell only on hit**
  (`finishnodeget` copies only non-nil results) — an absent key
  leaves the caller's TValue untouched, so the guard must pre-nil the
  cell or it reads stale stack and declines everything. This is also
  why `sc_resolve` pre-nils its pin cells.

Enter measure (probe-alloc, warm, 10,880-byte allocations = enters):
h_strlong 14,366 → **13,801** — the ~600 `string.format` calls are
off the protected path; the residual is the gmatch iterator (a C
closure, next open item) plus the per-line `string.sub`-shaped stdlib
calls. h_concat/h_str move only within noise (h_concat holds a
single format call; the U11c-era "139 = format" attribution was
loose — that residual was never format-dominated).

**U11e — the `// % / ^` arith arms (2026-10-21):** not a string lane
but the same measured family — the U11d residual decomposition put
the MOD/IDIV/DIV/POW fallbacks at ~43% of h_strlong's enters (every
`%` and `^` paid the protected `rt_arith`; U10's fast paths cover
only ADD/SUB/MUL and the compares). The reg and K arms now run
in-wasm:

- **int×int MOD/IDIV** — `luaV_mod`/`luaV_idiv` verbatim on
  `i64.rem_s`/`div_s` (C99 trunc) plus stock's floor fixups
  (`r != 0 && (r^n) < 0 → r += n`; `(m^n) < 0 && m%n != 0 → q -= 1`);
  n ∈ {0,−1} declines to the MMBIN slot (the `'n%%0'` raise and
  `attempt to divide by zero`, and the MININT/−1 overflow-avoid arm
  — a decline is always correct).
- **every float or mixed mix** — `op_arithf`'s law (tonumberns both
  operands, result always float): DIV/IDIV inline `f64.div`
  (+`f64.floor` = `luai_numidiv`), MOD/POW through two new plain
  exports **`rt_fmod`/`rt_pow`** — `luai_nummod`/`luai_numpow`
  verbatim, plain because neither can raise (NaN/inf are the stock
  results: `1.0 % 0` → `nan`, `1.0 // 0` → `inf`, no enter).
- **K forms** (MODK/IDIVK/POWK/DIVK) mirror the arms with the
  constant known at compile time — n ∈ {0,−1} is decided at
  emission, not runtime.

Enter measure: the arith micros (a `%`-loop shape) fall **4,336 → 3**
enters — the process floor, same as ADD; h_strlong **13,801 →
6,942** (−50%); h_concat 172 → 79, h_str 526 → 493. Warm h_strlong
4.99 → **2.87 ms** (2.2× interp same-day, was 3.8× at U11d); the
non-string lanes are unchanged within noise (numloop 39.5 ms, nsieve
41.6, fib 131.4, tco 9.1, callret 21.5 — same-day,
`bench/2026-10-21-u11e.txt`).

Three codegen lessons the smoke flushed out:

- **Two latent `wasm/instr.go` opcode bugs, both dead code until
  U11e first used them**: `I64RemS` was written 0x80 (`i64.div_u` —
  the smoke's `div_u` quotients, including a magic
  2635249153387078801 = div_u(2⁶⁴−3, 7), gave it away) and
  `F64Floor` was 0x9D (`f64.trunc` — `floor(-0.1667)` came back
  `-0.0`); `F64Ceil` was likewise 0x9C (floor). Fixed to spec
  (0x81 / 0x9C / 0x9B).
- **`fltArm` keys on reg opcodes**: the K family must map through
  `op − OP_ADDK + OP_ADD` (the `i64Op` idiom) before the call — an
  unmatched switch arm emits no operator at all, and only the wasm
  validator's stack-shape check catches it.
- The one-ULP `^` residue the differential smoke shows on
  interp-vs-wasm float pow is row 48's documented Go-vs-musl libm
  divergence — confirmed pre-existing by stash-testing; clua55↔wasm
  is byte-exact on the full float grid.

No ledger row: no engine diverged. Gates: native ×3, short, heavy
testdiff (976.8 s), host plain + `-race -count=2`, the four CLI legs
(1501/1501, 96/96+3 skips, 1501/1501, 1491/1491+10 skips), and
`_cli-tests` on both engines — all green.

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

| lane | U10 | U11 | U11c | U11d | U11e | residual |
|---|---|---|---|---|---|---|
| h_strlong | 27,439 | 14,371 | 14,366 | 13,801 | **6,942** | GETTABUP globals (~5.4k) + gmatch + misc |
| h_concat | 12,028 | 6,059 | **139** | 172* | **79** | sub/misc (its ONE format call now rides the probe) |
| h_str | 1,329 | 533 | 533 | 526 | **493** | misc |

\* cross-blob recount, not a regression signal: the residual was never
format-dominated (one format call in the lane), and enter counts are
only comparable same-binary.

U11's h_concat drop (−5,920) matched the U10-era decomposition exactly;
U11c's (−5,920) takes the `..` fold itself off the protected path —
under the DEFAULT host posture (no budget), where the v1 pre-flight
was measuring 6,059 because it gated on the memlimit (see U11c above).

## Benchmarks (same-day, quiet machine — `bench/2026-10-21-u11e.txt`)

Warm = pooled VM rerun on the production path (`host/` on wazero).
Columns U10/U11/U11c are the 2026-10-20 runs (`bench/2026-10-20-u10.txt`,
`bench/2026-10-20-u11.txt`, `bench/2026-10-20-u11c.txt`); U11d and
U11e + their interp columns are same-day 2026-10-21
(`bench/2026-10-21-u11d.txt`, `bench/2026-10-21-u11e.txt`) — per the
bench protocol the same-day/interp ratios are the only comparisons
that cross columns cleanly (the machine shifts between days; absolute
cross-day deltas are not meaningful):

| lane | U10 warm | U11 warm | U11c warm | U11d warm | U11e warm | U11e / interp |
|---|---|---|---|---|---|---|
| h_str | 552 µs | 297 µs | 282 µs | 224 µs | **215 µs** | 1.7× (interp 128 µs) |
| h_strlong | 15.2 ms | 6.01 ms | 5.98 ms | 4.99 ms | **2.87 ms** | **2.2×** (interp 1.32 ms; was 3.8× at U11d) |
| h_concat | 5.2 ms | 3.20 ms | 1.55 ms | 1.49 ms | **1.45 ms** | 1.7× (interp 875 µs) |

Non-string lanes are at or past interp parity same-day (numloop
39.5 ms vs 116.6, fib 131.4 ms, nsieve 41.6 ms, tco 9.07 ms,
callret 21.5 ms — unchanged within noise, as expected: U11e removes
enters, not work, and those lanes' arith was already a minor share).
U11e takes ~43% off h_strlong warm — the arith fallbacks were 43% of
its enters and the two match. The residual string gap is the
per-loop `_ENV` global reads and the gmatch iterator (below).

## Open items

- **GETTABUP/SETTABUP globals (the top item now):** every per-loop
  `string.byte` / `string.find` / `string.format` global read pays a
  protected `_ENV` enter (~5,400 of h_strlong's 6,942 residual) —
  the same getshortstr shape as U11a's `rt_gets_probe`, but against
  the frame function's `_ENV` upvalue cell.
- The gmatch iterator as the next stdcall target (~600 enters; a C
  closure with 3 upvalues, not a light C function — it needs its own
  pin/dispatch shape, not `rt_stdcall_probe`'s `ttislcf` check, plus
  a TFORCALL-site hook).
- wazero upstream: the per-enter stack clone is an engine cost the
  probes route around, not fix.
