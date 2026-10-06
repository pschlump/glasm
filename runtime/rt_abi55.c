/*
** runtime/rt_abi55.c — ABI v4 implementation on the STOCK Lua 5.5.1 tree
** (runtime/lua55/, zero patches — U2d). Ported from rt_abi.c (the 5.1
** ABI v3); the M5/M6 lessons carry unchanged:
**
**   - reentrancy law: any rt_* body that luaD_calls can re-enter compiled
**     code through the trampoline — argument statics read after the call
**     must live on the C stack (each body copies what it needs first);
**   - savestack rebasing (ledger row 31): a raw StkId dangles across a
**     nested luaD_growstack — only the offset survives;
**   - the upvalue registry is rt-owned (never L->openupval: stock's
**     correctstack/relativestack pass would re-base those pointers into
**     the Lua stack, and luaF_close's ordering would close wasm frames
**     early — plan §3.4);
**   - a longjmp never crosses the Go boundary: every dispatch runs under
**     luaD_rawrunprotected at the adapter (trampoline) level.
**
** What is deliberately NOT here (U6, plan §4 U2d): the golua55 message
** dialect + gnumfmt (stock 5.5 texts and luaO_tostringbuff only), the
** sandbox/globals-lockdown family, the budget allocator + deadline block,
** the host-fn seam, and the error() line-stack (positions for error()
** called from wasm frames are a U2e/U5 seam — ledger row 60).
*/

#ifndef LUA_CORE
#define LUA_CORE /* we link core internals; luaO_arith/luaF_* need it */
#endif
#include <stdint.h>
#include <stdlib.h>
#include <stdio.h>
#include <string.h>

#include "rt_abi55.h"
#include "lua55/src/lauxlib.h"
#include "lua55/src/lapi.h"
#include "lua55/src/ldo.h"
#include "lua55/src/lfunc.h"
#include "lua55/src/lgc.h"
#include "lua55/src/lobject.h"
#include "lua55/src/lstate.h"
#include "lua55/src/lstring.h"
#include "lua55/src/ltable.h"
#include "lua55/src/ltm.h"
#include "lua55/src/ldebug.h"
#include "lua55/src/lvm.h"

/* layout contract of the whole ABI (wasm32 AND the native64 test build) */
_Static_assert(sizeof(TValue) == 16, "TValue must be 16 bytes");
_Static_assert(sizeof(lua_Integer) == 8, "integers are i64");
_Static_assert(sizeof(lua_Number) == 8, "floats are f64");

/* the frozen cell tags (rt_abi55.h) pinned against the vendored tree */
_Static_assert(RT_VNUMINT == LUA_VNUMINT && RT_VNUMFLT == LUA_VNUMFLT &&
                   RT_VSHRSTR == LUA_VSHRSTR && RT_VLNGSTR == LUA_VLNGSTR &&
                   RT_VTABLE == LUA_VTABLE && RT_VLCL == LUA_VLCL &&
                   RT_VLCF == LUA_VLCF && RT_VCCL == LUA_VCCL &&
                   RT_VUSERDATA == LUA_VUSERDATA &&
                   RT_VLIGHTUSERDATA == LUA_VLIGHTUSERDATA &&
                   RT_VTHREAD == LUA_VTHREAD && RT_VFALSE == LUA_VFALSE &&
                   RT_VTRUE == LUA_VTRUE && RT_VNIL == LUA_VNIL &&
                   RT_VEMPTY == LUA_VEMPTY && RT_VABSTKEY == LUA_VABSTKEY &&
                   RT_ISCOLLECTABLE == BIT_ISCOLLECTABLE,
               "cell tags must mirror lobject.h");

/* the arith op codes must be lua.h's (lobject.c dispatches on them) */
_Static_assert(RT_OP_ADD == LUA_OPADD && RT_OP_MOD == LUA_OPMOD &&
                   RT_OP_IDIV == LUA_OPIDIV && RT_OP_BAND == LUA_OPBAND &&
                   RT_OP_SHR == LUA_OPSHR && RT_OP_UNM == LUA_OPUNM &&
                   RT_OP_BNOT == LUA_OPBNOT,
               "rt arith ops must mirror lua.h");

static lua_State *curL;
static int rt_wasm_depth; /* trampoline nesting (RTW_MAX_DEPTH guard) */

/* U5e (row 85): the protect shadow — every protect WE install (rt_run_raw's
   luaD_rawrunprotected, the msgh dispatch in rtw_reraise) records its
   lua_longjmp here; the push reads L->errorJmp from inside the body, where
   it IS ours, and the pop runs where control always resumes (after
   rawrunprotected returns — a longjmp lands inside its LUAI_TRY and returns
   normally, so the pairing holds on every exit path). rtw_reraise compares
   L->errorJmp against the shadow to decide whether its throw will be caught
   by a STOCK protect (a pcall/resume — run the message handler now, stock's
   luaG_errormsg-at-raise for errors that entered flight by direct staging)
   or by one of ours (the error keeps cascading as staged returns; a later
   reraise decides). Saturation answers "ours" — never risk a double run. */
#define RT_RP_MAX 512 /* > RTW_MAX_DEPTH * in-flight rt_run per level */
static void *rt_rp_stack[RT_RP_MAX];
static int rt_rp_n;

static void rt_rp_push(lua_State *L) {
  if (rt_rp_n < RT_RP_MAX) rt_rp_stack[rt_rp_n] = L->errorJmp;
  rt_rp_n++;
}

static int rt_rp_ours(void *lj) {
  int i;
  if (lj == NULL) return 0;
  if (rt_rp_n > RT_RP_MAX) return 1; /* saturated: conservative */
  for (i = 0; i < rt_rp_n; i++)
    if (rt_rp_stack[i] == lj) return 1;
  return 0;
}

/* rt_curL: the seam file (rt_seam55.c) needs the running state for its
   rt_sea_main bottom entry; curL is static here, so hand it out */
lua_State *rt_curL(void) { return curL; }

static int rt_wasm_trampoline(lua_State *L); /* the CClosure seam (below) */
static void rtw_dispatch_body(lua_State *L, void *ud); /* U5e seam order */
static void rtw_reraise(lua_State *L);

/* ---- staged-error machinery (v3 contract, unchanged) ----
**
** Sticky until rt_err_clear; err_value carries the EXACT error TValue
** (the trampoline re-raises FROM it, so non-string objects survive);
** err_buf holds message bytes when the value is a string. err_prefixed:
** the raiser already decided the position (error() levels / rt_error
** built one / the value is non-string) — no rt prefix on re-staging.
**
** v4 rule (replaces v3's rt_pcall_caught patch in ldo.c): staging is
** cleared exactly when the error crosses from wasm-land into C-land —
** the trampoline moves the value onto the Lua stack, clears, throws.
** A Lua-level pcall that catches therefore finds the runtime clean; an
** rt_run that catches a throw originating in wasm-land re-stages from
** the pushed value. Invariant: pending <=> the error is in flight
** between wasm frames. */

static TValue err_value;
static char err_buf[1024];
static int err_buf_len, err_pending, err_prefixed;

/* the raise line for the host's error-position rendering (M8 surface
   kept); the FIRST staging of an in-flight error wins */
static int err_line, err_line_set;

static char chunk_name[256] = "script";
static int chunk_name_len = 6;

static void stage_err_line(int line) {
  if (!err_line_set) {
    err_line = line;
    err_line_set = 1;
  }
}

/* stage_tvalue fills value/buf/prefixed from a TValue (NOT pending — the
   callers decide; U4f's error-path close loop re-stages a replaced object
   through it with the same prefix rules) */
static void stage_tvalue(const TValue *ev) {
  err_value = *ev;
  err_buf_len = 0;
  err_buf[0] = '\0';
  if (ttisstring(ev)) {
    const char *s = getstr(tsvalue(ev));
    size_t n = tsslen(tsvalue(ev));
    if (n > sizeof err_buf - 1) n = sizeof err_buf - 1;
    memcpy(err_buf, s, n);
    err_buf[n] = '\0';
    err_buf_len = (int)n;
    /* U2e (row 60's error-position seam): a message that already carries
       this chunk's position prefix — error() built it, or a nested
       rt_run re-raise staged it — must not gain another prefix at every
       level (stock never re-prefixes; v3 needed the dialect line-stack
       for this, the zero-patch tree detects the prefix instead).
       err_prefixed is a property of THIS value, recomputed per staging:
       the tbc error path stages several objects back-to-back without an
       rt_err_clear between them (the in-flight error, then a failing
       __close's replacement), so a stale flag from the previous staging
       would suppress the position on the next one (U4f). */
    err_prefixed = 0;
    if (chunk_name_len > 0 && err_buf_len > chunk_name_len &&
        memcmp(err_buf, chunk_name, (size_t)chunk_name_len) == 0 &&
        err_buf[chunk_name_len] == ':')
      err_prefixed = 1;
  } else {
    err_prefixed = 1; /* non-string: the engine renders the value */
  }
}

static void stage_error(void) {
  const TValue *ev = s2v(curL->top.p - 1);
  stage_tvalue(ev);
  curL->top.p--;
  err_pending = 1;
}

/* LUA_ERRMEM/LUA_ERRERR land with NO value on the Lua stack (stock
   luaD_seterrorobj only runs at pcall-family recoveries): stage the bare
   literal exactly as seterrorobj would. Stock ERRMEM/ERRERR carry no
   position, hence err_prefixed. */
static void stage_c_error(int status) {
  const char *msg = (status == LUA_ERRMEM) ? "not enough memory"
                                           : "error in error handling";
  size_t n = strlen(msg);
  TString *ts = luaS_newlstr(curL, msg, n);
  setsvalue(curL, &err_value, ts);
  memcpy(err_buf, msg, n + 1);
  err_buf_len = (int)n;
  err_prefixed = 1;
  err_pending = 1;
  rt_sea_msgh_ran_set(0); /* fresh error: its handler has not run */
}

/* stage a string error in one step (no Lua stack traffic — used by the
   trampoline's guard paths) */
static void stage_str_error(const char *msg, int prefixed) {
  size_t n = strlen(msg);
  TString *ts = luaS_newlstr(curL, msg, n);
  setsvalue(curL, &err_value, ts);
  if (n > sizeof err_buf - 1) n = sizeof err_buf - 1;
  memcpy(err_buf, msg, n);
  err_buf[n] = '\0';
  err_buf_len = (int)n;
  err_prefixed = prefixed;
  err_pending = 1;
  rt_sea_msgh_ran_set(0); /* fresh error: its handler has not run */
}

int32_t rt_err_pending(void) { return err_pending; }

void rt_err_clear(void) {
  err_pending = 0;
  err_buf_len = 0;
  err_line = 0;
  err_line_set = 0;
  err_prefixed = 0;
  setnilvalue(&err_value);
  rt_sea_nopos_set(0); /* U5e: the verdict is per-staging */
  rt_sea_msgh_ran_set(0); /* row 85: no in-flight error anymore */
}

int32_t rt_err_stage_copy(rt_addr dst, int32_t cap) {
  int32_t n = err_buf_len < cap ? err_buf_len : cap;
  if (n > 0) memcpy((void *)(size_t)dst, err_buf, (size_t)n);
  return n;
}

int32_t rt_err_line(void) { return err_line; }

rt_addr rt_err_value_ptr(void) { return (rt_addr)(size_t)&err_value; }

int32_t rt_err_stage_value(rt_addr dst, int32_t cap) {
  if (cap < (int32_t)sizeof(TValue)) return 0;
  memcpy((void *)(size_t)dst, &err_value, sizeof err_value);
  return (int32_t)sizeof(TValue);
}

/* ---- the protect trampoline (v3's rt_run) ----
**
** Returns RT_OK, or RT_ERR with the error staged; a string error whose
** raiser did not decide a position gets the "chunk:line: " prefix (built
** in the buffer; the staged TValue is rebuilt from the prefixed bytes so
** a catching pcall sees the same string — row 28's lesson).
*/

typedef void (*body_fn)(void);
static body_fn cur_body;

static void protect_trampoline(lua_State *L, void *ud) {
  (void)ud;
  rt_rp_push(L); /* row 85: L->errorJmp is ours inside the body */
  cur_body();
}

/* rt_run_raw is the protection without rt_run's err_pending gate, staging
   or prefixing (U4f): bodies that must run while an error is already
   staged — the error-path close loop — fold failures themselves. It does
   NOT touch the hint slots: rt_run arms them right before calling here,
   and re-arming "none" inside would strip every entry's hints (the raw
   close callers arm "none" themselves — see rt_close_call). */
static int rt_run_raw(body_fn fn) {
  TStatus status;
  cur_body = fn;
  lua_lock(curL);
  status = luaD_rawrunprotected(curL, protect_trampoline, NULL);
  if (rt_rp_n > 0) rt_rp_n--; /* pairs the push even across a longjmp */
  lua_unlock(curL);
  return (int)status;
}

/* rt_run (U5e): every protected entry funnels its varinfo hints through
   here — armed BEFORE the body runs, so the seam's typeerror/callerror
   replicas read exactly this entry's precomputed answers and a raise can
   never see a stale hint from an earlier dispatch. tgtop is the entry's
   typeerror operand (the value its luaG_typeerror raises would name);
   the table-op entries pass their operand static, everyone else NULL. */
static int rt_run(body_fn fn, int32_t line, int32_t h1, int32_t h2,
                  int32_t hc, const void *tgtop) {
  int prefixed;
  if (err_pending) return RT_ERR;
  rt_sea_hint_set(h1, h2, hc, tgtop);
  TStatus status = (TStatus)rt_run_raw(fn);
  if (status != LUA_OK) {
    if (status == LUA_ERRMEM || status == LUA_ERRERR)
      stage_c_error(status); /* no stack value: stage the literal */
    else
      stage_error(); /* value + message bytes */
    prefixed = err_prefixed;
    /* U5e (row 60): rt_sea_nopos — the interposed lua_error's verdict
       that this string was raised WITHOUT a resolvable position (level
       0, past the bottom, a where() miss): stock adds no prefix there,
       so neither does the rt. Core raises (luaG_runerror through the
       seam) clear the flag and keep the rt prefix. */
    if (line != 0 && !prefixed && !rt_sea_nopos() && err_buf_len > 0) {
      char tmp[sizeof err_buf];
      int w = snprintf(tmp, sizeof tmp, "%.*s:%d: %s", chunk_name_len,
                       chunk_name, (int)line, err_buf);
      if (w > 0) {
        size_t n = (size_t)w;
        if (n >= sizeof tmp) n = sizeof tmp - 1;
        memcpy(err_buf, tmp, n);
        err_buf[n] = '\0';
        err_buf_len = (int)n;
        err_prefixed = 1; /* sticky: outer re-stagings must not re-prefix */
        if (ttisstring(&err_value))
          setsvalue(curL, &err_value,
                    luaS_newlstr(curL, err_buf, (size_t)err_buf_len));
      }
    }
    stage_err_line((int)line);
    return RT_ERR;
  }
  return RT_OK;
}

/* ---- ABI surface ---- */

int32_t rt_abi_version(void) { return LUA_RT_ABI; }

void rt_set_state(rt_addr p) {
  curL = (lua_State *)(size_t)p;
  err_pending = 0;
  err_buf_len = 0;
  err_prefixed = 0;
  err_line = 0;
  err_line_set = 0;
  setnilvalue(&err_value);
  rt_wasm_depth = 0;
  rt_sea_reset(); /* U5e: no shadow entries survive a state reset */
  /* U2e: make the v1 "GC stopped per run" posture airtight. The host
     stops the collector (collectgarbage('stop') => GCSTPUSR), but
     5.5's luaL_pushresult calls lua_gc(LUA_GCSTEP) after building any
     box-backed buffer (strings > LUAL_BUFFERSIZE), and the STEP option
     force-clears gcstp to run a collection EVEN under a user stop — a
     sweep would free every value held only from wasm cells (the pool
     and frames are not GC roots; the arena lifecycle is U6). The
     internal GCSTPGC bit makes lua_gc refuse ALL options (lapi.c's
     entry guard), including the forced step, so no collection can ever
     start. collectgarbage turns inert (returns -1); no corpus observes
     it (the wasm corpora never call it — the 5.1 blob's dialect made
     the same surface unobservable). The U6 arena retires this freeze
     with real rooting. */
  G(curL)->gcstp |= GCSTPGC;
  rt_rp_n = 0; /* row 85: no protects of ours survive a state reset */
}

/* ---- value construction ---- */

void rt_mknumber(rt_addr cell, double v) {
  setfltvalue((TValue *)(size_t)cell, v);
}

void rt_mkinteger(rt_addr cell, int64_t v) {
  setivalue((TValue *)(size_t)cell, (lua_Integer)v);
}

void rt_mkbool(rt_addr cell, int32_t b) {
  if (b)
    setbtvalue((TValue *)(size_t)cell);
  else
    setbfvalue((TValue *)(size_t)cell);
}

void rt_mknil(rt_addr cell) { setnilvalue((TValue *)(size_t)cell); }

int32_t rt_intern(rt_addr cell, rt_addr ptr, int32_t len) {
  if (err_pending) return RT_ERR;
  TString *ts = luaS_newlstr(curL, (const char *)(size_t)ptr, (size_t)len);
  setsvalue(curL, (TValue *)(size_t)cell, ts);
  return RT_OK;
}

int32_t rt_newtable(rt_addr cell, int32_t narr, int32_t nrec) {
  if (err_pending) return RT_ERR;
  Table *t = luaH_new(curL);
  if (narr > 0 || nrec > 0) /* pre-size hints (NEWTABLE's vB/vC) */
    luaH_resize(curL, t, (unsigned)(narr > 0 ? narr : 0),
                (unsigned)(nrec > 0 ? nrec : 0));
  sethvalue(curL, (TValue *)(size_t)cell, t);
  return RT_OK;
}

/* ---- table access (metamethod-complete; the runtime's own lvm runs
   __index/__newindex — no callback crosses the ABI) ---- */

static TValue gt_t, gt_k, *gt_dst;

static void gettable_body(void) {
  TValue *dst = gt_dst; /* reentrancy: an __index metamethod on a wasm
                           closure re-enters the ABI and overwrites the
                           statics before we read them back */
  luaD_checkstack(curL, 2);
  setobj2s(curL, curL->top.p, &gt_t);
  curL->top.p++;
  setobj2s(curL, curL->top.p, &gt_k);
  curL->top.p++;
  lua_gettable(curL, -2);
  *dst = *s2v(curL->top.p - 1);
  curL->top.p -= 2; /* table + result */
}

int32_t rt_gettable(rt_addr tblcell, rt_addr keycell, rt_addr dstcell,
                    int32_t line, int32_t hobj, int32_t hcall) {
  gt_t = *(TValue *)(size_t)tblcell;
  gt_k = *(TValue *)(size_t)keycell;
  gt_dst = (TValue *)(size_t)dstcell;
  return rt_run(gettable_body, line, hobj, -1, hcall, &gt_t);
}

static TValue st_t, st_k, st_v;

static void settable_body(void) {
  luaD_checkstack(curL, 3);
  setobj2s(curL, curL->top.p, &st_t);
  curL->top.p++;
  setobj2s(curL, curL->top.p, &st_k);
  curL->top.p++;
  setobj2s(curL, curL->top.p, &st_v);
  curL->top.p++;
  lua_settable(curL, -3); /* pops key + value */
  curL->top.p--;          /* the table */
}

int32_t rt_settable(rt_addr tblcell, rt_addr keycell, rt_addr valcell,
                    int32_t line, int32_t hobj, int32_t hcall) {
  st_t = *(TValue *)(size_t)tblcell;
  st_k = *(TValue *)(size_t)keycell;
  st_v = *(TValue *)(size_t)valcell;
  return rt_run(settable_body, line, hobj, -1, hcall, &st_t);
}

/* ---- arithmetic: 5.5's own engine, stock texts ----
**
** luaO_arith (lobject.c) is the complete stock dispatcher: raw int/float
** subtype rules, '/'-always-float, floor // and %, luaV_shiftl shifts,
** string operands through the string metatable (the 5.4+ coercion model
** — exactly what U3 will pin on the interp), and metamethods via
** luaT_trybinTM with stock error texts. The v3 hand-rolled float engine
** and its dialect layer are gone. */

static TValue ar_l, ar_r, *ar_dst;
static int ar_op;

static void arith_body(void) {
  TValue *dst = ar_dst; /* reentrancy: a metamethod may re-enter the ABI */
  StkId res = curL->top.p; /* scratch result slot (below the TM window) */
  luaD_checkstack(curL, 1);
  curL->top.p++;
  luaO_arith(curL, ar_op, &ar_l, &ar_r, res);
  *dst = *s2v(res);
  curL->top.p--;
}

int32_t rt_arith(int32_t op, rt_addr lhscell, rt_addr rhscell, rt_addr dstcell,
                 int32_t line, int32_t h1, int32_t h2, int32_t hcall) {
  ar_op = (int)op;
  ar_l = *(TValue *)(size_t)lhscell;
  ar_r = *(TValue *)(size_t)rhscell;
  ar_dst = (TValue *)(size_t)dstcell;
  return rt_run(arith_body, line, h1, h2, hcall, NULL);
}

/* ---- length (#): luaV_objlen (strings/tables integer, __len honored) */

static TValue len_v, *len_dst;

static void len_body(void) {
  TValue *dst = len_dst; /* reentrancy (metamethod) */
  StkId res = curL->top.p;
  luaD_checkstack(curL, 1);
  curL->top.p++;
  luaV_objlen(curL, res, &len_v);
  *dst = *s2v(res);
  curL->top.p--;
}

int32_t rt_len(rt_addr cell, rt_addr dstcell, int32_t line, int32_t hobj,
               int32_t hcall) {
  len_v = *(TValue *)(size_t)cell;
  len_dst = (TValue *)(size_t)dstcell;
  return rt_run(len_body, line, hobj, -1, hcall, &len_v);
}

/* ---- comparisons: luaV_equalobj/lessthan/lessequal (mixed int/float
   order compare is stock's l_intfitsf+floor/ceil exact algorithm) ---- */

static TValue cmp_a, cmp_b, *cmp_dst;
static int cmp_op; /* 0 = ==, 1 = <, 2 = <= */

static void cmp_body(void) {
  int res;
  TValue *dst = cmp_dst; /* reentrancy (metamethod) */
  switch (cmp_op) {
  case 0:
    res = luaV_equalobj(curL, &cmp_a, &cmp_b);
    break;
  case 1:
    res = luaV_lessthan(curL, &cmp_a, &cmp_b);
    break;
  default:
    res = luaV_lessequal(curL, &cmp_a, &cmp_b);
    break;
  }
  if (res)
    setbtvalue(dst);
  else
    setbfvalue(dst);
}

static int cmp_entry(int op, rt_addr acell, rt_addr bcell, rt_addr dstcell,
                     int32_t line) {
  cmp_a = *(TValue *)(size_t)acell;
  cmp_b = *(TValue *)(size_t)bcell;
  cmp_dst = (TValue *)(size_t)dstcell;
  cmp_op = op;
  return rt_run(cmp_body, line, -1, -1, -1, NULL);
}

int32_t rt_eq(rt_addr a, rt_addr b, rt_addr dst, int32_t line) {
  return cmp_entry(0, a, b, dst, line);
}
int32_t rt_lt(rt_addr a, rt_addr b, rt_addr dst, int32_t line) {
  return cmp_entry(1, a, b, dst, line);
}
int32_t rt_le(rt_addr a, rt_addr b, rt_addr dst, int32_t line) {
  return cmp_entry(2, a, b, dst, line);
}

/* ---- concat: cells contiguous, lowest first. 5.5's luaV_concat (unlike
   5.1's) ADJUSTS L->top itself — n values collapse to one result at
   top-1 (its lvm callers rely on "luaV_concat ensures correct top") ---- */

static TValue *cc_cells;
static int cc_n;
static TValue *cc_dst;

static void concat_body(void) {
  int i;
  TValue *dst = cc_dst; /* reentrancy (__concat may re-enter the ABI) */
  int n = cc_n;
  luaD_checkstack(curL, n + 1);
  for (i = 0; i < n; i++) {
    setobj2s(curL, curL->top.p, &cc_cells[i]);
    curL->top.p++;
  }
  luaV_concat(curL, n);
  *dst = *s2v(curL->top.p - 1);
  curL->top.p--; /* the single result */
}

int32_t rt_concat(rt_addr cells, int32_t count, rt_addr dstcell, int32_t line,
                  int32_t hbase, int32_t hcall) {
  if (count < 2) {
    if (count == 1)
      *(TValue *)(size_t)dstcell = *(TValue *)(size_t)cells;
    return RT_OK;
  }
  cc_cells = (TValue *)(size_t)cells;
  cc_n = (int)count;
  cc_dst = (TValue *)(size_t)dstcell;
  /* U5e (row 71): the concat window for luaG_concaterror — operand 0
     lands at the CURRENT top (the body's pushes start there); the
     savestack offset survives the stack growth a metamethod may cause
     (row 31). hcall is funcnamefromcode's OP_CONCAT answer
     ("metamethod 'concat'" — the non-callable-__concat call error). */
  rt_sea_concat_push(hbase, (int64_t)savestack(curL, curL->top.p));
  {
    int st = rt_run(concat_body, line, -1, -1, hcall, NULL);
    rt_sea_concat_pop();
    return st;
  }
}

/* ---- calls: the universal fallback (v3 posture). Results overwrite the
   arg cells; want<0 = multret with the negative encoded count returned */

static TValue ca_f;
static TValue *ca_args;
static int ca_n, ca_w, ca_nres;

/* Reentrancy law: the callee may be a wasm closure re-entering the ABI
   (including another rt_call) before this body resumes — no static may
   be read after luaD_callnoyield; everything lives on the C stack and
   only ca_nres is written back at the very end. */
static void call_body(void) {
  int i, n = ca_n, w = ca_w, nres;
  TValue *args = ca_args;
  StkId func;
  luaD_checkstack(curL, n + 1);
  setobj2s(curL, curL->top.p, &ca_f);
  curL->top.p++;
  for (i = 0; i < n; i++) {
    setobj2s(curL, curL->top.p, &args[i]);
    curL->top.p++;
  }
  func = curL->top.p - n - 1;
  /* Ledger row 31: the callee may nest deep enough to grow — and MOVE —
     the Lua stack. A raw StkId dangles across that; only the offset
     survives (savestack/restorestack). */
  {
    ptrdiff_t funcr = savestack(curL, func);
    luaD_callnoyield(curL, func, w < 0 ? LUA_MULTRET : w);
    func = restorestack(curL, funcr);
  }
  /* 5.5 return convention (changed from 5.1): results are moved down
     onto the func slot itself — OP_RETURN1's fast path does
     setobjs2s(base-1, ra), and moveresults targets ci->func — so the
     k-th result reads at func+k with top == func+nres. */
  nres = cast_int(curL->top.p - func);
  if (nres < 0) nres = 0;
  if (w >= 0) nres = w;
  for (i = 0; i < nres; i++) args[i] = *s2v(func + i);
  curL->top.p = func;
  ca_nres = nres;
}

int32_t rt_call(rt_addr funcell, rt_addr argcells, int32_t nargs, int32_t want,
                int32_t line, int32_t hcallee) {
  int w = (int)want, nres;
  ca_f = *(TValue *)(size_t)funcell;
  ca_args = (TValue *)(size_t)argcells;
  ca_n = (int)nargs;
  ca_w = w;
  ca_nres = 0;
  int st = rt_run(call_body, line, -1, -1, hcallee, NULL);
  if (st != RT_OK) return RT_ERR;
  nres = ca_nres; /* read before encoding — a nested call may follow */
  return w < 0 ? -(nres + 1) : RT_OK; /* encode count; -1 means zero */
}

int32_t rt_call_count(int32_t encoded) { return -(encoded + 1); }

/* ---- numeric-for preparation: a verbatim port of lvm.c's forprep +
   forlimit (both static there) onto the three control cells. Stock
   rewrites the cells: integer loops become (count, step, init) — the
   counter holds iterations-1 and never wraps (row 57's edge, stock's
   fix); float loops become (limit, step, init), all forced float. */

static int rt_forlimit(lua_State *L, lua_Integer init, const TValue *lim,
                       lua_Integer *p, lua_Integer step) {
  if (!luaV_tointeger(lim, p, (step < 0 ? F2Iceil : F2Ifloor))) {
    /* not coercible to an integer */
    lua_Number flim; /* try to convert to float */
    if (!tonumber(lim, &flim)) /* cannot convert to float? */
      luaG_forerror(L, lim, "limit");
    /* else 'flim' is a float out of integer bounds */
    if (luai_numlt(0, flim)) { /* if it is positive, it is too large */
      if (step < 0) return 1;  /* initial value must be less than it */
      *p = LUA_MAXINTEGER;    /* truncate */
    } else {                  /* it is less than min integer */
      if (step > 0) return 1; /* initial value must be greater than it */
      *p = LUA_MININTEGER;   /* truncate */
    }
  }
  return (step > 0 ? init > *p : init < *p); /* not to run? */
}

/* returns 1 to skip the loop (lvm.c forprep's contract) */
static int rt_forprep_aux(lua_State *L, TValue *pinit, TValue *plimit,
                          TValue *pstep) {
  if (ttisinteger(pinit) && ttisinteger(pstep)) { /* integer loop? */
    lua_Integer init = ivalue(pinit);
    lua_Integer step = ivalue(pstep);
    lua_Integer limit;
    if (step == 0)
      luaG_runerror(L, "'for' step is zero");
    if (rt_forlimit(L, init, plimit, &limit, step))
      return 1; /* skip the loop */
    else {      /* prepare loop counter */
      lua_Unsigned count;
      if (step > 0) { /* ascending loop? */
        count = l_castS2U(limit) - l_castS2U(init);
        if (step != 1) /* avoid division in the too common case */
          count /= l_castS2U(step);
      } else { /* step < 0; descending loop */
        count = l_castS2U(init) - l_castS2U(limit);
        /* 'step+1' avoids negating 'mininteger' */
        count /= l_castS2U(-(step + 1)) + 1u;
      }
      chgivalue(pinit, l_castU2S(count)); /* change init to count */
      setivalue(plimit, step);            /* change limit to step */
      chgivalue(pstep, init);             /* change step to init */
    }
  } else { /* try making all values floats */
    lua_Number init;
    lua_Number limit;
    lua_Number step;
    if (l_unlikely(!tonumber(plimit, &limit)))
      luaG_forerror(L, plimit, "limit");
    if (l_unlikely(!tonumber(pstep, &step)))
      luaG_forerror(L, pstep, "step");
    if (l_unlikely(!tonumber(pinit, &init)))
      luaG_forerror(L, pinit, "initial value");
    if (step == 0)
      luaG_runerror(L, "'for' step is zero");
    if (luai_numlt(0, step) ? luai_numlt(limit, init)
                            : luai_numlt(init, limit))
      return 1; /* skip the loop */
    else {      /* make sure all values are floats */
      setfltvalue(pinit, limit);
      setfltvalue(plimit, step);
      setfltvalue(pstep, init); /* control variable */
    }
  }
  return 0;
}

static TValue *fp_cells;
static int fp_skip;

static void forprep_body(void) {
  fp_skip = rt_forprep_aux(curL, &fp_cells[0], &fp_cells[1], &fp_cells[2]);
}

int32_t rt_forprep(rt_addr cells, int32_t line) {
  if (err_pending) return RT_ERR;
  fp_cells = (TValue *)(size_t)cells;
  fp_skip = 0;
  int st = rt_run(forprep_body, line, -1, -1, -1, NULL);
  if (st != RT_OK) return RT_ERR;
  return fp_skip ? RT_FORPREP_SKIP : RT_OK;
}

/* ---- scratch: frame/cell regions in the shared heap (v1: malloc) ---- */

int32_t rt_frame_alloc(int32_t bytes) {
  void *p = malloc((size_t)bytes);
  return (int32_t)(size_t)p;
}

/* ---- raising from emitted code ---- */

void rt_error(rt_addr msgptr, int32_t msglen, int32_t line) {
  int w = snprintf(err_buf, sizeof err_buf, "%.*s:%d: %.*s", chunk_name_len,
                   chunk_name, (int)line, (int)msglen,
                   (const char *)(size_t)msgptr);
  err_buf_len = w > 0 ? w : 0;
  err_prefixed = 1;
  err_pending = 1;
  rt_sea_msgh_ran_set(0); /* fresh error: its handler has not run */
  stage_err_line((int)line);
  /* v4: stage the exact string value too — the trampoline re-raises from
     err_value, so a pcall that catches a wasm error() sees the string */
  setsvalue(curL, &err_value,
            luaS_newlstr(curL, err_buf, (size_t)err_buf_len));
}

/* ---- globals ---- */

static TValue gg_k, *gg_dst, sg_k, sg_v;

static void getglobal_body(void) {
  TValue *dst = gg_dst;
  const char *name = getstr(tsvalue(&gg_k));
  luaD_checkstack(curL, 1);
  lua_getglobal(curL, name); /* pushes exactly the value */
  *dst = *s2v(curL->top.p - 1);
  curL->top.p -= 1;
}

int32_t rt_getglobal(rt_addr dstcell, rt_addr keycell, int32_t line) {
  gg_k = *(TValue *)(size_t)keycell;
  gg_dst = (TValue *)(size_t)dstcell;
  return rt_run(getglobal_body, line, -1, -1, -1, NULL);
}

static void setglobal_body(void) {
  const char *name = getstr(tsvalue(&sg_k));
  luaD_checkstack(curL, 1);
  setobj2s(curL, curL->top.p, &sg_v);
  curL->top.p++;
  lua_setglobal(curL, name); /* pops the value */
}

int32_t rt_setglobal(rt_addr keycell, rt_addr valcell, int32_t line) {
  sg_k = *(TValue *)(size_t)keycell;
  sg_v = *(TValue *)(size_t)valcell;
  return rt_run(setglobal_body, line, -1, -1, -1, NULL);
}

void rt_set_chunkname(rt_addr ptr, int32_t len) {
  int32_t n = len;
  if (n > (int32_t)sizeof chunk_name - 1) n = (int32_t)sizeof chunk_name - 1;
  memcpy(chunk_name, (const void *)(size_t)ptr, (size_t)n);
  chunk_name[n] = '\0';
  chunk_name_len = (int)n;
}

const char *rt_chunkname_ptr(void) { return chunk_name; }
int32_t rt_chunkname_len(void) { return chunk_name_len; }

/* ---- the wasm-proto registry (v3 shape minus the C Proto: 5.5's Proto
   has no wasm_idx field and the tree stays zero-patch — the Lua-visible
   identity of a compiled proto is the CClosure the trampoline hangs off) */

struct rt_wasm_md {
  int numparams, isvararg, nupvalues, framecells;
  struct { int instack, idx; } *uv; /* capture descriptors (nupvalues) */
};

static struct rt_wasm_md *rt_md;
static int rt_md_n, rt_md_cap;

int32_t rt_wasm_proto(int32_t idx, int32_t numparams, int32_t isvararg,
                      int32_t nupvalues, int32_t framecells) {
  struct rt_wasm_md *m;
  if (idx < 0) return RT_ERR;
  if (idx >= rt_md_cap) {
    int ncap = rt_md_cap ? rt_md_cap * 2 : 16;
    while (ncap <= idx) ncap *= 2;
    struct rt_wasm_md *nm =
        (struct rt_wasm_md *)realloc(rt_md, (size_t)ncap * sizeof *nm);
    if (nm == NULL) return RT_ERR;
    rt_md = nm;
    rt_md_cap = ncap;
  }
  m = &rt_md[idx];
  m->numparams = numparams;
  m->isvararg = isvararg;
  m->nupvalues = nupvalues;
  m->framecells = framecells;
  m->uv = NULL;
  if (nupvalues > 0) {
    m->uv = (void *)calloc((size_t)nupvalues, sizeof *m->uv);
    if (m->uv == NULL) return RT_ERR;
  }
  if (idx >= rt_md_n) rt_md_n = idx + 1;
  return RT_OK;
}

void rt_wasm_upval(int32_t protoidx, int32_t uvidx, int32_t instack,
                   int32_t idx) {
  if (protoidx < 0 || protoidx >= rt_md_n) return;
  struct rt_wasm_md *m = &rt_md[protoidx];
  if (uvidx < 0 || uvidx >= m->nupvalues) return;
  m->uv[uvidx].instack = instack;
  m->uv[uvidx].idx = idx;
}

int32_t rt_wasm_count(void) { return rt_md_n; }

/* ---- frame stack: chunked bump region in shared memory; malloc'd,
   never realloc'd, so live frames below the cursor never move ---- */

struct rt_frchunk {
  struct rt_frchunk *prev;
  rt_addr base;
  uint32_t cap, used;
};
static struct rt_frchunk *rt_fr;

/* size, bump and arg-fill a frame from a TValue array (the trampoline
   passes the L stack; the M5c tail restage passes the staging buffer). */
static rt_addr rt_push_frame_from(int mdidx, int nargs, const TValue *src) {
  struct rt_wasm_md *m;
  uint32_t need;
  rt_addr frame;
  int np, nv, i;

  m = &rt_md[mdidx];
  nv = (m->isvararg && nargs > m->numparams) ? nargs - m->numparams : 0;
  need = (uint32_t)sizeof(TValue) * (uint32_t)(m->framecells + 1 + nv);
  if (rt_fr == NULL || rt_fr->cap - rt_fr->used < need) {
    uint32_t cap = need > (1u << 20) ? need * 2 : (1u << 20);
    struct rt_frchunk *ch = (struct rt_frchunk *)malloc(sizeof *ch);
    void *mem = malloc(cap);
    if (ch == NULL || mem == NULL) {
      free(ch);
      free(mem);
      return 0;
    }
    ch->prev = rt_fr;
    ch->base = (rt_addr)(size_t)mem;
    ch->cap = cap;
    ch->used = 0;
    rt_fr = ch;
  }
  frame = rt_fr->base + rt_fr->used;
  rt_fr->used += need;

  /* copy args: params -> frame+0.., extras -> varargBase (above the
     nregs+4 register window; plan §3.2). Non-vararg protos drop extras,
     like stock luaD_precall. */
  np = nargs < m->numparams ? nargs : m->numparams;
  for (i = 0; i < np; i++)
    *(TValue *)(size_t)(frame + (rt_addr)sizeof(TValue) * i) = src[i];
  if (m->isvararg) {
    rt_addr vb = frame + (rt_addr)sizeof(TValue) * m->framecells;
    for (i = 0; i < nv; i++)
      *(TValue *)(size_t)(vb + (rt_addr)sizeof(TValue) * i) =
          src[m->numparams + i];
  }
  return frame;
}

rt_addr rt_wasm_push_frame(void *L, void *base, int mdidx, int nargs) {
  (void)L;
  return rt_push_frame_from(mdidx, nargs, s2v((StkId)base));
}

rt_addr rt_frame_cursor(void) {
  return rt_fr ? rt_fr->base + (rt_addr)rt_fr->used : 0;
}

void rt_frame_restore(rt_addr saved) {
  while (rt_fr != NULL && saved < rt_fr->base) {
    struct rt_frchunk *p = rt_fr->prev;
    free((void *)(size_t)rt_fr->base);
    free(rt_fr);
    rt_fr = p;
  }
  if (rt_fr != NULL)
    rt_fr->used = (uint32_t)(saved - rt_fr->base);
}

/* ---- the rt-owned open-upvalue registry (NOT L->openupval — see the
   file header; v3's discipline verbatim). UpVals are stock GC objects
   (luaC_newobj links them into allgc — GC-visible like any upvalue) but
   "open" ones point into wasm frame memory and are linked only here,
   address-ordered descending like luaF_findupval's list. */

struct rt_uvlink {
  struct rt_uvlink *next;
  rt_addr v;
  UpVal *uv;
};
static struct rt_uvlink *rt_openupval;

/* the wasm closure record: what the emitted code's `cl` addresses. Hung
   off the CClosure (upvalue 0, light userdata) so both sides find it;
   rt-owned malloc, safe under the stopped-GC v1 posture (v3 row). */
struct rt_wcl {
  int32_t mdidx;
  int32_t nupvalues;
  UpVal **upvals;
};

static UpVal *rt_newupval(void) {
  UpVal *uv = gco2upv(luaC_newobj(curL, LUA_VUPVAL, sizeof(UpVal)));
  setnilvalue(&uv->u.value); /* closed-nil until pointed/loaded */
  return uv;
}

static UpVal *rt_findupval(rt_addr addr) {
  struct rt_uvlink **pp = &rt_openupval, *n;
  while (*pp != NULL && (*pp)->v >= addr) {
    if ((*pp)->v == addr) return (*pp)->uv;
    pp = &(*pp)->next;
  }
  n = (struct rt_uvlink *)malloc(sizeof *n);
  if (n == NULL) return NULL;
  n->uv = rt_newupval();
  n->uv->v.p = (TValue *)(size_t)addr; /* open: points into the wasm frame */
  n->v = addr;
  n->next = *pp;
  *pp = n;
  return n->uv;
}

static void rt_closeuv(struct rt_uvlink *node) {
  setobj(curL, &node->uv->u.value, node->uv->v.p);
  node->uv->v.p = &node->uv->u.value; /* closed: the value lives here now */
  free(node);
}

void rt_close_upvals(rt_addr level) {
  while (rt_openupval != NULL && rt_openupval->v >= level) {
    struct rt_uvlink *n = rt_openupval->next;
    rt_closeuv(rt_openupval);
    rt_openupval = n;
  }
}

/* ---- the rt-owned to-be-closed registry (U4f) --------------------------
**
** Stock threads L->tbclist through the Lua stack slots (luaF_newtbcupval);
** compiled frames' registers live in the wasm frame region instead, so the
** rt keeps its own address-keyed list — same discipline as the upvalue
** registry above (never L->tbclist: stock's poptbclist/prepcallclosemth
** would dereference wasm cell addresses as StkIds). Innermost = highest
** cell = closed first, like both stock's list and the interp's LState
** tbclist.
*/

struct rt_tbclink {
  struct rt_tbclink *next;
  rt_addr v; /* the variable's cell */
};
static struct rt_tbclink *rt_tbclist;

/* the variable-name table the backend registers at init: the non-closable
   raise names the variable (the interp answers from the proto's debug
   locals at run time; the backend precomputed the same name and passes its
   table index — the rt side stays debug-info-free). The bytes live in the
   never-freed rt_frame_alloc string buffer, like the chunk name. */
struct rt_name {
  const char *ptr;
  int32_t len;
};
static struct rt_name *rt_names;
static int rt_names_n, rt_names_cap;

int32_t rt_name_put(rt_addr ptr, int32_t len) {
  if (rt_names_n == rt_names_cap) {
    int ncap = rt_names_cap ? rt_names_cap * 2 : 8;
    struct rt_name *nn =
        (struct rt_name *)realloc(rt_names, (size_t)ncap * sizeof *nn);
    if (nn == NULL) return -1;
    rt_names = nn;
    rt_names_cap = ncap;
  }
  rt_names[rt_names_n].ptr = (const char *)(size_t)ptr;
  rt_names[rt_names_n].len = len;
  return rt_names_n++;
}

static const char *rt_name_get(int32_t idx, int32_t *len) {
  if (idx < 0 || idx >= rt_names_n) {
    *len = 1;
    return "?";
  }
  *len = rt_names[idx].len;
  return rt_names[idx].ptr;
}

/* registration (OP_TBC; stock luaF_newtbcupval + checkclosemth): false/nil
   exempt; anything else needs a __close metamethod AT REGISTRATION TIME,
   else the variable-name error. luaG_runerror over the trampoline's C
   frame adds no position (isLua(ci) is false), so rt_run's line parameter
   builds the "chunk:line: " prefix — matching the interp's raise at the
   OP_TBC position (stock lvm.c luaG_runerror at the running frame). */
static rt_addr tbc_cell;
static int32_t tbc_nameidx;

static void tbc_body(void) {
  TValue *obj = (TValue *)(size_t)tbc_cell;
  const TValue *tm;
  struct rt_tbclink *e;
  if (l_isfalse(obj)) return;
  tm = luaT_gettmbyobj(curL, obj, TM_CLOSE);
  if (ttisnil(tm)) {
    int32_t nlen;
    const char *nm = rt_name_get(tbc_nameidx, &nlen);
    char buf[160];
    snprintf(buf, sizeof buf, "variable '%.*s' got a non-closable value",
             (int)nlen, nm);
    luaG_runerror(curL, "%s", buf);
    return; /* unreachable (noret) */
  }
  e = (struct rt_tbclink *)malloc(sizeof *e);
  if (e == NULL) {
    luaG_runerror(curL, "not enough memory");
    return;
  }
  e->v = tbc_cell;
  e->next = rt_tbclist;
  rt_tbclist = e;
}

int32_t rt_tbc(rt_addr celladdr, int32_t nameidx, int32_t line) {
  tbc_cell = celladdr;
  tbc_nameidx = nameidx;
  return rt_run(tbc_body, line, -1, -1, -1, NULL);
}

int32_t rt_tbc_count(void) {
  int32_t n = 0;
  struct rt_tbclink *e;
  for (e = rt_tbclist; e != NULL; e = e->next) n++;
  return n;
}

/* the __close call (stock callclosemethod): push tm, obj and, on error
   paths, the in-flight errobj; luaD_callnoyield with 0 results. The tm is
   looked up fresh (stock does the same — a metatable edit between
   registration and close means stock calls whatever it finds, nil
   included). Statics are (re)set immediately before each protected call,
   so a __close that re-enters compiled code through the trampoline sees
   its own values (the reentrancy law). */
static TValue cl_obj, cl_err;
static int cl_twoarg;

static void close_body(void) {
  const TValue *tm = luaT_gettmbyobj(curL, &cl_obj, TM_CLOSE);
  StkId func = curL->top.p;
  luaD_checkstack(curL, 3);
  setobj2s(curL, func, tm);
  setobj2s(curL, func + 1, &cl_obj);
  if (cl_twoarg) setobj2s(curL, func + 2, &cl_err);
  curL->top.p = func + 2 + cl_twoarg;
  luaD_callnoyield(curL, func, 0);
}

/* call __close under raw protection; returns the TStatus. LUA_ERRRUN
   failures leave the new error at top-1 (the raiser pushed it before
   luaD_throw — the staging discipline's invariant); ERRMEM/ERRERR leave
   nothing on the stack. */
static int rt_close_call(TValue *obj, TValue *err) {
  rt_sea_hint_set(-1, -1, -1, NULL); /* raw body: a non-closable __close raises
                                  luaG_callerror with no site hint */
  cl_obj = *obj;
  if (err != NULL) {
    cl_err = *err;
    cl_twoarg = 1;
  } else {
    cl_twoarg = 0;
  }
  return rt_run_raw(close_body);
}

/* rt_close is the OP_CLOSE/return-path close (stock luaF_close at status
   LUA_OK): upvalues first, then the tbc loop with the 1-arg __close(obj).
   A __close failure propagates immediately (stock luaD_callnoyield under
   OP_CLOSE): it is staged, RT_ERR returned, and the REMAINING entries stay
   linked — the unwinding boundary's rt_close_err finishes them with the
   error-path form, exactly as stock's failing luaF_close leaves the rest
   of L->tbclist for luaD_closeprotected at the pcall. */
int32_t rt_close(rt_addr level) {
  rt_close_upvals(level);
  while (rt_tbclist != NULL && rt_tbclist->v >= level) {
    struct rt_tbclink *e = rt_tbclist;
    TValue *obj;
    int st;
    rt_tbclist = e->next;
    obj = (TValue *)(size_t)e->v;
    free(e);
    if (l_isfalse(obj)) continue; /* defensive: false/nil never register */
    st = rt_close_call(obj, NULL);
    if (st != LUA_OK) {
      if (st == LUA_ERRMEM || st == LUA_ERRERR)
        stage_c_error(st); /* nothing on the stack: stage the literal */
      else
        stage_error();
      return RT_ERR;
    }
  }
  return RT_OK;
}

/* rt_close_err is the unwinding boundary's close (stock
   luaD_closeprotected over the tbc list; called from the trampoline's
   error paths only). The in-flight error is the staged err_value; every
   entry >= level closes with the 2-arg __close(obj, errobj) under
   per-entry raw protection, a failing close only REPLACES the in-flight
   object, and closing continues until the list is clean — the LAST raised
   error wins (stock's status replacement; there is no special "error in
   __close" text). The final object is re-staged for rtw_reraise.

   The staging is CLEARED while the closes dispatch: stock hands the
   in-flight object to __close as an ARGUMENT, never as ambient state, so
   the metamethod's body is an ordinary call — leaving err_pending set
   would make rt_run refuse its first protected op (concat, tostring, a
   nested call) and the close would abort silently. had remembers whether
   an error was in flight at entry: a nested (empty) rt_close_err must
   not resurrect a stale value, and the no-tbc path stays untouched. */
void rt_close_err(rt_addr level) {
  int had = err_pending;
  int save_line = err_line, save_line_set = err_line_set;
  TValue errobj = err_value; /* snapshot: reentrant dispatches re-stage */
  int failed = 0;
  err_pending = 0;
  while (rt_tbclist != NULL && rt_tbclist->v >= level) {
    struct rt_tbclink *e = rt_tbclist;
    TValue *obj;
    int st;
    rt_tbclist = e->next;
    obj = (TValue *)(size_t)e->v;
    free(e);
    if (l_isfalse(obj)) continue;
    st = rt_close_call(obj, &errobj);
    if (st != LUA_OK) {
      failed = 1;
      if (st == LUA_ERRMEM || st == LUA_ERRERR) {
        stage_c_error(st);
        errobj = err_value;
      } else {
        errobj = *s2v(curL->top.p - 1);
        curL->top.p--;
      }
    }
  }
  if (failed || had) {
    stage_tvalue(&errobj);
    err_pending = 1;
    if (had && !err_line_set) {
      err_line = save_line; /* the raise line belongs to the original error */
      err_line_set = save_line_set;
    }
  }
}

/* the globals-table TValue (registry[LUA_RIDX_GLOBALS]) — read through
   the public API under protection (allocation-free but runs no code) */
static TValue nc_gt, *nc_gtdst;

static void globals_table_body(void) {
  luaD_checkstack(curL, 1);
  lua_pushglobaltable(curL);
  *nc_gtdst = *s2v(curL->top.p - 1);
  curL->top.p--;
}

static const TValue *rt_globals_table(void) {
  nc_gtdst = &nc_gt;
  if (rt_run(globals_table_body, 0, -1, -1, -1, NULL) != RT_OK) return NULL;
  return &nc_gt;
}

/* rt_newclosure: build the CClosure + record. Protected (allocations and
   UpVal creation pass through the GC; no longjmp may cross into wasm). */
static rt_addr nc_dst, nc_parent, nc_frame;
static int32_t nc_protoidx;

static void newclosure_body(void) {
  struct rt_wasm_md *m = &rt_md[nc_protoidx];
  struct rt_wcl *parent = nc_parent ? (struct rt_wcl *)(size_t)nc_parent
                                    : NULL;
  struct rt_wcl *rec;
  CClosure *ccl;
  int i;

  rec = (struct rt_wcl *)calloc(1, sizeof *rec);
  if (rec == NULL) {
    luaG_runerror(curL, "not enough memory");
    return;
  }
  rec->mdidx = nc_protoidx;
  rec->nupvalues = m->nupvalues;
  rec->upvals = NULL;
  if (m->nupvalues > 0) {
    rec->upvals =
        (UpVal **)calloc((size_t)m->nupvalues, sizeof(UpVal *));
    if (rec->upvals == NULL) {
      free(rec);
      luaG_runerror(curL, "not enough memory");
      return;
    }
  }
  for (i = 0; i < m->nupvalues; i++) {
    if (m->uv[i].instack) {
      UpVal *uv = rt_findupval(
          nc_frame + (rt_addr)sizeof(TValue) * m->uv[i].idx);
      if (uv == NULL) {
        free(rec->upvals);
        free(rec);
        luaG_runerror(curL, "not enough memory");
        return;
      }
      rec->upvals[i] = uv;
    } else if (parent != NULL) {
      rec->upvals[i] = parent->upvals[m->uv[i].idx];
    } else if (m->uv[i].idx == 0) {
      /* U1/_ENV (ledger row 53): the main chunk runs proto 0 with no
         parent — its parentless captures all point at main's _ENV slot.
         Seed a CLOSED upvalue with the globals table: wasm chunks that
         merely read _ENV stay consistent; real environments land with
         the U2e/U6 rt work. */
      UpVal *uv = rt_newupval();
      const TValue *gt = rt_globals_table();
      if (gt == NULL) {
        free(rec->upvals);
        free(rec);
        return; /* error already staged by the nested rt_run */
      }
      setobj(curL, &uv->u.value, gt);
      uv->v.p = &uv->u.value; /* closed */
      rec->upvals[i] = uv;
    } else {
      free(rec->upvals);
      free(rec);
      luaG_runerror(curL,
                    "rt_newclosure: upvalue capture without a parent");
      return;
    }
  }

  /* the Lua-visible value: a CClosure over the dispatch trampoline,
     upvalue 0 = light userdata -> the record (the emitted code's `cl`) */
  ccl = luaF_newCclosure(curL, 1);
  ccl->f = rt_wasm_trampoline;
  setpvalue(&ccl->upvalue[0], rec);
  setclCvalue(curL, (TValue *)(size_t)nc_dst, ccl);
}

int32_t rt_newclosure(rt_addr dstcell, int32_t protoidx, rt_addr parentcl,
                      rt_addr frameaddr, int32_t line) {
  if (err_pending || protoidx < 0 || protoidx >= rt_md_n) return RT_ERR;
  nc_dst = dstcell;
  nc_protoidx = protoidx;
  nc_parent = parentcl;
  nc_frame = frameaddr;
  {
    int st = rt_run(newclosure_body, line, -1, -1, -1, NULL);
    if (st != RT_OK) return st;
    /* a nested failure (the _ENV seed's own rt_run) stages without
       raising — carry it out as this call's RT_ERR */
    return err_pending ? RT_ERR : RT_OK;
  }
}

void rt_getupval(rt_addr cl, int32_t idx, rt_addr cell) {
  UpVal *uv = ((struct rt_wcl *)(size_t)cl)->upvals[idx];
  *(TValue *)(size_t)cell = *uv->v.p;
}

void rt_setupval(rt_addr cl, int32_t idx, rt_addr cell) {
  UpVal *uv = ((struct rt_wcl *)(size_t)cl)->upvals[idx];
  *uv->v.p = *(TValue *)(size_t)cell; /* write-through: open -> the cell */
}

/* ---- U5e (row 85): the 5.5 vararg / global-decl entries ---- */

/* rt_varargtab mirrors stock createvarargtab (ltm.c) for the table
   model's OP_VARARGPREP: the table anchors at the C top while filled,
   'n' first as an INTEGER (math.type(t.n) == "integer"), then the
   extras raw, luaC_checkGC to close. src = the frame's vararg base
   (the extras' cells, above the register window); the built table
   lands in dst (register np). The reentrancy law is respected: the
   body copies the statics to C locals before anything that could
   dispatch. */
static TValue *vt_dst;
static TValue *vt_src;
static int vt_n;

static void varargtab_body(void) {
  lua_State *L = curL;
  TValue *dst = vt_dst;
  TValue *src = vt_src;
  int i, n = vt_n;
  TValue key, value;
  Table *t = luaH_new(L);
  sethvalue(L, s2v(L->top.p), t);
  L->top.p++;
  luaH_resize(L, t, cast_uint(n), 1);
  setsvalue(L, &key, luaS_new(L, "n")); /* key is "n" */
  setivalue(&value, n);                 /* value is n */
  luaH_set(L, t, &key, &value);         /* t.n = n */
  for (i = 0; i < n; i++)
    luaH_setint(L, t, i + 1, src + i);
  luaC_checkGC(L);
  *dst = *s2v(L->top.p - 1); /* the built table */
  L->top.p--;                /* pop the anchor */
}

int32_t rt_varargtab(rt_addr dst, rt_addr src, int32_t n, int32_t line) {
  vt_dst = (TValue *)(size_t)dst;
  vt_src = (TValue *)(size_t)src;
  vt_n = n;
  return rt_run(varargtab_body, line, -1, -1, -1, NULL);
}

/* rt_getvarg mirrors stock luaT_getvararg for OP_GETVARG (hidden
   model): an integral key 1..nextra (tointegerns — exact floats count)
   reads the hidden extra, the string "n" yields nextra as an INTEGER,
   anything else nil. Never raises: plain and unprotected like
   rt_getupval. vabase = the frame's vararg base; the extras live at
   vabase+16*(k-1) (the trampoline staged them there). */
void rt_getvarg(rt_addr dst, rt_addr key, rt_addr vabase, int32_t nextra) {
  TValue *rc = (TValue *)(size_t)key;
  TValue *ra = (TValue *)(size_t)dst;
  lua_Integer n;
  if (tointegerns(rc, &n)) { /* integral value? */
    if (l_castS2U(n) - 1 < cast_uint(nextra)) {
      TValue *slot = (TValue *)(size_t)(vabase +
                                        sizeof(TValue) * (cast_int(n) - 1));
      *ra = *slot;
      return;
    }
  } else if (ttisstring(rc)) { /* string value? */
    size_t len;
    const char *s = getlstr(tsvalue(rc), len);
    if (len == 1 && s[0] == 'n') { /* key is "n"? */
      setivalue(ra, nextra);
      return;
    }
  }
  setnilvalue(ra); /* else produce nil */
}

/* rt_errnnil mirrors stock luaG_errnnil's raise for OP_ERRNNIL: a
   global declaration whose register is already non-nil. Stock's own
   helper takes the LClosure (to read the name constant); the wasm side
   has no LClosure, so the name rides a cell — kcell(Bx-1) when the
   compiler recorded a string constant, else 0 ("?"). */
static TValue nn_v, nn_name;

static void errnnil_body(void) {
  if (!ttisnil(&nn_v)) {
    const char *name = "?";
    if (ttisstring(&nn_name))
      name = getstr(tsvalue(&nn_name));
    luaG_runerror(curL, "global '%s' already defined", name);
  }
}

int32_t rt_errnnil(rt_addr cell, rt_addr namecell, int32_t line) {
  nn_v = *(TValue *)(size_t)cell;
  if (namecell != 0)
    nn_name = *(TValue *)(size_t)namecell;
  else
    setnilvalue(&nn_name);
  return rt_run(errnnil_body, line, -1, -1, -1, NULL);
}

/* rt_vararg_read is OP_VARARG's k=1 arm (the table model — the interp
   spec, _vm.go): the count comes from the table's raw "n" (an exact
   integer 0..INT_MAX/2, stock getnumargs's unsigned test), the values
   from raw integer reads 1..n — no metamethod consult, user edits of
   t.n/t[i] honored, nil-padded to nwant. nwant < 0 means multret (C==0:
   all n, top moves by the returned count); dstcap is the frame's cell
   budget past dst — the register window is a fixed allocation, so an
   absurd t.n cannot write past it (the interp grows its register stack
   instead; unobservable in the corpus, defensive here). */
static TValue *vr_dst, vr_tab;
static int32_t vr_nwant, vr_dstcap, vr_ret;

static void vararg_read_body(void) {
  lua_State *L = curL;
  TValue *dst = vr_dst;
  int32_t nwant = vr_nwant;
  Table *t;
  TValue tvn;
  lua_Integer n;
  int32_t i, touse;
  if (!ttistable(&vr_tab))
    luaG_runerror(L, "vararg table has no proper 'n'");
  t = hvalue(&vr_tab);
  setnilvalue(&tvn);
  luaH_getstr(t, luaS_new(L, "n"), &tvn);
  if (!ttisinteger(&tvn) ||
      l_castS2U(ivalue(&tvn)) > (lua_Unsigned)(INT_MAX / 2))
    luaG_runerror(L, "vararg table has no proper 'n'");
  n = ivalue(&tvn);
  if (nwant < 0) nwant = (int32_t)n; /* multret: all available */
  if (nwant > vr_dstcap) /* the fixed frame's budget — see above */
    luaG_runerror(L, "stack overflow");
  touse = nwant < (int32_t)n ? nwant : (int32_t)n;
  for (i = 0; i < nwant; i++) {
    if (i < touse) {
      TValue res;
      setnilvalue(&res);
      luaH_getint(t, (lua_Integer)i + 1, &res);
      dst[i] = res;
    } else {
      setnilvalue(&dst[i]);
    }
  }
  vr_ret = nwant;
}

int32_t rt_vararg_read(rt_addr dst, rt_addr tabcell, int32_t nwant,
                       int32_t dstcap, int32_t line) {
  if (err_pending) return RT_ERR;
  vr_dst = (TValue *)(size_t)dst;
  vr_tab = *(TValue *)(size_t)tabcell;
  vr_nwant = nwant;
  vr_dstcap = dstcap;
  return rt_run(vararg_read_body, line, -1, -1, -1, NULL) != RT_OK
             ? RT_ERR
             : vr_ret;
}

int32_t rt_clidx(rt_addr funcell) {
  const TValue *f = (const TValue *)(size_t)funcell;
  if (ttisCclosure(f)) {
    CClosure *c = clCvalue(f);
    if (c->f == rt_wasm_trampoline)
      return ((struct rt_wcl *)pvalue(&c->upvalue[0]))->mdidx;
  }
  return -1;
}

/* ---- M5c: tailcall staging (v3 protocol; `cl` is the record) ----
**
** A wasm-closure tailcall stages its call descriptor here and returns
** the -2 sentinel; the trampoline's restage loop turns it into a fresh
** dispatch at the SAME adapter level — O(1) wasm stack, O(1) frames.
** Anything not a registered wasm closure declines (-1) and the emitted
** code falls back to rt_call. Reentrancy: stage->restage is a closed
** cycle inside one dispatch — the staged fields are consumed before any
** nested dispatch can run. */

static TValue *tail_args;
static int tail_nargs, tail_idx;
static rt_addr tail_cl;

int32_t rt_tail_stage(rt_addr funcell, rt_addr argcells, int32_t nargs,
                      rt_addr frame) {
  const TValue *f = (const TValue *)(size_t)funcell;
  TValue *na;
  int i;
  if (err_pending) return -1;
  if (!ttisCclosure(f)) return -1;
  {
    CClosure *c = clCvalue(f);
    struct rt_wcl *rec;
    if (c->f != rt_wasm_trampoline) return -1;
    rec = (struct rt_wcl *)pvalue(&c->upvalue[0]);
    if (rec->mdidx < 0 || rec->mdidx >= rt_md_n) return -1;
    tail_idx = rec->mdidx;
    tail_cl = (rt_addr)(size_t)rec;
  }
  na = (TValue *)realloc(tail_args,
                         (size_t)(nargs > 0 ? nargs : 1) * sizeof(TValue));
  if (na == NULL) return -1;
  tail_args = na;
  for (i = 0; i < nargs; i++)
    tail_args[i] = *(TValue *)(size_t)(argcells + (rt_addr)sizeof(TValue) * i);
  tail_nargs = (int)nargs;
  rt_frame_restore(frame); /* the tailcalling frame is dead */
  return tail_idx;
}

int32_t rt_tail_clidx(void) { return tail_idx; }
int32_t rt_tail_nargs(void) { return tail_nargs; }
rt_addr rt_tail_funcell(void) { return tail_cl; }

rt_addr rt_tail_restage(void) {
  return rt_push_frame_from(tail_idx, tail_nargs, tail_args);
}

/* ---- the precall seam: the dispatch trampoline (U2d) ------------------
**
** Everything C-initiated (pcall, sort comparators, gsub replacements,
** __index on wasm closures) funnels through stock luaD_precall's C path
** into rt_wasm_trampoline — the zero-patch replacement for v3's
** precall_wasm adapter (patched lua51/ldo.c). Error-safety law unchanged:
** a longjmp NEVER crosses the Go boundary; the dispatch runs under
** luaD_rawrunprotected, frame-cursor/depth are restored on every exit
** path, and the staged error is re-raised as a Lua error after moving
** the exact value onto the stack and CLEARING the staging (the v4 rule —
** this is what makes a C-level pcall catch work without v3's
** luaD_pcall patch).
*/

int32_t rt_wasm_enter(int32_t idx) {
  (void)idx;
  if (rt_wasm_depth >= RTW_MAX_DEPTH) {
    /* stock's own wording (lstate.c luaE_checkcstack); bare — the
       interpreter oracle raises "stack overflow" without a position */
    stage_str_error("stack overflow", 1);
    return 1;
  }
  rt_wasm_depth++;
  return 0;
}

void rt_wasm_leave(void) {
  if (rt_wasm_depth > 0) rt_wasm_depth--;
}

struct rtw_ctx {
  int32_t idx;
  rt_addr frame, cl;
  int32_t nargs, want, nret;
  rt_addr saved_cursor;
  rt_addr ci_funcbase; /* the args' stack slot (push_frame source) */
};

/* rtw_trampoline_arms is folded away (U5e): a second protect per
   dispatch pushed wasmtime's wasm-frame budget past the RTW_MAX_DEPTH
   guard — deep recursion trapped "call stack exhausted" before the
   clean "stack overflow" raise (so00/so01). The single-protect shape
   keeps the pre-seam per-level cost; the shadow entry is popped the
   moment the protect returns, BEFORE any arm below can raise, so no
   entry can outlive its CallInfo: every deeper longjmp folds into
   `status` at this protect, and the reraise paths that run after it
   (rtw_reraise's checkstack included) execute with the entry already
   gone. */
static int rt_wasm_trampoline(lua_State *L) {
  CallInfo *ci = L->ci;
  CClosure *ccl = clCvalue(s2v(ci->func.p));
  struct rt_wcl *rec = (struct rt_wcl *)pvalue(&ccl->upvalue[0]);
  struct rtw_ctx ctx;
  TStatus status;

  ctx.idx = rec->mdidx;
  ctx.cl = (rt_addr)(size_t)rec;
  ctx.nargs = cast_int(L->top.p - (ci->func.p + 1));
  ctx.want = get_nresults(ci->callstatus);
  ctx.nret = RTW_ERR;
  ctx.saved_cursor = rt_frame_cursor();
  ctx.ci_funcbase = (rt_addr)(size_t)(ci->func.p + 1);

  if (rt_wasm_enter(ctx.idx) != 0) { /* over RTW_MAX_DEPTH: staged */
    rt_frame_restore(ctx.saved_cursor);
    rtw_reraise(L);
  }
  ctx.frame = rt_wasm_push_frame(L, (void *)(size_t)ctx.ci_funcbase,
                                 ctx.idx, ctx.nargs);
  if (ctx.frame == 0) { /* frame region exhausted */
    rt_wasm_leave();
    stage_str_error("not enough memory", 1);
    rt_frame_restore(ctx.saved_cursor);
    rtw_reraise(L);
  }

  /* U5e (row 60): the shadow entry for this dispatch — visible to the
     seam's luaL_where from before the first wasm instruction (whose
     lowering stores the line into the bound cell) until the pop below.
     The M5c restage re-dispatches successor protos at this same entry;
     each successor prologue's rt_sea_setline rebinds its own cell. */
  rt_sea_push(ci);
  status = luaD_rawrunprotected(L, rtw_dispatch_body, &ctx);
  rt_sea_pop(); /* before any arm below can raise (comment above) */
  rt_wasm_leave();
  if (status != LUA_OK) {
    /* a deeper luaD_throw crossed the boundary (a C-side throw inside
       the dispatch — e.g. the result copy's checkstack): restore this
       level and re-raise. The error object stays wherever the thrower
       left it; luaD_seterrorobj reads it there (v3 posture).
       U4f: when this dispatch's dying frames hold to-be-closed variables,
       the boundary must run the protected closes FIRST (stock luaD_pcall:
       closeprotected, then seterrorobj) — the staged object folds into
       the 2-arg __close calls and a failing close replaces it (last
       error wins). Cells must stay allocated while __close runs (its
       dispatches push frames above the cursor), so the close precedes
       rt_frame_restore; guarded on pending entries so the no-tbc posture
       is byte-identical to before (an ERRMEM/ERRERR status re-raises as
       LUA_ERRRUN on this arm only). */
    if (rt_tbclist != NULL && rt_tbclist->v >= ctx.saved_cursor) {
      if (status == LUA_ERRMEM || status == LUA_ERRERR)
        stage_c_error(status); /* no stack value: stage the literal */
      else
        stage_error();
      rt_close_err(ctx.saved_cursor);
      rt_frame_restore(ctx.saved_cursor);
      rtw_reraise(L);
    }
    rt_frame_restore(ctx.saved_cursor);
    luaD_throw(L, status);
  }
  if (ctx.nret == RTW_ERR) {
    /* U4f: the dispatch staged the error — close this dispatch's dying
       frames with the error-path form BEFORE the region goes away (see
       the arm above), then re-raise the (possibly replaced) object */
    rt_close_err(ctx.saved_cursor);
    rt_frame_restore(ctx.saved_cursor);
    rtw_reraise(L);
  }
  if (ctx.nret == RTW_REFUSED) {
    /* host-contract violation (no script module): surface as an ordinary
       catchable error, never a trap */
    stage_str_error("wasm dispatch refused (no script module)", 1);
    rt_frame_restore(ctx.saved_cursor);
    rtw_reraise(L);
  }
  if (ctx.nret == RTW_STACKTRAP) {
    /* U5e (row 20 family): the engine's own wasm stack exhausted inside
       the nested dispatch — wasmtime/wazero trap "call stack exhausted",
       which the host maps to this sentinel. Chunk frame sizes vary, so
       this can fire BELOW RTW_MAX_DEPTH (the plain-CALL recursion shape
       traps ~2 levels early); stage the same bare "stack overflow" the
       depth guard stages and let the catching rt_run cascade add the
       position — byte-identical to the guard path (so00/so01). */
    stage_str_error("stack overflow", 1);
    rt_frame_restore(ctx.saved_cursor);
    rtw_reraise(L);
  }
  rt_frame_restore(ctx.saved_cursor);
  return (int)ctx.nret;
}

static void rtw_dispatch_body(lua_State *L, void *ud) {
  struct rtw_ctx *c = (struct rtw_ctx *)ud;
  (void)L;
  for (;;) {
    c->nret = wasm_dispatch_host(c->idx, c->frame, c->cl, c->nargs, c->want);
    if (c->nret != RTW_TAIL) break;
    /* M5c restage: a fresh frame push at the same adapter level */
    c->frame = rt_tail_restage();
    c->idx = rt_tail_clidx();
    c->nargs = rt_tail_nargs();
    c->cl = rt_tail_funcell();
    if (c->frame == 0) { /* frame region exhausted */
      stage_str_error("not enough memory", 1);
      c->nret = RTW_ERR;
      break;
    }
  }
  if (c->nret >= 0) {
    /* success: nret results staged at frame+0.. — copy to the stack top
       (a C function returns its results there; luaD_poscall moves them
       down and trims to nresults) */
    int i;
    luaD_checkstack(L, c->nret + 1);
    for (i = 0; i < c->nret; i++)
      setobj2s(L, L->top.p + i,
               (TValue *)(size_t)(c->frame + (rt_addr)sizeof(TValue) * i));
    L->top.p += c->nret;
  }
}

/* re-raise the staged value: push it, clear the staging, throw. The
   checkstack may itself throw — the cursor is already restored by then
   and the error unwinds to the caller's protect (v3 posture).
**
** U5e (row 85) — the message-handler arm: stock runs the handler at the
   RAISE (luaG_errormsg), which C-land raises still funnels through (the
   seam's errormsg replica). Errors that enter flight by DIRECT staging —
   the depth guard, the engine stack trap, rt_error — bypass it, so the
   handler runs HERE, at the crossing into C-land, exactly once:
**
**   - only when the throw will be caught by a STOCK protect (a
**     pcall/resume): L->errorJmp is none of ours (the rp shadow). When
**     one of ours catches instead, the error keeps cascading as staged
**     returns and a later reraise re-decides;
**   - only when no handler already ran for THIS in-flight error (the
       replica's msgh-ran flag — a C-land raise ran it at the raise;
**     consuming it here would be a double run);
**   - under our own protect (an rp-shadow entry), so a handler that
**     itself raises lands in the collapse below instead of looping —
**     its nested reraise sees the catcher as ours and plain-throws.
**
** The handler runs after the dying frames are restored (this arm sits
** past rt_wasm_leave/rt_frame_restore, and the cascade decremented the
** depth level by level), so it has full frame budget — the interp's
   row-78e reserve problem cannot arise. Stock runs it with the dying
   frames still on the stack; observable only to stack-inspecting
   handlers (row 85). A FAILING handler's object replaces the in-flight
   one (last-error-wins — rt_close_err's ruling; stock never defines
   this path, it recurses in errormsg until the C-stack guard fires).
*/
static TValue msgh_ev; /* the in-flight object, snapshotted past the arm */

static void msgh_body(lua_State *L, void *ud) {
  (void)ud;
  rt_rp_push(L); /* row 85: this protect is ours */
  luaD_callnoyield(L, L->top.p - 2, 1);
}

static void rtw_reraise(lua_State *L) {
  int ran = rt_sea_msgh_ran() != 0;
  msgh_ev = err_value; /* snapshot: the handler re-enters and re-stages */
  if (!ran && L->errfunc != 0 && !rt_rp_ours(L->errorJmp)) {
    TStatus st;
    rt_err_clear(); /* re-entrancy: the handler's rt entries must not
                       refuse on a pending staging */
    luaD_checkstack(L, 2);
    setobj2s(L, L->top.p, &msgh_ev); /* the error object */
    L->top.p++;
    setobjs2s(L, L->top.p, L->top.p - 1); /* move argument up (stock) */
    setobjs2s(L, L->top.p - 1, restorestack(L, L->errfunc)); /* push h */
    L->top.p++;
    st = luaD_rawrunprotected(L, msgh_body, NULL);
    if (rt_rp_n > 0) rt_rp_n--; /* pairs msgh_body's push on every exit */
    if (st == LUA_OK) {
      /* the handler's result sits at top-1 (callnoyield moved it onto
         the func slot); the nil-object fix is stock errormsg's tail */
      if (ttisnil(s2v(L->top.p - 1)))
        setsvalue2s(L, L->top.p - 1,
                    luaS_newliteral(L, "<no error object>"));
      luaD_throw(L, LUA_ERRRUN); /* the catcher reads top-1 */
    }
    if (st == LUA_ERRMEM || st == LUA_ERRERR)
      luaD_throw(L, st); /* no object on the stack: seterrorobj stages */
    /* the handler raised: its object is at top-1 (the raiser pushed it
       before its throw; the nil-fix ran in its own errormsg pass) */
    luaD_throw(L, LUA_ERRRUN);
  }
  luaD_checkstack(L, 1);
  setobj2s(L, L->top.p, &msgh_ev);
  L->top.p++;
  rt_err_clear(); /* the zero-patch rt_pcall_caught */
  luaD_throw(L, LUA_ERRRUN);
}

