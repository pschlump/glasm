/*
** runtime/tests/rt_native_test.c — native unit tests for the rt_* ABI
** (design doc §8.1 L1/L2): the same C sources compiled for the host,
** exercising the ABI surface directly. The wasm32-only concerns (TValue
** 16-byte layout, shared memory) are covered by the seam gates; these
** tests validate the semantics/protocol on a normal 64-bit host.
**
** Build & run: runtime/tests/run.sh (includes ASan+UBSan builds).
*/

#include <math.h>
#include <stdio.h>
#include <string.h>

/* pull the ABI in directly (single translation unit with Lua core) */
#include "../rt_abi.c"
#include "lua51/src/lualib.h" /* luaL_openlibs (the sandbox leg) */

/* wasm_dispatch_host is a host import on wasm; native tests have no
   script module, so stub it host-refused (nothing registers a wasm proto
   here, so precall_wasm never dispatches) */
int32_t wasm_dispatch_host(int32_t idx, rt_addr frame, rt_addr cl,
                           int32_t nargs, int32_t want) {
  (void)idx; (void)frame; (void)cl; (void)nargs; (void)want;
  return RTW_REFUSED;
}

static int failures = 0;
#define CHECK(cond) do { \
	if (!(cond)) { failures++; printf("FAIL %d: %s\n", __LINE__, #cond); } \
} while (0)

static rt_addr cell_of(TValue *v) { return (rt_addr)(uintptr_t)v; }

/* M6d: allocation-hungry bodies run under lua_cpcall (an unprotected
   ERRMEM would abort the test — the point is that it is CATCHABLE) */
static int oom_body(lua_State *L) {
  const char *big = (const char *)lua_touserdata(L, 1);
  lua_pushlstring(L, big, 200 * 1024); /* over the 64 KiB cap → ERRMEM */
  return 1;
}

static int small_body(lua_State *L) {
  lua_pushliteral(L, "fits");
  return 1;
}

int main(void) {
  TValue c[16];
  rt_addr cc[16];
  int i;
  for (i = 0; i < 16; i++) cc[i] = cell_of(&c[i]);

  lua_State *L = luaL_newstate();
  CHECK(L != NULL);
  rt_set_state((rt_addr)(uintptr_t)L);
  CHECK(rt_abi_version() == LUA_RT_ABI);

  /* M6c sandbox: the dev build opens all libs, so rt_sandbox(1) must
     remove exactly the lockdown list (m6 plan §4.3) and keep the rest */
  {
    static const char *killed[] = {"io",     "os",     "package", "require",
                                   "module", "dofile", "loadfile", "load",
                                   "loadstring", "debug", NULL};
    int n, k;
    luaL_openlibs(L); /* dev-flavor surface for the sandbox to remove */
    n = rt_sandbox(1);
    CHECK(n == 10); /* every listed global was present and got nil'd */
    for (k = 0; killed[k]; k++) {
      lua_getglobal(L, killed[k]);
      CHECK(lua_isnil(L, -1));
      lua_pop(L, 1);
    }
    lua_getglobal(L, "pcall");
    CHECK(lua_isfunction(L, -1));
    lua_pop(L, 1);
    lua_getglobal(L, "string");
    CHECK(lua_istable(L, -1));
    lua_pop(L, 1);
    CHECK(rt_sandbox(0) == -1); /* off (and NULL-state) refused */
  }

  /* value construction */
  rt_mknumber(cc[0], 2.5);
  CHECK(ttisnumber(&c[0]) && nvalue(&c[0]) == 2.5);
  rt_mkbool(cc[1], 1);
  CHECK(ttisboolean(&c[1]) && bvalue(&c[1]) == 1);
  rt_mknil(cc[2]);
  CHECK(ttisnil(&c[2]));

  /* arith: all ops, string coercion, metamethod, error */
  rt_mknumber(cc[3], 7);
  CHECK(rt_arith(RT_OP_ADD, cc[0], cc[3], cc[4], 10) == RT_OK);
  CHECK(nvalue(&c[4]) == 9.5);
  CHECK(rt_arith(RT_OP_MUL, cc[0], cc[3], cc[4], 10) == RT_OK);
  CHECK(nvalue(&c[4]) == 17.5);
  CHECK(rt_arith(RT_OP_UNM, cc[0], cc[0], cc[4], 10) == RT_OK);
  CHECK(nvalue(&c[4]) == -2.5);
  CHECK(rt_arith(RT_OP_POW, cc[3], cc[0], cc[4], 10) == RT_OK);
  CHECK(nvalue(&c[4]) == pow(7, 2.5));
  rt_intern(cc[5], (rt_addr)(uintptr_t)"10", 2);
  CHECK(rt_arith(RT_OP_ADD, cc[5], cc[3], cc[6], 11) == RT_OK);
  CHECK(nvalue(&c[6]) == 17); /* "10" + 7 coerces */
  rt_mkbool(cc[7], 1);
  CHECK(rt_arith(RT_OP_ADD, cc[7], cc[7], cc[8], 12) == RT_ERR);
  CHECK(rt_err_pending() == 1);
  rt_err_clear();
  CHECK(rt_err_pending() == 0);

  /* tables: string keys, round trip, errors */
  CHECK(rt_newtable(cc[9], 0, 0) == RT_OK);
  rt_mknumber(cc[10], 42);
  CHECK(rt_settable(cc[9], cc[5], cc[10], 20) == RT_OK); /* t["10"]=42 */
  CHECK(rt_gettable(cc[9], cc[5], cc[11], 21) == RT_OK);
  CHECK(ttisnumber(&c[11]) && nvalue(&c[11]) == 42);
  rt_mknil(cc[12]);
  CHECK(rt_gettable(cc[12], cc[5], cc[11], 22) == RT_ERR); /* nil index */
  rt_err_clear();
  /* fractional keys are legal in the hash part (Lua 5.1 semantics) */
  CHECK(rt_settable(cc[9], cc[0], cc[10], 23) == RT_OK); /* t[2.5]=.. */

  /* length: string, table */
  CHECK(rt_len(cc[5], cc[13], 24) == RT_OK);
  CHECK(nvalue(&c[13]) == 2);
  CHECK(rt_settable(cc[9], cc[3], cc[10], 25) == RT_OK); /* t[7]=42 */
  /* not checking luaH_getn here: holes/resize semantics belong to the
     corpus; the ABI wrapper just forwards */

  /* comparisons */
  rt_mknumber(cc[14], 9.5);
  CHECK(rt_eq(cc[0], cc[0], cc[15], 30) == RT_OK);
  CHECK(ttisboolean(&c[15]) && bvalue(&c[15]) == 1);
  CHECK(rt_eq(cc[0], cc[14], cc[15], 30) == RT_OK);
  CHECK(ttisboolean(&c[15]) && bvalue(&c[15]) == 0);
  CHECK(rt_lt(cc[0], cc[14], cc[15], 31) == RT_OK);
  CHECK(bvalue(&c[15]) == 1); /* 2.5 < 9.5 */
  CHECK(rt_le(cc[14], cc[0], cc[15], 32) == RT_OK);
  CHECK(bvalue(&c[15]) == 0);
  rt_mkbool(cc[14], 1);
  CHECK(rt_lt(cc[0], cc[14], cc[15], 33) == RT_ERR); /* number < bool */
  rt_err_clear();

  /* concat: strings and numbers (cells must be contiguous) */
  {
    TValue pair[2];
    rt_addr pc[2] = { cell_of(&pair[0]), cell_of(&pair[1]) };
    rt_intern(pc[0], (rt_addr)(uintptr_t)"x=", 2);
    rt_mknumber(pc[1], 3);
    int cstat = rt_concat(pc[0], 2, cc[13], 41);
    CHECK(cstat == RT_OK);
    CHECK(ttisstring(&c[13]) && strcmp(svalue(&c[13]), "x=3") == 0);
  }

  /* forprep: coercion + error message parity */
  {
    TValue fv[3];
    rt_addr fc[3] = { cell_of(&fv[0]), cell_of(&fv[1]), cell_of(&fv[2]) };
    rt_intern(fc[0], (rt_addr)(uintptr_t)"3", 1);
    rt_mknumber(fc[1], 10);
    rt_mknumber(fc[2], 1);
    CHECK(rt_forprep(fc[0], 50) == RT_OK);
    CHECK(nvalue(&fv[0]) == 3);
    rt_mkbool(fc[2], 1);
    CHECK(rt_forprep(fc[0], 51) == RT_ERR);
    rt_err_clear();
  }

  /* error staging: bytes + clear */
  {
    char buf[256];
    rt_mknil(cc[0]);
    rt_mknil(cc[1]);
    CHECK(rt_gettable(cc[0], cc[1], cc[2], 77) == RT_ERR);
    int32_t n = rt_err_stage_copy((rt_addr)(uintptr_t)buf, sizeof buf);
    CHECK(n > 0);
    CHECK(strncmp(buf, "script:77:", 10) == 0);
    CHECK(rt_err_pending() == 1);
    rt_err_clear();
    CHECK(rt_err_stage_copy((rt_addr)(uintptr_t)buf, sizeof buf) == 0);
  }

  /* calls: a Lua closure through the ABI (the lvm-fallback path) */
  {
    const char *src = "local function add(a,b) return a+b end return add";
    lua_State *T = L;
    CHECK(luaL_loadbuffer(T, src, strlen(src), "=test") == 0);
    lua_call(T, 0, 1); /* add on the stack */
    /* move the closure value off the stack into a local cell */
    TValue fcell, args[2];
    fcell = *(T->top - 1);
    /* keep the stack reference alive across the call: a C-stack TValue is
       invisible to the GC, and the ABI cell alone could let the closure
       be collected mid-call. The backend owes every callable the same
       liveness while its cell is live. Args must be a real array — the
       ABI treats argcells as contiguous slots (a lesson this test
       learned the hard way: two separate locals are not adjacent). */
    rt_mknumber(cell_of(&args[0]), 20);
    rt_mknumber(cell_of(&args[1]), 22);
    int32_t st = rt_call(cell_of(&fcell), cell_of(&args[0]), 2, 1, 60);
    T->top--; /* release the kept reference */
    CHECK(st == RT_OK);
    CHECK(nvalue(&args[0]) == 42); /* result overwrites the first arg cell */
  }

  /* ABI v3: the rt-owned open-upvalue registry — open → write-through →
     close → snapshot, and close-level ordering */
  {
    TValue cells[4];  /* a fake wasm frame: four contiguous register cells */
    TValue c0, c1, out;
    rt_addr cf = cell_of(&cells[0]);
    CHECK(rt_wasm_proto(0, 0, 0, 1, 8) == RT_OK);
    rt_wasm_upval(0, 0, 1, 1); /* the closure captures register 1 */
    rt_mknumber(cell_of(&cells[1]), 42);
    CHECK(rt_newclosure(cell_of(&c0), 0, 0, cf, 1) == RT_OK);
    CHECK(ttisfunction(&c0));
    CHECK(rt_clidx(cell_of(&c0)) == 0);
    rt_mknumber(cell_of(&c1), 1);
    CHECK(rt_clidx(cell_of(&c1)) == -1);
    /* open: GETUPVAL reads the frame cell */
    rt_mknil(cell_of(&out));
    rt_getupval((rt_addr)(uintptr_t)clvalue(&c0), 0, cell_of(&out));
    CHECK(ttisnumber(&out) && nvalue(&out) == 42);
    /* write-through: SETUPVAL lands in the open frame cell */
    rt_mknumber(cell_of(&out), 99);
    rt_setupval((rt_addr)(uintptr_t)clvalue(&c0), 0, cell_of(&out));
    CHECK(nvalue(&cells[1]) == 99);
    /* close-level ordering: closing ABOVE the cell leaves it open */
    rt_close_upvals(cf + 2 * (rt_addr)sizeof(TValue));
    rt_mknumber(cell_of(&cells[1]), 77);
    rt_mknil(cell_of(&out));
    rt_getupval((rt_addr)(uintptr_t)clvalue(&c0), 0, cell_of(&out));
    CHECK(nvalue(&out) == 77);
    /* close AT the frame: the value is snapshotted; later frame writes
       are invisible through the closure */
    rt_close_upvals(cf);
    rt_mknumber(cell_of(&cells[1]), 111);
    rt_mknil(cell_of(&out));
    rt_getupval((rt_addr)(uintptr_t)clvalue(&c0), 0, cell_of(&out));
    CHECK(nvalue(&out) == 77);
    /* two closures, two levels: closing the higher cell leaves the lower
       one open */
    CHECK(rt_wasm_proto(1, 0, 0, 1, 8) == RT_OK);
    rt_wasm_upval(1, 0, 1, 0); /* captures register 0 */
    rt_mknumber(cell_of(&cells[0]), 5);
    CHECK(rt_newclosure(cell_of(&c1), 1, 0, cf, 1) == RT_OK);
    rt_close_upvals(cf + (rt_addr)sizeof(TValue)); /* closes cells[1] only */
    rt_mknumber(cell_of(&cells[1]), 0);
    rt_mknil(cell_of(&out));
    rt_getupval((rt_addr)(uintptr_t)clvalue(&c1), 0, cell_of(&out));
    CHECK(nvalue(&out) == 5); /* cells[0] still open, reads through */
  }

  /* ---- M6d (D3): the allocation cap ---- */
  {
    /* A capped state: creation succeeds, allocation over the budget
       refuses, the refusal raises a catchable LUA_ERRMEM carrying stock
       5.1's bare "not enough memory" (no position — seterrorobj's
       literal), and the catch re-arms the refusal (emergency slack is
       one-time, not per-catch). */
    static char big[200 * 1024];
    lua_State *M;
    memset(big, 'x', sizeof big);
    rt_set_memlimit(64 * 1024);
    M = lua_newstate(rt_alloc, NULL);
    CHECK(M != NULL); /* state + core structs fit the budget */
    CHECK(lua_cpcall(M, oom_body, (void *)big) == LUA_ERRMEM);
    CHECK(lua_isstring(M, -1));
    CHECK(strcmp(lua_tostring(M, -1), "not enough memory") == 0);
    lua_pop(M, 1);
    /* catch re-arms: still over budget (used ≥ limit) → still refused */
    CHECK(lua_cpcall(M, oom_body, (void *)big) == LUA_ERRMEM);
    lua_pop(M, 1);
    /* small allocations keep working at/under the cap */
    CHECK(lua_cpcall(M, small_body, NULL) == 0);
    /* unbounded again */
    rt_set_memlimit(0);
    CHECK(lua_cpcall(M, oom_body, (void *)big) == 0);
    lua_close(M);
    /* the original state's allocator is unaffected by the second state's
       budget (rt_set_memlimit is process-global; engines create one VM
       per instance — documented posture, reset here for the rest of the
       suite) */
    rt_set_memlimit(0);
  }

  /* ---- M6d (D4): the deadline control block ---- */
  {
    char buf[64];
    rt_set_deadline(0);
    CHECK(rt_deadline_flag() == 0);
    CHECK(rt_ctrl_addr() == (rt_addr)(uintptr_t)&g_deadline_flag);
    /* the watchdog contract: a bare 4-byte store at rt_ctrl_addr() */
    *(int32_t *)(uintptr_t)rt_ctrl_addr() = 1;
    CHECK(rt_deadline_flag() == 1);
    CHECK(rt_deadline() == RT_ERR);
    CHECK(rt_err_pending() == 1);
    {
      int32_t n = rt_err_stage_copy((rt_addr)(uintptr_t)buf, sizeof buf);
      CHECK(n == (int32_t)strlen("context deadline exceeded"));
      buf[n] = '\0'; /* stage_copy copies err_buf_len bytes, no NUL */
      CHECK(strcmp(buf, "context deadline exceeded") == 0);
    }
    /* sticky: rt_deadline never re-stages over a pending error */
    CHECK(rt_deadline() == RT_ERR);
    rt_err_clear();
    rt_set_deadline(0);
    CHECK(rt_deadline_flag() == 0);
  }

  /* ---- row 38 fix (2026-09-19): fork-exact number→string ----
  ** gn_lua_number_to_string ports the interp oracle's LNumber.String()
  ** (goldens generated FROM the fork on darwin/arm64; the port was also
  ** swept against the oracle over 7.9M values — random bit patterns,
  ** binade/decimal boundaries — zero mismatches). Note 2^63 prints
  ** saturated (arm64 float64→int64): "9223372036854775807". */
  {
    static const struct { double v; const char *want; } gold[] = {
      {0x1.c6bf52634032p+49, "1000000000000100"},         /* c0071344 */
      {0x1.2aaaaaaaaaaabp+00, "1.1666666666666667"},      /* c0750767 */
      {0x1.010101010101p-07, "0.00784313725490196"},      /* c1034921 */
      {0x1p+53, "9007199254740992"},                      /* c0916336/c1066545 */
      {0x1.c6bf52634p+49, "1000000000000000"},            /* 1e15 full digits */
      {0x1.1c37937e08p+53, "10000000000000000"},
      {0x1.2d6878p+20, "1.2345675e+06"},                  /* %v goes e-form at 1e6 */
      {0x1.e847fp+19, "999999.5"},
      {0x1.4f8b588e368f1p-17, "1e-05"},                   /* …and below 1e-4 */
      {0x1.ad7f29abcaf48p-24, "1e-07"},
      {0x1p-1074, "5e-324"},                              /* smallest subnormal */
      {0x1.1ccf385ebc8ap+1023, "1e+308"},
      {0x1.5555555555555p-02, "0.3333333333333333"},
      {-0x1.5555555555555p-02, "-0.3333333333333333"},
      {0x0p+00, "0"},                                     /* -0.0 → "0" (row 42) */
      {0x1.0000000000001p-1022, "2.225073858507202e-308"},
      {0x1p+63, "9223372036854775807"},                   /* saturated 2^63 */
      {-0x1p+63, "-9223372036854775808"},
      {0x1.02207973f644p+63, "9.3e+18"},                  /* beyond ±2^63 */
      {-0x1.02207973f644p+63, "-9.3e+18"},
      {0x1.999999999999ap-04, "0.1"},
      {0x1.e848p+19, "1000000"},
      {0x1.b1ae4d6e2ef5p+69, "1e+21"},
      {0x1.0000000000001p+00, "1.0000000000000002"},
      {0x1.fffffffffffffp+1023, "1.7976931348623157e+308"},
      {INFINITY, "+Inf"},
      {-INFINITY, "-Inf"},
      {NAN, "NaN"},
    };
    size_t gi;
    char buf[64];
    for (gi = 0; gi < sizeof gold / sizeof gold[0]; gi++) {
      gn_lua_number_to_string(buf, gold[gi].v);
      if (strcmp(buf, gold[gi].want) != 0) {
        failures++;
        printf("FAIL %d: gnumfmt(%g) = %s want %s\n", __LINE__,
               gold[gi].v, buf, gold[gi].want);
      }
    }
    /* the lua_number2str hook is dialect-gated: stock %.14g for the
    ** clua oracle, fork texts for the wasm engine */
    {
      int prev_dialect = rt_gopher_dialect();
      rt_set_dialect(1);
      rt_gnumfmt(buf, 1e15 + 100);
      CHECK(strcmp(buf, "1000000000000100") == 0);
      rt_set_dialect(0);
      rt_gnumfmt(buf, 1e15 + 100);
      CHECK(strcmp(buf, "1.0000000000001e+15") == 0);
      rt_set_dialect(prev_dialect);
    }
  }

  if (failures == 0) printf("RT-NATIVE PASS\n");
  else printf("RT-NATIVE FAIL (%d)\n", failures);
  return failures;
}
