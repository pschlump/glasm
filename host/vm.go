package host

// VM: one Lua memory image — a private wazero runtime holding the
// runtime-module instance (the C rt_* world and the shared linear
// memory), the script-module instance, and the host module whose
// closures are bound to this VM — plus the mutex that makes it
// thread-proof (design A9). All guest state lives in the image.
//
// Why one wazero.Runtime per VM instead of one shared runtime: module
// names resolve inside a runtime's namespace at instantiation time (the
// script module imports "rt"), so two concurrently-live images in one
// runtime would bind every script to whichever rt instance won the name.
// Per-VM runtimes make concurrent images trivially correct; script
// COMPILATION (the expensive frontend work) stays cached at the Engine
// level, and only wazero's per-instance module compilation is repaid per
// VM. A pooled-VM refinement is deferred until benchmarks justify it
// (guide R3).

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// RunOptions is one execution of a script against a VM image.
type RunOptions struct {
	// Keys and Argv are staged as the KEYS / ARGV globals (copies — the
	// script can never alias daemon memory).
	Keys, Argv []string
	// Deadline bounds the run: a watchdog goroutine flips the guest's
	// deadline flag and the next loop back-edge raises an ordinary
	// pcall-catchable error ("context deadline exceeded" — M6d D4). Zero
	// disables the watchdog.
	Deadline time.Duration
	// Seed reseals the VM's math.random stream for this run by running
	// math.randomseed(n) in the guest (determinism D5): the stock
	// xoshiro256** stream, identical to the interpreter's — same seed,
	// same sequence on every engine. Zero keeps the VM's existing stream
	// (fresh VMs start at the build-pinned stream: luai_makeseed()=0u in
	// runtime/build55.sh; the host-RNG imports retired at U3b).
	Seed int64
}

// Result is a script's return values, converted.
type Result struct {
	Values []Value
}

// ScriptError is a Lua-level error (raised by the script, a library, a
// host function, or the deadline/OOM machinery). ErrValue is the exact
// error value; for string errors Redis semantics pass the text through
// verbatim. Line is the raise line inside the script (0 = unknown) — the
// position Redis reports as "on @user_script:N" in error replies.
type ScriptError struct {
	ErrValue Value
	Line     int
}

func (e *ScriptError) Error() string { return e.ErrValue.String() }

// TrapError is a wasm trap or engine-level failure. By contract traps are
// always backend bugs (design §6.4): log them with the module SHA, never
// surface them as script errors.
type TrapError struct{ Err error }

func (e *TrapError) Error() string { return "host: trap (backend bug class): " + e.Err.Error() }
func (e *TrapError) Unwrap() error { return e.Err }

// ErrReentrant is returned when a HostFunc tries to re-enter its own VM.
var ErrReentrant = errors.New("host: VM.Run called from inside a host function (the image lock is not reentrant)")

// ErrScriptBound is the v1 one-script law: a VM image executes scripts
// compiled from ONE source (proto indices are per-script, 0-based, into a
// per-image registry — a second script would collide). Use a fresh VM
// (Engine.Run) per distinct script.
var ErrScriptBound = errors.New("host: VM is bound to a different script (v1: one script per VM image)")

// deadlineFlagLE is the watchdog's single store: 1 as 4 little-endian
// bytes (the rt control-block contract, rt_abi55.c).
var deadlineFlagLE = []byte{1, 0, 0, 0}

func u32v(v int32) uint64 { return uint64(uint32(v)) }

// VM is one locked Lua memory image.
type VM struct {
	mu     sync.Mutex
	e      *Engine
	closed bool

	rt        wazero.Runtime
	rtInst    api.Module
	mem       api.Memory
	L         int32 // the lua_State handle from lnewstate
	ctrlAddr  uint32
	hostFns   []registeredFn  // snapshot of the engine registry
	hostVals  []registeredVal // snapshot of the engine value registry
	eventSink func(vm *VM, args []Value)

	script     *Script         // bound at first Run (one-script law)
	scriptInst api.Module      // nil until then
	frame      uint32          // the script's frame cells (gFrameCells)
	inAddr     uint32          // cached linbuf staging address (1 MiB)
	curCtx     context.Context // in-flight Run's ctx (nested dispatches)

	// inRun is the same-goroutine reentrancy guard: checked BEFORE the
	// mutex (a HostFunc re-entering its VM would otherwise deadlock on
	// the lock it already holds). A Run from a different goroutine may
	// transiently observe ErrReentrant instead of blocking — retryable,
	// and the fresh-VM-per-run deployment never trips it.
	inRun atomic.Bool

	// flagMu serializes deadline-flag writes: the watchdog goroutine's
	// arm() against this goroutine's per-run hygiene clear, plus the
	// done-store that retires the watchdog. Without it a late-firing
	// timer from a previous Run races the next Run's clear at the same
	// address (-race gate).
	flagMu sync.Mutex
}

// NewVM builds a fresh image: runtime + host module + rt instance + Lua
// state, sandboxed, with the engine's host functions registered. µs–ms
// (instantiation plus the C state build); the M0-era measurement put
// wazero instantiation at ~7.4 µs for a tiny module — this one also
// opens the Lua stdlib.
func (e *Engine) NewVM() (*VM, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, errors.New("host: engine closed")
	}
	vm := &VM{e: e, hostFns: e.hostFns, hostVals: e.hostVals, eventSink: e.opts.eventSink}
	e.vms++
	e.mu.Unlock()

	ctx := context.Background()
	rc := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling)
	r := wazero.NewRuntimeWithConfig(ctx, rc)
	vm.rt = r

	// host module — closures bound to THIS VM (per-runtime instance, so
	// concurrent VMs never cross-dispatch). Exactly the blob's import
	// surface: event, wasm_dispatch, host_call (the host.random* RNG
	// imports retired at U3b — math.random is the guest's own stock
	// xoshiro256** stream now; see RunOptions.Seed)
	_, err := r.NewHostModuleBuilder("host").
		NewFunctionBuilder().WithFunc(vm.hostEvent).Export("event").
		NewFunctionBuilder().WithFunc(vm.hostDispatch).Export("wasm_dispatch").
		NewFunctionBuilder().WithFunc(vm.hostCall).Export("host_call").
		Instantiate(ctx)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("host: host module: %w", err)
	}

	// the runtime blob — no WASI, no FS, no stdio (verified at NewEngine)
	rtInst, err := r.InstantiateWithConfig(ctx, e.opts.blob, wazero.NewModuleConfig().WithName("rt"))
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("host: rt instantiate: %w", err)
	}
	vm.rtInst = rtInst
	vm.mem = rtInst.Memory()

	// memory budget BEFORE lnewstate so the whole VM lifetime counts
	if e.opts.maxMem > 0 {
		if _, err := vm.call0(ctx, "rt_set_memlimit", u32v(int32(e.opts.maxMem))); err != nil {
			r.Close(ctx)
			return nil, fmt.Errorf("host: rt_set_memlimit: %w", err)
		}
	}

	// the Lua state + ABI wiring
	L, err := vm.call0(ctx, "lnewstate")
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("host: lnewstate: %w", err)
	}
	if L[0] == 0 {
		// the state build itself hit the budget (rt_alloc refusal) —
		// a clean constructor error, not a broken image
		r.Close(ctx)
		return nil, errors.New("host: lnewstate failed (not enough memory for the state — raise WithMemoryBudgetBytes)")
	}
	vm.L = int32(L[0])
	if _, err := vm.call0(ctx, "rt_set_state", u32v(vm.L)); err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("host: rt_set_state: %w", err)
	}
	// globals lockdown (prod belt-and-suspenders): rt_sandbox nils the
	// 5.5 untrusted surface — io os package require dofile loadfile load
	// debug warn (loadstring/module never existed in 5.5; the prod blob
	// compiles them out and this is the no-op return)
	if _, err := vm.call0(ctx, "rt_sandbox", 1); err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("host: rt_sandbox: %w", err)
	}
	// v1 GC law: register cells are not GC roots — GC stays stopped
	if err := vm.ldostring(ctx, "collectgarbage('stop')", "=gcstop"); err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("host: gc stop: %w", err)
	}
	// ABI v4 (the 5.5 tree): variant-tag cells, stock 5.5 error texts —
	// no dialect layer (the interp oracle was re-pinned to stock, row 56)
	if v, err := vm.call0(ctx, "rt_abi_version"); err != nil || int32(v[0]) != 4 {
		r.Close(ctx)
		return nil, fmt.Errorf("host: ABI version mismatch (need 4)")
	}
	// host functions: redis.call and friends (names ride namebuf)
	for i, hf := range vm.hostFns {
		names := hf.table + "\x00" + hf.name + "\x00"
		if len(names) > 508 {
			r.Close(ctx)
			return nil, fmt.Errorf("host: hostfn name too long: %s.%s", hf.table, hf.name)
		}
		nameAddr, err := vm.call0(ctx, "lnamebuf")
		if err != nil {
			r.Close(ctx)
			return nil, err
		}
		if !vm.mem.Write(uint32(nameAddr[0]), []byte(names)) {
			r.Close(ctx)
			return nil, errors.New("host: namebuf write")
		}
		if _, err := vm.call0(ctx, "rt_hostfn", u32v(int32(nameAddr[0])),
			u32v(int32(nameAddr[0])+int32(len(hf.table))+1), u32v(int32(i))); err != nil {
			r.Close(ctx)
			return nil, fmt.Errorf("host: rt_hostfn(%s.%s): %w", hf.table, hf.name, err)
		}
	}
	// value constants (redis.LOG_WARNING and friends): staged as source
	// after the hostfn tables exist, still before any script runs
	if len(vm.hostVals) > 0 {
		var sb strings.Builder
		for _, rv := range vm.hostVals {
			fmt.Fprintf(&sb, "%s=%s or {};", rv.table, rv.table)
		}
		for _, rv := range vm.hostVals {
			fmt.Fprintf(&sb, "%s.%s=%s;", rv.table, rv.name, luaValueLit(rv.v))
		}
		if err := vm.ldostring(ctx, sb.String(), "=hostvals"); err != nil {
			r.Close(ctx)
			return nil, fmt.Errorf("host: staging value constants: %w", err)
		}
	}
	// globals lockdown (Redis-classic): after ALL host staging, before any
	// script runs — Run stages KEYS/ARGV inside a readonly-off window
	if e.opts.globalsProtection {
		if _, err := vm.call0(ctx, "rt_protect_globals", 1); err != nil {
			r.Close(ctx)
			return nil, fmt.Errorf("host: rt_protect_globals: %w", err)
		}
	}
	// cache the deadline flag address once (valid for the instance's
	// lifetime — the memory never moves)
	if a, err := vm.call0(ctx, "rt_ctrl_addr"); err == nil {
		vm.ctrlAddr = uint32(a[0])
	}
	return vm, nil
}

// call0 invokes an rt export with raw uint64 args.
func (vm *VM) call0(ctx context.Context, fn string, args ...uint64) ([]uint64, error) {
	f := vm.rtInst.ExportedFunction(fn)
	if f == nil {
		return nil, fmt.Errorf("missing rt export %s", fn)
	}
	return f.Call(ctx, args...)
}

// ldostring loads+runs a chunk in the image's state. want-globals stays
// off (this package consumes no GLOBALS events).
func (vm *VM) ldostring(ctx context.Context, src, chunkname string) error {
	in, err := vm.call0(ctx, "linbuf")
	if err != nil {
		return err
	}
	na, err := vm.call0(ctx, "lnamebuf")
	if err != nil {
		return err
	}
	inAddr, nameAddr := uint32(in[0]), uint32(na[0])
	if len(src) >= 1<<20 || len(chunkname) >= 500 {
		return errors.New("host: staging chunk too large")
	}
	if !vm.mem.Write(inAddr, []byte(src)) || !vm.mem.Write(nameAddr, []byte(chunkname)) {
		return errors.New("host: staging buffer write out of bounds")
	}
	res, err := vm.call0(ctx, "ldostring", u32v(vm.L), u32v(int32(inAddr)), u32v(int32(len(src))),
		u32v(int32(nameAddr)), 0)
	if err != nil {
		return err
	}
	if int32(res[0]) != 0 {
		// the message rides errdata — copy it out through inbuf (the
		// failing chunk has already been consumed)
		n, _ := vm.call0(ctx, "lerrlen")
		length := int(int32(n[0]))
		if length > 4096 {
			length = 4096
		}
		if length > 0 {
			c, _ := vm.call0(ctx, "lerrcopy", u32v(int32(inAddr)), u32v(int32(length)))
			if int32(c[0]) > 0 {
				if raw, ok := vm.mem.Read(inAddr, uint32(int32(c[0]))); ok {
					return errors.New(string(raw))
				}
			}
		}
		return fmt.Errorf("ldostring status %d", int32(res[0]))
	}
	return nil
}

// luaQuote renders s as a Lua 5.1 double-quoted string literal (binary
// safe: control bytes as \ddd).
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

// Run executes s against the image, holding vm.mu end-to-end (A9): one
// execution at a time per image, any number of images in parallel, safe
// from any goroutine.
func (vm *VM) Run(ctx context.Context, s *Script, opt RunOptions) (res Result, err error) {
	if s == nil {
		return Result{}, errors.New("host: nil script")
	}
	if vm.inRun.Load() {
		return Result{}, ErrReentrant
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.closed {
		return Result{}, errors.New("host: VM closed")
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	vm.curCtx = ctx
	defer func() { vm.curCtx = nil }()

	// one-script law: bind at first Run
	if vm.script == nil {
		if err := vm.bindScript(ctx, s); err != nil {
			return Result{}, err
		}
	} else if vm.script != s {
		return Result{}, ErrScriptBound
	}

	// per-run hygiene: clear any staged error and the sticky deadline
	// flag left by a previous (killed) run on this image (flagMu: a
	// previous run's late-firing watchdog must be ordered before this)
	if _, err := vm.call0(ctx, "rt_err_clear"); err != nil {
		return Result{}, err
	}
	if vm.ctrlAddr != 0 {
		vm.flagMu.Lock()
		_ = vm.mem.Write(vm.ctrlAddr, []byte{0, 0, 0, 0})
		vm.flagMu.Unlock()
	}

	// stage KEYS/ARGV as globals (copies; binary-safe quoting)
	var sb strings.Builder
	sb.WriteString("KEYS={")
	for i, k := range opt.Keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(luaQuote(k))
	}
	sb.WriteString("}ARGV={")
	for i, a := range opt.Argv {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(luaQuote(a))
	}
	sb.WriteString("}")
	if vm.e.opts.globalsProtection {
		// KEYS/ARGV write globals: open the readonly window for the
		// staging chunk, close it right after (Redis sets KEYS/ARGV with
		// the globals protection temporarily lifted the same way)
		if _, err := vm.call0(ctx, "rt_globals_readonly", 0); err != nil {
			return Result{}, err
		}
	}
	if err := vm.ldostring(ctx, sb.String(), "=args"); err != nil {
		return Result{}, &ScriptError{ErrValue: String(err.Error())}
	}
	if vm.e.opts.globalsProtection {
		if _, err := vm.call0(ctx, "rt_globals_readonly", 1); err != nil {
			return Result{}, err
		}
	}

	if opt.Seed != 0 {
		// D5 determinism: reseal in the guest — math.randomseed(n) over
		// the stock xoshiro256** stream (identical to the interpreter's;
		// the host-RNG imports retired at U3b)
		seed := "math.randomseed(" + strconv.FormatInt(opt.Seed, 10) + ")"
		if err := vm.ldostring(ctx, seed, "=seed"); err != nil {
			return Result{}, err
		}
	}

	// the deadline watchdog: the timer and ctx cancellation both flip the
	// guest flag (a plain 4-byte linear-memory store — safe off-thread);
	// the done flag skips the store after the run returns (the M6e soak
	// teardown race), and the watch channel + WaitGroup retire the ctx
	// watcher goroutine before Run returns
	var (
		done  atomic.Bool
		watch = make(chan struct{})
		wg    sync.WaitGroup
		timer *time.Timer
	)
	arm := func() {
		vm.flagMu.Lock()
		if !done.Load() && vm.ctrlAddr != 0 {
			_ = vm.mem.Write(vm.ctrlAddr, deadlineFlagLE)
		}
		vm.flagMu.Unlock()
	}
	if opt.Deadline > 0 {
		timer = time.AfterFunc(opt.Deadline, arm)
	}
	if ctx.Done() != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				arm()
			case <-watch:
			}
		}()
	}
	defer func() {
		// done-store and timer retirement under flagMu: an arm() that
		// already passed its done-check must complete its write BEFORE
		// the next Run's hygiene clear (vm.mu serializes the Runs; this
		// closes the late-fire ordering)
		vm.flagMu.Lock()
		done.Store(true)
		vm.flagMu.Unlock()
		close(watch)
		if timer != nil {
			timer.Stop()
		}
		wg.Wait()
	}()

	vm.inRun.Store(true)
	main := vm.scriptInst.ExportedFunction("lua_main")
	if main == nil {
		vm.inRun.Store(false)
		return Result{}, errors.New("host: script module missing lua_main")
	}
	status, err := main.Call(ctx, u32v(int32(vm.frame)))
	vm.inRun.Store(false)
	if err != nil {
		// a trap is always a backend bug class (design §6.4): wrap it so
		// the daemon can log module SHA + meta and answer with an
		// internal error, never a script error
		return Result{}, &TrapError{Err: err}
	}

	n := int32(status[0])
	if n < 0 {
		return Result{}, &ScriptError{ErrValue: vm.readErrorValue(ctx), Line: vm.readErrLine(ctx)}
	}
	vals := make([]Value, 0, max(int(n), 1))
	for i := int32(0); i < n; i++ {
		v, err := vm.readCellValue(ctx, vm.frame+uint32(i)*16)
		if err != nil {
			return Result{}, err
		}
		vals = append(vals, v)
	}
	return Result{Values: vals}, nil
}

// bindScript instantiates the script module and runs its init (interned
// constants, proto registration, frame allocation).
func (vm *VM) bindScript(ctx context.Context, s *Script) error {
	inst, err := vm.rt.InstantiateWithConfig(ctx, s.Wasm, wazero.NewModuleConfig().WithName("script"))
	if err != nil {
		return fmt.Errorf("host: script instantiate: %w", err)
	}
	vm.scriptInst = inst
	if _, err := inst.ExportedFunction("luawasm_init").Call(ctx, 2); err != nil {
		return fmt.Errorf("host: luawasm_init: %w", err)
	}
	cells := uint32(inst.ExportedGlobal("gFrameCells").Get())
	f, err := vm.call0(ctx, "rt_frame_alloc", u32v(int32(cells*16)))
	if err != nil {
		return fmt.Errorf("host: rt_frame_alloc: %w", err)
	}
	vm.frame = uint32(f[0])
	vm.script = s
	return nil
}

// readErrorValue reconstructs the staged error as a Value: the exact
// error TValue first (strings included via the wire encoder), the staged
// message bytes as a fallback.
func (vm *VM) readErrorValue(ctx context.Context) Value {
	if p, err := vm.call0(ctx, "rt_err_value_ptr"); err == nil && p[0] != 0 {
		if v, err := vm.readCellValue(ctx, uint32(p[0])); err == nil {
			return v
		}
	}
	// fallback: err_buf copy (4 KB cap, through inbuf)
	inAddr, err := vm.stagingAddr(ctx)
	if err != nil {
		return String("host: unreadable script error")
	}
	c, err := vm.call0(ctx, "rt_err_stage_copy", u32v(int32(inAddr)), 4096)
	if err != nil || int32(c[0]) <= 0 {
		return String("host: unreadable script error")
	}
	if raw, ok := vm.mem.Read(inAddr, uint32(int32(c[0]))); ok {
		return String(string(raw))
	}
	return String("host: unreadable script error")
}

// stagingAddr caches the rt module's 1 MiB input staging buffer address.
func (vm *VM) stagingAddr(ctx context.Context) (uint32, error) {
	if vm.inAddr == 0 {
		in, err := vm.call0(ctx, "linbuf")
		if err != nil {
			return 0, err
		}
		vm.inAddr = uint32(in[0])
	}
	return vm.inAddr, nil
}

// readCellValue encodes one TValue cell through the guest's wire encoder
// (rt_encode_value — tables expanded, strings inline; no host-side
// knowledge of TString/Table layouts) and decodes it. Values whose wire
// form fits the 1 MiB staging buffer round-trip through it; larger ones
// take a one-off frame allocation.
func (vm *VM) readCellValue(ctx context.Context, cellAddr uint32) (Value, error) {
	staging, err := vm.stagingAddr(ctx)
	if err != nil {
		return Value{}, err
	}
	const stageCap = 1 << 20
	n, err := vm.call0(ctx, "rt_encode_value", u32v(int32(cellAddr)),
		u32v(int32(staging)), stageCap)
	if err != nil {
		return Value{}, fmt.Errorf("host: rt_encode_value: %w", err)
	}
	full := int32(n[0])
	if full < 0 {
		return Value{}, errors.New("host: result value encoding failed in guest")
	}
	src := staging
	if int(full) > stageCap {
		b, err := vm.call0(ctx, "rt_frame_alloc", u32v(full))
		if err != nil {
			return Value{}, err
		}
		big := uint32(b[0])
		if n2, err := vm.call0(ctx, "rt_encode_value", u32v(int32(cellAddr)),
			u32v(int32(big)), u32v(full)); err != nil || int32(n2[0]) != full {
			return Value{}, errors.New("host: oversized result re-encode failed")
		}
		src = big
	}
	raw, ok := vm.mem.Read(src, uint32(full))
	if !ok {
		return Value{}, errors.New("host: result readback out of bounds")
	}
	v, _, derr := decodeValue(raw, 0, 0)
	if derr != nil {
		return Value{}, derr
	}
	return v, nil
}

// readErrLine returns the staged error's raise line (rt_err_line, M8) —
// 0 when the blob predates the export or the line is unknown.
func (vm *VM) readErrLine(ctx context.Context) int {
	v, err := vm.call0(ctx, "rt_err_line")
	if err != nil || len(v) == 0 {
		return 0
	}
	return int(int32(v[0]))
}

// Kill trips the in-flight run's deadline flag: the next guest loop
// back-edge raises the ordinary (pcall-catchable) deadline error, exactly
// as if the watchdog had fired. No-op between runs. This is the SCRIPT
// KILL mechanism: the daemon's scripting manager calls it on the VM a
// killable script is running on.
func (vm *VM) Kill() {
	vm.flagMu.Lock()
	defer vm.flagMu.Unlock()
	if vm.ctrlAddr != 0 && vm.inRun.Load() {
		_ = vm.mem.Write(vm.ctrlAddr, deadlineFlagLE)
	}
}

// UsedBytes reports the image's Lua allocation total (rt_mem_used_bytes):
// the same counter the WithMemoryBudgetBytes budget is enforced against.
// Guest GC is stopped (v1 law), so on a reused VM the figure grows
// monotonically — pooled-VM deployments use it as the recycling watermark.
// Serialized against Run; 0 when the blob predates the export.
func (vm *VM) UsedBytes(ctx context.Context) int64 {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.closed {
		return 0
	}
	v, err := vm.call0(ctx, "rt_mem_used_bytes")
	if err != nil || len(v) == 0 {
		return 0
	}
	return int64(int32(v[0]))
}

// Close tears the image down: the Lua state, both module instances, and
// the private runtime. The VM is unusable afterwards.
func (vm *VM) Close() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.closed {
		return nil
	}
	vm.closed = true
	ctx := context.Background()
	if vm.L != 0 {
		_, _ = vm.call0(ctx, "lclose", u32v(vm.L))
	}
	if vm.rt != nil {
		return vm.rt.Close(ctx)
	}
	return nil
}
