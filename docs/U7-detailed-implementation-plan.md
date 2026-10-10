# U7 implementation plan — soak + performance (+ row 99's durable writer)

Status: DRAFT 2026-10-10. Parent: `docs/Upgrade-from-5.2-to-5.5-of-Lua.md` (U7 row, §3
milestone table). U6 is COMPLETE (U6a–U6f landed; exit gate met — parent status header).

## 0. What U7 is (from the parent plan)

> Soak + performance: extend `cmd/luafuzz` generators to 5.5 constructs (ints, bitwise,
> TBC, globals, named varargs); fresh-clock soak (bin/run-fuzzer.sh); perf re-baseline;
> re-introduce fork fast paths where stock encoding allows (int/f64 dual inline arith,
> K-operand coverage). Gate: N-night soak with zero new-clock findings; perf regression
> report vs 5.1 baseline accepted; determinism gates green.

Plus the U6f carry-in: **row 99's daemon-side durable fix is U7's FIRST item** (U6-plan
closeout: "a deadline writer that cannot be frozen by STW, e.g. off-scheduler delivery …
ahead of soak"). That is U7a here.

### 0.1 Where things stand (verified in-tree 2026-10-10)

- **U7a is in flight, uncommitted** (working tree vs `02fb828`): the row-100 backstop —
  `host.deadline_poll` import + `gDeadlinePollCtr` back-edge counter in
  `luawasm/backend.go` (`deadlinePollInterval = 1<<20`, one host call per ~3 ms of full
  spin at the measured ~350 M transitions/s); `VM.deadlineAt` + `hostDeadlinePoll` +
  `Run` store/clear in `host/`; the probe `TestDeadlineSTWWedge` in `host/host_test.go`
  (GCPercent(1) + churn goroutine + `while true do end` + post-run liveness check).
  `note/u7a-done.md` records what the lost session left: the probe passed ONCE, but
  nothing yet proves an STW actually collided during the run, and it has not been run
  20×.
- **A blocking gap found in this tree walk:** the backend now emits an import
  (`host.deadline_poll`) that only `host/vm.go` defines. `testdiff/wasmengine.go`
  (wasmtime linker) and `testdiff/wazeroengine.go` (host module builder) do NOT — every
  wasm-family differential gate fails at instantiation until they do (§1.2). The CLI
  runners are subprocess-based (`cmd/luawasm-run` drives `testdiff.NewRunEngine`) and
  need nothing.
- **The soak is stale:** `note/fuzz/soak.json` last updated **2026-09-25**, streak
  1/7 clean nights, 2.35 M cases on the current clock (seed 1789821516881963000,
  started 2026-09-19). The clock died two weeks ago (U6 churn era) and is dead anyway
  for U7 purposes: generator changes and any engine/backend change re-arm it (R5).
- No `docs/U7-*` plan existed before this file; U7a's shape came from the U6-plan
  closeout note + `note/u7a-done.md`.

## 1. Ground-truth inventory (verified 2026-10-10, tree + greps + soak state)

### 1.1 U7a in the working tree — what exists

| Piece | File | State |
|---|---|---|
| Backstop emission (counter + poll at every dispatch back-edge / tail-restage re-entry) | `luawasm/backend.go` (`emitDeadlinePoll`, `gDeadlinePollCtr`, `deadline_poll` import) | written, builds |
| Host answer (absolute `deadlineAt` + ctx check; runs on the Run goroutine, races nothing) | `host/hostfn.go` (`hostDeadlinePoll`), `host/vm.go` (field + store/clear + export) | written |
| Wedge probe | `host/host_test.go` `TestDeadlineSTWWedge` | passes once, NOT self-verifying |
| Engine hosts for the new import | `testdiff/{wasmengine,wazeroengine}.go` | **MISSING — gates red** |
| Ledger/docs | row 100, parent status, `note/u7a-implemented.md` | not written |

Mechanism recap (row 99 → row 100): under a pending GC mark-STW the wazero-JIT spinning
guest is the only goroutine still executing; the timer that writes the deadline flag is
frozen with the scheduler. The backstop makes the guest itself ask the host every
`deadlinePollInterval`-th basic-block transition; the import call transits into ordinary
Go frames (a preemption safe point the pure spin lacks — this is also WHY the STW drains
instead of wedging), and an affirmative answer takes the same `rt_deadline` raise as the
flag path: identical text ("context deadline exceeded"), staging, and pcall/catch
semantics.

### 1.2 The instantiation-host gap (found today, U7a-blocking)

Every host that instantiates backend-compiled script modules must now satisfy
`host.deadline_poll`:

- `testdiff/wazeroengine.go:212` — builder exports `event`, `wasm_dispatch`,
  `host_call` only. The engine already has a deadline posture (U6b/row 37: bare flag
  write at `rt_ctrl_addr()` from a timer). Add `deadline_poll` that consults the same
  armed deadline (start recorded when `e.Deadline` is set) and, when expired, writes
  the flag AND returns 1 — both delivery paths raise identical text, so the M6d
  goldens are unchanged; delivery just becomes STW-proof on this engine too.
- `testdiff/wasmengine.go` (~line 345+ linker defines) — `linker.DefineFunc` the same
  consult-only shape (no flag write needed; wasmtime preempts JIT spins itself, the
  poll is correctness-of-instantiation + honesty, not wedge rescue).
- Unarmed (`e.Deadline == 0`): return 0 — zero behavioral delta on every
  non-deadline gate.

### 1.3 The fuzzer inventory

- `cmd/luafuzz` (455 lines): CLI over the `luafuzz` package — `-soak` (self-restarts
  every 2000 cases via exec; process-bounded chunks because wasmtime per-Store
  resources only reclaim at exit), `-replay`, `-min`, `-dumpgen`, `-status`,
  `-reset-soak`.
- `luafuzz/` package: `gen.go` (1200 lines — the generator), `runner.go` (engine legs:
  **interp, wasm, wazero, wazero-prod** — no clua55, see R2), `shrink.go` (source-level
  reducer), `soak.go` (state/streaks), `luafuzz_test.go`.
- `bin/run-fuzzer.sh`: 390 m budget loop, battery watchdog (pause <50%, resume >60%),
  builds once, execs chunks; `bin/check-fuzz-status.sh` for progress.
- Soak accounting: per-night case/exec rows, streak vs the **7-night / 10 M-exec**
  standing target (soak.json's "×10⁷ gate"; note/run-the-fuzzer.md's "nights
  accumulating toward 7").

### 1.4 Generator gaps vs 5.5 (gen.go today is 5.1-shaped — U2-plan line 145 deferred
this here)

Present pools: `numPool` (float-shaped), `intPool`, `numStrPool` (`"10"`,`"2"`,`"100"`,
`"-3"`,`"3.5"` — arith-coercible strings), `safeNumStrPool` (row 49: constrained so
computed tostring stays engine-identical), `smallInts`, fmt specs. Missing 5.5
constructs, with the oracle corners each must cover (source of truth: the U2f grids in
`testdiff/gen_matrix_corpus_test.go`, rows 62/70):

| Construct | Corners to generate |
|---|---|
| int/float duality | integer vs float literals (incl. `x.0`, `.5`, `1e15`, hex `0x…`, maxint/minint/2^53/2^53+1), `//` and `%` with negatives/zero, integer overflow wrap, mixed int/float compare edges, `math.type` in outputs, string→number coercion by subtype |
| bitwise | `& | ~ << >>` on ints; negative shift counts, shifts ≥64, integral-float and coercible-string operands (bwcoercion.lua corners), unary `~` |
| TBC | `local x <close> =` over `setmetatable({}, {__close=…})`; nil/false TBC; closes on the error path (pcall around); `__close` itself raising — **bounded: ≤3 live TBCs per case, close bodies tiny** (row 44 semantics) |
| globals | `global g`, `global <const> K` (parser: contextual keyword, attribs — `global <close>` is a semantic error and stays OUT of generation), implicit-global writes, reads through `_ENV` (GETTABUP/SETTABUP legs) |
| named varargs | `function f(...name)` (parser parlist `T3Comma TIdent` → `VArgName`) plus ordinary `...` — GETVARG/VARARGPREP shapes (U5 vararg model), `#name`, selects, mixed with fixed params |

Every extension must respect: `spend()` budget (case wall-time stays bounded), per-case
`caseRNG` determinism, and shrinkability (§3 U7b). Generated scripts keep the row-98
self-naming discipline: first output line identifies the case (already the convention;
verify, don't regress).

### 1.5 Perf baseline inventory

- `note/bench-vs-orig/` — the M8-era hostbench (h_tiny/h_str/h_strlong; interp-fork vs
  wasm cold/warm postures) + its 2026-09-23 output = the **5.1-core-era reference
  numbers** (pre-U0).
- Baseline tree pin: `9b89643` ("Table formated for text read") is the last 5.1-core
  commit before `7dac60a` (U0). Bench both trees on the same machine/session;
  benchstat for deltas.
- Backend fast-path state: U2e already gives int64/float dual inline arith with MMBIN*
  miss folding; K operands come free from the stock frontend (U2c `compile.go`). D2
  dropped `MOVEN`/`GETTABLEKS` (fork opcodes; no stock encoding → **stay dropped**, R4).
- `docs/Performance-Improvement-Plan.md` — the interp cost-center ranking (LValue
  interface, register bookkeeping, table multi-map, metamethod checks, no interning…)
  still describes the fork's interp; U7c re-validates the top items against pprof
  rather than trusting 2026-09 reads.

### 1.6 Constraints carried in

- **M6e cliff:** past ~4k wasmtime Stores per process the test binary SIGTRAPs; the
  fuzzer's 2000-case exec-chunks are the mitigation. clua55 stays out of the soak
  engine set (R2).
- **Row 99 test posture:** the three host watchdog tests quiesce GC via `noHostGC`;
  U7a's probe is the ADVERSARIAL complement (forces the collision) — both live.
- **Ledger discipline:** every soak finding gets a row + covering test; every skip a
  reason string.

## 2. The chunks

| # | Name | Scope | Gate |
|---|---|---|---|
| U7a | Row-100 STW-proof deadline writer — finish + land | (a) `deadline_poll` on both testdiff engines (§1.2); (b) self-verifying probe (R6 metric); (c) evidence runs: probe ×20 plain + ×2 `-race`, host suite, `go test -short ./...`, wasm-family gates; (d) poll-cost spot-check (`note/u7a-hot.lua` vs `02fb828` build); (e) ledger row 100 + parent status + `note/u7a-implemented.md` + commit | All listed runs green with the collision proven ≥1 STW in-window per probe run; wasm-family gates instantiate and stay byte-exact; tight-loop regression ≤ ~5% |
| U7b | 5.5 fuzz generators + interim soak | gen.go extensions (§1.4 table, one construct per commit-able slice); shrink + determinism + unit tests; 4-engine differential over a sampled slice; re-arm fresh interim clock via `bin/run-fuzzer.sh` | Generator unit tests green over ≥10 seeds; sampled differential slice (≥2000 cases × {interp,wasm,wazero,wazero-prod}) green or triaged-to-rows; interim soak running with ≥1 clean chunk and the triage path re-verified |
| U7c | Perf re-baseline + fast paths + report | promote hostbench into the tree (+ numeric-loop/table-churn/hostfn-roundtrip scripts); bench vs `9b89643` (interp) and vs U6 (wasm family); profile-gated fast paths (K-operand audit first; then top pprof winners); `note/u7-perf.md` report + acceptance | Report written and accepted; every fast path lands byte-exact-gated with a recorded micro-bench delta; determinism + full gates green |
| U7d | Final-clock soak + closeout | re-arm the COUNTING clock on the post-U7c engine state; 7 clean nights / 10 M execs; triage discipline; ledger audit for U7 rows; parent status → U7 COMPLETE; CLAUDE.md sweep (bench command); memory | **U7 exit gate (§5)** |

Dependencies: U7a first — the tree is red on the wasm-family gates until §1.2 lands,
nothing rides a red tree. U7b next (arms the interim clock early so generator/engine
bugs surface while U7c proceeds). U7c's engine changes re-arm the clock, so the
counting soak is U7d, after U7c. Within U7b the construct slices are independent;
within U7c the audit precedes any fast path (measure twice, patch once).

## 3. Chunk details

### U7a — row-100 writer: finish + land

Most of the code exists (§1.1). The finishing work, in order:

1. **Engine hosts.** `testdiff/wazeroengine.go`: extend the builder with
   `deadline_poll`; capture `deadlineAt := time.Now().Add(e.Deadline)` where the
   flag-timer arms (only when `e.Deadline > 0`); the poll returns 0 unarmed, and 1 +
   the same `mem.Write(flagAddr, deadlineFlagLE)` when past — the M6d legs keep their
   goldens (identical raise either way) and gain STW-proof delivery.
   `testdiff/wasmengine.go`: `linker.DefineFunc(store, "host", "deadline_poll", …)`
   with the consult-only shape. Both engines' differential legs otherwise observe
   zero change (unarmed polls return a constant 0).
2. **Self-verifying probe** (closes `note/u7a-done.md`'s open question — R6):
   `runtime.ReadMemStats` immediately before `vm.Run` and right after the kill;
   assert `NumGC` delta ≥ 1 AND `PauseTotalNs` increased, `t.Logf` both. With the
   guest spinning unpreemptibly for the whole window, a cycle completing in-window
   means its mark STW was satisfied through the poll's Go-frames safe point — the
   row-99 collision, survived. The assertion kills the silent-pass mode where GC
   never engaged and the probe proved nothing.
3. **Evidence runs.** Probe ×20 plain + ×2 `-race` (record counts in the note);
   `go test ./host/` plain + `-race` ×2; `go test -short ./...`;
   TestWasmFullMatrix/TestWasmErrorSuite/TestWasmGluaFull + wazero legs (these
   instantiate the import — item 1 is what makes them run at all).
4. **Poll-cost spot-check.** `note/u7a-hot.lua` (50 M-iteration integer FORLOOP) on
   this tree vs the `02fb828` build; expect ≤ ~5% (one global dec + branch per
   basic-block transition; one host call per ~3 ms of spin). Record the numbers.
5. **Docs + commit.** Ledger row 100 (writer design, the safe-point insight, engine
   poll semantics, R6 metric ruling, evidence); parent status header gains the U7a
   line; `note/u7a-implemented.md`; commit `U7a: STW-proof deadline writer — row 100`.

### U7b — 5.5 fuzz generators + interim soak

Per construct in §1.4 (suggested order — ints first, they exercise the most
semantics; bitwise rides the same pools; then TBC (error-path closes), globals, named
varargs):

- Pools/primitives first (`intPool` → true int corners incl. 64-bit edges; a float
  pool; extend `arithOperand`/`num` to subtype-aware picks), then statement/expr
  emitters, then the construct's dedicated emission (e.g. `stmtLocalDecl` learns the
  `<close>` attrib; `function` emission learns `...name`).
- Every emitted construct goes through the existing guards: `spend()`-bounded,
  deterministic under `caseRNG`, output-normalization-safe (row 49's lesson: only
  literals from safe pools into `tostring`-visible slots unless the value is
  provably engine-identical — extend `safeNumStrPool` thinking to int results:
  integer arithmetic is exactly representable on every engine, floats are the hazard
  they already handle).
- `shrink.go`: confirm the reducer tolerates the new syntax (it reduces source text;
  add cases to `luafuzz_test.go` that shrink a TBC/named-vararg/bitwise repro).
- `luafuzz_test.go`: generator validity sweep — over ≥10 master seeds, every case
  parses and completes on interp within the case deadline.
- Differential validation: `-dumpgen` a fixed seed slice (≥2000 cases), run
  ×{interp,wasm,wazero,wazero-prod} in-process (the runner already supports engine
  sets); any DIVERGE → shrink → repro → ledger row → fix or ruled skip. This is the
  construct-coverage gate: each slice's corners must APPEAR in the dump (grep the
  dump for `//`, `<close>`, `global `, `...` names — coverage assertions, not vibes).
- **Re-arm the interim clock**: `note/fuzz` archived (the 2026-09-19 clock's 1/7
  streak and 2.35 M cases preserved under `note/fuzz-retired-*`), fresh seed, nohup
  caffeinate `bin/run-fuzzer.sh`, first chunk green, `bin/check-fuzz-status.sh`
  verified. This clock does NOT count toward the exit gate (R5) — it exists to shake
  out the new generators at scale while U7c proceeds.

### U7c — perf re-baseline + fast paths + report

1. **Harness in-tree.** Promote `note/bench-vs-orig/hostbench` → `cmd/luabench` (same
   postures: interp-fork TOTAL, wasm cold fresh-VM, wasm warm pooled-VM; add
   wazero-prod) + scripts: h_tiny/h_str/h_strlong carried over, plus numeric-loop
   (generalized `u7a-hot.lua`), table-churn, hostfn-roundtrip (the Redis-call shape —
   exercises the row-96 seam), string/concat-heavy. `-short`-skippable in any test
   leg; it's a CLI.
2. **Baseline + report.** Worktree at `9b89643`; same machine, interleaved runs,
   `benchstat` for {interp now vs 5.1}, {wasm family now vs the note/bench-vs-orig
   2026-09-23 reference}. Honest attribution of expected regressions: `_ENV` globals
   (GETTABUP+K vs the old stringConstants-cached GETGLOBAL), the 5.5 vararg model,
   dual-number asserts on interp; MMBIN slots + D2 drops on wasm.
   Report at `note/u7-perf.md` with an explicit ACCEPTED/WAIVED line per delta —
   "accepted" is a decision, not a mood.
3. **K-operand audit** (the parent plan's "K-operand coverage", done as verification
   not invention): confirm the backend lowers GETFIELD/SETFIELD/GETI/SETI/
   GETTABUP/SETTABUP with their k-bits straight from the stock frontend's K
   operands (no degenerate GETTABLE fallback); fix any site that misses. Byte-exact
   gates + the matrix confirm semantics; the bench quantifies the win if any.
4. **Profile-gated fast paths.** pprof the benches; only the top measured items get
   patches (candidates IF the profile says so: int/int + int/float compare fast path,
   concat int fast path, adjacent-MOVE peephole — MOVEN itself stays out per R4).
   Each lands with the full byte-exact gate set + a recorded micro-bench delta.

### U7d — final-clock soak + closeout

- Archive the interim clock; arm the counting clock on the post-U7c tree (R5: any
  later change to interp/wasm/wazero legs or generators re-arms it again).
- 7 clean nights / 10 M execs, zero new-clock findings; every finding triaged
  (shrink → repro → ledger row → fix or ruled skip) same-day per the standing
  discipline.
- Ledger audit: rows 100+ all test-covered; skip reasons current.
- Parent status header → U7 COMPLETE; CLAUDE.md sweep (luabench command, any engine
  flag changes); memory update.

## 4. Documented rulings / deviations (decided here, ledgered in the chunks)

- **R1 — engine poll semantics.** testdiff engines define `host.deadline_poll` as an
  armed-deadline consult (wazero also writes the flag — both paths raise identical
  text, goldens unchanged); unarmed polls return 0. The alternative (refusing the
  import, or emitting it only in host-package builds) was rejected: the import is the
  contract, and a backend flavor matrix is exactly the divergence surface the ledger
  exists to avoid.
- **R2 — clua55 stays out of the soak set.** The M6e Store cliff (~4k/process) makes
  a fifth leg a wedge risk, not coverage: interp remains the differential anchor;
  oracle conformance stays with the standing corpus gates. Revisit only if a soak
  finding demands the C oracle at scale.
- **R3 — the 5.1 baseline is `9b89643`** (last 5.1-core commit, parent of U0),
  benched from a worktree on the same machine interleaved with HEAD, compared via
  benchstat; the note/bench-vs-orig 2026-09-23 numbers are the recorded reference,
  not the measurement.
- **R4 — MOVEN/GETTABLEKS stay dropped (D2).** No stock 5.5 encoding carries them;
  re-adding fork opcodes violates the zero-fork posture. The parent plan's
  "re-introduce fork fast paths where stock encoding allows" resolves to: K-operand
  audit + profile-gated peepholes only.
- **R5 — soak-clock policy.** The clock that counts toward the exit gate starts after
  the LAST change to any soak-relevant engine path (interp, luawasm backend, rt
  blobs) or the generator. Interim clocks (U7b) exist to find bugs early and never
  count.
- **R6 — the probe metric is ReadMemStats deltas, not the `/gc/pauses:seconds`
  histogram.** The histogram's boundary semantics under an STW-wait were the open
  question in `note/u7a-done.md`; NumGC/PauseTotalNs deltas across the Run window are
  unambiguous, and with the guest spinning for the whole window, an in-window
  completed cycle IS the collision evidence (the STW could only be satisfied through
  the poll's safe point).

## 5. U7 exit gate (consolidated)

1. U7a gate: self-verifying probe green ×20 plain + ×2 `-race` with proven in-window
   STW collisions; host suite plain + `-race` ×2; `go test -short ./...`; wasm-family
   gates green (import satisfied on all in-process hosts).
2. Generator gate: §1.4 construct table fully emitted, coverage-asserted in dumps;
   validity sweep ≥10 seeds; sampled ≥2000-case ×4-engine differential green or
   rowed.
3. Perf gate: `note/u7-perf.md` exists with benchstat tables vs `9b89643` (interp)
   and the 2026-09-23 reference (wasm family), every delta ACCEPTED or WAIVED with a
   reason; K-operand audit recorded; fast paths (if any) byte-exact-gated with
   deltas.
4. Soak gate: counting clock (post-U7c) — 7 clean nights / 10 M execs, zero
   new-clock findings, every finding triaged to a row.
5. Standing gates all green at close: TestWasmFullMatrix/TestWasmErrorSuite/
   TestWasmGluaFull/TestU5* (× the U6f engine set), TestCLua55Conformance,
   determinism gates (TestM6cDeterminism5x et al.), `go test ./host/` plain +
   `-race` ×2, `go test -short ./...`, `_cli-tests` ×{wasmtime, WAZERO=1}.
6. Docs: ledger rows complete with covering tests; parent status header → U7
   COMPLETE; CLAUDE.md current.

## 6. Status log

| Date | Chunk | Note |
|---|---|---|
| 2026-10-10 | — | Plan drafted. U7a code in tree (uncommitted, from the lost session); §1.2 instantiation gap found; soak confirmed stale (2026-09-25, streak 1/7). |
| 2026-10-10 | U7a | COMPLETE. §1.2 wired (wazero: consult + flag write; wasmtime: consult-only); probe self-verifying per R6; ×20 plain (30–37 cycles/run) + ×2 `-race` green; host suite plain + `-race` ×2; matrix/error/glia/U5/U6/U2f/M6d/M6cDet gates green; poll cost +2–4% (0.49→0.50s median warm, artifact +93B); ledger row 100; committed. Next: U7b. |
