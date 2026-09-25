package testdiff

// Trap isolation (note/m4-to-m5-plan.md §6.2): the engine traps with
// "uninitialized element" on the first global-touching script while the
// seam environment works. Add the engine's deltas to the seam environment
// one at a time and probe rt_getglobal for a MISSING global (the engine's
// failing case) after each.

import (
	"fmt"
	"os"
	"testing"

	wt "github.com/bytecodealliance/wasmtime-go/v48"
)

var _ = os.Getenv

// trapEnv = rtSetup clone with switchable deltas:
//
//	wasi: configure WASI (preopen cwd + stdout file) before instantiation
//	script: instantiate a script module bound to "rt"
//	init: call luawasm_init on the script instance
func trapEnvNamedBin(t *testing.T, wasi, script bool, chunkName string, binOverride []byte) (*wt.Store, *wt.Instance, *wt.Instance, func(*wt.Instance, string, ...interface{}) (interface{}, error)) {
	t.Helper()
	cfg := wt.NewConfig()
	cfg.SetWasmExceptions(true)
	engine := wt.NewEngineWithConfig(cfg)
	store := wt.NewStore(engine)
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
	// ABI v3 reverse seam (M5a): refusing stub, per the plan
	if err := linker.DefineFunc(store, "host", "wasm_dispatch",
		func(idx, frame, cl, nargs, want int32) int32 { return -3 }); err != nil {
		t.Fatal(err)
	}
	// M7a: hostfn seam — refusing stub (no host functions registered)
	if err := linker.DefineFunc(store, "host", "host_call",
		func(fnidx, argsPtr, argsLen, retPtr, retCap int32) int32 { return -1 }); err != nil {
		t.Fatal(err)
	}
	if wasi {
		w := wt.NewWasiConfig()
		if err := w.PreopenDir(t.TempDir(), "/", true); err != nil {
			t.Fatal(err)
		}
		tf, err := os.CreateTemp("", "trapiso-*")
		if err != nil {
			t.Fatal(err)
		}
		tf.Close()
		if err := w.SetStdoutFile(tf.Name()); err != nil {
			t.Fatal(err)
		}
		store.SetWasi(w)
	} else {
		store.SetWasi(wt.NewWasiConfig())
	}

	rtMod, err := wt.NewModule(engine, lua51SjljWasm)
	if err != nil {
		t.Fatal(err)
	}
	rtInst, err := linker.Instantiate(store, rtMod)
	if err != nil {
		t.Fatal(err)
	}
	var scriptInst *wt.Instance
	if script {
		if err := linker.DefineInstance(store, "rt", rtInst); err != nil {
			t.Fatal(err)
		}
		bin := binOverride
		if bin == nil {
			var err error
			bin, err = CompileSource([]byte("local y = x\n"), chunkName)
			if err != nil {
				t.Fatal(err)
			}
		}
		sm, err := wt.NewModule(engine, bin)
		if err != nil {
			t.Fatal(err)
		}
		scriptInst, err = linker.Instantiate(store, sm)
		if err != nil {
			t.Fatal(err)
		}
	}
	call := func(inst *wt.Instance, fn string, args ...interface{}) (interface{}, error) {
		f := inst.GetFunc(store, fn)
		if f == nil {
			return nil, errMissing(fn)
		}
		for i, a := range args {
			switch v := a.(type) {
			case uint64:
				args[i] = int32(v)
			case int:
				args[i] = int32(v)
			}
		}
		return f.Call(store, args...)
	}
	return store, rtInst, scriptInst, call
}

func errMissing(name string) error { return &missingExport{name} }

type missingExport struct{ name string }

func (m *missingExport) Error() string { return "missing export " + m.name }

// probeMissingGlobal: lnewstate → rt_set_state → frame cells → intern "x"
// → rt_getglobal(dst, key, line) for a MISSING global. Returns trap error
// or nil.
func probeMissingGlobal(t *testing.T, store *wt.Store, rtInst *wt.Instance, call func(*wt.Instance, string, ...interface{}) (interface{}, error)) error {
	t.Helper()
	L, err := call(rtInst, "lnewstate")
	if err != nil {
		return err
	}
	if _, err := call(rtInst, "rt_set_state", L); err != nil {
		return err
	}
	buf, err := call(rtInst, "rt_frame_alloc", 256)
	if err != nil {
		return err
	}
	// place "x" at buf+64 via host memory write, then intern from there
	if err := writeGuestMem(store, rtInst, uint32(mustUint(buf))+64, []byte("x")); err != nil {
		return err
	}
	if _, err := call(rtInst, "rt_intern", mustUint(buf), mustUint(buf)+64, 1); err != nil {
		return err
	}
	_, err = call(rtInst, "rt_getglobal", mustUint(buf)+16, mustUint(buf), 9)
	return err
}

func writeGuestMem(store *wt.Store, rtInst *wt.Instance, addr uint32, b []byte) error {
	data := rtInst.GetExport(store, "memory").Memory().UnsafeData(store)
	if int(addr)+len(b) > len(data) {
		return fmt.Errorf("memory bounds: addr=%x len=%d size=%d", addr, len(b), len(data))
	}
	copy(data[addr:], b)
	return nil
}

func mustUint(v interface{}) uint64 {
	if i, ok := v.(int32); ok {
		return uint64(uint32(i))
	}
	return 0
}

func TestTrapIsolation(t *testing.T) {
	skipBackendUntilU2e(t)
	cases := []struct {
		name         string
		wasi, script bool
	}{
		{"baseline (seam env)", false, false},
		{"+wasi", true, false},
		{"+script", false, true},
		{"+wasi+script", true, true},
	}
	for _, c := range cases {
		store, rtInst, scriptInst, call := trapEnvNamedBin(t, c.wasi, c.script, "iso.lua", nil)
		_ = scriptInst
		err := probeMissingGlobal(t, store, rtInst, call)
		status := "ok"
		if err != nil {
			msg := err.Error()
			if len(msg) > 80 {
				msg = msg[:80]
			}
			status = "TRAP: " + msg
		}
		t.Logf("%-20s %s", c.name, status)
	}

	// chunkname sensitivity: same failing script under several names
	for _, name := range []string{"iso.lua", "b.lua", "p.lua", "smoke.lua"} {
		func() {
			b, _ := CompileSource([]byte("local y = x\n"), name)
			store, rtInst, scriptInst, call := trapEnvNamedBin(t, true, true, name, b)
			_ = store
			L, err := call(rtInst, "lnewstate")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := call(rtInst, "rt_set_state", L); err != nil {
				t.Fatal(err)
			}
			if _, err := call(scriptInst, "luawasm_init", 2); err != nil {
				t.Logf("name=%-10s init TRAP", name)
				return
			}
			gv := scriptInst.GetExport(store, "gFrameCells").Global().Get(store)
			frame, err := call(rtInst, "rt_frame_alloc", int(gv.I32())*16)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := call(scriptInst, "lua_main", frame); err != nil {
				t.Logf("name=%-10s lua_main TRAP", name)
				return
			}
			if _, err := call(rtInst, "lglobals"); err != nil {
				t.Logf("name=%-10s lua_main ok; LGLOBALS TRAP: %v", name, firstLine(err.Error()))
				return
			}
			t.Logf("name=%-10s lua_main + lglobals ok", name)
		}()
	}

	// the full engine flow for `local y = x`, decomposed: init then main
	t.Run("engine-flow", func(t *testing.T) {
		store, rtInst, scriptInst, call := trapEnvNamedBin(t, true, true, "iso.lua", nil)
		_ = store
		L, err := call(rtInst, "lnewstate")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := call(rtInst, "rt_set_state", L); err != nil {
			t.Fatal(err)
		}
		if _, err := call(scriptInst, "luawasm_init", 2); err != nil {
			t.Logf("init: TRAP/err: %v", err)
			return
		}
		t.Log("init ok")
		fc, err := call(scriptInst, "luawasm_init") // placeholder: read frame cells global below
		_ = fc
		_ = err
		gv := scriptInst.GetExport(store, "gFrameCells").Global().Get(store)
		frameCells := gv.I32()
		frame, err := call(rtInst, "rt_frame_alloc", int(frameCells)*16)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := call(scriptInst, "lua_main", frame); err != nil {
			t.Logf("lua_main: TRAP: %v", err)
			return
		}
		t.Log("lua_main ok")
	})
}

func firstLine(s string) string {
	if i := indexByteStr(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

func indexByteStr(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
