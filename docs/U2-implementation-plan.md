# U2 implementation plan — 5.3 integers + encoding cutover (D1 + D2)

> Parent plan: `docs/Upgrade-from-5.2-to-5.5-of-Lua.md` §6 (U2 row), decisions D1, D2, D6, D7,
> D11. This document is the working breakdown of U2 into independently-gated chunks. It records
> scope, file-level inventory, the chunk boundaries **and the reasons for them**, deviations from
> the milestone table (with rulings), and a status log. The divergence-ledger discipline applies
> to every chunk: a divergence without a row is a bug; a row without a covering test is a bug.

## 0. What U2 is (from the parent plan)

5.3 integers + encoding cutover:

- **D1** — dual-number value model: `LValue` gains an `LInteger int64` leaf; `LNumber` stays
  `float64`. Integer subtype identity is observable end-to-end (`tostring`, table keys, `//`,
  shifts, comparisons, for-loop flavor, wrap-around).
- **D2** — one cutover to the stock Lua **5.5.1** instruction set/encoding: 85 opcodes, 7-bit
  opcode, A/B/C 8 bits + 1-bit `k`, `Bx` 17, `iAx`/`isJ` 25, `ivABC` (vB 6 + vC 10 — used only
  by `NEWTABLE`/`SETLIST`). Fork extras `OP_MOVEN`/`OP_GETTABLEKS`/`OP_SETTABLEKS`/`OP_NOP` die.
- Gate (milestone table): `_wasm-tests` regenerated with int/float/bitwise matrix green on
  `interp`, `clua55`, `wasm`, `wazero`; `_wasm-err-tests` re-pinned (D7 texts); official
  `literals.lua`, `math.lua` (int parts), `bwcoercion.lua` green on `interp`↔`clua55`.
- Effort estimate: 4–8 person-weeks (the largest milestone; this chunk plan exists because a
  single landing is not reviewable).

## 1. Chunking principle

**Semantics first, encoding second, engine parity last** (the parent plan's own ordering
principle, applied inside U2):

1. The old 42-opcode fork encoding has **22 free opcode slots** (6-bit op field, 42 used) —
   enough for the seven new operators (`IDIV BAND BOR BXOR SHL SHR BNOT`) as plain RK-operand
   opcodes. So the full 5.3 *semantics* can land and be gated against `clua55` **before** the
   encoding flips. The value model is the riskiest part of U2 (parent plan §8); it must be
   testable without simultaneously rewriting the compiler, VM dispatch, wasm backend, and C ABI.
2. The encoding cutover (U2c) is then *behavior-preserving by construction*: its gate is "every
   corpus that was green is still green, byte-for-byte". The oracle never sees the encoding.
3. The wasm engines and the C rt must move in lockstep with nothing observable between them, so
   they form the final chunk pair.

## 2. Ground-truth inventory (verified against the tree, 2026-09-24)

- **Encoding consumers outside the VM/luawasm: only `compile.go` + `function.go`.** No binary
  chunk reader/writer exists (`parse/` is lexer+parser only; `string.dump` raises; `load` mode
  "b" always rejects). There are no golden bytecode dumps. The encoding contract lives in
  `opcode.go` (producer side: `compile.go` emitters + `PropagateKMV/MV` peepholes + `patchCode`
  1951–2027; consumer side: `function.go` `str()` dump, cosmetic only).
- **`luawasm/` re-implements the decode inline ~15 sites** (not via package `lua` accessors):
  `emit.go:424-428` (the main decode), `:364` jumpTarget, `:327/:331/:440-441` LOADBOOL/MOVEN,
  `:315/:321/:373-374` partitionBlocks peeks, `backend.go:268-283` CLOSURE-pseudo scan; RK
  discrimination `emit.go:106-119`, `ops.go:17`. The rewrite should make `lua` export the
  decoder and delete the duplication (recorded as a U2c task).
- **rt ABI v3** (`runtime/rt_abi.c`, 1439 lines): arith/forprep semantics are **hand-rolled in
  the glue** (5.1 has no `luaV_arith`), so integers land there; op codes passed over the ABI are
  the raw 5.1 opcode numbers frozen in `rt_abi.h:64-72` (`RT_OP_ADD=15..RT_OP_UNM=21`); error
  texts are split between `rt_abi.c` dialect branches and ~6 patched files of `runtime/lua51/`;
  the wasm cells ARE 5.1 `TValue`s (tag at byte 8, f64 at 0) shared with the C heap.
- **Upvalue capture info is pseudo-instructions after `OP_CLOSURE`** (producer
  `compile.go:1374-1393`; five consumers in the VM/backend/patchCode). 5.5 puts descriptors in
  `Proto.upvalues[]` (`Upvaldesc{instack,idx,kind}`); the cutover deletes all five sites.
- **`parse/` regen**: `parse/Makefile` runs `goyacc` from PATH (`~/go/bin/goyacc` is installed);
  the root Makefile has no yacc rule — `cd parse && make` after editing `parser.go.y`.
- **5.5.1 reference facts** (vendored `runtime/lua55/src/`): float→string = `%.15g`, re-read,
  escalate to `%.17g`, append `.0` when the result looks like an integer (`lobject.c:427-443`;
  `luaconf.h:452-453`) — this is **U5 work (D7)**, not U2; `tonumber(s, base)` returns an
  integer (`lbaselib.c`); string arithmetic coercion goes through the **string metatable**
  (`lstrlib.c` `stringmetamethods`, subtype-preserving) — the 5.4 model, scheduled at **U3**;
  mixed int/float order comparisons use the `l_intfitsf` (2^53) + floor/ceil algorithm
  (`lvm.c:455-543`); shifts: `y<0` shifts the other way, `|y|>=64` ⇒ 0 (`luaV_shiftl`).
- **testdiff**: `NumRepr` = `%.14g` mask applies only to values still numeric at the print
  boundary — **strings are never masked**, so `tostring` dialect is visible in PRINT lines
  (corpus discipline continues to avoid printing integral floats directly until U5);
  `skips.go` keys on corpus dir basename; gates hardcode corpus paths (`u1_test.go` pattern).

## 3. The chunks

| # | Name | Scope | Gate |
|---|---|---|---|
| U2a | Integer value model | D1 leaf, boxing, `parseNumber` subtype, table-key normalization, mixed compare/equality, `tonumber`/`tostring` | new `_u2` corpus slice A, `interp`↔`clua55` byte-exact; **all existing gates stay green** |
| U2b | Operators + 5.3 arithmetic semantics | lexer/grammar/AST for `// & \| ~ << >>` + `\u{}`, 7 new opcodes (old encoding), int arith/shift/idiv-mod errors, integer `for`, subtype const-fold, VM-level string coercion (5.3 model), `global`-statement *parse acceptance* | `_u2` corpus complete; ported official `literals.lua`/`math.lua` (int parts)/`bitwise.lua` sections; old-gate triage with ledger rows |
| U2c | Encoding cutover in Go | 5.5 format in `opcode.go`/`compile.go`/`_vm.go` (+`function.go` dump), upvalue descriptors into `FunctionProto`, drop fork extras | behavior-preserving: every green corpus stays green on `interp`; new opcode unit tests |
| U2d | rt port tranche 1: ABI v4 on `lua55/` | `rt_abi55.c` core families (arith/compare/concat/tables/globals/upvalues/calls/forprep) on the **stock, zero-patch** 5.5 tree; `LUA_RT_ABI=4`; 5.5 cell tags | `runtime/tests` native ABI harness extended + green |
| U2e | wasm backend on the new encoding | `luawasm/` rewrite: 5.5 decode (via exported `lua` accessors), int64 cells + int fast paths, new op families, `rt_abi55` call table, error texts 5.5 | `_wasm-tests` (regenerated int/float/bitwise matrix) green on `interp`/`clua55`/`wasm`/`wazero`; M5/M6 gates |
| U2f | Corpora + D7 re-pin + closeout | `_wasm-err-tests` regenerated from clua55 output, dialect retirement on the 5.1 blob path, ledger rows for every intentional break, docs | full U2 gate (§6 below), milestone records |

Dependencies: U2a → U2b → (U2c → U2d → U2e) → U2f; U2c/U2d can interleave with U2b's tail but
U2e needs both. Each chunk lands green (or with documented, ledgered triage) before the next
starts.

## 4. Chunk details

### U2a — integer value model (D1), encoding-neutral

**Files**: `value.go` (`LInteger int64` leaf, `LTInteger` type tag appended after `LTChannel`,
`lValueNames` entry `"number"` — `type()` keeps printing "number"; `LVAsString`/
`LVCanConvToString` gain the leaf), `alloc.go` (int preload cache 0..127 + `LInteger2I` pool,
same pattern as `LNumber2I`), `utils.go` (`parseNumber` → `(LValue, error)` with stock
`luaO_str2num` subtype rules: decimal-int fits ⇒ `LInteger`, decimal overflow ⇒ float (5.4
rule), plain hex wraps as *unsigned* then casts (5.3 rule), dot/p-bearing hex and all decimal
float forms ⇒ float; keep the whitespace-trim + sign handling), `table.go` (key normalization:
float with exact int value ⇒ integer key at `RawSet`/`RawGet`/`RawSetH`/`RawGetH` boundaries —
the dict `map[LValue]LValue` and `k2i` must never hold two spellings of one number; array
membership 1..`MaxArrayIndex` for ints), `_state.go` (`reg.SetInteger`), `_vm.go` (`equals`:
mixed int/float numeric equality; `lessThan`/`OP_LE`: port `LTnum`/`LEnum` incl. the
`l_intfitsf` 2^53 + floor/ceil exact algorithm; **temporary** float fallbacks in `opArith`/
`OP_UNM`/`FORPREP`/`FORLOOP` for `LInteger` operands — real int semantics arrive in U2b),
`baselib.go` (`tonumber` subtype-preserving; base form returns integers like stock
`lbaselib.c`), `normalize.go` + `testdiff` (`LInteger` cases in `PrintArg`/`ValueRepr`), and
`luawasm/backend.go` constant staging (`LInteger` constants staged as f64 cells until ABI v4 —
one switch case; keeps every wasm/wazero gate green during U2a/U2b).

**Deliberate non-goals at U2a**: integer arithmetic results (float fallback as above), integer
`for` flavor, `#`/`string.rep`/lib int returns (U2b/U3), float printing dialect (U5, D7).

**Gate**: `_u2` corpus (slice A: literals & `tonumber`/`tostring` subtype, table-key
normalization `t[2.0]`≡`t[2]`, mixed comparisons incl. 2^53 edges, `type()` unchanged) +
`testdiff/u2_test.go` (interp↔clua55, the `u1_test.go` pattern). Corpora discipline identical
to `_u1` (no direct printing of integral *floats*; parse errors byte-exact; no uncaught
position-bearing runtime errors).

**Risk**: silent `case LNumber:` misses. Mitigation: `LTInteger` is a *new* `LValueType` so
every existing `Type()`-tag switch that should handle numbers is found by grep and either gets
both cases or uses a new `isNumberValue()` helper; the `.(LNumber)` assertion sites (61 in the
root package) are audited by hand for value-flow (the interpreter spec files first, libs
second — libs mostly go through `LVAsNumber`, which keeps working).

### U2b — operators + 5.3 arithmetic semantics (old encoding, additive opcodes)

**Frontend**: `parse/lexer.go` — `//` `&` `|` `~` `<<` `>>` tokens (`~=` still wins over `~`),
`\u{XXXXXX}` escapes (1–6 hex digits, ≤ 0x7FFFFFFF, UTF-8 encoded — stock `read_UTF8_seq`
semantics + error texts); `parse/parser.go.y` — precedence ladder per stock `lparser.c`
`priority[]`: `|` < `~` < `&` < shifts < `..` < `+ -` < `* / // %` < unary < `^`; unary `~`
via `%prec UNARY`; `cd parse && make` (goyacc). **`global` statement parse acceptance**
(contextual keyword — parse `global *`/`global <const> *`/`global namelist`/`global function
f() end` and lower to no-ops ≈ the implicit `global *` default; full reserved-word +
enforcement at U5): this lets the official gate files run unmodified. Ledger row: "global
declarations accepted-as-no-op until U5".

**AST**: `ArithmeticOpExpr.Operator` gains `"//" "&" "|" "~" "<<" ">>"` (string-keyed today;
no enum churn), new `UnaryBNotOpExpr`.

**opcode.go** (additive, old format): `OP_IDIV OP_BAND OP_BOR OP_BXOR OP_SHL OP_SHR` as
RK-operand arith ops + `OP_BNOT` unary; `opProps`/`opToString` rows. The wasm backend's switch
does not need them yet — the Go compiler emits them, the interpreter runs them, and old corpora
never contain them (the luawasm compile of `_u2` is not a gate until U2e; guard: the fuzz
generators stay 5.1-shaped until U7).

**compile.go**: operator switches + subtype-aware `constFold` ported from stock `lconstants.c`
(int ops fold only when representable — no overflow traps folded, division by zero not folded,
`/` always folds to float, shifts fold via `luaV_shiftl` rules); `compileArithmeticOpExpr`
emits the new ops; for-loop over int literals.

**_vm.go/_state.go** (the executable spec — the core of U2):
- `numberArith` splits into int/float/mixed paths: int±× wrap (int64), any float ⇒ float
  result, `/` always float, `//`/`%` integer floor semantics with the `'n//0'`/`'n%%0'` errors,
  float `//` = `floor(a/b)`, float `%` = `fmod` sign-adjusted (nan for 0 divisor);
- bitwise ops: operands via `tointegerns` F2Ieq (integral floats convert; else
  `"number has no integer representation"`), shifts per `luaV_shiftl`;
- `OP_UNM` int path; `OP_BNOT`; `OP_LEN` returns integers (strings and tables);
- for loop: `FORPREP` picks the integer flavor when all three of init/limit/step convert
  exactly (`luaV_doforprep`), integer stepping with the overflow check, float flavor unchanged
  (still float-accumulating);
- string→number coercion in arith: VM-level (5.3 model), subtype-preserving via the new
  `parseNumber`; **documented deviation row**: 5.5 dispatches string arith through the string
  metatable (U3 delivers that; user-overridden string-mt arithmetic mm behaves differently
  until then — `bwcoercion.lua` is the covering test, skipped until U3);
- new-error texts pinned to stock 5.5 wording (D7 direction): `"attempt to perform 'n//0'"`,
  `"number has no integer representation"`, `"'for' loop overflow"`, arithmetic-on-non-number
  texts per `luaG_opinterror` shapes.

**Gate**: `_u2` complete (arith/bitwise/shift/for/corner matrix + ported official sections);
`go test ./...` + `cmd/testdiff` legs triaged: expected intentional breaks in 5.1-era corpora
(negative-int `%` sign, `//` on mixed, integer `for` flavor edges) get `skips.go` reasons +
ledger rows (D11); `_u1` reviewed (its discipline already avoids the affected shapes).

### U2c — encoding cutover (D2) in Go

**opcode.go**: full rewrite to the 85-opcode 5.5.1 set and format (verified against vendored
`lopcodes.h`): `iABC` (op 7 / A 8 / k 1 / B 8 / C 8), `ivABC` (vB 6 / vC 10; `NEWTABLE`,
`SETLIST`), `iABx` 17, `iAsBx` 17, `iAx` 25, `isJ` 25 with `OFFSET_sJ`; **exported**
`OpGet*`/`OpCreate*` accessors so `luawasm` stops re-implementing them. Note the 5.5 control
shape: `FORLOOP`/`TFORLOOP` use unsigned `pc -= Bx` back-edges, `JMP` is `sJ`; compare ops take
`(A B k)` register operands (`EQK/EQI/LTI/LEI/GTI/GEI` immediate family); `MMBIN/MMBINI/
MMBINK` are explicit fallback slots after arith ops; `LFALSESKIP` replaces the `LOADBOOL`-C
trick; `RETURN0/RETURN1`; `GETTABUP/SETTABUP` for globals (folding the U1 `OP_GETGLOBAL` _ENV
semantics into the standard shape); `SETLIST` k-flag + `EXTRAARG`; `LOADKX`+`EXTRAARG`.
`VARARGPREP` is emitted as the first instruction of vararg protos and implemented as a thin
marker over the existing call-time frame shuffle (the real vararg model is U5 — ledger row).

**compile.go**: emitters rewritten to the new families; **upvalue descriptors move into
`FunctionProto.Upvalues []Upvaldesc{InStack bool, Idx uint8}`** (deleting the pseudo-
instruction producer and all five consumers); register/k selection per stock `lcode.c`
(`luaK_isKint`-style immediate eligibility, `sC` immediates, K-pool ints); jump patching keeps
the label-based backpatch but writes `sJ`/`Bx` offsets at finalization; `MOVEN` peephole and
`NOP` filler die; SETLIST extra-word encoding dies (k-flag form replaces it — also fixes the
latent backend bug with >25,550-element constructors noted by the backend survey).

**_vm.go**: dispatch + all handlers re-decoded; semantics unchanged from U2b (the handlers were
written semantics-first, so this is decode + operand-shape work plus the genuinely new shapes:
MMBIN fallback slots, immediate compares, TFORPREP/TFORCALL/TFORLOOP split, ERRNNIL stub).

**function.go**: dump update (cosmetic). **CLAUDE.md**: update the "41 opcodes"/pseudo-
instruction invariants.

**Gate**: behavior-preservation — every corpus green on `interp` before the cutover is green
after (byte-exact event logs; `TestU1Gate`, full `_u2`, `_lua5.1-tests`/`_glua-tests` interp
legs, selfdiffs), plus new `opcode_test.go` encode/decode round-trips.

### U2d — rt port tranche 1: ABI v4 on `runtime/lua55/` (zero patches)

The U2 wasm gates require the rt blob to speak integers **losslessly**; a 5.1-based blob
cannot (values crossing `rt_settable`/`rt_getupval`/… would round-trip through f64). So the
port onto the stock 5.5 tree starts here (the parent plan's "rt_abi.c port begins"), with U6
completing it (prod flavor, `rt_sandbox`, TBC/vararg entries, memlimit/deadline re-validation).

- `runtime/rt_abi55.c` + `rt_abi.h` → `LUA_RT_ABI=4`: cells become native 5.5 `TValue`s with
  variant tags (`LUA_VNUMINT`/`LUA_VNUMFLT`) — no dual representation anywhere; arith
  delegates to 5.5's own `luaV_arith`/`lua_tointegerns` family; compare/concat/tables/globals/
  upvalues/`rt_call`/forprep port mechanically from `rt_abi.c` (the M5/M6 lessons carry:
  reentrancy law, rt-owned upvalue registry off `L->openupval`, savestack rebasing — row 31).
- **The precall seam without patching ldo.c** (keeps the U0 zero-patch discipline): compiled
  protos register as CClosures over the existing `host.wasm_dispatch` trampoline (wasm idx as
  upvalue) instead of the 5.1 `precall_wasm` adapter; `rt_wasm_*` registration and the
  tailcall-staging protocol (nret −2) re-derived on 5.5's `CallInfo`.
- Error texts: stock 5.5 wording directly (no dialect layer on this blob); `gnumfmt` is not
  linked — 5.5's own `luaO_tostringbuff` is the formatter.
- `build55.sh` grows the rt_* export list (a `lua55_rt.wasm` flavor alongside the oracle
  `lua55_sjlj.wasm`).
- `runtime/tests/rt_native_test.c`: integer arith/compare/forprep/table-key-normalization/
  huge-int fidelity cases added; run.sh flavors unchanged (plain/ASan/UBSan).

**Gate**: native ABI tests green; the blob compiles byte-deterministically on the arm64
wasi-sdk pin.

### U2e — wasm backend on the new encoding

`luawasm/emit.go`+`ops.go` substantially rewritten against the exported `lua` decoders:
partitionBlocks leader rules for the new jump shapes (`JMP` sJ, compare+`k`, `FORLOOP` Bx
back-edge, `TFORCALL`, `LOADKX`+`EXTRAARG` variable-length, MMBIN skip slots); int64 cells
(two-way tag dispatch `VNUMINT`/`VNUMFLT`; i64 fast paths for `ADD/SUB/MUL`, `BAND/BOR/BXOR/
SHL/SHR`, `UNM/BNOT`; mixed/`DIV/MOD/IDIV/POW` and every fallback via `rt_arith` v4);
integer+float inline for-loops; `GETTABUP`/`GETFIELD`/`GETI`… lowering to `rt_gettable` shapes;
constant pool unified (single k table with ints; `rt_mkinteger`); upvalue descriptors read from
the proto field (the five pseudo-sites collapse); `_ENV` seeding via the parentless-capture
path (ledger row 53 carries). Scratch/vararg frame layout untouched (U5).

**Gate**: `TestWasmFullMatrix` (regenerated matrix) on `interp`/`clua55`/`wasm`/`wazero`;
`TestWasmErrorSuite`, `TestWasmGluaFull`, M5/M6 seams, `host` package, `_cli-tests` wasm legs.

### U2f — corpora, D7 re-pin, closeout

Regenerate `_wasm-tests` (int/float/bitwise/shift/for matrix via the existing generators —
D11) and re-pin `_wasm-err-tests` mechanically from `clua55` output; retire `rt_set_dialect`
usage on any engine still shipping the 5.1 blob (the `clua` 5.1 oracle keeps stock 5.1 texts);
ledger pass: every intentional 5.1-era break gets its row + covering test; update
`docs/Lua-Wasm-Design-and-Test-Plan.md` milestone history + the parent plan's status block +
CLAUDE.md; bump `_VERSION`-adjacent surfaces only where the parent plan says (5.5 `_VERSION` is
U5).

## 5. Deviations from the milestone table (documented rulings)

1. **`bwcoercion.lua` is a U3 gate, not U2.** The file *overrides* the string metatable's
   arithmetic metamethods — it tests the 5.4 string-mt coercion model, which the parent plan
   schedules at U3 ("string-metatable arithmetic (5.4 coercion model)"). At U2 the interp does
   5.3-style VM-level coercion; `bwcoercion.lua` cannot pass (it also needs `math.tointeger`,
   a U3 lib). U2's official-suite coverage = `literals.lua` (minus the `os.setlocale` locale
   block, unavailable on wasm hosts) and `math.lua` int parts + `bitwise.lua` core, with the
   `global`-statement lines running via U2b's parse acceptance and `string.packsize`/
   `math.maxinteger` dependencies stubbed-in or the sections ported into `_u2`.
2. **`global` becomes a contextual keyword at U2b** (parse-and-ignore), a reserved word with
   enforcement at U5. Keeps every 5.1-era corpus green (`global` stays usable as an identifier
   there) while letting the official 5.5 files execute. Ledger row required.
3. **rt blob target**: the milestone's "rt_abi.c port begins (ABI v4)" is read as *begins on
   `lua55/`* (§4 U2d rationale: lossless int64 fidelity through the ABI is impossible on a
   5.1-based blob). U6 keeps its scope (completion: prod flavor, sandbox, TBC/vararg entries,
   hardening).
4. **Float printing stays Go-dialect until U5** (parent plan D7 schedules the 5.5 formatter
   there; verified `%.15g`→`%.17g`+`.0` algorithm in `lobject.c`). `_u2`/`_wasm-tests`
   disciplines avoid printing integral floats; integers print exactly.
5. **`\u{}` escapes move into U2b** (they are 5.3 lexer features, and `literals.lua` needs
   them; the parent plan's U1 list did not mention them).

## 6. U2 exit gate (consolidated)

- `go test ./...` green (root = interpreter; testdiff gates), including the new
  `TestU2Gate` (`_u2`, `interp`↔`clua55`) and the U2 slices of the official suite.
- `TestWasmFullMatrix` on the regenerated int/float/bitwise matrix × {`interp`, `clua55`,
  `wasm`, `wazero`} green; `TestWasmErrorSuite` re-pinned and green; M5/M6/host/`_cli-tests`
  green.
- Ledger: zero un-rowed divergences; rows for — global-no-op acceptance, string-mt arithmetic
  deferral, forprep marker varargs (if still applicable), every skipped 5.1-era case.
- Docs updated (this file's status log, parent plan status block, design-plan history).

## 7. Status log

- **2026-09-24** — plan written; tree surveyed (three exploration reports: backend structure,
  rt ABI surface, FunctionProto blast radius — distilled into §2). **U2a started** (see git
  history for progress within each chunk).
- **2026-09-24 (later)** — **U2a implemented and gated**: `LInteger` leaf + `LTInteger` (name
  "number"), int preload/pool in `alloc.go`, subtype-preserving `parseNumber` (decimal
  overflow→float, hex wraps unsigned) + `floatToIntExact`/`intFitsFloat`/floor/ceil helpers,
  table-key normalization at every `LTable` boundary (`t[2.0]`≡`t[2]`, `Next` yields integer
  array keys), mixed int/float equality and order comparisons (LTnum/LEnum ports incl. the
  beyond-2^53 exact paths), `tonumber` re-pinned to stock semantics (base form = strict
  `b_str2int` port returning integers), integer-tolerant arg plumbing
  (`CheckTypes`/`CheckInt`/`OptInt*`/`OptNumber`/`ToInt64`/`debug.getinfo`/`io.read` sizes/
  `select`/`channel.make`/`ObjLen`), lexer `numeralOK` accepts decimal-overflow and any-length
  hex numerals, wasm backend stages integer constants as float cells (ABI v3 compat),
  `reg.SetInteger` + VM float-fallbacks for arith/UNM/forprep (temporary, U2b replaces).
  `parseNumber` signature change audited across all callers; coroutine-yield test
  expectations moved to `LInteger`. **Gate**: `_u2` corpus (5 cases: lit/cnv/key/cmp/eq) +
  `testdiff/u2_test.go` (`TestU2Gate`, interp↔clua55 byte-exact) green; full `go test ./...`
  green. Corpus discipline notes: `-0` folding, integer-wrap arithmetic, and arg-error
  wording are U2b+ material (documented in-case).
- **2026-09-24 (U2a closeout)** — three fixes fell out of the gate work:
  (1) `next()` re-pinned to stock 5.5 `findindex` semantics — the incoming key is NOT
  normalized (`next(t, 2.0)` is an *invalid key* even though `t[2.0]` reads fine —
  `keyinarray` is integer-only and `getgeneric` hashes the raw key); `baseNext` screens
  presence with a `findindex` port and raises the positionless `invalid key to 'next'`
  (stock raises from the C frame). Hash-part iteration *order* remains engine-specific
  (interp = insertion order, stock = slot order) — corpus discipline, as before.
  (2) `cmd/glua -w` and `cmd/luawasmc` now pass `@path` chunk names (loadfile convention)
  so compile-refusal errors match the other legs; the `_cli-tests` `syntaxerr` goldens were
  stale **since U1** (they still pinned the pre-U1 `at EOF: syntax error` wording) —
  regenerated; `_cli-tests make` PASS. (3) key02 case added covering explicit-key `next`.
  Final U2a state: `go test ./...` exit=0 (testdiff full gates ~6 min), `_cli-tests` PASS,
  `TestU2Gate` 6/6 byte-exact. **Next: U2b** (operators + integer arithmetic semantics).
