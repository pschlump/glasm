#!/bin/sh
# runtime/build55.sh — builds the Lua 5.5.1 oracle module (U0,
# docs/Upgrade-from-5.2-to-5.5-of-Lua.md; decisions D6/D9).
#
# lua55_sjlj.wasm — stock Lua 5.5.1 (runtime/lua55/, zero patches) +
# luawasm55.c glue with native EH-based setjmp/longjmp
# (-mllvm -wasm-enable-sjlj), the same dialect as lua51_sjlj.wasm: runs on
# engines that implement the (standardized) wasm EH proposal — wasmtime
# AND wazero (ledger row 32's one-blob-two-hosts posture). 5.5's LUAI_THROW
# is still setjmp/longjmp (ldo.c), so the SJLJ transform applies unchanged.
#
# Determinism is pinned here (not by patching lua55/):
#   -D'luai_makeseed()=0u'  math.random initial seed, randomseed() no-arg
#                           seed, table.sort pivot randomization (default
#                           is time+address based)
# (the state seed — string hashing AND hash_search's '#' probe ladder — is
# pinned to the fixed non-zero value 42 by lua_newstate in luawasm55.c;
# seed 0 is the adversarial worst case for '#', see the comment there)
#
# 5.5 (unlike 5.1) pulls <signal.h> into lstate.h for sig_atomic_t (debug
# hook mask/flag); WASI needs the emulation define for the types. Lua only
# uses the typedef — no signal symbols are referenced, so the
# -lwasi-emulated-signal archive member is never pulled.
#
# WASI also lacks tmpnam/L_tmpnam. os.tmpname is shimmed in luawasm55.c
# (deterministic names), so stock os_tmpnam is dead code that must merely
# compile: give it a stub macro + a buffer size.
#
# After rebuilding, run `go clean -testcache` — go-test caching can mask a
# stale embedded lua55_sjlj.wasm (same rule as build.sh).
#
# Requires wasi-sdk (WASI_SDK=...).
set -e
cd "$(dirname "$0")"

WASI_SDK=${WASI_SDK:-$HOME/wasi-sdk-dl/wasi-sdk-34.0-arm64-macos}
CC="$WASI_SDK/bin/clang"
SYSROOT="$WASI_SDK/share/wasi-sysroot"

SRC=$(ls lua55/src/*.c)

# Driver-only export surface for the oracle blob (the rt_* ABI is NOT in
# it): exactly the eight functions the Go oracle hosts call.
EXPORTS="-Wl,--export=lnewstate -Wl,--export=lclose -Wl,--export=ldostring \
  -Wl,--export=lerrlen -Wl,--export=lerrcopy \
  -Wl,--export=linbuf -Wl,--export=lnamebuf -Wl,--export=lglobals"

# A static (max-bounded) linear memory never moves its base — and is the
# coarse OOM backstop (a memory.grow failure makes dlmalloc return NULL →
# the same clean "not enough memory" pcall path). 256 MiB, as build.sh.
MEMMAX="-Wl,--max-memory=268435456"

# Function-like -D macros must stay on the command line (their quotes
# are literal inside a double-quoted variable).
CFLAGS="-O2 -DNDEBUG -mexec-model=reactor -I. \
  -D_WASI_EMULATED_SIGNAL \
  -DLUA_TMPNAMBUFSIZE=64 \
  -mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false \
  -lsetjmp \
  $MEMMAX \
  -Wl,-z,stack-size=8388608 -Wl,--strip-all"

# ---- lua55_sjlj.wasm: the oracle blob (driver only) ----

"$CC" --sysroot="$SYSROOT" \
  $CFLAGS \
  -D'luai_makeseed()=0u' \
  -D'lua_tmpnam(b,e)={e=1;}' \
  $EXPORTS \
  -o lua55_sjlj.wasm luawasm55.c $SRC

cp lua55_sjlj.wasm ../testdiff/lua55_sjlj.wasm

# ---- lua55_rt.wasm: ABI v4 flavor (U2d) — same tree, same driver, plus
# rt_abi55.c: the stock 5.5 runtime with the frozen rt_* ABI for the
# U2e wasm backend. The blob keeps the zero-patch discipline: compiled
# protos register as CClosures over the host.wasm_dispatch trampoline
# (no ldo.c precall hook), cells are native 5.5 TValues (integer/float
# subtypes cross losslessly), errors carry stock 5.5 texts. Tranche 1
# scope (docs/U2-implementation-plan.md §4 U2d): arith/compare/concat/
# tables/globals/upvalues/calls/forprep + the seam; sandbox/memlimit/
# deadline/hostfn are U6. Determinism: same pins as the oracle blob.
# Not consumed by any Go host yet (U2e wires the backend); build-gated
# here + the native ABI tests (tests/rt_native55_test.c).

RT_EXPORTS="$EXPORTS \
  -Wl,--export=rt_abi_version -Wl,--export=rt_set_state \
  -Wl,--export=rt_mknumber -Wl,--export=rt_mkinteger \
  -Wl,--export=rt_mkbool -Wl,--export=rt_mknil -Wl,--export=rt_intern \
  -Wl,--export=rt_newtable -Wl,--export=rt_gettable -Wl,--export=rt_settable \
  -Wl,--export=rt_arith -Wl,--export=rt_len -Wl,--export=rt_eq \
  -Wl,--export=rt_lt -Wl,--export=rt_le -Wl,--export=rt_concat \
  -Wl,--export=rt_call -Wl,--export=rt_call_addr \
  -Wl,--export=rt_call_count -Wl,--export=rt_forprep \
  -Wl,--export=rt_frame_alloc -Wl,--export=rt_error \
  -Wl,--export=rt_getglobal -Wl,--export=rt_setglobal \
  -Wl,--export=rt_set_chunkname -Wl,--export=rt_chunkname_ptr \
  -Wl,--export=rt_chunkname_len \
  -Wl,--export=rt_err_pending -Wl,--export=rt_err_clear \
  -Wl,--export=rt_err_stage_copy -Wl,--export=rt_err_line \
  -Wl,--export=rt_err_value_ptr -Wl,--export=rt_err_stage_value \
  -Wl,--export=rt_wasm_proto -Wl,--export=rt_wasm_upval \
  -Wl,--export=rt_wasm_count -Wl,--export=rt_newclosure \
  -Wl,--export=rt_getupval -Wl,--export=rt_setupval \
  -Wl,--export=rt_close_upvals -Wl,--export=rt_clidx \
  -Wl,--export=rt_varargtab -Wl,--export=rt_getvarg \
  -Wl,--export=rt_errnnil -Wl,--export=rt_vararg_read \
  -Wl,--export=rt_sea_setline -Wl,--export=rt_sea_main \
  -Wl,--export=rt_sea_pop \
  -Wl,--export=rt_name_put -Wl,--export=rt_hint_put -Wl,--export=rt_tbc \
  -Wl,--export=rt_close -Wl,--export=rt_close_err \
  -Wl,--export=rt_wasm_push_frame -Wl,--export=rt_frame_cursor \
  -Wl,--export=rt_frame_restore \
  -Wl,--export=rt_set_memlimit -Wl,--export=rt_mem_used_bytes \
  -Wl,--export=rt_ctrl_addr -Wl,--export=rt_set_deadline \
  -Wl,--export=rt_deadline_flag -Wl,--export=rt_deadline \
  -Wl,--export=rt_tail_stage -Wl,--export=rt_tail_clidx \
  -Wl,--export=rt_tail_nargs -Wl,--export=rt_tail_funcell \
  -Wl,--export=rt_tail_restage"

# U5e (row 60): the error-position seam — rt_seam55.c interposes
# luaL_where/lua_error/luaG_runerror for the RT blob only. It must link
# BEFORE the stock objects and the whole link needs --allow-multiple-
# definition: GNU ld resolves a duplicated symbol to the FIRST
# definition on the command line, so the seam's replicas win and the
# stock tree stays untouched. No -flto in CFLAGS, so object-level
# interposition holds (an LTO link would fold the duplicates first).
"$CC" --sysroot="$SYSROOT" \
  $CFLAGS \
  -D'luai_makeseed()=0u' \
  -D'lua_tmpnam(b,e)={e=1;}' \
  $RT_EXPORTS \
  -Wl,--allow-multiple-definition \
  -DLUA_RT_FLAVOR \
  -o lua55_rt.wasm luawasm55.c rt_abi55.c rt_seam55.c $SRC

cp lua55_rt.wasm ../testdiff/lua55_rt.wasm

ls -la lua55_sjlj.wasm lua55_rt.wasm
exit 0
