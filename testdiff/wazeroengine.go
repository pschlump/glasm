package testdiff

// The wasm-backend engine hosted on wazero (M6a): the pure-Go production
// engine (design A8). Hosts the SAME SJLJ/new-EH runtime blob as the
// wasmtime engine — wazero v1.12 implements the standardized
// exception-handling proposal behind
// experimental.CoreFeaturesExceptionHandling, and the blob is built in that
// dialect (-mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false). The
// M5 error-safety invariant (no longjmp/throw ever crosses a host or script
// boundary; compiled code returns -1 + staged error and the adapter
// re-raises after the dispatch returns) means EH unwinding stays inside the
// blob's own frames — the host never has to propagate an exception across
// its own call sites.
//
// The protocol mirrors wasmengine.go (the wasmtime host): shared memory
// owned by the blob, host.event/random*/wasm_dispatch imports, the M2
// driver exports (lnewstate/ldostring/lglobals...), gc-stop + arg-setup
// ldostrings, rt_set_dialect(1), luawasm_init, rt_frame_alloc, lua_main,
// staged-error readback, io.flush before reading the stdout capture. stdout
// is a bytes.Buffer (wazero WASI WithStdout) instead of a temp file.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/pschlump/golua55/wasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

func u32[T int32 | int | uint32](v T) uint64 { return uint64(uint32(v)) }

// deadlineFlagLE is the watchdog's single store: 1 as 4 little-endian
// bytes, the encoding of rt_ctrl_addr()'s g_deadline_flag (U6b, row 37).
var deadlineFlagLE = []byte{1, 0, 0, 0}

// NewRunEngine returns the wasm-backend engine for the standalone runners
// (glua -W, luawasm-run): the wasmtime differential oracle by default, or
// the wazero production engine (ledger row 32 — one blob, two hosts) when
// GLUA_WASM_ENGINE=wazero. The _cli-tests WAZERO=1 leg selects it so the
// black-box suite runs its wasm legs on the production engine unchanged.
func NewRunEngine(name string, precompiled []byte) Engine {
	if strings.EqualFold(os.Getenv("GLUA_WASM_ENGINE"), "wazero") {
		e := NewWazeroEngine(name)
		e.Precompiled = precompiled
		return e
	}
	e := NewWasmEngine(name)
	e.Precompiled = precompiled
	return e
}

// WazeroEngine compiles and executes scripts through the wasm backend on
// wazero (the production engine). Construction mirrors WasmEngine.
type WazeroEngine struct {
	name string
	// NoopHosts: replace host shims with no-ops (trap triage)
	NoopHosts bool
	// Precompiled module bytes (nil → compile from source)
	Precompiled []byte
	// SkipUnsupported: scripts using v1-unsupported opcodes produce a
	// SKIP-UNSUPPORTED log instead of an engine error (corpus tests)
	SkipUnsupported bool
	// RtBin overrides the runtime blob (nil → embedded lua51_sjlj.wasm;
	// the RTWASM env var still wins). The M6c gates pass lua51ProdWasm.
	RtBin []byte
	// Sandbox: call rt_sandbox(1) after rt_set_state — the M6c globals
	// lockdown (ledger row 33) — and skip the io.flush drain (io is nil).
	Sandbox bool
	// MaxMem: per-VM allocation budget in bytes (0 = unlimited), via
	// rt_set_memlimit before lnewstate (M6d D3, ledger row 36).
	MaxMem int64
	// Deadline: host timeout — the watchdog writes the rt control-block
	// flag and the guest's back-edge polls raise the error (M6d D4).
	Deadline time.Duration
}

// NewWazeroEngine returns a wazero-hosted wasm-backend engine.
func NewWazeroEngine(name string) *WazeroEngine { return &WazeroEngine{name: name} }

// UseProdBlob switches the engine to the embedded production blob with the
// sandbox lockdown — the M6c "wazero-prod" configuration (ledger rows
// 33-34): zero wasi imports, rt_sandbox(1) globals surface.
func (e *WazeroEngine) UseProdBlob() {
	e.RtBin = lua51ProdWasm
	e.Sandbox = true
}

// moduleConfig wires the rt module's host-side stdio/fs surface: only a
// WASI-importing blob gets the stdout capture and the corpus dir mount —
// the prod blob gets neither (nothing to capture; no fs to mount).
func moduleConfig(c Case, wasi bool, stdout *bytes.Buffer) wazero.ModuleConfig {
	cfg := wazero.NewModuleConfig().WithName("rt")
	if !wasi {
		return cfg
	}
	return cfg.
		WithStdout(stdout).WithStderr(stdout).
		WithFSConfig(wazero.NewFSConfig().WithDirMount(c.Dir, "/"))
}

func (e *WazeroEngine) Name() string { return e.name }

func (e *WazeroEngine) Run(c Case) (log []string) {
	defer func() {
		if p := recover(); p != nil {
			log = []string{fmt.Sprintf("ENGINE-PANIC\t%v\t%s", p, debug.Stack())}
		}
	}()
	emit := func(event, payload string) {
		log = append(log, event+"\t"+payload)
	}

	bin := e.Precompiled
	if bin == nil {
		var err error
		// U1: '@'-sigil chunk names (see wasmengine.go)
		bin, err = CompileSource(c.Source, "@"+c.Name)
		if err != nil {
			if e.SkipUnsupported && strings.Contains(err.Error(), "backend v1") {
				return []string{"SKIP-UNSUPPORTED\t" + err.Error()}
			}
			return []string{"ERROR\t" + strconv.Quote(err.Error())}
		}
	}

	ctx := context.Background()
	rc := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling)
	r := wazero.NewRuntimeWithConfig(ctx, rc)
	defer r.Close(ctx)

	// U2e: the default blob is the ABI v4 rt flavor on the stock 5.5 tree
	// (lua55_rt.wasm); WASI is wired only when the blob actually imports
	// it. The prod flavor is 5.1-v3 until U6.
	rtBin := lua55RtWasm
	if e.RtBin != nil {
		rtBin = e.RtBin
	}
	if p := os.Getenv("RTWASM"); p != "" {
		if b, e := os.ReadFile(p); e == nil {
			rtBin = b
		}
	}
	blobNeedsWASI, err := wasm.HasWASIImports(rtBin)
	if err != nil {
		return []string{"ENGINE-ERROR\trt blob parse: " + err.Error()}
	}
	if blobNeedsWASI {
		wasi_snapshot_preview1.MustInstantiate(ctx, r)
	}

	var stdout bytes.Buffer // wasi fd_write capture (replaces the temp file)
	decoder := &CLua{}

	// host.wasm_dispatch: the M5a re-entrancy seam — the blob's precall
	// adapter dispatches compiled protos through the host into the script
	// module's lua_dispatch (same goroutine, nested call). A trap inside
	// the dispatch must not unwind through wasm: surface as a refused
	// dispatch (-3); the adapter re-raises it as a Lua error. U5e (row 20
	// family): a stack-exhaustion trap maps to RTW_STACKTRAP instead —
	// the blob stages its clean catchable "stack overflow".
	var scriptInst api.Module
	dispatch := func(idx, frame, cl, nargs, want int32) int32 {
		if scriptInst == nil {
			return -3
		}
		fn := scriptInst.ExportedFunction("lua_dispatch")
		if fn == nil {
			return -3
		}
		res, err := fn.Call(ctx, u32(idx), u32(frame), u32(cl), u32(nargs), u32(want))
		if err != nil {
			if strings.Contains(err.Error(), "call stack exhausted") {
				return rtwStackTrap
			}
			return -3
		}
		return int32(res[0])
	}

	// U3b: no host.random* imports — stock xoshiro math.random runs in
	// the blob over the makeseed=0 pin (the D5 host-RNG shim retired).
	eventFn := func(kind, ptr, length int32) {
		if m := r.Module("rt"); m != nil {
			if raw, ok := m.Memory().Read(uint32(ptr), uint32(length)); ok {
				decoder.onEvent(raw, kind, emit)
			}
		}
	}
	if e.NoopHosts {
		eventFn = func(kind, ptr, length int32) {}
	}
	_, err = r.NewHostModuleBuilder("host").
		NewFunctionBuilder().WithFunc(eventFn).Export("event").
		NewFunctionBuilder().WithFunc(dispatch).Export("wasm_dispatch").
		NewFunctionBuilder().WithFunc(func(fnidx, argsPtr, argsLen, retPtr, retCap int32) int32 {
		// M7a: the hostfn seam is a host-package feature; this engine
		// refuses (the guest raises the staged error value).
		if m := r.Module("rt"); m != nil {
			_ = m.Memory().Write(uint32(retPtr), wireErrHostFns)
		}
		return -1
	}).Export("host_call").
		Instantiate(ctx)
	if err != nil {
		return []string{"ENGINE-ERROR\thost module: " + err.Error()}
	}

	rtInst, err := r.InstantiateWithConfig(ctx, rtBin,
		moduleConfig(c, blobNeedsWASI, &stdout))
	if err != nil {
		return []string{"ENGINE-ERROR\trt instantiate: " + err.Error()}
	}
	mem := rtInst.Memory()

	scriptInst, err = r.InstantiateWithConfig(ctx, bin, wazero.NewModuleConfig().WithName("script"))
	if err != nil {
		return []string{"ENGINE-ERROR\tscript instantiate: " + err.Error()}
	}

	call := func(mod api.Module, fn string, args ...uint64) ([]uint64, error) {
		f := mod.ExportedFunction(fn)
		if f == nil {
			return nil, fmt.Errorf("missing export %s", fn)
		}
		return f.Call(ctx, args...)
	}

	// U6b (rows 36/93, M6d D3 back on ABI v4): the per-VM allocation cap
	// — before lnewstate, so the whole VM lifetime counts.
	if e.MaxMem > 0 {
		if _, err := call(rtInst, "rt_set_memlimit", u32(int32(e.MaxMem))); err != nil {
			return []string{"ENGINE-ERROR\trt_set_memlimit: " + err.Error()}
		}
	}

	L, err := call(rtInst, "lnewstate")
	if err != nil {
		return []string{"ENGINE-ERROR\tlnewstate: " + err.Error()}
	}
	Lv := int32(L[0])
	ldostring := func(src, chunkname string, nres uint32) error {
		in, err := call(rtInst, "linbuf")
		if err != nil {
			return err
		}
		nameA, err := call(rtInst, "lnamebuf")
		if err != nil {
			return err
		}
		inAddr, nameAddr := uint32(in[0]), uint32(nameA[0])
		if !mem.Write(inAddr, []byte(src)) || !mem.Write(nameAddr, []byte(chunkname)) {
			return fmt.Errorf("ldostring buffer write OOB")
		}
		_, err = call(rtInst, "ldostring", u32(Lv), u32(inAddr), u32(len(src)), u32(nameAddr), u32(nres))
		return err
	}
	// the ABI operates on this state (curL in rt_abi.c) — without it,
	// NULL-derefs land in mapped page 0 and surface as wild call_indirects
	if _, err := call(rtInst, "rt_set_state", u32(Lv)); err != nil {
		return []string{"ENGINE-ERROR\trt_set_state: " + err.Error()}
	}
	// M6c: rt_sandbox is v3-only until U6 (row 60); refuse rather than
	// run unsandboxed.
	if e.Sandbox {
		return []string{"ENGINE-ERROR\tsandbox is not on the ABI v4 blob until U6 (row 60)"}
	}
	// v1 GC stop: register cells are not GC roots (the M3 ABI obligation);
	// the M6 cap+fresh-VM posture (ledger row 11) replaces arena lifecycle
	if err := ldostring("collectgarbage('stop')", c.Name, 1); err != nil {
		return []string{"ENGINE-ERROR\tgc stop: " + err.Error()}
	}
	// the CLI arg surface (after gcStop, which resets arg = {[0]=name})
	argSetup := "arg={[0]=" + luaQuote(strings.TrimPrefix(c.Name, "@"))
	for _, a := range c.Args {
		argSetup += "," + luaQuote(a)
	}
	argSetup += "}"
	if err := ldostring(argSetup, "=arg", 0); err != nil {
		return []string{"ENGINE-ERROR\targ setup: " + err.Error()}
	}
	// U2e: no dialect layer on the v4 blob — stock 5.5 texts, which is
	// what the interp oracle was re-pinned to (row 56)
	av, err := call(rtInst, "rt_abi_version")
	if err != nil {
		return []string{"ENGINE-ERROR\tABI version: " + err.Error()}
	}
	if int32(av[0]) != 4 {
		return []string{fmt.Sprintf("ENGINE-ERROR\tABI version %d (need 4)", int32(av[0]))}
	}
	if _, err := call(scriptInst, "luawasm_init", 2); err != nil {
		return append(log, "ENGINE-ERROR\tinit: "+err.Error())
	}
	frameCells := int32(scriptInst.ExportedGlobal("gFrameCells").Get())
	frame, err := call(rtInst, "rt_frame_alloc", u32(frameCells*16))
	if err != nil {
		return []string{"ENGINE-ERROR\tframe: " + err.Error()}
	}

	// U6b (row 37, the M6d D4 watchdog back on ABI v4): a bare 4-byte
	// store into the linear memory at the control-block flag (wazero's
	// Memory.Write is a plain buffer copy; no engine state touched, safe
	// off-thread). The guest's basic-block-transition polls raise
	// rt_deadline's ordinary error.
	if e.Deadline > 0 {
		if a, err := call(rtInst, "rt_ctrl_addr"); err == nil {
			flagAddr := uint32(a[0])
			timer := time.AfterFunc(e.Deadline, func() {
				_ = mem.Write(flagAddr, deadlineFlagLE)
			})
			defer timer.Stop()
		}
	}

	status, err := call(scriptInst, "lua_main", frame[0])
	if err != nil {
		return append(log, "ENGINE-ERROR\ttrap: "+err.Error())
	}
	// M7a: lua_main returns the dispatch status verbatim — nret ≥ 0 or a
	// negative error code (want=-1 multret; the count survives the entry)
	if int32(status[0]) < 0 {
		// staged error: message bytes, or — for non-string error values —
		// the exact TValue rendered (matches the wasmtime engine's path)
		emitted := false
		if buf, err := call(rtInst, "rt_frame_alloc", 4096); err == nil {
			if n, err := call(rtInst, "rt_err_stage_copy", buf[0], 4096); err == nil && int32(n[0]) > 0 {
				if raw, ok := mem.Read(uint32(buf[0]), uint32(int32(n[0]))); ok {
					emit("ERROR", strconv.Quote(string(raw)))
					emitted = true
				}
			}
		}
		if !emitted {
			if vp, err := call(rtInst, "rt_err_value_ptr"); err == nil {
				if raw, ok := mem.Read(uint32(vp[0]), 16); ok {
					emit("ERROR", strconv.Quote(renderErrValue(raw)))
				}
			}
		}
	}
	if _, err := call(rtInst, "lglobals"); err != nil {
		emit("GLOBALS", "<lglobals failed: "+err.Error()+">")
	}
	// Ledger row 29: best-effort io.flush before reading the capture (the
	// driver returns without libc exit(), so the atexit flush never runs).
	// Skipped under Sandbox — io is nil there, the drain is a no-op error.
	if !e.Sandbox {
		_ = ldostring("io.flush()", "=flush", 0)
	}

	if out := stdout.Bytes(); len(out) > 0 {
		for _, line := range bytes.Split(bytes.TrimRight(out, "\n"), []byte("\n")) {
			emit("STDOUT", string(line))
		}
	}
	return log
}
