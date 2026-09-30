# U4 implementation plan — 5.4 language

> Parent plan: `docs/Upgrade-from-5.2-to-5.5-of-Lua.md` §6 (U4 row), decisions D4, D5, D6, D7, D8, D11.
> Same working-breakdown convention as `docs/U2-implementation-plan.md` / `docs/U3-implementation-plan.md`:
> independently gated chunks, documented deviations, status log. The divergence-ledger discipline
> applies to every chunk: a divergence without a row is a bug; a row without a covering test is a bug.

## 0. What U4 is (from the parent plan)

The 5.4 language core in the Go interpreter + the wasm backend, gated against the stock-5.5.1 oracle:

- `<const>`/`<close>` local attributes (parse acceptance landed at U3a as no-ops — semantics here),
- `OP_TBC` emitted by the compiler, executed by the VM and lowered by the backend,
- `__close` on normal scope exits AND error-path unwinds (the PCall row-44 extension; the wasm
  RT_ERR path at the trampoline seam),
- the 5.4 duplicate-label visibility rules,
- the `__le`-from-`__lt` mirror removal,
- `os.exit(code, close)`.

Gate (milestone table): official `attrib.lua`, `events.lua`, `errors.lua`, `coroutine.lua` green
`interp`↔`clua55`; TBC-in-error-path cases added to `_wasm-err-tests` (all engines).
Effort estimate: 2–4 person-weeks.

## 1. Ground-truth inventory (verified 2026-09-30, against the vendored tree + oracle probes)

All semantics verified by `cmd/clua55-run` probes + reading `runtime/lua55/src/{lparser.c,lfunc.c,
ldo.c,lvm.c,lcoro.c,loslib.c}`; line references are the vendored tree.

### 1.1 Attributes (`local x <const>` / `local x <close>`)

- Grammar (`localstat`, lparser.c:1821): an optional LIST-WIDE prefix attrib (`local <close> a, b`)
  sets the default kind; each name may override with its own postfix attrib. Unknown attrib name →
  semerror `unknown attribute 'bogus'`. Two attribs on one name parse as garbage (expression
  statement) — `local x <close> = nil <const>` → "unexpected symbol near <eof>".
- **At most one `<close>` per local statement**: `local <close> a, b` → `multiple to-be-closed
  variables in local list` (errors.lua:537 gates this text).
- Kinds (lparser.h:101): `VDKREG 0, RDKCONST 1, RDKVAVAR 2, RDKTOCLOSE 3, RDKCTC 4, GDKREG 5,
  GDKCONST 6`. Assignment to anything not VDKREG → `attempt to assign to const variable 'x'`
  (`check_readonly`, lparser.c:286) — raised from `restassign` per LHS var and `funcstat`; a
  `<close>` local is assignment-locked the same way. Const-ness flows into upvalues
  (`Upvaldesc.kind`; the VUPVAL arm of check_read_only).
- **5.5 has TWO const mechanisms**: `RDKCONST` (register-resident, assignment-checked) and
  `RDKCTC` (compile-time constant: promoted when nvars==nexps and the initializer folds; occupies
  NO register — value rides the Vardesc; `local x <const> = 8` folds like a literal). **U4 ruling:
  the fork implements RDKCONST only** — every local keeps its register; RDKCTC (and the `global`
  GDK kinds) land with U5's declaration resolver. Not observable in the four gate files (they use
  `<const>` only for assignment-error and `_ENV`-swap tests; the RDKCTC-fold-sensitive files —
  code.lua, locals.lua — are U5 gates). Ledger row.
- `OP_TBC` is emitted AFTER the initializer expressions evaluate (lparser.c:1843 `checktoclose`
  tail), A = the `<close>` var's register; an initializer error never closes the variable.
- The numeric-for and generic-for control variables are `RDKCONST` (lparser.c:1694/1721):
  `for i=1,3 do i = 10 end` → `attempt to assign to const variable 'i'`. **Parent plan had this
  at U5 ("read-only for variables"); it falls out of the const machinery — pulled forward into
  U4a** (goto.lua/locals.lua pin it; no U4 gate file needs it, but the divergence window closes
  free).
- Semerror shape (luaK_semerror, lcode.c:43): NO "near 'token'" suffix; reported at the line of
  the last CONSUMED token. The fork's current raiseCompileError(line+1 …) shape must match this.

### 1.2 TBC runtime (`luaF_newtbcupval` / `luaF_close`, lfunc.c)

- Registration (OP_TBC, lvm.c:1646 → luaF_newtbcupval lfunc.c:165): `false`/`nil` values are
  exempt (no entry, no check); any other value MUST have `__close` at registration time, else
  `variable 'x' got a non-closable value` — name from debug info (luaG_findlocal), position
  prefix from the current frame (probe: `t:1: variable 'q' got a non-closable value`). The tbc
  list is stack-ordered (innermost = highest slot = closed first), threaded through the stack
  slots (deltas; dummy nodes past 65535 — the fork uses an explicit list instead, same order).
- `luaF_close(L, level, status, yy)` (lfunc.c:216): closes upvalues >= level FIRST, then tbc
  entries >= level top-down. `__close(obj)` **1-arg** on normal exits (OP_CLOSE: status LUA_OK;
  returns: CLOSEKTOP), `__close(obj, errobj)` **2-arg** on error paths (errobj = the in-flight
  error object).
- OP_CLOSE (lvm.c:1640): `luaF_close(ra, LUA_OK, 1)` — a `__close` error here propagates as a
  normal error (probe: normal-path close error surfaces as-is).
- Error paths — `luaD_closeprotected` (ldo.c:1042): loop-until-clean. Each `__close` error
  becomes the in-flight status/errobj for the REMAINING closes; closing continues; the LAST
  raised error wins (probe: closes `c,err / b,error / a,ok` + `error("orig")` → all three close
  (C, B, A order), pcall reports "b bad"; two failing closers `a bad` then `b bad` → reports
  "a bad"). No special "error in __close handler" text exists in 5.5 — status replacement only.
- Returns: `luaK_finish` (lcode.c:1930) sets the RETURN/RETURN0/RETURN1/RETURN k-bit when
  `fs->needclose`; the VM closes the WHOLE frame at return (CLOSEKTOP). Tail-call promotion is
  suppressed inside any tbc scope (`bl->insidetbc`, lparser.c:2032) — a tbc var must not be
  skipped by a tailcall.
- Generic for: the 4th explist value is the closing value — TFORPREP swaps it with the control
  and registers it tbc (lvm.c:1862). **Stays deferred to U5** (the existing row; locals.lua:1202
  is its gate; no U4 gate file uses the 4th value).

### 1.3 Coroutines + os.exit

- `coroutine.close` of a suspended coroutine runs the thread's pending tbc closes (`__close(obj)`
  with err==nil — coroutine.lua:200-210 asserts `err == nil`), then the thread is dead; a failing
  `__close` → `false` + the error (coroutine.lua:172-179, 251-288, 557-574: "last error (20)").
- Dying-thread closes: a coroutine that errors has its tbc closed as it dies (threadRun's recover
  today closes nothing — row-44's frame loop extended to tbc).
- `os.exit(code, close)` (loslib.c:396): boolean arg1 maps true→0/false→1; `lua_toboolean(L,2)`
  gates a full `lua_close` (all live tbc/upvalue closes) before `exit(status)`.
- `error()`/`error(nil)` re-pin: 5.5 replaces a nil error object with the STRING
  `<no error object>` (probe: `print(pcall(error))` → `false <no error object>`). **Parent plan
  had this at U5; errors.lua:49 needs it now — pulled forward.** The interp currently surfaces a
  bare nil.

### 1.4 Labels/gotos (the 5.4 duplicate-label rules as implemented by 5.5.1)

- Duplicate detection (`checkrepeated`, lparser.c:1568): a label name may not repeat while a
  same-name label is VISIBLE — the label list is function-scoped with block truncation
  (`solvegotos` pops to `bl->firstlabel` at block end), so: same block → error; enclosing block →
  error (probe: `do ::l:: do ::l:: end end` → `label 'l' already defined on line 1`; with a local
  in between too); SIBLING blocks → fine (first label popped before the second is declared).
  Message: `label 'l' already defined on line %d` (errors.lua:525 pins it).
- Pending gotos match ONLY the finishing block's own labels (`solvegotos` lparser.c:696); the
  fork's current machinery walks into the parent at each block close and never tries the current
  block — so `goto l; do local a=1; ::l:: end` (a goto BEFORE the block matching a label INSIDE
  it, scope error expected: `<goto l> at line 1 jumps into the scope of 'a'`) reports the wrong
  error today ("no visible label") — and worse, `do ::l:: local a=1; do goto l end end` compiles
  into an infinite backward loop on the fork (stock compiles it; the loop is legal code there —
  the FORK's mis-resolution direction is the bug, matching stock's pc wiring fixes it).
- Wording: stock `"<goto %s> at line %d jumps into the scope of '%s'"` — NO "local" word; the
  fork prints `local 'x'`. Semerror line = lastline (fork: line+1 off-by-one on multi-line cases).
- closegoto (lparser.c:597): gotos that escape a scope needing close get the dead OP_CLOSE
  placeholder SWAPPED before the JMP (A = register level at the label); `gt->close` is set when
  a finishing block has `upval` (marktobeclosed sets it for tbc too) and the goto escapes some
  variable's scope. The fork's FindLabel patches A but the trigger conditions only cover
  captured upvalues — tbc scopes must drive CLOSE on goto/break too (D4).
- createlabel's last-statement rule (lparser.c:678): a label that is the last statement of a
  block records the OUTER variable level, so `do local a <close> = nil; goto l; ::l:: end` (goto
  AFTER the decl) is legal, while jumping FORWARD over the declaration (`goto l; local a <close>
  = nil; ::l:: …`) is the scope error (probe).

### 1.5 `__le` mirror removal

- Stock `luaT_callorderTM`: `__le` (or `__lt`) is looked up on the lhs FIRST, then the rhs —
  there is NO both-sides-same-metamethod requirement and NO `not __lt(swapped)` mirroring
  (removed in 5.4). events.lua:416-429 pins the single-sided case (`d <= c` must use the OTHER
  operand's `__le`).
- The fork today: `leValues` (_vm.go) mirrors via `!objectRationalWithError(rhs, lhs, "__lt")`,
  and `objectRational` requires `m1 == m2` (both sides present AND identical) — events.lua's
  compare battery fails at line 146 (cap mismatch) on this family. Both must be re-ported:
  lhs-then-rhs lookup, no mirror, `orderError` when absent.

### 1.6 Gate-file ground truth (baselined 2026-09-30, suite posture, interp vs clua55)

- attrib.lua fails at :33 — **package.searchpath's not-found message is built TEXTUALLY in stock**
  (pusherrornotfound gsubs the raw path string: `"no file '" ++ gsub(path,";","'\n\tno file '")
  ++ "'"`, loadlib.c:471) so a trailing `;` contributes a literal `no file ''` entry (101
  templates → 102 entries / 101 newlines); the fork appends per-tried-candidate and skips empty
  templates (99 newlines). Message-construction re-pin, not a search change.
- events.lua fails at :146 — the metamethod capture battery (see §1.5; possibly more events in
  the battery — triaged in U4e's gate).
- errors.lua fails at :49 — `error()` nil-object rendering (§1.3); the file ALSO needs U5
  (`global none` + `local x = b` → "not declared", errors.lua:532) — **errors.lua cannot fully
  pass before U5**; the U4 official gate carries it as a documented skip (row cites the U5
  global-declaration work) and the U4-relevant error battery is mirrored into the `_u4` corpus +
  `_wasm-err-tests` additions instead. U5's gate (full suite ≥95%) un-skips it.
- coroutine.lua fails at :210 — coroutine.close of a suspended thread must run its tbc closes
  (§1.3); the rest of the file (self-close during close, close-in-__close chains, message-handler
  overwrite at :557-574) is the U4 coroutine work.

## 2. The chunks

| # | Name | Scope | Gate |
|---|---|---|---|
| U4a | Attributes into the AST + `<const>` semantics | parser threads attribs (prefix default + per-name), `ast.LocalAssignStmt` carries them; unknown-attribute / multiple-tbc semerrors; compiler var-kinds (const/toclose) with `check_readonly` port (locals, upvalue kind, for-control vars); semerror line/near fidelity | `_u4` const slice interp↔clua55 byte-exact; existing gates green (parse acceptance keeps the no-op `global` posture) |
| U4b | `<close>` lowering + TBC runtime, normal paths | OP_TBC emission (after inits; A = var register); block/loop/goto CLOSE sites gain tbc-awareness (RefUpvalue → needs-close); RETURN k-bit at function exit + VM frame close; no tailcall promotion inside tbc scope; VM OP_TBC/OP_CLOSE (false/nil exempt, non-closable raise with varinfo name, 1-arg `__close`); the LState tbc registry | `_u4` tbc-normal slice; locals.lua-style close-order cases in-corpus |
| U4c | goto/label re-port (the 5.4 rules) | function-visible label list + block truncation; checkrepeated; pending-goto hoisting (nactvar), closegoto CLOSE-swap with tbc; createlabel last-statement rule; wording/line fixes | `_u4` goto slice (the probe battery); goto.lua baselined + triaged (full green is a U5 gate) |
| U4d | Error-path `__close` (PCall row-44 extension) + coroutines + error(nil) | PCall recover: closeTBCprotected (loop-until-clean, last-error-wins, panic-guarded); threadRun recover closes the dying thread; coClose suspended/running arms run closes (false+err on failure); `error()`/`error(nil)` → `<no error object>`; os.exit(code, close) + L.Close running Lua-level closes | coroutine.lua tbc sections; `_u4` tbc-error slice |
| U4e | `__le` mirror removal + metamethod lookup re-port | leValues/lessThan/objectRational → luaT_callorderTM shape (lhs-then-rhs, no mirror, no m1==m2); events.lua battery triage to green | events.lua green interp↔clua55 |
| U4f | wasm backend + rt ABI TBC | OP_TBC/OP_CLOSE lowering (rt entries `rt_tbc`/`rt_close` with the tbc-aware close + name for the non-closable raise); return-path frame close; trampoline error-path closes (closeprotected loop, 2-arg `__close` with the staged error); blob rebuild + native ABI tests | matrix/error-suite wasm legs green incl. new TBC cases; `rt_close` semantics pinned in `runtime/tests` |
| U4g | corpora + gates + ledger + closeout | `_u4` corpus; `_wasm-err-tests` TBC-in-error-path additions (all engines); `u4_test.go` (TestU4Gate + TestU4OfficialSuite incl. the errors.lua U5 skip); attrib.lua's searchpath message re-pin; skips map; ledger rows; docs + memory | **U4 exit gate**: TestU4Gate, TestU4OfficialSuite (attrib/events/coroutine green, errors skipped-U5), all four engine gates on the new error cases, every prior gate green |

Dependencies: U4a blocks U4b (kinds feed TBC emission). U4b blocks U4c (CLOSE-site machinery).
U4d extends U4b's runtime. U4e independent. U4f needs U4a–U4d stable (backend mirrors the interp).
U4g last. Each chunk lands green (or with documented, ledgered triage) before the next starts.

## 3. Chunk details

### U4a — attributes + `<const>`

Parser (`parse/parser.go.y`): `attnamelist` becomes `[]attnamelistItem{name, attrib string}` (or a
parallel returns); the `TLocal` rules thread it into `ast.LocalAssignStmt.NamesAttribs` (new field,
`nil` = plain). The prefix-attrib form sets the list default (postfix overrides). `TGlobal` rules
KEEP ignoring attribs except the stock `global variables cannot be to-be-closed` semerror (cheap
parity, no resolver). `local function` keeps no attribs (stock allows none). `funcbody`'s named
varargs stay U5.

Compiler: local declarations get a kind (VDKREG/RDKCONST/RDKTOCLOSE) — carried next to the
`varNamePool` entries / `DbgLocalInfo`; `Upvaldesc` gains `Kind` (default VDKREG; serialization
sites updated — the wasm backend and `luawasmc` artifacts see an additive field). The assignment
checks port `check_read_only`: every LHS resolution in `compileAssignStmt`/`compileChainedAssign`/
function-name `funcstat` paths + SETUPVAL emission raises `attempt to assign to const variable 'x'`.
For-loop control vars declare RDKCONST (numeric + generic). Semerrors go through a helper that
matches luaK_semerror (no near-token, lastline).

### U4b — `<close>` lowering + TBC runtime (normal paths)

Compiler: after `compileRegAssignment` of a local statement with a `<close>` name, emit
`OP_TBC A` (A = that name's register). `marktobeclosed` equivalent: the owning block gains
needs-close (the existing RefUpvalue bit suffices — CloseUpvalues already emits the block-exit
OP_CLOSE; verify A operands = register levels, not var indices). RETURN: the fork's VM closes the
frame's upvalues at doReturn/TAILCALL today — extend to tbc (the unconditional frame-level close
is safe: the registry is empty when no tbc). No TAILCALL promotion when any enclosing block of the
function has a live tbc scope (the compiler tracks an insidetbc flag through the block tree).

VM (`_vm.go`/`_state.go`): `LState` gains a tbc registry — entries `{idx int (register index),
prev *tbcEntry}`, strictly increasing idx at registration (cross-frame: inner frames sit above).
`closeTBC(level int, err LValue, protected bool)`. OP_TBC: nil/false → skip; else metaOp1
`__close` or raise `variable '%s' got a non-closable value` (name via the row-70f varinfo/lazy
carrier: DbgLocals at pc). OP_CLOSE: `closeUpvalues(RA)` then `closeTBC(RA, nil, false)` —
`__close(obj)` 1-arg, errors propagate. doReturn/TAILCALL: frame-level close (CLOSEKTOP shape).

### U4c — goto/label re-port

Replace the per-block label maps with the stock model: a function-scoped label array with
`firstlabel` truncation, pending-goto array with nactvar levels, `checkrepeated` on
`::name::` creation, `solvegotos` at LeaveBlock matching ONLY the block's own labels (hoisting
the rest outward with `gt->close` stickiness), `closegoto` performing the JMP/CLOSE swap
(A = label's register level) when the goto escapes a closing scope, `createlabel` last-statement
rule. Fix the two error texts + line attribution. The break machinery (BreakLabel) maps onto the
stock hidden break-label created at loop-block leaveblock.

### U4d — error-path closes + coroutines + error(nil) + os.exit

PCall: in the recover, after `closeUpvalues(base)`, run `closeTBCprotected(base, errObj)` — a
closeprotected loop: keep closing entries >= base while catching `__close` panics; each failure
replaces the in-flight error object; the last one becomes the ApiError PCall reports. The loop
runs with `ls.Panic = panicWithoutTraceback` and its own recover (the errfunc-path pattern).
threadRun: the same close over the dying coroutine's frames before deadErr is recorded.
coClose: suspended → run the thread's closes ON the thread (its frames intact), then kill;
failure → `false, err`; running arm → the stock self-close semantics (coroutine.lua:312-321 —
os.exit(false) there is unreachable per the file's own comment; verify). os.exit: stock arg
shapes; `close` → run every live frame's closes (mainLoop-unwound equivalent of lua_close) before
the Go-side teardown. `error()`/`error(nil)`: the object becomes the string `<no error object>`
at raise (find the stock seam — luaG_/luaD_ level — and mirror it).

### U4e — `__le` mirror removal

Port `luaT_callorderTM`: lookup lhs mt then rhs mt; call with (a, b); nil → orderError. Applies
to `__le` (leValues) and `__lt` (lessThan) paths incl. the OP_LTI/LEI/GTI/GEI dispatch; delete
`objectRational`'s `m1 == m2` requirement. Triage the rest of events.lua's battery to green.

### U4f — wasm backend + rt ABI

`rt_abi55.{c,h}`: rt-owned tbc registry (frame-cell-address keyed, mirroring the upvalue
registry discipline): `rt_tbc(celladdr, name)` (nil/false skip; `__close` check; raise
`variable '%s' got a non-closable value` — the backend passes the var's name so the seam matches
the interp's varinfo answer), `rt_close(level)` (upvals then tbc, 1-arg `__close`, errors →
staged -1), `rt_close_err(level, errobj)` for the trampoline unwind (2-arg; closeprotected loop;
last error re-staged). Backend (`luawasm/`): OP_TBC/OP_CLOSE lowering (OP_TBC leaves the
"never compiles until U5" panic list), return-path close of the proto's own frame, the name table
for raise sites. Rebuild `lua55_rt.wasm` (build55.sh), `go clean -testcache`, extend
`runtime/tests` for the new ABI entries.

### U4g — corpora + gates + ledger + closeout

`_u4` slices (const/tbc-normal/tbc-error/goto/le), `_wasm-err-tests` TBC-in-error-path cases
(all engines; corpus discipline: substring finds for hint-bearing texts), `u4_test.go` mirroring
`u3_test.go` (TestU4Gate interp↔clua55; TestU4OfficialSuite over attrib/events/errors/coroutine
with the documented errors.lua→U5 skip), attrib.lua's searchpath message re-pin (loadlib.go —
textual construction), skips map entries, ledger rows (U4a const/RDKCTC deferral, U4b/c label
re-port + searchpath, U4d error(nil)/os surface, U4e mirror removal, U4f seam entries), docs
(parent status block, this file's status log), memory update.

## 4. Deviations from the milestone table (documented rulings)

1. **RDKCTC (register-less compile-time constants) deferred to U5** — the fork keeps consts in
   registers; the promotion machinery + `global` GDK kinds are one system with U5's resolver.
   Not observable in the U4 gate files.
2. **Read-only for-control variables pulled forward from U5** — free with the const kinds.
3. **`error(nil)` → `<no error object>` pulled forward from U5** — errors.lua:49 (a U4 gate file)
   needs it.
4. **errors.lua gated with a U5 skip** — its `global none`/`local x = b` case needs the U5 global
   declarations; U4 covers the file's error battery via corpus additions; U5 un-skips (its own
   gate is the full suite ≥95%).
5. **Generic-for 4th-value TBC stays deferred** (existing row) — locals.lua:1202 is its gate.
6. **os surfaces stay harness-shim-opaque** — os.exit's close semantics are unit-tested + probed
   via the raw CLI vs clua55 (the standing no-os corpus discipline).

## 5. U4 exit gate (consolidated)

- `go test ./testdiff/ -run 'TestU4'` green (gate + official suite with the one U5 skip).
- `TestWasmFullMatrix` / `TestWasmErrorSuite` / wazero legs green over the extended corpora
  (TBC-in-error-path cases on all four engines).
- Every prior gate green: U1/U2/U2f/U3 gates, clua + clua55 conformance, selfdiffs, M5/M6,
  host, `_cli-tests` (both engine flavors).
- `runtime/tests/run.sh` green on the rebuilt blob; native ABI coverage for the new entries.
- Docs updated (this file's status log, parent plan status block, milestone history, ledger rows
  with covering tests); memory updated.

## 6. Status log

- **U4a DONE (2026-09-30)** — attributes + `<const>`. Parser: `attriblist`/`attnames`
  with the three stock semerrors (unknown attribute / multiple to-be-closed /
  global to-be-closed, incl. `global attrib '*'`), `Lexer.SemError` (no
  "near '<token>'" suffix, current line). AST: `LocalAssignStmt.Attribs`.
  Compiler: `varNamePool.kinds` + `RegisterKind`/`kindAt`/`countKind`
  (varKindReg/Const/TBC = stock VDKREG/RDKCONST/RDKTOCLOSE),
  `checkReadOnlyLocal` (locals of this function + const upvalues via the
  ancestor-chain walk `capturedUpvalueKind` — no Upvaldesc.Kind field),
  for-loop control names declared varKindConst (pulled forward from U5).
  Byte-exact vs oracle on the four parse-level errors and the const-assignment
  battery. `go test .`, TestU1Gate/U2Gate/U3Gate, full short `./...` green.
- **U4b DONE (2026-09-30)** — `<close>` lowering + TBC runtime, normal paths.
  Compiler: `compileStmtTBC` (OP_TBC after initializers, RefUpvalue +
  NeedsClose + tbcActive++), RETURN0/1→RETURN k-bit rewrites in
  `compileReturnStmt` (B=1/2 per the fork's fixed-results convention),
  OP_TAILCALL suppression while tbcActive>0, `NeedsClose` set by captured
  locals (stock markupval). Runtime: `LState.tbclist` registry
  (`registerTBC` with luaG_findlocal naming, `closeTBC`), OP_CLOSE =
  closeUpvalues-then-closeTBC, doReturn/OP_TAILCALL close at lbase.
  /tmp probe battery byte-exact vs oracle (close order, nil/false exempt,
  return/loop/break closes, close-error propagation, object identity).
  The one pcall-position off-by-one vs `cmd/clua55-run` is that runner's
  documented one-line `arg` prelude (cmd/clua55-run/main.go header), not
  an engine divergence. `go test .`, U gates, TestWasmFullMatrix +
  TestWasmErrorSuite green (the RETURN rewrites pass through the U2e
  backend unchanged).
- **U4c DONE (2026-09-30)** — goto/label re-port onto the 5.5.1 machinery
  (a restructure from 5.4: labels are a function-wide ACTIVE list,
  block-truncated; gotos resolve only at leaveblock). `fc.labels`/`fc.gotos`
  replace the per-block label map; `firstLabelIndex`/`firstGotoIndex`/
  `entryNactvar` on codeBlock; `compileGotoStmt` = JMP + dead CLOSE(0,1,0)
  placeholder; `solveGotos` at LeaveBlock (match own labels, hoist the rest
  with NeedClose, retire labels); `closeGoto` CLOSE-swap; `checkrepeated`
  function-wide over active labels (raises at the label's line); wording
  "jumps into the scope of 'x'" (no "local"); `finishFunctionGotos` for the
  root block. Fixed: the old always-executed placeholder CLOSE(A=0) froze
  open upvalues at every goto (probe: captured local mutated after a
  cross-goto do-block printed stale value; now matches oracle), nested
  shadowing of an active label, per-block-only duplicate detection.
  `_glua-tests/goto.lua` updated to the 5.5 rulings ("scope of 'aa'" /
  "scope of 'xuxu'" errmsg targets; the 5.4 "goto to correct label when
  nested" case is an error now — replaced with an errmsg; the official
  5.5 suite dropped it). tbc×goto/break probes byte-exact. All gates green.
- **U4d done (row 73)** — error-path `__close` + coroutine lifecycle + the
  errors.lua naming battery. coResume = stock lua_resume guards (running/
  normal via the resume-chain Parent walk → "non-suspended"; dead; the
  resumeDepth ceiling → "C stack overflow"); threadRun's recover fixes the
  nil object to `<no error object>`, closes the dying thread's upvalues +
  tbc list with the error object (2-arg `__close`, last-error-wins), records
  deadErr; wrappedDeath makes a wrapped thread's corpse state + re-raise
  match the official "bug in 5.4.1" block; coroutine.close() on the running
  thread = kill-first + whole-list 1-arg closes + a base-level
  closeSelfUnwind panic no pcall catches (nested self-close returns true;
  os.exit after it unreachable); os.exit(code, close) runs L.Close()'s
  main-thread Lua-level exits before os.Exit. Naming battery: a
  present-but-not-callable metamethod is CALLED (guard `!= LNil`, unary/
  len events pass the object twice) so the error reads "attempt to call a X
  value (metamethod 'add')" via row 62's tmEventName; objectRational =
  callbinTM first-found (the gopherlua m1==m2 guard dies; a real 5.1↔5.5
  `__eq` divergence resolved toward 5.5, no corpus case hits the mismatch);
  objTypeName = luaT_objtypename (`__name` overrides every error text; io's
  "FILE*" metatable + io.input/io.output arg errors replace the "string or
  file expedted" typo). Re-pins: state_test.go + _glua-tests/coroutine.lua
  to the stock resume texts (+ the `%-` pattern escape). Verified: the
  official close battery (coroutine.lua :142-233) byte-exact standalone;
  errors.lua green except the light-udata block (:262-268, U4g skip —
  debug.upvalueid + light userdata, sethook precedent); remaining official
  failures attributed: events.lua:186 → U4e, attrib.lua:33 → U4g
  (loadlib), coroutine.lua's pcall-vs-close/self-close/msgh sections →
  row 22 (yield across the G seam; isyieldable-in-C is the same machinery;
  confirmed pre-existing at HEAD 8430230). Full gate battery green (root,
  U1/U2/U3, GluaFull, CLua55Conformance, U2fMatrix/Error oracles,
  WasmFullMatrix/ErrorSuite × wasmtime/wazero) — no baseline shifts.
- **U4e DONE (2026-09-30, row 74)** — the order-family restructure +
  events.lua green. lessThan/leValues = luaV_lessthan/luaV_lessequal:
  numbers primitive, strings primitive, else first-found metamethod
  (objectRational, row 73g); the mixed-type/number early orderError
  (5.1's ttype guard) and the `__le`→`!__lt` mirror (LUA_COMPAT_LT_LE,
  gone in 5.4) both die; opOrderI's metamethod arms route through the
  same two functions. rawlen (luaB_rawlen, 5.2+) added. builtinMts keyed
  by the BASIC type (`basicTypeIndex`: the integer/float pair shares the
  number bucket — `debug.setmetatable(10, mt)` covers 3.45). PLUS a
  latent compiler bug found on the way (events.lua's
  `(a or 1) + (b or 2)` metamethod body): compileConstOperand popped a
  TRAILING constant load as the operand's value, but an and/or operand
  ends with the never-executed other arm's load behind a jump —
  `5 + (23 or 2)` compiled to `23 + 2` (and a materialized operand
  landed dead behind the residual jump). The pop is now gated on the
  load being the expression's entire code. events.lua GREEN vs clua55;
  full battery green, no baseline shifts (root, U1/U2/U3 gates,
  GluaFull, CLua55Conformance, U2f oracles, WasmFullMatrix/ErrorSuite
  ×wasmtime/wazero, full -short ./...). Official triage unchanged:
  attrib:33 → U4g, errors:264 → U4g skip, coroutine:283 → row 22.
- **U4f DONE (2026-09-30, row 75)** — wasm backend + rt ABI TBC. The rt
  keeps an rt-OWNED tbc registry (address-keyed descending list, same
  zero-patch discipline as the upvalue registry — never L->tbclist):
  rt_tbc = luaF_newtbcupval + checkclosemth (false/nil exempt; the
  non-closable raise names the variable from a backend-registered name
  table via LocalNameAt — `variable 'v' got a non-closable value`, the
  position from rt_run's line); rt_close = the OP_CLOSE/return/tailcall
  close (upvalues first, 1-arg __close innermost-first, a failure keeps
  remaining entries linked like stock's failing luaF_close). Backend:
  OP_TBC→rt_tbc, OP_CLOSE→rt_close, RETURN/TAILCALL close the frame
  first. The trampoline's two error arms run rt_close_err (stock
  luaD_closeprotected: 2-arg `__close(obj, errobj)`, last-error-wins
  replacement) BEFORE rt_frame_restore — cells stay allocated while
  __close dispatches. TWO runtime bugs found + fixed en route: (1) the
  staging must be CLEARED while error-path closes dispatch (rt_run_raw,
  new): rt_run's err_pending gate was silently refusing each
  metamethod's first protected op — half the closes aborted mid-body;
  (2) err_prefixed is recomputed per staging (reset before detection —
  latent until U4f's back-to-back stagings): a stale flag suppressed
  the position prefix on the first failing close's message. ERRMEM/
  ERRERR on the guarded arm downgrade to LUA_ERRRUN through the loop
  (stock's own closeprotected catch). Ruled: error(msg, 0) inside
  __close keeps the row-60 seam (corpora use level-1 — byte-exact);
  `_cli-tests/calc.lua` fixed for 5.4's const generic-for control
  variable (U4a's check_readonly, matches stock). Verified: RT-NATIVE55
  ×{plain,ASan,UBSan} (normal/error-path/non-closable/exempt battery);
  a three-engine level-1 probe battery byte-IDENTICAL (incl. tbc×upvalue
  interplay); blob rebuilt + `go clean -testcache`; full battery green —
  TestWasmFullMatrix 1497/1497 (0 diffs, 4 ledgered skips),
  ErrorSuites + wazero legs, root, CLua55/CLua conformance, U1/U2
  gates, U2f oracles, WasmGluaFull, `_cli-tests` make
  ×{wasmtime, WAZERO=1}.
- U4g next — `_u4` corpus slices; `_wasm-err-tests` TBC-in-error-path
  additions (all engines); `u4_test.go` (TestU4Gate + TestU4OfficialSuite
  with the errors.lua light-udata and coroutine row-22 skips);
  attrib.lua's searchpath message re-pin (loadlib.go); delete
  `testdiff/u4probe_test.go` + `_u3/dev/`; ledger/docs/memory closeout.
