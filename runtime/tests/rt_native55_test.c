/*
** runtime/tests/rt_native55_test.c — native unit tests for ABI v4
** (rt_abi55.c on the stock 5.5 tree; U2d gate). The same C sources
** compiled for the host, exercising the ABI surface directly: integer
** fidelity end-to-end (the reason ABI v4 exists), the stock-5.5 arith/
** compare/concat/forprep semantics now delegated to the tree's own
** luaO_arith/luaV_* family, and the zero-patch CClosure dispatch seam
** with a programmable wasm_dispatch_host stub that drives the trampoline
** exactly like the emitted code would (it calls the same rt_* APIs).
**
** Build & run: runtime/tests/run.sh (plain/ASan/UBSan).
*/

#include <math.h>
#include <stdio.h>
#include <string.h>

/* pull the ABI in directly (single translation unit with the Lua core) */
#include "../rt_abi55.c"
#include "lua55/src/lualib.h" /* luaL_openlibs (string metatable etc.) */

static int failures = 0;
#define CHECK(cond) do { \
	if (!(cond)) { failures++; printf("FAIL %d: %s\n", __LINE__, #cond); } \
} while (0)

static rt_addr cell_of(TValue *v) { return (rt_addr)(uintptr_t)v; }

/* the staged message ends with the given text (the "script:<line>: "
   prefix is the ABI's position protocol) */
static int err_ends_with(const char *suffix) {
  static char buf[512];
  int32_t n = rt_err_stage_copy((rt_addr)(uintptr_t)buf, sizeof buf - 1);
  if (n <= 0) return 0;
  buf[n] = '\0';
  size_t sl = strlen(suffix);
  return (size_t)n >= sl && strcmp(buf + n - sl, suffix) == 0;
}

/* ---- the script-module stub: behavior keyed by proto idx ----
**
** Each behavior is a miniature "compiled function": it reads its frame
** cells and calls rt_* exactly like emitted code, so the trampoline is
** exercised through the same protocol the real backend will use. */

enum {
  IDX_SUM = 10,     /* frame[0] = frame[0] + frame[1] via rt_arith */
  IDX_FAIL = 11,    /* rt_arith on booleans -> staged error, -1 */
  IDX_TAIL = 12,    /* tailcall into IDX_SUM via rt_tail_stage, -2 */
  IDX_RECURSE = 13  /* rt_call itself forever -> a stack-overflow family */
};

static TValue g_sum_closure;    /* CClosures over IDX_SUM / IDX_RECURSE */
static TValue g_recurse_closure;

int32_t wasm_dispatch_host(int32_t idx, rt_addr frame, rt_addr cl,
                           int32_t nargs, int32_t want) {
  TValue *f = (TValue *)(size_t)frame;
  (void)cl; (void)want; /* record/seam context the real script module uses */
  switch (idx) {
  case IDX_SUM:
    if (nargs < 2) return -1;
    if (rt_arith(RT_OP_ADD, cell_of(&f[0]), cell_of(&f[1]), cell_of(&f[0]),
                 0) != RT_OK)
      return -1; /* staged; the trampoline re-raises */
    return want < 0 ? 1 : want;
  case IDX_FAIL: {
    TValue t;
    rt_mkbool(cell_of(&t), 1);
    if (rt_arith(RT_OP_ADD, cell_of(&t), cell_of(&t), cell_of(&f[0]),
                 7) != RT_OK)
      return -1;
    return 0;
  }
  case IDX_TAIL:
    /* stage a tailcall into the IDX_SUM closure: -2 makes the
       trampoline restage at the same adapter level */
    return rt_tail_stage(cell_of(&g_sum_closure), frame, nargs, frame) >= 0
               ? RTW_TAIL
               : -1;
  case IDX_RECURSE: {
    /* count the argument down through a fresh call of THIS closure —
       the rt_call fallback chain exercises reentrancy + the depth
       guards (the rt one at 150, Lua's C counter first with 5.5's
       "C stack overflow"; both catchable) */
    TValue one;
    rt_mkinteger(cell_of(&one), 1);
    if (rt_arith(RT_OP_SUB, cell_of(&f[0]), cell_of(&one), cell_of(&f[0]),
                 0) != RT_OK)
      return -1;
    return rt_call(cell_of(&g_recurse_closure), frame, 1, 1, 0) == RT_OK
               ? 0
               : -1;
  }
  default:
    return RTW_REFUSED;
  }
}

/* the record address of a wasm CClosure (what emitted code passes as cl) */
static rt_addr rec_of(TValue *cell) {
  return (rt_addr)(uintptr_t)pvalue(&clCvalue(cell)->upvalue[0]);
}

/* run a chunk against the state; on error returns the message */
static int dostring_msg(lua_State *L, const char *src, const char **msg) {
  lua_settop(L, 0);
  *msg = "";
  if (luaL_loadbuffer(L, src, strlen(src), "=t") != 0) {
    *msg = lua_tostring(L, -1);
    return -1;
  }
  if (lua_pcall(L, 0, LUA_MULTRET, 0) != 0) {
    *msg = lua_tostring(L, -1);
    return 1;
  }
  return 0;
}

int main(void) {
  TValue c[16];
  rt_addr cc[16];
  int i;
  for (i = 0; i < 16; i++) cc[i] = cell_of(&c[i]);

  lua_State *L = luaL_newstate();
  luaL_openlibs(L); /* the string metatable is part of 5.5 arith */
  CHECK(L != NULL);
  /* the v1 engine posture: GC stopped while wasm cells (and the
     rt-owned upvalue registry, invisible to the GC by design) are live —
     the M6/U6 arena lifecycle is the durable fix. The native test runs
     the same posture. */
  lua_gc(L, LUA_GCSTOP);
  rt_set_state((rt_addr)(uintptr_t)L);
  CHECK(rt_abi_version() == LUA_RT_ABI);
  CHECK(rt_abi_version() == 4);

  /* ---- value construction + the variant-tag contract ---- */
  rt_mknumber(cc[0], 2.5);
  CHECK(ttisfloat(&c[0]) && fltvalue(&c[0]) == 2.5);
  rt_mkinteger(cc[1], 0x7fffffffffffffffLL);
  CHECK(ttisinteger(&c[1]) && ivalue(&c[1]) == LUA_MAXINTEGER);
  rt_mkinteger(cc[2], LUA_MININTEGER);
  CHECK(ttisinteger(&c[2]) && ivalue(&c[2]) == LUA_MININTEGER);
  rt_mkbool(cc[3], 1);
  CHECK(ttistrue(&c[3]));
  rt_mkbool(cc[3], 0);
  CHECK(ttisfalse(&c[3]));
  rt_mknil(cc[4]);
  CHECK(ttisnil(&c[4]));
  CHECK(rawtt(&c[1]) == RT_VNUMINT && rawtt(&c[0]) == RT_VNUMFLT);

  /* ---- integer arith fidelity (luaO_arith: stock semantics/texts) ---- */
  {
    TValue a, b, r;
    rt_addr ra = cell_of(&a), rb = cell_of(&b), rr = cell_of(&r);

    rt_mkinteger(ra, 3);
    rt_mkinteger(rb, 4);
    CHECK(rt_arith(RT_OP_ADD, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 7);

    rt_mknumber(rb, 4.5);
    CHECK(rt_arith(RT_OP_ADD, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisfloat(&r) && fltvalue(&r) == 7.5);

    /* int/int '/' is a float (5.3+ rule) */
    rt_mkinteger(ra, 7);
    rt_mkinteger(rb, 2);
    CHECK(rt_arith(RT_OP_DIV, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisfloat(&r) && fltvalue(&r) == 3.5);

    /* floor idiv/mod keep the integer subtype */
    CHECK(rt_arith(RT_OP_IDIV, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 3);
    rt_mkinteger(ra, -7);
    CHECK(rt_arith(RT_OP_IDIV, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == -4);
    CHECK(rt_arith(RT_OP_MOD, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 1); /* -7 % 2 == 1 (floor) */

    /* integer wrap-around (intop unsigned casts) */
    rt_mkinteger(ra, LUA_MAXINTEGER);
    rt_mkinteger(rb, 1);
    CHECK(rt_arith(RT_OP_ADD, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == LUA_MININTEGER);

    /* shifts: logical right, negative shift flips direction */
    rt_mkinteger(ra, 1);
    rt_mkinteger(rb, 10);
    CHECK(rt_arith(RT_OP_SHL, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 1024);
    rt_mkinteger(ra, -1);
    rt_mkinteger(rb, 1);
    CHECK(rt_arith(RT_OP_SHR, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == LUA_MAXINTEGER); /* logical */
    rt_mkinteger(ra, 1);
    rt_mkinteger(rb, -1);
    CHECK(rt_arith(RT_OP_SHL, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 0); /* 1 << -1 == 1 >> 1 */

    /* bnot / band */
    rt_mkinteger(ra, 0);
    CHECK(rt_arith(RT_OP_BNOT, ra, ra, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == -1);
    rt_mkinteger(ra, 0xf0);
    rt_mkinteger(rb, 0x3c);
    CHECK(rt_arith(RT_OP_BAND, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 0x30);

    /* unary minus on mininteger wraps to itself */
    rt_mkinteger(ra, LUA_MININTEGER);
    CHECK(rt_arith(RT_OP_UNM, ra, ra, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == LUA_MININTEGER);

    /* pow is float; huge floats saturate */
    rt_mkinteger(ra, 2);
    rt_mkinteger(rb, 63);
    CHECK(rt_arith(RT_OP_POW, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisfloat(&r) && fltvalue(&r) == 9223372036854775808.0);

    /* error texts, stock 5.5 wording */
    rt_mkinteger(ra, 1);
    rt_mkinteger(rb, 0);
    CHECK(rt_arith(RT_OP_IDIV, ra, rb, rr, 100) == RT_ERR);
    CHECK(err_ends_with("attempt to divide by zero"));
    CHECK(rt_err_line() == 100);
    rt_err_clear();
    CHECK(rt_arith(RT_OP_MOD, ra, rb, rr, 101) == RT_ERR);
    CHECK(err_ends_with("attempt to perform 'n%0'"));
    rt_err_clear();

    /* floats with no integer representation (luaG_tointerror) */
    rt_mknumber(ra, 3.5);
    rt_mkinteger(rb, 1);
    CHECK(rt_arith(RT_OP_BAND, ra, rb, rr, 102) == RT_ERR);
    CHECK(err_ends_with("number has no integer representation"));
    rt_err_clear();

    /* non-number arith (stock luaG_opinterror wording) */
    rt_mkbool(ra, 1);
    rt_mkbool(rb, 1);
    CHECK(rt_arith(RT_OP_ADD, ra, rb, rr, 103) == RT_ERR);
    CHECK(err_ends_with("attempt to perform arithmetic on a boolean value"));
    rt_err_clear();

    /* numeric strings go through the string metatable (5.4+ model) */
    rt_intern(ra, (rt_addr)(uintptr_t)"10", 2);
    rt_mkinteger(rb, 5);
    CHECK(rt_arith(RT_OP_ADD, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 15);
    /* subtype-preserving: "2.5" + 1 is a float */
    rt_mkinteger(rb, 1);
    rt_intern(ra, (rt_addr)(uintptr_t)"2.5", 3);
    CHECK(rt_arith(RT_OP_ADD, ra, rb, rr, 10) == RT_OK);
    CHECK(ttisfloat(&r) && fltvalue(&r) == 3.5);
  }

  /* ---- comparisons: mixed subtypes, the 2^53 exact edges ---- */
  {
    TValue a, b, r;
    rt_addr ra = cell_of(&a), rb = cell_of(&b), rr = cell_of(&r);

    rt_mkinteger(ra, 3);
    rt_mknumber(rb, 3.0);
    CHECK(rt_eq(ra, rb, rr, 30) == RT_OK);
    CHECK(ttistrue(&r));
    CHECK(rt_le(ra, rb, rr, 30) == RT_OK && ttistrue(&r));

    /* beyond 2^53 the float cannot represent odd integers: exact math */
    rt_mknumber(ra, 9007199254740992.0); /* 2^53 */
    rt_mkinteger(rb, 9007199254740993LL); /* 2^53 + 1 */
    CHECK(rt_lt(ra, rb, rr, 31) == RT_OK && ttistrue(&r));
    CHECK(rt_lt(rb, ra, rr, 31) == RT_OK && ttisfalse(&r));
    CHECK(rt_eq(ra, rb, rr, 31) == RT_OK && ttisfalse(&r));

    /* int < float beyond 2^63 (float rounds to 2^63 > maxint) */
    rt_mkinteger(ra, LUA_MAXINTEGER);
    rt_mknumber(rb, 9223372036854775808.0); /* 2^63 exactly */
    CHECK(rt_lt(ra, rb, rr, 31) == RT_OK && ttistrue(&r));

    /* strings compare bytewise */
    rt_intern(ra, (rt_addr)(uintptr_t)"abc", 3);
    rt_intern(rb, (rt_addr)(uintptr_t)"abd", 3);
    CHECK(rt_lt(ra, rb, rr, 32) == RT_OK && ttistrue(&r));

    /* order error: stock text */
    rt_mkinteger(ra, 1);
    CHECK(rt_lt(ra, rb, rr, 33) == RT_ERR);
    CHECK(err_ends_with("attempt to compare number with string"));
    rt_err_clear();
  }

  /* ---- tables: string keys, float-key normalization, nil index ---- */
  {
    TValue t, k, v, r;
    rt_addr rt_ = cell_of(&t), rk = cell_of(&k), rv = cell_of(&v),
            rr = cell_of(&r);
    CHECK(rt_newtable(rt_, 4, 4) == RT_OK);
    CHECK(ttistable(&t));

    rt_intern(rk, (rt_addr)(uintptr_t)"x", 1);
    rt_mkinteger(rv, 42);
    CHECK(rt_settable(rt_, rk, rv, 20) == RT_OK);
    CHECK(rt_gettable(rt_, rk, rr, 21) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 42);

    /* t[2.0] and t[2] are the SAME key (5.4+ normalization) */
    rt_mknumber(rk, 2.0);
    rt_mkinteger(rv, 7);
    CHECK(rt_settable(rt_, rk, rv, 22) == RT_OK);
    rt_mkinteger(rk, 2);
    CHECK(rt_gettable(rt_, rk, rr, 23) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 7);

    /* 3.5 stays a distinct float key */
    rt_mknumber(rk, 3.5);
    rt_mkinteger(rv, 9);
    CHECK(rt_settable(rt_, rk, rv, 24) == RT_OK);
    rt_mknumber(rk, 3.5);
    CHECK(rt_gettable(rt_, rk, rr, 25) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 9);

    /* __index metamethod through the seam (a Lua closure runs under
       rt_run's protect — the metamethod body is real Lua) */
    {
      TValue mt, idx;
      CHECK(rt_newtable(cell_of(&mt), 0, 1) == RT_OK);
      {
        const char *src = "return function(_, k) return 'mm:' .. k end";
        CHECK(luaL_loadbuffer(L, src, strlen(src), "=mt") == 0);
        CHECK(lua_pcall(L, 0, 1, 0) == 0);
        idx = *s2v(L->top.p - 1);
        L->top.p--;
      }
      rt_intern(rk, (rt_addr)(uintptr_t)"__index", 7);
      CHECK(rt_settable(cell_of(&mt), rk, cell_of(&idx), 26) == RT_OK);
      /* attach the metatable via the public API: t's cell -> stack */
      luaD_checkstack(L, 2);
      setobj2s(L, L->top.p, &t);
      L->top.p++;
      setobj2s(L, L->top.p, &mt);
      L->top.p++;
      lua_setmetatable(L, -2);
      L->top.p--;
      rt_intern(rk, (rt_addr)(uintptr_t)"nope", 4);
      CHECK(rt_gettable(rt_, rk, rr, 27) == RT_OK);
      CHECK(ttisstring(&r) &&
            strcmp(getstr(tsvalue(&r)), "mm:nope") == 0);
    }

    /* nil index errors */
    rt_mknil(rk);
    CHECK(rt_gettable(rt_, rk, rr, 77) == RT_ERR);
    CHECK(rt_err_pending() == 1);
    {
      static char buf[256];
      int32_t n = rt_err_stage_copy((rt_addr)(uintptr_t)buf, sizeof buf);
      CHECK(n > 0);
      CHECK(strncmp(buf, "script:77:", 10) == 0);
    }
    rt_err_clear();
    CHECK(rt_err_stage_copy((rt_addr)(uintptr_t)&c[0], 16) == 0);
  }
  CHECK(rt_err_pending() == 0);

  /* ---- length: strings and tables return integers ---- */
  {
    TValue s, t, r;
    rt_intern(cell_of(&s), (rt_addr)(uintptr_t)"hello", 5);
    CHECK(rt_len(cell_of(&s), cell_of(&r), 24) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 5);
    CHECK(rt_newtable(cell_of(&t), 3, 0) == RT_OK);
    CHECK(rt_len(cell_of(&t), cell_of(&r), 24) == RT_OK);
    CHECK(ttisinteger(&r));
    rt_mkbool(cell_of(&s), 1);
    CHECK(rt_len(cell_of(&s), cell_of(&r), 24) == RT_ERR);
    CHECK(err_ends_with("attempt to get length of a boolean value"));
    rt_err_clear();
  }

  /* ---- concat: subtype-visible number formatting (luaO_tostringbuff) */
  {
    TValue pair[2], r;
    rt_addr pc[2] = {cell_of(&pair[0]), cell_of(&pair[1])};
    rt_intern(pc[0], (rt_addr)(uintptr_t)"x=", 2);
    rt_mkinteger(pc[1], 3);
    CHECK(rt_concat(pc[0], 2, cell_of(&r), 41) == RT_OK);
    CHECK(ttisstring(&r) && strcmp(getstr(tsvalue(&r)), "x=3") == 0);
    rt_mknumber(pc[1], 3.0);
    CHECK(rt_concat(pc[0], 2, cell_of(&r), 41) == RT_OK);
    CHECK(ttisstring(&r) && strcmp(getstr(tsvalue(&r)), "x=3.0") == 0);
    /* a huge integer concatenates exactly (f64 could not) */
    rt_intern(pc[0], (rt_addr)(uintptr_t)"", 0);
    rt_mkinteger(pc[1], 9007199254740993LL);
    CHECK(rt_concat(pc[0], 2, cell_of(&r), 41) == RT_OK);
    CHECK(ttisstring(&r) &&
          strcmp(getstr(tsvalue(&r)), "9007199254740993") == 0);
    /* __concat on non-strings */
    rt_mkbool(pc[0], 1);
    CHECK(rt_concat(pc[0], 2, cell_of(&r), 41) == RT_ERR);
    rt_err_clear();
  }

  /* ---- forprep: stock cell rewrite (count, step, init) ---- */
  {
    TValue fv[3];
    rt_addr fc[3] = {cell_of(&fv[0]), cell_of(&fv[1]), cell_of(&fv[2])};

    rt_mkinteger(fc[0], 1);
    rt_mkinteger(fc[1], 10);
    rt_mkinteger(fc[2], 1);
    CHECK(rt_forprep(fc[0], 50) == RT_OK); /* enter */
    CHECK(ttisinteger(&fv[0]) && ivalue(&fv[0]) == 9);  /* count */
    CHECK(ttisinteger(&fv[1]) && ivalue(&fv[1]) == 1);  /* step */
    CHECK(ttisinteger(&fv[2]) && ivalue(&fv[2]) == 1);  /* init */

    /* descending, step -3: 10,7,4,1 -> 4 iterations */
    rt_mkinteger(fc[0], 10);
    rt_mkinteger(fc[1], 1);
    rt_mkinteger(fc[2], -3);
    CHECK(rt_forprep(fc[0], 50) == RT_OK);
    CHECK(ivalue(&fv[0]) == 3 && ivalue(&fv[1]) == -3 &&
          ivalue(&fv[2]) == 10);

    /* skip */
    rt_mkinteger(fc[0], 11);
    rt_mkinteger(fc[1], 10);
    rt_mkinteger(fc[2], 1);
    CHECK(rt_forprep(fc[0], 50) == RT_FORPREP_SKIP);

    /* float flavor rewrites to (limit, step, init), all floats */
    rt_mknumber(fc[0], 1.5);
    rt_mkinteger(fc[1], 10);
    rt_mkinteger(fc[2], 1);
    CHECK(rt_forprep(fc[0], 50) == RT_OK);
    CHECK(ttisfloat(&fv[0]) && fltvalue(&fv[0]) == 10.0);
    CHECK(ttisfloat(&fv[1]) && fltvalue(&fv[1]) == 1.0);
    CHECK(ttisfloat(&fv[2]) && fltvalue(&fv[2]) == 1.5);

    /* float limit that is integral keeps the INTEGER flavor */
    rt_mkinteger(fc[0], 1);
    rt_mknumber(fc[1], 10.0);
    rt_mkinteger(fc[2], 2);
    CHECK(rt_forprep(fc[0], 50) == RT_OK);
    CHECK(ttisinteger(&fv[0]) && ivalue(&fv[0]) == 4); /* (10-1)/2 */

    /* string limits coerce (tonumber in forlimit) */
    rt_mkinteger(fc[0], 1);
    rt_intern(fc[1], (rt_addr)(uintptr_t)"10", 2);
    rt_mkinteger(fc[2], 1);
    CHECK(rt_forprep(fc[0], 50) == RT_OK);
    CHECK(ttisinteger(&fv[0]) && ivalue(&fv[0]) == 9);

    /* the full-range loop: count saturates at 2^64-1 == -1 signed (the
       stock fix for the 5.3 wrap bug — row 57 closed in U2c on the Go
       side; here the same count lives in the cell) */
    rt_mkinteger(fc[0], LUA_MININTEGER);
    rt_mkinteger(fc[1], LUA_MAXINTEGER);
    rt_mkinteger(fc[2], 1);
    CHECK(rt_forprep(fc[0], 50) == RT_OK);
    CHECK(ivalue(&fv[0]) == -1 && ivalue(&fv[1]) == 1 &&
          ivalue(&fv[2]) == LUA_MININTEGER);

    /* float limit out of integer range: saturate to maxint, don't run
       off (step < 0 against +huge skips) */
    rt_mkinteger(fc[0], 1);
    rt_mknumber(fc[1], 1e300);
    rt_mkinteger(fc[2], -1);
    CHECK(rt_forprep(fc[0], 50) == RT_FORPREP_SKIP);

    /* errors */
    rt_mkinteger(fc[0], 1);
    rt_mkinteger(fc[1], 10);
    rt_mkinteger(fc[2], 0);
    CHECK(rt_forprep(fc[0], 51) == RT_ERR);
    CHECK(err_ends_with("'for' step is zero"));
    rt_err_clear();
    rt_mkbool(fc[1], 1);
    rt_mkinteger(fc[2], 1);
    CHECK(rt_forprep(fc[0], 51) == RT_ERR);
    CHECK(err_ends_with("bad 'for' limit (number expected, got boolean)"));
    rt_err_clear();
  }

  /* ---- globals ---- */
  {
    TValue k, v, r;
    rt_intern(cell_of(&k), (rt_addr)(uintptr_t)"gvar", 4);
    rt_mkinteger(cell_of(&v), 42);
    CHECK(rt_setglobal(cell_of(&k), cell_of(&v), 60) == RT_OK);
    CHECK(rt_getglobal(cell_of(&r), cell_of(&k), 61) == RT_OK);
    CHECK(ttisinteger(&r) && ivalue(&r) == 42);
    /* cross-check through the public API */
    lua_getglobal(L, "gvar");
    CHECK(lua_isinteger(L, -1) && lua_tointeger(L, -1) == 42);
    L->top.p--;
  }

  /* ---- rt_call: subtype preserved through a real Lua closure ---- */
  {
    const char *src = "local function add(a,b) return a+b end return add";
    TValue fcell, args[2];
    CHECK(luaL_loadbuffer(L, src, strlen(src), "=test") == 0);
    CHECK(lua_pcall(L, 0, 1, 0) == 0);
    fcell = *s2v(L->top.p - 1); /* keep the stack reference alive: a
                                   C-stack TValue is invisible to the GC */
    rt_mkinteger(cell_of(&args[0]), 20);
    rt_mkinteger(cell_of(&args[1]), 22);
    int32_t st = rt_call(cell_of(&fcell), cell_of(&args[0]), 2, 1, 60);
    L->top.p--;
    CHECK(st == RT_OK);
    CHECK(ttisinteger(&args[0]) && ivalue(&args[0]) == 42);
    /* multret encoding */
    {
      const char *src2 = "return function(...) return ... end";
      TValue f2;
      CHECK(luaL_loadbuffer(L, src2, strlen(src2), "=t2") == 0);
      CHECK(lua_pcall(L, 0, 1, 0) == 0);
      f2 = *s2v(L->top.p - 1);
      rt_mkinteger(cell_of(&args[0]), 1);
      rt_mkinteger(cell_of(&args[1]), 2);
      int32_t enc = rt_call(cell_of(&f2), cell_of(&args[0]), 2, -1, 60);
      L->top.p--;
      CHECK(enc < 0 && rt_call_count(enc) == 2);
    }
  }

  /* ---- rt_error: emitted-code raise with position + string value ---- */
  {
    rt_error((rt_addr)(uintptr_t)"boom", 4, 12);
    CHECK(rt_err_pending() == 1);
    CHECK(err_ends_with("script:12: boom"));
    {
      TValue ev;
      CHECK(rt_err_stage_value(cell_of(&ev), sizeof ev) == (int32_t)sizeof ev);
      CHECK(ttisstring(&ev) && strcmp(getstr(tsvalue(&ev)), "script:12: boom") == 0);
    }
    CHECK(rt_err_line() == 12);
    rt_err_clear();
  }

  /* ---- ABI v4: the rt-owned upvalue registry (record-based) ---- */
  {
    TValue cells[4]; /* a fake wasm frame: four contiguous register cells */
    TValue c0, c1, out;
    rt_addr cf = cell_of(&cells[0]);
    CHECK(rt_wasm_proto(0, 0, 0, 1, 8) == RT_OK);
    rt_wasm_upval(0, 0, 1, 1); /* the closure captures register 1 */
    rt_mkinteger(cell_of(&cells[1]), 42);
    CHECK(rt_newclosure(cell_of(&c0), 0, 0, cf, 1) == RT_OK);
    CHECK(ttisCclosure(&c0)); /* the zero-patch seam: CClosure + record */
    CHECK(rt_clidx(cell_of(&c0)) == 0);
    rt_mkinteger(cell_of(&c1), 1);
    CHECK(rt_clidx(cell_of(&c1)) == -1);
    rt_addr rec = rec_of(&c0);
    /* open: GETUPVAL reads the frame cell */
    rt_mknil(cell_of(&out));
    rt_getupval(rec, 0, cell_of(&out));
    CHECK(ttisinteger(&out) && ivalue(&out) == 42);
    /* write-through: SETUPVAL lands in the open frame cell */
    rt_mkinteger(cell_of(&out), 99);
    rt_setupval(rec, 0, cell_of(&out));
    CHECK(ivalue(&cells[1]) == 99);
    /* close-level ordering: closing ABOVE the cell leaves it open */
    rt_close_upvals(cf + 2 * (rt_addr)sizeof(TValue));
    rt_mkinteger(cell_of(&cells[1]), 77);
    rt_mknil(cell_of(&out));
    rt_getupval(rec, 0, cell_of(&out));
    CHECK(ivalue(&out) == 77);
    /* close AT the frame: snapshotted; later frame writes invisible */
    rt_close_upvals(cf);
    rt_mkinteger(cell_of(&cells[1]), 111);
    rt_mknil(cell_of(&out));
    rt_getupval(rec, 0, cell_of(&out));
    CHECK(ivalue(&out) == 77);
    /* two closures, two levels: closing the higher leaves the lower open */
    CHECK(rt_wasm_proto(1, 0, 0, 1, 8) == RT_OK);
    rt_wasm_upval(1, 0, 1, 0); /* captures register 0 */
    rt_mkinteger(cell_of(&cells[0]), 5);
    CHECK(rt_newclosure(cell_of(&c1), 1, 0, cf, 1) == RT_OK);
    rt_close_upvals(cf + (rt_addr)sizeof(TValue)); /* closes cells[1] only */
    rt_mkinteger(cell_of(&cells[1]), 0);
    rt_mknil(cell_of(&out));
    rt_getupval(rec_of(&c1), 0, cell_of(&out));
    CHECK(ivalue(&out) == 5); /* cells[0] still open, reads through */
  }

  /* ---- the CClosure dispatch seam (trampoline end-to-end) ---- */
  {
    TValue fcell;
    const char *msg;

    /* install wasmf = <IDX_SUM closure> as a real global */
    CHECK(rt_wasm_proto(IDX_SUM, 2, 0, 0, 8) == RT_OK);
    CHECK(rt_newclosure(cell_of(&fcell), IDX_SUM, 0, 0, 1) == RT_OK);
    g_sum_closure = fcell;
    luaD_checkstack(L, 1);
    setobj2s(L, L->top.p, &fcell);
    L->top.p++;
    lua_setglobal(L, "wasmf");

    /* plain call through Lua: precall C path -> trampoline -> stub
       (which itself calls rt_arith) -> results copied back */
    CHECK(dostring_msg(L, "local s = wasmf(20, 22) return s", &msg) == 0);
    CHECK(lua_isinteger(L, -1) && lua_tointeger(L, -1) == 42);

    /* multret */
    CHECK(dostring_msg(L, "return select('#', wasmf(1,2))", &msg) == 0);
    CHECK(lua_tointeger(L, -1) == 1);

    /* error path: the staged error re-raises through the trampoline,
       pcall catches the STOCK text, and the staging is CLEARED (the v4
       rule that replaces v3's patched-ldo.c rt_pcall_caught) */
    CHECK(rt_wasm_proto(IDX_FAIL, 0, 0, 0, 8) == RT_OK);
    CHECK(rt_newclosure(cell_of(&fcell), IDX_FAIL, 0, 0, 1) == RT_OK);
    luaD_checkstack(L, 1);
    setobj2s(L, L->top.p, &fcell);
    L->top.p++;
    lua_setglobal(L, "wasmfail");
    CHECK(dostring_msg(L, "local ok, e = pcall(wasmfail) return e", &msg) == 0);
    CHECK(strcmp(lua_tostring(L, -1),
                 "script:7: attempt to perform arithmetic on a boolean value") ==
          0);
    CHECK(rt_err_pending() == 0); /* cleared at the re-raise boundary */

    /* a follow-up rt_* works after the catch (no sticky staging) */
    {
      TValue a, b, r;
      rt_mkinteger(cell_of(&a), 1);
      rt_mkinteger(cell_of(&b), 2);
      CHECK(rt_arith(RT_OP_ADD, cell_of(&a), cell_of(&b), cell_of(&r),
                     10) == RT_OK);
      CHECK(ttisinteger(&r) && ivalue(&r) == 3);
    }

    /* nested: Lua -> wasm -> rt_call -> Lua -> wasm (reentrancy law) */
    CHECK(dostring_msg(L,
        "local function twice(a,b) return wasmf(a,b)*2 end "
        "return twice(15, 20)", &msg) == 0);
    CHECK(lua_isinteger(L, -1) && lua_tointeger(L, -1) == 70);

    /* tailcall staging: -2 restages at the same adapter level */
    CHECK(rt_wasm_proto(IDX_TAIL, 2, 0, 0, 8) == RT_OK);
    CHECK(rt_newclosure(cell_of(&fcell), IDX_TAIL, 0, 0, 1) == RT_OK);
    luaD_checkstack(L, 1);
    setobj2s(L, L->top.p, &fcell);
    L->top.p++;
    lua_setglobal(L, "wasmtail");
    CHECK(dostring_msg(L, "return wasmtail(30, 12)", &msg) == 0);
    CHECK(lua_isinteger(L, -1) && lua_tointeger(L, -1) == 42);

    /* depth guard: rt_wasm_enter stages "stack overflow"; the Lua-level
       catch survives it and staging is clean after */
    CHECK(rt_wasm_proto(IDX_RECURSE, 1, 0, 0, 8) == RT_OK);
    CHECK(rt_newclosure(cell_of(&g_recurse_closure), IDX_RECURSE, 0, 0, 1) ==
          RT_OK);
    luaD_checkstack(L, 1);
    setobj2s(L, L->top.p, &g_recurse_closure);
    L->top.p++;
    lua_setglobal(L, "wasmdeep");
    CHECK(dostring_msg(L,
        "local ok, e = pcall(wasmdeep, 1000000) return ok, e", &msg) == 0);
    CHECK(lua_toboolean(L, -2) == 0);
    {
      /* which guard fires depends on the C-call accounting (the rt one
         at 150 pure-wasm frames, Lua's at 200 C units with 5.5's "C
         stack overflow" wording); either way it is catchable and the
         staging is clean after */
      const char *e = lua_tostring(L, -1);
      CHECK(e != NULL && strstr(e, "stack overflow") != NULL);
    }
    CHECK(rt_err_pending() == 0);
  }

  /* the rt depth guard itself, pinned directly: 150 wasm frames */
  {
    int i;
    for (i = 0; i < RTW_MAX_DEPTH; i++) CHECK(rt_wasm_enter(0) == 0);
    CHECK(rt_wasm_enter(0) == 1); /* staged "stack overflow" */
    CHECK(err_ends_with("stack overflow"));
    for (i = 0; i < RTW_MAX_DEPTH; i++) rt_wasm_leave();
    rt_err_clear();
  }

  CHECK(rt_wasm_count() == IDX_RECURSE + 1);
  CHECK(rt_wasm_proto(-1, 0, 0, 0, 0) == RT_ERR);

  if (failures == 0) printf("RT-NATIVE55 PASS\n");
  else printf("RT-NATIVE55 FAIL (%d)\n", failures);
  return failures;
}
