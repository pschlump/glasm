/*
** runtime/rt_wasm.h — ABI v3: the wasm-dispatch seam shared by ldo.c's
** precall adapter and rt_abi.c (note/m5-to-m6-detailed-plan.md §4–§5).
**
** Architecture: a closure whose Proto carries wasm_idx >= 0 is a compiled
** Lua→wasm function. The script module shares this module's linear memory
** but lives in a separate instance, so the runtime dispatches through the
** HOST: wasm_dispatch_host(idx, frame, cl, nargs, want) — an import the Go
** host implements by calling the script module's exported lua_dispatch.
**
** Include AFTER ldo.h/lobject.h/lstate.h (prototypes use Lua types).
*/

#ifndef RT_WASM_H
#define RT_WASM_H

#include <stddef.h>
#include <stdint.h>
#include "rt_abi.h"

/* dispatch status convention (returned by wasm_dispatch_host /
** lua_dispatch, and by protoFn bodies):
**   n >= 0 : n results staged at frame+0..16n
**   -1     : error; the exact error TValue is staged in the runtime
**            (rt_err_value_ptr) — the adapter re-raises it
**   -2     : tailcall sentinel (M5c; nothing produces it yet)
**   -3     : host refused (no script module registered — oracle/stub hosts)
*/
#define RTW_ERR    (-1)
#define RTW_TAIL   (-2)
#define RTW_REFUSED (-3)

/* Lua-level wasm frame guard. The interpreter harness allows 1024 frames;
** LUAI_MAXCCALLS=200 caps C nesting before it — raise the interpreter's
** "stack overflow" (state.go:1141) before either (plan §3.5; depth
** divergence is ledgered). */
#define RTW_MAX_DEPTH 150

/* the host import (defined by the Go host; declared here so both ldo.c
** and rt_abi.c see the same signature). On the native build there is no
** host: the test harness defines a stub. */
#ifdef __wasm__
__attribute__((import_module("host"), import_name("wasm_dispatch")))
#endif
int32_t wasm_dispatch_host(int32_t idx, rt_addr frame, rt_addr cl,
                           int32_t nargs, int32_t want);

/* ---- exported ABI v3 surface (rt_abi.c) ---- */

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
int32_t rt_compat_arg(rt_addr dstcell, rt_addr varargbase, int32_t nvarargs);
int32_t rt_clidx(rt_addr funcell);
rt_addr rt_err_value_ptr(void);
int32_t rt_err_stage_value(rt_addr dst, int32_t cap);

/* ---- M5c: tailcall staging ---- */

/* Stage a wasm-closure tailcall: returns the callee's clidx (>=0) with
   the descriptor staged and the tailcalling frame's cursor restored, or
   -1 to decline (C function / __call'd object / unregistered proto) —
   the emitted code then falls back to rt_call. */
int32_t rt_tail_stage(rt_addr funcell, rt_addr argcells, int32_t nargs,
                      rt_addr frame);
/* staging readback + the restage (fresh frame push with the params/
   vararg split applied to the staged args) */
int32_t rt_tail_clidx(void);
int32_t rt_tail_nargs(void);
rt_addr rt_tail_funcell(void);
rt_addr rt_tail_restage(void);

/* ---- M5d: golua55 message dialect (internal helpers) ---- */

void rt_set_dialect(int32_t d); /* exported to the host: 1 = gopher texts */
int rt_gopher_dialect(void);
void rt_where_mark(void);    /* a raise already decided the position */
const char *rt_gtypename_safe(const TValue *v); /* gopher type name */
const char *rt_chunkname_ptr(void); /* current chunk name (where prefix) */
int rt_line_depth(void);     /* activation line-stack (per live rt_run) */
int rt_line_at(int32_t from_top); /* 0 = top */
void rt_gindex_error(lua_State *L, const TValue *t, const TValue *k);
int rt_wasm_ci(lua_State *L); /* current CallInfo is a wasm frame */
int rt_wasm_ciframe(CallInfo *ci);

/* ---- M6d: memory cap (D3) + deadline (D4) — rt_abi.c ---- */

/* the budget-tracking lua_Alloc (lnewstate builds states with
   lua_newstate(rt_alloc, NULL) so every Lua-object byte funnels through
   the cap). See the rt_set_memlimit block in rt_abi.c. */
void *rt_alloc(void *ud, void *ptr, size_t osize, size_t nsize);

/* init-time per-VM budget in bytes (0 = unlimited); call BEFORE
   lnewstate so the whole VM lifetime counts. Resets the used counter. */
void rt_set_memlimit(int32_t bytes);
int32_t rt_mem_used_bytes(void); /* diagnostics/tests */

/* deadline control block: the address hosts write 1 to when the run's
   deadline expires (watchdog: a single 4-byte LE store — engines cannot
   take store-API calls off-thread; main thread: rt_set_deadline). The
   backend emits a poll against it at every basic-block transition; a set
   flag makes emitted code call rt_deadline → ordinary pcall-catchable
   error. Sticky for the run. */
rt_addr rt_ctrl_addr(void); /* i32 on wasm32 (rt_addr: the ABI's
                               pointer-carrying type) */
void rt_set_deadline(int32_t on);
int32_t rt_deadline_flag(void);
int32_t rt_deadline(void); /* stages "context deadline exceeded", RT_ERR */

/* ---- internal helpers (rt_abi.c; called from ldo.c's adapter) ---- */

int32_t rt_wasm_enter(int32_t idx); /* 0 = ok; 1 = over RTW_MAX_DEPTH,
                                       "stack overflow" staged with a
                                       string value */
void rt_wasm_leave(void);
void rt_pcall_caught(void); /* luaD_pcall recovery hook: consume the
                               rt-staged error (sticky-staging fix) */

/* frame stack: a chunked bump region in shared memory. push computes the
   dynamic size 16*(framecells + 1 + nvarargs) and copies the args
   (params → frame+0.., varargs → frame+16*framecells — plan §3.2). */
rt_addr rt_wasm_push_frame(lua_State *L, StkId base, int mdidx, int nargs);
rt_addr rt_frame_cursor(void);
void rt_frame_restore(rt_addr saved);

#endif /* RT_WASM_H */
