/*
** linit55.c — U6d (ledger row 95, plan §U6d): the production flavor's
** library initialization for the 5.5 tree, zero patches to lua55/src/.
**
** lua55_prod.wasm must open only the script-facing surface (base minus
** the loader entries, coroutine, string, table, math, utf8 — the §4.3
** allowlist as compiled code, not a runtime lockdown). Stock
** lua55/src/linit.c cannot serve: its stdlibs table references
** luaopen_package/io/os/debug, whose sources the prod link EXCLUDES
** (build55.sh PROD55_SRC — even dead-referenced, those pulls drag
** wasi-libc's stdio/preopen cluster back in, row 34's archaeology).
** So the prod leg drops linit.c from the source list and links this
** file instead. 5.5's luaL_openlibs is a macro over
** luaL_openselectedlibs (lualib.h:62), so defining that symbol here
** serves every caller with no interposition and no duplicate symbol
** (stock linit.o is simply not in the link).
**
** The base library itself still registers load/loadfile/dofile/warn
** (they live in lbaselib.c's base_funcs, untouchable without a patch);
** this open nils them from the globals table right after base opens —
** the §4.5 killed list's base side. io/os/package/require/debug never
** exist (their sources are out). The observable surface is exactly
** dev-blob-plus-rt_sandbox(1): rt_sandbox on this blob finds all nine
** names absent and returns 0 (the belt-and-suspenders contract).
**
** Open order matches stock linit.c minus the skipped rows (pairs-order
** stability across fresh VMs; the m6cdet isolation gate).
*/

#define LUA_LIB

#include "lua55/src/lprefix.h"

#include <stddef.h>

#include "lua55/src/lua.h"
#include "lua55/src/lualib.h"
#include "lua55/src/lauxlib.h"

static const luaL_Reg stdlibs[] = {
    {LUA_GNAME, luaopen_base},
    {LUA_COLIBNAME, luaopen_coroutine},
    {LUA_MATHLIBNAME, luaopen_math},
    {LUA_STRLIBNAME, luaopen_string},
    {LUA_TABLIBNAME, luaopen_table},
    {LUA_UTF8LIBNAME, luaopen_utf8},
    {NULL, NULL}
};

/* the §4.5 base-side names lbaselib registers but prod must not
   surface (require is defensive — package, its home, never opens) */
static const char *const nil_after_open[] = {
    "dofile", "load", "loadfile", "warn", "require", NULL
};

LUALIB_API void (luaL_openselectedlibs) (lua_State *L, int load, int preload) {
  int mask;
  const luaL_Reg *lib;
  const char *const *p;
  /* stock's loop shape (linit.c): luaL_requiref works without the
     package library — package.loaded is the registry's _LOADED table,
     which luaL_requiref creates lazily; only package.preload (unused
     here) belongs to the lib itself. */
  luaL_getsubtable(L, LUA_REGISTRYINDEX, LUA_PRELOAD_TABLE);
  for (lib = stdlibs, mask = 1; lib->name != NULL; lib++, mask <<= 1) {
    if (load & mask) {
      luaL_requiref(L, lib->name, lib->func, 1);
      lua_pop(L, 1);
    }
    else if (preload & mask) {
      lua_pushcfunction(L, lib->func);
      lua_setfield(L, -2, lib->name);
    }
  }
  lua_pop(L, 1);  /* remove PRELOAD */
  /* nil the loader entries (plain setglobal on an unprotect table at
     open time — no lockdown is armed yet, the host arms it later) */
  for (p = nil_after_open; *p != NULL; p++) {
    lua_pushnil(L);
    lua_setglobal(L, *p);
  }
}
