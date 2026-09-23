Vendored stock Lua 5.5.1 (lua-5.5.1.tar.gz, saved under
note/conv-from-5.2-to-5.5/; U0 of docs/Upgrade-from-5.2-to-5.5-of-Lua.md,
decision D9).

Unmodified except: `lua.c`, `luac.c`, `src/Makefile` and `lua.hpp` are not
vendored (the standalone interpreter/compiler drivers — the blob is a reactor
driven by runtime/luawasm55.c, and the conformance oracle never needs them).

## Patch list

**lua55#1 — `src/loadlib.c` `readable()`: reject the empty filename.**
POSIX `fopen("")` fails with ENOENT on every native platform; wasi-libc
resolves the empty path to the preopen root directory and SUCCEEDS, so
`package.searchpath` wrongly "finds" the empty template (the official suite
builds `"a;b;;"`-style paths ending in an empty template on purpose,
attrib.lua:20-35). Two-line guard restoring native semantics.

That is the whole list so far. The 5.1 tree's 7-file patch list
(docs/M5-summary.md) is the precedent; the rt_* ABI v4 port (U2/U6) will
extend this list.

Determinism is pinned from the BUILD FLAGS (runtime/build55.sh), not by
patching this tree:

  -D'luai_makeseed()=0u'   pins luaL_makeseed (math.random initial seed,
                           randomseed() no-arg seed, table.sort pivot
                           randomization) — default is time+address based.
  lua_newstate(alloc,NULL,42) in luawasm55.c pins g->seed (string hashing
                           → deterministic lua_next order; also '#''s
                           hash_search probe ladder). MUST be non-zero:
                           seed 0 probes pure powers of two, which is
                           exactly nextvar.lua's "attack on table length"
                           key set — '#' then answers 2^61 and the suite
                           never terminates.
