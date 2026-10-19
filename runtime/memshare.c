/* M3 boundary experiment: exports memory + a reader, so an emitted module
   can share this address space and pass pointers to functions here.
   Still the fixture of wasm/sharedmem_test.go (the shared-linear-memory
   load-bearing check of the ABI).

   Build (no build.sh target — this blob is standalone freestanding wasm;
   wasi-sdk, same toolchain as build55.sh):
     clang --sysroot=$WASI_SDK/share/wasi-sysroot --target=wasm32-wasi \
       -O2 -nostartfiles -mexec-model=reactor \
       -Wl,--export=memory -Wl,--export=rt_peek \
       -Wl,--export=rt_poke -Wl,--export=rt_add_at \
       -Wl,--strip-all -o memshare.wasm memshare.c
*/
#include <stdint.h>

int32_t rt_peek(int32_t addr) {
  return *(int32_t *)(size_t)addr;
}

void rt_poke(int32_t addr, int32_t v) {
  *(int32_t *)(size_t)addr = v;
}

int32_t rt_add_at(int32_t a, int32_t b) {
  return *(int32_t *)(size_t)a + *(int32_t *)(size_t)b;
}
