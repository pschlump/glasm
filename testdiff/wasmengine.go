package testdiff

// The wasm-backend engine (M4): compiles Lua through the glasm
// frontend + luawasm backend and runs the emitted module against the C
// runtime on wasmtime — the third differential engine. The harness shim
// comes from the runtime module's install_shims (print→host event,
// deterministic os.*, host RNG for math.random), matching the interp
// and clua engines' contract. Since U2e the runtime blob is the ABI v4
// rt flavor on the stock 5.5 tree (lua55_rt.wasm, embedded below —
// rebuilt by runtime/build55.sh; the 5.1 blob stays the clua oracle's).

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"math"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	wt "github.com/bytecodealliance/wasmtime-go/v48"

	lua "github.com/pschlump/glasm"
	"github.com/pschlump/glasm/luawasm"
	"github.com/pschlump/glasm/parse"
	"github.com/pschlump/glasm/wasm"
)

//go:embed lua55_rt.wasm
var lua55RtWasm []byte

// rtwStackTrap mirrors the blob's RTW_STACKTRAP (-4, rt_abi55.h): the
// host dispatch returns it when the engine trapped the nested call with
// "call stack exhausted", and the blob's trampoline stages the same
// bare "stack overflow" its depth guard stages (U5e, row 20 family).
const rtwStackTrap = int32(-4)

// gcStop runs collectgarbage('stop') in the runtime state via the M2
// driver's ldostring (v1 mitigation — see the Run comment).
func (e *WasmEngine) gcStop(store *wt.Store, rtInst *wt.Instance, call func(*wt.Instance, string, ...interface{}) (interface{}, error), L interface{}, name string) error {
	inAddr, err := call(rtInst, "linbuf")
	if err != nil {
		return err
	}
	nameAddr, err := call(rtInst, "lnamebuf")
	if err != nil {
		return err
	}
	src := []byte("collectgarbage('stop')")
	if len(name) > 500 {
		name = name[:500]
	}
	mem := rtInst.GetExport(store, "memory").Memory().UnsafeData(store)
	in, nameA := uint32(toI32(inAddr)), uint32(toI32(nameAddr))
	copy(mem[in:], src)
	copy(mem[nameA:], []byte(name))
	_, err = call(rtInst, "ldostring", L, int(in), len(src), int(nameA), 1)
	return err
}

// luaQuote renders s as a Lua 5.1 double-quoted string literal: \\ and \"
// escaped, control bytes as 3-digit decimal \ddd (5.1 has no \x escapes;
// 3-digit padding keeps a following digit from being absorbed).
func luaQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			fmt.Fprintf(&b, "\\%03d", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// wireErrHostFns is the host_call refusal as one wire-protocol value
// (PT_STRING: tag 4, u32 LE length, bytes) — engines without the hostfn
// surface stage it so the guest raises a clean Lua error.
var wireErrHostFns = func() []byte {
	const msg = "host functions are not available in this engine"
	b := []byte{4, byte(len(msg)), 0, 0, 0}
	return append(b, msg...)
}()

// setArgGlobal exposes the CLI args in the runtime state as the global arg
// table — arg[0] = chunkname (the C driver contract, luawasm.c's
// dostring_body: raw name without the '@' prefix), arg[i] = c.Args[i-1] —
// before lua_main dispatches the script, so standalone wasm runs (glua -W,
// luawasm-run) see the same surface the interpreter CLI provides.
func (e *WasmEngine) setArgGlobal(store *wt.Store, rtInst *wt.Instance, call func(*wt.Instance, string, ...interface{}) (interface{}, error), L interface{}, name string, args []string) error {
	name = strings.TrimPrefix(name, "@") // arg[0] is the raw name (luawasm.c)
	inAddr, err := call(rtInst, "linbuf")
	if err != nil {
		return err
	}
	nameAddr, err := call(rtInst, "lnamebuf")
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("arg={[0]=")
	b.WriteString(luaQuote(name))
	for _, a := range args {
		b.WriteByte(',')
		b.WriteString(luaQuote(a))
	}
	b.WriteString("}")
	src := []byte(b.String())
	if len(src) > 1<<20 { // inbuf is 1 MiB (luawasm.c)
		return fmt.Errorf("arg table snippet too large: %d bytes", len(src))
	}
	mem := rtInst.GetExport(store, "memory").Memory().UnsafeData(store)
	in, nameA := uint32(toI32(inAddr)), uint32(toI32(nameAddr))
	copy(mem[in:], src)
	copy(mem[nameA:], []byte("=arg")) // literal chunkname: host setup, not script
	_, err = call(rtInst, "ldostring", L, int(in), len(src), int(nameA), 0)
	return err
}

func toI32(v interface{}) int32 {
	switch x := v.(type) {
	case int32:
		return x
	case uint64:
		return int32(x)
	case int64:
		return int32(x)
	case float64:
		return int32(x)
	}
	return 0
}

// renderErrValue: a staged non-string error TValue (16 bytes) → the
// interp's ValueRepr shape for the deterministic tags. Matches PrintArg /
// ValueRepr: nil, boolean, number (NumRepr); collectables render as bare
// markers (addresses are engine-local — the suite avoids them).
// renderErrValue: a non-string staged error TValue, v4 variant tags
// (rt_abi55.h RT_V*; U2e — the old 5.1 tags read VNUMINT cells as f64,
// rendering 42 as a denormal). Strings never land here (the bytes path
// handled them); table/function values carry addresses and stay outside
// the byte-exact suite by corpus discipline.
func renderErrValue(raw []byte) string {
	tag := raw[8]
	u64 := func() uint64 {
		bits := uint64(0)
		for i := 7; i >= 0; i-- {
			bits = bits<<8 | uint64(raw[i])
		}
		return bits
	}
	switch tag {
	case 0: // VNIL
		return "nil"
	case 1: // VFALSE
		return "false"
	case 17: // VTRUE
		return "true"
	case 3: // VNUMINT: i64 at offset 0
		return strconv.FormatInt(int64(u64()), 10)
	case 19: // VNUMFLT: f64 at offset 0
		return NumRepr(math.Float64frombits(u64()))
	case 4, 20: // VSHRSTR / VLNGSTR — the bytes path handles these
		return ""
	case 5: // VTABLE
		return "<table>"
	case 6, 22, 38: // VLCL / VLCF / VCCL (a wasm function is a C closure)
		return "<function>"
	case 2: // VLIGHTUSERDATA
		return "<userdata>"
	case 7: // VUSERDATA
		return "<userdata>"
	case 8: // VTHREAD
		return "<thread>"
	}
	return "<value>"
}

// CompileSource runs the frontend and the backend: source → script.wasm.
// This is what cmd/luawasmc saves to disk.
func CompileSource(source []byte, name string) ([]byte, error) {
	chunk, err := parse.Parse(strings.NewReader(string(source)), name)
	if err != nil {
		return nil, err
	}
	proto, err := lua.Compile(chunk, name)
	if err != nil {
		return nil, err
	}
	return luawasm.Compile(proto, name)
}

// WasmEngine compiles and executes scripts through the wasm backend.
type WasmEngine struct {
	name string
	// one shared wasmtime Engine (lazy) — see Run
	engineOnce sync.Once
	wtEngine   *wt.Engine
	// NoopHosts: replace host shims with no-ops (trap triage)
	NoopHosts bool
	// Precompiled module bytes (nil → compile from source)
	Precompiled []byte
	// SkipUnsupported: scripts using v1-unsupported opcodes produce a
	// SKIP-UNSUPPORTED log instead of an engine error (corpus tests)
	SkipUnsupported bool
	// RtBin overrides the runtime blob (nil → embedded lua51_sjlj.wasm;
	// the RTDBG env var still wins). M6c gates pass lua51ProdWasm.
	RtBin []byte
	// Sandbox: call rt_sandbox(1) after rt_set_state (M6c globals
	// lockdown, ledger row 33; U6c re-pins the list on 5.5 — ledger row
	// 94) and skip the io.flush drain.
	Sandbox bool
	// Protect: call rt_protect_globals(1) after every other host write
	// into _G — the Redis 7.2.7 script-env lockdown: missing-global reads
	// raise, and every table reachable from _G (plus the string
	// metatable) rejects writes (U6c, ledger row 94).
	Protect bool
	// MaxMem: per-VM allocation budget in bytes (0 = unlimited). Applied
	// via rt_set_memlimit BEFORE lnewstate so the whole VM lifetime
	// counts; over-budget allocation dies with a clean pcall-catchable
	// "not enough memory" (M6d D3, ledger row 36).
	MaxMem int64
	// Deadline: host timeout for the run. When it expires, a watchdog
	// writes the rt control-block flag; the guest's back-edge polls raise
	// "context deadline exceeded" as an ordinary Lua error (M6d D4,
	// ledger row 37).
	Deadline time.Duration
}

// NewWasmEngine returns a wasm-backend engine with the given name.
func NewWasmEngine(name string) *WasmEngine { return &WasmEngine{name: name} }

func (e *WasmEngine) Name() string { return e.name }

func (e *WasmEngine) Run(c Case) (log []string) {
	defer func() {
		if p := recover(); p != nil {
			log = []string{fmt.Sprintf("ENGINE-PANIC\t%v\t%s", p, debug.Stack())}
		}
	}()
	var log2 []string
	log = log2
	emit := func(event, payload string) {
		log = append(log, event+"\t"+payload)
	}

	bin := e.Precompiled
	if bin == nil {
		var err error
		// U1: '@'-sigil chunk names (parse.ChunkID renders the bare file
		// name, matching the interp engine and the C drivers)
		bin, err = CompileSource(c.Source, "@"+c.Name)
		if err != nil {
			if e.SkipUnsupported && strings.Contains(err.Error(), "backend v1") {
				return []string{"SKIP-UNSUPPORTED\t" + err.Error()}
			}
			// parse/compile failures are script-level ERRORs (the interp
			// engine reports them the same way), not engine errors
			return []string{"ERROR\t" + strconv.Quote(err.Error())}
		}
	}

	wd, _ := os.Getwd()
	if err := os.Chdir(c.Dir); err != nil {
		return []string{fmt.Sprintf("ENGINE-ERROR\tchdir %s: %v", c.Dir, err)}
	}
	defer os.Chdir(wd)

	_ = context.Background()
	// One wasmtime Engine per WasmEngine (lazy, shared across Runs —
	// engines are designed for sharing; this is the compilation cache,
	// not per-run state). The M6e soak GC fix (runner.go) bounds the
	// per-run Stores; reusing the Engine cuts the per-case object churn
	// that fed the finalizer backlog.
	e.engineOnce.Do(func() {
		cfg := wt.NewConfig()
		cfg.SetWasmExceptions(true)
		e.wtEngine = wt.NewEngineWithConfig(cfg)
	})
	engine := e.wtEngine
	store := wt.NewStore(engine)

	var guestMem *wt.Memory
	memRead := func(ptr int32, length int32) []byte {
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
	memWrite := func(ptr int32, b []byte) {
		if dst := memRead(ptr, int32(len(b))); dst != nil {
			copy(dst, b)
		}
	}

	linker := wt.NewLinker(engine)

	// U2e: blob selection — the default is the ABI v4 rt blob (the stock
	// 5.5 tree's rt_abi55), wired by the same host contract as v3.
	// WASI wiring driven by the blob's own imports.
	// U3b: no host.random* imports — stock xoshiro math.random runs in
	// the blob over the makeseed=0 pin (the D5 host-RNG shim retired).
	rtBin := lua55RtWasm
	if e.RtBin != nil {
		rtBin = e.RtBin
	}
	if p := os.Getenv("RTDBG"); p != "" {
		if b, e := os.ReadFile(p); e == nil {
			rtBin = b
		}
	}
	blobNeedsWASI, err := wasm.HasWASIImports(rtBin)
	if err != nil {
		return []string{"ENGINE-ERROR\trt blob parse: " + err.Error()}
	}
	var stdoutFile *os.File // non-nil iff the blob writes to wasi stdout
	if blobNeedsWASI {
		if err := linker.DefineWasi(); err != nil {
			return []string{"ENGINE-ERROR\t" + err.Error()}
		}
	}

	decoder := &CLua{}
	eventFn := func(kind, ptr, length int32) {
		if raw := memRead(ptr, length); raw != nil {
			decoder.onEvent(raw, kind, emit)
		}
	}
	if e.NoopHosts {
		eventFn = func(kind, ptr, length int32) {}
	}
	if err := linker.DefineFunc(store, "host", "event", eventFn); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	// host.wasm_dispatch: the M5a reverse seam — the C runtime's
	// precall_wasm adapter dispatches compiled protos through the host
	// into the script module's exported lua_dispatch (same store, nested
	// call). scriptInst is wired below once instantiated; until then (and
	// for stub hosts) this refuses with -3, which the adapter re-raises
	// as a Lua error.
	var scriptInst *wt.Instance
	dispatch := func(idx, frame, cl, nargs, want int32) int32 {
		if scriptInst == nil {
			return -3
		}
		fn := scriptInst.GetFunc(store, "lua_dispatch")
		if fn == nil {
			return -3
		}
		res, err := fn.Call(store, idx, frame, cl, nargs, want)
		if err != nil {
			// a trap inside the dispatch must not unwind through wasm.
			// U5e (row 20 family): a stack-exhaustion trap is the depth
			// bound for chunks whose per-level frames beat RTW_MAX_DEPTH
			// to the engine limit — map it to the sentinel the blob
			// stages as a clean catchable "stack overflow"; every other
			// trap surfaces as a refused dispatch.
			if strings.Contains(err.Error(), "call stack exhausted") {
				return rtwStackTrap
			}
			return -3
		}
		if n, ok := res.(int32); ok {
			return n
		}
		return -3
	}
	if err := linker.DefineFunc(store, "host", "wasm_dispatch", dispatch); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	// M7a: host-backed Lua functions (the redis.call seam) are a
	// host-package feature; the differential engines refuse them — the
	// guest raises the staged error value as a clean Lua error.
	if err := linker.DefineFunc(store, "host", "host_call",
		func(fnidx, argsPtr, argsLen, retPtr, retCap int32) int32 {
			memWrite(retPtr, wireErrHostFns)
			return -1
		}); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	// Row 100 (U7a): the STW-proof backstop's consult leg — armed at the
	// deadline site below (0 = not expired / no deadline). wasmtime
	// preempts a spinning guest at its own store epoch, so unlike wazero
	// this needs no flag write: the affirmative answer alone makes the
	// guest raise rt_deadline's ordinary error at its next back-edge
	// poll; the flag timer stays the primary delivery.
	var deadlineAt int64
	if err := linker.DefineFunc(store, "host", "deadline_poll",
		func() int32 {
			if deadlineAt != 0 && time.Now().UnixNano() >= deadlineAt {
				return 1
			}
			return 0
		}); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}

	if blobNeedsWASI {
		wasi := wt.NewWasiConfig()
		if err := wasi.PreopenDir(c.Dir, "/", true); err != nil {
			return []string{"ENGINE-ERROR\t" + err.Error()}
		}
		f, err := os.CreateTemp("", "wasmstdout-*")
		if err != nil {
			return []string{"ENGINE-ERROR\t" + err.Error()}
		}
		f.Close()
		defer os.Remove(f.Name())
		if err := wasi.SetStdoutFile(f.Name()); err != nil {
			return []string{"ENGINE-ERROR\t" + err.Error()}
		}
		stdoutFile = f
		store.SetWasi(wasi)
	}

	rtMod, err := wt.NewModule(engine, rtBin)
	if err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	rtInst, err := linker.Instantiate(store, rtMod)
	if err != nil {
		return []string{"ENGINE-ERROR\trt instantiate: " + err.Error()}
	}
	if err := linker.DefineInstance(store, "rt", rtInst); err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	guestMem = rtInst.GetExport(store, "memory").Memory()

	call := func(inst *wt.Instance, fn string, args ...interface{}) (interface{}, error) {
		f := inst.GetFunc(store, fn)
		if f == nil {
			return nil, fmt.Errorf("missing export %s", fn)
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

	// U6b (rows 36/93, M6d D3 back on ABI v4): the per-VM allocation cap
	// — before lnewstate, so the state, stdlib, shims, constant pool and
	// script all count; the whole VM lifetime runs through rt_alloc.
	if e.MaxMem > 0 {
		if _, err := call(rtInst, "rt_set_memlimit", int32(e.MaxMem)); err != nil {
			return []string{"ENGINE-ERROR\trt_set_memlimit: " + err.Error()}
		}
	}

	scriptMod, err := wt.NewModule(engine, bin)
	if err != nil {
		return []string{"ENGINE-ERROR\tscript module: " + err.Error()}
	}
	si, err := linker.Instantiate(store, scriptMod)
	if err != nil {
		return []string{"ENGINE-ERROR\tscript instantiate: " + err.Error()}
	}
	scriptInst = si

	L, err := call(rtInst, "lnewstate")
	if err != nil {
		return []string{"ENGINE-ERROR\tlnewstate: " + err.Error()}
	}
	// the ABI operates on this state (curL in rt_abi.c) — without it,
	// NULL-derefs land in mapped page 0 and surface as wild call_indirects
	if _, err := call(rtInst, "rt_set_state", L); err != nil {
		return []string{"ENGINE-ERROR\trt_set_state: " + err.Error()}
	}
	// U6c (ledger rows 33/94): the sandbox globals lockdown back
	// on ABI v4 — rt_sandbox(1) nils the 5.5 untrusted surface (io os
	// package require dofile loadfile load debug warn) out of the globals
	// table; the dev flavor opens all 9 (the U6d prod flavor compiles
	// them out and returns 0). Body runs under rt_run protection.
	if e.Sandbox {
		if _, err := call(rtInst, "rt_sandbox", int32(1)); err != nil {
			return []string{"ENGINE-ERROR\trt_sandbox: " + err.Error()}
		}
	}
	// v1 GC stop: register cells are not GC roots (the M3 ABI obligation —
	// a full GC-rooted frame arrives with the M6 arena lifecycle); any
	// luaC_checkGC inside library calls (e.g. table.sort's stack churn)
	// would collect tables referenced only from cells. Arena-reset replaces
	// GC for script runs anyway.
	if err := e.gcStop(store, rtInst, call, L, c.Name); err != nil {
		return []string{"ENGINE-ERROR\tgc stop: " + err.Error()}
	}
	// expose the CLI args (gcStop's ldostring resets arg = {[0]=name}, so
	// this runs after it and owns the full surface)
	if err := e.setArgGlobal(store, rtInst, call, L, c.Name, c.Args); err != nil {
		return []string{"ENGINE-ERROR\targ setup: " + err.Error()}
	}
	// U6c (ledger row 94): the script-env lockdown, LAST host write into
	// _G — rt_protect_globals(1) installs the Redis 7.2.7 error metatable
	// (missing-global reads raise) and the protected set behind the seven
	// interposed table-write guards (writes raise "Attempt to modify a
	// readonly table"). Order contract: sandbox -> gcStop -> arg ->
	// protect -> init -> main.
	if e.Protect {
		if _, err := call(rtInst, "rt_protect_globals", int32(1)); err != nil {
			return []string{"ENGINE-ERROR\trt_protect_globals: " + err.Error()}
		}
	}
	// U2e: no dialect layer on the v4 blob — the 5.5 tree speaks stock
	// texts natively, which is what the interp oracle was re-pinned to
	// (row 56); the M5d gnumfmt machinery stays 5.1-blob-only.
	av, err := call(rtInst, "rt_abi_version")
	if err != nil {
		return []string{"ENGINE-ERROR\tABI version: " + err.Error()}
	}
	if avv, ok := av.(int32); !ok || avv != 4 {
		return []string{fmt.Sprintf("ENGINE-ERROR\tABI version %v (need 4)", av)}
	}
	initStep := int32(2)
	if v := os.Getenv("INITSTEP"); v != "" {
		fmt.Sscanf(v, "%d", &initStep)
	}
	if _, err := call(scriptInst, "luawasm_init", initStep); err != nil {
		return append(log, "ENGINE-ERROR\tinit: "+err.Error())
	}
	gv := scriptInst.GetExport(store, "gFrameCells").Global().Get(store)
	frameCells := gv.I32()
	frame, err := call(rtInst, "rt_frame_alloc", int(frameCells)*16)
	if err != nil {
		return []string{"ENGINE-ERROR\tframe: " + err.Error()}
	}

	// U6b (row 37, the M6d D4 watchdog back on ABI v4). wasmtime Stores
	// are !Sync — the only safe off-thread crossing is a bare aligned
	// word store into the linear memory: the blob declares a memory max,
	// so wasmtime maps it statically (the base never moves) and the slice
	// captured here on the main goroutine stays valid. The guest's
	// basic-block-transition polls read the flag and raise rt_deadline's
	// ordinary (pcall-catchable) error.
	if e.Deadline > 0 {
		if a, err := call(rtInst, "rt_ctrl_addr"); err == nil {
			flagAddr := uint32(toI32(a))
			memBase := guestMem.UnsafeData(store)
			// row 100: arm the consult leg with the same absolute
			// deadline the timer below fires on
			deadlineAt = time.Now().Add(e.Deadline).UnixNano()
			timer := time.AfterFunc(e.Deadline, func() {
				// 4-byte little-endian 1 — one aligned store
				b := memBase[flagAddr : flagAddr+4 : flagAddr+4]
				b[0], b[1], b[2], b[3] = 1, 0, 0, 0
			})
			defer timer.Stop()
		}
	}

	status, err := call(scriptInst, "lua_main", frame)
	// KeepAlive holds the mapping live until Run itself returns (the
	// M6e teardown race; the v4 watchdog returns at U6)
	runtime.KeepAlive(store)
	runtime.KeepAlive(engine)
	if err != nil {
		return append(log, "ENGINE-ERROR\ttrap: "+err.Error())
	}
	stv, _ := status.(int32)
	// M7a: lua_main returns the dispatch status verbatim — nret ≥ 0 or a
	// negative error code (want=-1 multret; the count survives the entry)
	if stv < 0 {
		// staged error: message bytes, or — for non-string error values —
		// the exact TValue rendered here (M5d: error(nil)/error(42) match
		// the interp's deterministic renders; table/function values carry
		// addresses and stay outside the byte-exact suite)
		emitted := false
		if buf, err := call(rtInst, "rt_frame_alloc", 4096); err == nil {
			if n, err := call(rtInst, "rt_err_stage_copy", buf, 4096); err == nil {
				nv, _ := n.(int32)
				if nv > 0 {
					if raw := memRead(buf.(int32), nv); raw != nil {
						emit("ERROR", strconv.Quote(string(raw)))
						emitted = true
					}
				}
			}
		}
		if !emitted {
			if vp, err := call(rtInst, "rt_err_value_ptr"); err == nil {
				if raw := memRead(vp.(int32), 16); raw != nil {
					emit("ERROR", strconv.Quote(renderErrValue(raw)))
				}
			}
		}
	}
	if _, err := call(rtInst, "lglobals"); err != nil {
		emit("GLOBALS", "<lglobals failed: "+err.Error()+">")
	}

	// Ledger row 29: the driver returns without libc exit(), so
	// wasi-libc's atexit stdout flush never runs and io.write's buffered
	// tail is lost (the first line alone made it through). Best-effort
	// io.flush() in the live state drains it before the host reads the
	// capture file; failures (e.g. a script that closed io.stdout) are
	// ignored — the flush is an engine obligation, not script behavior.
	eFlush := func() {
		inAddr, err := call(rtInst, "linbuf")
		if err != nil {
			return
		}
		nameAddr, err := call(rtInst, "lnamebuf")
		if err != nil {
			return
		}
		src := []byte("io.flush()")
		mem := rtInst.GetExport(store, "memory").Memory().UnsafeData(store)
		in, nameA := uint32(toI32(inAddr)), uint32(toI32(nameAddr))
		copy(mem[in:], src)
		copy(mem[nameA:], []byte("=flush"))
		_, _ = call(rtInst, "ldostring", L, int(in), len(src), int(nameA), 0)
	}
	if !e.Sandbox { // io is nil under the sandbox — nothing to drain
		eFlush()
	}

	if stdoutFile != nil {
		if out, err := os.ReadFile(stdoutFile.Name()); err == nil && len(out) > 0 {
			for _, line := range bytes.Split(bytes.TrimRight(out, "\n"), []byte("\n")) {
				emit("STDOUT", string(line))
			}
		}
	}
	return log
}
