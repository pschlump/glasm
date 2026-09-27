package testdiff

// U2d smoke gate for the ABI v4 runtime blob: lua55_rt.wasm (stock Lua
// 5.5.1 + rt_abi55.c, zero patches) must instantiate on the production
// engine (wazero) with its two host imports, answer LUA_RT_ABI=4, and
// run a chunk through the driver — integer semantics intact. The full
// ABI surface is exercised natively by runtime/tests (rt_native55_test,
// the U2d gate proper); the engines wire up at U2e.
//
// Blob provenance: runtime/build55.sh → testdiff/lua55_rt.wasm (same
// pipeline as lua55_sjlj.wasm; `go clean -testcache` after rebuilding).

import (
	"context"
	_ "embed"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed lua55_rt.wasm
var lua55RTWasm []byte

func TestRT55BlobSmoke(t *testing.T) {
	ctx := t.Context()
	// the blob is built in the EH/SJLJ dialect like lua55_sjlj.wasm
	// (build55.sh) — wazero needs the feature flag (wazeroengine.go)
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2|experimental.CoreFeaturesExceptionHandling))
	defer r.Close(ctx)
	// the reactor still links wasi-libc (the driver's shims); instantiate
	// the preview1 facade first, like wazeroengine.go does
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		t.Fatalf("wasi: %v", err)
	}

	// the blob imports host.event (the oracle driver) and
	// host.wasm_dispatch (the ABI v4 trampoline seam)
	var events []string
	host, err := r.NewHostModuleBuilder("host").
		NewFunctionBuilder().
		WithFunc(func(ctx2 context.Context, kind, ptr, ln uint32) {
			if kind == 1 && ln > 4 { // EVT_PRINT: header + values, decoded loosely
				events = append(events, "print")
			}
		}).
		Export("event").
		NewFunctionBuilder().
		WithFunc(func(idx, frame, cl, nargs, want uint32) uint32 { // v4 seam
			return 0xFFFFFFFD // RTW_REFUSED (-3): no script module here
		}).
		Export("wasm_dispatch").
		// U2e: the D5 host-RNG shim imports (math.random determinism)
		NewFunctionBuilder().WithFunc(func() float64 { return 0.5 }).Export("random01").
		NewFunctionBuilder().WithFunc(func(lo, hi uint32) uint32 { return lo }).Export("randomint").
		NewFunctionBuilder().WithFunc(func(seed int64) {}).Export("randomseed").
		Instantiate(ctx)
	if err != nil {
		t.Fatalf("host module: %v", err)
	}
	_ = host

	mod, err := r.InstantiateWithConfig(ctx, lua55RTWasm,
		wazero.NewModuleConfig().WithName("lua55rt"))
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}

	ver, err := mod.ExportedFunction("rt_abi_version").Call(ctx)
	if err != nil {
		t.Fatalf("rt_abi_version: %v", err)
	}
	if ver[0] != 4 {
		t.Fatalf("ABI version = %d, want 4", ver[0])
	}
	for _, fn := range []string{"rt_arith", "rt_forprep", "rt_newclosure",
		"rt_tail_restage", "rt_mkinteger", "rt_gettable", "ldostring"} {
		if mod.ExportedFunction(fn) == nil {
			t.Errorf("%s not exported", fn)
		}
	}

	// a full driver run: integers, //, bitwise — the shapes that forced
	// ABI v4 (a 5.1 blob round-trips every value through f64)
	st, err := mod.ExportedFunction("lnewstate").Call(ctx)
	if err != nil || st[0] == 0 {
		t.Fatalf("lnewstate: %v %v", st, err)
	}
	L := uint32(st[0])
	src := []byte("local x = 9007199254740993 " +
		"print(x // 7, 7 & 3, 1 << 62 == 4611686018427387904)")
	if !mod.Memory().Write(4096, src) {
		t.Fatal("memory write")
	}
	// chunk name at 4096+len+16: "@smoke"
	name := append([]byte("@smoke"), 0)
	mod.Memory().Write(4096+uint32(len(src))+16, name)
	res, err := mod.ExportedFunction("ldostring").Call(ctx,
		uint64(L), 4096, uint64(len(src)), 4096+uint64(len(src))+16, 0)
	if err != nil {
		t.Fatalf("ldostring trap: %v", err)
	}
	if res[0] != 0 {
		t.Fatalf("ldostring status = %d, want 0", res[0])
	}
	if len(events) == 0 {
		t.Fatal("no print event crossed host.event")
	}
	mod.ExportedFunction("lclose").Call(ctx, uint64(L))
}
