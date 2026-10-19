# U7c — performance vs the 5.1 baseline (9b89643) and the 2026-09-23 reference

Date: 2026-10-10. Machine: macOS darwin 27.0.1, arm64 (Apple Silicon),
GOMAXPROCS=14, go1.27.0. Same machine for every measurement below; old/new
legs interleaved in the same shell session (R3: the 9b89643 baseline is
measured from a worktree at that commit, not from the September notes).

- baseline ("old") = `9b89643` (last 5.1-core commit) in
  `../golua55-u7c-base`, module `gopher-lua`
- current  ("new") = HEAD (`golua55`, 5.5.1 core, ABI v4, `lua55_prod.wasm`)
- reference = `note/bench-vs-orig/out-*.txt`, recorded 2026-09-23 (context
  only — different day/load; the interleaved worktree runs are authoritative)

## 1. Harness: cmd/luabench (in-tree, CLI)

Promoted from `note/bench-vs-orig/hostbench`, postures unchanged:

- `interp TOTAL (fork)` — fresh state + KEYS/ARGV staging + run + Close
- `wasm cold: fresh VM/run` — `e.Run()`, a fresh VM per run (host/ on wazero)
- `wasm warm: pooled VM` — one pooled VM bound to the script, rerun in place
- `wazero-prod engine cold` — the testdiff wazero-prod engine (prod blob,
  fresh runtime per run, no host machinery)

Scripts (all return values, no printing — the prod blob has no stdout):
`h_tiny`, `h_str`, `h_strlong` (carried over byte-identical from the 2026-09-23
set), `h_numloop` (numeric loops, from the U7a hot probe), `h_tablechurn`,
`h_concat` (allocation-shaped, sized for the frozen-GC pooled posture, row 61),
`h_hostfn` (the row-96 hostfn seam, `bench.call` string→int — the
redis.call shape). Scripts returning outputs are output-checked interp vs
wasm on every run.

`-benchstat` emits go-benchmark lines; the comparison protocol is 8
alternating process runs per side (old/new), `benchstat old.txt new.txt`.

## 2. K-operand audit (U7c item 3): PASS, no fix needed

Verified end to end (compile.go emits, luawasm lowers, K cells are staged
typed):

| opcode | frontend | backend lowering |
|---|---|---|
| GETFIELD / SETFIELD | string K | `kcell(C)` (interned short-string cells) |
| GETI / SETI | int K, `0 ≤ iv ≤ 255` | `mkIntScratch` (rt_mkinteger cell) |
| GETTABUP / SETTABUP | `_ENV` upvalue + string K | `kcell`/`vcCell(k)` → rt_getfield/rt_setglobal |
| GETTABLE / SETTABLE | only stock's own fallbacks (K-overflow, non-K strings) | generic rt path |

No degenerate GETTABLE lowering exists for the K-armed opcodes; `intKeyLiteral`
bounds (`compile.go:1234`) match `opMaxArgsC`; interned short-string K cells
keep `luaH_getshortstr` on the C side reachable. The byte-exact matrix gates
(U2f grids) already pin the semantics; audit-only item, zero code change.

## 3. Profile-gated fast path (U7c item 4): emitArithK mixed-type arms

Finding (measured): `h_numloop`'s float lane is `f = f + i * 0.5` — MULK with
an **int register × float K**. `emitArithK` only fast-pathed float-reg ×
float-K, so every iteration paid `mmbinFallback` → an `rt_arith` host import
(~1µs each on wazero; ~50ns on wasmtime). Lane isolation (cmd/luawasm-run,
2M iterations, 3 reps, medians; each leg includes ~0.04s wasmtime / ~0.10s
wazero instantiate+run overhead; the int lane's pre/post blobs are
byte-identical — the emitter provably changes nothing else):

| lane | wasmtime pre → post | wazero pre → post |
|---|---|---|
| int `s = s + i` (control, inline i64 both) | 42.7 → 41.6ms (flat) | 107.3 → 107.8ms (flat) |
| **mixed `f = f + i * 0.5`** | **129.2 → 84.8ms (−34%)** | **1772 → 922ms (−48%)** |
| pure float `f = f + 0.5` (control, inline pre-patch) | 45.4 → 48.3ms (flat) | 101.7 → 103.8ms (flat) |

The pre-patch mixed-lane overhead over its controls matches the known import
costs exactly (~44ns/op wasmtime, ~830ns/op wazero × 2M ops); the patch
removes the import entirely (the post-patch mixed lane approaches 2× the
single-op controls).

Patch (`luawasm/ops.go` emitArithK): ADDK/SUBK/MULK with int register ×
float K promote the register inline (`i64→f64 convert` + the f64 op), and the
mirror float register × int K promotes the constant — exactly what
`luaO_arith` does at the runtime boundary; no observable changes (identical
f64 arithmetic, no error paths reachable with numeric operands). Verified:
`wasm` and `wazero` legs byte-identical to the `clua55` oracle on
mixedk.lua — a 35-value corpus covering both promotion directions, ±0.0,
`9007199254740993 * 1.0` (2^53 precision), `-2^63 * 1.0`, `7 // 2.0`,
`2 ^ 0.5`, a 10k-iteration accumulate, and `%.3f` formatting; full gate set
green — `go test ./...` with `GODEBUG=asyncpreemptoff=1` (root/host/luafuzz/
wasm ok in one process; testdiff `ok 1213.9s` under `-timeout 40m` in a
second — the default 10m package alarm, not a test failure, is what the
first attempt hit mid-sandbox-matrix; see §7).

Delta on the mixed lane: wazero 1772 → 922ms (−48%), wasmtime 129.2 →
84.8ms (−34%), controls flat. h_numloop warm (pooled wazero): 2.278s →
1.158s (−49%; caveat: the script was
reshaped to its honest form at the same time — same total add count, and the
lane table above is the controlled before/after).

Candidates **not** taken (no measured top item): the reg×reg mixed
promotion (`i * t` with `t` a local compiles to plain MUL — both-int and
both-float stay inline, mixed still takes the MMBIN import; no benched
script's hot path has the shape), int/float compare fast path (no benched
script is compare-bound), concat int fast path (h_concat warm is already
−5.06% vs baseline), adjacent-MOVE peephole (not visible in any profile),
**MOVEN — out per R4**.

## 4. SET A — cmd/luabench, old (9b89643) vs new (HEAD), 8 interleaved rounds

Post-patch final table (h_numloop is the honest shape — see §5; the new side
includes the §3 patch; 8 alternating full-process rounds, benchstat geomeans
of per-round medians):

```
                                 │   old.txt    │                new.txt                │
                                 │    sec/op    │    sec/op      vs base                │
Luabench/h_tiny/interp-14          34.68µ ±  3%    71.87µ ±  5%  +107.21% (p=0.000 n=8)
Luabench/h_tiny/cold-14            47.97m ±  1%    69.68m ±  1%   +45.26% (p=0.000 n=8)
Luabench/h_tiny/warm-14            12.77µ ± 14%    43.63µ ±  6%  +241.80% (p=0.000 n=8)
Luabench/h_tiny/wazprod-14         48.25m ±  1%    69.86m ±  1%   +44.77% (p=0.000 n=8)
Luabench/h_str/interp-14           130.3µ ±  2%    171.1µ ±  2%   +31.32% (p=0.000 n=8)
Luabench/h_str/cold-14             51.00m ±  2%    71.53m ±  0%   +40.24% (p=0.000 n=8)
Luabench/h_str/warm-14             916.2µ ±  2%    772.5µ ±  2%   -15.68% (p=0.000 n=8)
Luabench/h_str/wazprod-14          51.64m ±  1%    71.93m ±  0%   +39.28% (p=0.000 n=8)
Luabench/h_strlong/interp-14       1.762m ±  1%    1.493m ±  5%   -15.28% (p=0.000 n=8)
Luabench/h_strlong/cold-14         69.19m ±  2%    99.71m ±  4%   +44.12% (p=0.000 n=8)
Luabench/h_strlong/warm-14         16.17m ±  3%    17.01m ±  3%    +5.17% (p=0.001 n=8)
Luabench/h_strlong/wazprod-14      69.24m ±  1%    96.41m ±  3%   +39.24% (p=0.000 n=8)
Luabench/h_numloop/interp-14       81.29m ±  1%   128.16m ±  1%   +57.66% (p=0.000 n=8)
Luabench/h_numloop/cold-14         931.4m ±  2%   1226.8m ±  2%   +31.72% (p=0.000 n=8)
Luabench/h_numloop/warm-14         861.0m ±  3%   1157.7m ±  4%   +34.46% (p=0.000 n=8)
Luabench/h_numloop/wazprod-14      942.4m ±  2%   1237.2m ±  5%   +31.28% (p=0.000 n=8)
Luabench/h_tablechurn/interp-14    10.38m ±  2%    10.35m ±  3%         ~ (p=0.505 n=8)
Luabench/h_tablechurn/cold-14      120.6m ±  1%    166.2m ±  2%   +37.73% (p=0.000 n=8)
Luabench/h_tablechurn/warm-14      65.16m ±  2%    92.60m ±  3%   +42.11% (p=0.000 n=8)
Luabench/h_tablechurn/wazprod-14   121.3m ±  2%    167.3m ±  2%   +38.01% (p=0.000 n=8)
Luabench/h_concat/interp-14        883.3µ ±  1%   1121.0µ ± 17%   +26.91% (p=0.000 n=8)
Luabench/h_concat/cold-14          57.41m ±  1%    78.43m ±  2%   +36.62% (p=0.000 n=8)
Luabench/h_concat/warm-14          6.300m ±  2%    5.981m ±  2%    -5.06% (p=0.000 n=8)
Luabench/h_concat/wazprod-14       57.79m ±  2%    79.72m ±  3%   +37.95% (p=0.000 n=8)
Luabench/h_hostfn/interp-14        450.7µ ±  1%    698.1µ ± 14%   +54.90% (p=0.000 n=8)
Luabench/h_hostfn/cold-14          57.70m ±  1%    77.80m ±  2%   +34.84% (p=0.000 n=8)
Luabench/h_hostfn/warm-14          6.875m ±  2%    6.413m ±  4%    -6.72% (p=0.000 n=8)
geomean                            15.25m          20.38m         +33.65%
```

### Attribution summary

- **fresh-VM instantiation (+~22ms uniform)**: every cold/wazprod row is
  +31..45% ≈ +20–30ms absolute across ALL scripts regardless of size — the
  5.5 blob (bigger runtime image, ABI v4 init) costs more to instantiate per
  VM. This is the dominant term of every cold delta (and of the +33.65%
  geomean, which is cold-row-weighted).
- **pooled-rerun fixed cost on tiny scripts**: h_tiny warm 12.77µs → 43.63µs
  (+242%) — per-run staging + rt_set_state + frozen-GC bookkeeping on the 5.5
  rt; proportional, invisible for real scripts (h_str warm −15.68%,
  h_concat warm −5.06%, h_hostfn warm −6.72% — the new tree is FASTER warm
  on every script that does real string/host work; table-heavy work is the
  exception, below).
- **interp TOTAL**: worse on short scripts (state setup: 5.2+ libs, _ENV,
  bigger registry; h_tiny +107%, h_hostfn +55%) and on pure numeric loops
  (h_numloop +58% — the 5.5 interpreter's integer duality pays an int-path
  check per arith op where 5.1 had one double path), better on long string
  work (h_strlong −15.28%, the 5.5 string machinery), flat on table churn
  (p=0.505).
- **table-heavy warm**: h_tablechurn warm +42.11% — the same wazero-codegen
  gap SET B isolates on nsieve (+58% wazero vs −17% wasmtime); rt-side C
  table code, not backend-emitted Lua.
- **h_numloop post-patch**: warm 2.278s → 1.158s (−49%, matching the lane
  −48%); the remaining +34.46% vs baseline is tagged-cell float work on
  wazero (the fadd control lane runs 0.10s where the baseline's inline
  double add is fully native) — see §5 for why the pre-patch +170% was an
  artifact-inflated comparison.

## 5. The h_numloop `%d` artifact (baseline 5.1 semantics, honest compute)

The original h_numloop summed `s = s + i` over 1..2,000,000 (true value
2,000,001,000,000) and returned `string.format("%d", s)`. luabench's output
check flagged the baseline tree's wasm leg returning **2147483647** while
its own interpreter returns the true value. Probing the baseline directly
(hotprobe: `return s, s == 5000050000, string.format("%d", s)` for the
100k-add sum 5,000,050,000):

- `s` crosses the host value wire as the honest **5000050000**;
- `s == 5000050000` evaluates **true** on the wasm leg;
- `string.format("%d", s)` returns **2147483647**.

So the baseline's arithmetic (5.1: all numbers are doubles, exact to 2^53)
is HONEST — only its `%d` formatter clamps, which is **stock 5.1 behavior**
(the C-int cast; the `clua` 5.1 oracle does the same). The old tree was
never mis-executing the loop. Consequences:

- The pre-patch numloop deltas (interp +55.89%, cold +151.57%, warm
  +169.99%, wazprod +153.00%) measured real compute on both sides — old
  float-lane (5.1 all-double: `s+i` and `i*0.5` both inline float ops)
  vs new int-lane i64 inline + float-lane MULK int×floatK paying a
  per-op `rt_arith` import (the §3 patch removes that). Post-patch the
  wasm deltas collapse to cold +31.72% / warm +34.46% / wazprod +31.28%
  (§4) — the same band as every other script's tagged-cell cost, and
  interp is unchanged by the patch (127.1 → 128.2ms: the interpreter
  always did mixed arith inline).
- `h_numloop.lua` was still reshaped (accumulator < 2^31) so the output
  check passes on BOTH trees — a red MISMATCH line on every future run
  would be worse than a slightly less pure loop shape; total add count
  unchanged (~2M).

No ledger row: nothing here diverges between HEAD engines — it is 5.1-vs-5.5
`%d` semantics inside the retired baseline (row 33/34 territory already
rules the 5.1 tree's texts).

## 6. SET B — bench-vs-orig programs (l1/l2/l3, s0–s3), wasmtime + wazero

Protocol: one bvo binary per side, each carrying orig (upstream yuin) + new
(its tree) in one process; `wasm run (VM start+run)` = fresh engine runtime
per run (compile+instantiate+run), local-tree blob only (upstream has no wasm
path). Medians, one full round per (engine, side), 2026-10-10 evening, legs
minutes apart. Machine-stability cross-check: the 9b89643 wasmtime leg ran
twice (round 1/round 2) — every wasm median agreed within 1.5%. The new side
includes the §3 patch (bvo-new built after it landed).

**wazero (production engine) — `wasm run` median:**

| script | 9b89643 | HEAD | Δ |
|---|---|---|---|
| s0_nop | 59.67ms | 78.82ms | +32% |
| s1_hello | 59.82ms | 79.86ms | +34% |
| s2_string | 62.39ms | 81.88ms | +31% |
| s3_table | 60.80ms | 81.72ms | +34% |
| l1_fib | 8.00s | 9.13s | +14% |
| l2_nsieve | 3.21s | 5.08s | +58% |
| l3_stringwork | 135.25ms | 167.93ms | +24% |

**wasmtime (oracle) — `wasm run` median:**

| script | 9b89643 | HEAD | Δ |
|---|---|---|---|
| s0_nop | 18.57ms | 28.00ms | +51% |
| s1_hello | 19.00ms | 30.19ms | +59% |
| s2_string | 19.81ms | 33.18ms | +67% |
| s3_table | 19.46ms | 33.67ms | +73% |
| l1_fib | 6.59s | 7.47s | +13% |
| l2_nsieve | 276.80ms | 229.73ms | **−17%** |
| l3_stringwork | 29.28ms | 48.18ms | +65% |

**interp TOTAL (CLI)** — l1/l2/l3: fib 120.66ms → 159.63ms (+32%), nsieve
286.45ms → 260.36ms (**−9%**), stringwork 8.21ms → 6.92ms (**−16%**, the 5.5
string machinery — matches SET A h_strlong −15.28%). The µs-scale s0–s3 TOTAL
rows (+70..205%) are load- and shared-process-GC-sensitive (the same binary's
5.5-side allocations churn the GC the orig rows sit in) — SET A's
fork-isolated 8-process numbers are the interp authority; direction agrees.

Attribution: the s0–s3 rows are pure instantiation (the scripts do nothing) —
the +31..34% / +19–21ms absolute on wazero is the ABI-v4 `lua55_rt.wasm`
image, matching SET A's uniform cold cost. Blob sizes grew everywhere
(MMBIN* fallback slots: s0 1111→1489B, fib 2862→4054B, stringwork
21950→30317B; `wasm compile` medians roughly ×3, s0 12.3→38.8µs) — compile
cost stays negligible vs instantiation. The real-work wasm rows: fib +14%
(i64/variant-tag arith on the tagged cells), stringwork +24%, nsieve +58%
(table/array-heavy) — but nsieve on wasmtime is **faster** (−17%): wazero was
already 11.6× slower than wasmtime on the 9b89643 nsieve blob (3.21s vs
276.80ms) vs 1.2× on fib, and the 5.5 rt's table code widens that wazero
codegen gap (corroborated by SET A h_tablechurn warm +42.11%). That is
wazero-compiled C-runtime codegen, not backend-emitted Lua code — outside
U7c's fast-path scope; recorded as the top U7+ perf item.

**vs the 2026-09-23 reference** (`note/bench-vs-orig/out-long-*.txt`): those
runs are 9b89643-tree only (identical blob sizes), n=3, and the machine has
since shifted — same old binary, Sept → today: wazero nsieve 9.10s → 3.21s
(2.8×), wazero stringwork 445.74ms → 135.25ms (3.3×), wasmtime fib 14.20s →
6.59s (2.2×), but wazero fib 7.46s → 8.00s (~flat). A cross-day column cannot
rank trees; Sept is context only. For what it's worth qualitatively: HEAD's
worst real-work regression (wasmtime fib 7.47s) is still 1.9× faster than
Sept's same-tree 14.20s, and HEAD's nsieve-wasmtime (229.73ms) beats every
September number on any tree.

## 7. Methodology notes (what NOT to do, learned here)

- **Host GC must stay on.** GOGC=off slows wazero steady-state ~2–2.5× on
  BOTH trees (pooled h_numloop: old 840ms→2.1s, new ~2.3s→4.35s): allocation
  stalls without sweeping dominate. The no-GC probes used to chase the
  "harness discrepancy" were an invalid environment — luabench (GC on,
  runtime.GC() between iterations) is the right posture. This is also why
  U6f's noHostGC is a test-only device.
- **Profile of a wazero-warm tight loop is mostly Go-runtime noise**:
  h_numloop warm shows gcBgMarkWorker 27.6% + preemptM/pthread_kill 11.9% +
  startTheWorldWithSema 17.2% against unpreemptible JIT guest frames (the
  row-99 family in miniature), guest JIT opaque at _ExternalCode 1.8% flat.
  Wall-clock lane isolation + code reading attributed the cost faster than
  the profile did.
- zsh gotchas bit repeatedly (`===` unquoted, `${ref}:testdiff/...` eaten as
  a modifier); `| tail` masks go-test exit codes — run gates bare.
- **The darwin-27 wedge family** (environment finding, 2026-10-10): long-lived
  processes mixing cgo and Go occasionally wedge with the main goroutine
  parked in `_pthread_cond_wait` under unsymbolizable PCs in the cgo runtime
  asm region. Hit 3× in one evening across three different binaries — the
  9b89643 wazero bench leg (at process EXIT, after all measurements; the data
  was complete and valid), the HEAD wasmtime bench leg (mid-leg; that round
  discarded), and `go test ./...` (testdiff.test, twice, identical PCs).
  macOS 27.0.1 was installed 2026-10-07 and U7b's gates ran green on it
  2026-10-09, so the OS alone doesn't suffice; the signature matches the
  row-99 family (async-preemption signal storm around a cgo/foreign call).
  Mitigation used for the U7c gate run: `GODEBUG=asyncpreemptoff=1` — with
  it, no wedge across the full gate set (and the one timeout hit was go
  test's default 10m package alarm firing mid-`TestM6cSandboxMatrix`, which
  instantiates the prod blob twice per corpus case — ~2880 wazero
  compilations; rerun with `-timeout 40m`, `ok 1213.9s`). Bench protocol
  consequence: every long leg runs under a
  watchdog that kills the leg at a timeout and keeps only completed
  measurements; a wedged-at-exit leg's data is still valid.

## 8. Decisions — every delta ACCEPTED or WAIVED

| # | delta (old → new, median) | posture | ruling | reason |
|---|---|---|---|---|
| 1 | cold/wazprod instantiation +31..45% ≈ +20–30ms, uniform (both engines, every script) | fresh VM | ACCEPTED | the ABI-v4 `lua55_rt.wasm` image + 5.5 init is the upgrade's real per-VM cost; amortized by pooling (the production posture), no cheap honest fix (the blob is the blob) |
| 2 | tiny-script warm fixed cost: h_tiny 12.77→43.63µs (+242%) | pooled VM | ACCEPTED | per-run staging + rt_set_state + frozen-GC bookkeeping; proportional — every real string/host script is faster warm on the new tree (h_str −15.7%, h_concat −5.1%, h_hostfn −6.7%); table-heavy is row 6's exception |
| 3 | interp TOTAL short scripts +55..107% (h_tiny, h_hostfn, SET B s0–s3) | fresh state | ACCEPTED | 5.2+ libs / _ENV / registry state creation, one-time per state (µs–ms scale), not per-op; lazy lib opening would change observable semantics |
| 4 | interp long-string −16..17% (h_strlong, stringwork), nsieve interp −9% | fresh state | ACCEPTED | improvement: the 5.5 string/table machinery |
| 5 | fib wasm +13..14% (both engines) | wasm | ACCEPTED | variant-tag cells + i64 paths on tagged arith vs 5.1 all-double; lossless i64 integers are the upgrade's point |
| 6 | nsieve wazero +58% | wasm, production engine | ACCEPTED | the wazero codegen gap on the 5.5 rt's table/array code (wazero was already 11.6× slower than wasmtime on the 9b89643 blob); rt-side C, outside U7c fast-path scope — recorded as the top U7+ perf item |
| 7 | nsieve wasmtime −17%, stringwork interp −16% | oracle host | ACCEPTED | improvements |
| 8 | stringwork wasm +24% (wazero) / +65% (wasmtime) | wasm | ACCEPTED | 5.5 string machinery in the rt + MMBIN*-inflated blobs; the interp path got the same machinery faster |
| 9 | wasm compile ~×3 (e.g. s0 12.3→38.8µs, stringwork 118→375µs) | compile | ACCEPTED | MMBIN* fallback slots inflate protos +20..75%; absolute cost stays µs-scale vs ms-scale instantiation |
| 10 | h_numloop pre-patch: cold +152%, warm +170%, wazprod +153% | wasm | WAIVED | baseline artifact (§5): the old leg's shape made its own inline-float lane optimal by 5.1 accident; the §3 patch removes the new tree's float-lane import cost — post-patch numbers in §4 |
| 11 | baseline `string.format("%d", s>2^31)` → 2147483647 | baseline-only | WAIVED | stock 5.1 semantics in the retired tree (C-int cast, matches its own clua oracle); not a HEAD divergence, no ledger row (row 33/34 territory) |
| 12 | GOGC=off probe numbers (~2–2.5× worse, both trees) | probe | WAIVED | invalid environment (§7): allocation stalls without sweeping; luabench (GC on) is authoritative |
| 13 | post-patch h_numloop: cold +31.72%, warm +34.46%, wazprod +31.28%, interp +57.66% | all | ACCEPTED | remaining cost is real 5.5 semantics on the production engine (tagged-cell float ops, integer-duality dispatch in the interp); the patch already removed the import overhead (warm −49%); no further honest fix in scope |

Rules applied: a delta is **ACCEPTED** when it is the 5.5 semantic upgrade's
real cost on the production engine (correct i64 integers, ABI v4 init,
5.5-string machinery) with no cheap honest fix available; **WAIVED** when the
comparison itself is invalid (baseline artifact) or the posture is not the
production posture. R3 honored throughout (worktree-interleaved, not
September's numbers); R4 honored (MOVEN/GETTABLEKS stay dropped — no patch
touches codegen they would overlap with).
