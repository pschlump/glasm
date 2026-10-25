# U14 — the pairs/next + ipairs iterator arms (2026-10-25)

U13 left `pairs`/TFORCALL as the measured top cost of `h_tablechurn`:
every loop iteration crossed wasm→C (`rt_call_addr` → `luaD_call` →
`luaB_next`/`ipairsaux` → staging). U14 moves the whole iteration
step in-wasm: `armNext` lowers `luaB_next` + `luaH_next` (findindex +
both scan loops) and `armIpairs` lowers `ipairsaux`, both inline at
OP_TFORCALL ahead of the gmatch probe. Strict refinements as always —
every decline falls through to the unchanged chain, so the gates stay
the oracle.

## The arms

**Pin-by-construction.** `next` and `ipairsaux` are LIGHT C functions
(`lua_pushcclosure` with n=0 → `setfvalue`: no closure object, the
cell's value word IS the function identity). In wasm32 C a function
pointer is a table index — the pin is just that index, captured once
per process at `rt_set_state` under `rt_run` protection
(`pair_pin_body`): `next` straight from the pristine globals table,
`ipairsaux` by calling stock `ipairs({})` once (it is `static` in
lbaselib.c — its f value is the only stable identity). The getters
return the ADDRESS of the C statics (`rt_next_pin_addr` /
`rt_ipaux_pin_addr`, two new rt exports) and the module init
`i32.load`s the pin value into two mutable globals. A shadowed,
replaced, or custom iterator compares unequal — the arms decline.

**armNext** (stock `luaB_next` + `luaH_next` verbatim):

- screens: callee tag `LUA_VLCF (0x16)` + value == the pin, state is
  a table. `luaB_next`'s checktype raise belongs to C.
- findindex: nil control → i=0; an integer key with
  `(u64)(k-1) <u asize` resumes AT k (the slot after the key's own
  k−1); a short string walks the bucket chain `node +
  24*(hash & (hashN-1))` matching the KEY ONLY — `getgeneric`'s
  deadok=1: a DELETED key's node still anchors the resume, value
  liveness never checked; found → i = asize + nodeIdx + 1. Integer
  keys beyond the array stay C's (hashint); any other key type
  declines; an absent key declines (C raises "invalid key to 'next'").
- scan: array part first (tag row at `array+4+i`, `&0x0F==0` =
  empty), then the hash part SLOT-SEQUENTIAL (order is observable —
  chains are irrelevant), `sizenode = 1<<lsizenode` even for dummy
  tables (the shared dummynode's `VEMPTY` value never hits as live).
- hits stage key/value directly into `R[A+3..A+2+C]` (poscall's
  nil-fill for the missing tail); exhausted stages all nils —
  `luaB_next`'s one nil result, poscall-adjusted.

**armIpairs** (`ipairsaux` verbatim): j = k+1; `(u64)k <u asize`
screens the bounds; in-bounds and live → `(j, t[j])` staged; in-bounds
and empty → all nils (the loop exits); beyond `asize` declines (hash
integers and `__index` metamethods are C's). One extra screen the
audit forced: for an EMPTY in-bounds slot, stock `lua_geti` consults
`__index` — the metamethod may return non-nil and the loop continues,
so the arm's nil-staging would diverge. `flags` cannot screen this
(`invalidateTMcache` zeroes them on any insert — a churned plain
table reads 0x00), so ANY metatable on the state declines,
conservative; plain tables are the hot path and arm as before.

## The two bugs the triage caught

**No arm-body block — declines skipped the world.** The arms ran
directly inside emitTForcall's A/B wrapper with no block of their
own, so every screen's `br_if 0` targeted B — whose end is PAST the
remaining arms and the gmatch probe. A decline therefore landed at
the TFORLOOP convergence with NOTHING staged: the stale control cell
(non-nil — it still held the iterator) kept the loop alive and the
body ran on stale state (`v` = the state table — "attempt to perform
arithmetic on a table value"), and the string-found resume `br`d out
the same way, never advancing the control — an infinite loop on every
hash-part pairs. Fix: each arm opens its own body block; declines
fall out of the arm into the next arm / the probe, hits `br` to A.
Every branch depth is now counted against the real stack
`[arm frames…] N(0) B(1) A(2)`.

**`I32Load(2)` — the argument is the OFFSET, not the alignment.**
The init's pin loads emitted `i32.load 2 2`: align 2, offset 2 —
reading bytes `addr+2..addr+6`, i.e. the top half of `pin_next`
spliced with the bottom half of `pin_ipairsaux`. The pin globals held
garbage, the screens never passed once (stepwise gDbg counters: 0
passes — while the lanes ran green through the stock path), and the
only symptom was m-ipairs not moving at all. The probe that pinned
it: read the rt statics from Go (`0x2b`/`0x29` — wasm32 function
pointers as table indices), then re-check the emitted init bytes.
Fix: `I32Load(0)`.

A third catch came from auditing the semantics against stock rather
than the tests: armIpairs's empty-slot staging diverges when the
state has an `__index` metamethod (stock `lua_geti` consults it —
the loop can continue with the metamethod's value). Fixed by the
any-metatable screen above; verified with a `setmetatable` ipairs
case against clua55 (byte-identical through the C decline).

## Numbers (2026-10-25, interleaved A/B vs the 134f57e worktree, warm
wazero-prod pooled-VM lane, best-of-7 ×3 reps — medians vary <2%)

| micro | U13 | U14 | Δ |
|---|---|---|---|
| pairs, 500 str keys × 300 (`for k,v in pairs(t)`) | 69.9ms | 4.00ms | **17.5× (−94%)** |
| ipairs, 1000 els × 300 (`for i,v in ipairs(a)`) | 131.6ms | 5.09ms | **25.9× (−96%)** |
| h_tablechurn (luabench wasm warm pooled) | 22.6ms | 9.34ms | **2.4× (−59%)** — now below interp (10.7ms) |
| existing-key str get/set (m-str) | 48.7ms | 48.6ms | — |
| in-bounds int get/set (m-int) | 50.9ms | 50.9ms | — |
| control (m-glob) | 25.5ms | 25.5ms | — |

h_tablechurn was the measured target U13 named (its remaining time
sat in pairs/TFORCALL by measurement, not assumption); the insert and
get/set lanes are untouched by construction.

## Gates

`TestWasmFullMatrix` 1501/1501, ErrorSuite, GluaFull, U1/U2/U2f×2/U5/U9
gates, U6 sandbox corpus, root `-short ./...`, `_cli-tests` (wasmtime +
wazero legs), host plain + `-race -count=2` — all green
(`GODEBUG=asyncpreemptoff=1`, `-timeout 40m`; exit codes checked
directly). Smoke battery vs clua55: byte-identical including the
hash-iteration order line (the interp order difference is the known
seed delta, pre-existing). Blob re-pin: the two new rt exports
(prod 292,865 B, `d0857866…`, drift-locked both sides).
