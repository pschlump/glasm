#!/bin/sh
# build-native55.sh — build a NATIVE stock C Lua 5.5.1 interpreter from the
# vendored runtime/lua55 tree, for cmd/perfcmp's "original C" leg
# (docs/Perf-vs-C-Lua.md).
#
# The vendored tree carries no lua.c (the blob is a reactor, VENDORED.md),
# so the stock standalone driver is extracted from U0's saved upstream
# tarball. The result is stock 5.5.1 modulo the one vendored patch
# (loadlib.c readable(), VENDORED.md) and keeps stock runtime defaults
# (time-based seed — the build55.sh determinism pins bind the wasm oracle,
# not native C).
#
# Output: note/native-lua55/lua   (note/ is gitignored)
set -e
cd "$(dirname "$0")/.."

SRC=runtime/lua55/src
OUT=note/native-lua55
TARBALL=note/conv-from-5.2-to-5.5/lua-5.5.1.tar.gz

[ -f "$TARBALL" ] || { echo "missing $TARBALL (U0's saved upstream tarball)" >&2; exit 1; }

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
tar xzf "$TARBALL" -C "$WORK" lua-5.5.1/src/lua.c

mkdir -p "$OUT"
clang -O2 -Wall -DLUA_USE_MACOSX -I"$SRC" \
  -o "$OUT/lua" "$WORK/lua-5.5.1/src/lua.c" "$SRC"/*.c \
  -lreadline -lm

"$OUT/lua" -v
echo "built $OUT/lua"
