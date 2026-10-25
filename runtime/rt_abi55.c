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
#include <limits.h>
#include <ctype.h>
#include <float.h>
#include <locale.h>
#include <math.h>

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
static int rt_oom_inflight; /* U6b: the memlimit block's one-time slack
                               flag — tentative here so the re-arms in
                               rt_err_clear/rt_set_state (above it in the
                               file) see it */
static int prot_n; /* U6c: the protected set's live count — same
                      tentativity (rt_set_state resets it above the
                      block that owns the rest of the set) */

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
       would suppress the position on the next one (U4f).
       U6b (row 37): the positionless literals — stock seterrorobj's
       ERRMEM/ERRERR texts and rt_deadline's cancellation text — carry
       their raisers' "no position" verdict IN THE CONTENT. v3 kept the
       flag sticky across the C-land crossing (its adapter never cleared
       the staging); v4 clears at the crossing (rtw_reraise — the
       zero-patch rt_pcall_caught), so a re-staged bare literal must be
       re-recognized here or the catching rt_run prefixes it (the
       tail-spin deadline fired under an OP_CALL site and gained
       "chunk:2: " — the m6d tailspin gate). A script's own
       error("not enough memory") is unaffected: error()'s level-1 raise
       prefixes the value BEFORE it ever stages, so the bare literal only
       occurs when the positionless machinery built it. */
    err_prefixed = 0;
    if (chunk_name_len > 0 && err_buf_len > chunk_name_len &&
        memcmp(err_buf, chunk_name, (size_t)chunk_name_len) == 0 &&
        err_buf[chunk_name_len] == ':')
      err_prefixed = 1;
    else if (strcmp(err_buf, "not enough memory") == 0 ||
             strcmp(err_buf, "error in error handling") == 0 ||
             strcmp(err_buf, "context deadline exceeded") == 0)
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
  rt_oom_inflight = 0; /* U6b: a consumed OOM re-arms the refusal */
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

static void sc_resolve(void); /* U11b: defined with the probe, below */

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
  /* U6b (row 93): also stop EMERGENCY collections. 5.5's luaM_realloc_
     retries a failed allocation after luaC_fullgc(L,1) (lmem.c tryagain
     — gated only by completestate+!gcstopem, NOT by gcstp), and fullinc
     runs unconditionally: under the freeze a full mark-and-sweep from
     the real roots would free every object held only from wasm cells
     (the pool/frames are not GC roots) the moment rt_alloc refuses —
     exactly the corruption the freeze exists to prevent, on the OOM
     path the cap makes routine. 5.1 (v3's tree) raised LUA_ERRMEM
     straight from the NULL return, which is why this hazard is new.
     gcstopem=1 turns 5.5's path into 5.1's: tryagain declines, the
     callers luaM_error, the refusal surfaces as the catchable
     "not enough memory". Nothing clears it: the collector never runs a
     cycle under GCSTPGC (lgc.c clears gcstopem only at cycle ends), and
     ldo.c's closeprotected save/restores OUR value. */
  G(curL)->gcstopem = 1;
  rt_oom_inflight = 0; /* U6b: fresh activation re-arms the cap refusal */
  prot_n = 0; /* U6c: the protected set does not survive a state reset
                 (fresh VMs only — the set's Table* belong to the dead
                 state; sandbox/protect re-run per activation) */
  rt_rp_n = 0; /* row 85: no protects of ours survive a state reset */
  sc_resolve(); /* U11b: pin the pure stdlib probe targets (once) */
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

/* ---- U10: unprotected table probes (int keys) ----
**
** The VM's own lvm.c fast paths (OP_GETTABLE/OP_SETTABLE's
** luaV_fastgeti/luaV_fastseti), lifted to probes so the compiled hit
** path skips the per-op try_table enter inside rt_run. Same macros,
** same conditions — semantics identical by construction. They cannot
** raise (no allocation, no luaD_* call: fval2arr/luaH_psetint are
** pure writes or code returns, and the barrier is pointer work under
** the frozen-GC posture the VM itself runs), so they carry no
** rt_run/setjmp; the caller falls back to the protected rt_gettable/
** rt_settable on a miss (non-table, non-integer key, hole,
** NEWINDEX metamethod, growth needed). */

int32_t rt_geti_probe(rt_addr tblcell, rt_addr keycell, rt_addr dstcell) {
  TValue *t = (TValue *)(size_t)tblcell;
  const TValue *k = (const TValue *)(size_t)keycell;
  TValue *dst = (TValue *)(size_t)dstcell;
  if (ttisinteger(k)) {
    lu_byte tag;
    luaV_fastgeti(t, ivalue(k), dst, tag);
    if (!tagisempty(tag))
      return 1; /* dst holds the value */
  }
  return 0; /* miss: the protected path observes metamethods etc. */
}

int32_t rt_seti_probe(rt_addr tblcell, rt_addr keycell, rt_addr valcell) {
  TValue *t = (TValue *)(size_t)tblcell;
  const TValue *k = (const TValue *)(size_t)keycell;
  TValue *v = (TValue *)(size_t)valcell;
  if (ttisinteger(k)) {
    int hres;
    /* the blob's pset family is the U6c-guarded seam (rt_seam55.c):
       a protected table RAISES from inside the writer, which an
       unprotected probe cannot let fly — decline, and the protected
       path raises cleanly under rt_run */
    if (ttistable(t) && rt_prot_isprotected(hvalue(t)))
      return 0;
    luaV_fastseti(t, ivalue(k), v, hres);
    if (hres == HOK) {
      luaV_finishfastset(curL, t, v); /* the write barrier */
      return 1;
    }
  }
  return 0;
}

/* ---- U11: short-string-key and length probes (U10 posture) ----
**
** Same construction as above: the VM's own fast arms lifted verbatim
** (OP_GETFIELD/SETFIELD via luaV_fastget/fastset over getshortstr/
** psetshortstr, OP_LEN via luaV_objlen's primitive arms). psetshortstr
** declines every raise-shaped case itself (no space for the key, the
** black/white barrier, any NEWINDEX metamethod) — rehash and barrier
** misses take the protected path. luaH_getn is search plus lenhint
** write; the __len and non-primitive arms miss. */

int32_t rt_gets_probe(rt_addr tblcell, rt_addr keycell, rt_addr dstcell) {
  TValue *t = (TValue *)(size_t)tblcell;
  const TValue *k = (const TValue *)(size_t)keycell;
  TValue *dst = (TValue *)(size_t)dstcell;
  if (ttisshrstring(k)) { /* long-string keys: the protected path */
    lu_byte tag;
    luaV_fastget(t, tsvalue(k), dst, luaH_getshortstr, tag);
    if (!tagisempty(tag))
      return 1; /* dst holds the value */
  }
  return 0;
}

int32_t rt_sets_probe(rt_addr tblcell, rt_addr keycell, rt_addr valcell) {
  TValue *t = (TValue *)(size_t)tblcell;
  const TValue *k = (const TValue *)(size_t)keycell;
  TValue *v = (TValue *)(size_t)valcell;
  if (ttisshrstring(k)) {
    int hres;
    /* same U6c-guard reason as rt_seti_probe: the guarded
       luaH_psetshortstr would raise out of this unprotected probe */
    if (ttistable(t) && rt_prot_isprotected(hvalue(t)))
      return 0;
    luaV_fastset(t, tsvalue(k), v, hres, luaH_psetshortstr);
    if (hres == HOK) {
      luaV_finishfastset(curL, t, v);
      return 1;
    }
  }
  return 0;
}

int32_t rt_len_probe(rt_addr srccell, rt_addr dstcell) {
  TValue *o = (TValue *)(size_t)srccell;
  TValue *dst = (TValue *)(size_t)dstcell;
  switch (ttypetag(o)) { /* luaV_objlen's primitive arms, verbatim */
    case LUA_VTABLE: {
      Table *h = hvalue(o);
      if (fasttm(curL, h->metatable, TM_LEN) == NULL) { /* no __len */
        setivalue(dst, l_castU2S(luaH_getn(curL, h)));
        return 1;
      }
      break;
    }
    case LUA_VSHRSTR:
      setivalue(dst, tsvalue(o)->shrlen);
      return 1;
    case LUA_VLNGSTR:
      setivalue(dst, cast_st2S(tsvalue(o)->u.lnglen));
      return 1;
    default:
      break; /* __len metamethod or typeerror: the protected path */
  }
  return 0;
}

/* ---- U11b: pure stdlib callee probe (string.byte / string.find) ----
**
** Both are pure: reads and integer staging only, no allocation, and
** they raise only on argument shapes the probe declines (non-string
** subjects, coercion-needed indices, the INT_MAX slice guard). The
** callee is compared against the stock functions resolved ONCE PER
** PROCESS, at the first rt_set_state (the globals are pristine then:
** the driver opens libraries before any script runs; light C
** functions carry their pointer inline, so the pinned values are
** state-independent). A pooled VM's later runs keep matching only the
** ORIGINAL functions: a script that swaps string.byte — any value,
** closure or C function — stops matching and takes the protected
** call; a saved original restored under the name matches because it
** IS the original.
** Results stage at the callee cell exactly as rt_call_addr's
** luaD_poscall would — truncated and nil-padded to want — and the
** return uses rt_call_addr's encoding. 1 = MISS (this probe never
** errors): the caller falls through to the protected call. */

static TValue sc_byte, sc_find, sc_format;
static TValue sc_min, sc_max, sc_concat; /* the U11e-c h_strlong residual */
static TString *sc_tostring_key; /* "__tostring": 5.5.1 has no TM slot
                                    (luaL_tolstring rawgets the real mt) */
static int sc_resolved = 0;

/* U11e-c: gmatch's iterator is a CClosure over lstrlib.c's STATIC
** gmatch_aux — not pinable by value like the light-C targets. It is
** pinned by CONSTRUCTION: the stock gmatch runs once over two empty
** strings (under protection, one enter per process at state init)
** and the resulting closure's C-function pointer is recorded; no
** Lua code can place gmatch_aux under a different f. The structs
** below are lstrlib.c's verbatim — the pin verifies them against a
** live object before arming (a vendored-layout drift disarms the
** probe; every call then declines). */
#define GMP_MAXCCALLS 200 /* lstrlib.c's MAXCCALLS default */
typedef struct gmp_MatchState {
  const char *src_init; /* init of source string */
  const char *src_end;  /* end ('\0') of source string */
  const char *p_end;    /* end ('\0') of pattern */
  lua_State *L;
  int matchdepth; /* control for recursive depth */
  int level;      /* total number of captures */
  struct {
    const char *init;
    ptrdiff_t len; /* length or special value (CAP_*) */
  } capture[32];   /* LUA_MAXCAPTURES */
} gmp_MatchState;
typedef struct gmp_GMatchState {
  const char *src;       /* current position */
  const char *p;         /* pattern */
  const char *lastmatch; /* end of last match */
  gmp_MatchState ms;     /* match state */
} gmp_GMatchState;

static lua_CFunction sc_gmatch_aux;

static void gmp_pin_body(void) {
  lua_State *L = curL;
  TValue st, key;
  Table *gt;
  CClosure *cl;
  if (!ttistable(&G(L)->l_registry))
    return;
  luaH_getint(hvalue(&G(L)->l_registry), LUA_RIDX_GLOBALS, &st);
  if (!ttistable(&st))
    return;
  gt = hvalue(&st);
  setsvalue(L, &key, luaS_new(L, "string"));
  luaH_getshortstr(gt, tsvalue(&key), &st);
  if (!ttistable(&st))
    return;
  setsvalue(L, &key, luaS_new(L, "gmatch"));
  luaH_getshortstr(hvalue(&st), tsvalue(&key), &st);
  if (!ttislcf(&st))
    return; /* swapped gmatch: no pin, the probes all decline */
  luaD_checkstack(L, 4);
  setobj2s(L, L->top.p, &st);
  L->top.p++;
  setsvalue(L, s2v(L->top.p), luaS_new(L, "")); /* the EMPTYSTRING singleton */
  L->top.p++;
  setsvalue(L, s2v(L->top.p), luaS_new(L, ""));
  L->top.p++;
  lua_call(L, 2, 1);
  cl = clCvalue(s2v(L->top.p - 1));
  if (cl->nupvalues == 3 && ttisstring(&cl->upvalue[0]) &&
      ttisstring(&cl->upvalue[1]) && ttisfulluserdata(&cl->upvalue[2])) {
    Udata *u = uvalue(&cl->upvalue[2]);
    if (u->nuvalue == 0 && u->len == sizeof(gmp_GMatchState)) {
      gmp_GMatchState *gm = (gmp_GMatchState *)getudatamem(u);
      const char *es = getstr(tsvalue(&cl->upvalue[0]));
      const char *ep = getstr(tsvalue(&cl->upvalue[1]));
      if (gm->src == es && gm->p == ep && gm->lastmatch == NULL &&
          gm->ms.src_init == es && gm->ms.src_end == es &&
          gm->ms.p_end == ep)
        sc_gmatch_aux = cl->f; /* layout verified against a live object */
    }
  }
  L->top.p--;
}

static void sc_resolve(void) {
  TValue st, key;
  Table *gt;
  if (sc_resolved)
    return;
  sc_resolved = 1;
  sc_tostring_key = luaS_new(curL, "__tostring");
  setnilvalue(&sc_byte);
  setnilvalue(&sc_find);
  setnilvalue(&sc_format);
  setnilvalue(&sc_min);
  setnilvalue(&sc_max);
  setnilvalue(&sc_concat);
  /* raw walk: registry[LUA_RIDX_GLOBALS] -> "string" -> "byte"/"find" —
     no metamethods, no C stack, no ordering hazard against globals
     protection. The short names are interned, so luaS_new finds them
     without allocating. */
  if (!ttistable(&G(curL)->l_registry))
    return;
  luaH_getint(hvalue(&G(curL)->l_registry), LUA_RIDX_GLOBALS, &st);
  if (!ttistable(&st))
    return;
  gt = hvalue(&st);
  setsvalue(curL, &key, luaS_new(curL, "string"));
  luaH_getshortstr(gt, tsvalue(&key), &st);
  if (!ttistable(&st))
    return; /* no string library: the probe simply never matches */
  gt = hvalue(&st);
  setsvalue(curL, &key, luaS_new(curL, "byte"));
  luaH_getshortstr(gt, tsvalue(&key), &sc_byte);
  if (!ttislcf(&sc_byte))
    setnilvalue(&sc_byte); /* only the stock light-C shape matches */
  setsvalue(curL, &key, luaS_new(curL, "find"));
  luaH_getshortstr(gt, tsvalue(&key), &sc_find);
  if (!ttislcf(&sc_find))
    setnilvalue(&sc_find);
  setsvalue(curL, &key, luaS_new(curL, "format"));
  luaH_getshortstr(gt, tsvalue(&key), &sc_format);
  if (!ttislcf(&sc_format))
    setnilvalue(&sc_format);
  /* the math.min/max and table.concat pins (the U11e-c h_strlong
     residual): two more library tables, same light-C shape. gt was
     reassigned to the string table above, so globals is re-fetched. */
  luaH_getint(hvalue(&G(curL)->l_registry), LUA_RIDX_GLOBALS, &st);
  if (ttistable(&st)) {
    setsvalue(curL, &key, luaS_new(curL, "math"));
    luaH_getshortstr(hvalue(&st), tsvalue(&key), &st);
    if (ttistable(&st)) {
      setsvalue(curL, &key, luaS_new(curL, "min"));
      luaH_getshortstr(hvalue(&st), tsvalue(&key), &sc_min);
      if (!ttislcf(&sc_min))
        setnilvalue(&sc_min);
      setsvalue(curL, &key, luaS_new(curL, "max"));
      luaH_getshortstr(hvalue(&st), tsvalue(&key), &sc_max);
      if (!ttislcf(&sc_max))
        setnilvalue(&sc_max);
    }
  }
  luaH_getint(hvalue(&G(curL)->l_registry), LUA_RIDX_GLOBALS, &st);
  if (ttistable(&st)) {
    setsvalue(curL, &key, luaS_new(curL, "table"));
    luaH_getshortstr(hvalue(&st), tsvalue(&key), &st);
    if (ttistable(&st)) {
      setsvalue(curL, &key, luaS_new(curL, "concat"));
      luaH_getshortstr(hvalue(&st), tsvalue(&key), &sc_concat);
      if (!ttislcf(&sc_concat))
        setnilvalue(&sc_concat);
    }
  }
  /* U11e-c: the gmatch_aux pin (by construction — see gmp_pin_body) */
  if (rt_run(gmp_pin_body, 0, -1, -1, -1, NULL) != RT_OK)
    sc_gmatch_aux = NULL; /* OOM-only; the staged raise is the truth */
}

/* lstrlib.c's index helpers, verbatim (arg coercion removed: the
   probe declines non-integer indices before calling these) */
static size_t sc_posrelatI(lua_Integer pos, size_t len) {
  if (pos > 0)
    return (size_t)pos;
  else if (pos == 0)
    return 1;
  else if (pos < -(lua_Integer)len) /* inverted comparison */
    return 1; /* clip to 1 */
  else
    return len + (size_t)pos + 1;
}

static size_t sc_getendpos(lua_Integer pos, size_t len) {
  /* stock's getendpos, verbatim, minus luaL_optinteger (the caller
     resolved the default; stock: clip result to [0, len]) */
  if (pos > (lua_Integer)len)
    return len;
  else if (pos >= 0)
    return (size_t)pos;
  else if (pos < -(lua_Integer)len)
    return 0;
  else
    return len + (size_t)pos + 1;
}

static const char *sc_lmemfind(const char *s1, size_t l1, const char *s2,
                               size_t l2) { /* lstrlib.c, verbatim */
  if (l2 == 0)
    return s1;
  else if (l2 > l1)
    return NULL;
  else {
    const char *init;
    l2--;
    l1 = l1 - l2;
    while (l1 > 0 && (init = (const char *)memchr(s1, *s2, l1)) != NULL) {
      init++;
      if (memcmp(init, s2 + 1, l2) == 0)
        return init - 1;
      else {
        l1 -= ct_diff2sz(init - s1);
        s1 = init;
      }
    }
    return NULL;
  }
}

static const char sc_specials[] = "^$*+?.([%-";

static int sc_nospecials(const char *p, size_t l) { /* verbatim */
  size_t upto = 0;
  do {
    if (strpbrk(p + upto, sc_specials))
      return 0;
    upto += strlen(p + upto) + 1;
  } while (upto <= l);
  return 1;
}

/* stage nres results (nil-padded/truncated to the caller's want) with
   rt_call_addr's success encoding */
static int32_t sc_stage(TValue *dst, int w, const TValue *res, int nres) {
  int i, lim = (w < 0) ? nres : w;
  for (i = 0; i < lim; i++) {
    if (i < nres)
      dst[i] = res[i];
    else
      setnilvalue(&dst[i]);
  }
  return w < 0 ? -(nres + 1) : RT_OK;
}

static int sc_format_arm(TValue *args, int n, TValue *dst, int w);
static int sc_minmax_arm(TValue *args, int n, TValue *dst, int w, int ismax);
static int sc_tconcat_arm(TValue *args, int n, TValue *dst, int w);

int32_t rt_stdcall_probe(rt_addr funcaddr, rt_addr argcells, int32_t nargs,
                         int32_t want) {
  const TValue *fn = (const TValue *)(size_t)funcaddr;
  TValue *args = (TValue *)(size_t)argcells;
  TValue *dst = (TValue *)(size_t)(argcells - (rt_addr)sizeof(TValue));
  int w = (int)want, n = (int)nargs;
  TValue res[2];
  if (luaV_rawequalobj(fn, &sc_byte)) {
    /* str_byte's arithmetic, verbatim; only the raise shapes differ */
    size_t l;
    const char *sp;
    lua_Integer pi = 1, pj;
    size_t posi, pose;
    int i, nres, lim;
    if (n < 1 || !ttisstring(&args[0]))
      return 1; /* coercion/metamethod/error shapes: protected path */
    sp = getstr(tsvalue(&args[0]));
    l = tsslen(tsvalue(&args[0]));
    if (n >= 2) {
      if (!ttisinteger(&args[1]))
        return 1;
      pi = ivalue(&args[1]);
    }
    pj = pi; /* luaL_optinteger(L, 3, pi): absent j defaults to i */
    if (n >= 3) {
      if (!ttisinteger(&args[2]))
        return 1;
      pj = ivalue(&args[2]);
    }
    posi = sc_posrelatI(pi, l);
    pose = sc_getendpos(pj, l);
    if (posi > pose)
      return sc_stage(dst, w, res, 0); /* empty interval, no values */
    if (pose - posi >= (size_t)INT_MAX)
      return 1; /* "string slice too long": the protected path raises */
    nres = (int)(pose - posi) + 1;
    if (w < 0 && nres > 8)
      return 1; /* big multret slices: the fallback stages them */
    lim = (w < 0) ? nres : w;
    for (i = 0; i < lim; i++) {
      if (i < nres) {
        setivalue(&dst[i], cast_uchar(sp[posi + (size_t)i - 1]));
      } else { /* braces: setivalue expands to a bare block */
        setnilvalue(&dst[i]);
      }
    }
    return w < 0 ? -(nres + 1) : RT_OK;
  }
  if (luaV_rawequalobj(fn, &sc_find)) {
    /* str_find_aux's plain-search branch (explicit plain flag or a
       pattern with no special characters), verbatim */
    size_t ls, lp, init;
    const char *sp, *pp, *s2;
    lua_Integer ii = 1;
    int plain;
    if (n < 2 || !ttisstring(&args[0]) || !ttisstring(&args[1]))
      return 1;
    sp = getstr(tsvalue(&args[0]));
    ls = tsslen(tsvalue(&args[0]));
    pp = getstr(tsvalue(&args[1]));
    lp = tsslen(tsvalue(&args[1]));
    if (n >= 3) {
      if (!ttisinteger(&args[2]))
        return 1;
      ii = ivalue(&args[2]);
    }
    init = sc_posrelatI(ii, ls) - 1;
    plain = (n >= 4 && !l_isfalse(&args[3])) || sc_nospecials(pp, lp);
    if (!plain)
      return 1; /* the pattern engine: protected path */
    if (init > ls) { /* start after string's end */
      setnilvalue(&res[0]); /* luaL_pushfail = nil in this build
                               (LUA_FAILISFALSE unset) */
      return sc_stage(dst, w, res, 1);
    }
    s2 = sc_lmemfind(sp + init, ls - init, pp, lp);
    if (s2 != NULL) {
      setivalue(&res[0], ct_diff2S(s2 - sp) + 1);
      setivalue(&res[1], cast_st2S(ct_diff2sz(s2 - sp) + lp));
      return sc_stage(dst, w, res, 2);
    }
    setnilvalue(&res[0]);
    return sc_stage(dst, w, res, 1);
  }
  if (luaV_rawequalobj(fn, &sc_format)) {
    /* str_format's fold (U11d): stages its single result via sc_stage,
       or returns 1 = MISS for every raising/exotic shape */
    return sc_format_arm(args, n, dst, w);
  }
  if (luaV_rawequalobj(fn, &sc_min)) {
    /* math_min's fold (U11e-c residual): the winner is an arg cell */
    return sc_minmax_arm(args, n, dst, w, 0);
  }
  if (luaV_rawequalobj(fn, &sc_max)) {
    /* math_max's fold (U11e-c residual) */
    return sc_minmax_arm(args, n, dst, w, 1);
  }
  if (luaV_rawequalobj(fn, &sc_concat)) {
    /* tconcat's fold (U11e-c residual): concat-probe shaped (U11c) */
    return sc_tconcat_arm(args, n, dst, w);
  }
  return 1; /* not a probe-known function */
}

/* rt_fmod / rt_pow: luai_nummod and luai_numpow (llimits.h) verbatim,
   as plain calls. Neither can raise — NaN/inf are the stock results —
   so the U11e in-wasm float MOD/POW arms pay no try_table enter for
   them (same posture as the probes; byte-exact with the protected
   path by construction: same macros, same libc). */
double rt_fmod(lua_Number a, lua_Number b) {
  lua_Number m = fmod(a, b);
  if (((m) > 0) ? (b) < 0 : ((m) < 0 && (b) > 0))
    m += (b);
  return m;
}

double rt_pow(lua_Number a, lua_Number b) {
  return ((b == 2) ? (a) * (a) : pow(a, b));
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
  ptrdiff_t resr; /* row 31: a metamethod (or string coercion) below may
                     grow — and move — the Lua stack; a raw StkId dangles
                     across that (the U7d soak's c0027655/c0076725/
                     c0080533 family, row 102) */
  StkId res = curL->top.p; /* scratch result slot (below the TM window) */
  luaD_checkstack(curL, 1);
  curL->top.p++;
  resr = savestack(curL, res);
  luaO_arith(curL, ar_op, &ar_l, &ar_r, res);
  res = restorestack(curL, resr);
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
  ptrdiff_t resr; /* row 31/102: a __len metamethod may grow — and move —
                     the stack; rebase the scratch slot like arith_body */
  StkId res = curL->top.p;
  luaD_checkstack(curL, 1);
  curL->top.p++;
  resr = savestack(curL, res);
  luaV_objlen(curL, res, &len_v);
  res = restorestack(curL, resr);
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
static TValue *ca_args, *ca_dst;
static int ca_n, ca_w, ca_nres;

/* Reentrancy law: the callee may be a wasm closure re-entering the ABI
   (including another rt_call) before this body resumes — no static may
   be read after luaD_callnoyield; everything lives on the C stack and
   only ca_nres is written back at the very end. */
static void call_body(void) {
  int i, n = ca_n, w = ca_w, nres;
  TValue *args = ca_args, *dst = ca_dst;
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
  for (i = 0; i < nres; i++) dst[i] = *s2v(func + i);
  curL->top.p = func;
  ca_nres = nres;
}

int32_t rt_call(rt_addr funcell, rt_addr argcells, int32_t nargs, int32_t want,
                int32_t line, int32_t hcallee) {
  int w = (int)want, nres;
  ca_f = *(TValue *)(size_t)funcell;
  ca_args = (TValue *)(size_t)argcells;
  ca_dst = (TValue *)(size_t)argcells; /* results overwrite the args */
  ca_n = (int)nargs;
  ca_w = w;
  ca_nres = 0;
  int st = rt_run(call_body, line, -1, -1, hcallee, NULL);
  if (st != RT_OK) return RT_ERR;
  nres = ca_nres; /* read before encoding — a nested call may follow */
  return w < 0 ? -(nres + 1) : RT_OK; /* encode count; -1 means zero */
}

/* U6a (row 86(A)): the callee-address form. funcaddr reads the callee
   in place and argcells stay at their natural R(A+1).. position — the
   emitted code saves and shifts nothing; results are moved down to
   argcells-16 (R(A), where OP_CALL/TFORCALL expect them) by the body
   itself, multret top bookkeeping included. Same body, same reentrancy
   law: the callee TValue is copied into ca_f (a C static) before any
   re-entry can overwrite the cells. The ABI stays v4 — additive entry
   (D9); rt_call remains exported for the native tests. */
int32_t rt_call_addr(rt_addr funcaddr, rt_addr argcells, int32_t nargs,
                     int32_t want, int32_t line, int32_t hcallee) {
  int w = (int)want, nres;
  ca_f = *(TValue *)(size_t)funcaddr;
  ca_args = (TValue *)(size_t)argcells;
  ca_dst = (TValue *)(size_t)(argcells - (rt_addr)sizeof(TValue));
  ca_n = (int)nargs;
  ca_w = w;
  ca_nres = 0;
  int st = rt_run(call_body, line, -1, -1, hcallee, NULL);
  if (st != RT_OK) return RT_ERR;
  nres = ca_nres;
  return w < 0 ? -(nres + 1) : RT_OK;
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

/* ---- U6b (rows 36/37, ledger row 93): the per-VM allocation cap ----
**
** rt_alloc replaces the driver's plain realloc shim (the RT flavor's
** lnewstate builds the state with lua_newstate(rt_alloc, NULL)): every
** Lua-object byte passes through here, so a budget check at the
** chokepoint converts runaway allocation into a clean LUA_ERRMEM ("not
** enough memory") — an ordinary pcall-catchable Lua error, never a trap
** (design §6.4/§8.6, m6 plan D3; the v3 port verbatim-in-spirit).
**
** rt_set_memlimit is an init-time hook (per-VM lifetime, not per-run):
** the host calls it BEFORE lnewstate, so the whole VM — stdlib open,
** shims, constant pool, script — counts against the budget. It zeroes
** the used counter (calling it mid-lifetime undercounts live blocks;
** frees clamp at 0, so accounting stays monotone-safe either way).
**
** Emergency slack: a refusal sets rt_oom_inflight, and while it is set,
** allocations up to limit+RT_MEM_EMERGENCY still pass. Without it the
** OOM raise would dead-end — the ERRMEM recovery interns the message
** string through the same allocator ("not enough memory"; our
** stage_c_error does too); refusing THAT would recurse the error
** machinery into error handling. The slack is one-time: the used
** counter keeps counting, so a catch-and-retry loop gains at most
** RT_MEM_EMERGENCY bytes total over the limit. rt_err_clear and
** rt_set_state clear the flag so the next over-limit allocation
** re-refuses (v4 has no rt_pcall_caught — rtw_reraise's rt_err_clear at
** the C-land crossing IS the catch hook).
**
** The interp oracle has NO counterpart surface: Go's allocator cannot
** refuse (SetMx watches runtime stats and os.Exit(3)s — not catchable),
** so the OOM text is pinned absolutely, not diffed (ledger row 36).
** Non-Lua allocations (rt_frame_alloc's malloc, frame chunks) bypass the
** cap by design — bounded by the depth guard and restored per frame; the
** linear-memory max (build55.sh --max-memory) is the second, coarser
** backstop: memory.grow failure makes malloc fail → the same clean path.
*/
static int64_t rt_mem_limit, rt_mem_used;
static int rt_oom_inflight;
#define RT_MEM_EMERGENCY (64 * 1024)

void rt_set_memlimit(int32_t bytes) {
  rt_mem_limit = bytes > 0 ? (int64_t)bytes : 0;
  rt_mem_used = 0;
  rt_oom_inflight = 0;
}

int32_t rt_mem_used_bytes(void) {
  return rt_mem_used > 0x7fffffffLL ? 0x7fffffff : (int32_t)rt_mem_used;
}

void *rt_alloc(void *ud, void *ptr, size_t osize, size_t nsize) {
  void *np;
  (void)ud;
  if (nsize == 0) {
    free(ptr);
    rt_mem_used -= (int64_t)osize;
    if (rt_mem_used < 0) rt_mem_used = 0; /* frees of pre-limit blocks */
    return NULL;
  }
  if (rt_mem_limit > 0 && nsize > osize) {
    int64_t cap =
        rt_oom_inflight ? rt_mem_limit + RT_MEM_EMERGENCY : rt_mem_limit;
    if (rt_mem_used + (int64_t)(nsize - osize) > cap) {
      rt_oom_inflight = 1; /* → luaM_realloc_ raises LUA_ERRMEM */
      return NULL;
    }
  }
  np = realloc(ptr, nsize);
  if (np == NULL) {
    if (rt_mem_limit > 0) rt_oom_inflight = 1; /* real exhaustion: same path */
    return NULL;
  }
  rt_mem_used += (int64_t)nsize - (int64_t)osize; /* signed: shrinks subtract */
  return np;
}

/* The two-sided probe pre-flight (U11c), shared by the concat fold and
** the string.format fold. 'resultlen' is the result string's byte
** length; 'mayintern' adds the exact growstrtab peak when a short
** interning is due. Side 1: a malloc(need) round trip against the real
** allocator — a following request of at most 'need' bytes cannot fail,
** so a NULL is exactly the real wall (linear-memory max / dlmalloc
** fragmentation), which no accounting predicts and the only check
** under limit==0. Side 2: rt_alloc's own cap arithmetic when budgeted
** (the emergency cap only adds headroom, so the plain limit is the
** conservative side). Returns 1 = safe, 0 = decline (never attempt). */
static int rt_probe_mem_ok(size_t resultlen, int mayintern) {
  size_t need = sizeof(TString) + resultlen + 1;
  if (mayintern && resultlen <= LUAI_MAXSHORTLEN) {
    stringtable *tb = &G(curL)->strt;
    if (tb->nuse >= tb->size)
      need += (size_t)tb->size * 2 * sizeof(TString *);
  }
  {
    void *t = malloc(need + 64);
    if (t == NULL) return 0; /* the real wall: miss, never attempt */
    free(t);
  }
  if (rt_mem_limit > 0 && rt_mem_used + (int64_t)need > rt_mem_limit)
    return 0; /* the cap refusal: clean catchable raise on the other path */
  return 1;
}

/* ---- U11c: concat probe ----
**
** OP_CONCAT's fold as an unprotected probe with an exact memory
** pre-flight against the machinery above. Stock luaV_concat converts
** number operands in place and, for each pass, may allocate interned
** temps; this seam never did that to the CELLS anyway (concat_body
** copies them onto the stack and converts the stack copies), so the
** probe formats numbers into a C stack buffer via stock's own
** luaO_tostringbuff — strictly fewer allocations, same bytes (the
** frozen-GC posture makes the savings invisible). The result is one
** allocation exactly like stock's last pass: luaS_newlstr (<=40,
** interned — a repeat returns the existing object, no alloc) or
** luaS_createlngstrobj (>40, never interned). Everything that can
** raise — metamethod operands, "string length overflow", the OOM
** refusal — declines to the protected rt_concat. The pre-flight is
** two-sided: a malloc(need) round trip against the real allocator
** (the only wall under limit==0, and unpredictable fragmentation under
** a limit), plus rt_alloc's own cap arithmetic when budgeted — which
** makes every refusal unreachable here by construction.
*/
int32_t rt_concat_probe(rt_addr cellsaddr, int32_t count, rt_addr dstaddr) {
  char nbuf[LUA_N2SBUFFSZ];
  TValue *cells = (TValue *)(size_t)cellsaddr;
  TValue *dst = (TValue *)(size_t)dstaddr;
  int i;
  size_t total = 0;
  if (count < 2) return 0; /* the n==1 move stays on the protected path */
  for (i = 0; i < count; i++) {
    const TValue *o = &cells[i];
    if (ttisstring(o))
      total += tsslen(tsvalue(o));
    else if (ttisnumber(o))
      total += luaO_tostringbuff(o, nbuf); /* no alloc, cells stay as-is */
    else
      return 0; /* metamethod / concat-error shape: the protected path */
  }
  if (total >= MAX_SIZE - sizeof(TString))
    return 0; /* would raise "string length overflow" */
  if (!rt_probe_mem_ok(total, 1))
    return 0; /* the shared two-sided pre-flight (see it for the law) */
  {
    char sbuff[LUAI_MAXSHORTLEN];
    TString *ts = NULL; /* NULL = building into sbuff (short result) */
    char *p;
    if (total > LUAI_MAXSHORTLEN) {
      ts = luaS_createlngstrobj(curL, total);
      p = getlngstr(ts);
    } else
      p = sbuff;
    for (i = 0; i < count; i++) {
      const TValue *o = &cells[i];
      const char *src;
      size_t l;
      if (ttisstring(o)) {
        src = getstr(tsvalue(o));
        l = tsslen(tsvalue(o));
      } else {
        l = luaO_tostringbuff(o, nbuf);
        src = nbuf;
      }
      memcpy(p, src, l);
      p += l;
    }
    if (ts == NULL) /* intern the short result (a hit: no alloc at all) */
      ts = luaS_newlstr(curL, sbuff, total);
    /* dst aliases cells[0] at every OP_CONCAT site — written last, by
       the protected path's own contract */
    setsvalue(curL, dst, ts);
  }
  return 1; /* hit: like the U10/U11a table probes (1 = handled) */
}

/* ---- U11d: string.format probe ----
**
** str_format's fold over cells, same posture as byte/find/concat: the
** stock success arms verbatim, every raising or exotic shape declines
** to the protected call (which raises the stock texts), and the one
** allocation — the result string — goes through the shared two-sided
** pre-flight. Pass 1 walks the format with stock's own parser
** (getformat/checkformat/get2digits/addlenmod, verbatim), screening
** each arg cell (numbers via luaO_tostringbuff, integer-exact floats,
** raw strings) and formatting numeric pieces into per-piece C buffers,
** so 'total' is exact; pass 2 emits. The hex float formatter is the
** tree's own fallback (num2straux — this build has no system %a),
** ported verbatim for %a/%A and %q-float, like quotefloat and
** addquoted's count/emit twins. luaL_tolstring's __tostring callmeta
** arm can re-enter: string/number/boolean metatables with __tostring
** decline. %p (pointers are engine-specific) and numeral-string
** coercions (stock coerces "42" in %d; the probe declines string cells
** for numeric conversions) also decline. Everything declined is
** byte-identical on the protected path — no ledger row.
*/

/* lstrlib.c's format constants and flag sets (they are #define'd there,
   out of this file's reach — pinned here, shapes verbatim) */
#define SC_FMT_MAX_FORMAT 32 /* MAX_FORMAT */
#define SC_FMT_ITEM 120      /* MAX_ITEM */
#define SC_FMT_ITEMF (110 + DBL_MAX_10_EXP) /* MAX_ITEMF; lua_Number = double */
_Static_assert(sizeof(lua_Number) == sizeof(double),
               "SC_FMT_ITEMF is derived from DBL_MAX_10_EXP");
#define SC_FMT_PIECES 24 /* directives + literal runs; beyond = miss */
#define SC_FMTFLAGSF "-+#0 " /* valid flags overall (getformat's span) */
#define SC_FMTFLAGSC "-"     /* c, p, s */
#define SC_FMTFLAGSI "-+0 "  /* d, i */
#define SC_FMTFLAGSU "-0"    /* u */
#define SC_FMTFLAGSX "-#0"   /* o, x, X */

typedef struct {
  int qstr;  /* 1 = p/rl is raw string, emit quoted */
  int inbuf; /* 1 = bytes in buf */
  const char *p;
  size_t rl; /* raw length (qstr) */
  size_t l;  /* emitted length */
  char buf[SC_FMT_ITEMF + 1];
} ScPiece;

/* lstrlib.c helpers, verbatim (errors become declines) */
static const char *sc_get2digits(const char *s) {
  if (isdigit(cast_uchar(*s))) {
    s++;
    if (isdigit(cast_uchar(*s)))
      s++; /* (2 digits at most) */
  }
  return s;
}

static int sc_checkformat(const char *form, const char *flags,
                          int precision) {
  const char *spec = form + 1; /* skip '%' */
  spec += strspn(spec, flags); /* skip flags */
  if (*spec != '0') {          /* a width cannot start with '0' */
    spec = sc_get2digits(spec); /* skip width */
    if (*spec == '.' && precision) {
      spec++;
      spec = sc_get2digits(spec); /* skip precision */
    }
  }
  return isalpha(cast_uchar(*spec)) != 0; /* reached the end = valid */
}

static const char *sc_getformat(const char *strfrmt, char *form) {
  size_t len = strspn(strfrmt, SC_FMTFLAGSF "123456789.");
  len++; /* adds following character (should be the specifier) */
  if (len >= SC_FMT_MAX_FORMAT - 10)
    return NULL; /* "invalid format (too long)": protected path */
  *(form++) = '%';
  memcpy(form, strfrmt, len);
  *(form + len) = '\0';
  return strfrmt + len - 1;
}

static void sc_addlenmod(char *form, const char *lenmod) {
  size_t l = strlen(form);
  size_t lm = strlen(lenmod);
  char spec = form[l - 1];
  strcpy(form + l - 1, lenmod);
  form[l + lm - 1] = spec;
  form[l + lm] = '\0';
}

/* the tree's hexadecimal float formatter (lstrlib.c compiles the
   fallback — no system %a), verbatim */
#define SC_NBFD ((DBL_MANT_DIG - 1) % 4 + 1) /* L_NBFD for double */

static lua_Number sc_adddigit(char *buff, unsigned n, lua_Number x) {
  lua_Number dd = floor(x); /* get integer part from 'x' */
  int d = (int)dd;
  buff[n] = cast_char(d < 10 ? d + '0' : d - 10 + 'a');
  return x - dd; /* return what is left */
}

static int sc_num2straux(char *buff, unsigned sz, lua_Number x) {
  if (x != x || x == (lua_Number)HUGE_VAL || x == -(lua_Number)HUGE_VAL)
    return snprintf(buff, sz, LUA_NUMBER_FMT, (double)x);
  else if (x == 0) /* can be -0... */
    return snprintf(buff, sz, LUA_NUMBER_FMT "x0p+0", (double)x);
  else {
    int e;
    lua_Number m = frexp(x, &e);
    unsigned n = 0;
    if (m < 0) {
      buff[n++] = '-';
      m = -m;
    }
    buff[n++] = '0';
    buff[n++] = 'x';
    m = sc_adddigit(buff, n++, m * (1 << SC_NBFD));
    e -= SC_NBFD;
    if (m > 0) {
      buff[n++] = lua_getlocaledecpoint(); /* add radix point ("." here) */
      do {
        m = sc_adddigit(buff, n++, m * 16);
      } while (m > 0);
    }
    n += (unsigned)snprintf(buff + n, sz - n, "p%+d", e);
    return (int)n;
  }
}

static int sc_number2strx(char *buff, unsigned sz, const char *fmt,
                          lua_Number x) {
  int n = sc_num2straux(buff, sz, x);
  if (fmt[sizeof(LUA_NUMBER_FRMLEN)] == 'A') {
    int i;
    for (i = 0; i < n; i++)
      buff[i] = toupper(cast_uchar(buff[i]));
  } else if (fmt[sizeof(LUA_NUMBER_FRMLEN)] != 'a')
    return -1; /* "modifiers for format '%a'/'%A' not implemented" */
  return n;
}

/* quotefloat, verbatim (%q on floats) */
static int sc_quotefloat(char *buff, lua_Number n) {
  const char *s;
  if (n == (lua_Number)HUGE_VAL)
    s = "1e9999";
  else if (n == -(lua_Number)HUGE_VAL)
    s = "-1e9999";
  else if (n != n)
    s = "(0/0)";
  else {
    int nb = sc_number2strx(buff, SC_FMT_ITEM, "%" LUA_NUMBER_FRMLEN "a", n);
    if (nb < 0)
      return -1;
    if (memchr(buff, '.', (size_t)nb) == NULL) { /* no dot? */
      char point = lua_getlocaledecpoint();
      char *ppoint = (char *)memchr(buff, point, (size_t)nb);
      if (ppoint)
        *ppoint = '.'; /* change it to a dot */
    }
    return nb;
  }
  return snprintf(buff, SC_FMT_ITEM, "%s", s);
}

/* addquoted's counting twin (%q on strings): same branches, bytes
   counted instead of emitted — the emit twin below must stay in lockstep */
static size_t sc_count_quoted(const char *s, size_t len) {
  size_t n = 2; /* the quotes */
  while (len--) {
    if (*s == '"' || *s == '\\' || *s == '\n')
      n += 2;
    else if (iscntrl(cast_uchar(*s))) {
      unsigned char c = (unsigned char)*s;
      if (isdigit(cast_uchar(*(s + 1))))
        n += 4; /* \\%03d */
      else
        n += 1 + (c < 10 ? 1 : c < 100 ? 2 : 3); /* \\%d */
    } else
      n += 1;
    s++;
  }
  return n;
}

static void sc_emit_quoted(char **pp, const char *s, size_t len) {
  char *p = *pp;
  *p++ = '"';
  while (len--) {
    if (*s == '"' || *s == '\\' || *s == '\n') {
      *p++ = '\\';
      *p++ = *s;
    } else if (iscntrl(cast_uchar(*s))) {
      if (!isdigit(cast_uchar(*(s + 1))))
        p += sprintf(p, "\\%d", (int)cast_uchar(*s));
      else
        p += sprintf(p, "\\%03d", (int)cast_uchar(*s));
    } else
      *p++ = *s;
    s++;
  }
  *p++ = '"';
  *pp = p;
}

/* luaL_tolstring's callmeta arm can re-enter Lua: if the type carries a
   __tostring, the probe declines (raw strings/numbers are safe) */
static int sc_has_tostring_meta(int tt) {
  Table *mt;
  TValue v;
  if (sc_tostring_key == NULL)
    return 1; /* pin failed: decline everything with a metatable */
  mt = G(curL)->mt[tt];
  if (mt == NULL)
    return 0;
  setnilvalue(&v); /* finishnodeget writes res only on hit */
  luaH_getshortstr(mt, sc_tostring_key, &v);
  return !ttisnil(&v); /* luaL_getmetafield's rawget, verbatim */
}

/* luaL_checkinteger's acceptance minus numeral-string coercion (stock
   coerces "42"; the probe declines string cells for numeric formats) */
static int sc_tointexact(const TValue *o, lua_Integer *out) {
  if (ttisinteger(o)) {
    *out = ivalue(o);
    return 1;
  }
  if (ttisfloat(o))
    return luaV_flttointeger(fltvalue(o), out, F2Ieq);
  return 0;
}

static int sc_format_arm(TValue *args, int n, TValue *dst, int w) {
  ScPiece pcs[SC_FMT_PIECES];
  int np = 0, arg = 1, i; /* arg counts from 1 = the fmt itself */
  size_t total = 0;
  const char *frmt, *end;
  TValue resv;
  TString *ts;
  char *p;
  char sbuff[LUAI_MAXSHORTLEN];

  if (n < 1 || !ttisstring(&args[0]))
    return 1; /* luaL_checklstring's coercion shapes: protected path */
  frmt = getstr(tsvalue(&args[0]));
  end = frmt + tsslen(tsvalue(&args[0]));

  while (frmt < end) {
    ScPiece *pc;
    if (np >= SC_FMT_PIECES)
      return 1; /* beyond 24 pieces: not a hot-lane shape */
    pc = &pcs[np];
    pc->qstr = 0;
    pc->inbuf = 0;
    pc->p = NULL;
    pc->rl = pc->l = 0;
    pc->buf[0] = '\0';
    if (*frmt != '%') { /* literal run up to the next '%' */
      const char *st = frmt;
      while (frmt < end && *frmt != '%')
        frmt++;
      pc->p = st;
      pc->l = (size_t)(frmt - st);
      total += pc->l;
      np++;
      continue;
    }
    if (*++frmt == '%') { /* %% */
      pc->p = frmt++;
      pc->l = 1;
      total += 1;
      np++;
      continue;
    }
    { /* format item */
      char form[SC_FMT_MAX_FORMAT];
      unsigned maxitem = SC_FMT_ITEM;
      const char *flags;
      int nb = 0;
      const TValue *av;
      if (++arg > n)
        return 1; /* "no value" */
      av = &args[arg - 1];
      frmt = sc_getformat(frmt, form);
      if (frmt == NULL)
        return 1;
      switch (*frmt++) {
        case 'c': {
          lua_Integer nv;
          if (!sc_checkformat(form, SC_FMTFLAGSC, 0))
            return 1;
          if (!sc_tointexact(av, &nv))
            return 1;
          nb = snprintf(pc->buf, maxitem, form, (int)nv);
          pc->inbuf = 1;
          break;
        }
        case 'd':
        case 'i':
          flags = SC_FMTFLAGSI;
          goto intcase;
        case 'u':
          flags = SC_FMTFLAGSU;
          goto intcase;
        case 'o':
        case 'x':
        case 'X':
          flags = SC_FMTFLAGSX;
        intcase : {
          lua_Integer nv;
          if (!sc_tointexact(av, &nv))
            return 1;
          if (!sc_checkformat(form, flags, 1))
            return 1;
          sc_addlenmod(form, LUA_INTEGER_FRMLEN);
          nb = snprintf(pc->buf, maxitem, form, (LUAI_UACINT)nv);
          pc->inbuf = 1;
          break;
        }
        case 'a':
        case 'A': {
          if (!ttisnumber(av))
            return 1;
          if (!sc_checkformat(form, SC_FMTFLAGSF, 1))
            return 1;
          sc_addlenmod(form, LUA_NUMBER_FRMLEN);
          nb = sc_number2strx(pc->buf, maxitem, form, nvalue(av));
          if (nb < 0)
            return 1;
          pc->inbuf = 1;
          break;
        }
        case 'f':
          maxitem = SC_FMT_ITEMF; /* extra space for '%f' */
          /* FALLTHROUGH */
        case 'e':
        case 'E':
        case 'g':
        case 'G': {
          if (!ttisnumber(av))
            return 1;
          if (!sc_checkformat(form, SC_FMTFLAGSF, 1))
            return 1;
          sc_addlenmod(form, LUA_NUMBER_FRMLEN);
          nb = snprintf(pc->buf, maxitem, form, (LUAI_UACNUMBER)nvalue(av));
          pc->inbuf = 1;
          break;
        }
        case 'q': {
          if (form[2] != '\0')
            return 1; /* "specifier '%%q' cannot have modifiers" */
          if (ttisstring(av)) { /* addliteral's raw string arm */
            pc->p = getstr(tsvalue(av));
            pc->rl = tsslen(tsvalue(av));
            pc->qstr = 1;
            pc->l = sc_count_quoted(pc->p, pc->rl);
          } else if (ttisinteger(av)) {
            lua_Integer nv = ivalue(av);
            nb = snprintf(pc->buf, SC_FMT_ITEM,
                          nv == LUA_MININTEGER
                              ? "0x%" LUA_INTEGER_FRMLEN "x"
                              : LUA_INTEGER_FMT,
                          (LUAI_UACINT)nv);
            pc->inbuf = 1;
          } else if (ttisfloat(av)) {
            nb = sc_quotefloat(pc->buf, fltvalue(av));
            if (nb < 0)
              return 1;
            pc->inbuf = 1;
          } else if (ttisnil(av) || ttisboolean(av)) {
            if (sc_has_tostring_meta(ttype(av)))
              return 1; /* addliteral goes through luaL_tolstring here */
            if (ttisnil(av)) {
              pc->p = "nil";
              pc->l = 3;
            } else if (l_isfalse(av)) {
              pc->p = "false";
              pc->l = 5;
            } else {
              pc->p = "true";
              pc->l = 4;
            }
          } else {
            return 1; /* "value has no literal form" */
          }
          break;
        }
        case 's': {
          const char *sv;
          size_t sl;
          char nbuf[LUA_N2SBUFFSZ];
          int numsrc = 0;
          if (ttisstring(av)) {
            if (sc_has_tostring_meta(LUA_TSTRING))
              return 1; /* callmeta would re-enter */
            sv = getstr(tsvalue(av));
            sl = tsslen(tsvalue(av));
          } else if (ttisnumber(av)) {
            if (sc_has_tostring_meta(LUA_TNUMBER))
              return 1; /* callmeta would re-enter */
            sl = luaO_tostringbuff(av, nbuf); /* lua_numbertocstring */
            sv = nbuf;
            numsrc = 1;
          } else if (ttisnil(av)) {
            if (sc_has_tostring_meta(LUA_TNIL))
              return 1;
            sv = "nil";
            sl = 3;
          } else if (ttisboolean(av)) {
            if (sc_has_tostring_meta(LUA_TBOOLEAN))
              return 1;
            if (l_isfalse(av)) {
              sv = "false";
              sl = 5;
            } else {
              sv = "true";
              sl = 4;
            }
          } else {
            return 1; /* __tostring/__name on tables etc.: protected path */
          }
          if (form[2] == '\0') { /* no modifiers: keep entire string */
            if (numsrc) {
              memcpy(pc->buf, nbuf, sl);
              pc->inbuf = 1;
              nb = (int)sl; /* the shared tail re-reads nb for inbuf */
            } else {
              pc->p = sv;
            }
            pc->l = sl;
          } else {
            if (ttisstring(av) && sl != strlen(sv))
              return 1; /* "string contains zeros" */
            if (!sc_checkformat(form, SC_FMTFLAGSC, 1))
              return 1;
            if (strchr(form, '.') == NULL && sl >= 100) {
              if (numsrc) { /* too long to be formatted: keep it all */
                memcpy(pc->buf, nbuf, sl);
                pc->inbuf = 1;
                nb = (int)sl; /* the shared tail re-reads nb for inbuf */
              } else {
                pc->p = sv;
              }
              pc->l = sl;
            } else { /* format the string into 'buff' */
              nb = snprintf(pc->buf, maxitem, form, sv);
              pc->inbuf = 1;
            }
          }
          break;
        }
        default:
          return 1; /* invalid conversion (also p, n, L, l, h) */
      }
      if (pc->inbuf)
        pc->l = (size_t)nb;
      total += pc->l;
      np++;
    }
  }

  if (total >= MAX_SIZE - sizeof(TString))
    return 1; /* would raise "string length overflow" */
  if (!rt_probe_mem_ok(total, 1))
    return 1; /* the shared two-sided pre-flight (U11c's law) */

  if (total > LUAI_MAXSHORTLEN) {
    ts = luaS_createlngstrobj(curL, total);
    p = getlngstr(ts);
  } else
    p = sbuff;
  for (i = 0; i < np; i++) {
    ScPiece *pc = &pcs[i];
    if (pc->qstr) {
      char *p0 = p;
      sc_emit_quoted(&p, pc->p, pc->rl);
      lua_assert((size_t)(p - p0) == pc->l); /* count/emit twins agree */
    } else if (pc->inbuf) {
      memcpy(p, pc->buf, pc->l);
      p += pc->l;
    } else {
      memcpy(p, pc->p, pc->l);
      p += pc->l;
    }
  }
  if (total <= LUAI_MAXSHORTLEN)
    ts = luaS_newlstr(curL, sbuff, total);
  setsvalue(curL, &resv, ts);
  return sc_stage(dst, w, &resv, 1);
}

/* ---- the U11e-c h_strlong residual pins: math.min/max, table.concat
** ----
**
** h_strlong's remaining protected enters were math.min (the block
** bound) and table.concat (the block fold). math_min/max are the
** ideal probe shapes: pure reads of pre-screened NUMBER cells (the
** pairs go through luaV_lessthan's LTnum arm — no metamethod, no
** allocation, nothing that can raise) and the result IS the winning
** argument cell (stock's lua_pushvalue), so the arm cannot diverge.
** tconcat is concat-probe shaped (U11c): fields read raw via
** luaH_fastgeti — a PRESENT slot is exactly lua_geti's fast path
** (a present key never consults __index), numbers fold through
** luaO_tostringbuff like every string lane, and the one result
** allocation sits behind the shared two-sided pre-flight. Every
** raise shape (min's n==0 argcheck, the table/string/integer
** argument checks, absent fields = __index territory, "invalid
** value ... in table for 'concat'", length overflow, OOM) declines
** to the protected call, which raises the stock texts. */

static int sc_minmax_arm(TValue *args, int n, TValue *dst, int w, int ismax) {
  int imin = 0, i;
  TValue resv;
  if (n < 1)
    return 1; /* "value expected": the protected path raises */
  for (i = 0; i < n; i++)
    if (!ttisnumber(&args[i]))
      return 1; /* string ordering / __lt / type error: protected path */
  for (i = 1; i < n; i++) {
    /* stock's lua_compare(L, i, imin, LUA_OPLT) (min) and
       (L, imin, i) (max), verbatim */
    if (ismax ? luaV_lessthan(curL, &args[imin], &args[i])
              : luaV_lessthan(curL, &args[i], &args[imin]))
      imin = i;
  }
  resv = args[imin]; /* lua_pushvalue: the winner verbatim */
  return sc_stage(dst, w, &resv, 1);
}

/* one tconcat field: luaH_fastgeti's PRESENT slot is exactly lua_geti's
   fast path; strings pass through, numbers fold via luaO_tostringbuff
   (lua_tolstring's own bytes). NULL = the decline/raise shapes. */
static const char *sc_tfield(Table *t, lua_Integer j, size_t *len,
                             char *nbuf) {
  TValue field;
  lu_byte tag;
  luaH_fastgeti(t, j, &field, tag);
  if (tagisempty(tag))
    return NULL; /* absent: __index metamethod territory */
  if (ttisstring(&field)) {
    *len = tsslen(tsvalue(&field));
    return getstr(tsvalue(&field));
  }
  if (ttisnumber(&field)) {
    *len = luaO_tostringbuff(&field, nbuf);
    return nbuf;
  }
  return NULL; /* "invalid value ... in table for 'concat'" */
}

static int sc_tconcat_arm(TValue *args, int n, TValue *dst, int w) {
  char nbuf[LUA_N2SBUFFSZ];   /* number fields' tostringbuff bytes */
  char sepbuf[LUA_N2SBUFFSZ]; /* a number separator's coercion bytes */
  char sbuff[LUAI_MAXSHORTLEN];
  Table *t;
  lua_Integer i, last;
  const char *sep = "";
  size_t lsep = 0, fl, total = 0;
  TValue resv;
  TString *ts;
  char *p;

  if (n < 1 || !ttistable(&args[0]))
    return 1; /* checktab's "table expected": the protected path */
  t = hvalue(&args[0]);
  if (fasttm(curL, t->metatable, TM_LEN) == NULL) /* no __len */
    last = l_castU2S(luaH_getn(curL, t)); /* luaL_len's plain border */
  else
    return 1; /* the __len metamethod: the protected path */
  if (n >= 2 && !ttisnil(&args[1])) { /* luaL_optlstring's "" default */
    if (ttisstring(&args[1])) {
      sep = getstr(tsvalue(&args[1]));
      lsep = tsslen(tsvalue(&args[1]));
    } else if (ttisnumber(&args[1])) { /* stock's number coercion */
      lsep = luaO_tostringbuff(&args[1], sepbuf);
      sep = sepbuf;
    } else
      return 1; /* "string expected": the protected path */
  }
  i = 1; /* luaL_optinteger(L, 3, 1) */
  if (n >= 3 && !ttisnil(&args[2])) {
    if (!ttisinteger(&args[2]))
      return 1; /* coercion shapes: the protected path */
    i = ivalue(&args[2]);
  }
  if (n >= 4 && !ttisnil(&args[3])) { /* luaL_optinteger(L, 4, last) */
    if (!ttisinteger(&args[3]))
      return 1;
    last = ivalue(&args[3]);
  }
  { /* pass 1: screen every field, sum the exact total — stock's loop
       (fields i..last-1 with separators, the last field alone; the
       final field lands whenever i <= last, because stock's own loop
       variable IS i and exits at last) */
    lua_Integer j;
    const char *fp;
    for (j = i; j < last; j++) {
      fp = sc_tfield(t, j, &fl, nbuf);
      if (fp == NULL)
        return 1;
      total += fl + lsep;
    }
    if (i <= last) {
      fp = sc_tfield(t, last, &fl, nbuf);
      if (fp == NULL)
        return 1;
      total += fl;
    }
  }
  if (total >= MAX_SIZE - sizeof(TString))
    return 1; /* "string length overflow": the protected path raises */
  if (!rt_probe_mem_ok(total, 1))
    return 1; /* the shared two-sided pre-flight (U11c's law) */
  { /* pass 2: emit (no code ran between the passes; GC is frozen) */
    lua_Integer j;
    const char *fp;
    if (total > LUAI_MAXSHORTLEN) {
      ts = luaS_createlngstrobj(curL, total);
      p = getlngstr(ts);
    } else
      p = sbuff;
    for (j = i; j < last; j++) {
      fp = sc_tfield(t, j, &fl, nbuf);
      memcpy(p, fp, fl);
      p += fl;
      memcpy(p, sep, lsep);
      p += lsep;
    }
    if (i <= last) {
      fp = sc_tfield(t, last, &fl, nbuf);
      memcpy(p, fp, fl);
      p += fl;
    }
    if (total <= LUAI_MAXSHORTLEN)
      ts = luaS_newlstr(curL, sbuff, total);
  }
  setsvalue(curL, &resv, ts);
  return sc_stage(dst, w, &resv, 1);
}

/* ---- U11e-c: the gmatch iterator step (rt_gmatch_probe) ----
**
** ~600 of h_strlong's residual enters were the per-step protected
** call of the gmatch iterator — a CClosure over lstrlib.c's static
** gmatch_aux, pinned by construction at rt_set_state (gmp_pin_body
** above). What follows is lstrlib.c's pattern engine ported VERBATIM
** (check_capture .. match, reprepstate) with every raise site
** converted to a decline: this probe never raises. A raise-shaped
** step returns MISS and the protected gmatch_aux re-runs it from
** the UN-mutated userdata and raises the stock text — the probe's
** only state change is stock's own `gm->src = gm->lastmatch = e`,
** made only after the whole step's outcome is computed and its
** allocations are two-sided pre-flighted (rt_probe_mem_ok), so
** probe/protected interleaving over the shared userdata is seamless.
** Results stage at the callee cell as rt_call_addr would (truncated,
** nil-padded, fixed want only — TFORCALL never asks multret);
** 1 = MISS, else rt_call_addr's success encoding. Additive;
** LUA_RT_ABI stays 4. */
#define GMP_ESC '%'
static int gmp_err; /* a raise site tripped: decline the whole step */

static int gmp_check_capture(gmp_MatchState *ms, int l) {
  l -= '1';
  if (l_unlikely(l < 0 || l >= ms->level ||
                 ms->capture[l].len == -1)) { /* CAP_UNFINISHED */
    gmp_err = 1;
    return -1; /* "invalid capture index %%%d" — the protected path */
  }
  return l;
}

static int gmp_capture_to_close(gmp_MatchState *ms) {
  int level = ms->level;
  for (level--; level >= 0; level--)
    if (ms->capture[level].len == -1)
      return level;
  gmp_err = 1; /* "invalid pattern capture" */
  return 0;
}

static const char *gmp_classend(gmp_MatchState *ms, const char *p) {
  switch (*p++) {
    case GMP_ESC: {
      if (l_unlikely(p == ms->p_end)) {
        gmp_err = 1; /* "malformed pattern (ends with '%%')" */
        return p;
      }
      return p + 1;
    }
    case '[': {
      if (*p == '^')
        p++;
      do { /* look for a ']' */
        if (l_unlikely(p == ms->p_end)) {
          gmp_err = 1; /* "malformed pattern (missing ']')" */
          return p;
        }
        if (*(p++) == GMP_ESC && p < ms->p_end)
          p++; /* skip escapes (e.g. '%]') */
      } while (*p != ']');
      return p + 1;
    }
    default: {
      return p;
    }
  }
}

static int gmp_match_class(int c, int cl) { /* verbatim */
  int res;
  switch (tolower(cl)) {
    case 'a': res = isalpha(c); break;
    case 'c': res = iscntrl(c); break;
    case 'd': res = isdigit(c); break;
    case 'g': res = isgraph(c); break;
    case 'l': res = islower(c); break;
    case 'p': res = ispunct(c); break;
    case 's': res = isspace(c); break;
    case 'u': res = isupper(c); break;
    case 'w': res = isalnum(c); break;
    case 'x': res = isxdigit(c); break;
    case 'z': res = (c == 0); break; /* deprecated option */
    default: return (cl == c);
  }
  return (islower(cl) ? res : !res);
}

static int gmp_matchbracketclass(int c, const char *p, const char *ec) {
  /* verbatim */
  int sig = 1;
  if (*(p + 1) == '^') {
    sig = 0;
    p++; /* skip the '^' */
  }
  while (++p < ec) {
    if (*p == GMP_ESC) {
      p++;
      if (gmp_match_class(c, cast_uchar(*p)))
        return sig;
    } else if ((*(p + 1) == '-') && (p + 2 < ec)) {
      p += 2;
      if (cast_uchar(*(p - 2)) <= c && c <= cast_uchar(*p))
        return sig;
    } else if (cast_uchar(*p) == c)
      return sig;
  }
  return !sig;
}

static int gmp_singlematch(gmp_MatchState *ms, const char *s,
                           const char *p, const char *ep) { /* verbatim */
  if (s >= ms->src_end)
    return 0;
  else {
    int c = cast_uchar(*s);
    switch (*p) {
      case '.': return 1; /* matches any char */
      case GMP_ESC: return gmp_match_class(c, cast_uchar(*(p + 1)));
      case '[': return gmp_matchbracketclass(c, p, ep - 1);
      default: return (cast_uchar(*p) == c);
    }
  }
}

static const char *gmp_match(gmp_MatchState *ms, const char *s,
                             const char *p);

static const char *gmp_matchbalance(gmp_MatchState *ms, const char *s,
                                    const char *p) {
  if (l_unlikely(p >= ms->p_end - 1)) {
    gmp_err = 1; /* "malformed pattern (missing arguments to '%%b')" */
    return NULL;
  }
  if (*s != *p)
    return NULL;
  else {
    int b = *p;
    int e = *(p + 1);
    int cont = 1;
    while (++s < ms->src_end) {
      if (*s == e) {
        if (--cont == 0)
          return s + 1;
      } else if (*s == b)
        cont++;
    }
  }
  return NULL; /* string ends out of balance */
}

static const char *gmp_max_expand(gmp_MatchState *ms, const char *s,
                                  const char *p, const char *ep) {
  ptrdiff_t i = 0; /* counts maximum expand for item */
  while (gmp_singlematch(ms, s + i, p, ep))
    i++;
  /* keeps trying to match with the maximum repetitions */
  while (i >= 0) {
    const char *res = gmp_match(ms, (s + i), ep + 1);
    if (gmp_err)
      return NULL;
    if (res)
      return res;
    i--; /* else didn't match; reduce 1 repetition to try again */
  }
  return NULL;
}

static const char *gmp_min_expand(gmp_MatchState *ms, const char *s,
                                  const char *p, const char *ep) {
  for (;;) {
    const char *res = gmp_match(ms, s, ep + 1);
    if (gmp_err)
      return NULL;
    if (res != NULL)
      return res;
    else if (gmp_singlematch(ms, s, p, ep))
      s++; /* try with one more repetition */
    else
      return NULL;
  }
}

static const char *gmp_start_capture(gmp_MatchState *ms, const char *s,
                                     const char *p, int what) {
  const char *res;
  int level = ms->level;
  if (level >= 32) { /* LUA_MAXCAPTURES */
    gmp_err = 1;     /* "too many captures" */
    return NULL;
  }
  ms->capture[level].init = s;
  ms->capture[level].len = what;
  ms->level = level + 1;
  if ((res = gmp_match(ms, s, p)) == NULL) /* match failed? */
    ms->level--;                            /* undo capture */
  return res;
}

static const char *gmp_end_capture(gmp_MatchState *ms, const char *s,
                                   const char *p) {
  int l = gmp_capture_to_close(ms);
  const char *res;
  if (gmp_err)
    return NULL;
  ms->capture[l].len = s - ms->capture[l].init; /* close capture */
  if ((res = gmp_match(ms, s, p)) == NULL)      /* match failed? */
    ms->capture[l].len = -1; /* undo capture (CAP_UNFINISHED) */
  return res;
}

static const char *gmp_match_capture(gmp_MatchState *ms, const char *s,
                                     int l) {
  size_t len;
  l = gmp_check_capture(ms, l);
  if (gmp_err)
    return NULL;
  len = cast_sizet(ms->capture[l].len);
  if ((size_t)(ms->src_end - s) >= len &&
      memcmp(ms->capture[l].init, s, len) == 0)
    return s + len;
  else
    return NULL;
}

static const char *gmp_match(gmp_MatchState *ms, const char *s,
                             const char *p) {
  if (l_unlikely(ms->matchdepth-- == 0)) {
    gmp_err = 1; /* "pattern too complex" */
    return NULL;
  }
  if (gmp_err)
    return NULL; /* a lower frame tripped: unwind without walking */
init: /* using goto to optimize tail recursion */
  if (p != ms->p_end) { /* end of pattern? */
    switch (*p) {
      case '(': { /* start capture */
        if (*(p + 1) == ')') /* position capture? */
          s = gmp_start_capture(ms, s, p + 2, -2); /* CAP_POSITION */
        else
          s = gmp_start_capture(ms, s, p + 1, -1); /* CAP_UNFINISHED */
        break;
      }
      case ')': { /* end capture */
        s = gmp_end_capture(ms, s, p + 1);
        break;
      }
      case '$': {
        if ((p + 1) != ms->p_end) /* is the '$' the last char? */
          goto dflt;              /* no; go to default */
        s = (s == ms->src_end) ? s : NULL; /* check end of string */
        break;
      }
      case GMP_ESC: { /* escaped sequences not in the class format */
        switch (*(p + 1)) {
          case 'b': { /* balanced string? */
            s = gmp_matchbalance(ms, s, p + 2);
            if (s != NULL) {
              p += 4;
              goto init; /* return match(ms, s, p + 4) */
            }             /* else fail (s == NULL) */
            break;
          }
          case 'f': { /* frontier? */
            const char *ep;
            char previous;
            p += 2;
            if (l_unlikely(*p != '[')) {
              gmp_err = 1; /* "missing '[' after '%%f' in pattern" */
              return NULL;
            }
            ep = gmp_classend(ms, p); /* points to what is next */
            if (gmp_err)
              return NULL;
            previous = (s == ms->src_init) ? '\0' : *(s - 1);
            if (!gmp_matchbracketclass(cast_uchar(previous), p, ep - 1) &&
                gmp_matchbracketclass(cast_uchar(*s), p, ep - 1)) {
              p = ep;
              goto init; /* return match(ms, s, ep) */
            }
            s = NULL; /* match failed */
            break;
          }
          case '0': case '1': case '2': case '3':
          case '4': case '5': case '6': case '7':
          case '8': case '9': { /* capture results (%0-%9)? */
            s = gmp_match_capture(ms, s, cast_uchar(*(p + 1)));
            if (s != NULL) {
              p += 2;
              goto init; /* return match(ms, s, p + 2) */
            }
            break;
          }
          default:
            goto dflt;
        }
        break;
      }
      default: dflt: { /* pattern class plus optional suffix */
        const char *ep = gmp_classend(ms, p); /* optional suffix */
        if (gmp_err)
          return NULL;
        /* does not match at least once? */
        if (!gmp_singlematch(ms, s, p, ep)) {
          if (*ep == '*' || *ep == '?' || *ep == '-') { /* accept empty? */
            p = ep + 1;
            goto init; /* return match(ms, s, ep + 1) */
          } else        /* '+' or no suffix */
            s = NULL;   /* fail */
        } else {        /* matched once */
          switch (*ep) { /* handle optional suffix */
            case '?': { /* optional */
              const char *res;
              if ((res = gmp_match(ms, s + 1, ep + 1)) != NULL)
                s = res;
              else {
                p = ep + 1;
                goto init; /* else return match(ms, s, ep + 1) */
              }
              break;
            }
            case '+': /* 1 or more repetitions */
              s++;   /* 1 match already done */
              /* FALLTHROUGH */
            case '*': /* 0 or more repetitions */
              s = gmp_max_expand(ms, s, p, ep);
              break;
            case '-': /* 0 or more repetitions (minimum) */
              s = gmp_min_expand(ms, s, p, ep);
              break;
            default: /* no suffix */
              s++;
              p = ep;
              goto init; /* return match(ms, s + 1, ep) */
          }
        }
        break;
      }
    }
  }
  ms->matchdepth++;
  return s;
}

static void gmp_reprepstate(gmp_MatchState *ms) { /* verbatim */
  ms->matchdepth = GMP_MAXCCALLS;
  ms->level = 0;
}

int32_t rt_gmatch_probe(rt_addr funcell, int32_t want) {
  const TValue *fn = (const TValue *)(size_t)funcell;
  TValue *dst = (TValue *)(size_t)funcell;
  int w = (int)want, i, nlevels, nstr;
  size_t need;
  CClosure *cl;
  gmp_GMatchState *gm;
  const char *src, *e;
  TValue res[32];
  Udata *u;
  if (sc_gmatch_aux == NULL || w < 1 || !ttisCclosure(fn))
    return 1;
  cl = clCvalue(fn);
  if (cl->f != sc_gmatch_aux || cl->nupvalues != 3 ||
      !ttisstring(&cl->upvalue[0]) || !ttisstring(&cl->upvalue[1]) ||
      !ttisfulluserdata(&cl->upvalue[2]))
    return 1;
  u = uvalue(&cl->upvalue[2]);
  if (u->nuvalue != 0 || u->len != sizeof(gmp_GMatchState))
    return 1;
  gm = (gmp_GMatchState *)getudatamem(u);
  gm->ms.L = curL; /* verbatim (gmatch_aux's first line) */
  gmp_err = 0;
  for (src = gm->src; src <= gm->ms.src_end; src++) {
    gmp_reprepstate(&gm->ms);
    e = gmp_match(&gm->ms, src, gm->p);
    if (gmp_err)
      return 1; /* raise-shaped: the protected step raises the stock text */
    if (e != NULL && e != gm->lastmatch) {
      /* push_captures computed, not pushed: every capture validated and
         its bytes summed BEFORE any mutation or allocation */
      nlevels = (gm->ms.level == 0 && src) ? 1 : gm->ms.level;
      need = 0;
      nstr = 0;
      for (i = 0; i < nlevels; i++) {
        const char *cap;
        ptrdiff_t l;
        if (i >= gm->ms.level) {
          /* i==0, level==0: the whole match */
          cap = src;
          l = e - src;
        } else {
          l = gm->ms.capture[i].len;
          cap = gm->ms.capture[i].init;
          if (l_unlikely(l == -1)) /* CAP_UNFINISHED */
            return 1; /* "unfinished capture" — the protected path */
          if (l == -2) /* CAP_POSITION: an integer result, no bytes */
            continue;
        }
        need += (size_t)l + sizeof(TString) + 1;
        nstr++;
      }
      /* the shared two-sided pre-flight (sum-contiguous: stricter than
         nstr separate requests ever need; mayintern covers the one
         growstrtab peak a step can trigger) */
      if (nstr > 0 && !rt_probe_mem_ok(need, 1))
        return 1;
      gm->src = gm->lastmatch = e; /* stock's only state change */
      for (i = 0; i < nlevels; i++) {
        const char *cap;
        ptrdiff_t l;
        if (i >= gm->ms.level) {
          cap = src;
          l = e - src;
        } else {
          l = gm->ms.capture[i].len;
          cap = gm->ms.capture[i].init;
          if (l == -2) { /* CAP_POSITION */
            setivalue(&res[i], ct_diff2S(cap - gm->ms.src_init) + 1);
            continue;
          }
        }
        setsvalue(curL, &res[i], luaS_newlstr(curL, cap, cast_sizet(l)));
      }
      return sc_stage(dst, w, res, nlevels);
    }
  }
  return sc_stage(dst, w, res, 0); /* exhausted: no values, nil-padded */
}

/* ---- U6b (row 37): the deadline control block ----
**
** g_deadline_flag is host-written and guest-polled. The host arms it
** when the run's deadline expires — either by calling rt_set_deadline
** (main thread) or, from a watchdog goroutine, a single 4-byte
** little-endian store of 1 at rt_ctrl_addr() (wasm engines cannot take
** API calls off-thread; an aligned word store into linear memory is the
** one safe crossing — the blob declares a memory max, so the base never
** moves under wasmtime's static memories). The backend emits a
** load-and-branch against it at every flattened dispatch-loop head and
** at the lua_dispatch restage loop (and rtw_dispatch_body's own restage
** loop below polls it directly — the ABI-level tail chains that never
** re-enter the backend's loop); a set flag → rt_deadline stages an
** ordinary Lua error (pcall-catchable, like the OOM raise).
**
** The flag is sticky for the run: a script can pcall-catch the deadline
** error, but its next back edge re-raises — no livelock, no progress.
** Fresh-VM-per-run resets it by construction (hosts may rt_set_deadline
** (0) to disarm). Polling is cooperative: a single non-returning C call
** (a giant string.rep) never sees a poll — the alloc cap bounds those.
*/
int32_t g_deadline_flag;

rt_addr rt_ctrl_addr(void) { return (rt_addr)(size_t)&g_deadline_flag; }

void rt_set_deadline(int32_t on) { g_deadline_flag = on ? 1 : 0; }

int32_t rt_deadline_flag(void) { return g_deadline_flag; }

/* rt_deadline: stage the deadline error and refuse — the emitted poll
   returns -1 on it, so the error unwinds the wasm frame chain exactly
   like any rt_* error; the adapter re-raises and pcall can catch. Body
   text matches the interp oracle's cancellation message byte-for-byte
   (mainLoopWithContext raises ctx.Err() — "context deadline exceeded"
   for a deadline context); the position prefix diverges (the poll site
   has no line info; interp's raise rides an instruction boundary) —
   ruled in ledger row 37, carried here unchanged. */
int32_t rt_deadline(void) {
  if (!err_pending) {
    if (curL != NULL) {
      stage_str_error("context deadline exceeded", 1); /* prefixed: no pos */
    } else { /* exported entry, no state set: buffer-only staging */
      static const char msg[] = "context deadline exceeded";
      size_t n = sizeof msg - 1;
      setnilvalue(&err_value);
      memcpy(err_buf, msg, n + 1);
      err_buf_len = (int)n;
      err_prefixed = 1;
      err_pending = 1;
    }
  }
  return RT_ERR;
}

/* ---- U6c (rows 33/50, ledger row 94): sandbox + globals lockdown ---
**
** The v3 surface (rt_abi.c :240-379) re-pinned on 5.5 globals. The list
** is 5.5-shaped: loadstring/module never existed here, warn is new
** (it writes through a host callback the prod flavor does not ship).
** lua_getglobal/lua_setglobal address the registry globals table —
** exactly what the main proto's seeded _ENV cell points at — so the
** nil-ing is visible to compiled readers with no _ENV plumbing.
**
** The protection half is zero-patch (plan §4.4 primary design): v3
** flagged the Table struct itself (our row-50 patches into the 5.1
** tree); stock 5.5 offers no readonly-table API, so the rt owns a
** protected-set and rt_seam55.c's seven interposed luaH_* write guards
** consult it (delegates = ltable_stock55.c, the renamed second
** compile). The set holds ~20-30 tables; every compiled table write
** already crosses rt_settable -> lua_settable -> a guard, so the
** linear scan is noise.
**
** rt_sandbox runs its whole body under rt_run protection: v3 ran it
** bare, but engines may combine MaxMem+Sandbox and an OOM inside a
** bare setglobal would longjmp with errorJmp NULL (the state dies).
** Same defensive shape for the protect walk (its allocations are
** OOM-able). The one-way contract is v3's: on=0 returns -1, there is
** no unprotect.
*/
static const char *const rt_sandbox_nil[] = {
    "io",     "os",    "package", "require", "dofile",
    "loadfile", "load", "debug",  "warn",   NULL};

static int sb_count; /* the body's live count, read after rt_run */

static void sandbox_body(void) {
  lua_State *L = curL;
  const char *const *p;
  int n = 0;
  for (p = rt_sandbox_nil; *p != NULL; p++) {
    lua_getglobal(L, *p);
    {
      int was_there = !lua_isnil(L, -1);
      lua_pop(L, 1);
      if (was_there) {
        lua_pushnil(L);
        lua_setglobal(L, *p);
        n++;
      }
    }
  }
  sb_count = n;
}

int32_t rt_sandbox(int32_t on) {
  if (!on || curL == NULL) return -1;
  if (rt_run(sandbox_body, 0, -1, -1, -1, NULL) != RT_OK) return -1;
  return sb_count;
}

/* the rt-owned protected set (rt_seam55.c's guards consult it; empty
   when unarmed = one length check per guarded write) */
static Table **prot_set;
static int prot_cap; /* prot_n lives with the early tentatives above */

int rt_prot_isprotected(const void *t) {
  int i;
  for (i = 0; i < prot_n; i++)
    if (prot_set[i] == (const Table *)t) return 1;
  return 0;
}

/* add t (raising on OOM — every caller runs under rt_run); returns
   whether it was newly added (0 = the cycle guard hit) */
static int prot_add(lua_State *L, Table *t) {
  if (rt_prot_isprotected(t)) return 0;
  if (prot_n == prot_cap) {
    int ncap = prot_cap ? prot_cap * 2 : 32;
    Table **ns = (Table **)realloc(prot_set, (size_t)ncap * sizeof *ns);
    if (ns == NULL) {
      luaG_runerror(L, "not enough memory");
      return 0; /* unreachable (noret) */
    }
    prot_set = ns;
    prot_cap = ncap;
  }
  prot_set[prot_n++] = t;
  return 1;
}

static int prot_remove(Table *t) {
  int i;
  for (i = 0; i < prot_n; i++)
    if (prot_set[i] == t) {
      prot_set[i] = prot_set[--prot_n]; /* swap-delete: order is not
                                            observable */
      return 1;
    }
  return 0;
}

/* rt_protected_index: the __index raise (Redis 7.2.7's
   luaProtectedTableError — texts verbatim). Missing-global READS only;
   present keys answer from the table without consulting it. */
static int rt_protected_index(lua_State *L) {
  int argc = lua_gettop(L);
  if (argc != 2)
    luaL_error(L, "Wrong number of arguments to luaProtectedTableError");
  if (!lua_isstring(L, -1) && !lua_isnumber(L, -1))
    luaL_error(L,
               "Second argument to luaProtectedTableError must be a "
               "string or number");
  luaL_error(L, "Script attempted to access nonexistent global variable "
                "'%s'",
             lua_tostring(L, -1));
  return 0; /* unreachable (noret) */
}

/* the recursive walk: the table sits at the top of the stack. Depth is
   bounded by the stdlib's nesting (~3: _G -> string -> its metatable);
   the C stack cost is fine. The set doubles as the visited set —
   shared metatables and _G's self-reference re-hit the cycle guard. */
static void prot_walk_rec(lua_State *L) {
  Table *t = hvalue(s2v(L->top.p - 1));
  if (rt_prot_isprotected(t)) return;
  (void)prot_add(L, t);
  luaD_checkstack(L, 3);
  if (lua_getmetatable(L, -1)) { /* has one? (pushed at the top) */
    prot_walk_rec(L);
    lua_pop(L, 1);
  }
  lua_pushnil(L);
  while (lua_next(L, -2)) { /* key at -2, value at -1 */
    if (lua_istable(L, -1)) prot_walk_rec(L);
    lua_pop(L, 1); /* the value; lua_next keeps iterating from the key */
  }
}

static void protect_body(void) {
  lua_State *L = curL;
  luaD_checkstack(L, 5);
  lua_pushglobaltable(L);
  /* the error metatable (public API only; setmetatable writes
     t->metatable directly, so installing it over a protected table is
     the one sanctioned write — see row 94's rulings) */
  lua_newtable(L);
  lua_pushcfunction(L, rt_protected_index);
  lua_setfield(L, -2, "__index");
  lua_setmetatable(L, -2);
  /* walk from _G itself (the walk protects the fresh metatable too —
     its contents are then unmodifiable) */
  prot_walk_rec(L);
  lua_pop(L, 1);
  /* the shared string metatable is not reachable from _G fields —
     probe it explicitly (Redis parity: string-library writes raise) */
  lua_pushliteral(L, "");
  if (lua_getmetatable(L, -1)) {
    prot_walk_rec(L);
    lua_pop(L, 1);
  }
  lua_pop(L, 1);
}

int32_t rt_protect_globals(int32_t on) {
  if (!on || curL == NULL) return -1; /* one-way per VM (v3 contract) */
  return rt_run(protect_body, 0, -1, -1, -1, NULL) != RT_OK ? -1 : 0;
}

/* the KEYS/ARGV staging window: _G leaves/re-enters the protected set
   (v3 toggled _G's own readonly flag; the zero-patch set does the
   membership instead). The metatable stays installed either way —
   reads of MISSING globals still raise, which is correct during
   staging (the host writes KEYS/ARGV, never reads-missing). */
static int ro_on;

static void readonly_body(void) {
  Table *g;
  lua_pushglobaltable(curL);
  g = hvalue(s2v(curL->top.p - 1));
  if (ro_on)
    (void)prot_add(curL, g);
  else
    (void)prot_remove(g);
  curL->top.p--;
}

int32_t rt_globals_readonly(int32_t on) {
  if (curL == NULL) return -1;
  ro_on = (on != 0);
  return rt_run(readonly_body, 0, -1, -1, -1, NULL) != RT_OK ? -1 : 0;
}

/* ---- U6e (row 96): the host-fn seam — the registration half ----
**
** v3's rt_hostfn rode lua_cpcall, which 5.5 retired; the protection
** here is rt_run (registration allocates: interned name, CClosure —
** same defensive shape as the U6c bodies). The CClosure body is
** rt_hf_closure (luawasm55.c, RT flavor): it marshals the call over
** host.host_call, raises verbatim via rt_sea_raise_nopos, and decodes
** results back onto the Lua stack. table/name point into the host's
** staging namebuf (NUL-terminated, v3's contract); the statics ride
** the C stack across the protected body — the reentrancy law's uniform
** discipline, though the constructor call cannot re-enter compiled
** code. */
static const char *hf_table, *hf_name;
static int hf_fnidx;

static void hf_reg_body(void) {
  lua_State *L = curL;
  lua_getglobal(L, hf_table);
  if (!lua_istable(L, -1)) { /* absent or shadowed by a non-table: make one */
    lua_pop(L, 1);
    lua_newtable(L);
    lua_pushvalue(L, -1);
    lua_setglobal(L, hf_table);
  }
  lua_pushinteger(L, hf_fnidx);
  lua_pushcclosure(L, rt_hf_closure, 1);
  lua_setfield(L, -2, hf_name);
  lua_pop(L, 1);
}

int32_t rt_hostfn(rt_addr table_ptr, rt_addr name_ptr, int32_t fnidx) {
  if (curL == NULL) return -1;
  hf_table = (const char *)(size_t)table_ptr;
  hf_name = (const char *)(size_t)name_ptr;
  hf_fnidx = (int)fnidx;
  return rt_run(hf_reg_body, 0, -1, -1, -1, NULL) != RT_OK ? -1 : 0;
}

/* rt_encode_value: the TValue CELL at `cell` in the wire protocol
   (tables expanded, PT_INTEGER distinct — int64 lossless) into
   dst[0..cap). Quiescent-state host call (between runs, like v3):
   encoding is raw traversal (lua_next honors no metamethods) and
   allocation-free, so no rt_run protection is needed. The cell is
   copied onto the stack with the push-nil-then-overwrite idiom (the
   slot must hold a valid TValue before top moves); rt_hf_encode_top
   (luawasm55.c) runs the shared encoder over the stack top. Returns
   the FULL encoded length — the host re-calls with a larger buffer on
   truncation — or -1. */
int32_t rt_encode_value(rt_addr cell, rt_addr dst, int32_t cap) {
  lua_State *L = curL;
  int base, n;
  if (L == NULL) return -1;
  base = lua_gettop(L);
  luaD_checkstack(L, 1);
  setnilvalue(s2v(L->top.p));
  L->top.p++;
  setobj2s(L, L->top.p - 1, (const TValue *)(size_t)cell);
  n = rt_hf_encode_top(L, dst, cap);
  L->top.p--;
  lua_settop(L, base);
  return n;
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

/* ---- U11e-b: the _ENV staging cell for GETTABUP/SETTABUP ----
**
** Every per-loop globals access paid a protected _ENV enter. This
** stages the globals-table TValue into a cell PLAIN: it is stock
** lua_getglobal's own first step (the rawgetint on the registry,
** lapi.c) — allocation-free, runs no code, cannot raise. rt_gets_probe
** / rt_sets_probe (the U11a short-string posture) then do the access
** against the cell; every miss (absent globals slot, long key, a
** metamethod, protected globals) re-issues to the protected
** rt_getglobal/rt_setglobal above. Re-read per access, like stock —
** lua_getglobal re-reads registry[LUA_RIDX_GLOBALS] on every call. */
void rt_globals_cell(rt_addr cell) {
  TValue st;
  setnilvalue(&st); /* getint writes its out-cell only on hit (U11d) */
  if (ttistable(&G(curL)->l_registry))
    luaH_getint(hvalue(&G(curL)->l_registry), LUA_RIDX_GLOBALS, &st);
  *(TValue *)(size_t)cell = st;
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

/* Row 92: multret staging headroom, in cells, guaranteed ABOVE every
   frame window (chunk-resident ones here; the main block gets the same
   margin through the backend's gFrameCells — the twin constant
   multretHeadroomCells in luawasm/emit.go; keep them equal). A call's
   results stage at R(A).. with a count only the callee knows (want=-1);
   stock grows the Lua stack there, the wasm window is fixed, so the
   region must absorb this many result cells past the window without
   running off its end into the malloc heap (limits01's 254-wide
   select('#', loaded()) is the covering corpus case). A count beyond it
   is memory corruption until growable windows exist. */
#define RT_HEADROOM_CELLS 4096

/* size, bump and arg-fill a frame from a TValue array (the trampoline
   passes the L stack; the M5c tail restage passes the staging buffer). */
static rt_addr rt_push_frame_from(int mdidx, int nargs, const TValue *src) {
  struct rt_wasm_md *m;
  uint32_t need, needhr;
  rt_addr block, frame;
  int np, nv, i;

  m = &rt_md[mdidx];
  nv = (m->isvararg && nargs > m->numparams) ? nargs - m->numparams : 0;
  /* U6a (row 86): the extras sit BELOW the register window — the block
     is [extras nv][window framecells][1 spare] and the window base is
     block+16*nv. Stock and the interp place them below the frame base;
     above, every dynamic window the emitted code legitimately stages
     past the window top (multret VARARG flush tails, call arg windows,
     result moves) used to land on them (vgap01/var07/limits01, row
     61(d)); past the window is now spare/virgin chunk space — but only
     RT_HEADROOM_CELLS of it are GUARANTEED (row 92): the chunk test
     below abandons a tail that cannot cover window+headroom. */
  need = (uint32_t)sizeof(TValue) * (uint32_t)(nv + m->framecells + 1);
  needhr = need + (uint32_t)sizeof(TValue) * RT_HEADROOM_CELLS;
  if (rt_fr == NULL || rt_fr->cap - rt_fr->used < needhr) {
    uint32_t cap = needhr > (1u << 20) ? needhr * 2 : (1u << 20);
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
  block = rt_fr->base + rt_fr->used;
  rt_fr->used += need;
  frame = block + (rt_addr)sizeof(TValue) * (rt_addr)nv;

  /* copy args: params -> frame+0.., extras -> block+0.. (below the
     window; the emitted code addresses them at frame-16*nv — its
     varargBase). Non-vararg protos drop extras, like stock
     luaD_precall. */
  np = nargs < m->numparams ? nargs : m->numparams;
  for (i = 0; i < np; i++)
    *(TValue *)(size_t)(frame + (rt_addr)sizeof(TValue) * i) = src[i];
  if (m->isvararg) {
    for (i = 0; i < nv; i++)
      *(TValue *)(size_t)(block + (rt_addr)sizeof(TValue) * i) =
          src[m->numparams + i];
  }
  return frame; /* the window base; rt_frame_restore takes the block */
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
** U6a: funcell/argcells take the callee's address with the args at
** their natural R(A+1).. position (the rt_call_addr form); the decline
** path leaves the cells untouched, so the emitted code's fallback is a
** plain rt_call_addr on the same operands. Reentrancy: stage->restage
** is a closed cycle inside one dispatch — the staged fields are
** consumed before any nested dispatch can run. */

static TValue *tail_args;
static int tail_nargs, tail_idx;
static rt_addr tail_cl;

/* U6a (row 86): the dispatch result-frame shadow. The wasm module's own
   restage loop (lua_dispatch — the backend's emitDispatch) consumes the
   -2 sentinel INSIDE one wasm_dispatch_host call and re-dispatches the
   successor at the tailcaller's BLOCK, which the below-window extras
   layout put 16*nv under the frame this dispatch level pushed. The
   successor stages its results at ITS frame, so the dispatch body must
   copy from the last restaged frame, not the one it pushed (pre-U6a the
   two coincided). rt_tail_restage publishes each push here; the body
   brackets its host call save/set/adopt/restore so nesting stays exact —
   a deeper dispatch's own bracket returns whatever THIS level last
   staged, so no inner restage can leak its frame up. */
static rt_addr rt_disp_frame;

int32_t rt_tail_stage(rt_addr funcell, rt_addr argcells, int32_t nargs,
                      rt_addr block) {
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
  rt_frame_restore(block); /* the tailcalling frame is dead. U6a: this
                              is the BLOCK address (window - 16*nv);
                              the cursor trims to the allocation start */
  return tail_idx;
}

int32_t rt_tail_clidx(void) { return tail_idx; }
int32_t rt_tail_nargs(void) { return tail_nargs; }
rt_addr rt_tail_funcell(void) { return tail_cl; }

rt_addr rt_tail_restage(void) {
  rt_addr frame = rt_push_frame_from(tail_idx, tail_nargs, tail_args);
  if (frame != 0) rt_disp_frame = frame; /* the dispatch body adopts it */
  return frame;
}

/* ---- U9c (row 104): direct in-module dispatch --------------------------
**
** A non-tail OP_CALL/TFORCALL whose callee is one of THIS module's
** compiled closures dispatches the module's own lua_dispatch directly
** from wasm: probe -> push frame -> wasm call -> retire. No C precall,
** no protect layer, no host seam crossing. Everything else (C
** functions, __call chains, foreign-registry closures) declines to the
** unchanged rt_call_addr fallback — the M5c decline pattern.
**
** Depth budget: a C static, NOT a wasm global. A trap caught by a
** mid-run C pcall skips every wasm-side decrement and lua_main never
** re-runs its prologue, so a wasm counter would ratchet until every
** later call falsely overflows. rt_dd_enter increments; rt_dd_ret /
** rt_dd_err decrement; rtw_dispatch_body bracket-restores around its
** host call (every trap surfaces at SOME host boundary — the engine
** stack-exhaustion trap returns through the wasm_dispatch_host import
** as -4, control resuming normally in C — so the bracket heals every
** leak); rt_dd_reset at lua_main's prologue covers run boundaries (a
** trap that surfaced at main leaves the counter high until the next
** run of a pooled VM). Direct frames consume no C stack, so
** rt_wasm_depth is untouched: RTW_MAX_DEPTH still bounds trampoline
** nesting only. The constant's derivation lives in ledger row 104. */
static int rt_dd_depth;
static rt_addr dd_disp_save[RT_DD_MAX_DEPTH];
static CallInfo *dd_ci_save[RT_DD_MAX_DEPTH]; /* NULL = degraded level */

/* the fused probe: (cl record address << 32) | (mdidx+1); 0 = decline.
   Exact on wasm32 (pointers are 32-bit); under RT_ABI_NATIVE64 the
   high word carries only the record pointer's low 32 bits — native
   tests mask, the low word is always the full truth. Read-only: never
   refuses on a pending staging, never touches the cells. */
int64_t rt_dd_callee(rt_addr funcell) {
  const TValue *f = (const TValue *)(size_t)funcell;
  if (ttisCclosure(f)) {
    CClosure *c = clCvalue(f);
    if (c->f == rt_wasm_trampoline) {
      struct rt_wcl *rec = (struct rt_wcl *)pvalue(&c->upvalue[0]);
      if (rec->mdidx >= 0 && rec->mdidx < rt_md_n)
        return ((int64_t)(uint32_t)(size_t)rec << 32) |
               (int64_t)(uint32_t)(rec->mdidx + 1);
    }
  }
  return 0;
}

/* the callee's stack presence (row 104): every direct frame gets a
   minimal real CallInfo — prepCallInfo's exact shape over a func slot
   holding the closure, CIST_C like the trampoline's own frame — plus a
   sea entry keyed by that ci. Stock lua_getstack/getinfo and the seam's
   luaL_where then resolve direct frames exactly like trampoline ones:
   level-1 getinfo answers istailcall/extraargs (0 — a direct callee
   passed through no __call chain, the interp's answer), where(1) is the
   callee's line, where(2) the caller's. luaE_extendCI's err=0 arm
   REPORTS OOM instead of raising (ldo.c's own non-raising use), so no
   longjmp can start here; on NULL the frame runs ci-less — the degraded
   posture (no levels for it; the callee prologue then binds the
   caller's sea entry, which dd_ret's rebind heals on the way out). */

/* pop the presence for the current (deepest) level — ret's and err's
   exit share it. `retract` restores L->top to the closure slot and is
   for the NORMAL return only: there the stack above the slot is exactly
   the callee's balanced windows (luaD_poscall's own discipline — it too
   runs only on a normal return). The error path passes 0: an error
   leaves live content above the slot (call_body's un-retracted window,
   the error object at L->top-1) that the CATCH boundary owns
   (luaD_pcall resets L->top to its save point; stock never lets an
   intermediate frame retract past a throw) — retracting here clobbers
   it. Same for the trap unwinds: the catch or the next run boundary
   cleans up. The ci pop itself is safe on every path (the error object
   is not ci-relative), and required — the in-wasm -1 cascade keeps
   executing (close handlers) with a truthful L->ci. */
static void dd_pop_ci(int retract) {
  CallInfo *ci = dd_ci_save[rt_dd_depth - 1];
  if (ci != NULL) {
    lua_State *L = curL;
    dd_ci_save[rt_dd_depth - 1] = NULL;
    rt_sea_pop();              /* LIFO: entered ci-then-sea, pop sea first */
    if (retract) L->top.p = ci->func.p; /* retract the closure slot */
    L->ci = ci->previous;      /* stock's pop */
  }
}

/* unwind leaked direct frames down to `depth`: a trap kills a wasm
   activation without running its rt_dd_ret/rt_dd_err, and L->ci is the
   dead activation's deepest frame (nothing else pushes at a wasm call
   boundary), so popping level by level restores L->ci/L->top/sea to the
   save point exactly. Callers: the rtw_dispatch_body bracket (every
   trap surfaces at some host boundary) and rt_dd_reset (run
   boundaries). */
static void dd_unwind_to(int depth) {
  while (rt_dd_depth > depth) {
    dd_pop_ci(0); /* trap path: no retract (see dd_pop_ci) */
    rt_dd_depth--;
  }
}

/* push the callee frame — the same rt_push_frame_from the trampoline
   uses (vararg extras below the window, rt_md sizing), reading args
   from their natural R(A+1).. cells — plus the callee's stack presence
   (a minimal CallInfo + sea entry, the block comment above rt_dd_callee).
   Takes the probe's low word
   (mdidx+1; the emitted code forwards it unopened) and the call site's
   line (rt_call_addr's convention). Returns the window base, or 0 WITH
   an error staged (budget/OOM — the caller returns -1 and the error
   propagates as a staged return; the budget arm stages PREFIXED, see
   the arm). Also saves the
   trampoline's rt_disp_frame shadow at this depth: the callee's
   wasm-side restage loop publishes restages into that single static,
   and without the save/restore pair in ret/err a restage inside a
   direct callee would clobber the OUTER trampoline level's adopt frame
   (rtw_dispatch_body reads it after its host call). */
rt_addr rt_dd_enter(int32_t mdidx1, rt_addr argcells, int32_t nargs,
                    int32_t line) {
  rt_addr frame;
  int mdidx = (int)mdidx1 - 1;
  if (mdidx < 0 || mdidx >= rt_md_n) return 0; /* probe bounded it */
  if (rt_dd_depth >= RT_DD_MAX_DEPTH) {
    /* stock's own wording (lstate.c luaE_checkcstack), staged WITH the
       call site's position: a budget hit rides a -1 cascade back to the
       outermost trampoline, which re-raises the staged value verbatim —
       no rt_run catch ever prefixes it (the trampoline depth guard gets
       its prefix from luaG_runerror, whose isLua arm skips compiled
       frames and so never fired for the caller anyway). Same formatting
       as rt_error. */
    int w = snprintf(err_buf, sizeof err_buf, "%.*s:%d: stack overflow",
                     chunk_name_len, chunk_name, (int)line);
    err_buf_len = w > 0 ? w : 0;
    err_prefixed = 1;
    err_pending = 1;
    rt_sea_msgh_ran_set(0); /* fresh error: its handler has not run */
    stage_err_line((int)line);
    setsvalue(curL, &err_value,
              luaS_newlstr(curL, err_buf, (size_t)err_buf_len));
    return 0;
  }
  frame = rt_push_frame_from(mdidx, (int)nargs, (const TValue *)(size_t)argcells);
  if (frame == 0) {
    stage_str_error("not enough memory", 1);
    return 0;
  }
  { /* push the callee's stack presence — see the block comment above.
       Gated on headroom: each direct level parks ONE closure slot above
       L->top for the callee's lifetime, and a deep direct chain stacks
       them — beyond the containing frames' checkstack guarantees the
       writes would run off the Lua stack into the heap (found the hard
       way: deep OP_CALL recursion corrupted the globals table). No
       wasm-safe growth exists (luaD_growstack raises), so past the
       headroom the levels degrade to the ci-less posture — the ruled
       fallback, exactly like the extendCI-NULL case. */
    lua_State *L = curL;
    CallInfo *ci = NULL;
    if (L->top.p < L->stack_last.p - LUA_MINSTACK) {
      CallInfo *prev = L->ci;
      ci = prev->next != NULL ? prev->next : luaE_extendCI(L, 0);
      if (ci != NULL) {
        TValue *f = (TValue *)(size_t)(argcells - (rt_addr)sizeof(TValue));
        setobj2s(L, L->top.p, f); /* getinfo 'f'/'u' read this slot */
        ci->func.p = L->top.p;
        L->top.p++; /* one slot, inside the headroom just checked */
        ci->callstatus = CIST_C; /* nresults/CCMT/TAIL bits 0 */
        ci->top.p = L->top.p;
        ci->previous = prev; /* restated; extendCI/shrinkCI maintain it */
        L->ci = ci;
        rt_sea_push(ci); /* the callee prologue's setline binds here */
      }
    }
    dd_ci_save[rt_dd_depth] = ci; /* NULL = degraded */
  }
  dd_disp_save[rt_dd_depth] = rt_disp_frame;
  rt_dd_depth++;
  return frame;
}

/* retire a successful direct dispatch: stage want-truncated results at
   R(A) (argcells-16) with EXPLICIT nil padding of short returns — the
   C path gets its padding from luaD_poscall; nothing between here and
   the wasm callee runs one — then restore the caller's frame region,
   rebind the caller's line cell (the callee's prologue repointed the
   shared sea entry at its own cell; without this the caller's next
   level-1 raise reads a stale line off a dead frame cell), restore the
   saved rt_disp_frame shadow, and return rt_call_addr's success
   encoding so the emitted top-adjust is reused verbatim. `frame` is
   lua_dispatch's EXIT frame (the emitted code passes the module
   global lua_dispatch publishes on return — the M5c restage loop may
   have moved the window 16*nv' off the block rt_dd_enter allocated). */
int32_t rt_dd_ret(rt_addr saved, rt_addr argcells, rt_addr frame,
                  int32_t nret, int32_t want, rt_addr linecell) {
  TValue *dst = (TValue *)(size_t)(argcells - (rt_addr)sizeof(TValue));
  const TValue *src = (const TValue *)(size_t)frame;
  int w = (int)want, n = (int)nret, i;
  TValue nilv;
  dd_pop_ci(1); /* normal return: poscall's retract discipline */
  if (n < 0) n = 0;
  setnilvalue(&nilv);
  if (w >= 0) {
    for (i = 0; i < w; i++)
      dst[i] = (i < n) ? src[i] : nilv;
    n = w;
  } else {
    for (i = 0; i < n; i++)
      dst[i] = src[i];
  }
  rt_disp_frame = dd_disp_save[rt_dd_depth - 1];
  rt_dd_depth--;
  rt_frame_restore(saved);
  rt_sea_setline(linecell);
  return w < 0 ? -(n + 1) : RT_OK;
}

/* retire a failed one: the callee returned -1 with the error staged.
   Close the dying callee frames with the unwinding-boundary form BEFORE
   the region goes away (cells must stay allocated while __close runs —
   the trampoline arm's ordering). The INNERMOST rt_dd_err does the
   work; each outer level of the -1 chain finds the list already clean
   (rt_close_err self-protects). The error stays staged — the caller
   returns -1 in turn, exactly the in-wasm error protocol. */
int32_t rt_dd_err(rt_addr saved, rt_addr linecell) {
  rt_close_err(saved);
  dd_pop_ci(0); /* after the close: __close handlers run inside the dying
                   frame, with its ci/sea entry still live; no retract —
                   the catch boundary owns L->top on the error path */
  rt_disp_frame = dd_disp_save[rt_dd_depth - 1];
  rt_dd_depth--;
  rt_frame_restore(saved);
  rt_sea_setline(linecell);
  return -1;
}

void rt_dd_reset(void) {
  dd_unwind_to(0); /* also unwinds a lua_main-level trap's leaked frames */
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
    /* U6a (row 86): re-base the result frame around the host call — the
       wasm-side restage loop may move the running frame inside it (see
       rt_disp_frame above); the copy below reads c->frame afterward. */
    rt_addr saved_df = rt_disp_frame;
    rt_disp_frame = c->frame;
    {
      /* U9c (row 104): heal the direct-dispatch state at this host
         boundary. A trap caught deeper (engine stack exhaustion returns
         through the import as -4) skipped every wasm-side rt_dd_ret/
         rt_dd_err — unwind the leaked levels so a pcall-catch-continue
         run keeps its full budget AND its CallInfo/sea stack (the dead
         activation's deepest frame is L->ci, so the pops land exactly). */
      int saved_dd = rt_dd_depth;
      c->nret = wasm_dispatch_host(c->idx, c->frame, c->cl, c->nargs, c->want);
      dd_unwind_to(saved_dd);
    }
    c->frame = rt_disp_frame; /* adopt this level's latest restage */
    rt_disp_frame = saved_df;
    if (c->nret != RTW_TAIL) break;
    /* U6b (row 37): the restage loop is an unbounded back edge the
       backend's own poll may never see (ABI-level tail chains) — poll
       the control-block flag directly and map it to the same staged
       deadline error. Mirrors the wasm-side poll in lua_dispatch. */
    if (g_deadline_flag) {
      rt_deadline();
      c->nret = RTW_ERR;
      break;
    }
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

