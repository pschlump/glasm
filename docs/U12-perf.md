# U12 — in-wasm table arms (2026-10-25)

U11 moved every hit-path table op into fast C probes (`rt_geti_probe`,
`rt_seti_probe`, `rt_gets_probe`, `rt_sets_probe`), but a hit still cost
one wasm→C import crossing. U12 removes the crossing for the hit cases
that matter: the backend walks the stock 5.5.1 (wasm32) Table/Node/
TString layouts directly in wasm — an **arm** per opcode, emitted in
front of the existing probe→protected chain. An arm is a strict
refinement of its probe's hit set: every condition the probe checks is
checked in-wasm first, and any decline falls through to the exact chain
that ran before, so semantics are unchanged by construction and the
differential gates stay the oracle (no ledger row: no engine diverged).

## What landed

**The arms (`luawasm/tablearms.go`):**
- `armGeti` / `armSeti` — OP_GETI/SETI and the int-key arm of
  GETTABLE/SETTABLE: `luaH_fastgeti` verbatim — `u = k-1` as u64 (key 0
  wraps and misses by itself), `u <u asize`, tag row at `array+4+u`
  (byte granular, the inverted array part), value at
  `array-8*(u+1)`. Seti takes fastseti's empty-slot `checknoTM`
  (`mt == NULL` or `mt->flags & (1<<1)` — __newindex cached absent);
  out-of-bounds keys decline (hash-part ints stay with the probes).
- `armGets` / `armSets` — OP_GETFIELD/SETFIELD, the str-key arm of
  GETTABLE/SETTABLE, and GETTABUP/SETTABUP after their staging call:
  `luaH_Hgetshortstr`'s chain walk verbatim — bucket
  `key->hash & (sizenode-1)`, then `keyisshrstr(n) &&
  keystrval(n)==key` with `gnext(n)` advance (NODE units, 0 = end).
  Dummynode-safe without an isdummy check: lsizenode 0 → node[0], whose
  DEADKEY key_tt never matches and whose next is 0. armSets is
  restricted to psetshortstr's **existing-key overwrite** (empty-value
  slots decline — insert/rehash/ABSTKEY stays with the probes).
- **Screens before any write:** the barrier's conservative superset
  (`iscollectable(v) && isblack(t)` — declining more only costs speed)
  and the **protection generation**: `g_prot_gen`, a sticky C global
  that `prot_add` bumps and nothing lowers, polled via a wasm global
  cached at init (`rt_protgen_addr`, the gDeadline idiom). First
  `WithGlobalsProtection`/staging use freezes both write arms to
  decline-forever — writes to protected tables must raise through the
  U6c seam, never bypass it.

**The rt surface (`runtime/rt_abi55.{c,h}`, `build55.sh`):**
- `rt_protgen_addr()` — `&g_prot_gen`.
- `rt_layout_num(which)` — offsetof/sizeof for the 15 layout facts the
  arms bake in as i32 immediates (asize/array/node/metatable/marked/
  lsizenode, sizeof(Node), key_tt/next/key_val/tt_, TString.hash,
  sizeof(TValue), getArrTag row, sizeof(Value)). Three C-side
  `_Static_assert`s pin the same numbers at build; the Go-side mirror
  (`luawasm.LayoutPins`) is checked against the live blob by
  `host.TestTableArmLayoutPins` — a vendored-tree layout change now
  fails loudly instead of reading garbage.

Blobs re-pinned: prod `lua55_prod.wasm` 292,483 B,
`c1515b2e368bb5fe7a39e8e88b56846b096507b40272a4cf5d16c875296ac49f`
(host/blob.go + testdiff/m6c_test.go — the m6c drift lock caught the
pin on cue).

## The trap: wasm binops are rhs-then-lhs

The mask `sizenode(t)-1 = (1<<lsizenode)-1` was emitted as
`[lsizenode, 1] shl` — which computes `lsizenode << 1`. With lsizenode
0 (every fresh/dummy table) the mask is `(0<<1)-1 = -1` = 0xFFFFFFFF:
the raw hash becomes the bucket, `node + 24*hash` lands far OOB, and
`print(t.name)` on a just-built table trapped at
`0x9a3530 + 24*0x30ad8f7 = 0x499e8c58`. Lsizenode ≥ 1 tables got a
*wrong but small* mask — the walk starts in the wrong bucket, misses,
falls back to the probe, and the result stays **correct**: 1501/1501
matrix cases green even with the bug, and only the exact combination
constructor→read trapped. Found by instrumenting the arm with five
mutable globals (set at each step, read from Go after the trap —
wasmtime keeps memory and globals readable past a trap): the mask
global read 0xFFFFFFFF against a dumped lsizenode byte of 0. The fix
pushes the 1 first. Lesson: **the emitter's fluent chains read left to
right but wasm pops rhs-then-lhs — every non-commutative binop needs
the left operand pushed first.** (Same trap class as the never-emitted
builder bugs of U11e: builders nobody exercised were wrong.)

## Numbers (2026-10-25, interleaved A/B vs 3988dbf worktree, warm
wazero-prod pooled-VM lane, 3M-iteration loops, best-of-7 ×3 reps —
medians vary <2% across reps)

| micro | U11f | U12 | Δ |
|---|---|---|---|
| existing-key `t.k1=j; s=s+t.k1` (SETFIELD+GETFIELD) | 67.3ms | 48.4ms | **−28%** |
| in-bounds `t[5]=j; s=s+t[5]` (SETI+GETI) | 56.9ms | 50.8ms | **−11%** |
| control (no table ops) | 25.1ms | 25.2ms | — |

The h_* lanes do **not** move (h_nsieve 36.8→35.8ms, h_tablechurn
21.8→21.7ms, within noise) — and that is expected, not a failure of
the arms: h_nsieve writes `t[0]` (hash-part int, declined by design)
and grows past asize (inserts, declined); h_tablechurn is dominated by
new-key inserts and `pairs` (TFORCALL, not armed). The lanes that would
show it are lookup-heavy existing-key workloads. Follow-on candidates:
the insert path (space check + rehash) is where churn time actually
sits, and `pairs`/NEXT remains fully protected.

## Gates

`TestWasmFullMatrix` 1501/1501, ErrorSuite, GluaFull, CLua55
conformance, U1/U2/U2f×2/U5/U9×2 gates, U6 sandbox corpus, root
`-short ./...`, `_cli-tests` (wasmtime + wazero legs), host plain +
`-race -count=2` — all green (`GODEBUG=asyncpreemptoff=1`,
`-timeout 40m`; exit codes checked directly, never through a pipe).
