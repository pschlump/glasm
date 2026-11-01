/*
** embedbench55.c — native stock C Lua 5.5.1 EMBEDDING-API harness for
** cmd/embedcmp (docs/Embedded-EVAL-Perf.md): the shape an application
** actually embeds Lua in (Redis EVAL) — a short script loaded once and
** called over and over with per-run KEYS/ARGV tables. Unlike the lua
** CLI leg of cmd/perfcmp (which re-loadfiles per run), this is the C
** API an app links against.
**
** Build (runtime/build-embedbench55.sh; same flags as the documented
** comparison target in docs/Perf-vs-C-Lua.md, minus the tarball lua.c —
** the vendored tree ships no lua.c, so there is no conflicting main;
** link every core .c under runtime/lua55/src): see the build script.
**
** Usage: embedbench <reuse|fresh> <script.lua> <runs> [warmup=2]
**
**   reuse  ONE lua_State: openlibs + register bench.call + ONE
**          luaL_loadbuffer (kept via a registry ref); then per run —
**          all inside the timed region — stage KEYS/ARGV, lua_pcall
**          the ref, collect results. The Redis-EVAL shape.
**   fresh  the naive embedder: newstate+openlibs+register+load+stage+
**          pcall+close per run, all timed.
**
** The staged KEYS/ARGV (g_keys/g_argv below) MUST mirror the constants
** in cmd/embedcmp/main.go — the harness's RET cross-check is the
** tripwire that keeps the two definitions in sync.
**
** Line protocol on stdout, printed once at exit (nothing per run —
** stdout writes would dominate µs-scale runs):
**
**   PERFRUN <median-ns> <min-ns>   per-run wall (mach_absolute_time on
**                                  darwin — see now_ns), warmup
**                                  excluded, one-decimal precision
**   LOADNS <ns>                    reuse mode: the one luaL_loadbuffer
**   RET <results of the last run>  space-joined, newlines escaped \n
**   HEAPKB <live-kb>               after a forced lua_gc(LUA_GCCOLLECT)
**                                  — LIVE heap, deliberately, so it is
**                                  never compared against glasm's
**                                  frozen-GC monotonic total (UsedBytes)
**
** Lua errors go to stderr with exit 1 (the parent fails loudly).
** Stock GC/seed defaults throughout — that is what an embedded app gets.
*/

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#ifdef __APPLE__
#include <mach/mach_time.h>
#else
#include <time.h>
#endif

#include "lua.h"
#include "lauxlib.h"
#include "lualib.h"

/* ------------------------------------------------------------------ args */

/* Per-run KEYS/ARGV staged as STRING tables before every call (Redis
** EVAL semantics; numbers go through tonumber() in the scripts). These
** mirror stagedKeys/stagedArgv in cmd/embedcmp/main.go exactly. */
static const char *g_keys[] = {
    "key:1", "key:2", "key:3", "key:4", "key:5",
    "key:6", "key:7", "key:8", "key:9", "key:10",
};
static const int g_nkeys = (int)(sizeof(g_keys) / sizeof(g_keys[0]));

static const char *g_argv[] = {"17", "4", "alpha beta", "0"};
static const int g_nargv = (int)(sizeof(g_argv) / sizeof(g_argv[0]));

/* ------------------------------------------------------------- hostfn */

/* bench_call — the redis.call analog, same contract as luabench's
** regBenchCall and cmd/embedcmp's Go benchCall: one string down,
** its byte-length integer back. */
static int bench_call(lua_State *L) {
  const char *s = luaL_checkstring(L, 1);
  lua_pushinteger(L, (lua_Integer)strlen(s));
  return 1;
}

static void register_bench(lua_State *L) {
  lua_newtable(L);
  lua_pushcfunction(L, bench_call);
  lua_setfield(L, -2, "call");
  lua_setglobal(L, "bench");
}

/* --------------------------------------------------------------- timing */

/* now_ns — monotonic nanoseconds. On darwin this MUST be
** mach_absolute_time, not clock_gettime(CLOCK_MONOTONIC): on this
** machine (darwin 27) the clock_gettime path ticks at ~1 µs
** granularity in practice, and e_arg-class runs (< 1 µs) collapse to
** bimodal 0/1000ns samples. mach_absolute_time is also what Go's
** time.Now uses, so both harnesses measure against the same source. */
static double now_ns(void) {
#ifdef __APPLE__
  static mach_timebase_info_data_t tb;
  if (tb.denom == 0) {
    mach_timebase_info(&tb);
  }
  uint64_t t = mach_absolute_time();
  return (double)((t * (uint64_t)tb.numer) / (uint64_t)tb.denom);
#else
  struct timespec ts;
  clock_gettime(CLOCK_MONOTONIC, &ts);
  return (double)ts.tv_sec * 1e9 + (double)ts.tv_nsec;
#endif
}

/* ------------------------------------------------------------- the runs */

static void stage_args(lua_State *L) {
  lua_createtable(L, g_nkeys, 0);
  for (int i = 0; i < g_nkeys; i++) {
    lua_pushstring(L, g_keys[i]);
    lua_rawseti(L, -2, i + 1);
  }
  lua_setglobal(L, "KEYS");
  lua_createtable(L, g_nargv, 0);
  for (int i = 0; i < g_nargv; i++) {
    lua_pushstring(L, g_argv[i]);
    lua_rawseti(L, -2, i + 1);
  }
  lua_setglobal(L, "ARGV");
}

/* collect_ret joins the MULTRET results on top of the stack with single
** spaces (luaL_tolstring semantics = tostring), escaping newlines as
** literal \n (warm55.lua does the same, so the parent's normRet folds
** both spellings), then pops everything. */
static void collect_ret(lua_State *L, char *buf, size_t cap) {
  int nres = lua_gettop(L);
  size_t off = 0;
  for (int i = 1; i <= nres; i++) {
    size_t len;
    const char *s = luaL_tolstring(L, i, &len); /* pushes the conversion */
    if (off > 0 && off < cap) buf[off++] = ' ';
    for (size_t j = 0; j < len; j++) {
      if (off + 3 >= cap) {
        fprintf(stderr, "embedbench: RET buffer overflow (cap %zu)\n", cap);
        exit(1);
      }
      if (s[j] == '\n') {
        buf[off++] = '\\';
        buf[off++] = 'n';
      } else {
        buf[off++] = s[j];
      }
    }
    lua_pop(L, 1); /* pop the conversion, keep the results */
  }
  buf[off] = '\0';
  lua_pop(L, nres);
}

/* live_heap forces a full collection and returns the live heap in KB —
** the comparable figure against an app's steady-state footprint. */
static double live_heap(lua_State *L) {
  lua_gc(L, LUA_GCCOLLECT);
  return (double)lua_gc(L, LUA_GCCOUNT) + (double)lua_gc(L, LUA_GCCOUNTB) / 1024.0;
}

static char *read_file(const char *path, size_t *len) {
  FILE *f = fopen(path, "rb");
  if (f == NULL) {
    perror(path);
    exit(1);
  }
  if (fseek(f, 0, SEEK_END) != 0) {
    perror("fseek");
    exit(1);
  }
  long sz = ftell(f);
  if (sz < 0 || fseek(f, 0, SEEK_SET) != 0) {
    perror("fseek");
    exit(1);
  }
  char *buf = malloc((size_t)sz + 1);
  if (buf == NULL || fread(buf, 1, (size_t)sz, f) != (size_t)sz) {
    fprintf(stderr, "embedbench: short read of %s\n", path);
    exit(1);
  }
  buf[sz] = '\0';
  fclose(f);
  *len = (size_t)sz;
  return buf;
}

/* one_run performs one full staged call.
**
**   reuse=1: L/ref are the persistent state + registry ref; the run is
**            rawgeti → stage_args → pcall → collect.
**   reuse=0: a complete naive cycle — newstate, openlibs, register,
**            load (re-parse per run), stage, pcall, collect, close.
**
** Results of every run are collected into ret (the parent's cross-check
** wants them and the Go side converts results inside its timed region
** too). Dies loudly on any Lua error. */
static void one_run(lua_State **Lp, int ref, const char *src, size_t srclen,
                    int reuse, char *ret, size_t cap) {
  lua_State *L;
  if (reuse) {
    L = *Lp;
    lua_rawgeti(L, LUA_REGISTRYINDEX, ref);
  } else {
    L = luaL_newstate();
    luaL_openlibs(L);
    register_bench(L);
    if (luaL_loadbuffer(L, src, srclen, "@script") != LUA_OK) {
      fprintf(stderr, "embedbench: load error: %s\n", lua_tolstring(L, -1, NULL));
      exit(1);
    }
    *Lp = L;
  }
  stage_args(L);
  if (lua_pcall(L, 0, LUA_MULTRET, 0) != LUA_OK) {
    fprintf(stderr, "embedbench: script error: %s\n", lua_tolstring(L, -1, NULL));
    exit(1);
  }
  collect_ret(L, ret, cap);
  if (!reuse) {
    lua_close(L);
    *Lp = NULL;
  }
}

static int cmp_double(const void *a, const void *b) {
  double x = *(const double *)a, y = *(const double *)b;
  return (x > y) - (x < y);
}

int main(int argc, char **argv) {
  if (argc < 4 || argc > 5) {
    fprintf(stderr, "usage: %s <reuse|fresh> <script.lua> <runs> [warmup=2]\n", argv[0]);
    return 2;
  }
  const char *mode = argv[1];
  int reuse;
  if (strcmp(mode, "reuse") == 0) {
    reuse = 1;
  } else if (strcmp(mode, "fresh") == 0) {
    reuse = 0;
  } else {
    fprintf(stderr, "embedbench: unknown mode %s\n", mode);
    return 2;
  }
  const char *path = argv[2];
  int runs = atoi(argv[3]);
  int warmup = (argc == 5) ? atoi(argv[4]) : 2;
  if (runs < 1 || warmup < 0) {
    fprintf(stderr, "embedbench: bad runs/warmup\n");
    return 2;
  }

  size_t srclen;
  char *src = read_file(path, &srclen);

  double *ns = calloc((size_t)runs, sizeof(double));
  if (ns == NULL) {
    fprintf(stderr, "embedbench: oom\n");
    return 1;
  }
  char ret[65536];
  ret[0] = '\0';
  double load_ns = -1.0;

  lua_State *L = NULL;
  int ref = LUA_NOREF;
  if (reuse) {
    L = luaL_newstate();
    luaL_openlibs(L);
    register_bench(L);
    double t0 = now_ns();
    if (luaL_loadbuffer(L, src, srclen, "@script") != LUA_OK) {
      fprintf(stderr, "embedbench: load error: %s\n", lua_tolstring(L, -1, NULL));
      return 1;
    }
    load_ns = now_ns() - t0;
    ref = luaL_ref(L, LUA_REGISTRYINDEX);
  }

  for (int i = 0; i < warmup; i++) {
    one_run(&L, ref, src, srclen, reuse, ret, sizeof ret);
  }
  for (int i = 0; i < runs; i++) {
    double t0 = now_ns();
    one_run(&L, ref, src, srclen, reuse, ret, sizeof ret);
    ns[i] = now_ns() - t0;
  }

  /* live heap, OUTSIDE any timed region: reuse still holds the state;
** fresh needs one extra untimed cycle (its states are closed). */
  double heap = -1.0;
  if (reuse) {
    heap = live_heap(L);
    lua_close(L);
  } else {
    /* one extra untimed naive cycle whose state we KEEP long enough to
** read the live heap (one_run closes fresh states behind itself) */
    lua_State *M = luaL_newstate();
    luaL_openlibs(M);
    register_bench(M);
    if (luaL_loadbuffer(M, src, srclen, "@script") != LUA_OK) {
      fprintf(stderr, "embedbench: load error: %s\n", lua_tolstring(M, -1, NULL));
      return 1;
    }
    stage_args(M);
    if (lua_pcall(M, 0, LUA_MULTRET, 0) != LUA_OK) {
      fprintf(stderr, "embedbench: script error: %s\n", lua_tolstring(M, -1, NULL));
      return 1;
    }
    lua_pop(M, lua_gettop(M));
    heap = live_heap(M);
    lua_close(M);
  }

  qsort(ns, (size_t)runs, sizeof(double), cmp_double);
  /* sub-ns one-decimal precision: e_arg-class chunks run < 500 ns per
** call in C, and an integer-ns median would round to 0 (the parent
** then reads the leg as missing) */
  printf("PERFRUN %.1f %.1f\n", ns[runs / 2], ns[0]);
  if (load_ns >= 0.0) {
    printf("LOADNS %.1f\n", load_ns);
  }
  printf("RET %s\n", ret);
  printf("HEAPKB %.1f\n", heap);

  free(ns);
  free(src);
  return 0;
}
