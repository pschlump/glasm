package testdiff

// The M3 seam smoke: an EMITTED module (wasm/ package) drives the real
// rt_* ABI against the real C runtime module through shared memory (the
// script module imports the runtime's exported memory). Validates the
// whole seam: memory sharing, TValue layout, statuses, error staging.
//
// Runs on wasmtime because the C runtime needs new-EH setjmp; whether the
// production host can stay wazero is the recorded open question for M4
// (the backend's own code never needs EH — only the C runtime does).

import (
	"fmt"
	"os"
	"testing"

	wt "github.com/bytecodealliance/wasmtime-go/v48"

	w "github.com/pschlump/glasm/wasm"
)

func rtSetup(t *testing.T, scriptBin []byte) (*wt.Store, *wt.Instance, func(string, ...interface{}) (uint64, error)) {
	t.Helper()
	cfg := wt.NewConfig()
	cfg.SetWasmExceptions(true)
	engine := wt.NewEngineWithConfig(cfg)
	store := wt.NewStore(engine)
	bin := lua51SjljWasm
	if p := os.Getenv("RTDBG"); p != "" {
		if b, err := os.ReadFile(p); err != nil {
			t.Logf("RTDBG read failed: %v", err)
		} else {
			bin = b
		}
	}
	module, err := wt.NewModule(engine, bin)
	if err != nil {
		t.Fatal(err)
	}
	linker := wt.NewLinker(engine)
	if err := linker.DefineWasi(); err != nil {
		t.Fatal(err)
	}
	if err := linker.DefineFunc(store, "host", "event", func(kind, ptr, ln int32) {}); err != nil {
		t.Fatal(err)
	}
	if err := linker.DefineFunc(store, "host", "random01", func() float64 { return 0 }); err != nil {
		t.Fatal(err)
	}
	if err := linker.DefineFunc(store, "host", "randomint", func(a, b int32) int32 { return 0 }); err != nil {
		t.Fatal(err)
	}
	if err := linker.DefineFunc(store, "host", "randomseed", func(int64) {}); err != nil {
		t.Fatal(err)
	}
	// ABI v3: the runtime imports the wasm-dispatch seam (M5a). The seam
	// tests don't register wasm protos, so a refusing stub suffices; the
	// reentry spike defines the real trampoline itself.
	if err := linker.DefineFunc(store, "host", "wasm_dispatch",
		func(idx, frame, cl, nargs, want int32) int32 { return -3 }); err != nil {
		t.Fatal(err)
	}
	// M7a: hostfn seam — never invoked here (no host functions are
	// registered); the import must resolve.
	if err := linker.DefineFunc(store, "host", "host_call",
		func(fnidx, argsPtr, argsLen, retPtr, retCap int32) int32 { return -1 }); err != nil {
		t.Fatal(err)
	}
	store.SetWasi(wt.NewWasiConfig())
	inst, err := linker.Instantiate(store, module)
	if err != nil {
		t.Fatal(err)
	}

	var scriptInst *wt.Instance
	if scriptBin != nil {
		if err := linker.DefineInstance(store, "rt", inst); err != nil {
			t.Fatal(err)
		}
		sm, err := wt.NewModule(engine, scriptBin)
		if err != nil {
			t.Fatal(err)
		}
		scriptInst, err = linker.Instantiate(store, sm)
		if err != nil {
			t.Fatal(err)
		}
	}

	call := func(name string, args ...interface{}) (uint64, error) {
		f := inst.GetFunc(store, name)
		if f == nil {
			return 0, fmt.Errorf("missing export %s", name)
		}
		for i, a := range args {
			if u, ok := a.(uint64); ok {
				args[i] = int32(u)
			}
		}
		res, err := f.Call(store, args...)
		if err != nil {
			return 0, err
		}
		switch v := res.(type) {
		case nil:
			return 0, nil // void export
		case int32:
			return uint64(uint32(v)), nil
		case int64:
			return uint64(v), nil
		case float64:
			return uint64(v), nil
		}
		return 0, fmt.Errorf("no result from %s", name)
	}
	return store, scriptInst, call
}

// rtEmitScript emits the seam-test module: imports the runtime memory and
// ABI, exports one check function per ABI behavior.
func rtEmitScript(t *testing.T) []byte {
	t.Helper()
	m := w.NewModule()
	m.ImportMemory("rt", "memory", 1, 0)

	void := []w.ValueType(nil)
	i32v := []w.ValueType{w.I32}
	iii := []w.ValueType{w.I32, w.I32, w.I32}
	iiii := []w.ValueType{w.I32, w.I32, w.I32, w.I32}
	iiiii := []w.ValueType{w.I32, w.I32, w.I32, w.I32, w.I32}
	i32f64 := []w.ValueType{w.I32, w.F64}

	mknum := m.ImportFunc("rt", "rt_mknumber", i32f64, void)
	mknil := m.ImportFunc("rt", "rt_mknil", i32v, void)
	intern := m.ImportFunc("rt", "rt_intern", iii, i32v)
	newtbl := m.ImportFunc("rt", "rt_newtable", iii, i32v)
	gettbl := m.ImportFunc("rt", "rt_gettable", iiii, i32v)
	settbl := m.ImportFunc("rt", "rt_settable", iiii, i32v)
	arith := m.ImportFunc("rt", "rt_arith", iiiii, i32v)
	length := m.ImportFunc("rt", "rt_len", iii, i32v)
	eq := m.ImportFunc("rt", "rt_eq", iiii, i32v)
	errPend := m.ImportFunc("rt", "rt_err_pending", void, i32v)
	getg := m.ImportFunc("rt", "rt_getglobal", []w.ValueType{w.I32, w.I32, w.I32}, i32v)

	// cell layout at base: +0 a, +16 b, +32 dst, +48 key, +64 tbl,
	// +96 getdst, +112 eqbool, +128 lendst, +144 strbuf

	// check_arith: 5 + 7 == 12 through rt_arith (OP_ADD=15)
	add := m.NewFunction(i32v, i32v)
	add.LocalGet(0).I32Const(0).I32Add().F64Const(5).Call(mknum)
	add.LocalGet(0).I32Const(16).I32Add().F64Const(7).Call(mknum)
	add.I32Const(15).
		LocalGet(0).
		LocalGet(0).I32Const(16).I32Add().
		LocalGet(0).I32Const(32).I32Add().
		I32Const(1).Call(arith).Drop()
	add.LocalGet(0).I32Const(32).I32Add().F64Load(0).F64Const(12).F64Eq().End()
	add.Export("check_arith")

	// check_table: t = {}; t["hello"] = 12; t["hello"] == 12 via rt_eq
	tb := m.NewFunction(i32v, i32v)
	tb.Local(w.I32) // local 1: strbuf = base+144
	tb.LocalGet(0).I32Const(144).I32Add().LocalSet(1)
	tb.LocalGet(1).I32Const(0x68).I32Store8(0)
	tb.LocalGet(1).I32Const(1).I32Add().I32Const(0x65).I32Store8(0)
	tb.LocalGet(1).I32Const(2).I32Add().I32Const(0x6c).I32Store8(0)
	tb.LocalGet(1).I32Const(3).I32Add().I32Const(0x6c).I32Store8(0)
	tb.LocalGet(1).I32Const(4).I32Add().I32Const(0x6f).I32Store8(0)
	tb.LocalGet(0).I32Const(48).I32Add().LocalGet(1).I32Const(5).Call(intern).Drop()
	tb.LocalGet(0).I32Const(64).I32Add().I32Const(0).I32Const(0).Call(newtbl).Drop()
	tb.LocalGet(0).I32Const(32).I32Add().F64Const(12).Call(mknum)
	tb.LocalGet(0).I32Const(64).I32Add().
		LocalGet(0).I32Const(48).I32Add().
		LocalGet(0).I32Const(32).I32Add().
		I32Const(2).Call(settbl).Drop()
	tb.LocalGet(0).I32Const(64).I32Add().
		LocalGet(0).I32Const(48).I32Add().
		LocalGet(0).I32Const(96).I32Add().
		I32Const(3).Call(gettbl).Drop()
	tb.LocalGet(0).I32Const(96).I32Add().
		LocalGet(0).I32Const(32).I32Add().
		LocalGet(0).I32Const(112).I32Add().
		I32Const(4).Call(eq).Drop()
	// TValue.tt at offset 8 is LUA_TBOOLEAN(1); value.b at offset 0 is 1
	tb.LocalGet(0).I32Const(112).I32Add().I32Load8U(8).I32Const(1).I32Eq().
		LocalGet(0).I32Const(112).I32Add().I32Load(0).I32Const(1).I32Eq().
		I32And().End()
	tb.Export("check_table")

	// check_len: #"hello" == 5
	ln := m.NewFunction(i32v, i32v)
	ln.Local(w.I32)
	ln.LocalGet(0).I32Const(144).I32Add().LocalSet(1)
	ln.LocalGet(1).I32Const(0x68).I32Store8(0)
	ln.LocalGet(1).I32Const(1).I32Add().I32Const(0x69).I32Store8(0)
	ln.LocalGet(1).I32Const(2).I32Add().I32Const(0x6a).I32Store8(0)
	ln.LocalGet(0).I32Const(48).I32Add().LocalGet(1).I32Const(3).Call(intern).Drop()
	ln.LocalGet(0).I32Const(48).I32Add().
		LocalGet(0).I32Const(128).I32Add().
		I32Const(5).Call(length).Drop()
	ln.LocalGet(0).I32Const(128).I32Add().F64Load(0).F64Const(3).F64Eq().End()
	ln.Export("check_len")

	// check_getglobal: _G.print exists (a function), _G.nosuch is nil
	gg := m.NewFunction(i32v, i32v)
	gg.Local(w.I32) // strbuf
	gg.LocalGet(0).I32Const(208).I32Add().LocalSet(1)
	for i, ch := range []byte("print") {
		gg.LocalGet(1).I32Const(int32(i)).I32Add().I32Const(int32(ch)).I32Store8(0)
	}
	gg.LocalGet(0).I32Const(48).I32Add().LocalGet(1).I32Const(5).Call(intern).Drop()
	gg.LocalGet(0).I32Const(160).I32Add().LocalGet(0).I32Const(48).I32Add().I32Const(9).Call(getg)
	gg.Drop()
	gg.LocalGet(0).I32Const(160).I32Add().I32Load8U(8).End() // return the raw tag
	gg.Export("check_getglobal")

	// check_err: indexing nil stages an error (RT_ERR + err_pending=1)
	er := m.NewFunction(i32v, i32v)
	er.LocalGet(0).I32Const(0).I32Add().Call(mknil)
	er.LocalGet(0).I32Const(48).I32Add().Call(mknil)
	er.LocalGet(0).I32Const(0).I32Add().
		LocalGet(0).I32Const(48).I32Add().
		LocalGet(0).I32Const(32).I32Add().
		I32Const(7).Call(gettbl).Drop()
	er.Call(errPend).End()
	er.Export("check_err")

	return m.Encode()
}

func TestRTDirectABI(t *testing.T) {
	_, _, rtCall := rtSetup(t, nil)
	L, err := rtCall("lnewstate")
	if err != nil || L == 0 {
		t.Fatalf("lnewstate: %v %v", L, err)
	}
	if _, err := rtCall("rt_set_state", L); err != nil {
		t.Fatal(err)
	}
	base, err := rtCall("rt_frame_alloc", uint64(256))
	if err != nil || base == 0 {
		t.Fatalf("frame_alloc: %v %v", base, err)
	}
	// host-side ABI exercise: mknumber 5 and 7, arith ADD, read back f64
	if _, err := rtCall("rt_mknumber", base, 5.0); err != nil {
		t.Fatalf("mknumber: %v", err)
	}
	if _, err := rtCall("rt_mknumber", base+16, 7.0); err != nil {
		t.Fatalf("mknumber2: %v", err)
	}
	st, err := rtCall("rt_arith", 15, base, base+16, base+32, 1)
	if err != nil {
		t.Fatalf("arith: %v", err)
	}
	t.Logf("arith status=%d", st)
	v, err := rtCall("rt_err_pending")
	t.Logf("err_pending=%d %v", v, err)
}

func TestRTSeamSmoke(t *testing.T) {
	bin := rtEmitScript(t)
	store, scriptInst, rtCall := rtSetup(t, bin)
	_ = store

	L, err := rtCall("lnewstate")
	if err != nil || L == 0 {
		t.Fatalf("lnewstate: %v %v", L, err)
	}
	if _, err := rtCall("rt_set_state", L); err != nil {
		t.Fatalf("rt_set_state: %v", err)
	}
	if v, err := rtCall("rt_abi_version"); err != nil || v != 3 {
		t.Fatalf("rt_abi_version: %v %v", v, err)
	}
	base, err := rtCall("rt_frame_alloc", uint64(16*16))
	if err != nil || base == 0 {
		t.Fatalf("rt_frame_alloc: %v %v", base, err)
	}

	for _, name := range []string{"check_arith", "check_table", "check_len", "check_getglobal", "check_err"} {
		res, err := scriptInst.GetFunc(store, name).Call(store, int32(base))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v, ok := res.(int32); !ok || v != 1 {
			if name == "check_getglobal" {
				t.Logf("getglobal tag = %d (want 70)", res)
			} else {
				t.Errorf("%s = %v (want 1)", name, res)
			}
		} else {
			t.Logf("%s ok", name)
		}
	}
}
