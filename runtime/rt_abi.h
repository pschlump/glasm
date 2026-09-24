/*
** runtime/rt_abi.h — the frozen ABI between the Lua→wasm backend's
** emitted code and the C runtime (design doc §4.4, §5; M3).
**
** Architecture (proven by wasm/sharedmem_test.go):
**   - ONE linear memory: the script module imports the runtime module's
**     exported memory. All pointers below are i32 addresses in that
**     shared space; TValues written by either side are visible to both.
**   - Downward calls only: emitted code calls rt_*. The runtime NEVER
**     calls into script code. When the runtime must run Lua (function
**     metamethods, sort comparators, pcall of compiled functions) it
**     interprets the proto through its own lvm copy — script protos are
**     real Proto structs in shared memory. Correctness therefore never
**     depends on the compiled path; compiled code is pure acceleration.
**   - Values: a "cell" is a real Lua TValue (lobject.h). On wasm32
**     sizeof(TValue)==16 (8-byte union + tt + padding) — enforced by
**     static asserts in rt_abi.c. The tag constants below are frozen
**     (they are Lua 5.1's; collectable values carry BIT_ISCOLLECTABLE).
**
** Error protocol (no setjmp crosses the ABI): every rt_* returns a
** status. RT_ERR means an error value is staged (rt_err_copy) and the
** emitted code must early-return RT_ERR up its own frame chain; pcall
** boundaries call rt_err_clear.
**
** LUA_RT_ABI is versioned; bump on ANY change to signatures, semantics,
** or frozen constants. The backend refuses to emit against a mismatched
** runtime module.
*/

#ifndef RT_ABI_H
#define RT_ABI_H

#include "lua51/src/lua.h"

#define LUA_RT_ABI 3 /* v3 (M5a): wasm dispatch/closures/upvalue registry/frame stack */

/* rt_addr: the pointer-carrying parameter type. On wasm32 it is i32 —
   this IS the frozen ABI. The native test build (RT_ABI_NATIVE64)
   compiles the same logic with pointer-width addressing to validate
   semantics on a 64-bit host. */
#ifdef RT_ABI_NATIVE64
typedef long long rt_addr;
#else
typedef int32_t rt_addr;
#endif

/* statuses (i32 return of every rt_*) */
#define RT_OK 0
#define RT_ERR 1 /* error staged; see rt_err_len/rt_err_copy/rt_err_clear */

/* frozen Lua 5.1 type tags as stored in TValue.tt (lobject.h). Lua 5.1
   stores RAW type numbers — no collectable bit (that is 5.2+;
   collectability is tt >= LUA_TSTRING). Booleans carry the truth in
   value.b (the low 4 bytes at offset 0). */
#define RT_TNIL 0
#define RT_TBOOL 1
#define RT_TNUMBER 3
#define RT_TSTRING 4
#define RT_TTABLE 5
#define RT_TFUNCTION 6
#define RT_TUSERDATA 7
#define RT_TTHREAD 8

/* arith op codes: identical to the Lua 5.1 / golua55 opcode numbers
** the backend compiles from (OP_ADD=15 .. OP_POW=20) */
#define RT_OP_ADD 15
#define RT_OP_SUB 16
#define RT_OP_MUL 17
#define RT_OP_DIV 18
#define RT_OP_MOD 19
#define RT_OP_POW 20
#define RT_OP_UNM 21

#endif /* RT_ABI_H */
