/*
** runtime/rt_seam55.c — the row-60 error-position seam (U5e).
**
** Linked ONLY into lua55_rt.wasm (build55.sh places this object before
** the stock $SRC list with -Wl,--allow-multiple-definition: GNU ld
** resolves a multiply-defined symbol to the FIRST definition on the
** command line, so the replicas below win over the stock ones, and
** rt_abi55.c's own calls route through them). The stock tree stays
** zero-patch; the oracle blob (lua55_sjlj.wasm) never links this file.
**
** The problem being solved (ledger row 60): compiled wasm functions run
** as C closures — the stock position machinery sees a C frame with
** currentline -1 and no source, so error(msg, level) prefixes land on
** the wrong frame (or nowhere) and luaL_where pushes "" at main level
** (lua_main runs cl=0 with no CallInfo at all). The seam:
**
**   - a shadow line stack: every dispatch trampoline pushes an entry
**     keyed by its CallInfo; the wasm function's prologue binds its
**     line cell (frame+16*(nregs+4)) into the top entry, and every
**     emitted instruction first stores its source line there — so the
**     cell always holds the CURRENT line of every live compiled frame;
**
**   - interposed luaL_where resolves the level through the shadow
**     first (including the main chunk's bottom entry, keyed by
**     &base_ci, where stock lua_getstack itself fails), falling back
**     to the stock body for real Lua frames (load()-compiled closures
**     keep their own source/lines);
**
**   - interposed lua_error computes a "no position" verdict — a raised
**     string that does NOT already start with chunkname':' came from a
**     where() that could not resolve a position (level 0, past the
**     bottom, or a C frame), so rt_run must not bolt the rt line
**     prefix on (stock never adds one in those cases);
**
**   - interposed luaG_runerror clears that verdict: a fresh core
**     raise (tbc registration, 'for' checks, this file's own typeerror
**     family) keeps the rt prefix exactly as before.
**
** Every producer resets the flag for the single consumer (rt_run's
** prefix rule reads it once per staging), so the verdict cannot go
** stale across nested stagings.
**
** The msgh seam (U5e, row 85): interposed luaG_errormsg is the exact
** stock body plus one flag — rt_sea_msgh_ran records that THIS in-flight
** error already passed its message handler at the raise (every C-land
** raise funnels through here, so handlers run for those exactly as
** stock). The flag's consumer is rtw_reraise's handler arm (rt_abi55.c):
** errors that enter flight by DIRECT staging (the depth guard, the
** engine stack trap, rt_error) never pass through errormsg, and the
** trampoline runs the handler for them at the crossing into C-land —
** unless the flag says one already ran (a double run would otherwise
** fire at every cascade's final reraise). Fresh stagings clear the flag
** (the error is new; its handler has not run); stage_error preserves it
** (the value is still the same in-flight error); rt_err_clear clears it.
**
** The U6c write guards (rows 33/50, plan §4.4): the seven table-
** write entry points interposed the same way — guard+delegate, the
** delegates being ltable_stock55.c's renamed second compile of the
** stock table engine. See the block at the file's end.
*/

#ifndef LUA_CORE
#define LUA_CORE
#endif
#include <stdint.h>
#include <stdlib.h> /* malloc/realloc (the hint registry) */
#include <string.h> /* strncmp (the foreign-source verdict) */

#include "rt_abi55.h"
#include "lua55/src/lauxlib.h"
#include "lua55/src/lapi.h"
#include "lua55/src/ldebug.h"
#include "lua55/src/ldo.h"
#include "lua55/src/lobject.h"
#include "lua55/src/lstate.h"
#include "lua55/src/lstring.h"
#include "lua55/src/ltable.h"
#include "lua55/src/lvm.h"

/* ---- the shadow line stack ----
**
** One entry per live dispatch (the trampoline pushes before entering
** the arms and pops after, under its own rawrunprotected — an unwind
** cannot skip the pop). rt_sea_top counts pushes even past RT_SEA_MAX
** (depth is bounded by RTW_MAX_DEPTH=150 < 256, so the clamp is a
** backstop only); a clamped-out entry is simply invisible to lookups.
*/
#define RT_SEA_MAX 256

struct rt_sea_ent {
  CallInfo *ci;      /* the trampoline's CallInfo (&base_ci for main) */
  int32_t *linecell; /* the frame's line cell; NULL until the prologue */
};

static struct rt_sea_ent rt_sea_stack[RT_SEA_MAX];
static int rt_sea_top;
static int rt_sea_nopos_flag;
static int rt_sea_msgh_ran_flag; /* U5e, row 85 — see the accessors */

/* the where-verdict (U5e): 1 when the LAST luaL_where() THIS FILE ran
   could not resolve a position — read once by the lua_error replica
   below. Only lbaselib's error() (and ldblib's error-ish helpers) reach
   the interposed where: lauxlib's own luaL_error/luaL_argerror call
   luaL_where INTRA-object and bind to the stock definition, so library
   raises never touch the verdict and keep the rt line prefix exactly as
   pre-seam (their stock where pushes "" over compiled frames — the
   prefix comes from rt_run's line parameter, matching the interp's own
   raise position). */
static int rt_sea_wheremiss;

void rt_sea_push(CallInfo *ci) {
  if (rt_sea_top < RT_SEA_MAX) {
    rt_sea_stack[rt_sea_top].ci = ci;
    rt_sea_stack[rt_sea_top].linecell = NULL;
  }
  rt_sea_top++;
}

void rt_sea_pop(void) {
  if (rt_sea_top > 0) rt_sea_top--;
}

void rt_sea_reset(void) {
  rt_sea_top = 0;
  rt_sea_nopos_flag = 0;
  rt_sea_wheremiss = 0;
  rt_sea_msgh_ran_flag = 0;
}

/* rt_sea_setline: the wasm function's PROLOGUE binds its line cell
   into the CURRENT dispatch's entry (the M5c restage rebinds the same
   entry to the successor frame's cell) and zeroes the cell — the very
   first instruction's store then brings in the real line. */
void rt_sea_setline(rt_addr cell) {
  int32_t *p = (int32_t *)(size_t)cell;
  if (rt_sea_top >= 1 && rt_sea_top <= RT_SEA_MAX)
    rt_sea_stack[rt_sea_top - 1].linecell = p;
  *p = 0;
}

/* rt_sea_main: the main chunk's bottom entry (lua_main runs cl=0 with
   no CallInfo; keying &base_ci lets the where() walk resolve main
   level, where stock lua_getstack itself fails). Popped by the emitted
   main at both exits via rt_sea_pop. */
void rt_sea_main(rt_addr cell) {
  rt_sea_push(&rt_curL()->base_ci);
  rt_sea_setline(cell);
}

/* rt_sea_line: the current line for a dispatch ci, 0 when unknown. */
int rt_sea_line(CallInfo *ci) {
  int i, n = rt_sea_top > RT_SEA_MAX ? RT_SEA_MAX : rt_sea_top;
  for (i = n - 1; i >= 0; i--)
    if (rt_sea_stack[i].ci == ci && rt_sea_stack[i].linecell != NULL)
      return *rt_sea_stack[i].linecell;
  return 0;
}

int32_t rt_sea_nopos(void) { return rt_sea_nopos_flag; }

void rt_sea_nopos_set(int32_t v) { rt_sea_nopos_flag = (int)v; }

/* U6e (row 96): the hostfn raise mark — rt_hf_closure's errors are
   COMPLETE messages (Redis command texts propagate verbatim; the v3
   rt_where_mark analog). Setting wheremiss (not nopos directly) rides
   the interposed lua_error's own verdict plumbing: the replica copies
   wheremiss into rt_sea_nopos and consumes it, so the catching rt_run's
   staging skips the chunk:line prefix for exactly this one raise. */
void rt_sea_raise_nopos(void) { rt_sea_wheremiss = 1; }

/* the msgh-ran flag (U5e, row 85 — see the file header): 1 while the
   in-flight error has already had its message handler run at a
   C-land raise (the errormsg replica below sets it before dispatching
   the handler). */
int32_t rt_sea_msgh_ran(void) { return rt_sea_msgh_ran_flag; }

void rt_sea_msgh_ran_set(int32_t v) { rt_sea_msgh_ran_flag = (int)v; }

/* rt_sea_getstack: stock lua_getstack's walk (ldebug.c), with one
   extension — landing on base_ci at level 0 succeeds when the main
   chunk's bottom shadow entry is live. The walk itself is identical
   (level counts C frames too), so every non-main resolution matches
   stock bit-for-bit. */
static CallInfo *rt_sea_getstack(lua_State *L, int level) {
  CallInfo *ci;
  if (level < 0) return NULL;
  for (ci = L->ci; level > 0 && ci != &L->base_ci; level--)
    ci = ci->previous;
  if (level == 0 && ci != &L->base_ci)
    return ci; /* stock path */
  if (level == 0 && ci == &L->base_ci && rt_sea_line(&L->base_ci) != 0)
    return &L->base_ci; /* the seam's main-level resolution */
  return NULL;
}

/* ---- interposed luaL_where (lauxlib.c) ----
**
** Shadow resolution first: a compiled frame at the requested level
** with a live line renders chunkname:line (the interp oracle's
** spelling — its Where() walks its own frames the same way). Miss ->
** the exact stock body: real Lua frames keep their own short_src and
** currentline; C frames and misses push "" like stock. */
LUALIB_API void luaL_where(lua_State *L, int level) {
  lua_Debug ar;
  CallInfo *ci = rt_sea_getstack(L, level);
  if (ci != NULL) {
    int line = rt_sea_line(ci);
    if (line > 0) {
      rt_sea_wheremiss = 0; /* this raise carries a position */
      lua_pushfstring(L, "%s:%d: ", rt_chunkname_ptr(), line);
      return;
    }
    /* a shadow-visible ci with no line yet (before the first store):
       fall through — stock would resolve this same ci and, for a
       C-closure frame, push "" */
  }
  if (lua_getstack(L, level, &ar)) { /* check function at level */
    lua_getinfo(L, "Sl", &ar);       /* get info about it */
    if (ar.currentline > 0) {        /* is there info? */
      /* a real Lua frame (load()-compiled: interpreted by the C VM).
         A source matching the rt chunk name re-prefixes nothing —
         stage_tvalue's prefix detection handles it — but a FOREIGN
         source ("[string ...]") already carries its own position: the
         rt must not bolt its line in front of it (the interp never
         does), so the miss verdict applies. */
      int cnlen = rt_chunkname_len();
      rt_sea_wheremiss =
          !(cnlen > 0 && strncmp(ar.short_src, rt_chunkname_ptr(),
                                 (size_t)cnlen) == 0);
      lua_pushfstring(L, "%s:%d: ", ar.short_src, ar.currentline);
      return;
    }
  }
  rt_sea_wheremiss = 1; /* no resolvable position at this level */
  lua_pushfstring(L, ""); /* else, no information available... */
}

/* ---- interposed lua_error (lapi.c) ----
**
** Exact stock body, preceded by the nopos verdict (U5e): rt_sea_wheremiss
** records whether THIS raise's luaL_where() — the seam's, i.e. the one
** lbaselib's error() reaches cross-object — resolved a position. A miss
** means error() was called with a level that lands nowhere (0, or past
** the bottom): stock adds no prefix, so rt_run must not either (its
** prefix rule consults rt_sea_nopos). Library raises (luaL_error and
** friends) run the stock where inside lauxlib.o and leave the verdict
** at its consumed default 0 — the rt line parameter supplies their
** position, exactly as pre-seam. The verdict is one-shot: consumed by
** this raise, reset to 0 for the next. */
LUA_API int lua_error(lua_State *L) {
  TValue *errobj;
  lua_lock(L);
  errobj = s2v(L->top.p - 1);
  api_checkpop(L, 1);
  rt_sea_nopos_set(rt_sea_wheremiss);
  rt_sea_wheremiss = 0; /* consumed */
  if (ttisshrstring(errobj) && eqshrstr(tsvalue(errobj), G(L)->memerrmsg))
    luaM_error(L); /* raise a memory error */
  else
    luaG_errormsg(L); /* raise a regular error */
  /* code unreachable; will unlock when control actually leaves the kernel */
  return 0; /* to avoid warnings */
}

/* ---- interposed luaG_runerror (ldebug.c) ----
**
** Exact stock replica (macro-expanded pushvfstring), plus the verdict
** clear: a fresh core raise keeps the rt prefix (rt_* bodies raise
** through here for tbc/forprep/vararg-close checks). The isLua arm is
** stock's — compiled frames are C closures, skipped here exactly as
** stock skips them; their position comes from the rt line parameter. */
l_noret luaG_runerror(lua_State *L, const char *fmt, ...) {
  CallInfo *ci = L->ci;
  const char *msg;
  va_list argp;
  rt_sea_nopos_set(0); /* core raise: the rt prefix applies */
  luaC_checkGC(L);     /* error message uses memory */
  pushvfstring(L, argp, fmt, msg);
  if (isLua(ci)) { /* Lua function? */
    /* add source:line information */
    luaG_addinfo(L, msg, ci_func(ci)->p->source,
                 luaG_getfuncline(ci_func(ci)->p,
                                  pcRel(ci->u.l.savedpc, ci_func(ci)->p)));
    setobjs2s(L, L->top.p - 2, L->top.p - 1); /* remove 'msg' */
    L->top.p--;
  }
  luaG_errormsg(L);
}

/* ---- interposed luaG_errormsg (ldebug.c) — the msgh seam (U5e, row 85)
**
** The exact stock body plus one flag write: rt_sea_msgh_ran is set
** BEFORE the handler dispatches (a raise inside the handler — including
** one routed back through this replica — must find the flag already
** saying "ran", or rtw_reraise would run the handler again at the
** crossing). Nothing else differs: C-land raises keep stock's
** run-the-handler-at-the-raise semantics bit for bit, including the
** nil-object fix and the LUA_ERRRUN throw the catcher's seterrorobj
** reads at top-1. */
l_noret luaG_errormsg(lua_State *L) {
  if (L->errfunc != 0) { /* is there an error handling function? */
    StkId errfunc = restorestack(L, L->errfunc);
    rt_sea_msgh_ran_flag = 1;
    setobjs2s(L, L->top.p, L->top.p - 1); /* move argument */
    setobjs2s(L, L->top.p - 1, errfunc);  /* push function */
    L->top.p++;
    luaD_callnoyield(L, L->top.p - 2, 1);
  }
  if (ttisnil(s2v(L->top.p - 1)))
    setsvalue2s(L, L->top.p - 1,
                luaS_newliteral(L, "<no error object>"));
  luaD_throw(L, LUA_ERRRUN);
}

/* ---- the varinfo hint registry (U5e, ledger row 71) -------------------
**
** Stock's raises name the faulty operand via varinfo(): getupvalname /
** instack + getobjname over the CURRENT CallInfo — a compiled frame is a
** CClosure frame, so stock answers "" and every wasm error arrived
** hint-less (row 71's ERROR-line strip papered over this in the gates).
** The durable fix: the BACKEND precomputes the interp's answer at compile
** time (the same findsetreg/getobjname symbolic walk, varinfo.go), packs
** the formatted " (local 'x')" strings into this registry at init, and
** passes their indexes as trailing i32 immediates on the raising rt_*
** entries. rt_run funnels every entry: it arms the three slots below
** before running the protected body, so a raise can never read a stale
** hint from an earlier dispatch (a metamethod that re-enters compiled
** code overwrites the slots with its own values — and a post-return
** raise from the OUTER body happens only at paths with no intervening
** dispatch: the __index-not-callable and callbinTM call errors fire
** straight off the lookup, before any nested entry can run).
**
**   sea_h1/sea_h2 — the typeerror family's operand hints (luaG_typeerror
**   reads h1; luaG_opinterror/tointerror pick by the same operand test
**   stock applies to p1);
**   sea_hc        — the call-error hint (luaG_callerror; funcnamefromcall
**   over a C frame is NULL in stock — the backend supplies the call
**   site's funcnamefromcode answer, including the "metamethod 'x'"
**   spelling for metamethod invocations);
**   the concat window stack — luaG_concaterror receives STACK pointers,
**   not the frame cells rt_concat copied in, so rt_concat records where
**   its operand window landed (a savestack offset, row-31-proof) plus
**   the base of its contiguous hint run; the replica maps the faulty
**   slot back to its run index. A __concat metamethod may re-enter
**   rt_concat before the outer luaV_concat raises, so the window is a
**   push/pop stack (rt_run folds the error status, so the pop always
**   runs).
*/

static char **rt_hints;
static int rt_hints_n, rt_hints_cap;

/* rt_hint_put registers one NUL-terminated copy of the formatted hint
   (the rt_name_put pattern; the bytes are malloc'd here so %s consumers
   never see the packed, unterminated init buffer). Index = position. */
int32_t rt_hint_put(rt_addr ptr, int32_t len) {
  char *s;
  if (rt_hints_n == rt_hints_cap) {
    int ncap = rt_hints_cap ? rt_hints_cap * 2 : 16;
    char **nh = (char **)realloc(rt_hints, (size_t)ncap * sizeof *nh);
    if (nh == NULL) return -1;
    rt_hints = nh;
    rt_hints_cap = ncap;
  }
  s = (char *)malloc((size_t)len + 1);
  if (s == NULL) return -1;
  memcpy(s, (const void *)(size_t)ptr, (size_t)len);
  s[len] = '\0';
  rt_hints[rt_hints_n] = s;
  return rt_hints_n++;
}

/* rt_sea_hint: the registered hint text, "" for none (idx < 0, out of
   range, or the registered empty string — the backend registers ""
   placeholders inside concat runs so hbase+idx stays contiguous). */
static const char *rt_sea_hint(int32_t idx) {
  if (idx < 0 || idx >= rt_hints_n) return "";
  return rt_hints[idx];
}

static int32_t sea_h1, sea_h2, sea_hc; /* armed by rt_run (see above) */

/* the typeerror operand snapshot (rt_sea_hint_set's tgtop): the interp
   applies a table-op hint only at chain iteration 0 — when the raised
   object IS the entry's operand — so luaG_typeerror value-compares o
   against this copy before using h1 (a non-table __index chain fails on
   the LINK, which carries no name: "attempt to index a number value"
   bare). */
static TValue sea_tg;
static int sea_tg_active;

void rt_sea_hint_set(int32_t h1, int32_t h2, int32_t hc, const void *tgtop) {
  sea_h1 = h1;
  sea_h2 = h2;
  sea_hc = hc;
  sea_tg_active = (tgtop != NULL);
  if (tgtop != NULL) sea_tg = *(const TValue *)tgtop;
}

/* the concat window stack */
#define RT_SEA_CC 256 /* > RTW_MAX_DEPTH: nesting is dispatch-bounded */
struct rt_sea_cc {
  int32_t hbase;    /* first index of the contiguous run */
  ptrdiff_t winoff; /* savestack offset of operand 0's stack slot */
};
static struct rt_sea_cc rt_sea_ccs[RT_SEA_CC];
static int rt_sea_cc_top;

void rt_sea_concat_push(int32_t hbase, int64_t winoff) {
  if (rt_sea_cc_top < RT_SEA_CC) {
    rt_sea_ccs[rt_sea_cc_top].hbase = hbase;
    rt_sea_ccs[rt_sea_cc_top].winoff = (ptrdiff_t)winoff;
  }
  rt_sea_cc_top++;
}

void rt_sea_concat_pop(void) {
  if (rt_sea_cc_top > 0) rt_sea_cc_top--;
}

/* ---- interposed typeerror family (ldebug.c) ----
**
** Exact stock bodies with varinfo(L, o) replaced by the armed hint: over
** a compiled frame stock's varinfo answers "" (isLua(ci) false), so the
** hint is a strict addition — stock behavior for C frames and
** load()-interpreted chunks is untouched (their raises arm no hint).
** Every replica raises through THIS file's luaG_runerror, keeping the
** core-raise prefix rule. */

static l_noret sea_typeerror(lua_State *L, const TValue *o, const char *op,
                             const char *extra) {
  const char *t = luaT_objtypename(L, o);
  luaG_runerror(L, "attempt to %s a %s value%s", op, t, extra);
}

l_noret luaG_typeerror(lua_State *L, const TValue *o, const char *op) {
  /* h1 applies only when o IS the armed operand (the interp's i==0
     rule — see sea_tg above); arith/bitwise raises go through
     opinterror/tointerror, whose objects are definitionally p1/p2 */
  const char *extra = "";
  if (sea_tg_active && sea_h1 >= 0 && luaV_rawequalobj(o, &sea_tg))
    extra = rt_sea_hint(sea_h1);
  sea_typeerror(L, o, op, extra);
}

l_noret luaG_callerror(lua_State *L, const TValue *o) {
  /* stock: funcnamefromcall names it when the caller is a Lua frame,
     else varinfo(o) — over the seam's C frames both answer nothing, so
     the armed call-site hint (or "") stands in */
  sea_typeerror(L, o, "call", rt_sea_hint(sea_hc));
}

l_noret luaG_opinterror(lua_State *L, const TValue *p1, const TValue *p2,
                        const char *msg) {
  if (!ttisnumber(p1)) /* first operand is wrong? */
    p2 = p1;           /* now second is wrong too */
  sea_typeerror(L, p2, msg, rt_sea_hint(!ttisnumber(p1) ? sea_h1 : sea_h2));
}

l_noret luaG_tointerror(lua_State *L, const TValue *p1, const TValue *p2) {
  lua_Integer temp;
  int bad1 = !luaV_tointegerns(p1, &temp, LUA_FLOORN2I);
  if (bad1)
    p2 = p1;
  luaG_runerror(L, "number%s has no integer representation",
                rt_sea_hint(bad1 ? sea_h1 : sea_h2));
}

l_noret luaG_concaterror(lua_State *L, const TValue *p1, const TValue *p2) {
  if (ttisstring(p1) || cvt2str(p1))
    p1 = p2; /* error is in the 2nd operand */
  if (rt_sea_cc_top >= 1 && rt_sea_cc_top <= RT_SEA_CC) {
    /* map the faulty stack slot back to its run index: the window base
       is the recorded offset (restored against the CURRENT stack — the
       window may have moved under a grow), and remaining operands keep
       their original slots (luaV_concat folds top-down into the LOWER
       slot, never moving the ones below) */
    const struct rt_sea_cc *cc = &rt_sea_ccs[rt_sea_cc_top - 1];
    const TValue *base = s2v(restorestack(L, cc->winoff));
    int idx = (int)((const char *)p1 - (const char *)base) /
              (int)sizeof(TValue);
    if (idx >= 0)
      /* bare call, no return stmt — an l_noret body must not carry one
         (-Winvalid-noreturn); sea_typeerror never returns */
      sea_typeerror(L, p1, "concatenate",
                    rt_sea_hint(cc->hbase + idx));
  }
  sea_typeerror(L, p1, "concatenate", "");
}

/* ---- U6c (rows 33/50, plan §4.4): interposed table-write guards ----
**
** The zero-patch globals lockdown's write half. rt_protect_globals
** (rt_abi55.c) puts every table reachable from the globals table — and
** the shared string metatable — into the rt-owned protected set; these
** seven replicas are the enforcement point: every CROSS-OBJECT path
** into a stock table write funnels through exactly one of them
** (lua_settable/setfield/setglobal/seti → the luaV_fastset* macros →
** luaH_pset/psetstr/psetint; lua_rawset/rawsetp → luaH_set;
** lua_rawseti → luaH_setint; luaV_finishset → luaH_finishset), so a
** write to a protected table raises before any cell moves.
**
** Why luaH_set/finishset are guarded DIRECTLY (and not just the pset
** family): stock ltable.o's own luaH_set body calls luaH_pset
** INTRA-object, which binds to the stock definition — interposition is
** a cross-object mechanism only (same reason lauxlib's luaL_where
** never reaches the where replica). psetshortstr gets a guard for the
** native luaV_execute paths (dead in the blob — no LClosure executes
** there — but the guards are tree-shape, not blob-shape).
**
** The raise goes through THIS file's luaG_runerror replica (the
** proven sea_typeerror pattern): nopos cleared, so the catching rt_run
** bolts the raise site's "chunk:line: " prefix on — the tbc/forprep
** core-raise rule. Replicas with an L parameter raise with it; the
** pset family has none, so they use rt_curL().
**
** The delegates are runtime/ltable_stock55.c — the whole ltable.c
** compiled again under rt_lts_* names, so the guards call the TRUE
** stock bodies (static internals included) without patching anything.
** ltable.h LUA_DEBUG-only luaH_mainposition is not renamed (never
** compiled). */
LUAI_FUNC int rt_lts_pset(Table *t, const TValue *key, TValue *val);
LUAI_FUNC int rt_lts_psetstr(Table *t, TString *key, TValue *val);
LUAI_FUNC int rt_lts_psetshortstr(Table *t, TString *key, TValue *val);
LUAI_FUNC int rt_lts_psetint(Table *t, lua_Integer key, TValue *val);
LUAI_FUNC void rt_lts_setint(lua_State *L, Table *t, lua_Integer key,
                            TValue *value);
LUAI_FUNC void rt_lts_set(lua_State *L, Table *t, const TValue *key,
                         TValue *value);
LUAI_FUNC void rt_lts_finishset(lua_State *L, Table *t, const TValue *key,
                               TValue *value, int hres);

LUAI_FUNC int luaH_pset(Table *t, const TValue *key, TValue *val) {
  if (rt_prot_isprotected(t))
    luaG_runerror(rt_curL(), "Attempt to modify a readonly table");
  return rt_lts_pset(t, key, val);
}

LUAI_FUNC int luaH_psetstr(Table *t, TString *key, TValue *val) {
  if (rt_prot_isprotected(t))
    luaG_runerror(rt_curL(), "Attempt to modify a readonly table");
  return rt_lts_psetstr(t, key, val);
}

LUAI_FUNC int luaH_psetshortstr(Table *t, TString *key, TValue *val) {
  if (rt_prot_isprotected(t))
    luaG_runerror(rt_curL(), "Attempt to modify a readonly table");
  return rt_lts_psetshortstr(t, key, val);
}

LUAI_FUNC int luaH_psetint(Table *t, lua_Integer key, TValue *val) {
  if (rt_prot_isprotected(t))
    luaG_runerror(rt_curL(), "Attempt to modify a readonly table");
  return rt_lts_psetint(t, key, val);
}

LUAI_FUNC void luaH_setint(lua_State *L, Table *t, lua_Integer key,
                          TValue *value) {
  if (rt_prot_isprotected(t))
    luaG_runerror(L, "Attempt to modify a readonly table");
  rt_lts_setint(L, t, key, value);
}

LUAI_FUNC void luaH_set(lua_State *L, Table *t, const TValue *key,
                       TValue *value) {
  if (rt_prot_isprotected(t))
    luaG_runerror(L, "Attempt to modify a readonly table");
  rt_lts_set(L, t, key, value);
}

LUAI_FUNC void luaH_finishset(lua_State *L, Table *t, const TValue *key,
                             TValue *value, int hres) {
  if (rt_prot_isprotected(t))
    luaG_runerror(L, "Attempt to modify a readonly table");
  rt_lts_finishset(L, t, key, value, hres);
}
