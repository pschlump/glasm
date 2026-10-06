# U5 implementation plan — the 5.5 language

> Parent plan: `docs/Upgrade-from-5.2-to-5.5-of-Lua.md` §6 (U5 row), decisions D3, D6, D7, D8, D11.
> Same working-breakdown convention as `docs/U4-implementation-plan.md`: independently gated chunks,
> documented deviations, status log. The divergence-ledger discipline applies to every chunk: a
> divergence without a row is a bug; a row without a covering test is a bug.

## 0. What U5 is (from the parent plan)

The 5.5 language surface in the Go interpreter + the wasm backend, gated against the stock-5.5.1
oracle, ending in the program's first **full-suite** milestone:

- **`global` declarations** — `global a`, `global a <const>`, `global a, b`, `global a = expr`,
  `global function f() end`, and the collective `global *` / `global <const> *`; the implicit
  `global *` preamble and its voiding ("all variables must be declared" scopes); read-only
  `<const>` globals; goto-into-scope-of-`'*'`.
- **RDKCTC promotion** — register-less compile-time `<const>` locals (deferred from U4 with the
  GDK kinds; row 76's ruling: one declaration-resolver system).
- **The vararg model rework** — named vararg tables (`function f(a, ...t)`), `OP_VARARGPREP` /
  `OP_GETVARG`, the hidden-extra-args frame layout, `RETURN` C=`numparams+1`, `OP_VARARG` k=1,
  `GETVARG`→`GETTABLE` rewrite under the table model, `debug.getinfo(t).extraargs`;
  `CompatVarArg` and the legacy `arg` table die.
- **Generic-for 4th-value TBC** (deferred from U4; locals.lua:1202 is the gate).
- **`__call` chain cap** (15 replacements; the 16th → `'__call' chain too long`).
- **Light userdata** (the errors.lua un-skip: `debug.upvalueid` light udata + the
  `got light userdata` argument-check text).
- **`collectgarbage("param")`** (minormul/majorminor/minormajor/pause/stepmul/stepsize, get/set).
- **Row-22 yield semantics** — pcall/xpcall continuation across yields, the noyield region
  (`coroutine.isyieldable()` false inside C-library callbacks), and the catchable
  `attempt to yield across a C-call boundary` error.
- **The row-60 error/EH marker seam** — `error(msg, level)` position walks through wasm frames
  (e005 + the kvdb/calc1 xfails); backend + rt work.

**Gate (milestone table):** full official suite **≥95% interp↔clua55** with `vararg.lua`,
`locals.lua`, `goto.lua` fully green; `_cli-tests` goldens regenerated; `glua` CLI banner updated.

**The ≥95% arithmetic** — `clua55Skips` removes 4 driver/host files (main, all, heavy, files),
so 39 files run; 95% of 39 is 37.05 → **at most one file may fail**. Everything else — errors.lua
(light userdata), coroutine.lua (row 22), calls.lua (`__call` chains), code.lua (RDKCTC),
db.lua/api.lua/cstack.lua-class files — must be green. The chunk inventory below is therefore
organized around *gate files*, not just features.

Already delivered ahead of U5 (do NOT redo): read-only for-control variables (U4a), `error(nil)` →
`<no error object>` (U4d), `table.create` (U3d), `utf8.offset` extra return (U3e), 5.5 float
printing (row 70d), `os.exit(code, close)` (U4d).

## 1. Ground-truth inventory (verified 2026-10-01, vendored tree + oracle probes)

All semantics verified by `cmd/clua55-run` probes + reading `runtime/lua55/src/{lparser.c,llex.c,
lcode.c,lvm.c,ltm.c,ldo.c,lbaselib.c,lauxlib.c,lapi.c}`; line references are the vendored tree.

### 1.1 `global` declarations

- **`global` is NOT an unconditional reserved word in the oracle** — `luaconf.h:345` ships
  `LUA_COMPAT_GLOBAL 1` by default ("compatibility mode: 'global' is not a reserved word",
  llex.c:192): `local global = 42; print(global)` → `42` (probe). Reserved-word builds
  (`-DLUA_COMPAT_GLOBAL=0`) exist but our authority is the default build (D6). **Ruling: the fork
  keeps its contextual promotion (lexer promotes `global` only before NAME / `function` / `'<'` /
  `'*'`) — already the shipped posture; parent plan's "reserved word" note is corrected by this
  row.** No corpus churn.
- **The declaration resolver is the preambular algorithm** (searchvar, lparser.c:414-444): every
  function starts in the *implicit* `global *` state — there is no injected actvar entry; the
  state is "no named/preambular declaration seen". Walking active vars innermost-out on each name
  resolution:
  - a **named global decl** entry matching the name → resolved as that global (ro=1 when
    `GDKCONST`);
  - a **collective `global *`** entry (name==NULL, recorded with `u.info >= 0` only if no other
    global decl is active yet in scope) → all names resolve as globals;
  - a **named global decl entry that does NOT match** → *invalidates* the preambular state
    (`u.info -1 → -2`) — from that point undeclared names are errors;
  - no entry at all → global by default (preamble).
  buildvar (lparser.c:452): `var->u.info == -2` → semerror **`variable '%s' not declared`**;
  else buildglobal → `_ENV[name]`; if `_ENV` itself resolves global → semerror
  **`_ENV is global when accessing variable '%s'`**. `GDKCONST` → the VINDEXUP gets
  `ind.ro = 1` → assignment raises `attempt to assign to const variable 'x'` (check_readonly).
- **Declarations are function-scoped actvar entries, block-truncated, NOT inherited by nested
  functions** (searchvar walks only the current fs's active vars; nested functions start with
  their own implicit preamble).
- **Syntax/lowering:**
  - `global a` (no init) → declares `GDKREG`; emits NOTHING.
  - `global a = e` → initglobal: evaluate `e`; read `_ENV["a"]` into a register;
    **`OP_ERRNNIL A Bx`** (raise `global '%s' already defined` when the register is non-nil; name
    from `K[Bx-1]`, `Bx==0` → `"?"` — luaG_errnnil, ldebug.c:817); then store `e` through the
    same buildglobal reference.
  - `global function f() end` → declare `f` GDKREG *first* (recursion in the body sees it), then
    buildglobal, body, then the ERRNNIL check + store (checkglobal/initglobal, lparser.c:1900+).
  - attribs: prefix default + per-name override (the U4a attrib plumbing); `<close>` → the
    existing `global variables cannot be to-be-closed` semerror; unknown attrib → existing error.
- **goto**: `global *` entries count as scoping actvar entries — `goto l` jumping over one into
  its scope raises `"<goto l> at line N jumps into the scope of '*'"` (goto.lua:30 pins the
  wording). The fork's U4c machinery needs only the `'*'` name carried by the entry.
- Gate files: `locals.lua` (:4 `global <const> *`, :45, :161, :185-226 read-only battery,
  :217 `global a, var1 <const>, z`), `goto.lua` (:4-6, :30), `code.lua` (:4), `errors.lua`
  (:532 compile-through-load `global` cases).

### 1.2 RDKCTC promotion (register-less compile-time constants)

localstat tail (lparser.c:1846-1857): when `nvars == nexps && the last var is RDKCONST &&
luaK_exp2const(initializer)` → the var becomes **RDKCTC**: `adjustlocalvars(nvars-1)` +
`fs->nactvar++` — **active but NO register** (it never allocates a stack slot; nvarstack does not
move). References resolve to **VCONST** (searchvar:433) and fold like literals (code.lua's
`local k0 <const> = 0; local k6 <const> = k3 + (k3 << k0)` — code.lua early-returns when `T==nil`
but the whole top block must still *compile*); assignment to one →
`attempt to assign to const variable 'x'` (check_readonly's VCONST arm).
**This forces the fork's nactvar/nvarstack decoupling** — today `varNamePool` entry index IS the
register (`RegisterLocalVar` bumps `regTop`); stock separates the scope count (`fs->nactvar`,
includes register-less RDKCTC/GDK entries) from the register level (`luaY_nvarstack`, what CLOSE
operands and goto scope checks use). The var entries need an explicit register assignment
(allocated at adjust time), with `RegTop` derived rather than inherent.

### 1.3 The vararg model (the deepest interpreter change)

- **parlist reserves ONE slot after the fixed params** for every vararg function
  (`register == numparams`, `numparams` EXCLUDES it) — named (`...t` → the name is an `RDKVAVAR`
  actvar entry) or unnamed (debug name `(vararg table)`). setvararg: `PF_VAHID` +
  **`OP_VARARGPREP` as the first instruction** (funcbody emits it; the fork currently emits a
  no-op marker, compile.go:2112).
- **OP_VARARGPREP → luaT_adjustvarargs** (ltm.c:285+):
  - `PF_VATAB` (named, table model): **createvarargtab** — a fresh table `{1..n, n=n}` stored in
    the param slot; extra args stay on the stack until the first `t[k]` write forces
    materialization (luaK_vapar2local: any *value-use* of `t` → needvatab + VLOCAL).
  - else **buildhiddenargs**: stack layout `func nil..nil extra1..extran func arg1..argn`;
    `ci->func.p += totalargs+1`; the count lives in `ci->u.l.nextraargs`; the param slot holds
    nil.
- **GETVARG** (`t[k]` reads on the virtual table): `luaT_getvararg` — integer key in `1..nextra`
  → the hidden arg; the string `"n"` → nextra; else nil. Writes `t[k]=v` → check_readonly
  (VVARGIND) → needvatab + VINDEXED (materialize the real table first). luaK_finish: `PF_VATAB`
  clears `PF_VAHID`; **`GETVARG` rewrites to `GETTABLE`** when the function uses the table model;
  `OP_VARARG` gets k=1 (append the table as one value) under PF_VATAB.
- **RETURN/TAILCALL get `C = numparams+1`** — the hidden-arg pop signal: the frame teardown
  un-shifts `ci->func.p`. RETURN0/RETURN1 → RETURN when needclose or PF_VAHID.
- `luaT_getvarargs` errors `vararg table has no proper 'n'` (the corrupted-table case).
- **`debug.getinfo(f, 't').extraargs`** — the count of extra args at a level (calls.lua:209
  asserts it).
- **Fork today** (the divergence): `initCallFrame` (_state.go:1077-1155) performs a 5.1-style
  call-time frame shuffle + builds the compat `arg` table (:1136-1149) when `CompatVarArg`;
  `cf.LocalBase += nargs`; OP_GETVARG/OP_ERRNNIL raise "not supported until U5" (_vm.go:1071+);
  `config.go` `var CompatVarArg = true`.
- **`arg` dies**: vararg.lua:16 asserts `arg == _G.arg` — i.e. there is NO local `arg` (both are
  nil). `select('#', ...)`/`table.unpack` etc. all read the extra args; `main`'s script args
  surface is host-side (`cmd/glua` arg handling keeps its own plumbing, no `arg` table).
- Gate file: `vararg.lua` (named `...t` reads `t.n`/`t[i]`, `vararg(... t)` returning `t`,
  shadowing `local arg = {...}`, `self[...]` indexing).

### 1.4 Generic-for 4th-value TBC

- Stock forlist (lparser.c:1716+): 4 internal slots — iterator, state, **closing var (after
  swap)**, control (RDKCONST, the first loop name); `adjust_assign(4, explist)`; **`marktobeclosed`**
  → the loop block gets `upval=1` + `insidetbc=1` (so: OP_CLOSE at loop exit, no tailcall
  promotion inside, needclose RETURN k-bit) — *for every generic-for*, not only ones with a
  4th explist value.
- OP_TFORPREP (lvm.c:1862): swap `ra+3 ↔ ra+2`, then `luaF_newtbcupval(L, ra+2)` (nil/false
  exempt via newtbcupval itself), jump to the TFORCALL.
- **Fork today**: the 4-slot layout and the swap are already in (compile.go:1731-1770, _vm.go:962)
  with a "registration deferred to U5" comment — the delta is the tbc registration in TFORPREP +
  the marktobeclosed block marking (NeedsClose/tbcActive on the loop block) + the backend mirror.
- Gate: locals.lua:1202-1260 (`numopen` counting around to-be-closed generic-for loops).

### 1.5 `__call` chain cap

- tryfuncTM (ldo.c:525+): each `__call` replacement bumps a counter (`MAX_CCMT = 0xf<<8` in the
  CallInfo status); 15 replacements OK, the 16th → luaG_runerror **`'__call' chain too long`**.
- **Fork today**: `metaCall` (_state.go:1067) resolves exactly ONE level (probe: a 2-deep chain →
  `attempt to call a table value`).
- Gate: calls.lua:195-240 — a 15-deep chain returning `table.pack` results, `extraargs` asserts
  through chains, and :226-240 the 16-deep + self-`__call` infinite chain both raising "too long"
  (incl. through a tail call).

### 1.6 Light userdata

- `debug.upvalueid(...)` returns a **light userdata**; `type(x)` → `"userdata"`;
  `debug.setuservalue(x, {})` → **`bad argument #1 to 'debug.setuservalue' (userdata expected,
  got light userdata)`** (probe; lauxlib.c:203 names the type "light userdata" in arg messages).
- Fork: needs a light-userdata kind visible as `"userdata"` but rejected by full-userdata
  argument checks with that exact text; `debug.upvalueid` returns one. (The U4g skip precedent —
  sethook — stays; only the errors.lua surface is in scope.)
- Gate: errors.lua:262-268 — then errors.lua is fully green (its `global` decl cases landed in
  U5a).

### 1.7 `collectgarbage("param")` + the small surface

- lbaselib.c collectgarbage: the `"param"` option (LUA_GCPARAM) with params
  `minormul / majorminor / minormajor / pause / stepmul / stepsize`; returns the OLD value; the
  optional 3rd arg sets a new one. Accepted-and-mostly-no-op on the interp (D10 ruling — ledger
  row) but the protocol is byte-exact.
- Nothing else in the 5.5 lib surface remains open into U5 (checked against Appendix B).

### 1.8 Row-22 yields — the oracle matrix (probed 2026-09-30/10-01)

| shape | stock 5.5.1 | fork today |
|---|---|---|
| plain yields, tail-call yields, wrap-as-iterator | continue (works) | works (coroutine.lua:22-210 green) |
| `pcall(yield)` / `xpcall(yield, g)` | **continues**: resume value(s) become yield's returns; pcall returns normally after the body finishes | yield returns nils immediately; pcall "returns" surgically; wrong resume results (probe: oracle `inside true r1` vs fork `inside true nil`) |
| `table.sort(t, yield)` / `string.gsub` replacement yield / `__tostring` yield | **catchable error `attempt to yield across a C-call boundary`**, thread survives and keeps yielding | gsub completes silently (yield swallowed); sort → `can not yield from outside of a coroutine` + traceback, exit 1 |
| `coroutine.isyieldable()` inside a C-library callback (gsub f) | **false** (noyield region) | true (unmarked) |
| yield in a VM-called `__index` metamethod | continues (luaT_callTMres: `isLuacode(ci)` → yieldable, ltm.c:119-133) | would corrupt (defer — §4.3) |

Stock's mechanism (for translation): C frames with continuations (`luaD_callk`) + `unroll`/`luaV_finishOp`
on resume (ldo.c) — resume re-enters the top frame, and interrupted instructions are *finished*
by an opcode-specific completion function. `attempt to yield across a C-call boundary` fires when
the yield crosses a C frame without a continuation.

**Fork translation (the U5d design):** no goroutines, no luaV_finishOp port —
1. **pcall/xpcall become data-driven protected frames**: the VM's call dispatch special-cases the
   pcall/xpcall builtins → push a marker frame + invoke the argument as an ordinary Lua call;
   error dispatch consults the frame stack for the nearest marker (catch → truncate + rewrite
   results `true, res...` / `false, err` / msgh for xpcall). Because the protection is *data*, a
   yield unwinding the Go stack leaves the frames intact — resume re-enters the loop mid-body and
   the marker applies its semantics when the body finally returns. The Go `ls.PCall` API keeps its
   current implementation (host callers); only the VM CALL path converts.
2. **A noyield depth** on the LState, bumped by C-library callback dispatch (gsub replacements,
   sort comparators, tostring/metamethods from C frames — every place stock calls
   `luaD_callnoyield`/`luaD_protectedparser`-style): `coroutine.isyieldable()` returns false while
   depth>0; a yield attempt at depth>0 raises the catchable
   `attempt to yield across a C-call boundary` (pcall-inside-the-thread catches it, thread
   survives — coroutine.lua:341-351).
3. VM-called metamethod stays **noyield in the fork** (the resume-side `finishOp` per opcode is
   the deferred durable fix — §4.3 ledger row; no official-suite file exercises it).

Gate: **coroutine.lua fully green** (:236-336 pcall-vs-close + nonyieldable battery, :341-351
C-boundary, :354-368 iterator yields, :371-380 xpcall-yield, :383-398 unyieldable-C,
:565-570 msgh-swap).

### 1.9 The row-60 error/EH marker seam

- Of the e-suite's position-bearing cases, **e005 (`error('m',3)` through wasm frames) is the
  only live divergence** (interp `e005.lua:3: m` vs wasm `e005.lua:1: m`); e010/pc02 pass. The
  kvdb/calc1 `_cli-tests` xfails are the same seam. Row 71's wasm-side ERROR-line strip (the
  seam-hint suffix) is superseded by the durable fix landing here (pull forward from U6 — it is
  U5's `error(level)` story; ledger row).
- Mechanism (zero-patch on the 5.5 tree): **link-time interposition** — a glue object defining
  the position-bearing seam symbol(s) placed BEFORE the `$SRC` archive members in build55.sh's
  link line (the libc-symbol preemption precedent; `-Wl,--allow-multiple-definition` if the
  archive wins), backed by an **rt-owned shadow line stack** the backend maintains: each compiled
  function records its current line (a frame cell written at block transitions, already emitted
  for deadline/line purposes) and the trampoline pushes/pops entries; the interposed position
  walk consults the shadow stack first, falling back to stock `luaL_where`-equivalent behavior
  for pure-C frames.
- Gate: e005 byte-exact interp↔wasm; `_cli-tests` kvdb/calc1 un-xfailed; row-71's
  `seamHintPrintSkips` (pc10) retired or re-pointed at the durable fix.

### 1.10 Suite posture (baselined 2026-10-01, interp vs clua55, full official suite; corrected at the U5a closeout)

The original baseline here was WRONG where it claimed pm/sort/nextvar "green already":
TestCLua55Conformance compared the ORACLE to itself (clua55 on wasmtime vs wazero) — the
interp legs had never run against the oracle. The U5a closeout ran them and fixed what
diverged (row 80): **pm.lua and sort.lua are now green** (the string-library/pattern-matcher
and table-library/collectgarbage parity batteries), and nextvar.lua's non-yield surface is
green (ipairsaux wrap-around, basePairs 4-value, RawGetInt hash fallback, package.loaded).
Truly green from the start: attrib.lua, events.lua (U4), and the version-neutral files
(constructs, literals [after the :127 load-chunkname NUL fix, row 80d], math, strings, tpack,
utf8, verybig, big, bitwise, api). NON-U5 triage items discovered by the U5a probe (the plan's
own rule), all row-cited skips in `u5ProbeSkips`: **closure.lua hangs** (:36-42
repeat-until-GC needs weak tables, row 79); **gc.lua** (:31 `collectgarbage("param")` and the
`__mode` batteries — row 79) and **gengc.lua** (:122 — a minor collection must clear a
`__mode="kv"` table, row 79); **db.lua** (:16 debug.gethook — the sethook family, row 77
precedent); **calls.lua** (:168 xpcall(loop,loop) — the row-78e handler-overflow dialect, the
`:195-240` `__call` chains behind it are U5c) and **cstack.lua** (:29 — same row 78e). Failing
with U5-owned causes:

| file | first failure | needs |
|---|---|---|
| vararg.lua | :6 `...t` named varargs | U5b — **the U5b gate** |
| locals.lua | :315 `<close>` function's `...t` battery | U5a green; :309-331 (U5b), :829-853 sethook (U4g skip), :858-1179 yield-in-close (U5d) |
| goto.lua | :197 debug.upvalueid | U5a green; U5c (light userdata) |
| code.lua | — | U5a — **green** |
| errors.lua | :163 `...t` checkmessage | U5b, then :262 light userdata block (U5c) |
| calls.lua | :168 xpcall(loop,loop) | row 78e skip; then :195-240 `__call` chains (U5c) |
| coroutine.lua | :277 yield-inside-pcall (status reads "dead" after the yielding resume) | U5d — row 22, same seam as nextvar :937 |

U5a closeout state: **TestU5ProbeOfficialSuite 18/18 (100% of non-skipped), every skip
row-cited** (rows 22, 63, 77-precedent, 78e, 79 + the four harness-level clua55Skips:
main/all/heavy/files).

## 2. The chunks

| # | Name | Scope | Gate |
|---|---|---|---|
| U5a | Global declarations + RDKCTC + goto `'*'` + generic-for 4th-value TBC | contextual-`global` parity kept; AST `GlobalDeclStmt`; the declaration resolver (searchvar port over the block tree); GDK kinds + ro globals; `OP_ERRNNIL` (compiler+VM); nactvar/register decoupling; RDKCTC promotion + VCONST folding; `'*'` goto scope; marktobeclosed + TFORPREP tbc | **locals.lua, goto.lua, code.lua green interp↔clua55**; `_u5` globals/RDKCTC/tbc-for corpus slice byte-exact; every prior gate green |
| U5b | Vararg model rework | parlist slot + PF_VAHID/PF_VATAB flags; real OP_VARARGPREP (hidden-args layout + vararg table); named `...t` + OP_GETVARG + the GETTABLE rewrite + materialize-on-write; OP_VARARG k=1; RETURN/TAILCALL C=numparams+1 + frame un-shift; `extraargs`; `CompatVarArg`/`arg` death | **vararg.lua green**; `_u5` vararg slice; no `arg` anywhere (corpora audit); all prior gates |
| U5c | Small 5.5 items | `__call` chain cap (15 + text); light userdata + `debug.upvalueid` + the arg-check text; `collectgarbage("param")` | **calls.lua + errors.lua green**; `_u5` additions; all prior gates |
| U5d | Row-22 yields | pcall/xpcall as protected marker frames (continuation across yields); noyield depth + `isyieldable` semantics; `attempt to yield across a C-call boundary`; VM-metamethod noyield ruling (row) | **coroutine.lua green**; `_u5` yield slice incl. C-boundary error cases; all prior gates |
| U5e | wasm backend + rt ABI v4 extensions + row-60 marker seam | OP_VARARGPREP/GETVARG/ERRNNIL lowering; RETURN C-signal + frame layout; TFORPREP tbc mirror; __call-cap parity check (rt side is stock — verify); the interposed position seam + shadow line stack; blob rebuild + native ABI tests | TestWasmFullMatrix/TestWasmErrorSuite/TestWasmGluaFull ×{wasmtime,wazero} green incl. new `_u5` cases; e005 fixed; kvdb/calc1 un-xfailed; row-71 durable fix |
| U5f | Corpora + gates + ledger + closeout | `_u5` corpus complete; `TestU5Gate` + `TestU5OfficialSuite` (≥95%, vararg/locals/goto fully green); `_cli-tests` goldens + glua banner (`_VERSION` Lua 5.5 posture audit); ledger rows; docs + parent status header; memory; delete `u5probe_test.go` | **U5 exit gate** (§5) |

Dependencies: U5a blocks U5b (the parlist/var entries share the resolver + register machinery);
U5c independent of U5b but sequenced after (calls.lua's `extraargs` asserts need U5b); U5d after
U5b (both rework `_state.go` call/frame machinery — one at a time); U5e after a-d (the backend
mirrors the interpreter); U5f last. Each chunk lands green before the next starts.

## 3. Chunk details

### U5a — global declarations + RDKCTC + goto `'*'` + generic-for 4th-value TBC

**Parser** (`parse/parser.go.y`, `ast/`): replace the U3a placeholder `TGlobal` rules (currently
lowering to AssignStmt/DoBlockStmt no-ops) with `ast.GlobalDeclStmt{Name, Attribs, Exprs []Expr}`
and `ast.GlobalFuncStmt{FuncDefStmt}` (or a flag). The lexer's contextual promotion stays
(§1.1 ruling — probe-pinned parity).

**Compiler — the resolver** (`compile.go`): the one structural change this chunk needs first:
varNamePool entries get an explicit register field (`Reg int`, `-1` = register-less) assigned at
adjust time; `RegTop` becomes derived (max register + 1). Keep TWO counts per block/function:
the **active-var count** (scope identity — includes RDKCTC/GDK/`'*'` entries; drives goto scope
checks and entryNactvar) and the **register level** (drives CLOSE operands, upvalue Idx,
LocalBase). Then:
- kinds `varKindGlobalReg` / `varKindGlobalConst` / `varKindGlobalStar` (stock GDKREG/GDKCONST +
  the nameless collective entry); `global *` records a nameless star entry only when no global
  decl is active in scope (stock searchvar's `u.info >= 0` rule);
- `getIdentRefType` gains the searchvar port: innermost-out walk over the current function's
  active entries — named/global matches as in §1.1; no match + a voiding entry in scope →
  semerror `variable '%s' not declared`; else the existing `_ENV` buildglobal path
  (GETTABUP/SETTABUP via envAccess);
- read-only: GDKCONST reads get the ro flag; assignment sites (compileAssignStmt/chained/
  func-name paths — the U4a checkReadOnlyLocal surface) raise
  `attempt to assign to const variable 'x'` from the VINDEXUP/SETTABUP arm;
- `global x = e` lowering: eval `e`; GETTABUP `_ENV["x"]` → temp; `OP_ERRNNIL A Bx` (Bx = K
  index of the name + 1; 0 → `"?"`); store. `global x` emits nothing. `global function f` —
  declare-then-compile-then-check-then-store (§1.1);
- semerror shape stays the U4a `Lexer.SemError` port (no near-token suffix, last consumed line).
- **goto**: the `'*'` entry carries name `"*"` so the scope-jump error reads
  `jumps into the scope of '*'` (goto.lua:30) — the U4c check itself is count-based, unchanged.
- **RDKCTC**: at the end of local-decl compilation, when `nvars == nexps` and the last name is
  `<const>` and the initializer folds (the existing constFold surface), mark the var
  `varKindCTC` — active, register-less, value stored on the entry; references emit the folded
  constant; assignment → the const-variable error. Captured-upvalue interplay verified against
  locals.lua/code.lua during the chunk (stock VCONST → upvalue descriptor carries the constant).

**VM** (`_vm.go`): OP_ERRNNIL — `if R[A] != nil → error "global '%s' already defined"` (name
from K[Bx-1] via the frame's proto constants; Bx==0 → `"?"`). Generic-for: OP_TFORPREP gains
`registerTBC(RA+2)` after the swap (nil/false exempt inside registerTBC); compiler's
compileGenericForStmt marks the loop block NeedsClose+tbcActive (marktobeclosed) — every
generic-for closes at exit / suppresses tail calls / sets the RETURN k-bit path.

**Corpora**: `_u5/glob01..` (declaration forms, voiding, ro-globals, ERRNNIL redefinition,
`'*'` goto, RDKCTC fold/assign-error, tbc-generic-for) interp↔clua55 byte-exact; corpora audit
for `global`-as-identifier (expected clean — comment-only hits).

### U5b — the vararg model rework

**Compiler**: parlist reserves the after-params slot; `FunctionProto` gains `IsNamedVarArg` +
`NumParams` EXCLUDES the slot (audit consumers — backend frame window `nregs+5`, `luawasmc`
artifacts); `...t` declares the name RDKVAVAR (active, in the param slot's register);
funcbody's VARARGPREP marker becomes the real `OP_VARARGPREP A=numparams`; luaK_finish
equivalent at function end: named-table model (PF_VATAB) clears PF_VAHID, rewrites GETVARG→
GETTABLE, sets VARARG k=1; RETURN/TAILCALL get C=numparams+1; RETURN0/1→RETURN under VAHID/needclose.
`luaK_indexed` port: `t[expr]` reads → OP_GETVARG (key exp2anyreg'd, even constants); writes →
materialize (needvatab) + VINDEXED; any whole-value use of `t` → needvatab + VLOCAL (a real table
from VARARGPREP).

**Interpreter** (`_state.go` initCallFrame → the 5.5 layout): NO call-time shuffle — extra args
stay in place; the frame records `nextraargs`; VARARGPREP's table model builds `{1..n,n}` into
the param slot lazily-at-entry (named) or hidden-args (unnamed); doReturn/TAILCALL un-shift
(`C=numparams+1` signal). OP_GETVARG = luaT_getvararg (int 1..nextra / "n" / nil); OP_VARARG
k=1 appends the table. **`CompatVarArg` and the `arg` table are deleted** (config.go, the arg
construction at _state.go:1136-1149, `rt_compat_arg` on the wasm side in U5e); `debug.getinfo`
't' gains `extraargs`. `select`/`table.unpack`/tail-call paths audited against the new frame
shape (the vararg.lua battery is the gate).

### U5c — small 5.5 items

- `metaCall` (_state.go:1067): loop with a 15-replacement cap → `'__call' chain too long`
  (re-checking the metamethod each hop; the counter is per-call, matching MAX_CCMT's per-ci
  reset). calls.lua:195-240 is the gate (15-deep results, 16-deep + infinite, tail-call shape).
- Light userdata: an `LLightUserData`-style leaf (or a light flag on the existing userdata
  kind) whose typename is `userdata`; `debug.upvalueid` returns one; full-userdata argument
  checks (debug.setuservalue first) report `got light userdata` (§1.6 text). errors.lua then
  fully green.
- `collectgarbage("param")`: the 6 params, old-value return, optional set (D10: accepted,
  interp no-op storage — ledger row for the no-op).
- `debug.getinfo` option-filtered fields (deferred from U5b, where §3 first named
  `extraargs`): stock 'u' adds `nparams`/`isvararg`, 't' adds `istailcall`/`extraargs`
  (ldblib.c:178-198) — the fork's getinfo returns one unconditional field set. Part of the
  db.lua/debug surface this chunk owns (no U5b gate observes it — vararg.lua never calls
  getinfo; the db.lua skip covers it until then).

### U5d — row-22 yields

As §1.8's design: (1) pcall/xpcall as protected marker frames in the VM call dispatch + error
dispatch consulting markers (move ls.PCall's catch/fixup logic to the marker; xpcall msgh runs
inside the catch, its own errors falling back to the original object); (2) noyield depth bumped
by gsub/sort/tostring/C-metamethod dispatch (+ anything else stock guards — audit `callGFunction`
callers in the libs); `coroutine.isyieldable` consults it; (3) yield at depth>0 raises the
catchable boundary error. The `TestYieldPastBaseFrame` shape and the row-77d panic half carry
over. `_u5` yield slice: pcall/xpcall continuation values, sort/gsub boundary errors with the
thread surviving, isyieldable inside callbacks, msgh-after-yield (coroutine.lua:565-570).
Ledger row for the VM-metamethod-yield noyield ruling (§4.3).

### U5e — wasm backend + rt ABI v4 extensions + the row-60 marker seam

- Backend (`luawasm/`): OP_VARARGPREP prologue (frame layout under the new model — the frame
  window already sizes `nregs+5`; verify scratch vs extra args, the M5b collision tests re-run),
  OP_GETVARG (rt entry `rt_getvarg` or a table-model lowering), OP_ERRNNIL (rt entry or inline
  vs the staged-error seam), RETURN/TAILCALL C-signal un-shift at the trampoline, TFORPREP
  tbc (rt_tbc after the swap — stock luaF_newtbcupval on the rt side), `global` decls lower
  through GETTABUP/ERRNNIL with no new machinery.
- rt (`rt_abi55.c` + build55.sh): `rt_getvarg`/`rt_errnnil` (or inline equivalents),
  `rt_compat_arg` death, the GETVARG→GETTABLE rewrite means the backend can also emit GETTABLE
  after materialization — mirror the interp's choice; **the marker seam**: interpose the
  position walk (glue object linked before `$SRC`; `-Wl,--allow-multiple-definition` if needed)
  + the rt-owned shadow line stack maintained by the backend's line cell + trampoline push/pop;
  e005/kvdb/calc1 gates; row-71's print-strip skips retire if the durable fix covers pc10.
- Blob rebuild (`runtime/build55.sh`), `go clean -testcache`, native ABI tests extended
  (`runtime/tests/`).

### U5f — corpora + gates + ledger + closeout

`_u5` corpus complete (globals/varargs/call-cap/light-udata/yields/tbc-for); `u5_test.go`
(TestU5Gate over `_u5` + TestU5OfficialSuite: full suite ≥95%, vararg/locals/goto required-green,
the standing 4 driver skips); `_cli-tests` goldens regenerated (`make refs`) + glua CLI banner
(`_VERSION`/usage — 5.5 posture audit, `arg` removal note for the CLI's own argv plumbing);
ledger rows: globals resolver (incl. the contextual-`global` correction to the parent plan),
RDKCTC, vararg model + `arg` death, __call cap, light userdata, collectgarbage("param") no-op
(D10), row-22 continuation + the metamethod-noyield ruling, the marker seam (closes row 60's
position family + row 71's strip), the ≥95% gate row; docs (parent status header, milestone
history, this file's status log); memory; delete `testdiff/u5probe_test.go`.

## 4. Deviations from the milestone table (documented rulings)

1. **`global` stays contextual, not reserved** — the oracle build ships `LUA_COMPAT_GLOBAL 1`
   (luaconf.h:345); `local global = 42` works there (probe). The parent plan's "reserved word"
   wording is corrected by the U5a ledger row. Reserved-word builds exist but are not our
   authority (D6).
2. **Generic-for 4th-value TBC pulled into U5a** (was deferred at U4) — its only gate
   (locals.lua:1202) is a U5-required-green file; the swap machinery already exists.
3. **VM-called metamethods are noyield in the fork** (U5d) — stock resumes them via
   `luaV_finishOp`; the fork raises the C-call-boundary error instead. No official-suite file
   yields from a VM-called metamethod; ledger row; the per-opcode finishOp port is the durable
   follow-up (U7+ candidate).
4. **collectgarbage("param") is protocol-exact but storage-no-op on the interp** (D10 — no GC to
   tune); ledger row.
5. **pcall/xpcall remain Go-callable via ls.PCall for hosts** — only the VM CALL path converts
   to marker frames (U5d); observable behavior identical.
6. **Row-71's print-strip durable fix is pulled forward from U6 into U5e** — it is the same
   seam work as row-60's error positions; retiring the skip rides the same blob rebuild.

## 5. U5 exit gate (consolidated)

- `go test ./testdiff/ -run 'TestU5'` green: TestU5Gate (`_u5`, interp↔clua55 byte-exact) +
  **TestU5OfficialSuite ≥95% with vararg.lua, locals.lua, goto.lua (and code.lua, calls.lua,
  errors.lua, coroutine.lua) green**.
- Every prior gate green: U1/U2/U2f/U3/U4 gates, clua + clua55 conformance, selfdiffs, M5/M6,
  host, `_cli-tests` ×{wasmtime, WAZERO=1} with regenerated goldens.
- TestWasmFullMatrix / TestWasmErrorSuite / TestWasmGluaFull green ×{wasmtime,wazero} over the
  extended corpora (VARARGPREP/GETVARG/ERRNNIL cases on all engines); e005 + kvdb/calc1 fixed.
- `runtime/tests/run.sh` green on the rebuilt blob; native ABI coverage for new rt entries.
- Docs updated (this file's status log, parent plan status header + §6 milestone history, ledger
  rows with covering tests); memory updated; `u5probe_test.go` deleted.

## 6. Status log

- **U5 planning (2026-10-01)** — ground-truth inventory completed (this document); the suite
  posture probe (`u5probe_test.go`) baselined the pre-U5 failure set (§1.10). U5a started.
- **U5a sweep (2026-10-01)** — global decls + RDKCTC + goto `'*'` + generic-for TBC implemented
  (parser rules `ast.GlobalDeclStmt`/`GlobalFuncStmt`, the resolver port `globalDeclState`/
  `checkGlobalAccess`/`checkGlobalWrite`, flat-index/register decoupling `regOfVar`/`nvarstack`/
  `regLevelOfCount` with `kindHasRegister`, RDKCTC promotion in `compileLocalAssignStmt` +
  `ecConst` folding via `ctcValueOf`, `OP_ERRNNIL` compile+VM, `'*'` as a pool entry the U4c
  goto-scope check reads, `marktobeclosed` + TFORPREP `registerTBC`). The stub-driven sweep of
  the three gate files surfaced (and fixed) twelve non-inventory bugs beyond the plan:
  1. register-limit split — MAXVARS 200 (declared) vs MAX_FSTACK 255 (window); goto.lua:411.
  2. metamethod-'close' traceback naming — luaK_finish's implicit-return close rewrite
     (RETURN0→RETURN k=1) + funcnamefromcode's RETURN arm; locals.lua:544.
  3. unconditional `__close` dispatch (no metamethod → error naming the value); locals.lua:580.
  4. error-path caller attribution in the tbc registry close; locals.lua:662-adjacent.
  5. flat-index/register decoupling itself (a `global *` preamble shifted flat indices into
     register operands); locals.lua:68.
  6. chain-visible global declarations (an enclosing function's decl locks nested writes);
     locals.lua:217.
  7. named-global shadowing an outer local (`do global X; X = 20 end`); goto.lua:311.
  8. generic-for two-block close split (FOR-block CLOSE after TFORLOOP, BODY-block per
     iteration); files.lua's io.lines + locals.lua's numopen battery.
  9. `global x <const>` assignment lock via checkGlobalWrite; locals.lua's ro battery.
  10. ERRORSTACKSIZE analog — PCall's errfunc arm unwinds 16 dying frames on a full frame
      stack so the message handler can run (divergence → ledger row: a handler needing more
      than the reserve re-raises "stack overflow" where stock reports "error in error
      handling"; dropped frames invisible to the handler's traceback); locals.lua:662.
  11. EndPc close-window extension — stock removevars runs after OP_CLOSE, so endpc = closepc+1
      and a CLOSE/RETURN's own `__close` still sees the block's locals in debug.getlocal;
      `EndScope(extendToClose)` + the function-root extension; locals.lua:1198 family.
  12. repeat-until: the repetition CLOSE is emitted after LeaveBlock — the block's debug
      windows re-extend past the close dance (stock's scope block leaves after the dance);
      locals.lua:1198.
  13. CTC arithmetic folding — stock's constfolding reads VCONST values through const2val,
      so `local k6 <const> = k3 + (k3 << k0)` promotes to RDKCTC; the fork's ctcFoldValue
      needed the ctcSubst rewrite (code.lua's k-chain is behind the T==nil early-return —
      it compiles but never runs, so the official file could not catch it; `_u5/rdkctc01`
      does).
  14. stock 5.5.1 debug returns — a valid unnamed slot is `(temporary)` (5.4's asterisk is
      gone; `(C temporary)` for C frames — luaG_findlocal), `debug.getlocal` out-of-range
      pushes false (luaL_pushfail), and `debug.getupvalue`/`setupvalue` out-of-range return
      ZERO values (auxupvalue's `return 0`).
  Gate state: code.lua PASS; goto.lua PASS minus :195-227 (U5c debug.upvalueid); locals.lua
  PASS minus :309-331 (U5b named varargs), :829-853 (U4g sethook skip), :858-1179 (U5d
  yield-in-close). `_u5` corpus (glob/rdkctc/gotostar/tbcfor/dbg/ovfl, 6 files) + row 78
  landed, TestU5Gate green; full `go test ./...` verification pending.
- **U5a closeout (2026-10-01)** — triaged the unexpectedly-diverging official-suite files and
  closed U5a. §1.10's "green already" claim was wrong (the conformance gate had compared the
  oracle to itself); the closeout ran the interp legs and FIXED (ledger row 80): **pm.lua**
  (%g class; gsub replacement-value law; the add_s port with stock's `%%`/`%0`/`%1`-`%9`
  texts; digit-bearing capture-index errors; malformed patterns as MATCH-time `opPanic`
  errors with stock texts — ends-with-'%', missing ']', missing `%b` args, missing '[' after
  `%f`, toplevel-')' invalid pattern capture, unfinished capture; the matchbracketclass
  set-range re-port `[a-b-c]`; integer gsub count; original-string identity on zero
  substitutions; the 5.3.3 empty-match discipline); **sort.lua** (collectgarbage's option
  surface: synthetic `gcMemBytes` "count", pushmode's previous-mode tracking, stock texts;
  `table.create` "table overflow" before allocation; table.unpack's castS2U span guard +
  LUAI_MAXSTACK clause; the single luaL_len text; and table.sort as the auxsort port with the
  two `invalid order function for sorting` raises — Go's sort.Slice could not detect invalid
  order functions at all); **nextvar.lua's non-yield surface** (ipairsaux wrap-around +
  1-value nil, basePairs 4-value, RawGetInt hash fallback, package.loaded registry binding);
  **literals.lua:127** (load's default chunkname truncates at the chunk's first NUL).
  Remaining failures all row-cited skips: closure/gc/gengc → row 79; db (:16 gethook) and
  goto (:197 upvalueid) → the sethook/light-userdata families (U5c, row 77 precedent);
  errors (:163)/vararg (:5)/locals (:315) → row 63 (U5b `...t`); calls (:168)/cstack (:29) →
  row 78e; nextvar (:937)/coroutine (:277, yield-inside-pcall leaves the thread "dead") →
  row 22 (U5d). **TestU5ProbeOfficialSuite 18/18 (100% of non-skipped).** En route:
  collectgarbage("generational"/"incremental") now tracks and returns the previous mode.
- **U5b (2026-10-05)** — the vararg model rework landed (ledger row 81). Compiler: parlist
  reserves the after-params slot (`...t` binds varKindVararg at register NumParameters —
  NumParameters excludes the slot; the unnamed "(vararg table)" locvar for parlist
  functions, and NOTHING for the main chunk — stock mainfunc parity, which also keeps
  DbgLocals entry order == register order, the alignment varinfo's localNameAt(reg+1)
  convention needs for `(local 'x')` hints); real OP_VARARGPREP first-instruction (A=0,
  decorative operands — the model comes from the proto's VarArgHidden/VarArgTable flag);
  the luaK_finish pass (whole-value use/capture/write promotes the function to the table
  model, rewrites emitted GETVARGs to GETTABLEs, fixes RETURN C=numparams+1 and
  RETURN0/1→RETURN B=1/2); the luaK_indexed port (t[expr] reads → OP_GETVARG). Interpreter:
  no-shuffle initCallFrame (cf.Base never moves; nil-fill window max(nargs,
  NumUsedRegisters)); VARARGPREP builds hidden (params copied above the extras, LocalBase
  lifts — extras at [LocalBase-nextra, LocalBase)) or table ({1..n, n=INTEGER} into
  register np, no shift); OP_GETVARG (integer/exact-integral-float 1..nextra, "n"→INTEGER
  nextra, else nil); table-arm OP_VARARG validates t.n as a numint in [0, INT_MAX/2] else
  `vararg table has no proper 'n'`; the tailcall dance re-calibrated (the live span to the
  reused frame base is exactly reg.Top()-RA cells). **CompatVarArg and `arg` are dead** —
  what remains is a host surface: every driver (cmd/glua, the testdiff interp engine,
  luawasm55.c) seeds global `arg = {[0]=chunkname}` identically, invisible to gates; the
  corpus clears it before asserting the language surface. En-route bug fixes: OP_VARARG's
  k bit is bit 15 (was decoded from bit 25 — a C=2 single-value use took the table arm on
  hidden frames, vararg.lua:51) and the tailcall count above (vararg.lua:61). Gate state:
  TestU5Gate 15/15 with `_u5/var01–var09` added (named reads, the mutation battery, 'n'
  validation incl. the 2^30 edges, shadowing/capture, tailcalls across the models,
  promotion through returns, the official idioms); vararg.lua byte-identical
  interp↔clua55; `_wasm-tests` var08/16/20 regenerated onto pack-table shapes and
  un-skipped (row 60's rt_compat_arg clause closes — no engine compiles `arg` code);
  TestCLua55Conformance 30/30 on both hosts; TestWasmFullMatrix/ErrorSuite/GluaFull and
  the U1/U2/U2f gates all green; TestU5ProbeOfficialSuite 19/19 (vararg.lua returned to
  the non-skipped set). Row 82 records the pre-existing check_match parser-diagnostics
  family (`')' expected` et al. vs our `unexpected symbol`), surfaced by the var06
  battery — those shapes stay out of the corpora until grammar error productions land.
  Deviations from §3: IsNamedVarArg became the VarArgHidden/VarArgTable proto flags
  (stock's PF_VAHID/PF_VATAB shape); VARARGPREP keeps stock's A=0 (not A=numparams);
  `debug.getinfo 't' extraargs` deferred to U5c (above); the wasm backend still panics on
  OP_GETVARG (row 81's open seam — named-vararg code is interp/clua55-only until the U6
  blob work).
  Late addendum (same day): the boundary probe found two corpus-masking
  bugs — `select('#', ...)` pushed an LNumber where stock pushes an
  INTEGER (baselib.go; invisible because `3 == 3.0` and %.14g prints
  integral floats bare, and var02/var03's count prints sat after a
  staging raise that killed the chunk in both engines), and both files'
  `pcall(select, '#', ...)` batteries were uncatchable by construction
  (the '...' staging raise belongs to the owning frame — the wrap must
  be around the whole vararg function); var02/var03 rewritten
  accordingly (var03's `mk(3)` pins the integer count; var02's
  restored() pins the same-table restore), and the passing-boundary
  capacity texts ("registry overflow" vs "stack overflow") are excluded
  as row 81's e2 clause.
- **U5c (2026-10-05)** — the small-5.5-items chunk landed (ledger row 83). **`__call`
  chain** (`resolveCall`, stock luaD_precall's retry + tryfuncTM): per-hop metamethod
  re-fetch with the link inserted as the new first argument (args shift up — a 5-link chain
  delivers [t5..t1, args...]), MAX_CCMT=15 with the 16th raising `'__call' chain too long'`
  AFTER its insert, counter reset per call site, the type error naming the last value's
  TYPE with the original callee's varinfo, and the chain count riding the frame as
  `callFrame.CCmt` — which IS `getinfo 't' extraargs` (stock's ci MAX_CCMT bits, not the
  vararg count). **pcall/xpcall to stock callee semantics**: luaB_pcall is checkany+plain
  call (no pre-check — the chain runs THROUGH pcall; `pcall(v16)` answers the bare cap
  text), luaB_xpcall checks only arg 2; both ride the **luaG_runerror position rule**
  (`raiseErrorAt(0, ...)`: the raise names the current frame — a C/G caller yields NO
  position, so `pcall(42)` is a bare `attempt to call a number value`; `raiseError(0,...)`
  is wrong, its level>0 gate never prefixes). **Light userdata**
  (`LUserData.Light`): typename "userdata", `debug.upvalueid` produces one, full-userdata
  arg checks say `got light userdata`, base setmetatable refuses while debug.setmetatable
  uses the basic-type table. **Uservalue slots are a capacity** (`UserValues int8`,
  stock newuserdatauv's nuvalue): the io files carry ZERO slots — get answers a single
  nil, in-range answers value+true, writes store the value. **collectgarbage("param")**
  (D10, protocol-exact/storage-no-op): six params as luaO_codeparam bytes,
  applyparam(code,100) old-value return, optional set, checkoption/optinteger texts;
  defaults pinned to the wasm32 readbacks (70→68, stepsize 5600). The corpus caught a real
  port bug here: stock casts the set value through `(int)` inside lua_gc BEFORE the >=0
  guard and the coding — `pause = 2^53` truncates to 0 and stores code 0 (readback 0), 2^31
  wraps negative and reads get-only; the Go port had checked the 64-bit value (saturating
  to 0xFF/396800). **debug.getinfo** (stock db_getinfo): default "flnSrtu",
  option-filtered fields in stock's push order, name→nil when empty, thread-first form,
  both argerrors ('>' first; invalid option — options at arg+2), 'L' as
  collectvalidlines' table or nil for C (a nil *LTable must not land as a typed-nil
  LValue — it printed `<table>`; corpus-caught). **Cross-thread levels over a SUSPENDED
  thread**: stock's continuation model keeps the yielded C function's CallInfo, so level 0
  is that C frame and the thread's frames start at 1; the fork's Go stack unwinds the
  yield's G frame (running/normal-at-resume threads keep a REAL boundary frame), so
  GetStack shifts suspended-thread queries by one and answers level 0 as a synthetic G
  frame; debug.traceback(co) rides the same walk. **Row 78e closure**: a failing message
  handler (any raise or a panic) collapses to exactly `error in error handling` — the
  handler's object discarded (state_test.go's two legacy pins re-pinned);
  cstack.lua:29's handler-overflow dialect stays row 78e's open slice. **Closeout**:
  calls.lua (:195-240, :168) and errors.lua (:258-268) U5c items green — **row 25
  (string.dump) is now the sole blocker for both** (:361/:307); goto.lua fully green and
  un-skipped. Corpus: `_u5/call01, udata01, gcparam01, getinfo01` (positions stripped per
  the clua55 prelude rule). Gates: TestU5Gate 19/19; TestU5ProbeOfficialSuite 20/20
  (100%); TestPCall/TestGlua re-pinned; TestWasmFullMatrix/ErrorSuite/GluaFull,
  TestCLuaConformance, U1/U2/U2f gates all green (interp-side work; the wasm legs run the
  frozen U2e backend); wazero production legs (interp,wazero on _wasm-tests and
  _wasm-err-tests) green. Deviation from §3: the chunk's items landed via resolveCall in
  _state.go (not a metaCall loop edit — same file, same seam) and the errerr fix rode in
  from U5a's PCall structure.
- **U5d (2026-10-05)** — the row-22 yield chunk landed (ledger row 84). **Marker frames as
  data**: `callFrame.Pcall` kinds 1 (pcall) / 2 (xpcall, msgh in PcallHandler) / 3 (pairs'
  `__pairs` continuation); `markerKind` intercepts on the BUILTIN OBJECTS (a replaced
  `pcall` global never intercepts; degenerate arities/non-function handlers stay on the G
  path so row 83's argument texts keep raising stock's), the marker window IS the call
  site's, the body is an ordinary frame above it, a marker-builtin payload nests markers,
  `completePcallMarker` stages prefix+results (pairscont pads to 4 and returns as-is) and
  cascades through the tailcall seam / resume entry; the base-frame arm covers
  `create(pcall)`. **Recovery**: `loopAttempt` completes the innermost in-scope marker at
  the depth that owns it (minIdx scopes each recovery region — gsub callback pcall
  completes inside the callback); `completeErrorMarker` runs msgh AT raise (dying frames
  still visible), the nil-object fix, unwind + closeUpvalues + closeTBCProtected with the
  object in flight, `[false, obj]`. **Two corpus-caught regressions, both fixed**: (1) the
  marker path had LOST the U5a frame reserve — the old G-builtin xpcall ran msgh through
  PCall's errfunc arm (which unwinds errorFrameReserve dying frames on a full stack), the
  marker path's runMsgh PCalls bare, so ovfl01's handlers died of re-raised overflow and
  collapsed to "error in error handling" — completeErrorMarker now carries the reserve;
  (2) a tailcall INTO a marker (`return pcall(f)`) skipped the plain tailcall's window
  slide, so the caller read its stale callee/arg cells ("got function:... function:..."
  instead of "got true 7") — the marker window now CopyRanges down to the reused frame's
  original base. Both shapes were the corpus cases added without a verified probe first —
  the gate caught them (the corpus discipline pays). **noyieldDepth** (callR, callMM's G
  arm, closeTBCVM's C arm): yield inside raises the CATCHABLE boundary error (sort/gsub/
  __tostring callbacks — the thread survives under a pcall), `isyieldable` consults it.
  **Kinds 4/5**: closeTBCVM runs VM-called `__close` as tagged frames (4 = OP_CLOSE,
  ContB = level; 5 = RETURN, ContRA/ContB = the interrupted return's staging) — a Lua
  `__close` yields and doReturn's continuation arm resumes; a C `__close` runs noyield.
  **Kind 6**: `mmYieldable` = stock ltm.c's rule (the metamethod may yield when CALLED FROM
  LUA CODE — the caller's property, not the callee's); all seventeen VM dispatch sites run
  through `callMM`, which pushes the mm frame with contA/contB fold state and suspends the
  owner's instruction; `vmFinishMM` (the luaV_finishOp port — §3's "no finishOp port"
  design note is superseded: the kind-6 arms ARE the port, per-opcode) completes it: MMBIN
  dest from *(savedpc-2), unary/get-family dest = lbase+GETA, order ops condJump on
  LVAsBool(res), SET* no-op. The CONCAT resume: ContRA = the lhs cell, ContB = candidates
  remaining INCLUDING it; the mm result lands AT ContRA (the folded pair collapses into the
  lhs cell) and stringConcat(ContB, ContRA) re-enters the fold — the sync path's
  `rhs=v; total--; i--` invariant; ContB==1 was the final pair. The shared helpers moved
  to tuple signatures (`stringConcat (LValue,uint8)`, `lessThan/leValues/equals (bool,uint8)`,
  `objectRational (int,uint8)`, ...); Go-context callers (sortComp, mathMax/Min,
  lValueArraySorter, the host API) discard the state — always mmSync under noyield.
  **Divergence (row 84g, open)**: a C-FUNCTION metamethod that yields from Lua code
  (`__eq = coroutine.yield`) — stock suspends and finishes via finishOp; the fork raises
  the boundary error and the thread dies (a C frame has no continuation). Ruled out of
  scope; the excluded shapes are inlined in `_u5/coyield05.lua`'s header. **Closeout**:
  nextvar.lua (:937 `__pairs`-yield) green and un-skipped; coroutine.lua green through
  :409 (create(pcall) :277 + the mm-yield battery) — its skip narrows to sethook :410 (the
  db.lua precedent family). Corpus: `_u5/coyield01..06` (marker yields, boundary errors +
  isyieldable + main-thread yield, create/wrap flavors, `__pairs` + yielding iterators,
  the kind-6 battery, kinds 4/5). Gates: TestU5Gate 25/25; TestU5ProbeOfficialSuite 21/21
  (100% of non-skipped); full `go test ./...` green; wasm legs unchanged (interp-side; the
  backend grows the yield surface at U6). Deviation from the §6 table: "VM-metamethod
  noyield ruling" became the kind-6 yieldable model (only C-mms stay noyield — the row-84
  divergence), matching stock's observable behavior for Lua metamethods.
- **U5e (2026-10-05)** — the wasm backend + rt ABI v4 extensions + the row-60 marker seam
  chunk landed (ledger rows 85 + 86). **Stage A ops**: OP_VARARGPREP's table model builds
  the vararg table into R(np) via `rt_varargtab` over the trampoline-staged extras ({1..n}
  plus n as INTEGER); the hidden model is a NO-OP — extras already sit above the wasm
  window and registers are frame-relative, so the interp's param-lift (U5b) is structurally
  absent. OP_GETVARG = `rt_getvarg` (plain like rt_getupval, never raises: integral
  1..nextra, "n" → nextra, else nil); OP_ERRNNIL = `rt_errnnil` (the name from K[Bx−1],
  "?" at Bx=0); OP_VARARG k=1 = `rt_vararg_read(dst, tabcell, nwant, dstcap, line)` —
  t["n"] validated stock's way ("vararg table has no proper 'n'"), values raw-read
  nil-padded, dstcap the frame slack past dst (t.n is user-editable; the interp grows its
  register stack instead); TFORPREP = the swap + `rt_tbc` with a TFORPREP arm in the name
  table. **The position seam (row 60(2) closes)**: `rt_seam55.c` — a glue object linked
  BEFORE the stock `$SRC` with `-Wl,--allow-multiple-definition` (wasm-ld resolves to the
  first definition; the oracle blob never links it) interposing luaL_where / lua_error /
  luaG_runerror / luaG_errormsg / the typeerror family for the rt blob only; the rt-owned
  **shadow line stack** (trampoline push/pop per compiled frame; the line cell at
  frame+16*(nregs+4) with storeLine before every instruction; lua_main plants the bottom
  entry keyed &base_ci) makes error(msg, level)'s walk resolve compiled frames incl. main
  level. e003/e005/e010/e011/pc02 un-skipped byte-exact. **Still ruled**: e001 — level-0
  error() gains the rt prefix (luaB_error's level-0 arm calls no where(); interposing the
  static luaB_error would be needed) — `_cli-tests` kvdb/calc1's wasm legs re-pinned to
  this residual (their error commands are error(msg, 0)); e006/e007 — nil error objects
  (`<no error object>` + prefix vs the interp's "nil"). **The hint registry (row 71's
  durable fix, §4.6)**: the backend precomputes the interp's own varinfo answers per raise
  site at lowering (GetObjName/FuncNameFromCode/UpvalName exported from varinfo.go),
  registers them via `rt_hint_put` in a NO-DEDUP table (concat runs index hbase+idx
  densely), and every raising rt_* entry takes trailing hint indices (rt_arith both
  operand hints + the mm-call hint with the MMBIN k-flip; gettable/settable/len/concat
  object + call-site hints; GETTABUP/SETTABUP's upvalue hint; rt_call's funcnamefromcode).
  DiffLogs' stripVarinfoHints + the seamHintless flag + seamHintPrintSkips are DELETED —
  suffixes compare raw on every leg; pc10 un-skipped. Residual: unattributable call sites
  (kind "") need a run-time register scan — not precomputable, pass −1; no corpus case
  pins one. **RTW_STACKTRAP (−4)**: the engine's own wasm stack can exhaust below
  RTW_MAX_DEPTH (chunk frame sizes vary); the host dispatch maps such traps to −4 and the
  trampoline stages the same bare "stack overflow" the depth guard stages (row 20 family).
  **The msgh seam**: interposed luaG_errormsg = stock + `rt_sea_msgh_ran` set before the
  handler dispatch; rtw_reraise's handler arm runs xpcall handlers for directly-staged
  raises at the C-land crossing. Two ruled divergences: the handler runs after
  rt_frame_restore (the interp's completeErrorMarker runs msgh at the raise, dying frames
  visible — handlers that introspect diverge; ovfl01's are deliberately order-insensitive)
  and the H:-order (interp/clua55 return the handler's result with the position IN the
  message; wasm's re-raise prefix lands AROUND it — the x1 probe; handler-transforms-
  message shapes stay out of the wasm corpora; durable fix rides the U6 seam family).
  **Row 86 (open)**: the extras live ABOVE the register window (stock/interp place them
  below), so legitimate dynamic windows reaching past nregs trample them — vgap01
  (multret VARARG flush tail lands on extra[0]) and var07 (call's arg-shift reads/clobbers
  the saved callee at n≥12) skip the wasm gates on it; durable fix = U6 frame-layout
  surgery (callee-address rt_call + extras-below-window). **Native tests**: the hint
  parameters' arity migration (44 pre-existing call sites), the seam linked natively for
  the first time (the nine interposition replicas macro-renamed — macOS ld has no
  --allow-multiple-definition; the helpers keep real names), and the U5e battery
  (rt_varargtab/getvarg/errnnil/vararg_read incl. the user-edit and overflow arms, plus
  the shadow-stack/flag/rt_hint_put mechanics). **CLI**: ref/err1.w.out regenerated (the
  wasm engine now emits the `(upvalue 't')` hint, matching the interp's row-70f form).
  Note en route: `cmd/testdiff -corpus _glua-tests -engines interp,clua` exits 1 on
  math.lua/table.lua — pre-existing U3b/U3d re-pin artifacts (rows 64/65: the interp
  speaks the 5.5 lib surface, clua is the frozen 5.1 oracle; clua55 is that leg's
  authority and is green; the wasm-family gates run both files byte-exact).
  Gates: TestU5WasmGate/TestU5WazeroGate 13/13 + 13 row-cited skips (u5WasmSkips —
  rows 84/23/61/60/86); TestU5Gate 26/26 unchanged; TestWasmFullMatrix 1500/1500 +
  TestWasmErrorSuite/TestWazeroErrorSuite 96/96 (e001/e006/e007 remain) +
  TestWasmGluaFull/TestWazeroGluaFull (pc10 un-skipped) ×{wasmtime,wazero} on the
  rebuilt blob + `go clean -testcache`; RT-NATIVE55 ×{plain,ASan,UBSan}; `cmd/testdiff`
  legs exit 0 (`_wasm-tests`/`_wasm-err-tests` ×{wasm,wazero}); `_cli-tests`
  ×{wasmtime, WAZERO=1} PASS (4 ledgered xfails: life1/tsort1 row 61, kvdb/calc1 the
  e001 residual); full `go test ./...` green — one operational note: a CLEAN-CACHE run of
  the whole testdiff package no longer fits go-test's default 10m (it got through every
  gate and died to the alarm mid-TestWazeroSpike; the spike itself passes standalone in
  1.2s) — run `go test ./testdiff/ -timeout 45m` for the full suite.
