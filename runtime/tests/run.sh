#!/bin/sh
# runtime/tests/run.sh — native rt_* ABI unit tests (L1/L2 of the plan).
# Builds the runtime + ABI for the host and runs the test matrix:
#   plain, ASan, UBSan. The wasm-side gates (layout + seam) live in
# wasm/sharedmem_test.go and testdiff/rtseam_test.go.
#
# Two ABI generations run side by side until the U2e/U6 cutover:
#   rt_native_test.c  — ABI v3 (rt_abi.c on the 5.1 blob, clua oracle)
#   rt_native55_test.c— ABI v4 (rt_abi55.c on the stock 5.5 tree, U2d)
set -e
cd "$(dirname "$0")/.."

CC=${CC:-clang}
CFLAGS="-DLUA_CORE -DRT_ABI_NATIVE64 -DNDEBUG -I."

run_build() {
  tag=$1; shift
  $CC $CFLAGS "$@" -o tests/rt_native_$tag $SRC -lm
}

run_suite() {
  name=$1; shift
  SRC="$name lua51/src/*.c"
  echo "== 5.1 / ABI v3 plain"
  run_build plain && ./tests/rt_native_plain
  echo "== 5.1 / ABI v3 ASan"
  run_build asan -fsanitize=address -fno-omit-frame-pointer && ./tests/rt_native_asan
  echo "== 5.1 / ABI v3 UBSan"
  run_build ubsan -fsanitize=undefined && UBSAN_OPTIONS=halt_on_error=1 ./tests/rt_native_ubsan
}

run_suite55() {
  name=$1; shift
  SRC="$name lua55/src/*.c"
  echo "== 5.5 / ABI v4 plain"
  run_build plain && ./tests/rt_native_plain
  echo "== 5.5 / ABI v4 ASan"
  run_build asan -fsanitize=address -fno-omit-frame-pointer && ./tests/rt_native_asan
  echo "== 5.5 / ABI v4 UBSan"
  run_build ubsan -fsanitize=undefined && UBSAN_OPTIONS=halt_on_error=1 ./tests/rt_native_ubsan
}

run_suite "tests/rt_native_test.c gnumfmt.c"
run_suite55 "tests/rt_native55_test.c"

rm -f tests/rt_native_plain tests/rt_native_asan tests/rt_native_ubsan
