#!/bin/sh
# runtime/tests/run.sh — native rt_* ABI unit tests (L1/L2 of the plan).
# Builds the runtime + ABI for the host and runs the test matrix:
#   plain, ASan, UBSan. The wasm-side gates (layout + seam) live in
# wasm/sharedmem_test.go; the seam end-to-end is the differential matrix.
#
# rt_native55_test.c — ABI v4 (rt_abi55.c on the stock 5.5 tree, U2d).
# The ABI v3 suite (rt_native_test.c on the 5.1 tree, the clua oracle)
# was retired with that tree at U8 (ledger row 55's executed retirement).
set -e
cd "$(dirname "$0")/.."

CC=${CC:-clang}
CFLAGS="-DLUA_CORE -DRT_ABI_NATIVE64 -DNDEBUG -I."

run_build() {
  tag=$1; shift
  $CC $CFLAGS "$@" -o tests/rt_native_$tag $SRC -lm
}

run_suite55() {
  name=$1; shift
  # ltable_stock55.c (U6c): the renamed second compile whose rt_lts_*
  # bodies the seven table-write guards delegate to (linked beside the
  # stock tree, mirroring the blob's RT flavor link order)
  SRC="$name ltable_stock55.c lua55/src/*.c"
  echo "== 5.5 / ABI v4 plain"
  run_build plain && ./tests/rt_native_plain
  echo "== 5.5 / ABI v4 ASan"
  run_build asan -fsanitize=address -fno-omit-frame-pointer && ./tests/rt_native_asan
  echo "== 5.5 / ABI v4 UBSan"
  run_build ubsan -fsanitize=undefined && UBSAN_OPTIONS=halt_on_error=1 ./tests/rt_native_ubsan
}

run_suite55 "tests/rt_native55_test.c"

rm -f tests/rt_native_plain tests/rt_native_asan tests/rt_native_ubsan
