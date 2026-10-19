package testdiff

// The M5a A2 reentrancy spike (note/m5-to-m6-detailed-plan.md §6 A2):
// prove the C runtime can dispatch a registered wasm proto through the
// Go host back into the script module — nested calls on the SAME
// wasmtime store — BEFORE the backend builds on the machinery. Fallbacks
// if this fails: two-instance scheme / engine-level dispatch loop.
//
// The hand-emitted module exports lua_dispatch with four compiled
// "protos": f (calls g through rt_call — two wasm levels around one C
// call), g (identity — the adapter's arg copy is the result), h (errors
// via rt_error: staged value is nil, message bytes are not), f2 (calls
// h; the error must unwind through both adapter levels + rt_run + pcall).
//
// Asserts: value round-trip; forced -1 surfaces as a Lua error carrying
// the staged VALUE (nil, not the message bytes); two-deep nesting.

import (
	"fmt"
	"os"
	"testing"
	"unsafe"

	wt "github.com/bytecodealliance/wasmtime-go/v48"

	w "github.com/pschlump/glasm/wasm"
)

func reentryEmitScript(t *testing.T) []byte {
	t.Helper()
	m := w.NewModule()
	m.ImportMemory("rt", "memory", 1, 0)

	void := []w.ValueType(nil)
	i32v := []w.ValueType{w.I32}
	iii := []w.ValueType{w.I32, w.I32, w.I32}
	iiiii := []w.ValueType{w.I32, w.I32, w.I32, w.I32, w.I32}
	i32f64 := []w.ValueType{w.I32, w.F64}

	getg := m.ImportFunc("rt", "rt_getglobal", iii, i32v)
	callf := m.ImportFunc("rt", "rt_call", iiiii, i32v)
	errf := m.ImportFunc("rt", "rt_error", iii, void)
	mknum := m.ImportFunc("rt", "rt_mknumber", i32f64, void)
	intern := m.ImportFunc("rt", "rt_intern", iii, i32v)

	// lua_dispatch(idx, frame, cl, nargs, want) -> nret. Everything is a
	// cell at frame+16k; scratch at cells 12..14 sits inside the
	// framecells=16 window the adapter sizes.
	d := m.NewFunction([]w.ValueType{w.I32, w.I32, w.I32, w.I32, w.I32}, i32v)
	d.Local(w.I32) // 5: rt status
	d.Local(w.I64) // 6, 7: cell-copy pair
	d.Local(w.I64)
	cell := func(k int) { d.LocalGet(1).I32Const(int32(16 * k)).I32Add() }
	copyCell := func(dst, src int) {
		cell(src)
		d.I64Load(0).LocalSet(6)
		cell(src)
		d.I64Load(8).LocalSet(7)
		cell(dst)
		d.LocalGet(6).I64Store(0)
		cell(dst)
		d.LocalGet(7).I64Store(8)
	}
	writeBytes := func(k int, s string) {
		for i := 0; i < len(s); i++ {
			d.LocalGet(1).I32Const(int32(16*k + i)).I32Add().I32Const(int32(s[i])).I32Store8(0)
		}
	}
	internInPlace := func(k int, s string) {
		writeBytes(k, s)
		cell(k)
		cell(k)
		d.I32Const(int32(len(s))).Call(intern).Drop()
	}

	// idx 0 — f: g(42) through rt_call, copy the result to frame[0].
	// Nesting: f's dispatch → rt_call → precall_wasm → g's dispatch.
	d.LocalGet(0).I32Const(0).I32Eq().If(w.Void)
	internInPlace(12, "g")
	cell(13)
	cell(12)
	d.I32Const(1).Call(getg).LocalSet(5)
	d.LocalGet(5).I32Const(1).I32Eq().If(w.Void)
	d.I32Const(-1).Return()
	d.End()
	cell(14)
	d.F64Const(42).Call(mknum)
	cell(13)
	cell(14)
	d.I32Const(1)
	d.I32Const(1)
	d.I32Const(1)
	d.Call(callf).LocalSet(5)
	d.LocalGet(5).I32Const(1).I32Eq().If(w.Void)
	d.I32Const(-1).Return()
	d.End()
	copyCell(0, 14)
	d.I32Const(1).Return()
	d.End()

	// idx 1 — g: identity. The adapter copied arg 0 to frame[0]; missing
	// args are nil-filled here (the A3 prologue does this generally).
	d.LocalGet(0).I32Const(1).I32Eq().If(w.Void)
	d.LocalGet(3).I32Const(0).I32Eq().If(w.Void)
	cell(0)
	d.I32Const(0).I32Store8(8) // tag byte = nil
	d.End()
	d.I32Const(1).Return()
	d.End()

	// idx 2 — h: rt_error("boom") then -1. The staged VALUE is nil —
	// pcall must receive nil, not the message bytes.
	d.LocalGet(0).I32Const(2).I32Eq().If(w.Void)
	writeBytes(12, "boom")
	cell(12)
	d.I32Const(4)
	d.I32Const(1)
	d.Call(errf)
	d.I32Const(-1).Return()
	d.End()

	// idx 3 — f2: h() through rt_call; RT_ERR → -1. The error unwinds
	// h's adapter (throw) → rt_run (stage) → f2's wasm body (-1) → f2's
	// adapter (re-raise) → pcall.
	d.LocalGet(0).I32Const(3).I32Eq().If(w.Void)
	internInPlace(12, "h")
	cell(13)
	cell(12)
	d.I32Const(1).Call(getg).LocalSet(5)
	d.LocalGet(5).I32Const(1).I32Eq().If(w.Void)
	d.I32Const(-1).Return()
	d.End()
	cell(13)
	cell(14)
	d.I32Const(0)
	d.I32Const(0)
	d.I32Const(1)
	d.Call(callf).LocalSet(5)
	d.LocalGet(5).I32Const(1).I32Eq().If(w.Void)
	d.I32Const(-1).Return()
	d.End()
	d.I32Const(0).Return()
	d.End()

	// unknown idx: refused
	d.I32Const(-3).Return()
	d.End()
	d.Export("lua_dispatch")
	return m.Encode()
}

func TestWasmReentrySpike(t *testing.T) {
	bin := reentryEmitScript(t)

	cfg := wt.NewConfig()
	cfg.SetWasmExceptions(true)
	engine := wt.NewEngineWithConfig(cfg)
	store := wt.NewStore(engine)
	linker := wt.NewLinker(engine)
	if err := linker.DefineWasi(); err != nil {
		t.Fatal(err)
	}
	store.SetWasi(wt.NewWasiConfig())

	var lines []string
	decoder := &CLua{}
	var guestMem *wt.Memory
	memRead := func(ptr, length int32) []byte {
		if guestMem == nil {
			return nil
		}
		base := guestMem.Data(store)
		size := guestMem.DataSize(store)
		if uintptr(ptr)+uintptr(length) > size {
			return nil
		}
		return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(base)+uintptr(ptr))), length)
	}
	if err := linker.DefineFunc(store, "host", "event", func(kind, ptr, length int32) {
		if raw := memRead(ptr, length); raw != nil {
			decoder.onEvent(raw, kind, func(ev, payload string) {
				lines = append(lines, ev+"\t"+payload)
			})
		}
	}); err != nil {
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

	// THE seam under test: the real trampoline — the C adapter's dispatch
	// re-enters wasm through the host, same store, nested call.
	var scriptInst *wt.Instance
	if err := linker.DefineFunc(store, "host", "wasm_dispatch", func(idx, frame, cl, nargs, want int32) int32 {
		fn := scriptInst.GetFunc(store, "lua_dispatch")
		if fn == nil {
			return -3
		}
		res, err := fn.Call(store, idx, frame, cl, nargs, want)
		if err != nil {
			t.Logf("wasm_dispatch trap (idx=%d): %v", idx, err)
			return -3
		}
		n, _ := res.(int32)
		return n
	}); err != nil {
		t.Fatal(err)
	}
	// M7a: hostfn seam — refusing stub (no host functions registered)
	if err := linker.DefineFunc(store, "host", "host_call",
		func(fnidx, argsPtr, argsLen, retPtr, retCap int32) int32 { return -1 }); err != nil {
		t.Fatal(err)
	}

	rtBin := lua51SjljWasm
	if p := os.Getenv("RTDBG"); p != "" {
		if b, e := os.ReadFile(p); e == nil {
			rtBin = b
		}
	}
	rtMod, err := wt.NewModule(engine, rtBin)
	if err != nil {
		t.Fatal(err)
	}
	rtInst, err := linker.Instantiate(store, rtMod)
	if err != nil {
		t.Fatal(err)
	}
	if err := linker.DefineInstance(store, "rt", rtInst); err != nil {
		t.Fatal(err)
	}
	guestMem = rtInst.GetExport(store, "memory").Memory()

	sm, err := wt.NewModule(engine, bin)
	if err != nil {
		t.Fatal(err)
	}
	si, err := linker.Instantiate(store, sm)
	if err != nil {
		t.Fatal(err)
	}
	scriptInst = si

	call := func(name string, args ...interface{}) (int32, error) {
		f := rtInst.GetFunc(store, name)
		if f == nil {
			return 0, fmt.Errorf("missing export %s", name)
		}
		for i, a := range args {
			switch v := a.(type) {
			case int:
				args[i] = int32(v)
			case uint64:
				args[i] = int32(v)
			}
		}
		res, err := f.Call(store, args...)
		if err != nil {
			return 0, err
		}
		switch v := res.(type) {
		case nil:
			return 0, nil
		case int32:
			return v, nil
		case int64:
			return int32(v), nil
		case float64:
			return int32(v), nil
		}
		return 0, fmt.Errorf("no result from %s", name)
	}

	L, err := call("lnewstate")
	if err != nil || L == 0 {
		t.Fatalf("lnewstate: %v %v", L, err)
	}
	if _, err := call("rt_set_state", L); err != nil {
		t.Fatal(err)
	}
	if v, err := call("rt_abi_version"); err != nil || v != 3 {
		t.Fatalf("rt_abi_version: %v %v", v, err)
	}

	// the wasm heap can grow (relocating) mid-run — read lazily
	memNow := func() []byte {
		return unsafe.Slice((*byte)(guestMem.Data(store)), guestMem.DataSize(store))
	}
	run := func(src string) int32 {
		in, err := call("linbuf")
		if err != nil {
			t.Fatal(err)
		}
		nm, err := call("lnamebuf")
		if err != nil {
			t.Fatal(err)
		}
		copy(memNow()[in:], src)
		copy(memNow()[nm:], "spike")
		st, err := call("ldostring", L, in, len(src), nm, 0)
		if err != nil {
			t.Fatalf("ldostring(%q): %v", src, err)
		}
		if st != 0 {
			if el, e := call("lerrlen"); e == nil && el > 0 && el < 4096 {
				if scr, e2 := call("rt_frame_alloc", 8192); e2 == nil && scr > 0 {
					mem := memNow()
					if n, e3 := call("lerrcopy", scr, 8192); e3 == nil && n > 0 && int(scr)+int(n) <= len(mem) {
						t.Logf("ldostring(%q) err: %s", src, string(mem[scr:int(scr)+int(n)]))
					}
				}
			}
		}
		return st
	}

	// GC stop — the M4 posture: frame cells are not GC roots.
	if st := run("collectgarbage('stop')"); st != 0 {
		t.Fatalf("gc stop: status %d", st)
	}

	// register the four protos: (idx, numparams, isvararg, nupvalues,
	// framecells) — scratch cells 12..14 must fit, so 16
	for i := 0; i < 4; i++ {
		np := int32(0)
		if i == 1 {
			np = 1 // g takes one param
		}
		if _, err := call("rt_wasm_proto", i, np, 0, 0, 16); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := call("rt_wasm_count"); err != nil || n != 4 {
		t.Fatalf("rt_wasm_count: %v %v", n, err)
	}

	// closures into globals f/g/h/f2
	base, err := call("rt_frame_alloc", 16*16)
	if err != nil || base == 0 {
		t.Fatalf("rt_frame_alloc: %v %v", base, err)
	}
	for i, name := range []string{"f", "g", "h", "f2"} {
		if _, err := call("rt_newclosure", base+int32(16*i), i, 0, 0, 0); err != nil {
			t.Fatalf("rt_newclosure %s: %v", name, err)
		}
		kc := base + int32(16*(8+i))
		copy(memNow()[kc:], name)
		if _, err := call("rt_intern", kc, kc, len(name)); err != nil {
			t.Fatalf("rt_intern %s: %v", name, err)
		}
		if _, err := call("rt_setglobal", kc, base+int32(16*i), 0); err != nil {
			t.Fatalf("rt_setglobal %s: %v", name, err)
		}
	}

	for _, src := range []string{
		"print(g(42))",
		"print(f())",
		"print(pcall(h))",
		"print(pcall(f2))",
	} {
		if st := run(src); st != 0 {
			t.Errorf("%q status %d (want 0)", src, st)
		}
	}

	want := []string{
		"PRINT\t42",         // value round-trip through the adapter
		"PRINT\t42",         // two-deep nesting: f → rt_call → g
		"PRINT\tfalse\tnil", // forced -1: the staged VALUE (nil), not bytes
		"PRINT\tfalse\tnil", // error through two adapter levels + pcall
	}
	got := []string{}
	for _, l := range lines {
		if len(l) >= 5 && l[:5] == "PRINT" {
			got = append(got, l)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("PRINT lines = %v (want %v)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("PRINT[%d] = %q (want %q)", i, got[i], want[i])
		}
	}
}
