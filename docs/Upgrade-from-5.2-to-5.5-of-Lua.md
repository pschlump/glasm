# Upgrading from Lua 5.2 to Lua 5.5

> **Status (2026-10-05): U5 IN PROGRESS — U5a + U5b + U5c + U5d + U5e landed. U5a (`global` declarations + RDKCTC + goto `'*'` + generic-for 4th-value TBC, rows 78/80) with the pm/sort/nextvar library-parity closeout; U5b (the vararg model rework, row 81: `...t` named vararg parameters, real `OP_VARARGPREP`/`OP_GETVARG`, the vararg table with its 'n' validation, no-shuffle frames — and `CompatVarArg`/`arg` dead everywhere; vararg.lua byte-identical interp↔clua55, the wasm matrix corpus regenerated off `arg`); U5c (row 83: the `__call` chain with `extraargs`=ccmt, stock pcall/xpcall callee checks + the luaG_runerror position rule, light userdata + uservalue slot counts, `collectgarbage("param")` per D10, `debug.getinfo`'s option filtering incl. the suspended-thread continuation-model levels, the row-78e errerr closure — calls.lua/errors.lua now block only on string.dump (row 25), goto.lua fully green); U5d (row 84: pcall/xpcall/pairs as protected marker frames + the noyield depth + kind-4/5 close continuations + kind-6 yieldable VM metamethods via vmFinishMM — nextvar.lua green, coroutine.lua green through :409; open divergence: a C-function metamethod yielding from Lua code); U5e (row 85 + row 86: the wasm backend's 5.5 surface — Stage A ops VARARGPREP/GETVARG/ERRNNIL/TFORPREP, the row-60 position seam `rt_seam55.c` + the rt-owned shadow line stack, row-71's hint registry retiring the print-strip, RTW_STACKTRAP −4, the msgh seam; TestU5WasmGate/TestU5WazeroGate 13/13 each, TestU5Gate 26/26; open: row 86 extras-above-window layout, the e001 level-0 residual). Remaining: U5f (corpora + gates + closeout); exit gate: the full official suite ≥95% interp↔clua55 (currently 21/21 of non-skipped, every skip row-cited: rows 79/25/84 + the sethook family). Prior: U4 COMPLETE 2026-09-30 (rows 73–77).**
> Companion research artifacts are saved under `note/conv-from-5.2-to-5.5/` (source tarballs,
> extracted manual sections, official 5.5 test suite). This document follows the conventions of
> `Lua-Wasm-Design-and-Test-Plan.md`: design decisions are numbered **D1–D12**, milestones are
> numbered **U0–U8** with hard test gates, and the divergence-ledger discipline applies throughout.
> It supersedes `docs/Lua-5.2-to-5.5.md` (background research note) as the plan of record.
>
> **U0 record (2026-09-23).** `runtime/lua55/` vendors stock 5.5.1 with a one-entry patch
> list (`runtime/lua55/VENDORED.md`: `readable()` rejects the empty filename — POSIX
> `fopen("")` fails with ENOENT everywhere, wasi-libc resolves it to the preopen root and
> succeeds, wrongly "finding" `package.searchpath`'s trailing empty template); everything
> else is build flags. `runtime/luawasm55.c` ports the oracle driver onto the 5.5 public C
> API. Determinism is pinned by flags, not patches: `-D'luai_makeseed()=0u'` for the
> xoshiro/sort-pivot seeds (math.random itself runs stock — math.lua asserts exact xoshiro
> output of seed 1007), `time()`/`clock()` symbol preemption so stock os.time/os.date keep
> their 5.5 validation texts over the pinned 2000-01-01 instant, and
> `lua_newstate(...,42)` — the state seed MUST be non-zero: seed 0 makes `#`'s
> hash_search probe ladder hit pure powers of two, exactly nextvar.lua's "attack on table
> length" key set, so `#t` answers 2^61 and the suite never terminates (seed 0 also
> degraded hashing-heavy files globally — the gate ran 45+ minutes under seed 0 and runs in
> ~4 s under 42). `runtime/build55.sh` builds `lua55_sjlj.wasm` (byte-deterministic; same
> SJLJ/new-EH dialect as the 5.1 blob).
> `testdiff/clua55.go` adds the `clua55` (wasmtime) and `clua55-wazero` (wazero) engines —
> the U0 gate `TestCLua55Conformance` runs `_lua5.5-tests` (official suite) on both hosts in
> the suite's own "user tests" posture (`_port`/`_soft`/`_nomsg` prelude, engine-side, no
> corpus edits) and asserts host equivalence (same blob → byte-identical logs).
> `cmd/clua55-run` is the CLI leg (`GLUA55_ENGINE=wazero` selects the pure-Go host).
> Skips (reasons in the gate): `main.lua`, `all.lua` (subprocess drivers, as in 5.1),
> `heavy.lua` (not in the official all.lua run; exceeds the 256 MiB wasm backstop),
> `files.lua` (the `/dev/full` block asserts io.flush fails with ENOSPC — no WASI host here
> can express a write-failing device; `/dev/null` is served by an engine-provisioned empty
> stand-in file, gitignored under the corpus). All 5.1 gates unchanged and green.
>
> **U1 record (2026-09-23).** The interpreter speaks the 5.2 language core, gated by the new
> `_u1` corpus (`TestU1Gate`: 20 cases, interp ↔ clua55 byte-exact). What landed:
> **_ENV (D3)** — every main proto declares `_ENV` as upvalue 0; the loader seeds the cell
> (load's env argument / thread env); `OP_GETGLOBAL`/`OP_SETGLOBAL` keep their opcode but
> read the frame function's `_ENV` cell, so `_ENV = t` swaps propagate to every closure
> sharing the cell; `local _ENV` shadowing compiles as keyed table ops (GETTABLEKS/SETTABLEKS
> on the local); nested functions capture `_ENV` through the normal OP_CLOSURE machinery
> (`FunctionProto.EnvUpvalueIndex` records the slot). The wasm engines keep the old globals
> lowering, with one rt extension: parentless captures (main runs cl=0) seed a closed
> globals-table upvalue (ledger row 53); shadowing/env-swaps are interp-only until U6.
> **Lexer/parse errors** — `\xXX`, `\z`, `\ddd ≤ 255`, unknown-escape errors, hex floats
> (`0x1p-2`), greedy read_numeral scanning, and the whole error surface re-pinned to stock
> 5.5 texts/shapes: `<chunkid>:<line>: <msg> near '<tok>'`, ChunkID (@/=//[string] sigils —
> engine callers now pass `@case` names), "unexpected symbol"/"syntax error" split,
> `near <eof>` (row 51). `tonumber`'s string path now uses parseNumber (hex floats, "3.").
> **break mid-block** (stat-level TBreak; compileBreakStmt now CLOSEs the outermost crossed
> scope with captured locals — the old code missed nested do/local scopes), **table `__len`**
> (already honored by the VM's metaOp1-first shape), **5.2 libs** — xpcall arg pass-through,
> table.pack/unpack, string.rep separator, `%f` frontier, `file:write` returns the file,
> `*L` read option, os.execute 5.2 triple, package.searchers + searchpath, load/loadfile
> mode+env — and the **D8 removals**: setfenv/getfenv/module/loadstring/base-unpack/newproxy
> gone from the Lua surface (Go API keeps Fn.Env as the loader seed). **Plan correction: `%z`
> stays** — 5.5.1 still implements the deprecated class; the oracle is the authority.
> Re-triage: `_lua5.1-tests` interp legs pruned to sort/strings/files (rows 55, D11);
> `_glua-tests` ported (getinfo nups/getupvalue slots shift by `_ENV`; loadstring→load;
> both-shape os.execute asserts); the fuzzer no longer emits bare `unpack`. Deferred to
> U2 with rulings: error-position attribution (row 52 — corpus discipline: no uncaught
> position-bearing runtime errors in `_u1`; parse errors compare byte-exact), integral-float
> printing ("3" vs "3.0" — cases assert equality instead). All 5.1-era gates green
> (wasm matrices, error suite, glua, clua + clua55 conformance, selfdiffs, M5/M6, host,
> `_cli-tests`); prod-blob pins bumped for the rt change (native ABI tests pass).

---

## 1. Scope, baseline, and target

**Target.** Lua **5.5.1** — the current release of Lua (5.5.0 shipped 2025-12-22; 5.5.1 shipped
2026-08-03; the final 5.4 is 5.4.9, 2026-08-25). Pin the target at 5.5.1 exactly (decision D8).

**Baseline — what the tree implements today.** This fork is Lua **5.1 semantics end-to-end**:

- float64-only numbers (`type LNumber float64`, `config.go:15`; no integer subtype anywhere),
- the 5.1 instruction set (42 opcodes: stock 5.1's 38 plus fork extras `OP_MOVEN`,
  `OP_GETTABLEKS`, `OP_SETTABLEKS`, `OP_NOP`; `opcode.go:39-100`),
- per-function environments with `setfenv`/`getfenv`/`module` (5.1 env model),
- the 5.1 standard-library surface (`loadstring`, `unpack`, `math.pow`, `table.getn`, …),
- vendored stock **C Lua 5.1.5** both as the wasm runtime (`runtime/lua51/` + `rt_abi.c`) and as
  the differential oracle (the `clua` engine).

One deliberate 5.2 feature is **already fully backported**: `goto` / `::labels::`
(lexer keyword, grammar, compiler with scope checking and `OP_CLOSE`-aware lowering,
`compile.go:400-505`). That is why this plan is titled **5.2 → 5.5**: of 5.2, only `goto` is
done; everything else from 5.2 onward is missing. The 5.2 §8 incompatibility list
(`note/conv-from-5.2-to-5.5/sec8-5.2.4.txt`) otherwise applies in full.

**What must move.** All three code-producing pieces move in concert — the Go frontend
(`parse/`, `ast/`, `compile.go`), the Go interpreter (`_vm.go`/`_state.go`, the executable
spec), and the wasm backend (`luawasm/`) — plus the C runtime (both the `rt_*` blob and the
`clua` oracle driver), the differential harness corpora, and the divergence ledger.
The external consumer (the pure-Go Redis-clone daemon) takes a breaking release (D12).

**What does not change.** The pure-Go wasm emitter (`wasm/`), the `host.wasm_dispatch` seam,
the nret convention (`>=0` / `-1` error / `-2` tailcall / `-3` host-refused), the reentrancy
law, the rt-owned upvalue registry, the 16-byte TValue cell layout (stock 5.5 `TValue` is
still 16 bytes on wasm32 — re-assert in `build.sh`), the testdiff event model
(`PRINT`/`ERROR`/`STDOUT`/`GLOBALS` + `normalize.go`), and the `make build` go-inline flow.

---

## 2. Why upgrade — and the cost of doing so

The fork exists to run untrusted client scripts in a pure-Go Redis clone. Real Redis embeds
Lua 5.1 (LuaJIT dialect), so 5.1 was never a *compatibility* requirement — it is inherited
from upstream golua55. Reasons to move to 5.5:

- modern semantics for script authors: 64-bit integers, bitwise operators, `//`,
  `<const>`/`<close>`, `goto`-based control flow, string packing, utf-8 support;
- a living, maintained conformance target: the official 5.5 test suite (43 files) replaces the
  frozen 5.1 suite as the definition of "correct";
- removal of 5.1 legacy burdens this fork currently carries: the `arg` compat table
  (`CompatVarArg`), `setfenv`/`module` plumbing, the `%z` pattern class, etc.

The honest counterweights, recorded here so they are decisions rather than surprises:

- this is a **re-basing of every layer including the executable spec**, comparable in scale to
  the entire M0–M8 wasm program (§7);
- the byte-exact error-text contract — the spine of the differential method — gets re-pinned
  wholesale: stock 5.5 wording becomes canonical and the `rt_set_dialect` gopher-wording
  emulation is retired (D7);
- the f64-only fast paths that were ruled "correct for Lua 5.1" (`Performance-Improvement-Plan.md:252`)
  are superseded: 5.3+ integer semantics are now *in* scope and require dual int/float paths.

---

## 3. Current-state inventory (what will be touched)

### 3.1 Frontend (Go)

| File | LOC | Role | 5.5 gap |
|---|---|---|---|
| `parse/lexer.go` | 549 | scanner | no `\x`/`\z`/`\u{}` escapes, no hex-float literals, no `//` `&` `|` `~` `<<` `>>` tokens, no attributes, `global` not reserved; unknown escapes silently pass through (5.2+ errors); `\ddd` has no ≤255 range check |
| `parse/parser.go.y` | 535 | goyacc grammar (`parse/parser.go` generated, 1385) | no `global` statements, no variable attributes, no named varargs, no bitwise/`//` operators, no `<close>`/`<const>` |
| `ast/` (5 files) | 313 | AST | needs new nodes: `GlobalAssignStmt`, attributes, named-vararg param |
| `compile.go` | 1978 | AST→FunctionProto, label-based backpatch, const folding | globals lower to `OP_GETGLOBAL`/`OP_SETGLOBAL` against a per-function env (no `_ENV`); float-only const folding; `maxRegisters=200`; `OP_CLOSURE` upvalue **pseudo-instructions** encoding (5.3+ puts upvalue descriptors in the proto); `SETLIST` extra-word extension |
| `opcode.go` | 371 | 42-opcode enum + encoding (op in bits 26–31; A 8 bits; B/C 9 bits; Bx 18; sBx bias 131071) | wholesale replacement by the 5.5 format (D2): 7-bit opcode, 8-bit A/B/C + 1-bit k, Bx 17, iAx/isJ 25, plus the `ivABC` variable-width format |

### 3.2 Interpreter (Go, the executable spec)

- `_vm.go` (1049) / `_state.go` (2108) → generated `vm.go` (2465) / `state.go` (2321);
  dispatch via a package-level `jumpTable` of handler closures.
- Value model: `LValue` interface over `LNil/LTrue/LFalse/LString/LNumber(float64)/
  *LTable/*LFunction/*LUserData/*LState/LChannel`; number boxing pool (`alloc.go`);
  integer-ness is only the `isInteger` predicate (`utils.go:133`).
- Arithmetic (`opArith` → `numberArith`, pure float64), comparison (`lessThan`, `equals` with
  the 5.1 same-handler `__eq` rule; `OP_LE` with the 5.1 `not __lt(swapped)` fallback — removed
  in 5.4), concatenation (`stringConcat`), float-accumulating numeric `for` loop.
- Coroutines are same-thread stack switches over `*LState` (no goroutines) — carries forward.
- Errors: Go panics carrying `*ApiError`; `PCall` closes upvalues of dying frames
  (ledger row 44) — the seam where 5.4 `<close>` error-path semantics land.
- GC: none of its own (Go GC); no `__gc`, `__mode`, `__close`, `__name`, `__pairs`, `__ipairs`.
- Number→string dialect: `LNumber.String()` = Go shortest-round-trip (`%.14g`-ish 'g' format),
  mirrored in the C runtime by `runtime/gnumfmt.c` (ledger rows 38/49).

### 3.3 Standard libraries (Go)

| File              | LOC | Registers   | Notable 5.1-isms / 5.5 gaps                                                                                                               |
|-------------------|-----|-------------|-------------------------------------------------------------------------------------------------------------------------------------------|
| `baselib.go`      | 597 | `_G`        | `setfenv`/`getfenv`/`module`/`loadstring`/`unpack` present; `load` is the 5.1 reader-fn form (no `mode`/`env` args); `_VERSION="Lua 5.1"` |
| `stringlib.go`    | 448 | `string`    | no `pack`/`unpack`; `gfind` alias; patterns (pm/, 645 LOC) lack `\0`-in-pattern and the `%f` frontier; `dump` always errors               |
| `mathlib.go`      | 231 | `math`      | 5.1 set incl. `pow log10 mod atan2 frexp ldexp cosh sinh tanh`; no `type/tointeger/maxinteger/mininteger/ult`                             |
| `tablelib.go`     | 100 | `table`     | `getn`/`maxn`; no `move`/`unpack`/`pack`/`create`; ignores metamethods (5.3+ honors `__index`/`__newindex`)                               |
| `oslib.go`        | 235 | `os`        | `execute` returns 0/1 (5.2+ returns bool+info); `setlocale` stub; `setenv` extension                                                      |
| `iolib.go`        | 748 | `io`        | `*n/*a/*l` only (no `l`/`L`/`a` unstarred forms); `io.lines` single return                                                                |
| `debuglib.go`     | 173 | `debug`     | `getfenv`/`setfenv`; no nparams/isvararg info                                                                                             |
| `loadlib.go`      | 128 | `package`   | `loaders` (5.2+: `searchers`); `loadlib` always errors                                                                                    |
| `coroutinelib.go` | 112 | `coroutine` | no `close`, no `isyieldable`                                                                                                              |
| `channellib.go`   | 184 | `channel`   | golua55 extension — keep (D12)                                                                                                            |

### 3.4 C runtime and wasm backend

- `runtime/lua51/` — full stock C Lua 5.1.5 tree (30 `.c` files; the parser/codegen files are
  linked for the `clua` oracle, unused by the `rt_*` path).
- Glue: `rt_abi.c` (1332; implements the ~50-export `rt_*` ABI), `rt_abi.h` (**frozen
  `LUA_RT_ABI=3` and frozen raw 5.1 `TValue` type tags `RT_TNIL..RT_TTHREAD` 0..8** — no
  collectable bit), `rt_wasm.h` (nret convention, dispatch depth 150), `luawasm.c` (718;
  oracle driver + harness shims: print→host.event, deterministic `os.*`, host RNG for
  `math.random`), `gnumfmt.c` (374).
- A 7-file patch list over the vendored tree (Proto.wasm_idx, `precall_wasm` adapter in
  `ldo.c`, dialect branches in `ldebug.c`, etc. — `docs/M5-summary.md` table).
- `build.sh`: wasi-sdk, `-mexec-model=reactor`, wasm-EH-based SJLJ (`lua51_sjlj.wasm`) running
  on both wasmtime and wazero v1.12 (ledger row 32), plus the zero-wasi-import prod flavor
  (`lua51_prod.wasm` + `rt_sandbox`).
- `luawasm/` backend: `backend.go` (379) + `emit.go` (652) + `ops.go` (461). One wasm function
  per proto, signature `(frame, cl, nargs, want) → nret`; registers are TValue cells at
  `frame+16*k`; control flow flattened into a loop + `br_table` over basic blocks; one big
  per-opcode switch; inline f64 fast paths for arith; `OP_CLOSURE` pseudo-instruction handling;
  deadline poll per block. New opcodes land additively as new `case`s; the leader-detection
  switch in `partitionBlocks` must learn each new jump-shaped opcode.

### 3.5 Harness, corpora, ledger

- Engines: `interp` (primary oracle today), `clua` (stock C 5.1.5 blob = semantic authority),
  `wasm`, `wazero`, `wazero-prod`. Gates: `TestWasmFullMatrix` (`_wasm-tests`, 531 cases),
  `TestWasmErrorSuite` (`_wasm-err-tests`, 94), `TestWasmGluaFull` (`_glua-tests`, 10),
  `TestCLuaConformance` (`_lua5.1-tests`, 26, ≥95%), M5/M6 gates, `TestSelfDiff*`.
- `normalize.go`: numbers canonicalized to `%.14g` in event lines; GLOBALS and interp
  traceback tails excluded from diffs; ERROR message heads compared byte-exact — this is what
  forces `rt_set_dialect(1)` today.
- Divergence ledger: 51 rows; the dialect rows (9/25/38/40/49) are directly affected by D7;
  row 44 (pcall upvalue close) interacts with `<close>`; row 42 (±0.0 boxing) with the value
  model; row 31 (savestack rebasing) is re-verified when `ldo.c` is re-patched.
- `skips.go` is the single source of truth; every skip needs a reason string
  (`FilterSkips` panics otherwise). Corpora generators (`TestGenMatrixCorpus`,
  `TestGenM5ErrCorpus`) regenerate `_wasm-tests`/`_wasm-err-tests`.

---

## 4. The delta, version by version

Condensed from the authoritative §8 lists and readmes extracted into
`note/conv-from-5.2-to-5.5/` (`sec8-5.2.4.txt`, `sec8-5.3.6.txt`, `sec8-5.4.9.txt`,
`sec8-5.5.1.txt`, `readme-5.5.txt`). Each item notes where it lands in this repo.

### 4.1 Lua 5.2 (remaining items — `goto` already done)

**Language**
- `_ENV` lexical scheme: only Lua functions have environments; `setfenv`/`getfenv` removed;
  C functions have no environments → *compile.go (globals become an `_ENV` upvalue),
  `_state.go` (env fields, pseudo-indexes), `baselib.go`, `debuglib.go`* (D3).
- New escapes `\xXX`, `\z`; hexadecimal float literals (`0x1p-2`); `\ddd` must be ≤ 255;
  unknown escapes become errors → *`parse/lexer.go`*.
- `break` allowed mid-block; empty statement `;` → *verify current behavior; likely trivial*
  (the grammar already accepts stray `;`).
- `__len` honored for tables → *`_vm.go` OP_LEN / `_state.go` ObjLen*.
- Ephemeron tables (weak keys) → *out of scope with GC (D10), ledger row*.
- Function values may be reused (no observable difference) → *no change needed; note for
  `equals` tests*.

**Libraries** — `module` deprecated; `loadstring`→`load` (string chunks); `math.log10`
deprecated; `table.maxn` deprecated; `os.execute` returns `true|nil,reason`; `unpack`→
`table.unpack`; `%z` deprecated, `\0` allowed in patterns; `package.loaders`→`searchers`;
`load`/`loadfile` gain `mode`; `xpcall(f, err, args...)`; `__pairs` (removed again in 5.4 —
skip); frontier pattern `%f`; `string.rep` separator; `file:write` returns file;
`io.lines` file/options; `io.read` `*L` → *`baselib.go`, `mathlib.go`, `tablelib.go`,
`oslib.go`, `iolib.go`, `loadlib.go`, `pm/`*.
(`__pairs`/`__ipairs` were added in 5.2 but are already gone from 5.3+ — verified against
stock `ltm.c` in 5.3.6/5.4.9/5.5.1 — so they are never added.)

**C API** (affects the vendored tree/glue) — `LUA_GLOBALSINDEX`/`LUA_ENVIRONINDEX` removed;
`lua_objlen`→`lua_rawlen`; `lua_compare`; `lua_load` mode param; `lua_resume` `from` param →
*`luawasm.c` driver port; `auxlib.go` API surface audit*.

### 4.2 Lua 5.3 — the big one

**Language**
- **Integer subtype (64-bit)** — the defining change. Subtype identity is observable:
  `type()` is still "number" but `math.type`, printing (`2.0` vs `2`), table normalization,
  division semantics (`5/2 == 2.5` float, `5//2 == 2` integer), overflow wrap-around,
  shifts, comparisons — all require a real int64 in the value model → *D1; touches
  `value.go`, `alloc.go`, `utils.go` (parseNumber), `table.go` (array keys), `_vm.go`
  (numberArith/lessThan/equals/forloop), `compile.go` (constFold, K pool), every library*.
- Bitwise operators `& | ~ << >>` + unary `~`; integer division `//`; integer modulo sign
  semantics; `for` loop over integers (init/limit/step keep subtype, no float accumulation) →
  *lexer/grammar/`opcode.go`/`_vm.go`/`luawasm/ops.go`/rt ABI*.
- String→number coercion rules change (arithmetic coerces numeric strings; in 5.4 this moves
  into the string library); numeric strings compare numerically; float→string adds `.0`
  (`tostring(2.0) == "2.0"`) → *`utils.go`, `value.go`, `stringlib.go`*.
- Integer division/modulo by zero are errors (`attempt to perform 'n//0'`) → error corpus.

**Libraries** — `string.pack`/`string.unpack`/`string.packsize` (a full binary packing
implementation — new Go code); `utf8` library; `table.move`; `math.type`, `math.tointeger`,
`math.maxinteger/mininteger/ult`; deprecated math functions removed (`atan2 cosh sinh tanh
pow frexp ldexp`); table library respects metamethods; `ipairs` respects metamethods;
`io.read` option names without `*`; `collectgarbage("count")` single result →
*`stringlib.go` (large), new `utf8lib.go`, `mathlib.go`, `tablelib.go`, `iolib.go`*.

**Encoding/VM** — the instruction set was reworked (K-operand arithmetic, jump lists,
upvalue descriptors moved into the proto, no more `OP_SETGLOBAL`). This fork should jump
straight to the 5.5 format at the U2 cutover (D2) rather than pass through 5.3's.

### 4.3 Lua 5.4

**Language** — to-be-closed variables `<close>` + `__close`; `<const>` attributes; string
arithmetic coercion moved into the string library (preserving subtype: `"1" + "2"` is an
integer); decimal integer literals that overflow read as floats; `__le`-from-`__lt` mirroring
removed; integer `for` loop details (no wrap); goto duplicate-label visibility rules; `__gc`
values that are not functions are still called (out of scope with GC, D10).

→ *parser attributes; `OP_TBC`; PCall must close TBC slots on error unwinds (extends the row-44
machinery); `_vm.go` `OP_LE` drops the 5.1 fallback; `stringlib.go` string metatable gains
arith metamethods; error-path coverage in `luawasm/emit.go` + rt ABI.*

**Libraries** — `print` hardwires tostring (`__tostring` still honored); `math.random` =
xoshiro256** with random-ish seed (harness must re-pin determinism — the current host-RNG
shim is replaced by a seeded xoshiro in-process); utf8 surrogate policy; `io.lines` returns
4 values; `coroutine.close`; `warn`; `collectgarbage` incremental/generational options (D10:
accept, mostly no-op); `os.exit(code, close)`.

### 4.4 Lua 5.5

**Language**
- **Declarations for global variables; `global` becomes a reserved word.** Syntax from the
  official grammar (saved: `sec9-grammar-5.5.txt`):
  `global a`, `global a <const>`, `global function f() end`, `global a, b <close>`, and the
  catch-all `global *` / `global <const> *`. Every chunk starts with an implicit
  `global *`; any explicit global declaration voids the implicit one, so inside such a scope
  **all** variables must be declared. Verified against the official suite
  (`locals.lua:4 global <const> *`, `locals.lua:185 global assert<const>, load, string, X`).
  → *lexer (reserved word — breaks scripts using `global` as a name), grammar, AST, and a
  compile-time name resolver layered on the existing goto/block scoping machinery
  (`compile.go` codeBlock); interacts with `_ENV` lowering (D3)*.
- **For-loop control variables are read-only** (compile-time enforcement; `<close>`-style
  error) → *compile.go hidden-local handling + error text*.
- **Named vararg tables**: `function f(a, ...t)` — `t` is a table of the varargs with `t.n`
  (verified: `vararg.lua:6`). New opcodes `OP_VARARGPREP` / `OP_GETVARG`; the 5.5 vararg
  model replaces both this fork's frame shuffle and the legacy `arg` table → *the single
  deepest interpreter change: `_state.go initCallFrame` vararg layout, `OP_VARARG` lowering,
  `rt_compat_arg` retirement, `luawasm/emit.go` prologue + scratch layout (which is
  vararg-sensitive)*.
- `__call` chains capped at 15 objects; `error(nil)` replaces the nil object with a string →
  *`_state.go` metaCall depth guard; `baselib.go` error()*.

**Libraries/other** — `table.create`; `utf8.offset` extra return; `collectgarbage("param")`;
floats printed in decimal with enough digits to round-trip (**shortest-round-trip printing —
converges with this fork's Go-dialect goal, but the exact algorithm is Lua's, so the dialect
decision still needs a call**: D7); more constructor nesting levels.

**C API** — `lua_newstate` third `seed` param (+ `luaL_makeseed`); nresults ≤ 250;
`lua_resetthread`→`lua_closethread`; `lua_dump` changes; `LUA_GCPARAM` → *driver/blob port
(U0) only; the Go side does not consume the C API directly*.

---

## 5. Design decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | **Dual-number value model: `LValue` gains an `LInteger int64` leaf; `LNumber` stays `float64`.** No struct-number, no int-as-float. | Subtype identity is observable in 5.3+ (`math.type`, `tostring(2.0)`, `2^53+1`, table keys, `//`/shift semantics); the current `isInteger` predicate cannot fake it. A new leaf type keeps `LNumber` assertions working for floats and makes the int paths explicit; a `LNumber`→struct change would break every `float64(v)` in the tree at once. Boxing pool (`alloc.go`) gains an int64 pool (same pattern as the existing LNumber pool, row 42's ±0.0 lesson carried over). |
| D2 | **Adopt the stock Lua 5.5 instruction set and encoding in one cutover (U2): 85 opcodes, 7-bit opcode field, A/B/C 8 bits + 1-bit `k`, `Bx` 17, `iAx`/`isJ` 25, `ivABC`. No intermediate fork format.** Fork extras `OP_MOVEN`/`OP_GETTABLEKS`/`OP_SETTABLEKS`/`OP_NOP` are dropped in favor of stock K-operand/field opcodes (re-evaluate as optimizations later under the performance plan). | The encoding is the shared contract of `compile.go`, `_vm.go`, `luawasm/emit.go` and (via `Proto` registration) the C runtime. Migrating it once — directly to the final form — avoids doing it two or three times. Stock encodings keep the door open to optional `luac` interop later (not a goal). The compiler's label-based backpatch survives; only the encoders/decoders and the emitted op families change. |
| D3 | **Globals resolve through an `_ENV` upvalue.** `OP_GETTABUP`/`OP_SETTABUP` replace `OP_GETGLOBAL`/`OP_SETGLOBAL`; per-function `Env`, `setfenv`, `getfenv`, `module`, and the `LUA_ENVIRONINDEX` pseudo-index are removed; `load(chunk, chunkname, mode, env)` sets the chunk's `_ENV`. | The 5.2+ model. It also *simplifies* the fork: the `cf.Fn.Env` plumbing in `_vm.go`/`_state.go` and the 5.0 `CompatVarArg` machinery both go away. The glua CLI and the daemon host set the sandbox globals table through the `env` argument (feeds `rt_sandbox` on the wasm side). |
| D4 | **Keep and extend the existing goto machinery.** Add the 5.4 duplicate-label visibility rule; keep `OP_CLOSE`-before-jump lowering, now also covering TBC slots. | The fork's `gotoLabelDesc` scoping (compile.go:400-505) already implements the hard parts (forward/backward resolution, jump-into-scope-of-local errors). `global` declarations (U5) layer a second resolver onto the same block tree — they must share one scope walk. |
| D5 | **`<const>`/`<close>` supported end-to-end** (parser attributes incl. on `global` decls, `OP_TBC`, `__close` on normal and error unwinds). `PCall` closes TBC slots of dying frames, extending the current row-44 upvalue-close behavior; the wasm engine closes TBC slots on its RT_ERR unwind path. | To-be-closed is the 5.4 feature script authors actually use (`io.lines`-style scoped resources). Error-path fidelity is exactly what the differential error corpus will test. |
| D6 | **Oracle inversion: stock C Lua 5.5.1 becomes the semantic authority from U0.** A new `clua55` engine (stock `runtime/lua55/` + ported `luawasm.c` driver, `lua55_sjlj.wasm` on wasmtime, wazero leg) anchors every U-milestone; the Go interpreter becomes the implementation-under-test; the wasm backend remains the second implementation. | Both current oracles are 5.1 and both change; something stable must hold the semantics while they do. The project's own method — byte-exact differential logs against a stock-C oracle — is reused unchanged, pointed at 5.5.1. |
| D7 | **Error texts and number formatting re-pin to stock 5.5.** The interpreter's error messages are rewritten to stock wording; `rt_set_dialect` and `gnumfmt.c` are retired (or gnumfmt is re-targeted to 5.5's shortest-round-trip formatter if the Go side needs a mirror). `normalize.go`'s `%.14g` `NumRepr` mask stays as the cross-engine normalizer for printed numbers. Ledger rows 9/25/38/40/49 close or rebase on this decision; the row-38 `math.huge` survivor likely closes (stock prints `inf`). | With D6 the direction of emulation flips: instead of C mimicking gopher wording, Go adopts 5.5 wording. This deletes a whole class of dialect maintenance (gnumfmt was 374 LOC of dialect glue) and re-aligns with stock behavior, at the one-time cost of re-pinning every error corpus. Open rows 40 (M7 dialect) and 48 (M8 pow) fold into this re-pin. |
| D8 | **Hard cut, no compatibility options.** No `setfenv`/`getfenv`/`module`/`loadstring`/`unpack`/`arg`/`%z`/`bit32`; `CompatVarArg` deleted; all `LUA_COMPAT*` off in the C build; target pinned to **5.5.1** (revisit at U8 if a 5.5.x lands). | The manual's own advice (test with compat off); compat modes double every test matrix and resurrect the dialect problem D7 deletes. Scripts needing 5.1 stay on the pre-upgrade release. |
| D9 | **New vendored tree `runtime/lua55/` from stock 5.5.1; `rt_abi.c` ports onto it; `LUA_RT_ABI` bumps 3 → 4** with 5.5 `TValue` tags (`LUA_VNUMINT`, collectable-bit variants — the frozen 5.1 `RT_T*` constants are 5.1-specific) and new entries: integer/bitwise arith, `rt_tbc`/TBC-close-on-error, vararg-prep. `runtime/lua51/` is frozen at U0 and deleted at U8. | The ABI is frozen *per version*, not forever; the nret convention, the reentrancy law, the rt-owned upvalue registry, the memlimit/deadline blocks, and the 7-file patch-list approach (re-derived for 5.5 internals — `ldo.c`'s `precall_wasm` adapter is the deepest re-port) all carry forward. The same SJLJ/wasm-EH build continues (5.5 still uses `LUAI_THROW` setjmp — verified stock sources; both wasmtime and wazero v1.12 EH legs stay). |
| D10 | **GC stays out of scope.** Keep "GC stopped per run" for the wasm engine; the interpreter keeps relying on Go's GC. No `__gc`, no weak tables/ephemereron, no generational/incremental parameters — new `collectgarbage`/`warn` options are accepted (and mostly no-op) with ledger rows declaring the divergences. | Unchanged from v1 (M6 arena lifecycle is the durable fix on the wasm side). 5.5's compact arrays and incremental majors are C-internal and invisible to behavior diffs. Finalizers in untrusted scripts are a footgun for the Redis-clone use case anyway. |
| D11 | **Corpora migrate with the milestones.** New `_lua5.5-tests` (official 5.5.1 suite — 43 files, tarball saved) gated by `TestCLua55Conformance` (clua55 self) and, per stage, by interp legs. `_wasm-tests`/`_wasm-err-tests` generators are extended (integers, bitwise, `//`, TBC, globals) and regenerated per stage; `_wasm-err-tests` is re-pinned once at U2 (D7 changes nearly every error text). `_lua5.1-tests`/`_glua-tests` stay attached to the frozen 5.1 engines until U8; portable cases are ported forward. `_cli-tests` goldens regenerate at U5. | Keeps the ledger discipline intact: every intentionally-broken old gate gets a reason string in `skips.go`; every new dialect/limitation divergence gets a row with a covering test. |
| D12 | **The daemon (Redis-clone) takes a breaking host-API release at U8**: `_VERSION` bump, `load` env parameter, `arg`-table removal, new blobs (`lua55_*.wasm`), sandbox re-pin, artifacts format otherwise unchanged. Gopher-lua extensions stay: `channel` library, `string` metatable, `_printregs`. | The host package is the only external consumer; one coordinated break beats a compat shim layer. Extensions are additive and ledgered separately from conformance. |

---

## 6. Milestones

Ordering principle: **oracles first, semantics next, engine parity last.** The backend cannot
lag the frontend after U2 (the encoding cutover forces `emit.go`/`ops.go` to change in
lockstep), so it tracks each stage. Old gates stay green until their milestone intentionally
breaks them; each such break is ledgered.

| U | Deliverable | Gate (tests that must be green) | Est. |
|---|---|---|---|
| U0 | Groundwork: vendor stock `runtime/lua55/`, port `luawasm.c` driver to the 5.5 C API, build `lua55_sjlj.wasm` (wasmtime + wazero legs), add `clua55` engine + `cmd/clua55-run`, add `_lua5.5-tests` corpus | `TestCLua55Conformance` (clua55 self-run ≥95% of the official suite on both engine hosts); all existing 5.1 gates unchanged and green | S |
| U1 | 5.2 language core in Go: `_ENV` (D3), `\x`/`\z` escapes + hex-float literals + escape-error rules, `break` mid-block, table `__len`, `xpcall` args, `load`/`loadfile` `mode`, 5.2 library deltas (§4.1), removals per D8 | new `_u1` corpus: `interp` ↔ `clua55` byte-exact; official-suite subsets that are version-neutral (closures, goto, locals w/o 5.3+ literals) green on `interp`; `_lua5.1-tests` interp legs re-triaged (ledger rows for every intentional break) | M |
| U2 | 5.3 integers + **encoding cutover** (D1 + D2): dual-number value model, integer/bitwise/`//` semantics, subtype-aware compare/coerce/format, integer `for`; `opcode.go`/`compile.go`/`_vm.go`/`luawasm/*` move to the 85-opcode 5.5 format; `rt_abi.c` port begins (ABI v4: tags + integer `rt_arith`) | `_wasm-tests` regenerated with int/float/bitwise matrix — green on `interp`, `clua55`, `wasm`, `wazero`; `_wasm-err-tests` re-pinned (D7 texts); official `literals.lua`, `math.lua` (int parts), `bwcoercion.lua` green on `interp`↔`clua55` | XL |
| U3 | 5.3/5.4 stdlib: `string.pack`/`unpack`/`packsize` (new Go implementation), `utf8` library, `table.move`, `math.type` family + removals, string-metatable arithmetic (5.4 coercion model), hardwired `print`, `io.lines` 4 results, `io.read` unstarred forms, xoshiro `math.random` with harness seeding, `coroutine.close`, `warn` stub | official `strings.lua`, `tpack.lua`, `utf8.lua`, `math.lua`, `bwcoercion.lua` green `interp`↔`clua55`; stdlib corpus regenerated for wasm legs | L |
| U4 | 5.4 language: `<const>`/`<close>` attributes, `OP_TBC`, `__close` incl. error-path closes (PCall row-44 extension; wasm RT_ERR path), duplicate-label rules, `__le` mirror removal, `os.exit(code, close)` | official `attrib.lua`, `events.lua`, `errors.lua`, `coroutine.lua` green `interp`↔`clua55`; TBC-in-error-path cases added to `_wasm-err-tests` (all engines) | L |
| U5 | 5.5 language: `global` declarations + reserved word + read-only `<const>` globals + implicit `global *` voiding; read-only `for` variables; **named varargs + `OP_VARARGPREP`/`OP_GETVARG` + vararg-model rework** (delete `CompatVarArg`/`rt_compat_arg`); `__call` chain cap; `error(nil)`→string; `table.create`; `utf8.offset` extra return; `collectgarbage("param")`; 5.5 float printing (D7) | full official suite ≥95% on `interp`↔`clua55` (`vararg.lua`, `locals.lua`, `goto.lua` fully green); `_cli-tests` goldens regenerated; `glua` CLI banner/behavior updated | L |
| U6 | Runtime cutover complete: `rt_abi.c` fully on `lua55/` (TBC + vararg entries done), prod blob + `rt_sandbox` re-pin on 5.5 globals, memlimit/deadline re-validation, every wasm/wazero gate running 5.5 corpora | `TestWasmFullMatrix`/`TestWasmErrorSuite`/`TestWasmGluaFull` on 5.5 corpora × {`interp`,`clua55`,`wasm`,`wazero`,`wazero-prod`}; M6c artifact/sandbox/determinism gates re-run on `lua55_prod.wasm` | L |
| U7 | Soak + performance: extend `cmd/luafuzz` generators to 5.5 constructs (ints, bitwise, TBC, globals, named varargs); fresh-clock soak (bin/run-fuzzer.sh); perf re-baseline; re-introduce fork fast paths where stock encoding allows (int/f64 dual inline arith, K-operand coverage) | N-night soak with zero new-clock findings; perf regression report vs 5.1 baseline accepted; determinism gates green | M |
| U8 | Closure: ledger migration audit (all 51 old rows + U-rows: keep/rebase/close, each with covering tests), docs updates (design doc §A×U cross-refs, CLAUDE.md commands/oracles, this file's status blocks), delete `runtime/lua51/` + `clua` engine + retired corpora, daemon-repo breaking release (D12) | zero ledger rows without tests; docs review; daemon host package compiles and passes its own suite against `lua55_*.wasm` | S–M |

**Dependency notes.** U2 is the critical path and the only step where everything changes at
once; it is deliberately scheduled after U1 so the compiler's name resolution (`_ENV`) is
already stable when the encoding flips. U5's vararg rework is the riskiest interpreter change
(frame layout is what the wasm backend's scratch addressing depends on — M5b collision fix)
and is scheduled after the encoding and TBC work so it can move in isolation.

---

## 7. Effort and size

Rough, assuming the M0–M8 cadence (one developer, milestone-gated, heavy differential
testing). Uncertainty is dominated by U2 (value-model blast radius) and U5 (vararg model).

| Milestone | Est. (person-weeks)                  |
|-----------|--------------------------------------|
| U0        | 1–2                                  |
| U1        | 2–4                                  |
| U2        | 4–8                                  |
| U3        | 2–5 (`string.pack` is the long pole) |
| U4        | 2–4                                  |
| U5        | 3–5                                  |
| U6        | 3–6                                  |
| U7        | 2–3                                  |
| U8        | 1–2                                  |
| **Total** | **20–39 person-weeks**               |

For calibration: this is the same order of magnitude as the entire M0–M8 wasm program, and
larger than any single past milestone, because it re-bases the executable spec itself rather
than adding a consumer of it. Approximate code churn: `compile.go`+`opcode.go` largely
rewritten (~2.3k LOC), `_vm.go`/`_state.go` heavily reworked (~3.2k), `rt_abi.c` re-ported
(~1.3k), `luawasm/emit.go`+`ops.go` substantially rewritten (~1.1k), ~1.5k LOC of new stdlib
(`string.pack`, `utf8`, additions), plus corpus regeneration.

---

## 8. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Error-text re-pinning churn (D7) — hundreds of texts change once, and they are the project's byte-exact spine | clua55 authority exists from U0; `_wasm-err-tests` regenerated mechanically from clua55 output; dialect rows closed in one audited pass (U2) |
| D1 value-model blast radius (every `LNumber` consumer) | mechanical inventory exists (§3.2/§4.2); boxing pool pattern reused; official `math.lua`/`literals.lua` as the gate |
| Oracle driver port (`luawasm.c` on the 5.5 C API; `ldo.c` `precall_wasm` adapter onto 5.5's call machinery) | U0 does the driver port on stock 5.5 before any rt work; adapter port isolated in U2/U6 with the existing native `runtime/tests/run.sh` ABI harness extended |
| Vararg model rework destabilizing the wasm frame layout (M5b-class bugs) | U5 isolated after encoding+TBC land; dedicated generated vararg matrix; the existing scratch-address collision tests re-run |
| wazero/EH regression on the 5.5 blob | 5.5 core still setjmp/`LUAI_THROW`-based; U0 builds `lua55_sjlj.wasm` and runs it on **both** wasmtime and wazero as its gate |
| Ledger explosion during transition (old divergences vs new semantics) | policy: pre-upgrade rows are frozen at their milestone; each is explicitly kept/rebased/closed at U8 with a covering test — never silently invalidated |
| Perf regression from dropping fork opcodes (`MOVEN`, `GETTABLEKS`) at D2 | U7 re-baseline; stock K-operand/field opcodes cover most of the win; re-add fork peepholes only where the plan's profiler justifies |
| `global`-as-reserved-word breaks real scripts | hard cut is the decision (D8); release-notes callout; `_cli-tests`/`_glua-tests` audit at U5 |
| 5.5.x drift after 5.5.1 | pin 5.5.1; revisit at U8 (manual was last updated 2026-07; tarballs saved locally) |

---

## 9. Open questions

1. **Pin** — lock 5.5.1 (recommended; sources and tests saved in `note/conv-from-5.2-to-5.5/`)
   or track the 5.5.x series?
2. **Scheduling vs open rows** — fold open ledger rows 40 (M7 dialect) and 48 (M8 pow) into
   the D7 re-pin (recommended), or close them first under 5.1?
3. **5.1 escape hatch for the daemon** — is there any product need for a 5.1-dialect script
   mode in the Redis clone (real-Redis `SCRIPT LOAD` compatibility)? Assumed no (D8/D12).
4. **Extensions** — keep `channel`, `setenv`, `_printregs` as-is? Assumed keep (D12).
5. **Bytecode interop** — adopt stock `luac` dump format for artifacts later? Not a goal; D2
   keeps it possible. (Current artifacts are wasm modules, not Lua bytecode — unchanged.)
6. **GC horizon** — does the daemon ever need `__gc`/weak tables? If yes, that is a second
   project on top of D10 (arena lifecycle first).

---

## Appendix A — Instruction-set cross-reference (current 42 → 5.5's 85)

Current fork opcode set (`opcode.go:39-100`; stock-5.1 names plus four fork extras):
`MOVE MOVEN LOADK LOADBOOL LOADNIL GETUPVAL GETGLOBAL GETTABLE GETTABLEKS SETGLOBAL SETUPVAL
SETTABLE SETTABLEKS NEWTABLE SELF ADD SUB MUL DIV MOD POW UNM NOT LEN CONCAT JMP EQ LT LE
TEST TESTSET CALL TAILCALL RETURN FORLOOP FORPREP TFORLOOP SETLIST CLOSE CLOSURE VARARG NOP`.

Lua 5.5 set (from stock `lopcodes.h`, 85 total): `MOVE LOADI LOADF LOADK LOADKX LOADFALSE
LFALSESKIP LOADTRUE LOADNIL GETUPVAL SETUPVAL GETTABUP GETTABLE GETI GETFIELD SETTABUP
SETTABLE SETI SETFIELD NEWTABLE SELF ADDI ADDK SUBK MULK MODK POWK DIVK IDIVK BANDK BORK
BXORK SHLI SHRI ADD SUB MUL MOD POW DIV IDIV BAND BOR BXOR SHL SHR MMBIN MMBINI MMBINK UNM
BNOT NOT LEN CONCAT CLOSE TBC JMP EQ LT LE EQK EQI LTI LEI GTI GEI TEST TESTSET CALL
TAILCALL RETURN RETURN0 RETURN1 FORLOOP FORPREP TFORPREP TFORCALL TFORLOOP SETLIST CLOSURE
VARARG GETVARG ERRNNIL VARARGPREP EXTRAARG`.

Family mapping (what the compiler must learn):

| Family | 5.1 (fork) | 5.5 | Notes |
|---|---|---|---|
| Globals | `GETGLOBAL`/`SETGLOBAL` | `GETTABUP`/`SETTABUP` (+`SETFIELD` k) | via the `_ENV` upvalue (D3) |
| Loads | `LOADK`, `LOADBOOL`, `LOADNIL` | `LOADI`/`LOADF` (sBx immediates), `LOADK`/`LOADKX`+`EXTRAARG`, `LOADFALSE`/`LOADTRUE`/`LFALSESKIP` | integers/floats as immediates; `LFALSESKIP` replaces the `LOADBOOL` trick in conditionals |
| Arith | `ADD..POW` (RK operands) | register ops + `ADDI`/`SHLI`/`SHRI` immediates + `*K` const ops + `IDIV`/`BAND`/`BOR`/`BXOR`/`SHL`/`SHR` + `MMBIN`/`MMBINI`/`MMBINK` fallback slots | metamethod fallback becomes an explicit following opcode instead of an rt slow path |
| Compare/test | `EQ LT LE`, `TEST`, `TESTSET` | `EQ EQK EQI LT LE LTI LEI GTI GEI TEST TESTSET` | immediate-compare forms for constant folding into branches |
| For loops | `FORPREP/FORLOOP`, `TFORLOOP` | `FORPREP/FORLOOP` (integer/float flavors via flags), `TFORPREP`(+TBC)/`TFORCALL`/`TFORLOOP` | 5.4+ closes the iteration variable when needed |
| Calls/return | `CALL`, `TAILCALL`, `RETURN` | + `RETURN0`, `RETURN1` | common-case fast returns |
| Blocks | `CLOSE` | `CLOSE` (upvalues+TBC range), `TBC` | D5 |
| Vararg | `VARARG` (+ fork frame shuffle) | `VARARGPREP`, `VARARG`, `GETVARG`, `ERRNNIL` | U5; also removes `rt_compat_arg` |
| Closures | `CLOSURE` + upvalue pseudo-instructions | `CLOSURE` (descriptors live in `FunctionProto.Upvalues`) | simplifies the backend (`pseudo map` deleted) |
| Table | `NEWTABLE`, `SETLIST` (extra-word extension) | `NEWTABLE`, `SETLIST` (k-flag + `EXTRAARG`), `GETI`/`GETFIELD`/`SETI`/`SETFIELD` | fork's `GETTABLEKS`/`SETTABLEKS` covered by `GETFIELD`/`SETFIELD`; fork's extra-word `SETLIST` encoding dies |
| Fork extras | `MOVEN` (peephole), `NOP` (patch filler) | — | dropped at D2; re-derive as optimizations in U7 |

## Appendix B — Standard-library cross-reference

| Library | Remove (5.1-isms) | Add (by 5.5) | Keep-as-extension |
|---|---|---|---|
| base | `setfenv` `getfenv` `module` `loadstring` `unpack` `newproxy`(hidden) | `load(chunk,chunkname,mode,env)` semantics; `warn`; 5.4 `print` hardwiring; `error(nil)`→string (5.5) | `_printregs` |
| string | `gfind` | `pack` `unpack` `packsize`; `%f` frontier; `\0` in patterns; `gmatch` init arg (5.4); string-mt arithmetic metamethods (5.4 coercion); `rep` separator; `format` `%a`/`%p` audit | — |
| math | `pow` `log10` `mod` `atan2` `frexp` `ldexp` `cosh` `sinh` `tanh` | `type` `tointeger` `maxinteger` `mininteger` `ult`; xoshiro256** `random`; integer-exact `fmod`/`floor`/`ceil` | — |
| table | `getn` `maxn` | `unpack` `pack` (5.2), `move` (5.3), `create` (5.5) — all four verified in stock 5.5.1 `ltablib.c`; metamethod-honoring reads/writes | — |
| os | — | `execute` bool+info return; `exit(code, close)`; `date`/`time` integer semantics | `setenv` |
| io | — | unstarred read options; `l`/`L`; `io.lines` 4 returns; `file:read` "n" integer/float distinction | — |
| coroutine | — | `close`; `isyieldable` | — |
| debug | `getfenv`/`setfenv` | `getinfo` nparams/isvararg fields; 5.2+ hook-event model (`tail call`) | — |
| package | `loaders` | `searchers`; `searchpath` | — |
| utf8 (new) | — | full library (5.3) + 5.5 `offset` extra return; 5.4 surrogate policy | — |
| bit32 | never had | **not adding** (deprecated in 5.3, dead after) | — |
| channel | — | — | keep as golua55 extension (D12) |

## Appendix C — Source material

Everything below is saved under `note/conv-from-5.2-to-5.5/` (see its `README.md`):

- `lua-5.5.1.tar.gz`, `lua-5.4.9.tar.gz`, `lua-5.3.6.tar.gz`, `lua-5.2.4.tar.gz` — official
  source releases (reference implementations; §8 of each manual is the incompatibility
  authority).
- `lua55-tests.tar.gz` — official Lua 5.5.1 test suite (43 entries; the `_lua5.5-tests`
  corpus source for D11). Mirrored at lua.org/tests.
- `sec8-{5.2.4,5.3.6,5.4.9,5.5.1}.txt` — extracted manual §8 (incompatibilities), verbatim.
- `sec9-grammar-5.5.txt` — complete 5.5 EBNF grammar (global declarations, attributes,
  named varargs, `//` and bitwise operators).
- `readme-5.5.txt` — official 5.5 "changes since 5.4" list.
- `feat-5.5-globals-varargs.txt` — manual excerpt on the implicit `global *` preamble rule.

Web sources: lua.org/versions.html (version chronology), lua.org/manual/5.5/ (readme +
manual §8), lua.org/tests/ (official suites). Prior art surveyed: no maintained
golua55-family implementation of 5.4/5.5 exists in Go today (closest: Lua 5.4 VM
experiments); this fork would be the migration path.
