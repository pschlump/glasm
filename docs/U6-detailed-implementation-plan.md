# U6 implementation plan — the runtime cutover

> Parent plan: `docs/Upgrade-from-5.2-to-5.5-of-Lua.md` §6 (U6 row), decisions D6, D8, D9, D10, D12.
> Same working-breakdown convention as `docs/U5-detailed-implementation-plan.md`: independently gated
> chunks, documented deviations, status log. The divergence-ledger discipline applies to every chunk:
> a divergence without a row is a bug; a row without a covering test is a bug.

## 0. What U6 is (from the parent plan)

The runtime cutover completes: **ABI v4 grows its full production surface** and the 5.1-era v3
surfaces retire from every consumer. Concretely (milestone table + U5f's "Next" note):

- **`rt_abi55.c` fully on `lua55/`** — the entries `rt_abi55.h:26` explicitly defers ("Not in
  tranche 1 (U6…): rt_sandbox / globals lockdown, the budget alloc + deadline block, the host-fn
  seam"): `rt_set_memlimit`/`rt_alloc` (row 36), `rt_ctrl_addr`/`rt_deadline` + the backend's
  deadline polls (row 37), `rt_sandbox` + `rt_protect_globals` + `rt_globals_readonly` re-pinned on
  5.5 globals (rows 33/34/50), and `rt_hostfn` + `rt_encode_value` + the `host.host_call` seam
  (row 61f).
- **The prod blob** — `lua55_prod.wasm` (the `lua51_prod.wasm` flavor, rows 33/34, rebuilt on the
  zero-patch 5.5 tree): base/table/string/math(+utf8) only, zero `wasi_snapshot_preview1`
  imports, SHA-pinned artifact gate; the `wazero-prod` engine re-points at it.
- **Memlimit/deadline re-validation** — the M6d gates and the M6c determinism prod leg, un-skipped
  on the v4 blob.
- **Every wasm/wazero gate running 5.5 corpora × {interp, clua55, wasm, wazero, wazero-prod}.**
- **Row 86's frame-layout surgery** rides U6 (the extras-above-window bug family), closing the
  var07/vgap01/limits01 skips and the life1/tsort1 CLI xfails (row 61(d)).
- **Row 84's yield surface rides it** — the wasm backend's coroutine posture gets an explicit
  ruling + covering tests (§4.2).

**Gate (milestone table):** `TestWasmFullMatrix`/`TestWasmErrorSuite`/`TestWasmGluaFull` on 5.5
corpora × {`interp`,`clua55`,`wasm`,`wazero`,`wazero-prod`}; M6c artifact/sandbox/determinism
gates re-run on `lua55_prod.wasm`. Plus every prior gate (U1–U5, M5/M6, host, `_cli-tests`).

**Already dead and never to port (do NOT redo):** `rt_set_dialect`/`rt_gnumfmt`/`gnumfmt.c`
(D7 — stock texts everywhere since U2f), `rt_compat_arg` (died with U5b's vararg model),
`LUA_GLOBALSINDEX`-based host code (5.2+ removed it), the frozen 5.1 `RT_T*` wire tags 0..8
(superseded by 5.5 variant tags — the wire protocol re-pins in U6e).

## 1. Ground-truth inventory (verified 2026-10-08, tree + greps + oracle-plan reads)

### 1.1 The v3-only rt surface — reference implementations in `runtime/rt_abi.c`

All of these are **absent from `rt_abi55.c` today** (grep-verified; the file is 1777 lines of
tranche-1 v4 work only). The v3 bodies are the porting reference:

| entry | v3 site | contract highlights |
|---|---|---|
| `rt_sandbox(on)` | rt_abi.c:270 | nils a static name list out of `_G` (`rt_sandbox_nil[]` :266 — io, os, package, require, module, dofile, loadfile, load, loadstring, debug); returns how many were present (dev opens 10, prod sees 0 — tests pin both); belt-and-suspenders in the prod flavor |
| `rt_protect_globals(on)` | rt_abi.c:329 | Redis 7.2.7 script-env lockdown: error metatable on `_G` whose `__index` raises `Script attempted to access nonexistent global variable '<k>'` (`rt_protected_index` :302), then recursive readonly via `rt_protect_rec` :314 using **`lua_isreadonlytable`/`lua_enablereadonlytable` — deps/lua fork extensions that exist in our patched 5.1 tree (row 50) and DO NOT EXIST in stock 5.5** |
| `rt_globals_readonly(on)` | rt_abi.c:343 | the KEYS/ARGV staging window: toggles `_G`'s own readonly flag (same fork extension) |
| `rt_set_memlimit(bytes)` | rt_abi.c:385 | init-time, per-VM lifetime; **host must call it BEFORE `lnewstate`** so stdlib-open + shims + constant pool + script all count |
| `rt_alloc` | rt_abi.c:395 | the `lua_newstate` allocator: budget check at the chokepoint → refusal returns NULL → stock `luaM_realloc_` raises LUA_ERRMEM `not enough memory` (bare literal, pcall-catchable); emergency slack `RT_MEM_EMERGENCY` (64 KiB) while `rt_oom_inflight` (the OOM raise itself allocates — interning the message); `rt_err_clear`/`rt_pcall_caught`/`rt_set_state` re-arm; frees clamp at 0 |
| `rt_ctrl_addr`/`rt_set_deadline`/`rt_deadline_flag` | rt_abi.c:440–448 | `g_deadline_flag` static; host watchdog writes a single aligned 4-byte LE store of 1 at `rt_ctrl_addr()` (the one safe off-thread crossing into linear memory; blobs declare a memory max so the base never moves) |
| `rt_deadline` | rt_abi.c:458 | stages `context deadline exceeded` (byte-identical to the interp's `ctx.Err()` body; **unprefixed — ruled divergence, row 37**), sticky flag for the run (catch-and-continue re-raises at the next poll) |
| `rt_hostfn` + `host.host_call` + `rt_encode_value` | rt_abi.c (M7a block, ~:1100+) | Lua→Go host functions (`redis.call` seam): tag-first wire protocol over a staging buffer, `rt_where_mark` for verbatim command texts; `rt_encode_value` = any TValue cell → wire bytes with zero TString/Table layout knowledge on the Go side |

**Driver wiring:** `luawasm55.c:335` `d_alloc` is a plain realloc shim whose own comment says
"the budget-tracking rt_alloc arrives with the ABI v4 port (U6)"; `lnewstate` (:346) currently
calls `lua_newstate(d_alloc, NULL, 42)`.

### 1.2 The v4 blob today

- `runtime/rt_abi55.{c,h}` — tranche 1 complete: value construction with variant-tag cells,
  tables/arith/len/eq/lt/le/concat with hint i32s, `rt_call(funcell, argcells, nargs, want, line,
  hcallee)` (:627), forprep, error staging, the wasm-proto/upvalue/tbc registries, the vararg
  entries, frame stack (`rt_wasm_push_frame` :906 / `rt_frame_cursor` :911 / `rt_frame_restore`
  :915), tail staging, and `rt_seam55.c`'s interposed position/msgh seam (U5e).
- `runtime/build55.sh` builds **two flavors only**: `lua55_sjlj.wasm` (oracle) and `lua55_rt.wasm`
  (ABI v4). **No prod flavor exists** — that's U6d. Determinism pins: `-D'luai_makeseed()=0u'`,
  `lua_tmpnam` neutralized, `--max-memory=268435456`, state seed 42 (the memory's seed-regression
  note: never pin `g->seed` to 0).
- GC is frozen per run (`rt_set_state` sets GCSTPGC) — row 61's clause; see §4.3.

### 1.3 Row 86 — the frame layout and its tramples

- **Extras live ABOVE the window**: `rt_push_frame_from` (rt_abi55.c:891–902) copies params to
  `frame+0..` and extras to `frame + 16*framecells` — i.e. past the `nregs+4` register window.
  Stock and the interp place them below the frame base. Three legitimate dynamic windows
  therefore trample them (ledger row 86, found at U5e):
  1. **vgap01** — a multret `OP_VARARG` flush tail reaches `R(nregs)..` = extra[0];
  2. **var07** — `emitCall`'s dynamic arg-shift loop reads/writes past the callee cell at n≥12
     (the saved callee rides `scratchDyn0`, the dynamic scratch above the window — emit.go:116);
  3. **limits01** — register-pressure edge of the same family.
- **`emitCall`'s save+shift** (ops.go:497–560): callee saved to `scratchDyn0`, args shifted one
  cell down (`R(A+i) := R(A+1+i)`, static loop :541 or the dynamic `dynCellAddrSt` copy loop
  :530–539) so `rt_call`'s arg window = `R(A)..` and results land where the VM expects.
- **Row 61(d) (open)** — "a GETTABLE result feeding a call's arg window reads stale cells in some
  register layouts" (life.lua's `live()`; the `_cli-tests` life1/tsort1 xfails). Same seam family:
  the arg window vs the move machinery.
- **Durable fix (row 86's own prescription, both halves):**
  - **(B) extras-below-the-window** — `rt_push_frame_from` allocates `[nv extra cells][window]`,
    window base = `block + 16*nv`; everything reaching past `nregs+4` lands in virgin chunk
    space. The backend's `varargCell(j)` addressing (emit.go:167) and the VARARGPREP
    `rt_varargtab` staging shift accordingly; the frame-cursor growth law re-audited.
  - **(A) callee-address `rt_call`** — a new entry taking the callee's *address* (funcaddr,
    argcells at their natural `R(A+1)..` position, results moved down to the dst cell by the rt
    itself): kills the save+shift machinery entirely, both loops, and the scratchDyn0 callee
    hazard. `rt_tail_stage`'s arg window likewise. Additive entry (`rt_call_addr`) — the ABI v4
    freeze holds (§4.1).

### 1.4 Consumers gated on U6 — the exact refusal/skip sites

- **Engines refuse the options on the v4 blob**: `testdiff/wazeroengine.go:266` (Sandbox:
  "rt_sandbox is v3-only until U6 (row 60); refuse rather than…"), `:236` (MaxMem), `:304–305`
  (Deadline: "blob has no rt_ctrl_addr/rt_deadline block"); `testdiff/wasmengine.go:468` and
  `:511–513` (same two). The `Sandbox/MaxMem/Deadline` fields and their plumbing already exist —
  U6b/U6c delete the refusals.
- **`skipRtSurfacesUntilU6`** (testdiff/skips.go:130 — "rt sandbox/memlimit/deadline + prod-blob
  surfaces are v3-only until the U6 cutover — ledger row 60"): called from **10 gate functions**
  across m6c_test.go (:100, :165), m6d_test.go (:43, :93, :121, :172, :219, :251, :286),
  m6cdet_test.go (:134). Plus m6cdet_test.go:90 — the prod-blob determinism leg ("the 5.1 prod
  blob cannot host v4 modules — row 60").
- **The host package run path is dead**: `host/host_test.go:32` — `mustCompile` (the funnel for
  ~15 gates: EvalBasics, HostFunction*, DeadlineKill, MemoryBudget, Sandbox, Concurrency,
  SharedVM, EventSink, …) skips with "host run path awaits the U6 prod-blob cutover (v4 module
  over the 5.1 blob) — ledger row 60". `host/blob.go:30` still `//go:embed lua51_prod.wasm` (v3);
  `host/vm.go` calls `rt_set_memlimit` (:178), `rt_sandbox` (:203), `rt_hostfn` (:238),
  `rt_protect_globals` (:262), `rt_ctrl_addr` (:269), `rt_globals_readonly` (:412/:420),
  `rt_encode_value` (:574/:590).
- **Corpus skips riding U6 rows** (testdiff/skips.go `u5WasmSkips`): var07/vgap01/limits01 → row
  86 (U6a); coyield01–06 → row 84 (U6f); gcparam01 → row 61 (GC freeze, §4.3); dump01/load01 →
  row 60 (CClosure presentation — stays ruled, §4.7); getinfo01/udata01 → row 60 family;
  e001/e006/e007 → row 60 residuals (§4.6); `_cli-tests`: life1/tsort1 xfail (row 61(d)),
  kvdb/calc1 xfail (e001 residual).
- **Backend emits NO deadline polls today**: `luawasm/backend.go:244` names "the deadline block
  (U6)" among the deferred v1-era features. Design A5 (deadline via direct memory write polled
  at back-edges) is the contract; v1 had it, the U2e rewrite dropped it.

### 1.5 5.5 deltas that reshape the ported entries

- **`LUA_GLOBALSINDEX` is gone** (5.2+): the globals table is `LUA_RIDX_GLOBALS` in the registry
  (`lua_rawgeti(L, LUA_REGISTRYINDEX, LUA_RIDX_GLOBALS)`); `rt_protect_globals`' metatable
  install and the sandbox's `_G` walk re-point there. Globals access from compiled code runs
  through the main proto's `_ENV` upvalue cell (U1/U2c) seeded with the registry globals table —
  so runtime nil-ing under `_ENV` "just works" for readers; `_ENV`-swap scripts keep their own
  table (matches the interp).
- **The readonly-table fork API is gone** (`lua_enablereadonlytable`/`lua_isreadonlytable` were
  row-50 patches into OUR 5.1 tree; the 5.5 tree is **zero-patch by policy**, VENDORED.md). This
  is U6c's real design problem — §4.4.
- **The sandbox list is 5.1-shaped**: `loadstring`/`module` no longer exist; 5.5's compile surface
  is `load`/`loadfile`/`dofile`/`require` (+`package`); `warn` is new (nil it — it writes to a
  host callback we don't ship in prod); `utf8` stays (string data, no host surface). §4.5.
- **The wire protocol's number tag splits**: v3's `RT_TNUMBER` covered one float64 kind; 5.5
  values carry `LUA_VNUMINT` vs `LUA_VNUMFLT` — `rt_encode_value` and `host/wire.go` grow an
  integer tag (values must round-trip losslessly: int64 stays int64).
- **Prod flavor sources**: mirror `build.sh:74–100`'s mechanics on the 5.5 tree — exclude
  `liolib.c loslib.c loadlib.c ldblib.c` (+ luac's `print.c`) from the link, `-DLUAWASM_PROD`
  guards for the base-lib `load`/`dofile`/`require` entries and stock `print` (the driver's
  `g_print` host shim stays), `-ffunction-sections -fdata-sections` so the exclusions actually
  drop the wasi-libc archive members. The **zero-WASI artifact gate** (a Go test parsing the
  import section, failing on any `wasi_snapshot_preview1`) is the enforcement, exactly as v3.

## 2. The chunks

| # | Name | Scope | Gate |
|---|---|---|---|
| U6a | Row-86 frame-layout surgery + row-61(d) | (B) extras-below-window in `rt_push_frame_from` + backend vararg addressing; (A) additive `rt_call_addr`/tail-stage arg form replacing save+shift; scratchDyn0/margin audit; native ABI battery for the layout | TestWasmFullMatrix/ErrorSuite/GluaFull ×{wasmtime,wazero} green with **var07/vgap01/limits01 un-skipped**; `_cli-tests` **life1/tsort1 un-xfailed** (row 61(d) closed); RT-NATIVE55 ×{plain,ASan,UBSan}; all prior gates |
| U6b | Budget alloc + deadline block on v4 | `rt_set_memlimit`/`rt_alloc`/`rt_mem_used_bytes` + `rt_ctrl_addr`/`rt_set_deadline`/`rt_deadline_flag`/`rt_deadline` in rt_abi55.c; `lnewstate` → rt_alloc; backend deadline polls (dispatch-loop head + tailcall restage) + engine wiring; un-refuse MaxMem/Deadline | **m6d gates + m6cdet leg un-skipped on the v4 blob** (row 36/37 re-validation); native ABI OOM/deadline battery; wasm/wazero gates unchanged-green; perf spot-check (poll cost) |
| U6c | `rt_sandbox` + globals lockdown on 5.5 | 5.5 sandbox list; zero-patch protect-globals (§4.4 design: protected-set + interposed write path + backend fast-path gating; or the ruled fallback); `rt_globals_readonly` window | **m6c sandbox gates un-skipped on the dev blob** (sandboxed-dev ≡ prod precondition); `_u6` sandbox corpus; the protection's covering tests |
| U6d | `lua55_prod.wasm` flavor | build55.sh PROD leg (source exclusion + LUAWASM_PROD guards + zero-WASI); SHA pin + copies (testdiff/, host/); `wazero-prod` engine re-pin | **TestM6cProdBlobArtifact on lua55_prod.wasm** (import-section: zero wasi, exactly the expected `host.*` set); determinism prod leg; oracle flavor unchanged |
| U6e | hostfn + encode_value + host cutover | `rt_hostfn` + `host.host_call` + `rt_encode_value` on v4 (variant tags); `host/wire.go` int tag; host embeds lua55_prod.wasm + SHA bump; `mustCompile` funnel un-skipped | **`go test ./host/ -race` green** (~15 revived gates incl. DeadlineKill/MemoryBudget/Sandbox/ChaosHost); `CGO_ENABLED=0 go build ./host/`; examples run |
| U6f | Yield ruling + gate expansion + closeout | row-84 wasm posture probe + ruling (§4.2); clua55 + wazero-prod columns on the wasm-family gates; `_u6` corpus complete; ledger rows; docs + parent status header; memory | **U6 exit gate (§5)** |

Dependencies: U6a first — every later chunk rides the new frame layout and re-verified gates. U6b
then U6c (both touch rt_abi55.c + backend, one seam at a time; c's backend gating reuses b's
fast-path-fallback experience). U6d after U6c (the prod flavor's guarded-open shape depends on the
sandbox list). U6e after U6d (needs the prod blob to embed). U6f last. Each chunk lands green
before the next starts.

## 3. Chunk details

### U6a — row-86 frame-layout surgery + row-61(d)

**rt (`runtime/rt_abi55.c`):**
- `rt_push_frame_from`: allocate `16*nv` extra cells BELOW the window — block layout
  `[extras nv][window framecells]`, window base returned as before (callers see the window; the
  vararg base becomes `frame - 16*nv`). Re-audit the chunk `need` computation and the frame-cursor
  law (`rt_frame_restore` semantics unchanged — cursor still points at the block start).
- Additive `rt_call_addr(funcaddr, argcells, nargs, want, line, hcallee, dstcell)`: reads args at
  `argcells` (their natural `R(A+1)..` position), stages the call exactly as `rt_call`, and moves
  results down to `dstcell` (`R(A)`) itself — including the multret/want=-1 top bookkeeping the
  shift used to leave to the backend. `rt_call` stays exported (ABI freeze) but the backend stops
  emitting it. `rt_tail_stage` gains the same arg-window form (callee by address, no prior move).
- The MMBIN*/reentrancy law is untouched: `rt_call_addr`'s argument statics (funcaddr, argcells,
  counts) must live on the C stack across any `luaD_call` re-entry — the A4 lesson, now with
  addresses instead of copied cells.

**backend (`luawasm/emit.go`, `ops.go`):**
- `emitCall` loses the save+shift: pass `&R(A+1)` as argcells, `&R(A)` as dst, the callee cell's
  address directly. Both shift loops (static :541, dynamic :530) delete; `scratchDyn0` survives
  only where still needed (audit remaining users — TFORCALL's iterator staging :743 uses the same
  save pattern; convert it too).
- `varargCell(j)` addressing flips to the below-window base (the VARARGPREP `rt_varargtab`
  staging and `rt_vararg_read` paths in the rt move with it).
- Re-verify the window margin law: with extras below, the dynamic overflow margin above the
  window (multret flush tails, call arg windows) must still be covered by `framecells`' margin —
  the vgap01 class. Add the collision probes as corpus cases (`_u6/frame01..`: multret VARARG
  into a call tail, n≥12 arg windows, GETTABLE-result-into-arg-window = the life.lua `live()`
  shape minimized).

**Gates:** the three row-86 skips un-skipped; `_cli-tests` life1/tsort1 move from xfail to PASS
(kvdb/calc1 stay xfail — e001 residual, §4.6); `runtime/tests` battery extended (push_frame
layout, call_addr result placement incl. want=-1); full wasm/wazero matrix + error suite +
glua-full on the rebuilt blob (`go clean -testcache`); native ×3 sanitizers.

### U6b — budget alloc + deadline block on v4

**rt:** port rt_abi.c:352–475 verbatim-in-spirit into rt_abi55.c: the `rt_mem_limit/used/
oom_inflight` statics, `rt_set_memlimit` (init-time; zeroed counters; re-arm hooks already exist
in the v4 error-staging clear paths — wire `rt_oom_inflight = 0` into `rt_err_clear`/
`rt_pcall_caught`/`rt_set_state` exactly as v3 did), `rt_alloc` (budget chokepoint + emergency
slack + clamp-at-0 frees; refusal → NULL → stock 5.5 `luaM_error`→ LUA_ERRMEM `not enough
memory` — verify 5.5's raise path shapes identically; the interp has NO counterpart surface so
the text is pinned absolutely, row 36), `rt_mem_used_bytes`. Deadline block: `g_deadline_flag`,
`rt_ctrl_addr`, `rt_set_deadline`, `rt_deadline_flag`, `rt_deadline` (staging body
`context deadline exceeded`, `err_prefixed = 1` = no position — row 37's ruling carries; sticky
per run).

**driver:** `luawasm55.c` `lnewstate` switches `d_alloc` → `rt_alloc` (the comment at :332–334
already reserves this). The oracle flavor (`lua55_sjlj.wasm`) keeps `d_alloc` — the oracle must
not grow a budget (TestCLua55Conformance memory posture unchanged); if both flavors link
rt_abi55.c, the switch is a `LUA_RT_FLAVOR` guard or the oracle keeps linking the plain driver.

**backend:** deadline polls — cache `rt_ctrl_addr()` once at `lua_init` into a wasm global;
emit `global.get / i32.load / br_if(deadline-block)` at the flattened dispatch-loop head (once
per basic-block transition, design A5) and at the tailcall restage loop (the trampoline side —
rt_abi55.c's restage loop polls `g_deadline_flag` directly and maps to the same staged error);
the deadline block calls `rt_deadline` then propagates `-1` through the existing `checkStatus`
error path. Perf spot-check: the M8 trip-wire benchmarks re-run (one load+branch per block).

**engines:** delete the MaxMem/Deadline refusals (wazeroengine.go:236/:304, wasmengine.go:511)
— the existing wiring (rt_set_memlimit before lnewstate; watchdog goroutine + the aligned
4-byte store) re-activates on the v4 blob.

**Gates:** `skipRtSurfacesUntilU6` removed from the 7 m6d call sites + m6cdet (:134 stays
skipped until U6d's prod blob exists — split the helper so m6d legs run on the dev blob now);
the m6d corpus re-validates OOM (`not enough memory` catchable, re-arm semantics, slack
bounds) and deadline (kill within ε, sticky re-raise, catch-and-finish) against the v4 blob on
both hosts; native ABI battery (the 5.5-side port of rt_native_test.c's memlimit/deadline
tests — note/u5f's NUL-termination idiom); row-36/37 covering tests re-pointed at the new blob.

### U6c — `rt_sandbox` + globals lockdown on 5.5

- **`rt_sandbox`**: same shape as v3 (:270), new list per §4.5. Runs against the registry
  globals table via the public API (`lua_getglobal`/`lua_pushnil`+`lua_setglobal` on the state
  set by `rt_set_state` — unchanged; 5.5's `lua_setglobal` writes the `_ENV`-seeded globals
  table). Returns the count (dev opens N, prod sees 0 — both pinned).
- **`rt_protect_globals`** (the design decision, §4.4): metatable install ports directly
  (`lua_setmetatable` on the globals table from `LUA_RIDX_GLOBALS`; `rt_protected_index`'s
  Redis texts verbatim); the recursive-readonly half is the zero-patch problem.
- **`rt_globals_readonly`**: the KEYS/ARGV staging window under whatever §4.4 lands.
- **Backend fast-path gating**: whatever the mechanism, compiled code's inline table writes
  (array-part fast paths in GETTABLE/SETTABLE/GETI/SETI lowering) must not bypass the
  protection — a `g_protect_on` i32 (set by `rt_protect_globals`) gates the fast paths into
  their rt slow paths (one load+branch, the U6b poll pattern).
- **Corpus**: `_u6/sbx01..` — sandbox nil-ing (dev), protected read/write raises with the Redis
  texts, KEYS/ARGV window writability, `_ENV`-swap interaction (a swapped `_ENV` is NOT
  protected — scripts may build private tables; matches Redis semantics where the sandbox is
  the globals table only).

**Gates:** the two m6c sandbox gates (:100, :165) un-skipped on the dev blob — sandboxed-dev ≡
prod equivalence re-established as the U6d precondition; the interp/clua55 legs of the corpus
(where a counterpart exists — the lockdown is host-surface, so most cases pin absolutely like
rows 36/37); every prior gate green.

### U6d — `lua55_prod.wasm` flavor

- `build55.sh` grows the PROD leg (v3's build.sh:74–100 as the reference): `PROD55_SRC` excludes
  `liolib.c loslib.c loadlib.c ldblib.c print.c`; `-DLUAWASM_PROD -ffunction-sections
  -fdata-sections`; an linit-style guarded open for the prod flavor (base minus load/dofile/
  require/warn, plus table/string/math/utf8) — either the vendored `etc/linit.c` adapted or a
  local `linit55.c` in runtime/ (zero patches to `runtime/lua55/src/`); the driver's `g_print`
  host shim stays; same EH dialect, same exports as `lua55_rt.wasm` (plus nothing new — sandbox
  is compiled-in as belt-and-suspenders returning 0).
- Copies: `testdiff/lua55_prod.wasm` + `host/lua55_prod.wasm` (the embed + `cmp` anti-drift
  dance, build.sh:124–130 precedent); SHA-256 pin in `host/blob.go` (U6e bumps it; here the
  testdiff-side pin).
- `wazero-prod` engine config re-points from `lua51_prod.wasm` (testdiff/clua.go:33) to
  `lua55_prod.wasm`.
- **Artifact gate**: the import-section parse test on `lua55_prod.wasm` — zero
  `wasi_snapshot_preview1`, exactly the expected `host.*` set (`wasm_dispatch`, `host_call`,
  the event/sink entries — the set the driver imports); plus determinism (the m6cdet prod leg
  un-skips here).

**Gates:** TestM6cProdBlobArtifact (5.5 edition) green; the dev-oracle flavors byte-identical to
before (the PROD leg must not perturb `lua55_sjlj.wasm`/`lua55_rt.wasm` — byte-compare pins);
m6cdet full gate green.

### U6e — hostfn + encode_value + the host cutover

- **rt**: port the M7a block from rt_abi.c onto v4: `rt_hostfn` (register a Go host function as
  a callable value; the `host.host_call` import trampoline with args/results marshaled through
  the wire staging buffer; errors raise via the U5e `rt_where_mark` seam so command texts stay
  verbatim), `rt_encode_value` (TValue cell → wire bytes; **variant tags** — `LUA_VNUMINT` and
  `LUA_VNUMFLT` distinct, int64 lossless; expanded tables with the recursion-depth
  `luaD_checkstack` discipline the chaos-host suite demanded). Reentrancy refusal (`-3`
  RTW_REFUSED) unchanged.
- **host/**: `blob.go` embeds `lua55_prod.wasm` (+ SHA bump + `cmp` with testdiff's copy);
  `wire.go`/`hostfn.go` grow the integer wire tag (Go `int64` value kind surfaces — `Result`
  values keep Lua semantics: integers print bare, `2.0` keeps its `.0`); `vm.go`'s rt_* call
  sites (:178–:590) re-verified against the v4 signatures (all additive — the call sites should
  compile unchanged except the blob swap and any tag-map edits); delete the `mustCompile`
  funnel skip (host_test.go:27–32) and the :492 skip.
- The **one-script-per-image law** and A9 locking re-verify under the 5.5 blob (v1 findings
  assumed 5.1 blob sizes).

**Gates:** `go test ./host/ -race` ×2 green — the revived battery: EvalBasics, BinaryArgs,
CompileErrors, ScriptRuntimeError, HostFunction{RoundTrip,Returns,Errors,PanicContained},
TrampolinesHoldImageLock, HostFunctionReentrancyRefused, ChaosHost, DeadlineKill,
DeadlineNormalAndCancel, MemoryBudget, Determinism, Sandbox, ConcurrentFreshVMs,
SharedVMSerialization, OneScriptLaw, EventSink; `CGO_ENABLED=0 go build ./host/`; examples
(`examples/eval`, `evalserver` under `-race`, `sharedvm`, `bench`) run; BlobPinned re-pinned.

### U6f — yield ruling + gate expansion + closeout

- **Row-84 wasm posture (§4.2)**: probe first — a `coroutine.yield` reached with compiled frames
  below it (compiled code is CClosures over the dispatch seam, i.e. C frames without
  continuations from stock's viewpoint) should make stock's own `luaD_throw` raise the catchable
  `attempt to yield across a C-call boundary` naturally. If the probe shows anything else
  (silent success corrupting state, or a trap), add the explicit guard in the yield path before
  ruling. Then: the coyield01–06 wasm skips re-pin to the ruling (expected: the C-boundary error
  text byte-exact, thread survives under pcall — the interp's row-84g dialect); `_u6/coyield`
  wasm-leg cases; ledger row.
- **Gate expansion**: the wasm-family gates (`TestWasmFullMatrix`, `TestWasmErrorSuite`,
  `TestWasmGluaFull`, `TestU5WasmGate`/`TestU5WazeroGate`) grow the `clua55` column (the corpora
  are already 5.5-shaped — this is engine-set wiring + any skip re-pin) and the `wazero-prod`
  column on `lua55_prod.wasm`. Corpus audit for prod-incompatible entries (io/os users) with
  reason-string skips.
- **Closeout**: `_u6` corpus complete; ledger rows (frame surgery closing 86 + 61(d), the
  budget/deadline re-validation row, the sandbox/protect row incl. §4.4's ruling, the prod-blob
  row, the hostfn/wire row, the yield ruling, the clua55/wazero-prod gate row); docs (parent
  status header + §6 milestone history, this file's status log, CLAUDE.md's architecture notes
  where they name v3-only surfaces); memory; `skipRtSurfacesUntilU6` deleted outright.

## 4. Documented rulings / deviations (decided here, ledgered in the chunks)

1. **ABI v4 stays frozen; the calling-convention fix is additive.** Row 86(A) changes `rt_call`'s
   shape — implemented as a NEW export (`rt_call_addr` + the tail-stage arg form); `rt_call`
   remains exported and correct (native tests keep covering it). No `LUA_RT_ABI` bump, per D9's
   freeze-per-version discipline. Ledger row records the deprecation.
2. **The wasm yield surface is noyield parity, not continuations.** Full resumable yields from
   compiled code need Asyncify/CPS — ruled out of scope at design §7.7/row 22, unchanged. U6f's
   ruling: yields from compiled code raise the catchable `attempt to yield across a C-call
   boundary` (the interp's own row-84g dialect for C-boundary yields — compiled frames ARE the
   C boundary), probe-verified, covering tests, ledger row. `coroutine.lua`/`issues.lua` wasm
   legs stay skipped on row 22 with the narrowed reason.
3. **The GC freeze stays; real rooting is ruled out under zero-patch.** Row 61's aspirational
   "the U6 arena retires the freeze with real rooting" is downgraded: the pool/frame cells live
   in linear memory invisible to stock's collector, and anchoring them requires either patches
   (forbidden) or a parallel GC (a second project — D10). U6b's memlimit becomes the bound on
   frozen-GC growth for untrusted scripts; the freeze is re-ledgered as the durable posture with
   that bound named. `gcparam01` stays row-61-skipped.
4. **Globals protection goes zero-patch or explicitly relaxes.** Primary design (U6c): an
   rt-owned protected-table set + interposition of the stock write path (`luaH_finishset` and
   the pset* fast paths are `LUAI_FUNC` — non-static, cross-object callers, interposable by the
   proven rt_seam55.c link-order mechanism with replicated bodies) raising Redis's
   `Attempt to modify a readonly table`, PLUS the `g_protect_on` backend fast-path gate (inline
   array writes must not bypass it). Fallback if the interposition proves unsound (intra-object
   callers, GC-move hazards): metatable-only protection (`__index`/`__newindex` errors on `_G`)
   + a ledger row naming exactly what stays unprotected (raw writes to existing keys from
   compiled fast paths). The decision is made inside U6c with the probe results in the chunk
   note; either way row 50's v3 patches do NOT come back.
5. **The 5.5 sandbox list**: nil `io`, `os`, `package`, `require`, `dofile`, `loadfile`, `load`,
   `debug`, `warn`. `loadstring`/`module` entries die (never existed in 5.5); `utf8` stays;
   `print` stays (the host event shim); `coroutine` stays (noyield per §4.2 — presence is
   harmless and matches the interp's dev posture). The prod flavor compiles the removed libs out
   entirely (U6d) and `rt_sandbox(1)` returns 0 there — the both-pinned count tests carry over.
6. **e001/e006/e007 residuals stay ruled** (row 60's open tail): the level-0 `error()` prefix
   needs interposing the *static* `luaB_error` — impossible under zero-patch; the nil-object
   dialects likewise. `_cli-tests` kvdb/calc1 xfails stay; no U6 work item.
7. **dump01/load01 (string.dump of backend-compiled functions) stay row-60-ruled**: compiled
   protos are CClosures over the dispatch seam with no rt-side `Proto` — undumpable by
   construction. Not U6 scope; revisit only if a daemon use case appears.
8. **OOM/deadline texts remain pinned absolutely, not diffed** (rows 36/37): the interp oracle
   has no refusable allocator or watchdog error of its own; U6b re-validates the bytes on the
   5.5 raise path but does not reopen the texts.

## 5. U6 exit gate (consolidated)

- `TestWasmFullMatrix` / `TestWasmErrorSuite` / `TestWasmGluaFull` green on the 5.5 corpora ×
  {`interp`, `clua55`, `wasm`, `wazero`, `wazero-prod`} — the clua55 and wazero-prod columns
  added (U6f), zero un-ledgered diffs; var07/vgap01/limits01 in the green set (row 86 closed).
- M6c artifact/sandbox/determinism gates green **on `lua55_prod.wasm`**: import-section gate
  (zero wasi, exact `host.*` set), SHA pin, sandboxed-dev ≡ prod, D5 determinism legs.
- M6d memlimit/deadline gates green on the v4 blob (both hosts); native ABI battery
  (`runtime/tests/run.sh`) ×{plain, ASan, UBSan} on the 5.5 tree with the U6 entries covered.
- `go test ./host/ -race` green (the revived run path on `lua55_prod.wasm`); examples run;
  `CGO_ENABLED=0 go build ./host/`.
- `_cli-tests` ×{wasmtime, WAZERO=1} PASS with life1/tsort1 un-xfailed (kvdb/calc1 remain the
  §4.6 ledgered xfails).
- Every prior gate green: U1/U2/U2f/U3/U4/U5 gates, `TestCLuaConformance`, `TestCLua55Conformance`
  (the oracle blob byte-identical — U6d's no-perturbation pins), selfdiffs, `go test -short ./...`,
  full `go test ./testdiff/ -timeout 45m` clean-cache.
- Docs + ledger + memory updated; `skipRtSurfacesUntilU6` and the host funnel skip deleted.

## 6. Status log

- **U6 planning (2026-10-08)** — ground-truth inventory completed (this document): the v3-only
  rt surface located in `runtime/rt_abi.c` with line refs (§1.1), the ten gated test functions +
  the host `mustCompile` funnel enumerated (§1.4), the row-86 layout mechanics confirmed in
  `rt_abi55.c:891`/`ops.go:497` (§1.3), the 5.5 deltas that reshape the port enumerated (§1.5),
  and the four open design calls decided-as-rulings (§4). U6a started.

- **U6a COMPLETE (2026-10-08)** — rows 86 + 61(d) closed. (B) landed first: `rt_push_frame_from`
  allocates [extras nv][window framecells][1 spare] and returns block+16·nv; the backend's
  `varargCell`/`varargCellDyn`/`varargBase` flip to the below-window base; the M5c restage moves
  inside one `wasm_dispatch_host` call (`rt_disp_frame` + the save/set/adopt/restore bracket —
  the successor re-dispatches at the tailcaller's BLOCK). (A) followed: additive `rt_call_addr`
  (callee by address, args at R(A+1), results to R(A) by the body incl. want=-1) and
  `rt_tail_stage`'s BLOCK-operand form; `emitCall`'s save+shift and both shift loops deleted.
  The relayout surfaced row 61(d)'s REAL defect — not a stale GETTABLE read: C-side TValue
  setters write `tt_` as one byte, so the tag word's upper three bytes are stale garbage on any
  C-written cell, and the backend's 4-byte tag loads broke every ==-tag compare (life1's EQK
  false side). All five tag READS are now byte-wide `i32.load8_u`; stores stay full-width
  (row 45). **Row 92 (found by the gates, fixed in-chunk):** multret call results stage at
  R(A).. unbounded — the hosts malloc the main block at exactly `gFrameCells*16`, so
  limits01's 254-wide `select('#', loaded())` smashed the malloc heap (hang/trap). Every frame
  region now guarantees 4096 headroom cells above the window (`RT_HEADROOM_CELLS` in
  rt_abi55.c's chunk test + `multretHeadroomCells` in gFrameCells — twin constants); the
  >4096 residual is ruled in the row (growable windows are the durable design, U6b candidate).
  Gates: matrix 1500/1500 + error 96/96 + glua-full ×{wasmtime,wazero};
  var07/vgap01/limits01 un-skipped; `_cli-tests` life1/tsort1 un-xfailed (kvdb/calc1 stay on
  the e001 residual); RT-NATIVE55 extended for the layout ×3 sanitizers; prior gates re-run.
  Ledger rows 86 + 61(d) + 92 updated; note/u6a-implemented.md. Next: U6b (budget alloc +
  deadline block on v4).

- **U6b COMPLETE (2026-10-08)** — rows 36/37 re-pinned on ABI v4; **row 93 NEW** (the 5.5
  emergency-GC divergence + the positionless-literal crossing rule). The v3 surface ported:
  `rt_set_memlimit`/`rt_mem_used_bytes`/`rt_alloc` (the budget chokepoint + one-time 64KiB
  emergency slack, re-armed at rt_err_clear/rt_set_state) and the deadline block
  (`rt_ctrl_addr`/`rt_set_deadline`/`rt_deadline_flag`/`rt_deadline`) in rt_abi55.c; the RT
  flavor's `lnewstate` builds with `rt_alloc` (`#ifdef LUA_RT_FLAVOR` — the oracle blob
  preprocessor-identical and verified BYTE-IDENTICAL after the rebuild); build55.sh exports the
  six new entries (rt_alloc not exported, as v3). Backend: `emitDeadlinePoll` (global.get/
  i32.load/if → rt_deadline/return −1) at every proto's flattened dispatch-loop head — the
  v3 block-TRANSITION granularity (calls do not split blocks: pcall-catch + same-block tail
  completes, every back edge re-polls) — plus `lua_dispatch`'s restage loop and
  `rtw_dispatch_body`'s C-side restage loop (ABI-level tail chains); `gDeadline` cached from
  `rt_ctrl_addr()` before the step gate. Engines un-refused: MaxMem → `rt_set_memlimit`
  before lnewstate; Deadline → the watchdog word store (wasmtime `UnsafeData` slice /
  wazero `mem.Write(deadlineFlagLE)`), both the v3 shapes verbatim. **Row 93's find:** 5.5's
  `luaM_realloc_` retries a refusal via `tryagain` → `luaC_fullgc(L,1)`, and `cantryagain`
  is `completestate && !gcstopem` — NOT gcstp-gated — so under the row-61 freeze the
  emergency sweep frees every wasm-cell-held object exactly when rt_alloc refuses. Fix:
  `gcstopem=1` in rt_set_state (durable: cycles never start under GCSTPGC; reallocstack
  save/restores it). Second find (the m6d tailspin gate): v3 kept `err_prefixed` sticky
  across the C-land crossing; v4's rtw_reraise clears there, so the deadline's no-position
  verdict died and the catching rt_run prefixed "chunk:2: " — `stage_tvalue` now recognizes
  the positionless literals by content (seterrorobj's two + the deadline text). Skips:
  `skipRtSurfacesUntilU6` re-scoped to sandbox+prod (m6c/m6cdet keep it), the 7 m6d gates
  un-skipped with the wazero-prod legs on a new `skipProdBlobUntilU6d`. Gates: m6d
  ×{wasmtime,wazero} green (prod skipped); RT-NATIVE55 extended (memlimit + deadline
  batteries; `cpcall55` — 5.5 retired lua_cpcall) ×3 sanitizers; matrix/error/glua +
  U1/U2/U2f + CLua55Conformance over the rebuilt blob; `_cli-tests` ×{wasmtime, WAZERO=1};
  50M-iteration poll spot-check sub-second. Ledger row 93; note/u6b-implemented.md.
  Next: U6c (sandbox surface on v4).
