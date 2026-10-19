package wasm_test

// Execution gate for the M0 emitter spike (docs/Lua-Wasm-Design-and-Test-Plan.md):
// every emitted construct must run correctly on wazero. This file also
// answers the two M0 open questions: wazero's maximum wasm stack depth and
// its instantiation cost.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	w "github.com/pschlump/glasm/wasm"
)

func i32(v ...w.ValueType) []w.ValueType { return v }

func mustCall(t *testing.T, mod api.Module, name string, args ...uint64) uint64 {
	t.Helper()
	res, err := mod.ExportedFunction(name).Call(context.Background(), args...)
	if err != nil {
		t.Fatalf("call %s(%v): %v", name, args, err)
	}
	if len(res) == 0 {
		t.Fatalf("call %s: no result", name)
	}
	return res[0]
}

// mustCall0 invokes a void function.
func mustCall0(t *testing.T, mod api.Module, name string, args ...uint64) {
	t.Helper()
	if _, err := mod.ExportedFunction(name).Call(context.Background(), args...); err != nil {
		t.Fatalf("call %s(%v): %v", name, args, err)
	}
}

func instantiate(t *testing.T, bin []byte) (context.Context, api.Module) {
	t.Helper()
	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	mod, err := r.Instantiate(ctx, bin)
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	return ctx, mod
}

func TestExecArith(t *testing.T) {
	m := w.NewModule()
	add := m.NewFunction(i32(w.I32, w.I32), i32(w.I32))
	add.LocalGet(0).LocalGet(1).I32Add().End()
	add.Export("add")

	f := m.NewFunction(i32(w.F64), i32(w.F64))
	f.LocalGet(0).F64Const(2.5).F64Mul().F64Const(0.25).F64Add().F64Const(2).F64Div().End()
	f.Export("f")

	bin := m.Encode()
	ctx, mod := instantiate(t, bin)
	if got := mustCall(t, mod, "add", 20, 22); got != 42 {
		t.Errorf("add(20,22) = %d, want 42", got)
	}
	got := api.DecodeF64(mustCall(t, mod, "f", api.EncodeF64(4)))
	if got != (4*2.5+0.25)/2 {
		t.Errorf("f(4) = %v", got)
	}
	_ = ctx
}

func TestExecLoop(t *testing.T) {
	m := w.NewModule()
	sum := m.NewFunction(i32(w.I32), i32(w.I32))
	sum.Local(w.I32).Local(w.I32) // 1: i, 2: acc
	// canonical pre-test loop lowering: block { loop { br_if out; body; br loop } }
	sum.I32Const(1).LocalSet(1)
	sum.Block(w.Void).
		Loop(w.Void).
		LocalGet(1).LocalGet(0).I32GtS().BrIf(1).
		LocalGet(2).LocalGet(1).I32Add().LocalSet(2).
		LocalGet(1).I32Const(1).I32Add().LocalSet(1).
		Br(0).
		End().
		End()
	sum.LocalGet(2).End()
	sum.Export("sum")

	_, mod := instantiate(t, m.Encode())
	for _, c := range []struct{ n, want uint64 }{{10, 55}, {1000, 500500}, {0, 0}} {
		if got := mustCall(t, mod, "sum", c.n); got != c.want {
			t.Errorf("sum(%d) = %d, want %d", c.n, got, c.want)
		}
	}
}

func TestExecBrTable(t *testing.T) {
	m := w.NewModule()
	pick := m.NewFunction(i32(w.I32), i32(w.I32))
	pick.Local(w.I32) // 1: r (stays 0 for the default case)
	pick.Block(w.Void).Block(w.Void).Block(w.Void).
		LocalGet(0).
		BrTable([]uint32{0, 1, 2}, 2).
		End().
		I32Const(100).LocalSet(1).Br(1).
		End().
		I32Const(200).LocalSet(1).Br(0).
		End().
		LocalGet(1).End()
	pick.Export("pick")

	_, mod := instantiate(t, m.Encode())
	for _, c := range []struct {
		in, want uint64
	}{{0, 100}, {1, 200}, {2, 0}, {42, 0}} {
		if got := mustCall(t, mod, "pick", c.in); got != c.want {
			t.Errorf("pick(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestExecMemoryAndData(t *testing.T) {
	m := w.NewModule()
	m.Memory(1, 4)
	m.Data(64, []byte("hello"))
	m.ExportMemory("memory")

	byteAt := m.NewFunction(i32(w.I32), i32(w.I32))
	byteAt.LocalGet(0).I32Load8U(0).End()
	byteAt.Export("byteAt")

	store := m.NewFunction(i32(w.I32), nil)
	store.I32Const(128).LocalGet(0).I32Store(0).End()
	store.Export("store")

	_, mod := instantiate(t, m.Encode())
	if got := mustCall(t, mod, "byteAt", 64); got != 'h' {
		t.Errorf("byteAt(64) = %d, want %d", got, 'h')
	}
	mustCall0(t, mod, "store", 4242)
	if v, ok := mod.Memory().ReadUint32Le(128); !ok || v != 4242 {
		t.Errorf("host read after guest store: %d ok=%v, want 4242", v, ok)
	}
	// host write, guest read
	if !mod.Memory().WriteUint32Le(192, 777) {
		t.Fatal("host memory write failed")
	}
	if got := mustCall(t, mod, "byteAt", 192); got != 777&0xff {
		t.Errorf("byteAt(192) = %d, want %d", got, 777&0xff)
	}
}

func TestExecCallDirectAndIndirect(t *testing.T) {
	m := w.NewModule()
	m.Table(4, 0)

	double := m.NewFunction(i32(w.I32), i32(w.I32))
	double.LocalGet(0).LocalGet(0).I32Add().End()

	dec := m.NewFunction(i32(w.I32), i32(w.I32))
	dec.LocalGet(0).I32Const(1).I32Sub().End()

	m.Element(0, double.Idx(), dec.Idx())

	viaTable := m.NewFunction(i32(w.I32, w.I32), i32(w.I32))
	viaTable.LocalGet(1).LocalGet(0).CallIndirect(m.TypeIndex(i32(w.I32), i32(w.I32))).End()
	viaTable.Export("viaTable")

	main := m.NewFunction(i32(w.I32), i32(w.I32))
	main.LocalGet(0).Call(double.Idx()).End()
	main.Export("doubleIt")

	_, mod := instantiate(t, m.Encode())
	if got := mustCall(t, mod, "doubleIt", 21); got != 42 {
		t.Errorf("doubleIt(21) = %d", got)
	}
	if got := mustCall(t, mod, "viaTable", 0, 21); got != 42 {
		t.Errorf("viaTable(0,21) = %d", got)
	}
	if got := mustCall(t, mod, "viaTable", 1, 10); got != 9 {
		t.Errorf("viaTable(1,10) = %d", got)
	}
	// out-of-range table index must surface as a controlled error
	if _, err := mod.ExportedFunction("viaTable").Call(context.Background(), 5, 1); err == nil {
		t.Error("viaTable(5,1): expected trap for out-of-bounds table index")
	}
}

func TestExecHostRoundtrip(t *testing.T) {
	m := w.NewModule()
	dbl := m.ImportFunc("env", "double", i32(w.I32), i32(w.I32))
	m.Memory(1, 0)
	m.ExportMemory("memory")

	f := m.NewFunction(i32(w.I32), i32(w.I32))
	f.LocalGet(0).Call(dbl).I32Const(1).I32Add().End()
	f.Export("f")

	readAt := m.NewFunction(i32(w.I32), i32(w.I32))
	readAt.LocalGet(0).I32Load(0).End()
	readAt.Export("readAt")

	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	t.Cleanup(func() { r.Close(ctx) })
	if _, err := r.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithFunc(func(ctx context.Context, v uint32) uint32 { return v * 2 }).
		Export("double").
		Instantiate(ctx); err != nil {
		t.Fatal(err)
	}
	mod, err := r.Instantiate(ctx, m.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got := mustCall(t, mod, "f", 5); got != 11 {
		t.Errorf("f(5) = %d, want 11 (host double(5)=10, +1)", got)
	}
	if !mod.Memory().WriteUint32Le(200, 424242) {
		t.Fatal("host write failed")
	}
	if got := mustCall(t, mod, "readAt", 200); got != 424242 {
		t.Errorf("readAt(200) = %d", got)
	}
}

func TestExecGlobals(t *testing.T) {
	m := w.NewModule()
	g := m.GlobalI32(40, true)
	inc := m.NewFunction(nil, i32(w.I32))
	inc.GlobalGet(g).I32Const(1).I32Add().GlobalSet(g).GlobalGet(g).End()
	inc.Export("inc")

	_, mod := instantiate(t, m.Encode())
	if got := mustCall(t, mod, "inc"); got != 41 {
		t.Errorf("inc #1 = %d", got)
	}
	if got := mustCall(t, mod, "inc"); got != 42 {
		t.Errorf("inc #2 = %d", got)
	}
}

// TestWazeroStackDepthProbe answers the M0 open question: how deep can wasm
// recursion go on wazero before trapping, and does it trap cleanly?
func TestWazeroStackDepthProbe(t *testing.T) {
	const cap = uint64(2_000_000) // safety bound for slow interpreter engines
	m := w.NewModule()
	m.Memory(1, 0)
	m.ExportMemory("memory")
	rec := m.NewFunction(i32(w.I32), i32(w.I32))
	rec.LocalGet(0).I32Const(int32(cap)).I32GeU().
		If(w.Void).
		LocalGet(0).Return().
		End().
		I32Const(0).LocalGet(0).I32Store(0).
		LocalGet(0).I32Const(1).I32Add().Call(rec.Idx()).
		End()
	rec.Export("rec")

	ctx, mod := instantiate(t, m.Encode())
	res, err := mod.ExportedFunction("rec").Call(ctx, 0)
	if err != nil {
		depth, ok := mod.Memory().ReadUint32Le(0)
		if !ok {
			t.Fatal("trap but could not read depth from memory")
		}
		t.Logf("wasm stack: trapped at depth %d (clean error: %v)", depth, err)
		if depth < 1000 {
			t.Errorf("max wasm stack depth %d is too shallow for Lua recursion", depth)
		}
		return
	}
	t.Logf("wasm stack: no trap up to cap %d (result %d)", cap, res[0])
	if res[0] != cap {
		t.Errorf("unexpected result %d", res[0])
	}
}

// TestExternalToolValidation runs wabt's wasm2wat or wasm-tools validate over
// an emitted module when either is installed (M0 gate; skips otherwise).
func TestExternalToolValidation(t *testing.T) {
	m := w.NewModule()
	m.Memory(1, 0)
	m.Table(2, 0)
	f := m.NewFunction(i32(w.I32, w.I32), i32(w.I32))
	f.Local(w.I64)
	f.LocalGet(0).LocalGet(1).I32Add().I64ExtendI32S().LocalTee(2).I32WrapI64().End()
	f.Export("add")
	m.Element(0, f.Idx())

	bin := m.Encode()

	var tool, argsPrefix string
	if p, err := exec.LookPath("wasm2wat"); err == nil {
		tool, argsPrefix = p, ""
	} else if p, err := exec.LookPath("wasm-tools"); err == nil {
		tool, argsPrefix = p, "validate"
	} else {
		t.Skip("neither wasm2wat nor wasm-tools installed")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "mod.wasm")
	if err := os.WriteFile(file, bin, 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{}
	if argsPrefix != "" {
		args = append(args, argsPrefix)
	}
	args = append(args, file)
	if out, err := exec.Command(tool, args...).CombinedOutput(); err != nil {
		t.Errorf("external validation failed: %v\n%s", err, out)
	}
}

/* M0 open question: instantiation and call costs on wazero. */

func buildBenchModule() []byte {
	m := w.NewModule()
	add := m.NewFunction(i32(w.I32, w.I32), i32(w.I32))
	add.LocalGet(0).LocalGet(1).I32Add().End()
	add.Export("add")
	return m.Encode()
}

func BenchmarkInstantiate(b *testing.B) {
	ctx := context.Background()
	bin := buildBenchModule()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)
	compiled, err := r.CompileModule(ctx, bin)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("instantiate_only", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			mod, err := r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("bench"))
			if err != nil {
				b.Fatal(err)
			}
			if err := mod.Close(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("compile_and_instantiate", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			c, err := r.CompileModule(ctx, bin)
			if err != nil {
				b.Fatal(err)
			}
			mod, err := r.InstantiateModule(ctx, c, wazero.NewModuleConfig().WithName("bench"))
			if err != nil {
				b.Fatal(err)
			}
			if err := mod.Close(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("call", func(b *testing.B) {
		mod, err := r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("callbench"))
		if err != nil {
			b.Fatal(err)
		}
		defer mod.Close(ctx)
		fn := mod.ExportedFunction("add")
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := fn.Call(ctx, 20, 22); err != nil {
				b.Fatal(err)
			}
		}
	})
}
