/*
** luawasm55.c — driver glue for running stock Lua 5.5.1 inside a wasm
** reactor as the U0 differential-testing oracle
** (docs/Upgrade-from-5.2-to-5.5-of-Lua.md §6 U0, decision D6).
**
** Ported from luawasm.c (the 5.1 driver) onto the 5.5 public C API.
** Division of labor is unchanged: this file exposes raw primitives and a
** typed value encoding; all formatting lives in the Go host so the oracle
** and the engines under test produce byte-identical event logs.
**
** Where 5.1 shimmed library functions, 5.5 pins *sources of
** nondeterminism* one level down and lets stock C code run:
**
**   print            -> typed buffer -> host.event (Go formats)
**   time()/clock()   -> symbol preemption: the pinned instant
**                       (2000-01-01 00:00:00 UTC) / 0 — os.time, os.date,
**                       os.difftime, os.clock keep stock 5.5 semantics and
**                       validation ("invalid conversion specifier",
**                       "field 'year' is out-of-bound", ...) because the
**                       suite (files.lua) asserts those messages
**   os.getenv        -> pure map {PATH:/bin:/usr/bin}; os.setenv writes it
**   os.tmpname       -> "testdiff.tmp.N", unique per call, deterministic
**   os.setlocale     -> C-only system (wasi-libc's stub "succeeds" for any
**                       request, flipping locale-conditional tests)
**   math.random      -> NOT shimmed: stock xoshiro256** (math.lua asserts
**                       exact output of seed 1007). Determinism is pinned
**                       at build time: -D'luai_makeseed()=0u' (build55.sh)
**                       fixes the initial seed, randomseed() no-arg, and
**                       the table.sort pivot; lua_newstate(...,0) below
**                       fixes the string-hash seed (deterministic
**                       lua_next order). The interp engine grows the
**                       matching in-process xoshiro at U3.
**
** Everything M7a (host functions) and rt_* ABI related is 5.1-only until
** the ABI v4 port (U2/U6): this driver is pure public C API — no
** lua51/lua55 internals are included, and the blob imports exactly one
** host function (host.event).
**
** Value protocol (tag-first, self-describing, little-endian) — identical
** to luawasm.c so the Go decoders are shared:
**   tag byte: 0 nil, 1 false, 2 true, 3 number (f64), 4 string (u32 len,
**   bytes), 5 table (u32 nentries, then 2n values; PT_MORE sentinel if
**   truncated), 6 function, 7 userdata, 8 thread, 9 lightuserdata,
**   10 cycle, 11 depth-truncated table.
**
** (U0 caveat, resolved at U2 when the protocol gains integer tags: numbers
** cross as f64 only, so integer/float subtype is not observable in event
** logs yet — harmless for the U0 self-run gate, whose pass criterion is
** per-file completion, not cross-engine byte equality.)
**
** Build: runtime/build55.sh (wasi-sdk clang, reactor exec model, EH-based
** setjmp — 5.5's LUAI_THROW is still setjmp/longjmp, ldo.c).
*/

#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "lua55/src/lua.h"
#include "lua55/src/lauxlib.h"
#include "lua55/src/lualib.h"
#ifdef LUA_RT_FLAVOR
#include "rt_abi55.h" /* rt_alloc — the budget allocator (U6b, rows 36/93) */
#endif

/* ---- host imports ---- */

__attribute__((import_module("host"), import_name("event")))
void host_event(int32_t kind, int32_t ptr, int32_t len);

/* U3b: the D5 host-RNG shim is RETIRED. Every engine now runs stock
   xoshiro256** math.random over the build pin luai_makeseed()=0u
   (build55.sh) — the interp's mathlib port and this blob share the
   exact stream, so the host random01/randomint/randomseed imports are
   gone (ledger row 65). */

/* ---- event kinds and value tags ---- */

enum { EVT_PRINT = 1, EVT_GLOBALS = 2 };

enum {
  PT_NIL = 0, PT_FALSE = 1, PT_TRUE = 2, PT_NUMBER = 3, PT_STRING = 4,
  PT_TABLE = 5, PT_FUNCTION = 6, PT_USERDATA = 7, PT_THREAD = 8,
  PT_LIGHTUSERDATA = 9, PT_CYCLE = 10, PT_DEEPTABLE = 11, PT_MORE = 12
};

#define EVBUF_CAP (256 * 1024)
#define ENC_MAX_DEPTH 8
#define ENC_MAX_ENTRIES 512

static char evbuf[EVBUF_CAP];

static int oob(int off, size_t n) { return off < 0 || off + n > EVBUF_CAP; }

static int enc_byte(int off, uint8_t b) {
  if (oob(off, 1)) return -1;
  evbuf[off] = (char)b;
  return off + 1;
}

static int enc_u32(int off, uint32_t v) {
  if (oob(off, 4)) return -1;
  memcpy(evbuf + off, &v, 4);
  return off + 4;
}

static int enc_f64(int off, double v) {
  if (oob(off, 8)) return -1;
  memcpy(evbuf + off, &v, 8);
  return off + 8;
}

static int enc_str(int off, const char *s, size_t len) {
  if (oob(off, 5 + len)) return -1;
  off = enc_u32(off, (uint32_t)len);
  memcpy(evbuf + off, s, len);
  return off + (int)len;
}

/* encode the value at stack index idx. tables expand when expand_tables,
   with cycle detection via the ancestor pointer stack. */
static int enc_value(lua_State *L, int idx, int off, int expand, int depth,
                     const void *ancestors[], int nanc) {
  int t = lua_type(L, idx);
  switch (t) {
  case LUA_TNIL: return enc_byte(off, PT_NIL);
  case LUA_TBOOLEAN: return enc_byte(off, lua_toboolean(L, idx) ? PT_TRUE : PT_FALSE);
  case LUA_TNUMBER: {
    off = enc_byte(off, PT_NUMBER);
    if (off < 0) return -1;
    return enc_f64(off, lua_tonumber(L, idx));
  }
  case LUA_TSTRING: {
    size_t len;
    const char *s = lua_tolstring(L, idx, &len);
    off = enc_byte(off, PT_STRING);
    if (off < 0) return -1;
    return enc_str(off, s, len);
  }
  case LUA_TTABLE: {
    if (!expand) return enc_byte(off, PT_TABLE);
    const void *p = lua_topointer(L, idx);
    for (int i = 0; i < nanc; i++)
      if (ancestors[i] == p) return enc_byte(off, PT_CYCLE);
    if (depth >= ENC_MAX_DEPTH) return enc_byte(off, PT_DEEPTABLE);
    if (nanc >= ENC_MAX_DEPTH) return enc_byte(off, PT_DEEPTABLE);
    /* first pass: count entries (lua_next normalizes the array part too) */
    int base = lua_gettop(L);
    int n = 0, truncated = 0;
    lua_pushvalue(L, idx); /* table copy for lua_next */
    lua_pushnil(L);
    while (lua_next(L, -2) != 0) {
      if (n < ENC_MAX_ENTRIES) n++; else truncated = 1;
      lua_pop(L, 1);
    }
    lua_settop(L, base);
    off = enc_byte(off, PT_TABLE);
    if (off < 0) return -1;
    off = enc_u32(off, (uint32_t)n);
    if (off < 0) return -1;
    if (truncated) {
      off = enc_byte(off, PT_MORE);
      if (off < 0) return -1;
    }
    ancestors[nanc] = p;
    base = lua_gettop(L);
    lua_pushvalue(L, idx);
    lua_pushnil(L);
    int written = 0;
    while (lua_next(L, -2) != 0 && written < n) {
      /* key at -2, value at -1 */
      off = enc_value(L, -2, off, 1, depth + 1, ancestors, nanc + 1);
      if (off < 0) { lua_settop(L, base); return -1; }
      off = enc_value(L, -1, off, 1, depth + 1, ancestors, nanc + 1);
      if (off < 0) { lua_settop(L, base); return -1; }
      lua_pop(L, 1);
      written++;
    }
    lua_settop(L, base);
    return off;
  }
  case LUA_TFUNCTION: return enc_byte(off, PT_FUNCTION);
  case LUA_TUSERDATA: return enc_byte(off, PT_USERDATA);
  case LUA_TTHREAD: return enc_byte(off, PT_THREAD);
  case LUA_TLIGHTUSERDATA: return enc_byte(off, PT_LIGHTUSERDATA);
  }
  return enc_byte(off, PT_NIL);
}

static int g_print(lua_State *L) {
  int n = lua_gettop(L);
  int off = enc_u32(0, (uint32_t)n); /* arg count header */
  if (off < 0) return 0;
  for (int i = 1; i <= n; i++) {
    off = enc_value(L, i, off, 0, 0, NULL, 0); /* print never expands tables */
    if (off < 0) return 0; /* oversized print: dropped, host sees header only */
  }
  host_event(EVT_PRINT, (int32_t)(size_t)evbuf, off);
  return 0;
}

static void emit_globals(lua_State *L) {
  const void *anc[ENC_MAX_DEPTH + 1];
  lua_getglobal(L, "_G");
  int off = enc_value(L, -1, 0, 1, 0, anc, 0);
  lua_pop(L, 1);
  if (off > 0) host_event(EVT_GLOBALS, (int32_t)(size_t)evbuf, off);
}

/* ---- deterministic environment map for os.getenv/os.setenv ----
** (dev engines only — U6d: prod never opens os, so the shims compile
** out rather than sit as unused statics referencing the preemption
** stubs' signatures) */
#ifndef LUAWASM_PROD
typedef struct { char *k, *v; } envpair;
static envpair envmap[8];
static int envmap_n = 0;

static const char *env_lookup(const char *k) {
  for (int i = 0; i < envmap_n; i++)
    if (strcmp(envmap[i].k, k) == 0) return envmap[i].v;
  return NULL;
}

static void env_store(const char *k, const char *v) {
  for (int i = 0; i < envmap_n; i++) {
    if (strcmp(envmap[i].k, k) == 0) {
      free(envmap[i].v);
      envmap[i].v = strdup(v);
      return;
    }
  }
  if (envmap_n < 8) {
    envmap[envmap_n].k = strdup(k);
    envmap[envmap_n].v = strdup(v);
    envmap_n++;
  }
}

static int g_getenv(lua_State *L) {
  const char *v = env_lookup(luaL_checkstring(L, 1));
  if (v == NULL) lua_pushnil(L);
  else lua_pushstring(L, v);
  return 1;
}

/* os.setenv: golua55 extension (upgrade plan Appendix B, D12) */
static int g_setenv(lua_State *L) {
  const char *k = luaL_checkstring(L, 1);
  const char *v = luaL_optstring(L, 2, NULL);
  if (v != NULL) env_store(k, v);
  lua_pushboolean(L, 1);
  return 1;
}

static int g_setlocale(lua_State *L) {
  /* WASI has no locales; wasi-libc's setlocale stub "succeeds" for any
     request, flipping the suite's locale-conditional tests. Behave like
     a C-only system: "C"/nil succeed, everything else fails. (Carried
     over from the 5.1 driver finding.) */
  const char *req = luaL_optstring(L, 1, "C");
  if (strcmp(req, "C") == 0 || strcmp(req, "c") == 0) {
    lua_pushliteral(L, "C");
  } else {
    lua_pushnil(L);
  }
  return 1;
}
#endif /* !LUAWASM_PROD (env + os shims) */

/* ---- the pinned clock: stock os.time/date/clock over a fixed instant --
**
** Preempting the libc symbols (same link-time trick as the prod flavor's
** fopen override in build.sh) keeps ALL of loslib.c stock — including the
** 5.5 error texts files.lua asserts — while pinning every observable
** value. wasi-libc's time() would ask the host clock (nondeterministic).
*/
static time_t pinned_time_t = 946684800; /* 2000-01-01 00:00:00 UTC */

time_t time(time_t *t) {
  if (t != NULL) *t = pinned_time_t;
  return pinned_time_t;
}

clock_t clock(void) { return 0; }

/* os.tmpname: unique per call (files.lua renames tmpname() A to tmpname()
   B — a constant name would make that a rename onto itself); deterministic
   per module instance. Dev engines only (prod: os never opens). */
#ifndef LUAWASM_PROD
static int tmpname_counter = 0;
static int g_tmpname(lua_State *L) {
  char buf[64];
  snprintf(buf, sizeof buf, "testdiff.tmp.%d", ++tmpname_counter);
  lua_pushstring(L, buf);
  return 1;
}
#endif /* !LUAWASM_PROD (g_tmpname) */

/* WASI gaps: tmpfile/system are not provided. system returns -1 (only
   main.lua — skipped — exercises os.execute). tmpfile is backed by a
   unique deterministic name in the preopen FS; the engine removes
   testdiff.tmp* after the run. Prod compiles both out (LUAWASM_PROD:
   tmpfile's fopen and the shims below reference the stdio cluster). */
#ifndef LUAWASM_PROD
static int tmpfile_counter = 0;
FILE *tmpfile(void) {
  char name[64];
  snprintf(name, sizeof name, "testdiff.tmp.file%d", ++tmpfile_counter);
  return fopen(name, "w+");
}
int system(const char *cmd) { (void)cmd; return -1; }
#else
/* ---- U6d prod preemption (row 95, v3's row-34 technique) ----
**
** lua55_prod.wasm must import nothing but host.{event,wasm_dispatch}.
** The 5.5 tree still COMPILES the file paths even with io/os/package/
** debug excluded and the loader entries nil'd at open: lbaselib's
** base_funcs table (data-referenced by luaopen_base) retains
** luaB_load/loadfile/dofile, whose lauxlib luaL_loadfilex chain
** references fopen/getc/fread/ferror/fclose — and every one of those
** pulls wasi-libc stdio objects that root fd_* imports in .init_array
** data or ops-pointer tables that function-level GC cannot drop.
** Defining the symbols HERE stops the archive pull at link time; the
** stubs are never called at runtime (no script-facing caller remains:
** the loader globals are nil, and the driver loads only via
** luaL_loadbuffer) and the stub sections themselves are GC'd.
**
** print/warn/panic writes are cheaper still: build55.sh -D's
** lua_writestring/lua_writeline/lua_writestringerror to nothing for the
** prod leg, so lbaselib/lauxlib reference no fwrite/fflush/fprintf at
** all (g_print carries print over host.event on every flavor).
**
** musl's lazy stdio-exit chain and the FILE structs stay preempted as
** in v3: snprintf's vfprintf references __stderr_FILE & co on error
** branches prod never executes — zero byte arrays stand in so the real
** objects (whose initializers embed __stdio_write/__stdio_close ops
** pointers) stay out of the link.
*/
FILE *fopen(const char *path, const char *mode) {
  (void)path; (void)mode; return NULL;
}
FILE *freopen(const char *path, const char *mode, FILE *f) {
  (void)path; (void)mode; (void)f; return NULL;
}
int fclose(FILE *f) { (void)f; return 0; }
int getc(FILE *f) { (void)f; return -1; }
size_t fread(void *buf, size_t sz, size_t n, FILE *f) {
  (void)buf; (void)sz; (void)n; (void)f; return 0;
}
int ferror(FILE *f) { (void)f; return 0; }
void __stdio_exit_needed(void) {}
/* struct _IO_FILE is opaque in the public header; generous aligned byte
   arrays stand in for the FILE objects (link-time symbol preemption
   only — libc indexes into them on error branches prod never runs). */
char __stderr_FILE[256] __attribute__((aligned(8)));
char __stdin_FILE[256] __attribute__((aligned(8)));
char __stdout_FILE[256] __attribute__((aligned(8)));
#endif /* LUAWASM_PROD */

/* ---- host I/O staging buffers ---- */

static char inbuf[1 << 20];
static char namebuf[512];

int32_t linbuf(void) { return (int32_t)(size_t)inbuf; }
int32_t lnamebuf(void) { return (int32_t)(size_t)namebuf; }

/* ---- exported driver API ---- */

static void install_shims(lua_State *L) {
#ifndef LUAWASM_PROD
  env_store("PATH", "/bin:/usr/bin");
#endif

  lua_pushcfunction(L, g_print);
  lua_setglobal(L, "print");

  /* U3b: no math.random shim on any flavor — stock xoshiro everywhere
     (the D5 host-RNG shim retired; see the host-imports note above). */

#ifndef LUAWASM_PROD
  /* U6d: the dev engines' deterministic os.* surface. Prod never opens
     the os lib (linit55.c) — setfield into a nil `os' would raise
     through the unprotected driver, and the shims below would drag the
     preempted stdio stubs back into the call graph. */
  lua_getglobal(L, "os");
  lua_pushcfunction(L, g_tmpname);  lua_setfield(L, -2, "tmpname");
  lua_pushcfunction(L, g_getenv);   lua_setfield(L, -2, "getenv");
  lua_pushcfunction(L, g_setenv);   lua_setfield(L, -2, "setenv");
  lua_pushcfunction(L, g_setlocale); lua_setfield(L, -2, "setlocale");
  lua_pop(L, 1);
#endif
}

/* the default allocator (mirrors lauxlib.c's luaL_alloc: realloc+free;
   free(NULL) is valid, realloc(ptr,0) frees). The ORACLE flavor keeps
   it — the oracle must not grow a budget (TestCLua55Conformance memory
   posture). The RT flavor (LUA_RT_FLAVOR, build55.sh) switches to the
   budget-tracking rt_alloc (rt_abi55.c, U6b rows 36/93): the host arms
   it with rt_set_memlimit BEFORE this entry so the whole VM lifetime
   counts; a refusal rides stock LUA_ERRMEM as the bare pcall-catchable
   "not enough memory". Both flavors link this file; only build55.sh's
   RT leg defines the macro, so the oracle blob preprocessor-identical. */
static void *d_alloc(void *ud, void *ptr, size_t osize, size_t nsize) {
  (void)ud; (void)osize;
  if (nsize == 0) {
    free(ptr);
    return NULL;
  }
  return realloc(ptr, nsize);
}

static lua_State *g_state; /* for lglobals after the entry returns */

int32_t lnewstate(void) {
  /* Fixed non-zero seed pins g->seed (string hashing → deterministic
     lua_next order) WITHOUT hitting the seed-0 worst case: hash_search
     (ltable.c, the '#' border walk) randomizes its probe ladder with
     g->seed, and seed 0 makes every probe a pure power of two — exactly
     the key set of nextvar.lua's "attack on table length" table, so '#'
     returns 2^61 and the suite's `for i = 1, #t` never terminates. Any
     fixed non-zero seed walks off that ladder immediately (42 = the
     harness's canonical seed). The math random seed is pinned separately
     at build time (-Dluai_makeseed, build55.sh). */
#ifdef LUA_RT_FLAVOR
  lua_State *L = lua_newstate(rt_alloc, NULL, 42);
#else
  lua_State *L = lua_newstate(d_alloc, NULL, 42);
#endif
  if (L == NULL) return 0;
  luaL_openlibs(L);
  install_shims(L);
  g_state = L;
  return (int32_t)(size_t)L;
}

void lglobals(void) {
  if (g_state != NULL) emit_globals(g_state);
}

void lclose(int32_t p) {
  lua_State *L = (lua_State *)(size_t)p;
  if (L != NULL) {
    lua_close(L);
    if (L == g_state) g_state = NULL;
  }
}

static char errdata[8192];
static int errlen = 0;

static void capture_error(lua_State *L) {
  errlen = 0;
  const char *m = lua_tostring(L, -1);
  if (m == NULL) m = "(non-string error object)";
  size_t n = strlen(m);
  if (n > sizeof errdata - 1) n = sizeof errdata - 1;
  memcpy(errdata, m, n);
  errlen = (int)n;
}

struct dostring_args {
  lua_State *L;
  const char *s;
  size_t len;
  const char *cn;
  int status;
};

static void dostring_body(void *ud) {
  struct dostring_args *a = ud;
  const char *s = a->s;
  size_t len = a->len;
  /* skip a leading '#' line, like the standalone interpreter's loader
     (both '#!/path/to/lua' and '# comment' forms) */
  if (len >= 1 && s[0] == '#') {
    while (len > 0 && s[0] != '\n') { s++; len--; }
    if (len > 0) { s++; len--; }
  }
  /* file-loader semantics: '@' + name (short_src and error positions then
     match "db.lua:28" instead of [string "db.lua"], like the standalone
     runner) */
  char cnbuf[600];
  const char *cn = a->cn;
  if (cn[0] != '@' && cn[0] != '=') {
    snprintf(cnbuf, sizeof cnbuf, "@%s", cn);
    cn = cnbuf;
  }
  a->status = luaL_loadbuffer(a->L, s, len, cn);
  if (a->status != 0) {
    capture_error(a->L);
    lua_pop(a->L, 1);
    return;
  }
  /* the standalone runner exposes the script name via arg; suite drivers
     read it */
  {
    lua_createtable(a->L, 0, 2);
    lua_pushstring(a->L, a->cn);
    lua_rawseti(a->L, -2, 0);
    lua_setglobal(a->L, "arg");
  }
  a->status = lua_pcall(a->L, 0, LUA_MULTRET, 0);
  if (a->status != 0) {
    capture_error(a->L);
    lua_pop(a->L, 1);
  }
}

/* ldostring loads+runs a chunk. Returns 0 on success, 1 on runtime error,
   2 on load/syntax error. On error the message is available via lerrlen/
   lerrcopy. (The SJLJ build calls the body directly — native EH handles
   the longjmp; see build55.sh.) */
int32_t ldostring(int32_t p, int32_t src, int32_t len, int32_t chunkname_ptr,
                  int32_t want_globals) {
  struct dostring_args a;
  a.L = (lua_State *)(size_t)p;
  a.s = (const char *)(size_t)src;
  a.len = (size_t)len;
  a.cn = (const char *)(size_t)chunkname_ptr;
  a.status = 0;
  errlen = 0;
  dostring_body(&a);
  if (a.status != 0) return a.status == LUA_ERRRUN ? 1 : 2;
  if (want_globals) emit_globals(a.L);
  return 0;
}

int32_t lerrlen(void) { return errlen; }

int32_t lerrcopy(int32_t dst, int32_t cap) {
  int32_t n = errlen < cap ? errlen : cap;
  if (n > 0) memcpy((void *)(size_t)dst, errdata, (size_t)n);
  return n;
}
