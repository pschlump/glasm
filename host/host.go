package host

// Engine: process-wide. Owns the compile cache and the host-function
// registry; VMs own their wazero runtimes (see vm.go for why they are not
// shared). Safe for concurrent use.

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/pschlump/glasm"
	"github.com/pschlump/glasm/luawasm"
	"github.com/pschlump/glasm/parse"
	"github.com/pschlump/pluto/lru_ts"
	"github.com/tetratelabs/wazero"
)

// HostFunc is a Lua-visible function implemented in Go (the redis.call
// mechanism). It executes on the goroutine that holds the VM's image
// lock — the same goroutine running the script — so it may inspect VM
// state freely, but it must NOT call vm.Run (the lock is not reentrant;
// ErrReentrant is returned if it does).
type HostFunc func(vm *VM, args []Value) ([]Value, error)

// Option configures an Engine.
type Option func(*options)

type options struct {
	blob              []byte                     // runtime blob override (still SHA-pinned)
	maxMem            int64                      // per-VM allocation budget bytes (0 = unlimited; the blob's 256 MiB linear-memory max is the hard backstop either way)
	eventSink         func(vm *VM, args []Value) // EVT_PRINT observer (already wire-decoded)
	globalsProtection bool                       // Redis-classic globals lockdown per VM (rt_protect_globals)
	vmCacheSize       int                        // warm-VM pool capacity (0 = the v1 fresh-VM-per-run posture)
}

// WithRuntimeBlob replaces the embedded production blob (a staged/test
// build). The replacement must still pass the frozen-surface checks.
func WithRuntimeBlob(bin []byte) Option {
	return func(o *options) { o.blob = bin }
}

// WithMemoryBudgetBytes caps each VM's Lua allocation budget; crossing it
// raises a clean pcall-catchable "not enough memory" error inside the
// script (rt_set_memlimit, M6d D3) — never a trap, never host OOM.
// Applied at VM creation (the budget must precede lnewstate so the whole
// VM lifetime counts).
func WithMemoryBudgetBytes(n int64) Option {
	return func(o *options) { o.maxMem = n }
}

// WithEventSink observes every guest print() (arguments decoded from the
// wire protocol). Called on the lock-holding goroutine.
func WithEventSink(fn func(vm *VM, args []Value)) Option {
	return func(o *options) { o.eventSink = fn }
}

// WithGlobalsProtection installs the Redis-classic script environment
// lockdown on every VM (rt_protect_globals): reading an undefined global
// raises "Script attempted to access nonexistent global variable '<k>'",
// and globals plus every table reachable from them are readonly — any
// write (assignment, rawset, rawseti) raises "Attempt to modify a
// readonly table". KEYS/ARGV stage per run inside a readonly-off window,
// so they stay writable. Enable after all RegisterGlobal/RegisterValue
// calls (VM creation order: sandbox → host staging → protection).
func WithGlobalsProtection(on bool) Option {
	return func(o *options) { o.globalsProtection = on }
}

// WithVMCacheSize enables a bounded pool of warm VM images, keyed by
// script source (the deferred pooled posture, guide R3): Engine.Run
// reuses a pooled image bound to the same source instead of building a
// fresh VM per run — the NewVM cost (wazero blob compile + Lua state
// build) is amortized to zero for repeat scripts. Images recycle
// (close, not reuse) once their Lua allocation crosses the recycle
// watermark (frozen-GC heaps grow monotonically — UsedBytes is the
// recycling signal), and engine-level failures close them; guest-level
// script errors keep the image pooled (the stack unwinds to base).
// n <= 0 (the default) keeps the fresh-VM-per-run v1 posture.
func WithVMCacheSize(n int) Option {
	return func(o *options) { o.vmCacheSize = n }
}

// Engine is the shared, process-wide scripting engine.
type Engine struct {
	opts options

	mu       sync.Mutex
	cache    map[string]*Script
	hostFns  []registeredFn
	hostVals []registeredVal
	vms      int // live VMs (RegisterGlobal freezes once the first exists)
	closed   bool

	// wcache shares one wazero engine across every VM runtime: the blob
	// JIT-compiles once per Engine instead of once per VM (~75 ms → ~2 ms,
	// note/wazero-performance-note-001.md). Engine sharing does NOT share
	// module namespaces — import resolution goes through each runtime's
	// own store, so the per-VM isolation model is untouched.
	wcache wazero.CompilationCache
	// leaseOnce/leaseRT pin the compiled blob in the shared engine's
	// in-memory cache: compiled modules are refcounted and evicted when
	// the last instance closes (engine.go DeleteCompiledModule), so
	// without a permanent lease the sequential fresh-VM shape would
	// recompile the blob on every run. The lease runtime is never closed
	// while VMs exist (its lifetime belongs to the process — see Close).
	leaseOnce sync.Once
	leaseErr  error
	leaseRT   wazero.Runtime

	// pool is the warm-VM cache (WithVMCacheSize); nil when disabled.
	pool    *lru_ts.Lru[string, *VM]
	poolCap int
}

type registeredFn struct {
	table, name string
	fn          HostFunc
}

type registeredVal struct {
	table, name string
	v           Value
}

// luaIdent reports whether s is a plain Lua identifier — required for
// names that are staged into generated Lua source (RegisterValue).
func luaIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// RegisterValue installs <table>.<name> as a constant Lua value (nil,
// booleans, numbers, strings) in every VM — the redis.LOG_WARNING /
// redis.REDIS_VERSION mechanism. Names must be Lua identifiers (they are
// staged as source). Same freeze rule as RegisterGlobal.
func (e *Engine) RegisterValue(table, name string, v Value) error {
	if !luaIdent(table) || !luaIdent(name) {
		return errors.New("host: RegisterValue requires Lua-identifier table and name")
	}
	switch v.Kind {
	case KindNil, KindFalse, KindTrue, KindNumber, KindInteger, KindString:
	default:
		return errors.New("host: RegisterValue supports only nil/bool/number/string values")
	}
	if v.Kind == KindNumber && (math.IsInf(v.Num, 0) || math.IsNaN(v.Num)) {
		return errors.New("host: RegisterValue number must be finite")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.vms > 0 {
		return errors.New("host: RegisterValue after NewVM (the registry is frozen once VMs exist)")
	}
	for _, r := range e.hostVals {
		if r.table == table && r.name == name {
			return fmt.Errorf("host: RegisterValue duplicate %s.%s", table, name)
		}
	}
	e.hostVals = append(e.hostVals, registeredVal{table, name, v})
	return nil
}

// luaValueLit renders a scalar value as a Lua literal (RegisterValue
// staging; binary-safe string quoting).
func luaValueLit(v Value) string {
	switch v.Kind {
	case KindNil:
		return "nil"
	case KindFalse:
		return "false"
	case KindTrue:
		return "true"
	case KindNumber:
		return numRepr(v.Num)
	case KindInteger:
		return strconv.FormatInt(v.Int, 10)
	case KindString:
		return luaQuote(v.Str)
	}
	return "nil"
}

// NewEngine verifies the runtime blob and returns a ready engine.
func NewEngine(opts ...Option) (*Engine, error) {
	o := options{blob: prodBlob}
	for _, f := range opts {
		f(&o)
	}
	if err := verifyBlob(o.blob); err != nil {
		return nil, err
	}
	e := &Engine{opts: o, cache: map[string]*Script{}, wcache: wazero.NewCompilationCache()}
	if o.vmCacheSize > 0 {
		e.pool = lru_ts.NewLru[string, *VM](o.vmCacheSize)
		e.poolCap = o.vmCacheSize
	}
	return e, nil
}

// RegisterGlobal installs <table>.<name> as a Lua function backed by fn
// (e.g. RegisterGlobal("redis", "call", ...)). Must be called before the
// first NewVM — each VM snapshots the registry at creation.
func (e *Engine) RegisterGlobal(table, name string, fn HostFunc) error {
	if table == "" || name == "" {
		return errors.New("host: RegisterGlobal requires non-empty table and name")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.vms > 0 {
		return errors.New("host: RegisterGlobal after NewVM (the registry is frozen once VMs exist)")
	}
	for _, r := range e.hostFns {
		if r.table == table && r.name == name {
			return fmt.Errorf("host: RegisterGlobal duplicate %s.%s", table, name)
		}
	}
	e.hostFns = append(e.hostFns, registeredFn{table, name, fn})
	return nil
}

// Script is one compiled script: the durable wasm artifact plus its Redis
// identity. Deterministic for a given (source, name) — cacheable by SHA.
type Script struct {
	// SHA1 is the hex SHA-1 of the source bytes — the SCRIPT LOAD /
	// EVALSHA identity (Redis model).
	SHA1 string
	// Wasm is the compiled module — the artifact cmd/luawasmc writes.
	Wasm []byte

	name string
}

// Name returns the chunk name the script was compiled under (error
// position prefixes use it).
func (s *Script) Name() string { return s.name }

// Compile parses and lowers source to a wasm module, caching by
// (SHA-1, name). Compile errors (syntax, v1-unsupported constructs such
// as coroutines) return the frontend's message verbatim.
func (e *Engine) Compile(source []byte, name string) (*Script, error) {
	if name == "" {
		name = "=script"
	}
	sum := sha1.Sum(source)
	sha := hex.EncodeToString(sum[:])
	key := sha + "\x00" + name
	e.mu.Lock()
	if s, ok := e.cache[key]; ok {
		e.mu.Unlock()
		return s, nil
	}
	e.mu.Unlock()

	// the same three-stage chain as testdiff.CompileSource, minus the
	// testdiff import (that package hosts the wasmtime oracle engines —
	// cgo must never reach a daemon through this package)
	chunk, err := parse.Parse(strings.NewReader(string(source)), name)
	if err != nil {
		return nil, err
	}
	proto, err := lua.Compile(chunk, name)
	if err != nil {
		return nil, err
	}
	bin, err := luawasm.Compile(proto, name)
	if err != nil {
		return nil, err
	}
	s := &Script{SHA1: sha, Wasm: bin, name: name}
	e.mu.Lock()
	// A concurrent Compile of the same source may have won the race:
	// pointer identity per (SHA-1, name) is the invariant the VM
	// one-script law (ErrScriptBound) and consumer-side per-script pools
	// rely on — never hand out two pointers for one source.
	if prev, ok := e.cache[key]; ok {
		e.mu.Unlock()
		return prev, nil
	}
	e.cache[key] = s
	e.mu.Unlock()
	return s, nil
}

// Run compiles nothing: it executes s against a fresh VM (the v1 default
// deployment — maximum parallelism, zero shared mutable guest state), or
// against a pooled warm image when WithVMCacheSize is set (repeat scripts
// skip NewVM entirely; each image still serves one run at a time).
func (e *Engine) Run(ctx context.Context, s *Script, opt RunOptions) (Result, error) {
	if s == nil {
		return Result{}, errors.New("host: nil script")
	}
	key := s.SHA1 + "\x00" + s.name
	if e.pool != nil {
		// Atomic take (Get alone re-marks recency but LEAVES the entry —
		// two concurrent Runs would hold one image, and the recycle path
		// could close it under a live run).
		e.pool.Lock()
		var pooled *VM
		if v, ok := e.pool.NlGet(key); ok {
			pooled = v
			e.pool.NlDelete(key)
		}
		e.pool.Unlock()
		if pooled != nil {
			res, err := pooled.Run(ctx, s, opt)
			e.recycle(ctx, pooled, key, err)
			return res, err
		}
	}
	vm, err := e.NewVM()
	if err != nil {
		return Result{}, err
	}
	res, err := vm.Run(ctx, s, opt)
	if e.pool == nil || !poolable(err) {
		_ = vm.Close()
		return res, err
	}
	e.recycle(ctx, vm, key, err)
	return res, err
}

// poolable reports whether err leaves the image reusable: guest-level
// errors unwind the Lua stack to base (per-run hygiene clears the staged
// error and the deadline flag), while engine-level failures — traps are
// always backend bugs, design §6.4 — leave unknown machine state.
func poolable(err error) bool {
	var se *ScriptError
	return err == nil || errors.As(err, &se)
}

// recycle returns vm to the pool, or closes it when its Lua allocation
// crossed the recycle watermark (frozen-GC growth per run; the pool must
// not drift to the memory budget) or when it arrived at a full pool as
// the entry that loses the LRU race.
func (e *Engine) recycle(ctx context.Context, vm *VM, key string, runErr error) {
	if !poolable(runErr) || vm.UsedBytes(ctx) > e.recycleLimit() {
		_ = vm.Close()
		return
	}
	// Compound op under the pool lock (pluto convention): a key already
	// in the pool refreshes its own slot (an entry taken from the pool
	// and a fresh one must be treated alike); a NEW key at capacity pops
	// the LRU victim. Close victims OUTSIDE the lock — Close is too long
	// to run under it, and Put alone would silently drop the victim (a
	// leaked image).
	e.pool.Lock()
	var victim *VM
	if _, mine := e.pool.NlPeek(key); !mine && e.pool.NlLen() >= e.pool.NlCapacity() {
		if _, v, ok := e.pool.NlPopOldest(); ok {
			victim = v
		}
	}
	e.pool.NlPut(key, vm)
	e.pool.Unlock()
	if victim != nil && victim != vm {
		_ = victim.Close()
	}
}

// recycleLimit is the UsedBytes watermark at which pooled images are
// closed instead of reused: three quarters of the budget when one is
// set, else 64 MiB (the blob's 256 MiB linear-memory max backstops
// either way).
func (e *Engine) recycleLimit() int64 {
	if e.opts.maxMem > 0 {
		return e.opts.maxMem * 3 / 4
	}
	return 64 << 20
}

// Close marks the engine closed; live VMs keep working (they own their
// runtimes) but no new ones may be made. Pooled VMs are engine-owned
// and are drained here. The shared wazero engine (the compile cache's)
// outlives every VM runtime, so it is only released when no VM was ever
// created; otherwise its blob machine code lives until process exit —
// bounded (one blob's code per Engine).
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.pool != nil {
		e.pool.Lock()
		victims := make([]*VM, 0, e.pool.NlLen())
		for {
			_, v, ok := e.pool.NlPopOldest()
			if !ok {
				break
			}
			victims = append(victims, v)
		}
		e.pool.Unlock()
		for _, v := range victims {
			_ = v.Close()
		}
	}
	if e.vms == 0 && e.leaseRT != nil {
		_ = e.leaseRT.Close(context.Background())
		return e.wcache.Close(context.Background())
	}
	return nil
}
