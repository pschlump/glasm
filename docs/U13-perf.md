# U13 — the insert-path arm (2026-10-25)

U12's `armSets` handled only existing-key overwrites: every new-key
insert still paid the full decline chain (`rt_sets_probe` miss →
protected `rt_settable` — two crossings, one of them a try_table enter).
U13 arms `luaH_newkey`'s main-position-free branch: at the walk's chain
end (key absent), the arm inserts in-wasm exactly as stock
`insertkey` would. As with every arm, this is a strict refinement —
each decline falls through to the unchanged chain, so the differential
gates stay the oracle (no ledger row: no engine diverged).

## The arm

At chain end, in order, screens and stores verbatim from
`ltable.c`/`lvm.c`:

- **dummy** — `flags & BITDUMMY (0x40)` declines (insertkey's
  `|| isdummy(t)`) — the first insert into a fresh table rehashes
  through C, which is what allocates the node array in the first place.
- **nil value** — `luaH_newkey`'s `!ttisnil(value)`: setting `t[k]=nil`
  on an absent key is a no-op through C and declines here too.
- **fasttm `__newindex`** — `luaV_finishset` consults it *before*
  inserting; a metatable without bit 1 (TM_NEWINDEX) cached absent
  declines.
- **barrier** — the key is collectable by construction, so any black
  table declines (runtime tables are white with the frozen GC).
- **main position free** — `!isempty(gval(mp))` declines: colliding
  main positions (getfreepos/occupant-move) and full tables (rehash)
  stay with C.
- Fire: `setnodekey` (the kcell's value word + tag byte, wholesale),
  `setobj2t` (value word + `tt_` byte), and the caller's
  `invalidateTMcache` (`flags &= ~maskflags`). `mp->next` is untouched,
  matching insertkey — a deleted mid-chain head is reused in place.

Int-key inserts stay declined **by design**: stock's rehash promotes
near-array integer keys into the array part, and arming the hash-part
insert would diverge table shape (pairs order) from stock.

## Fresh-table flags, pinned while triaging

`luaH_new` sets `flags = maskflags = 0x3F` (all TM bits = "no
metamethod fields"); the size-0 `setnodevector` ORs in BITDUMMY → a
fresh table reads `0x7F`. A real node array clears only the dummy bit
(`0x3F`), and **any successful new-key insert then runs
`invalidateTMcache` (`flags &= ~0x3F`) leaving `flags == 0x00`** — the
plain-C path produces the same byte, so `0x00` after inserts is stock
state, not corruption.

## The bug the stepwise dumps caught

The first cut of the mp screen was **inverted**: it branched to decline
on `eqz` — when the main position was *empty* — the exact opposite of
insertkey (insert on free, fall back when occupied). The corrupted
signature: every insert whose mp held a live key did a raw `setnodekey`
over the occupant — key count never grows, no rehash ever runs — so a
100-insert loop ended holding only `k100` (`nil nil 100`) and a 3-key
literal ended a 1-node table with only `c`. Stepwise zzdump runs (1
insert: correct `{b→1}`; 2 inserts: 1-node `{a→2}`, b overwritten in
place, `lsizenode` still 0) pinned it to the screen before any store.
The fix is one operand: decline when the novariant tag is *nonzero*
(`I32And().BrIf(0)` — VNIL/VEMPTY/VABSTKEY are all variant 0). After
it, the colliding pair (b, a — both hash to bucket 0) declines and C
rehashes to a proper 2-node table.

Two triage lessons worth keeping: a screen that never runs on the
smoke shapes passes them (revive and `__newindex` cases never reach an
occupied mp), and a counter at the insert region measures *entry*, not
*fire* — "999/1000" was consistent with the inverted screen the whole
time.

## Numbers (2026-10-25, interleaved A/B vs the 134f57e worktree, warm
wazero-prod pooled-VM lane, best-of-7 ×3 reps — medians vary <2%)

| micro | U12 | U13 | Δ |
|---|---|---|---|
| insert/delete churn (300× [1000 str-key inserts + 1000 nil-deletes]) | 144.1ms | 29.4ms | **−80% (4.9×)** |
| 1000 fresh inserts (concat keys) | 273.7µs | 268.0µs | — (concat-dominated) |
| existing-key `t.k1=j; s=s+t.k1` | 48.1ms | 49.1ms | — (unchanged arm) |
| in-bounds `t[5]=j; s=s+t[5]` | 50.9ms | 51.2ms | — |
| control (no table ops) | 25.7ms | 25.1ms | — |
| h_tablechurn (luabench warm) | 21.45ms | 21.50ms | — (see below) |

`h_tablechurn` does not move, and that is now *measured*, not assumed:
the 4.9× insert micro proves the arm fires at volume, so the churn
lane's remaining time sits in what the arm deliberately leaves to C —
`pairs`/TFORCALL (not armed), the rehashes themselves (malloc +
reinsert), and key interning. Those are the next candidates, in that
order of expected value.

## Gates

Backend-only (no blob change, no re-pin): `TestWasmFullMatrix`
1501/1501, ErrorSuite, GluaFull, U1/U2/U2f×2/U5/U9 gates, U6 sandbox
corpus, root `-short ./...`, `_cli-tests` (wasmtime + wazero legs),
host plain + `-race -count=2` — all green (`GODEBUG=asyncpreemptoff=1`,
`-timeout 40m`; exit codes checked directly).
