#!/bin/sh
# build-embedbench55.sh — build the NATIVE C embedding-API harness for
# cmd/embedcmp (docs/Embedded-EVAL-Perf.md): runtime/embedbench55.c, a
# custom main that drives stock C Lua 5.5.1 through its C API the way an
# application embeds it (one state, chunk loaded once, staged pcalls).
#
# Sibling of build-native55.sh, NOT an extension: the embedbench main is
# in-repo, so this build needs no tarball lua.c (the vendored
# runtime/lua55 tree ships none — VENDORED.md), and build-native55.sh's
# documented contract (docs/Perf-vs-C-Lua.md "The comparison target")
# stays untouched. Flags match that build exactly so both native
# binaries share the -O2 LUA_USE_MACOSX comparison posture (readline is
# inert here — it lives only in lua.c — kept for flag parity).
#
# Output: note/native-lua55/embedbench   (note/ is gitignored)
set -e
cd "$(dirname "$0")/.."

SRC=runtime/lua55/src
OUT=note/native-lua55

mkdir -p "$OUT"
clang -O2 -Wall -DLUA_USE_MACOSX -I"$SRC" \
  -o "$OUT/embedbench" runtime/embedbench55.c "$SRC"/*.c \
  -lreadline -lm

# smoke: one reuse + one fresh run over a trivial staged script
T=$(mktemp /tmp/embedbench-smoke.XXXXXX)
printf 'return #KEYS + #ARGV\n' >"$T"
"$OUT/embedbench" reuse "$T" 3 >/dev/null
"$OUT/embedbench" fresh "$T" 3 >/dev/null
rm -f "$T"

echo "built $OUT/embedbench"
