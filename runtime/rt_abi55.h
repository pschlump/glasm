/*
** runtime/rt_abi55.h — ABI v4: the frozen rt_* ABI on the stock Lua 5.5.1
** tree (runtime/lua55/, zero patches — U2d,
** docs/U2-implementation-plan.md §4; docs/Upgrade-from-5.2-to-5.5-of-Lua.md).
**
** Same architecture as ABI v3 (rt_abi.h): one linear memory shared with the
** script module; downward rt_* calls only; every rt_* returns a status and
** errors are staged (rt_err_*) rather than longjmp'd across the boundary.
** What v4 changes:
**
**   - cells are native 5.5 TValues with variant tags — LUA_VNUMINT and
**     LUA_VNUMFLT are distinct, integers cross losslessly (THE reason the
**     port exists: a 5.1-based blob round-trips every value through f64);
**   - arith ops are the LUA_OP* numbers (lua.h, 0..13) — the same numbers
**     the 5.5 encoding's MMBIN C operand space uses — and the engine is
**     5.5's own luaO_arith/luaT_trybinTM family: stock semantics and stock
**     error texts with no dialect layer (gnumfmt is not linked);
**   - the precall seam keeps the zero-patch discipline: compiled protos
**     register as CClosures over the host.wasm_dispatch trampoline
**     (rt_wasm_trampoline, wasm idx + closure record in upvalue 0) instead
**     of v3's patched-ldo.c precall_wasm adapter;
**   - staging is cleared at the re-raise (the trampoline moves the staged
**     value onto the Lua stack and clears) — the zero-patch replacement for
**     v3's rt_pcall_caught hook in luaD_pcall.
**
** Not in tranche 1 (U6, plan §4 U2d): rt_sandbox / globals lockdown, the
** budget alloc + deadline block, the host-fn seam, the error()-position
** line-stack. LUA_RT_ABI bumps on ANY signature/semantic change; the
** backend refuses to emit against a mismatched module.
*/

#ifndef RT_ABI55_H
#define RT_ABI55_H

#include "lua55/src/lua.h"

#define LUA_RT_ABI 4 /* v4 (U2d): 5.5 tree, variant-tag cells, CClosure seam */

/* rt_addr: the pointer-carrying parameter type — i32 on wasm32 (this IS
   the frozen ABI). The native test build (RT_ABI_NATIVE64) compiles the
   same logic with pointer-width addressing to validate semantics. */
#ifdef RT_ABI_NATIVE64
typedef long long rt_addr;
#else
typedef int32_t rt_addr;
#endif

/* statuses (i32 return of every rt_*) */
#define RT_OK 0
#define RT_ERR 1 /* error staged; see rt_err_len/rt_err_copy/rt_err_clear */

/* rt_forprep's tristate: 0 = enter the loop, RT_ERR = staged error, and */
#define RT_FORPREP_SKIP 2 /* prep decided the body never runs */

/*
** Frozen cell tags: the raw tt_ byte of a 5.5 TValue (novariant(t) is the
** type, the high nibble is the variant, BIT_ISCOLLECTABLE is 0x40 —
** lobject.h). Static asserts in rt_abi55.c pin these against the vendored
** headers; the U2e backend reads cells with the same numbers.
*/
#define RT_VNIL 0           /* LUA_VNIL */
#define RT_VEMPTY 16        /* LUA_VEMPTY (internal) */
#define RT_VABSTKEY 32      /* LUA_VABSTKEY (internal) */
#define RT_VFALSE 1         /* LUA_VFALSE (boolean, variant 0) */
#define RT_VTRUE 17         /* LUA_VTRUE  (boolean, variant 1) */
#define RT_VNUMINT 3        /* LUA_VNUMINT */
#define RT_VNUMFLT 19       /* LUA_VNUMFLT */
#define RT_VSHRSTR 4        /* LUA_VSHRSTR  (+0x40 collectable) */
#define RT_VLNGSTR 20       /* LUA_VLNGSTR (+0x40 collectable) */
#define RT_VLIGHTUSERDATA 2 /* LUA_VLIGHTUSERDATA */
#define RT_VTABLE 5         /* LUA_VTABLE  (+0x40) */
#define RT_VLCL 6           /* LUA_VLCL — Lua closure (+0x40) */
#define RT_VLCF 22          /* LUA_VLCF — light C function */
#define RT_VCCL 38          /* LUA_VCCL — C closure (+0x40) */
#define RT_VUSERDATA 7      /* LUA_VUSERDATA (+0x40) */
#define RT_VTHREAD 8        /* LUA_VTHREAD (+0x40) */
#define RT_ISCOLLECTABLE 0x40

/*
** Arith op codes: identical to lua.h's LUA_OPADD..LUA_OPBNOT (0..13) —
** "ORDER TM, ORDER OP": op maps to metamethod TM_ADD + op, the same
** arithmetic lobject.c's luaO_arith dispatches and the 5.5 encoding's
** MMBIN slots encode. Includes the 5.3 bitwise family v3 lacked.
*/
#define RT_OP_ADD 0
#define RT_OP_SUB 1
#define RT_OP_MUL 2
#define RT_OP_MOD 3
#define RT_OP_POW 4
#define RT_OP_DIV 5
#define RT_OP_IDIV 6
#define RT_OP_BAND 7
#define RT_OP_BOR 8
#define RT_OP_BXOR 9
#define RT_OP_SHL 10
#define RT_OP_SHR 11
#define RT_OP_UNM 12
#define RT_OP_BNOT 13

/*
** The wasm-dispatch seam (v3's rt_wasm.h, re-derived on the CClosure
** trampoline — no ldo.c patch). The script module shares this module's
** linear memory but is a separate instance, so dispatch goes through the
** HOST: wasm_dispatch_host(idx, frame, cl, nargs, want), an import the Go
** host implements by calling the script module's exported lua_dispatch.
**
** `cl` is the address of the rt-owned closure record (rt_wcl in
** rt_abi55.c — the upvalue registry the emitted code passes to
** rt_getupval/rt_setupval), NOT a Lua Closure: v3 passed the LClosure,
** but a zero-patch 5.5 runtime cannot hang a wasm identity off an
** LClosure's Proto, so compiled functions are CClosures whose upvalue 0
** is a light userdata to the record.
**
** Return convention (unchanged from v3):
**   n >= 0 : n results staged at frame+0..16n
**   -1     : error; the exact error TValue staged in the runtime
**            (rt_err_value_ptr) — the trampoline re-raises it
**   -2     : tailcall sentinel (M5c): rt_tail_* staging holds the next
**            frame; the trampoline restages at the same adapter level
**   -3     : host refused (no script module registered)
*/
#define RTW_ERR (-1)
#define RTW_TAIL (-2)
#define RTW_REFUSED (-3)
/* U5e (row 20 family): the host's wasm_dispatch returns this when the
   engine trapped the nested call with "call stack exhausted" — chunk
   frame sizes vary, so the trap can fire below RTW_MAX_DEPTH. The
   trampoline stages the same bare "stack overflow" the depth guard
   stages (the catching rt_run adds the position). */
#define RTW_STACKTRAP (-4)

/* Lua-level wasm frame guard: bounds pure wasm-frame nesting (direct
   dispatch chains). Calls interleaved with C (rt_call through Lua) also
   consume Lua's own nCcalls counter (LUAI_MAXCCALLS=200, ~2 units per
   rt_call level), so THOSE hit 5.5's "C stack overflow" before 150 —
   both are catchable errors; the depth divergence stays ledgered (v3
   posture, re-pinned for the CClosure seam — ledger row 60). */
#define RTW_MAX_DEPTH 150

#ifdef __wasm__
__attribute__((import_module("host"), import_name("wasm_dispatch")))
#endif
int32_t wasm_dispatch_host(int32_t idx, rt_addr frame, rt_addr cl,
                           int32_t nargs, int32_t want);

/* ---- exported ABI v4 surface (rt_abi55.c) ---- */

int32_t rt_abi_version(void);
void rt_set_state(rt_addr p);

/* value construction (cells are 16-byte 5.5 TValues) */
void rt_mknumber(rt_addr cell, double v); /* LUA_VNUMFLT */
void rt_mkinteger(rt_addr cell, int64_t v); /* LUA_VNUMINT (new in v4) */
void rt_mkbool(rt_addr cell, int32_t b);
void rt_mknil(rt_addr cell);
int32_t rt_intern(rt_addr cell, rt_addr ptr, int32_t len);
int32_t rt_newtable(rt_addr cell, int32_t narr, int32_t nrec);

/* tables, arith, length, compares, concat — all metamethod-complete and
   protected (an error stages and returns RT_ERR).
   U5e (row 71): the raising entries carry trailing varinfo-hint indexes
   (rt_hint_put's positions; -1 = none) — the backend's precomputed
   interp answers for the typeerror/callerror/concaterror hints stock's
   varinfo cannot produce over CClosure frames. Additive trailing i32s:
   LUA_RT_ABI deliberately stays 4 (the blob and the backend evolve
   together in-tree; there is no external producer of either side). */
int32_t rt_gettable(rt_addr tblcell, rt_addr keycell, rt_addr dstcell,
                    int32_t line, int32_t hobj, int32_t hcall);
int32_t rt_settable(rt_addr tblcell, rt_addr keycell, rt_addr valcell,
                    int32_t line, int32_t hobj, int32_t hcall);
int32_t rt_arith(int32_t op, rt_addr lhscell, rt_addr rhscell, rt_addr dstcell,
                 int32_t line, int32_t h1, int32_t h2, int32_t hcall);
int32_t rt_len(rt_addr cell, rt_addr dstcell, int32_t line, int32_t hobj,
               int32_t hcall);
int32_t rt_eq(rt_addr a, rt_addr b, rt_addr dst, int32_t line);
int32_t rt_lt(rt_addr a, rt_addr b, rt_addr dst, int32_t line);
int32_t rt_le(rt_addr a, rt_addr b, rt_addr dst, int32_t line);
int32_t rt_concat(rt_addr cells, int32_t count, rt_addr dstcell, int32_t line,
                  int32_t hbase, int32_t hcall);

/* calls: the universal fallback for dynamic callees. Results overwrite
   the arg cells; want<0 means multret and the negative result count is
   returned (rt_call_count decodes). hcallee = the call-error hint
   (funcnamefromcode's answer for the call site; U5e, row 71). */
int32_t rt_call(rt_addr funcell, rt_addr argcells, int32_t nargs, int32_t want,
                int32_t line, int32_t hcallee);
int32_t rt_call_count(int32_t encoded);

/* numeric-for preparation on the three control cells (init, limit, step),
   rewritten stock-5.5 style: integer loops become (count, step, init),
   float loops (limit, step, init) with all three forced float. Returns
   RT_OK = enter, RT_FORPREP_SKIP = body never runs, RT_ERR = staged. */
int32_t rt_forprep(rt_addr cells, int32_t line);

/* scratch frames (v1 malloc; the arena lifecycle is unchanged) */
int32_t rt_frame_alloc(int32_t bytes);

/* raising from emitted code (message + the chunk:line position prefix) */
void rt_error(rt_addr msgptr, int32_t msglen, int32_t line);

/* globals */
int32_t rt_getglobal(rt_addr dstcell, rt_addr keycell, int32_t line);
int32_t rt_setglobal(rt_addr keycell, rt_addr valcell, int32_t line);
void rt_set_chunkname(rt_addr ptr, int32_t len);
const char *rt_chunkname_ptr(void);
int32_t rt_chunkname_len(void);

/* staged-error protocol */
int32_t rt_err_pending(void);
void rt_err_clear(void);
int32_t rt_err_stage_copy(rt_addr dst, int32_t cap);
int32_t rt_err_line(void);
rt_addr rt_err_value_ptr(void);
int32_t rt_err_stage_value(rt_addr dst, int32_t cap);

/* the wasm-proto registry + closures (CClosure seam) */
int32_t rt_wasm_proto(int32_t idx, int32_t numparams, int32_t isvararg,
                      int32_t nupvalues, int32_t framecells);
void rt_wasm_upval(int32_t protoidx, int32_t uvidx, int32_t instack,
                   int32_t idx);
int32_t rt_wasm_count(void);
int32_t rt_newclosure(rt_addr dstcell, int32_t protoidx, rt_addr parentcl,
                      rt_addr frameaddr, int32_t line);
void rt_getupval(rt_addr cl, int32_t idx, rt_addr cell);
void rt_setupval(rt_addr cl, int32_t idx, rt_addr cell);
void rt_close_upvals(rt_addr level);
int32_t rt_clidx(rt_addr funcell);

/* U5e (row 85): the 5.5 vararg / global-decl entries. rt_varargtab is
   the table model's OP_VARARGPREP (stock createvarargtab): builds
   {1..n, n=n-INTEGER} from the extras at src into dst, under rt_run.
   rt_getvarg is OP_GETVARG (stock luaT_getvararg, hidden model) —
   never raises, plain like rt_getupval: an integral key 1..nextra
   reads the extra at vabase, "n" yields nextra, else nil.
   rt_errnnil is OP_ERRNNIL (stock luaG_errnnil's raise): errors when
   cell is non-nil, naming it from namecell (0 => "?"), under rt_run. */
int32_t rt_varargtab(rt_addr dst, rt_addr src, int32_t n, int32_t line);
void rt_getvarg(rt_addr dst, rt_addr key, rt_addr vabase, int32_t nextra);
int32_t rt_errnnil(rt_addr cell, rt_addr namecell, int32_t line);
/* OP_VARARG's k=1 arm (the table model — the interp spec, _vm.go): dst
   cells receive min(nwant, t.n) raw table reads, nil-padded to nwant;
   the count is t.n as an exact integer 0..INT_MAX/2 (else the raise).
   nwant < 0 = multret (all n, count returned); dstcap bounds the write
   to the fixed register window. Returns the resolved count or RT_ERR. */
int32_t rt_vararg_read(rt_addr dst, rt_addr tabcell, int32_t nwant,
                       int32_t dstcap, int32_t line);

/* to-be-closed variables (U4f) — the rt-owned registry keyed by frame-cell
   address (stock threads L->tbclist through the Lua stack slots; compiled
   frames' registers live in the wasm frame region instead). rt_name_put
   registers one variable-name blob at init (index = position, embedded in
   the OP_TBC lowering); rt_tbc registers a cell (false/nil exempt, __close
   checked — the non-closable raise names the variable from that table);
   rt_close is the OP_CLOSE/return close (upvalues first, then 1-arg
   __close, a failure staged as RT_ERR like every rt entry). rt_close_err
   and rt_tbc_count are C-internal surfaces (the trampoline's unwind + the
   native ABI tests) — NOT wasm imports. */
int32_t rt_name_put(rt_addr ptr, int32_t len);
int32_t rt_tbc(rt_addr celladdr, int32_t nameidx, int32_t line);
int32_t rt_close(rt_addr level);
void rt_close_err(rt_addr level);
int32_t rt_tbc_count(void);

/* frame stack (chunked bump region; args copied at push) */
rt_addr rt_wasm_push_frame(void *L, void *base, int mdidx, int nargs);
rt_addr rt_frame_cursor(void);
void rt_frame_restore(rt_addr saved);

/* M5c tailcall staging: stage a descriptor, return the callee's clidx
   (>=0) with the tailcalling frame's cursor restored, or -1 to decline
   (the emitted code then falls back to rt_call) */
int32_t rt_tail_stage(rt_addr funcell, rt_addr argcells, int32_t nargs,
                      rt_addr frame);
int32_t rt_tail_clidx(void);
int32_t rt_tail_nargs(void);
rt_addr rt_tail_funcell(void);
rt_addr rt_tail_restage(void);

/* the row-60 error-position seam (U5e; runtime/rt_seam55.c — linked only
   into lua55_rt.wasm, ahead of the stock objects, so its luaL_where/
   lua_error/luaG_runerror replicas win the link). The shadow line stack:
   every dispatch trampoline pushes an entry keyed by its CallInfo, the
   wasm function's prologue binds its line cell (frame+16*(nregs+4), the
   spare inside frameCells) via rt_sea_setline, and each emitted
   instruction stores its source line there first — the seam's luaL_where
   reads it instead of stock's C-frame-blind getinfo. rt_sea_main plants
   the main chunk's bottom entry (keyed &base_ci — lua_main runs cl=0
   with no CallInfo, so stock lua_getstack fails at main level); the
   emitted main pops it at both exits. rt_sea_nopos is the interposed
   lua_error's verdict consulted by rt_run's prefix rule (a bare string
   raised without a resolvable position gets no rt prefix). rt_curL
   hands rt_seam55.c the static running state. */
void rt_sea_push(struct CallInfo *ci);
void rt_sea_pop(void);
void rt_sea_setline(rt_addr linecell);
void rt_sea_main(rt_addr linecell);
void rt_sea_reset(void);
int rt_sea_line(struct CallInfo *ci);
int32_t rt_sea_nopos(void);
void rt_sea_nopos_set(int32_t v);
/* the msgh seam (U5e, row 85): the interposed luaG_errormsg sets this
   when it dispatched an in-flight error's handler at a C-land raise;
   rtw_reraise's handler arm reads it to avoid a double run (errors that
   entered flight by direct staging never passed through errormsg — the
   arm runs their handler at the crossing into C-land, guarded on the
   rp shadow so exactly one reraise in a cascade does). */
int32_t rt_sea_msgh_ran(void);
void rt_sea_msgh_ran_set(int32_t v);
struct lua_State;
struct lua_State *rt_curL(void);

/* the varinfo hint seam (U5e, row 71 — same replica family as above).
   rt_hint_put registers one formatted " (local 'x')"-class hint at init
   (rt_name_put's pattern; index = position, embedded in the emitted
   code). rt_sea_hint_set arms the three raise slots — rt_run funnels it
   for every entry, so a raise can never read a stale hint. tgtop is the
   entry's typeerror operand (a TValue copy taken here; NULL when the
   entry arms none): the interp applies a table-op hint only when the
   RAISED object is the original operand (chain iteration 0 — _state.go
   getField/setField's `i == 0` guard), so the seam's luaG_typeerror
   replica value-compares o against the snapshot before using h1. The
   concat window stack maps luaG_concaterror's stack pointers back to a
   run index: rt_concat pushes (hbase, savestack offset of operand 0)
   before its protected body and pops after (rt_run folds the error
   status, so the pop always runs; a __concat metamethod may nest
   rt_concat). */
int32_t rt_hint_put(rt_addr ptr, int32_t len);
void rt_sea_hint_set(int32_t h1, int32_t h2, int32_t hc, const void *tgtop);
void rt_sea_concat_push(int32_t hbase, int64_t winoff);
void rt_sea_concat_pop(void);

#endif /* RT_ABI55_H */
