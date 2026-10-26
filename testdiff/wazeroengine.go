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
// owned by the blob, host.{event,wasm_dispatch,host_call} imports, the M2
// driver exports (lnewstate/ldostring/lglobals...), gc-stop + arg-setup
// ldostrings, luawasm_init, rt_frame_alloc, lua_main,
// staged-error readback, io.flush before reading the stdout capture. stdout
// is a bytes.Buffer (wazero WASI WithStdout) instead of a temp file.
// (No rt_set_dialect on v4 — the dialect layer retired at U3/row 56, when
// the interp oracle was re-pinned to stock texts.)

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/pschlump/glasm/wasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// lua55_prod.wasm — the zero-wasi production flavor of the 5.5 runtime
// (U6d, ledger row 95): base/coroutine/string/table/math/utf8 with the
// loaders compiled out, rebuilt by runtime/build55.sh and byte-compared
// against host/'s embedded copy (m6c_test.go — the embed-dance drift
// lock). Lived in clua.go until the U8 retirement of the 5.1 tree.
//
//go:embed lua55_prod.wasm
var lua55ProdWasm []byte

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
	// RtBin overrides the runtime blob (nil → embedded lua55_rt.wasm;
	// the RTWASM env var still wins). The prod legs pass lua55ProdWasm
	// (UseProdBlob).
	RtBin []byte
	// Sandbox: call rt_sandbox(1) after rt_set_state — the M6c globals
	// lockdown (ledger row 33; U6c re-pins the list on 5.5 — ledger row
	// 94) — and skip the io.flush drain (io is nil).
	Sandbox bool
	// Protect: call rt_protect_globals(1) after every other host write
	// into _G — the Redis 7.2.7 script-env lockdown: missing-global reads
	// raise, and every table reachable from _G (plus the string
	// metatable) rejects writes (U6c, ledger row 94).
	Protect bool
	// MaxMem: per-VM allocation budget in bytes (0 = unlimited), via
	// rt_set_memlimit before lnewstate (M6d D3, ledger row 36).
	MaxMem int64
	// Deadline: host timeout — the watchdog writes the rt control-block
	// flag and the guest's back-edge polls raise the error (M6d D4).
	Deadline time.Duration
	// CacheDir: persist the blob's compiled machine code under this dir
	// (the WithCompileCacheDir posture — perfcmp's wazero-disk leg): a
	// fresh process mmaps it in ~6 ms instead of paying the ~75 ms JIT.
	// "" (the default) compiles in-memory per engine, unless
	// GLUA_WASM_CACHE_DIR is set — honored in Run like RTWASM, so the
	// gate corpora run the disk posture with one env var; the field wins
	// when both are given.
	CacheDir string
}

// NewWazeroEngine returns a wazero-hosted wasm-backend engine.
func NewWazeroEngine(name string) *WazeroEngine { return &WazeroEngine{name: name} }

// UseProdBlob switches the engine to the embedded production blob with the
// sandbox lockdown — the U6d "wazero-prod" configuration (ledger rows
// 33-34 re-pinned, row 95): lua55_prod.wasm (the 5.5 flavor), zero wasi
// imports, the §4.3 compiled-down surface — rt_sandbox(1) is the
// belt-and-suspenders no-op returning 0.
func (e *WazeroEngine) UseProdBlob() {
	e.RtBin = lua55ProdWasm
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
	// CacheDir: the engine's machine code persists under the dir, so the
	// next process (or the next engine over the same dir) loads it
	// instead of re-JIT-ing the 659-function blob. The env is the same
	// posture opt-in for every engine instance — the corpus gates set it
	// to sweep the disk posture over the corpora.
	var wcache wazero.CompilationCache
	cacheDir := e.CacheDir
	if cacheDir == "" {
		cacheDir = os.Getenv("GLUA_WASM_CACHE_DIR")
	}
	if cacheDir != "" {
		var err error
		wcache, err = wazero.NewCompilationCacheWithDir(cacheDir)
		if err != nil {
			return []string{"ENGINE-ERROR\tcompile cache: " + err.Error()}
		}
		rc = rc.WithCompilationCache(wcache)
		// registered BEFORE r.Close's defer, so it runs AFTER it (defers
		// are LIFO): the runtime and its instances must be closed before
		// the cache frees the machine code they still point at
		defer wcache.Close(ctx)
	}
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

	// host.wasm_dispatch: the M5a re-entrancy seam — the blob's precall
	// adapter dispatches compiled protos through the host into the script
	// module's lua_dispatch (same goroutine, nested call). A trap inside
	// the dispatch must not unwind through wasm: surface as a refused
	// dispatch (-3); the adapter re-raises it as a Lua error. U5e (row 20
	// family): a stack-exhaustion trap maps to RTW_STACKTRAP instead —
	// the blob stages its clean catchable "stack overflow".
	var scriptInst api.Module
	// U9b: depth-indexed pool of lua_dispatch handles + arg slices. Each
	// ExportedFunction lookup builds a fresh wazevo callEngine (private
	// execCtx + machine stack) — the old per-call lookup was the seam's
	// dominant allocation — but a callEngine is single-flight, so nested
	// dispatches on this goroutine must take DIFFERENT handles (one
	// shared handle SIGBUSed mid-JIT). Level 0 serves the common
	// non-nested case; sequential reuse of a handle is the documented
	// CallWithStack pattern.
	var dispatchFns []api.Function
	var dispatchStacks [][]uint64
	dispatchDepth := 0
	dispatch := func(idx, frame, cl, nargs, want int32) int32 {
		if scriptInst == nil {
			return -3
		}
		d := dispatchDepth
		if d == len(dispatchFns) {
			fn := scriptInst.ExportedFunction("lua_dispatch")
			if fn == nil {
				return -3
			}
			dispatchFns = append(dispatchFns, fn)
			dispatchStacks = append(dispatchStacks, make([]uint64, 5))
		}
		fn, st := dispatchFns[d], dispatchStacks[d]
		dispatchDepth = d + 1
		defer func() { dispatchDepth = d }()
		st[0] = u32(idx)
		st[1] = u32(frame)
		st[2] = u32(cl)
		st[3] = u32(nargs)
		st[4] = u32(want)
		if err := fn.CallWithStack(ctx, st); err != nil {
			if strings.Contains(err.Error(), "call stack exhausted") {
				return rtwStackTrap
			}
			return -3
		}
		return int32(st[0])
	}

	// U3b: no host.random* imports — stock xoshiro math.random runs in
	// the blob over the makeseed=0 pin (the D5 host-RNG shim retired).
	eventFn := func(kind, ptr, length int32) {
		if m := r.Module("rt"); m != nil {
			if raw, ok := m.Memory().Read(uint32(ptr), uint32(length)); ok {
				onEvent(raw, kind, emit)
			}
		}
	}
	if e.NoopHosts {
		eventFn = func(kind, ptr, length int32) {}
	}
	// Row 100 (U7a): the STW-proof backstop's engine-side answer, wired
	// at the deadline-arming site below (the host module instantiates
	// before rt/memory exist to close over). Nil = no deadline armed →
	// constant 0; the guest's periodic poll then costs one import call.
	var deadlinePoll func() int32
	_, err = r.NewHostModuleBuilder("host").
		NewFunctionBuilder().WithFunc(eventFn).Export("event").
		NewFunctionBuilder().WithFunc(dispatch).Export("wasm_dispatch").
		NewFunctionBuilder().WithFunc(func() int32 {
		if deadlinePoll == nil {
			return 0
		}
		return deadlinePoll()
	}).Export("deadline_poll").
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
	// U6c (ledger rows 33/94): the sandbox globals lockdown back
	// on ABI v4 — rt_sandbox(1) nils the 5.5 untrusted surface out of the
	// globals table (dev flavor: all 9 present; U6d's prod flavor returns
	// 0). Body runs under rt_run protection.
	if e.Sandbox {
		if _, err := call(rtInst, "rt_sandbox", u32(1)); err != nil {
			return []string{"ENGINE-ERROR\trt_sandbox: " + err.Error()}
		}
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
	// U6c (ledger row 94): the script-env lockdown, LAST host write into
	// _G — rt_protect_globals(1) installs the Redis 7.2.7 error metatable
	// (missing-global reads raise) and the protected set behind the seven
	// interposed table-write guards. Order contract: sandbox -> gcStop ->
	// arg -> protect -> init -> main.
	if e.Protect {
		if _, err := call(rtInst, "rt_protect_globals", u32(1)); err != nil {
			return []string{"ENGINE-ERROR\trt_protect_globals: " + err.Error()}
		}
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
	// rt_deadline's ordinary error. Row 100 (U7a): deadline_poll answers
	// the same expiry from the guest's own goroutine — under a pending GC
	// mark-STW (row 99) the timer below is frozen with the scheduler, but
	// the spinning guest still asks, and its import call transits into
	// preemptible Go frames (which is also how the STW drains). Both
	// paths write the flag and raise identical text; delivery no longer
	// depends on the scheduler.
	if e.Deadline > 0 {
		if a, err := call(rtInst, "rt_ctrl_addr"); err == nil {
			flagAddr := uint32(a[0])
			deadlineAt := time.Now().Add(e.Deadline)
			timer := time.AfterFunc(e.Deadline, func() {
				_ = mem.Write(flagAddr, deadlineFlagLE)
			})
			deadlinePoll = func() int32 {
				if time.Now().Before(deadlineAt) {
					return 0
				}
				_ = mem.Write(flagAddr, deadlineFlagLE)
				return 1
			}
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
	if nres := int32(status[0]); nres >= 0 {
		// U9a2: chunk returns enter the differential surface — results are
		// TValue cells at frame+16*i (the U9a entry-frame staging), each
		// pulled through rt_encode_value (row 96) into linbuf.
		inA := uint32(0)
		if v, err := call(rtInst, "linbuf"); err == nil {
			inA = uint32(int32(v[0]))
		}
		emit("RESULTS", resultsLineFromCells(int(nres), func(i int) []byte {
			if inA == 0 {
				return nil
			}
			res, err := call(rtInst, "rt_encode_value",
				u32(int32(frame[0])+int32(16*i)), u32(int32(inA)), 1<<20)
			if err != nil {
				return nil
			}
			n := int32(res[0])
			if n <= 0 {
				return nil
			}
			if raw, ok := mem.Read(inA, uint32(n)); ok {
				return raw
			}
			return nil
		}))
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
