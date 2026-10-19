package wasm_test

// M3 module-boundary experiment (design doc §5, §7): the script module and
// the C runtime module are separate wasm instances — can they share one
// linear memory (script imports the runtime's exported memory) and pass
// pointers across rt_* imports? This is the load-bearing assumption of the
// ABI: shared address space + downward-only calls + lvm fallback.

import (
	"context"
	"os"
	"testing"

	"github.com/tetratelabs/wazero"

	w "github.com/pschlump/glasm/wasm"
)

func TestSharedMemoryBoundary(t *testing.T) {
	rtBin, err := os.ReadFile("../runtime/memshare.wasm")
	if err != nil {
		t.Skip("memshare.wasm not built — the recipe is in runtime/memshare.c's header")
	}

	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	// runtime module first: exports memory + pointer-taking functions
	rt, err := r.InstantiateWithConfig(ctx, rtBin, wazero.NewModuleConfig().WithName("rt"))
	if err != nil {
		t.Fatalf("instantiate rt: %v", err)
	}

	// emitted script module: imports rt's memory and functions
	m := w.NewModule()
	m.ImportMemory("rt", "memory", 2, 0)
	peek := m.ImportFunc("rt", "rt_peek", []w.ValueType{w.I32}, []w.ValueType{w.I32})
	addAt := m.ImportFunc("rt", "rt_add_at", []w.ValueType{w.I32, w.I32}, []w.ValueType{w.I32})
	m.Data(4096, []byte{7, 0, 0, 0, 35, 0, 0, 0}) // data segment in SHARED memory

	// run(a, b): store a at 4096 (emitted store), compute rt_add_at over
	// shared-memory cells, store the result at 8192, return it
	run := m.NewFunction([]w.ValueType{w.I32, w.I32}, []w.ValueType{w.I32})
	run.I32Const(4096).LocalGet(0).I32Store(0)
	run.I32Const(8192).I32Const(4096).I32Const(4100).Call(addAt).I32Store(0)
	run.I32Const(8192).I32Load(0).End()
	run.Export("run")

	chk := m.NewFunction([]w.ValueType{w.I32}, []w.ValueType{w.I32})
	chk.LocalGet(0).Call(peek).End()
	chk.Export("chk")

	mod, err := r.Instantiate(ctx, m.Encode())
	if err != nil {
		t.Fatalf("instantiate script: %v", err)
	}

	// data segment of the emitted module, visible through the C module's view
	if v, err := rt.ExportedFunction("rt_peek").Call(ctx, 4100); err != nil || uint32(v[0]) != 35 {
		t.Errorf("C reading emitted data segment: %v %v (want 35)", v, err)
	}
	// run(21, ...) overwrites 4096 with 21 → addAt(4096, 4100) = 21 + 35 = 56
	if v, err := mod.ExportedFunction("run").Call(ctx, 21, 1); err != nil || uint32(v[0]) != 56 {
		t.Fatalf("run: %v %v (want 56)", v, err)
	}
	// emitted store visible to C
	if v, err := rt.ExportedFunction("rt_peek").Call(ctx, 4096); err != nil || uint32(v[0]) != 21 {
		t.Errorf("C reading emitted store: %v %v (want 21)", v, err)
	}
	// C function, called from emitted code, over shared pointers
	if v, err := mod.ExportedFunction("chk").Call(ctx, 8192); err != nil || uint32(v[0]) != 56 {
		t.Errorf("emitted code calling C over shared pointers: %v %v (want 56)", v, err)
	}
}
