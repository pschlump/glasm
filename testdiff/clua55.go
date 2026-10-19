package testdiff

// The Lua 5.5.1 oracle engines (U0, docs/Upgrade-from-5.2-to-5.5-of-Lua.md;
// decision D6): stock C Lua 5.5.1 compiled to lua55_sjlj.wasm
// (runtime/build55.sh) and hosted on BOTH engine hosts — wasmtime (CLua55)
// and wazero (CLua55Wazero). The blob runs on both hosts (ledger row 32's
// one-blob-two-hosts posture, first proven on the retired 5.1 oracle), so
// the U0 gate runs the official 5.5.1 suite on each host independently.
//
// The value protocol and event decoding live in cwire.go (the PT_* wire
// runtime/luawasm55.c emits), shared with the wasm/wazero engines.
//
// Differences from the 5.1 CLua engine, all following from the 5.5 blob:
//   - no host.random* imports — stock xoshiro256** runs in-process
//     (determinism pinned at build time; the interp engine grows the
//     matching xoshiro at U3)
//   - no host.wasm_dispatch / host.host_call imports — the blob is pure
//     stock C Lua, no rt_* ABI (that arrives with ABI v4 at U2/U6)
//   - SuiteFlags: optionally run the suite's own "user tests" posture
//     (all.lua: _soft/_port/_nomsg = true) before the chunk — skips
//     shell-spawning and long/memory-heavy blocks. Set only by the
//     conformance gate, never by cmd/clua55-run.

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"unsafe"

	wt "github.com/bytecodealliance/wasmtime-go/v48"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed lua55_sjlj.wasm
var lua55SjljWasm []byte

// suiteFlagsPrelude is all.lua's "user tests" posture (_U mode): _soft
// skips long/memory-hungry blocks, _port skips non-portable (shell-driving)
// blocks, _nomsg silences "test not performed" messages. Run as a separate
// chunk before the case source so line numbers — and therefore error
// positions — are unaffected.
const suiteFlagsPrelude = "_port=true _soft=true _nomsg=true"

// clua55Prep does the engine-independent run preparation: chdir into the
// case dir (scripts dofile/require relative files), schedule testdiff.tmp*
// cleanup, provision the WASI /dev/null stand-in, and reject Case.Args
// (the C driver installs arg = {[0]=chunkname} inside ldostring — no
// host-side window to add entries; §8.9 ledger discipline: skip with a
// reason rather than diverge).
func clua55Prep(c Case) ([]string, func(), error) {
	wd, _ := os.Getwd()
	if err := os.Chdir(c.Dir); err != nil {
		return nil, nil, err
	}
	// WASI preopens have no character devices. files.lua:467 writes and
	// flushes to /dev/null; an empty regular file inside the preopen root
	// is a faithful sink. (Re)created empty per run so writes never
	// accumulate, and ignored by git (_lua5.5-tests/.gitignore).
	devNull := c.Dir + "/dev/null"
	_ = os.MkdirAll(c.Dir+"/dev", 0o755)
	_ = os.WriteFile(devNull, nil, 0o644)
	cleanup := func() {
		os.Chdir(wd)
		_ = os.WriteFile(devNull, nil, 0o644)
		matches, _ := filepath.Glob(c.Dir + "/testdiff.tmp*")
		for _, m := range matches {
			os.Remove(m)
		}
	}
	if len(c.Args) > 0 {
		cleanup()
		return []string{"SKIP-UNSUPPORTED\tclua55 engine does not support Case.Args"}, nil, nil
	}
	return nil, cleanup, nil
}

// clua55Bad marks a case log as pass/fail for the conformance gate: a pass
// is "completes with no uncaught error and no engine failure" (the M2 gate
// wording, unchanged).
func clua55Bad(log []string) string {
	for _, l := range log {
		if strings.HasPrefix(l, "ERROR\t") || strings.HasPrefix(l, "ENGINE-ERROR\t") {
			return l
		}
	}
	return ""
}

// CLua55 is the stock-C-Lua-5.5.1-in-wasm oracle engine on wasmtime.
type CLua55 struct {
	name string
	// SuiteFlags: run the _port/_soft/_nomsg prelude (conformance gate only)
	SuiteFlags bool
}

// NewCLua55 returns a wasmtime-hosted Lua 5.5.1 oracle engine.
func NewCLua55(name string) *CLua55 { return &CLua55{name: name} }

func (e *CLua55) Name() string { return e.name }

func (e *CLua55) Run(c Case) []string {
	var log []string
	emit := func(event, payload string) {
		log = append(log, event+"\t"+payload)
	}

	early, cleanup, err := clua55Prep(c)
	if err != nil {
		return []string{fmt.Sprintf("ENGINE-ERROR\tchdir %s: %v", c.Dir, err)}
	}
	if early != nil {
		return early
	}
	defer cleanup()

	cfg := wt.NewConfig()
	cfg.SetWasmExceptions(true) // EH-based setjmp (same dialect as the 5.1 blob)
	engine := wt.NewEngineWithConfig(cfg)
	store := wt.NewStore(engine)
	module, err := wt.NewModule(engine, lua55SjljWasm)
	if err != nil {
		return []string{"ENGINE-ERROR\tmodule: " + err.Error()}
	}
	linker := wt.NewLinker(engine)
	if err := linker.DefineWasi(); err != nil {
		return []string{"ENGINE-ERROR\twasi: " + err.Error()}
	}

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

	if err := linker.DefineFunc(store, "host", "event",
		func(kind, ptr, length int32) {
			if raw := memRead(ptr, length); raw != nil && length > 0 {
				onEvent(raw, kind, emit)
			}
		}); err != nil {
		return []string{"ENGINE-ERROR\thost event: " + err.Error()}
	}

	wasi := wt.NewWasiConfig()
	if err := wasi.PreopenDir(c.Dir, "/", true); err != nil {
		return []string{"ENGINE-ERROR\tpreopen: " + err.Error()}
	}
	stdoutFile, err := os.CreateTemp("", "clua55-stdout-*")
	if err != nil {
		return []string{"ENGINE-ERROR\tstdout tmp: " + err.Error()}
	}
	stdoutFile.Close()
	defer os.Remove(stdoutFile.Name())
	if err := wasi.SetStdoutFile(stdoutFile.Name()); err != nil {
		return []string{"ENGINE-ERROR\tstdout: " + err.Error()}
	}
	store.SetWasi(wasi)

	inst, err := linker.Instantiate(store, module)
	if err != nil {
		return []string{"ENGINE-ERROR\tinstantiate: " + err.Error()}
	}
	if init := inst.GetFunc(store, "_initialize"); init != nil {
		if _, err := init.Call(store); err != nil {
			return []string{"ENGINE-ERROR\tinitialize: " + err.Error()}
		}
	}
	guestMem = inst.GetExport(store, "memory").Memory()

	call := func(fn string, args ...interface{}) (uint64, error) {
		f := inst.GetFunc(store, fn)
		if f == nil {
			return 0, fmt.Errorf("clua55: missing export %s", fn)
		}
		for i, a := range args {
			switch v := a.(type) {
			case uint64:
				args[i] = int32(v)
			case int:
				args[i] = int32(v)
			}
		}
		res, err := f.Call(store, args...)
		if err != nil {
			return 0, err
		}
		switch v := res.(type) {
		case nil:
			return 0, fmt.Errorf("clua55: %s returned no result", fn)
		case int32:
			return uint64(uint32(v)), nil
		case int64:
			return uint64(v), nil
		default:
			return 0, fmt.Errorf("clua55: %s returned %T", fn, res)
		}
	}
	writeMem := func(ptr uint64, b []byte) bool {
		if guestMem == nil {
			return false
		}
		base := guestMem.Data(store)
		size := guestMem.DataSize(store)
		if uintptr(ptr)+uintptr(len(b)) > size {
			return false
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(base)+uintptr(ptr))), len(b)), b)
		return true
	}

	L, err := call("lnewstate")
	if err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	if e.SuiteFlags {
		if _, err := ldostring55(call, writeMem, L, suiteFlagsPrelude, "=u0-suite-flags", 0); err != nil {
			return append(log, "ENGINE-ERROR\t"+err.Error())
		}
	}
	status, err := ldostring55(call, writeMem, L, string(c.Source), c.Name, 1)
	if err != nil {
		// A trap is always a bug in the port (§8.7), reported as such.
		return append(log, fmt.Sprintf("ENGINE-ERROR\ttrap: %v", err))
	}
	if status != 0 {
		errLen, _ := call("lerrlen")
		if errLen > 0 {
			inAddr, _ := call("linbuf")
			if _, err := call("lerrcopy", inAddr, errLen); err == nil {
				emit("ERROR", strconv.Quote(string(memRead(int32(inAddr), int32(errLen)))))
			}
		} else {
			emit("ERROR", `""`)
		}
	}
	// Ledger row 29 (carried over): best-effort io.flush in the live state
	// before lclose — lclose returns without libc exit(), so the atexit
	// stdout flush never runs and io.write's buffered tail is lost.
	_, _ = ldostring55(call, writeMem, L, "io.flush()", "=flush", 0)
	call("lclose", L)
	if out, err := os.ReadFile(stdoutFile.Name()); err == nil && len(out) > 0 {
		for i := 0; i < len(out); {
			j := indexByte(out[i:], '\n')
			if j < 0 {
				emit("STDOUT", string(out[i:]))
				break
			}
			emit("STDOUT", string(out[i:i+j]))
			i += j + 1
		}
	}
	return log
}

// ldostring55 stages src+chunkname and runs ldostring on state L — shared
// by the prelude, the case, and the flush drain, on both hosts' closures.
// The chunkname write carries its own NUL: namebuf is reused across calls,
// and a shorter name over a longer stale one would leave a garbage tail
// (found as "attrib.luaflags" — the '=u0-suite-flags' prelude's tail —
// breaking every source/match assertion on chunknames).
func ldostring55(call func(string, ...interface{}) (uint64, error),
	writeMem func(uint64, []byte) bool, L uint64,
	src, chunkname string, wantGlobals uint32) (uint64, error) {
	inAddr, err := call("linbuf")
	if err != nil {
		return 0, err
	}
	nameAddr, err := call("lnamebuf")
	if err != nil {
		return 0, err
	}
	name := chunkname
	if len(name) > 500 {
		name = name[:500]
	}
	if !writeMem(nameAddr, append([]byte(name), 0)) || !writeMem(inAddr, []byte(src)) {
		return 0, fmt.Errorf("staging write failed")
	}
	return call("ldostring", L, inAddr, uint64(len(src)), nameAddr, uint64(wantGlobals))
}

// CLua55Wazero is the same oracle blob hosted on wazero (the pure-Go
// engine) — the U0 gate's second host: wazero v1.12 implements the
// standardized exception-handling proposal the SJLJ blob is built in
// (experimental.CoreFeaturesExceptionHandling, as the wasm-backend engine
// does for the rt blob).
type CLua55Wazero struct {
	name string
	// SuiteFlags: run the _port/_soft/_nomsg prelude (conformance gate only)
	SuiteFlags bool
}

// NewCLua55Wazero returns a wazero-hosted Lua 5.5.1 oracle engine.
func NewCLua55Wazero(name string) *CLua55Wazero { return &CLua55Wazero{name: name} }

func (e *CLua55Wazero) Name() string { return e.name }

func (e *CLua55Wazero) Run(c Case) (log []string) {
	defer func() {
		if p := recover(); p != nil {
			log = []string{fmt.Sprintf("ENGINE-PANIC\t%v\t%s", p, debug.Stack())}
		}
	}()
	emit := func(event, payload string) {
		log = append(log, event+"\t"+payload)
	}

	early, cleanup, err := clua55Prep(c)
	if err != nil {
		return []string{fmt.Sprintf("ENGINE-ERROR\tchdir %s: %v", c.Dir, err)}
	}
	if early != nil {
		return early
	}
	defer cleanup()

	ctx := context.Background()
	rc := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling)
	r := wazero.NewRuntimeWithConfig(ctx, rc)
	defer r.Close(ctx)

	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	var stdout bytes.Buffer
	_, err = r.NewHostModuleBuilder("host").
		NewFunctionBuilder().WithFunc(func(kind, ptr, length int32) {
		if m := r.Module("rt"); m != nil {
			if raw, ok := m.Memory().Read(uint32(ptr), uint32(length)); ok && length > 0 {
				onEvent(raw, kind, emit)
			}
		}
	}).Export("event").
		Instantiate(ctx)
	if err != nil {
		return []string{"ENGINE-ERROR\thost module: " + err.Error()}
	}

	rtInst, err := r.InstantiateWithConfig(ctx, lua55SjljWasm,
		wazero.NewModuleConfig().WithName("rt").
			WithStdout(&stdout).WithStderr(io.Discard).
			WithFSConfig(wazero.NewFSConfig().WithDirMount(c.Dir, "/")))
	if err != nil {
		return []string{"ENGINE-ERROR\trt instantiate: " + err.Error()}
	}
	// Reactor libc init (.init_array ctors). wazero auto-runs only the wasm
	// start section, which reactor modules don't use — call _initialize
	// explicitly like the wasmtime leg does. (Modern wasi-libc also
	// initializes preopens lazily, so this is belt-and-suspenders — but the
	// reactor contract is the contract.)
	if init := rtInst.ExportedFunction("_initialize"); init != nil {
		if _, err := init.Call(ctx); err != nil {
			return []string{"ENGINE-ERROR\tinitialize: " + err.Error()}
		}
	}
	mem := rtInst.Memory()

	call := func(fn string, args ...uint64) (uint64, error) {
		f := rtInst.ExportedFunction(fn)
		if f == nil {
			return 0, fmt.Errorf("clua55: missing export %s", fn)
		}
		res, err := f.Call(ctx, args...)
		if err != nil || len(res) == 0 {
			return 0, err
		}
		return res[0], nil
	}
	writeMem := func(ptr uint64, b []byte) bool {
		return mem.Write(uint32(ptr), b)
	}

	L, err := call("lnewstate")
	if err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	ldostring := func(src, chunkname string, wantGlobals uint32) (uint64, error) {
		return ldostring55u(call, writeMem, L, src, chunkname, wantGlobals)
	}
	if e.SuiteFlags {
		if _, err := ldostring(suiteFlagsPrelude, "=u0-suite-flags", 0); err != nil {
			return append(log, "ENGINE-ERROR\t"+err.Error())
		}
	}
	status, err := ldostring(string(c.Source), c.Name, 1)
	if err != nil {
		return append(log, fmt.Sprintf("ENGINE-ERROR\ttrap: %v", err))
	}
	if status != 0 {
		errLen, _ := call("lerrlen")
		if errLen > 0 {
			inAddr, _ := call("linbuf")
			if _, err := call("lerrcopy", inAddr, errLen); err == nil {
				if raw, ok := mem.Read(uint32(inAddr), uint32(errLen)); ok {
					emit("ERROR", strconv.Quote(string(raw)))
				}
			}
		} else {
			emit("ERROR", `""`)
		}
	}
	// Ledger row 29 (carried over): drain io.write's buffered tail.
	_, _ = ldostring("io.flush()", "=flush", 0)
	_, _ = call("lclose", L)
	if out := stdout.Bytes(); len(out) > 0 {
		for i := 0; i < len(out); {
			j := indexByte(out[i:], '\n')
			if j < 0 {
				emit("STDOUT", string(out[i:]))
				break
			}
			emit("STDOUT", string(out[i:i+j]))
			i += j + 1
		}
	}
	return log
}

// ldostring55u is ldostring55 over the wazero-shaped call/writeMem closures
// (uint64 args only; same NUL-terminated chunkname rule).
func ldostring55u(call func(string, ...uint64) (uint64, error),
	writeMem func(uint64, []byte) bool, L uint64,
	src, chunkname string, wantGlobals uint32) (uint64, error) {
	inAddr, err := call("linbuf")
	if err != nil {
		return 0, err
	}
	nameAddr, err := call("lnamebuf")
	if err != nil {
		return 0, err
	}
	name := chunkname
	if len(name) > 500 {
		name = name[:500]
	}
	if !writeMem(nameAddr, append([]byte(name), 0)) || !writeMem(inAddr, []byte(src)) {
		return 0, fmt.Errorf("staging write failed")
	}
	return call("ldostring", L, inAddr, uint64(len(src)), nameAddr, uint64(wantGlobals))
}
