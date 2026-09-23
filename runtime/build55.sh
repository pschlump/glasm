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

# Driver-only export surface (the rt_* ABI arrives with the ABI v4 port,
# U2/U6): exactly the eight functions the Go oracle hosts call.
EXPORTS="-Wl,--export=lnewstate -Wl,--export=lclose -Wl,--export=ldostring \
  -Wl,--export=lerrlen -Wl,--export=lerrcopy \
  -Wl,--export=linbuf -Wl,--export=lnamebuf -Wl,--export=lglobals"

# A static (max-bounded) linear memory never moves its base — and is the
# coarse OOM backstop (a memory.grow failure makes dlmalloc return NULL →
# the same clean "not enough memory" pcall path). 256 MiB, as build.sh.
MEMMAX="-Wl,--max-memory=268435456"

"$CC" --sysroot="$SYSROOT" \
  -O2 -DNDEBUG -mexec-model=reactor -I. \
  -D'luai_makeseed()=0u' \
  -D_WASI_EMULATED_SIGNAL \
  -D'lua_tmpnam(b,e)={e=1;}' \
  -DLUA_TMPNAMBUFSIZE=64 \
  -mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false \
  -lsetjmp \
  $EXPORTS $MEMMAX \
  -Wl,-z,stack-size=8388608 -Wl,--strip-all \
  -o lua55_sjlj.wasm luawasm55.c $SRC

cp lua55_sjlj.wasm ../testdiff/lua55_sjlj.wasm

ls -la lua55_sjlj.wasm
exit 0
