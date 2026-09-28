# Lua→Wasm Compiler — Detailed Design & Test Plan

**Date:** 2026-09-03
**Status:** Design
**Companion to:** `docs/Performance-Improvement-Plan.md` §7 (Option E)
**Goal:** A production Lua 5.1 → WebAssembly compiler reusing this repo's frontend (`parse/`, `ast/`, `compile.go`), targeting wazero on linux/amd64 + linux/arm64, with a test regime strong enough to trust it inside a Redis-class network daemon running untrusted client scripts.

**Integration target (updated 2026-09-14):** the daemon is a **pure-Go clone of Redis** — no C code is involved in the daemon: no cgo, no C toolchain, no C sources in the clone's repo. The clone consumes exactly two things from this project: a **Go host package** (the public API, §6) and **prebuilt, checksummed wasm blobs** embedded as data (`//go:embed`). The production engine is wazero precisely because it is pure Go; the wasmtime engines in `testdiff/` exist only as dev/CI differential-oracle hosts and their cgo dependency never ships. The C sources under `runtime/` are *this repo's* build-time concern only (§5): compiled once here into the runtime blob, never shipped as C.

---

## 1. System overview

```
                 ┌───────────────────── Go host (pure-Go Redis clone) ─────────────────────┐
                 │  script cache (SHA→module) · wazero Runtime · host module ("redis")     │
                 │  reply⇄linear-memory conversion · deadline flag · memory caps           │
                 └───────▲──────────────────────────────────▲──────────────────────────────┘
                         │ imports "rt.*"                   │ imports "redis.*"
                 ┌───────┴────────────┐            ┌────────┴──────────────┐
                 │ runtime.wasm       │            │ script.wasm          │
                 │ (C Lua 5.1 runtime │◄───────────│ (per-script, emitted │
                 │  minus lvm; built  │  rt_* calls│  by the new backend) │
                 │  once, embedded)   │            │                      │
                 └────────────────────┘            └──────────────────────┘
                          ▲                                ▲
                          └───── same pipeline, both oracles ─────┐
  source.lua → parse.Parse → lua.Compile → FunctionProto → backend → script.wasm
                 (existing frontend, reused untouched — see §2)
```

Three code-producing pieces and one host piece:

1. **`wasm/`** — a standalone wasm binary emitter (sections, LEB128, encoders for the ~50 instruction forms we generate). No dependencies; usable for any wasm project.
2. **`luawasm/`** — the backend: `[]FunctionProto` (tree) → module. This is the new compiler.
3. **`runtime/`** — C Lua 5.1 runtime adapted to freestanding wasm32, compiled **once, in this repo only**, with wasi-sdk/clang, shipped as a versioned, checksummed blob (`runtime.wasm`) embedded via `//go:embed`. To the daemon the blob is data, not C — nothing C crosses the boundary (A8).
4. **Host package (`host/`, §6)** — the public Go API the Redis clone imports: wazero wiring, instantiation, host imports (`redis.*`), reply⇄TValue conversion, lifecycle, and the per-VM memory-image lock. This is the entire integration surface.

### 1.1 Key architectural decisions (beyond the plan doc's §7)

| # | Decision | Rationale |
|---|---|---|
| A1 | **One script module imports two modules (runtime, host); the runtime never calls back into script code.** All Lua→Lua calls compile to `call_indirect` on a funcref table *inside* the script module. | Kills the circular-import problem entirely. Works because in the Redis model every function a script can call is either its own proto (in-module) or a host/C function (downward call). |
| A2 | **Single wasm calling convention for every emitted Lua function** (see §4.2); results returned via the frame, count as i32 return. | Uniform handling of multret/vararg; no reliance on wasm multi-value; one table, one signature → cheap `call_indirect`. |
| A3 | **v1 control flow is flattened** (`loop` + `br_table` over basic blocks); v2 adds structured-region emission (if/loop nesting) where the jump graph is reducible, falling back per-function. | Lua 5.1 bytecode has arbitrary gotos (this fork even accepts Lua 5.2 `goto`); wasm requires structured control. Flattened costs one indirect branch per *basic block* (not per instruction) and is unconditionally correct — v2 is a pure optimization. |
| A4 | **Line numbers are passed as immediates to every throwing runtime call.** `DbgSourcePositions[pc]` is known at emit time; `rt_*` signatures take a `line i32`. | Error messages get correct line info for free, making **byte-exact error-message diffing** against the oracle possible (§8.3). |
| A5 | **Deadline via direct memory write.** Host writes a flag at a fixed linear-memory address; emitted loop back-edges load it and branch to `rt_deadline`. | No host call on the hot path; replaces the interpreter's per-instruction `select` on `ctx.Done()`. |
| A6 | **Arena reset instead of GC for the common case.** Stateless script runs bump-allocate and reset; mark-sweep (frames-as-roots) engages only past a per-run memory threshold. | Most Redis scripts never collect. GC becomes a safety valve, not a component on the critical path. |
| A7 | **Bring-up vehicle: stock C Lua 5.1 compiled whole (including `lvm.c`) to wasm first.** | Validates the toolchain and gives an in-wasm oracle *before the backend exists* (§8.4). The backend then progressively replaces `lvm.c` calls with inline code — same seam LuaJIT used. |
| A8 | **The daemon is a pure-Go Redis clone: production engine is wazero, zero cgo, no C sources or C toolchain in the clone's repo.** The C under `runtime/` is this repo's build-time concern; the daemon consumes the Go host package (§6) plus embedded, checksummed wasm blobs. wasmtime stays dev/CI-only (oracle host). | Cross-compilation and deployment stay trivial; the security story for untrusted scripts lives entirely in wasm + Go. **(M6a ruling, ledger row 32):** wazero v1.12 implements the standardized EH proposal (`experimental.CoreFeaturesExceptionHandling`) in the blob's dialect — the same SJLJ blob runs on both hosts; the former "EH-free flavor" item is obsolete. |
| A9 | **Concurrency: one lock per Lua memory image.** A *VM* owns the whole guest state of one running Lua — script-module instance + runtime-module instance + shared linear memory + control block — guarded by a single `sync.Mutex`; `VM.Run` holds it end-to-end. Concurrent Runs on one VM serialize; VMs are independent and run in parallel. | A multi-goroutine daemon gets thread-proofing as the API unit (§6.2), not an internal courtesy; host trampolines execute on the lock-holding goroutine so reentrancy cannot deadlock. Examples demonstrate it under `go test -race` (§6.3). |

---

## 2. Reused from this repo (unchanged)

| Component | Location | Note |
|---|---|---|
| Lexer/parser | `parse/lexer.go:468` (`Parse → []ast.Stmt`), `parser.go.y` | untouched |
| AST | `ast/` | untouched |
| AST→bytecode | `compile.go:1849` (`Compile → *FunctionProto`) | untouched; carries scoping, upvalue capture, vararg lowering, constant handling |
| IR | `FunctionProto` (`function.go:25`) | 41 opcodes; `NumUsedRegisters`, `NumParameters`, `IsVarArg`, `NumUpvalues`, `Code`, `Constants`, `FunctionPrototypes[]`, `DbgSourcePositions[]` |
| Oracles + corpora | `_lua5.1-tests`, `_glua-tests` | the interpreter in this repo is the primary differential oracle; C Lua 5.1 native is the semantic authority |

Not reused on this path: `vm.go`/`_vm.go`, `state.go`, `table.go`, `alloc.go` (the interpreter keeps them; the wasm path replaces them).

---

## 3. Linear memory map (script instance)

Fixed layout, compile-time constants shared by backend, runtime, and host:

```
0x0000_0000  null guard (never mapped use; trap on deref)
0x0000_1000  control block:
               +0x00 err_flag i32        (runtime→code propagation)
               +0x08 err_value TValue    (error object)
               +0x10 deadline i32        (host writes; code polls at back-edges)
               +0x18 mem_watermark i32   (per-run allocation threshold)
               +0x20 gc_active i32
               +0x28 frame_top i32       (current frame-stack top)
               +0x30 stack_limit i32
0x0001_0000  frame stack (grows up; N slots × 16B TValue; capped, overflow → rt_error)
0x1000_0000  heap arena (bump pointer rt_alloc; mark-sweep above watermark)
0x8000_0000  args/results staging area (host writes KEYS/ARGV blobs; script returns here)
0xF000_0000  interned-constant handles + closure descriptors (module data)
```

### 3.1 Value representation

- **In memory (heap, frame slots, table cells):** 16-byte `TValue { u32 tag; u32 pad; f64 num; u32 ref }`.
  Tags: `0 nil, 1 false, 2 true, 3 number, 4 str, 5 table, 6 func, 7 userdata, 8 thread`.
- **In wasm locals (compiler convention):** a register is three locals `(t i32, n f64, r i32)`; exactly one of `n`/`r` meaningful per `t`. Constants: numbers inline as `f64.const`; strings interned at module init (data section → `rt_intern` → handle table at fixed offsets, loaded with `i32.load` of a constant address); nil/true/false are tag immediates.
- `ref` is a 32-bit offset into the arena — never a raw anything the host GC could care about. (Your GC, your rules: heap-cell NaN-boxing is a legal later optimization, unlike in the Go heap.)

### 3.2 Register placement (from §7.3 of the plan, made precise)

`compile.go`'s upvalue descriptors identify, per proto, which registers any nested `OP_CLOSURE` captures.

- **Captured registers** → frame slots in linear memory (addressable by open-upvalue objects).
- **Unaptured registers** → wasm locals.
- `OP_GETUPVAL`/`OP_SETUPVAL` → load/store through the closure's upvalue array (open: indirected to frame slot; closed: cell in the object).
- `OP_CLOSE`/function return → `rt_close_upvalues(frameSlotAddr)` — doubly-linked open-upvalue list exactly as in C Lua.

For-loop registers A..A+2 (hidden control vars) are **always** wasm f64 locals — they are uncapturable by construction; only the visible copy A+3 is materialized each iteration.

---

## 4. The backend (`luawasm/`)

### 4.1 Module skeleton

```wat
(module
  (import "rt"    "alloc"        (func $rt_alloc (param i32) (result i32)))      ;; ~30 imports
  (import "rt"    "gettable"     (func $rt_gettable (param i32 i32 i32 i32)))    ;; L tbl dst line
  (import "rt"    "settable"     ...)
  (import "rt"    "arith"        ...)   ;; op lhs dst line  (metamethod/coercion path)
  (import "rt"    "concat"       ...)
  (import "rt"    "newtable"     ...)
  (import "rt"    "newclosure"   ...)   ;; protoIdx upvalDescPtr -> handle
  (import "rt"    "find_upval"   ...)
  (import "rt"    "close_upvals" ...)
  (import "rt"    "call_cfn"     ...)   ;; C/host function dispatch
  (import "rt"    "error"        ...)   ;; msg TValue line
  (import "rt"    "stackoverflow" ...)
  (import "redis" "call"         ...)   ;; host functions (ptr len) -> (ptr len)

  (table $protos N funcref)              ;; one entry per FunctionProto, proto index = slot
  (func $lua_init  ...)                  ;; intern constants, build closure descriptors
  (func $lua_main  (param i32) (result i32))  ;; host entry: want -> nret, results staged
  (func $p0 $p1 ... $pN)                 ;; one per proto, common signature, all in $protos
  (memory 1 ..max)
  (export "lua_main" ...) (export "lua_init" ...))
```

### 4.2 Calling convention (every emitted Lua function)

```
sig: (param $L i32)         ;; thread/state pointer (current frame base etc.)
     (param $frame i32)     ;; absolute frame-slot base for this activation
     (param $nargs i32)
     (param $want i32)      ;; >=0: exactly want results; -1: multret
     (result $nret i32)     ;; results occupy $frame..$frame+$nret
```

- Callee sets up its window: uncaptured registers → locals; captured ones are already addressable at `$frame + k*16`.
- Stack-depth check at entry against `stack_limit` → `rt_stackoverflow` (message parity with the interpreter).
- `OP_CALL` lowering:
  1. load callee `TValue` from register A;
  2. tag = Lua-closure → `call_indirect $protos (local.get protoIdx)` with callee frame at `frame + (A+1)*16`;
  3. tag = C/host function → `rt_call_cfn`;
  4. else → `rt_call_meta` (`__call` metamethod loop, in runtime);
  5. after any call: `if err_flag != 0 { return -1 }` — the entire error-propagation protocol.
- `OP_TAILCALL` → trampoline: write a call-descriptor (callee, frame, nargs) into the thread state and return; `$lua_main` and every `call` site run a small loop that re-dispatches descriptors — `return f(x)` recursion is O(1) wasm stack.
- `OP_RETURN` with B=0 (multret): copy from `$frame` up to current top; else fixed count + nil padding (matches `copyReturnValues` semantics in `_vm.go:70`).

### 4.3 Control flow

- v1 (correctness first): function body = basic blocks; `loop $next { block … br_table }` on a block-id local; each block ends by setting `next_id` and `br $next`. All register state lives in locals, which persist across blocks.
- v2 (perf): structural analysis over the jump graph (loop detection via back-edges, if/else region formation) → nested wasm `block`/`loop`; irreducible functions (rare; `goto`-heavy code) fall back to v1 per-function. Emit both under a flag and **differential-test them against each other** (§8.5).

### 4.4 Opcode lowering table (complete inventory, 41 opcodes)

| Opcode(s)                                       | Lowering                                                                                                                                                                       |
|-------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `MOVE`, `MOVEN`, `LOADK`, `LOADBOOL`, `LOADNIL` | pure inline local moves / immediate tags / constant-handle loads                                                                                                               |
| `GETUPVAL`, `SETUPVAL`                          | inline: closure upvalue array load/store                                                                                                                                       |
| `GETGLOBAL`, `SETGLOBAL`                        | `rt_gettable/settable` on the globals-table handle with the interned name handle (globals are a table; env chain per closure)                                                  |
| `GETTABLE`, `GETTABLEKS`                        | fast path inline: table-tag ∧ number-key ∧ `0 < key ≤ array_len` → bounds-checked `i32.load` of the array part; else `rt_gettable`                                             |
| `SETTABLE`, `SETTABLEKS`                        | symmetric; the runtime call also owns the write barrier when GC is active                                                                                                      |
| `NEWTABLE`                                      | `rt_newtable(hintFromBC)`                                                                                                                                                      |
| `SELF`                                          | `rt_gettable` + register move                                                                                                                                                  |
| `ADD..POW`, `UNM`                               | fast path: both tags `number` → f64 op inline; else `rt_arith(op,…)` (string coercion, `__add`… )                                                                              |
| `NOT`, `TEST`, `TESTSET`                        | inline tag tests                                                                                                                                                               |
| `LEN`                                           | `rt_len` (string len inline fast path: tag=str → i32.load header)                                                                                                              |
| `CONCAT`                                        | `rt_concat(frame, start, count, line)` (right-associative, `__concat`)                                                                                                         |
| `JMP`                                           | branch (v1: set next-id; v2: wasm br)                                                                                                                                          |
| `EQ`                                            | fast: number×number → f64 cmp; string×string (both interned) → `i32 eq` on handles; else `rt_equals`                                                                           |
| `LT`, `LE`                                      | fast: number×number; else `rt_lessthan` (mixed-type errors, `__lt`/`__le`)                                                                                                     |
| `CALL`, `TAILCALL`, `RETURN`                    | §4.2                                                                                                                                                                           |
| `FORPREP`, `FORLOOP`                            | `FORPREP`: guard/convert A..A+2 to numbers once (`rt_forprep` handles metamethod/coercion errors) → f64 locals; `FORLOOP`: pure inline f64 add + compare + visible copy at A+3 |
| `TFORLOOP`                                      | runtime-driven: call iterator via §4.2 mechanism, nil test inline                                                                                                              |
| `SETLIST`                                       | `rt_setlist(tbl, frameSlot, n, baseIndex)` (flush at `FieldsPerFlush`)                                                                                                         |
| `CLOSE`                                         | `rt_close_upvals`                                                                                                                                                              |
| `CLOSURE`                                       | `rt_newclosure(protoIdx, descPtr)` — capture descriptor in module data                                                                                                         |
| `VARARG`                                        | copy from vararg area of frame (`frame + np*16 …`), nil-pad to B                                                                                                               |
| `NOP`                                           | nothing                                                                                                                                                                        |

**Fidelity note:** every `rt_*` behavior contract is written down as "what `_vm.go` does today" — the interpreter source is the executable spec (e.g. `opArith` `_vm.go:831`, `stringConcat` `_vm.go:930`, `lessThan` `_vm.go:967`, `equals` `_vm.go:989`, vararg frame shuffle `state.go:1192-1240`).

### 4.5 Init & host entry

- `$lua_init` (host calls once after instantiate): intern all string constants into the handle table, build closure descriptors, create the globals table, seed `math.random` (host-provided seed).
- `$lua_main(want)`: push KEYS/ARGV as TValues from the staging area, invoke the script proto, return `nret`; results TValues already in the staging area for host conversion.
- Per-run lifecycle v1: **fresh instance per execution** (wazero instantiation is µs–low-ms). v2 option: `rt_reset()` (arena top ← start; globals snapshot restore; interned strings retained) — measure before adopting.

---

## 5. Runtime module (C Lua 5.1 port)

- **Source**: Lua 5.1.4 `ltable.c lstring.c lgc.c lmem.c lobject.c ltm.c lvm.c(arith/compare helpers only) lapi.c(subset) lfunc.c ldo.c(structure only) lauxlib(subset)` — everything except the bytecode interpreter loop; MIT license.
- **Toolchain**: `clang --target=wasm32-unknown-unknown -nostdlib` (or wasi-sdk with a stub libc: `memcpy/memset/memmove/strlen` only). Built once per release, checksummed, embedded in the Go binary via `//go:embed`.
- **Build flavors (A8, updated M6a — ledger row 32)**: one blob, two hosts. The SJLJ/new-EH flavor (`lua51_sjlj.wasm`) serves dev/CI as oracle and differential engine on wasmtime **and runs unchanged as the production engine on wazero** (`experimental.CoreFeaturesExceptionHandling`; the M5 error-safety invariant keeps every throw inside the blob's frames, so the host never propagates an exception). The formerly planned EH-free flavor is obsolete; the Asyncify path (`build.sh --with-asyncify`, `runtime/setjmp_asyncify.c`) remains buildable documentation. Gate: full corpus green on wazero (`TestWazeroFullMatrix/ErrorSuite/GluaFull`) — met at M6a; the black-box CLI suite on the production engine (`_cli-tests: make test WAZERO=1` — every wasm leg swapped via `GLUA_WASM_ENGINE`, same golden files) and perf sanity (`testdiff/m6bperf_test.go`: wasmtime ≡ wazero byte-exact under load; wazero/wasmtime ≈ 15–24x on darwin/arm64's interpreter engine, the conservative case — trip-wire at 75x) — met at M6b. **Production sandbox flavor (M6c — ledger rows 33–34)**: `lua51_prod.wasm` (`-DLUAWASM_PROD`, same EH dialect and ABI) opens base/table/string/math only and imports exactly the five `host.*` functions — zero `wasi_snapshot_preview1` imports (fs/env/clock never enter the production host); `rt_sandbox(1)` applies the §4.3 globals lockdown on either blob. Gates: `TestM6cProdBlobArtifact` (import-section self-check + SHA-256 pin), `TestM6cSandboxSurface/Matrix` (dev+sandbox ≡ prod, 517/517), CLI `interp,wazero-prod` legs 517/517 + 94/94, and the D5 determinism gates (5×-identical corpus, host-seeded RNG anchors, twice-run isolation, 10⁷ tail chain).
- **setjmp/longjmp → error protocol**: `rt_error` sets `err_flag/err_value` and returns; callers (emitted code) early-return; `pcall` = `rt_pcall` saves frame/stack state, invokes, restores on flag — mirrors `PCall` semantics in `state.go:2029`.
- **Allocator**: bump arena (`rt_alloc`), watermark check, mark-sweep collector (roots: frame list, globals, registry, upvalue list) run inside `rt_alloc` only when past watermark.
- **Strings**: interned hash table (cached 32-bit hashes — the fix §2b of the plan wanted, now free); `string.format` etc. ported from `lstrlib.c` (deterministic subset).
- **Exports**: the `rt_*` surface of §4.4, frozen and versioned (`LUA_RT_ABI = 1`); the backend refuses modules whose runtime ABI doesn't match.
- **In-wasm determinism**: no clock, no `os.*`, host-seeded PRNG.

---

## 6. Host package (Go) — the public API the Redis clone calls

Everything the daemon needs is one import: package `host` (pure Go, wazero underneath, blobs embedded — A8). No cgo, no C toolchain, no C sources.

### 6.1 Interface

```go
package host

// Engine: process-wide. Owns the shared wazero Runtime, the runtime blob,
// and the compiled-script cache. Safe for concurrent use.
type Engine struct {
    // rt    wazero.Runtime        (shared)
    // blob  []byte                (runtime.wasm, embedded + checksummed)
    // cache map[SHA1]*Script
}

func NewEngine(opts ...Option) (*Engine, error)

// Compile: the EVAL path. source → parse → lua.Compile → backend → wasm,
// cached by SHA1 (Redis parity). SCRIPT LOAD = Compile; EVAL = Compile+Run;
// EVALSHA = Run from cache; SCRIPT FLUSH = cache drop.
func (e *Engine) Compile(source []byte, name string) (*Script, error)

type Script struct {
    SHA1 []byte
    Wasm []byte        // the durable artifact cmd/luawasmc writes/reads
    Meta ScriptMeta    // proto/line tables for Go-side stack traces
}

type RunOptions struct {
    Keys, Argv []string  // staged into the args area as TValues
    Deadline   time.Time // watchdog writes ctrl+0x10; loops poll at back-edges
    MaxPages   uint32    // instance memory cap → rt_oom error, not host OOM
}

// VM: one Lua memory image — script-module instance + runtime-module
// instance + the shared linear memory + control block — plus the mutex
// that makes it thread-proof (A9). All guest state lives in the image.
type VM struct {
    // mu   sync.Mutex
    // inst api.Instance (script), rtInst api.Instance (runtime)
    // mem  api.Memory   (shared linear memory)
}

func (e *Engine) NewVM() (*VM, error) // fresh image; µs-cheap (M0 measured ≈7.4 µs)

// Run executes s against this VM's memory image. It holds vm.mu from entry
// to result: one execution at a time per image, any number of images in
// parallel. Safe to call from any goroutine, any number of them.
func (vm *VM) Run(ctx context.Context, s *Script, opt RunOptions) (Result, error)
func (vm *VM) Close() error

// Engine.Run: convenience — fresh VM per call. The v1 lifecycle and the
// default deployment: maximum parallelism, zero shared mutable state.
func (e *Engine) Run(ctx context.Context, s *Script, opt RunOptions) (Result, error)

type Result struct { /* reply values, Tier-3 converted (§6.4) */ }
```

### 6.2 Concurrency contract (thread-proofing, A9)

- **The unit of exclusion is the VM — the Lua memory image.** `VM.Run` takes `vm.mu` for the entire execution. Fine-grained locking inside the image is impossible by construction (the guest state is one linear memory plus two module instances), so the whole image is the lock scope. Concurrent `Run`s on one VM serialize; distinct VMs run in parallel on distinct goroutines.
- **Reentrancy is same-goroutine.** Host trampolines (`redis.call`, and the M5 `host.wasm_dispatch` callback) fire on the goroutine that holds the lock — no deadlock — and the API provides no path for a different goroutine to re-enter a running VM.
- **Deployment modes** (policies over `NewVM`/`Run`, not engine changes):
  1. *Fresh VM per execution* — default (v1 lifecycle): instantiation is µs-cheap, scripts share nothing, full parallelism.
  2. *VM pool* — `sync.Pool`-style reuse, adopted with the v2 `rt_reset()` lifecycle (§4.5) if measurement favors it.
  3. *Single shared VM* — Redis-classic semantics: one global Lua state, scripts see each other's globals; the image lock then behaves exactly like Redis's single-threaded script execution.
- **Gate**: `go test -race` over the host package and examples is a CI requirement (§8.6 Concurrency).

### 6.3 Examples (deliverable; they double as integration tests)

`examples/`, all runnable and `-race`-clean:

1. `examples/eval` — minimal: compile once, run with keys/argv, print the reply.
2. `examples/evalserver` — a goroutine-per-"client" server over a pool of locked VMs: concurrent EVALs, a deadline kill, per-VM serialization — the thread-proofing demonstration.
3. `examples/sharedvm` — single-shared-VM mode: serialized scripts, shared globals.
4. `examples/bench` — the perf harness (§8.10): interpreter vs backend(wazero) vs backend(wasmtime) vs **C Lua 5.1 native**, same box.

### 6.4 Host module & mechanics

- Host module `"redis"`: `call/pcall(ptr,len,argc,argvArea) -> (ptr,len)` implemented over `api.Module.Memory()`; reply→TValue writer emits Tier-3 typed tables (`[]float64` array part / string hash) directly; `error_reply`/`status_reply`.
- Deadline: `mem.WriteBool(ctrlDeadline, true)` from a watchdog — no guest involvement until the next back-edge.
- Memory cap: instance max pages + watermark → `rt_oom` error object; the daemon returns `-BUSY`/script error per policy.
- Errors escaping as wasm traps are **always a backend bug** (§8.7 classification) — logged with module SHA + meta, never surfaced as a script error.

---

## 7. Test strategy — overview

**Backbone principle: differential testing against two independent oracles.**

| Oracle                                                                | Role                        | Notes                                                                                                        |
|-----------------------------------------------------------------------|-----------------------------|--------------------------------------------------------------------------------------------------------------|
| **gopher-lua interpreter (this repo)**                                | primary differential oracle | same frontend, same test corpus, run-for-run comparison; catches backend divergence with zero setup          |
| **C Lua 5.1 (native, via wasmoon-style build of stock Lua — see M3)** | semantic authority          | where the interpreter itself deviates from Lua 5.1, C wins; every known divergence goes in the ledger (§8.9) |

Equality oracle = **normalized event log** (not just final results):

1. Every `print`, `error`, `assert` failure, and pcall-captured error → one log line, normalized.
2. End of run → deterministic serialization of all globals (sorted-key traversal), returned values, and (in a special `--inspect` mode) any tables reachable from them.
3. Byte-compare logs. A diff is a failure regardless of which side is "wrong" — then the ledger decides.

Test pyramid:

```
L7  production soak (fuzz 24/7, Go Redis clone integration, perf gates incl. C-Lua comparison)
L6  conformance suites   (_lua5.1-tests, _glua-tests, curated real-world Redis scripts)
L5  differential fuzzing (random Lua programs, oracle comparison)
L4  end-to-end scripts   (multi-feature programs, coroutines of features per file)
L3  per-opcode semantic tests (41 opcodes × edge-case matrix)
L2  component tests      (emitter, ABI, runtime C, host package: API + VM locking under -race)
L1  unit tests           (LEB128, sections, TValue codec, allocator)
```

Every layer gates a milestone (§9). CI partitions: per-commit = L1–L4 (~minutes); nightly = L5–L6 + sanitizers; weekly = perf gates.

---

## 8. Test design, layer by layer

### 8.1 L1/L2 — component tests

**Emitter (`wasm/`):**
- Round-trip: emit → decode with wazero's module reader → re-encode → byte-identical.
- External validation: CI job runs `wasm-tools validate` / `wasm2wat` (wabt) over a corpus of emitted modules.
- Every instruction form has a micro-module whose *execution result* on wazero is asserted (don't test bytes, test behavior).
- Malformed-input tests: emitter must error cleanly on impossible requests (never produce an invalid module).

**Backend ABI:**
- Hand-written 3-proto micro modules exercising the calling convention: fixed-arity, multret, vararg, want=-1, missing-arg nil padding — expected register/frame layouts asserted via exported test hooks (a `#ifdef LUAWASM_TEST` export that dumps frames).
- Error-propagation contract: any `rt_*` setting `err_flag` must produce `nret=-1` from every level — tested with synthetic injected errors.

**Runtime (C):**
- The same C sources compile **natively** with a test harness (`runtime/tests/`): unit tests for allocator, table ops (incl. the `Next()` ordering semantics), string interning, upvalue open/close.
- Native build runs under ASan+UBSan in CI. Pointer width differs from wasm32 — the runtime keeps refs as `uint32_t` offsets internally precisely so the native build is layout-identical, not just logic-identical.
- wasm32 build additionally gets wasi-sdk's experimental wasm ASan when available; primary wasm safety net is that OOB = trap = loud failure.

### 8.2 L3 — per-opcode semantic matrix

For each of the 41 opcodes: a table in `luawasm/opcode_test.go` of (Lua snippet isolating the opcode, inputs, expected outputs, expected event log). The edge-case checklist each opcode must survive:

- Numbers: NaN (`NaN==NaN` false, `NaN<1` false, NaN keys error on table store), ±0, ±Inf, `1e308*10`, integer-valued floats, `-0//1`-style modulo signs (`luaModulo` semantics), `math.pow` edge powers.
- Coercion: `"10"+1`, `"0x10"+0` (5.1 rules), `"abc"+1` → error with exact message+line, string→number in for-loops.
- Table keys: `t[1]` vs `t["1"]` vs `t[1.0]` identity; array-boundary writes (`t[#t+1]`), holes, `#t` with trailing nils; large keys → hash part (`MaxArrayIndex` boundary from `config.go`).
- Closures/upvalues: the classic `for i=1,3 do f[i]=function() return i end end` (each closure distinct, values 1..4 per 5.1 semantics); break-from-loop closing; upvalue shared across two closures; `OP_CLOSE` via `break`.
- Varargs: `select('#')`, nil-padded varargs (`select('#', nil, nil) == 2`), `...` in non-vararg function (compile error parity), vararg + fixed params layout (the frame shuffle at `state.go:1192-1240`).
- Metamethods: each of `__index/__newindex/__call/__eq/__lt/__le/__unm/__concat/__len` (table and non-table operands), `__index` chained tables, `__newindex` function vs table, metamethod erroring mid-dispatch, metamethod invoked **exactly once** (property test with counters).
- Errors: non-string error objects (tables/nil), `error()` with level 0/1/2, pcall of pcall, error inside metamethod, error inside `__gc`-adjacent paths (out of scope: no `__gc`), stack-overflow message parity at the exact recursion depth (needs identical frame accounting — assert message and that depth differs by ≤ ε or matches documented divergence).
- Concat: right-associativity with numbers (`1 .. 2 .. 3`), `__concat` on left then right, long chains.
- Control: `goto` (this fork's 5.2 extension) — forward and backward (loop continue patterns) — the flattening fallback's stress test.
- Equality: `1 == 1.0` true; table identity by handle; string vs number never equal in `==` (but coerced in arith) — assert against oracle.

Each snippet runs in **all three engines** (interpreter, C-Lua-wasm, backend-wasm) and event logs must match; C divergences land in the ledger.

### 8.3 L4 — end-to-end + error-message fidelity

- Multi-feature programs (each combining ≥6 opcode classes, closures, metamethods, string lib).
- **Byte-exact error message suite**: a corpus of ~100 scripts each raising a specific error; assert identical (message, line) triples across engines — enabled by decision A4 (line immediates).
- Debug parity: `debug.traceback()` shape under a flag (or documented divergence + ledger entry).

### 8.4 L5 — differential fuzzing

- **Generator**: grammar/AST-based random Lua program generator (seeded, shrinking support). Reuse this repo's AST to generate *valid* programs by construction, then print them back to source — guarantees syntactic validity while exercising semantic corners. Mutations: swap operators, wrap subexpressions in `(… or nil)`, change literal kinds, insert `pcall` wrappers.
- Run: interpreter vs backend (and nightly vs C-Lua-wasm), compare event logs; any mismatch minimizes the seed and files a regression test automatically.
- **Bytecode-level fuzz**: mutate `FunctionProto` trees (register indices, RK bits, jump targets) — backend must either compile-and-agree with the interpreter running the same mutated proto (the interpreter is fed identical protos via an internal test hook) or reject the proto cleanly. Never trap.
- **Emitter fuzz**: random module shapes → wazero instantiate must succeed or our validator rejects pre-emptively.
- Budget: nightly corpus growth with dedup; 24/7 soak during M5–M6.

### 8.5 L6 — conformance

- `_glua-tests` and `_lua5.1-tests` run through the differential harness (they already run against the interpreter; add the backend and C-Lua-wasm as columns). Explicit, maintained skip list (os/io-dependent, interpreter-known-bugs) — **every skip has a reason string**; CI fails on skips without one.
- **Curated real-world corpus**: collect public Redis Lua scripts (rate limiters, locks, token buckets, cjson/cmsgpack users from Redis docs and common libraries) — run through the full oracle matrix. This is the corpus that actually predicts production behavior for the Go Redis clone.

### 8.6 L7 — production hardening tests

- **Isolation**: run each corpus script twice in the same instance (v2 lifecycle) / new instance (v1) — globals and arena must be indistinguishable from a cold run (memory-diff the globals serialization).
- **Concurrency (A9)**: N goroutines × M locked VMs (fresh-image and pool modes) run the corpus under `go test -race` — zero reports; the single-shared-VM mode serializes Runs on the image lock and observes strictly sequential globals (Redis-classic semantics); host trampolines (`redis.call`) execute only on the lock-holding goroutine — asserted with a goroutine-id check inside the trampoline.
- **Deadline**: infinite-loop scripts must be killed within `deadline + ε` (measure the back-edge check interval), returning the Redis-documented error.
- **Memory caps**: allocation-heavy scripts hit the watermark → clean OOM error, not a trap or host OOM.
- **Recursion**: non-tail deep recursion → "stack overflow" error (not a wasm trap) at a deterministic frame count; tail recursion 10⁷ deep → success, flat memory.
- **Determinism**: every L6 corpus script executed 5× — byte-identical event logs; `math.random` sequences reproducible from host seed.
- **Chaos host**: `redis.call` shim that returns wrong types, huge replies, and errors mid-iteration — engine must survive all (these are host-behavior tests of the conversion layer).
- **Soak**: 24h mixed corpus at randomized deadlines/caps; asserts no memory growth (host-side RSS slope), no trap classes.

### 8.7 Failure classification (drives CI severity)

| Class            | Meaning                                               | CI action                                          |
|------------------|-------------------------------------------------------|----------------------------------------------------|
| TRAP             | wasm trap (OOB, div-by-zero miscompiled, unreachable) | **always a backend bug**; blocks                   |
| DIVERGE          | event-log mismatch vs interpreter                     | blocks; ledger decides fix-vs-document             |
| C-DIVERGE        | interpreter and C Lua disagree                        | ledger entry; blocks only if backend ≠ interpreter |
| TIMEOUT-DEADLINE | deadline honored                                      | expected behavior test                             |
| OOM              | watermark honored                                     | expected behavior test                             |

### 8.8 Semantic coverage instrumentation

The differential harness logs an opcode histogram per script (interpreter side). A coverage report maps every corpus test to opcodes exercised; CI fails if any opcode's semantic matrix (8.2) isn't fully covered by at least one automated layer. Dead opcodes (`NOP`, `MOVEN` corner arity) must still have their rows green.

### 8.9 The divergence ledger

`docs/Lua-Wasm-Divergence-Ledger.md` — one row per known interpreter↔C-Lua↔backend difference: what, why, oracle ruling, ticket. Seed it from the README's "Differences between Lua and GopherLua" section. **Rule: a divergence without a ledger row is a bug; a row without a test is a bug.**

### 8.10 Performance testing

- Benchmark corpus = Week-0 corpus from the plan doc (string ops, cjson decode, table building, redis.call-heavy, numeric loops) + standard Lua benchmarks (fannkuch, nbody, binary-trees) for external comparability.
- **Mandatory comparison columns in every perf run** (fixed CI hardware, `benchstat`):
  1. gopher-lua interpreter (this fork) — the incumbent being replaced;
  2. this backend on **wazero** — the production configuration;
  3. this backend on wasmtime — isolates backend cost from engine cost;
  4. **stock C Lua 5.1, built natively on the same CI box** — the head-to-head reference. The question this project ultimately answers is how close compiled-to-wasm Lua gets to the C implementation of Lua, so every perf run reports the ratio `backend(wazero) / lua5.1-native` per benchmark, trended week-over-week like every other column.
- Gates at M8: backend-wasm ≥ 2× interpreter fork on the numeric corpus; no >5% week-over-week regression on any column **including the C-Lua ratio**; memory: peak arena per corpus script trended. Parity with native C Lua is the stretch goal, not a gate — the M8 report must state where the compiled path stands against C Lua and what would close the remaining gap (v2 control flow, inline caches, arena).
- The harness is runnable standalone: `examples/bench` against a stock `lua` 5.1 binary on PATH.

---

## 9. Milestones — each with a hard test gate

> **Status (2026-09-26): the 5.2→5.5 upgrade's U2 COMPLETE (see docs/Upgrade-from-5.2-to-5.5-of-Lua.md and docs/U2-implementation-plan.md).** U0–U2f landed: the 5.5.1 oracle engines (clua55, both hosts); the 5.2 core (_ENV, escapes, D8 removals); the integer value model + 5.3 operators + the stock 5.5.1 encoding cutover in Go; ABI v4 on the zero-patch 5.5 tree (rt_abi55.c → lua55_rt.wasm, the CClosure seam, ledger rows 60); the wasm backend rewritten onto the new encoding + v4 (row 61 — including the 5.5 forced-GC-step discovery: luaL_pushresult's box-backed buffers force lua_gc(LUA_GCSTEP) under a user stop, freezing every pool/frame-held string; rt_set_state sets GCSTPGC so lua_gc refuses everything until the U6 arena); and U2f (row 62): the interp's error surface re-pinned to stock (concat/index/call/compare/string-arith texts, assert passthrough, error() level semantics via stockWhere), `_wasm-tests` regenerated to 1438 cases with the int/float/bitwise/shift/for grids, and the oracle legs (TestU2fMatrixOracle/TestU2fErrorOracle — interp↔clua55) green. Open into U3+: the row-60/62 seams (error-position marker + varinfo hints → U5), the row-61 life arg-window bug, D7 float printing (U5), and the U3 stdlib program.

> **Status (2026-09-15): M5 COMPLETE — all 41 opcodes, byte-exact errors, the full-suite gate.** Five sub-milestones, one commit each. **A1** ABI v3 skeleton: `Proto.wasm_idx`, the `precall_wasm` adapter (ldo.c — CallInfo + frame push + arg split + dispatch through `host.wasm_dispatch`, the rawrunprotected/restore error law), the proto registry, chunked frame stack, rt-owned upvalue registry. **A2** reentrancy spike PASS — nested C→host→script dispatch on one wasmtime store proven; `luaF_newLclosure` leaves `l.p` unset (stock `pushclosure` sets it — the adapter must too). **A3** all protos compile to `(frame,cl,nargs,want)→nret` functions; `lua_dispatch` br_table + restage loop; thin `lua_main`; global constant pool; `-1` error convention (dispatch arms `Br(n-k-1)` — `Br(n-k)` targets the loop = infinite re-dispatch). **A4** closures/upvalues/CLOSE + the callback matrix — the reentrancy law: any `rt_*` body that `luaD_call`s can re-enter the ABI, so argument statics read after the call must live on the C stack (`rt_call`'s `ca_args/ca_w` were overwritten by nested calls — results landed in the inner's arg cells). **M5b** varargs + compat `arg` (scratch moved to the adapter's +1 spare cell above the vararg staging area — multret destinations reach `R(A+nv-1)` past `nregs`). **M5c** the tailcall trampoline: staged tailcalls re-dispatch at one adapter level via the `-2` sentinel — O(1) wasm stack and frames (10⁶-deep ≈ 0.7s); the sticky-staging fix (`luaD_pcall` recovery calls `rt_pcall_caught`). **M5d** the gopher dialect (`rt_set_dialect(1)`, wasm engine only): every core error family byte-exact; the activation line-stack (per live rt_run) drives `error()` levels with gopher's frame arithmetic (C frames transparent; the main chunk owns the bottom entry — no CallInfo); `err_prefixed` sticky per in-flight error; non-string uncaught errors render from the staged TValue; row 5 (os.time/date) resolved. **M5e** full-suite gates + two more fixes: `__len` metamethod honored (rt_len gap), and wasm-frame guards in ldebug.c (registered protos carry no C code — `getfuncname`'s `p->code[pc]` corrupted the error machinery, nil-ing pcall-caught messages). **Gates:** `_wasm-tests` 510/510 comparable 0 diffs (2 skips: sort, row 10); `_wasm-err-tests` 91/93 byte-exact (so00/01: stack-overflow depth/unwind, row 20 → M6); `_glua-tests` full gate 3/3 comparable 0 diffs, 7 ledgered skips (coroutines row 22, debug introspection row 23, loadstring-mixing row 24, lib-error wording row 25); native rt_* tests PASS ×3 (incl. the upvalue-registry suite); `go test ./...` all green. Docs: `docs/M5-summary.md` (bug classes, ABI v3 notes, vendored patch list, reentrancy findings, M8 perf observations).

> **Status (2026-09-14): M4 complete — backend v1 built, debugged, and gated.** `luawasm/` (backend.go + emit.go + ops.go): one wasm function per main proto (registers = TValue cells at `frame+16k`, constants interned at init into kcells via i64 stores — no data segments), flattened control flow (loop + br_table over basic blocks; an N+1-block structure so every body is enclosed), CALL through `rt_call` with the callee saved to scratch and args pre-shifted one cell so results land at R(A), numeric-for inlined on f64, generic-for staging the iterator triple at R(A+3..A+5) mirroring the interpreter. `testdiff/wasmengine.go` (third differential engine, wasmtime) + `cmd/luawasmc` (compile `.lua` → save `.wasm` — the durable-artifact request) + `cmd/luawasm-run` (execute a saved artifact). v1 rejects CLOSURE/VARARG/upvalue ops (clean compile error → SKIP-UNSUPPORTED in the harness). **Gates green:** the opcode-matrix corpus `_wasm-tests/` (460 generated cases: literals, arith × coercion grids, comparisons, and/or/not truth tables, strings, tables incl. >50-element SETLIST chunks, numeric/generic for, if/elseif, while/repeat/break, multret calls, nested control flow) — **interp vs wasm: 459/459 comparable cases 0 diffs**, 5 ledgered skips (3 error-message-wording, 1 table.sort callback bug, 1 closure-SKIP); the full `testdiff/` suite (mini matrix, backend smoke, seam, M1 self-diff, M2 conformance) green; `cmd/testdiff -engines interp,wasm` wired with skip-aware reporting. **Bring-up bug classes fixed (13 total, all root-caused):** operand-order against the ABI (gettable dst/key, compare dst-third), MOVEN partition reading B for the C field, TEST/TESTSET polarity (the fork compares against `(C==0)`; note `truth()` returns *falsiness*), rt_call results off-by-one (both CALL and TFORLOOP — masked for weeks by want=0 print calls), the single-block dispatch self-loop (constructor OOM to the 4 GB ceiling — diagnosed via wasmtime fuel), copy-loop `BrIf(1)` escaping to the whole body instead of the loop end, TFORLOOP branch arms inverted + the pseudo-JMP consumed as data, frame aliasing (TFORLOOP staging at R(A+5) overlapping scratch when nregs had no margin — now `gFrameCells = nregs+4`, scratch above a 2-cell register margin), and `forprep_body` reading the unfilled `luaV_tonumber` temp (the M3 lesson). **v1 posture decisions:** GC stopped per run (`collectgarbage('stop')` via ldostring — register cells are not GC roots, the M3-documented backend obligation; the M6 arena lifecycle replaces it); GLOBALS lines excluded from cross-engine diffs (library sets differ by construction until M7); interp's stack-traceback tails stripped from ERROR comparison (M5 compares message heads). **Known open (ledgered):** table.sort trips the wasm callback machinery after the sort call; error-message wording differs from gopher-lua's oracle texts (M5 byte-exact suite); closures/varargs are M5.

> **Status (2026-09-06): M3 complete — ABI implemented, frozen, and gated.** Full `rt_*` surface implemented in `runtime/rt_abi.c` (compiled into `lua51_sjlj.wasm`, 24 exports): version, set_state, mknumber/mkbool/mknil, intern, newtable, gettable/settable, arith (all 7 OP_*-coded ops with string coercion + metamethods), len, eq/lt/le, concat, rt_call (the universal call fallback — the runtime's lvm interprets Lua closures, C functions run directly), rt_call_count, forprep, rt_error, rt_frame_alloc, and the error-staging protocol (err_pending/err_clear/err_stage_copy; position prefix applied in the buffer, not on the Lua stack). Addressing is `rt_addr` (i32 = the frozen wasm ABI; pointer-width only in the native test build). **Gates green:** `runtime/tests/run.sh` — native unit tests PASS on plain + ASan + UBSan (L1/L2); `testdiff/rtseam_test.go` — an emitted module drives the real ABI against the real runtime through shared memory (arith/table/len/error protocol, `TestRTSeamSmoke` + host-side `TestRTDirectABI`); M2 conformance and M1 self-diff unaffected. **Bugs the gates caught** (each now a documented ABI lesson): `luaV_tonumber` returns a value pointer, not a boolean; `luaV_concat` leaves the result in the first window slot and does not adjust `L->top` (n values → 1 result: `top -= n-1`); boolean TValues are not collectable (tt=LUA_TBOOLEAN with truth in value.b); ABI cells must be genuinely contiguous arrays (two locals are not adjacent); callable values need a GC-visible reference while their cell is live (the backend owes this liveness); `LUA_CORE` must be defined to reach `luai_num*`. **Remaining, carried to M4:** closures/upvalue ABI (newclosure/find_upval/close_upvals — the Proto-struct layout freeze for emitted protos), setlist, and the big.lua yield investigation (still open from M2).


> **Status (2026-09-03): M0 complete.** `wasm/` package (wasm.go, instr.go, leb128.go — 685 lines source + 566 lines tests), wazero v1.12.0 as test-only dependency. Gates: 13 tests green (LEB128 spec vectors, section-framing walk, golden-bytes stability, and wazero execution of arith/loop/br_table/memory+data/call/call_indirect/globals/host-roundtrip); `wasm2wat` (wabt 1.0.41) validates emitted modules. M0 open questions answered on darwin/arm64 (M4 Max): instantiation ≈ **7.4 µs** (compiled module) / 8.3 µs compile+instantiate for a tiny module → **fresh-instance-per-run is viable** (v1 lifecycle decision); exported-call overhead ≈ **21 ns**; wasm recursion reached the **2,000,000-frame safety cap without trapping** (re-probe on linux/compiler engine at M3 — darwin/arm64 uses wazero's interpreter engine, so these numbers are the conservative case).
>
> **Status (2026-09-03): M1 complete.** `testdiff/` harness (Engine interface, normalized event logs PRINT/ERROR/GLOBALS, deterministic shim, corpus loader, differ) + `cmd/testdiff` CLI. **Gate green: self-diff = 0 diffs across `_glua-tests` (10/10) and `_lua5.1-tests` (24/24, zero skips)** — the oracle is deterministic before any second engine exists. Runs per-commit via `go test ./...` (heavy corpus `-short`-skipped; CI runs full). Two fork findings recorded in the divergence ledger (`docs/Lua-Wasm-Divergence-Ledger.md`): `math.randomseed` is a no-op on Go ≥ 1.24 (`rand.Seed`/`randseednop`) and the initial `math.random` stream is process-random unlike C Lua; `os.tmpname` paths are run-unique. Shim contract: every future engine must provide the same `print`/`os.*`/`math.random` replacements.
>
> **Status (2026-09-03): M2 complete.** Stock C Lua 5.1.5 runs in wasm as the third oracle: `runtime/lua51_sjlj.wasm` (built by `runtime/build.sh` with `-mllvm -wasm-enable-sjlj -mllvm -wasm-use-legacy-eh=false` — **new-EH** setjmp, linked with `-lsetjmp`, 8 MB wasm stack) hosted on **wasmtime-go v48** (`SetWasmExceptions(true)`; the oracle is CI/dev-only, so its cgo dep never touches production, which stays pure-Go wazero — the M4+ backend needs no setjmp by design §4.4). `testdiff/clua.go` drives it with the same typed value protocol and event-log format as the interp engine (WASI preopen FS for `dofile`/`io`, stdout capture, shared Go RNG for `math.random` parity). **Gate green: 20/20 non-skipped scripts of `_lua5.1-tests` pass (100% ≥ 95%)** with 4 documented skips: main/all (suite drivers that spawn the lua binary), pm.lua (fork-adapted: `do;` isn't Lua 5.1 syntax), big.lua (yield-across-C-boundary fires outside the suite's guarded position — open investigation carried to M3). Port fixes made to reach 100%: ILP32 `tonumber` base conversion (`strtoul`→`strtoll`), shebang/`#`-line skipping, `@`-prefixed chunk names (file-loader semantics for `short_src`), the `arg` global, and full deterministic `os.time/date` (calendar arithmetic over the pinned 2000-01-01 UTC instant, strftime subset, `*t` table form). Engine gotchas on record: wasmtime-go maps Go `uint64`→i64 (narrow i32 args explicitly); go-test caching can mask a stale embedded blob (`go clean -testcache` after rebuilds); `asyncify_get_state`-style resume checks are unnecessary on the SJLJ build. The Asyncify setjmp path remains as documentation (`runtime/setjmp_asyncify.c`, `runtime/canon*.c`; built only with `build.sh --with-asyncify`) — its constraints are recorded below for whenever a core-wasm-only oracle is needed.

**Asyncify path constraints (for the record):** the driver loop must be the `ASYNC_DRIVER_RUN` macro in the export frame (dispatching the rewind intrinsics through a helper function breaks the replay); every setjmp-reachable export must be wrapped (Lua's `lua_newstate` itself protected-calls `f_luaopen`); resume granularity after a rewind varies with optimization level (handle both call-site and function-top re-entry, e.g. via `asyncify_get_state`); setjmp/longjmp must be `noinline` and renamed (macro interposition) to dodge the compiler's `returns_twice` treatment of the literal names. That path reached full state creation and protected init on wazero; full execution traps on the pass's indirect-call dispatch during unwind replay (Lua calls every C function through `lua_CFunction` pointers) — the wall that motivated option (b).

| M | Deliverable | Gate (tests that must be green) | Est. |
|---|---|---|---|
| **M0** | Spike: `wasm/` emitter core (sections, LEB128, mov/arith/branch/call), host↔guest roundtrip on wazero | Emitter micro-modules execute correctly on wazero; `wasm2wat` validates; round-trip byte-identical | 1 wk |
| **M1** | **Test infrastructure first**: differential harness (event logs, globals serialization), corpus ingestion, CI wiring; oracle validated by diffing the interpreter against itself (must be 0 diffs) | Self-diff = 0 across `_glua-tests`; harness runs in CI per-commit | 1 wk |
| **M2** | Stock C Lua 5.1 compiled whole to wasm (bring-up vehicle + third oracle); host shims for its `print`/test needs | `_lua5.1-tests` ≥95% pass inside wazero (documented skips only); divergence ledger seeded | 1–2 wk |
| **M3** | Runtime split: `rt_*` ABI implemented (C, exported), native unit tests, ASan/UBSan CI, error protocol | L1/L2 for runtime green; ABI freeze review | 2–3 wk |
| **M4** | Backend v1 core: all pure-inline opcodes + arith/compare fast paths + `CALL/RETURN` direct & indirect, flattened control flow; scripts with no tables run | Opcode matrix rows green for covered set; differential corpus subset (hand-chosen ~200 cases) 100% match | 3–4 wk |
| **M5** | Backend v1 complete: all 41 opcodes, closures/upvalues/varargs/metamethods/pcall/tailcall trampoline, line immediates | Full `_glua-tests` + curated Redis corpus: 0 unledgered DIVERGE; error-message suite byte-exact | 3–4 wk |
| **M6** | Hardening: arena/GC watermark, stack limits, deadline, determinism, isolation | All 8.6 tests green; 7-night fuzz soak zero new classes; TRAP count = 0 over corpus ×10⁷ executions | 2 wk |
| **M7** | **Go Redis clone** integration: `host/` API freeze (§6), `redis.*` host module, script cache, EVAL/EVALSHA/SCRIPT FLUSH flows, caps, typed-table reply conversion, locked-VM examples | Integration suite (incl. deadline kill, OOM, flush-isolation) green **in the Go Redis clone repo**; `host/` + `examples/` green under `go test -race` | 2–3 wk |

> **Status (2026-09-22): M7a (the host-package core) complete.** `host/`
> is built on the wazero production engine per §6/A8/A9:
> `Engine` (SHA-pinned embedded prod blob, compile cache,
> `RegisterGlobal` host-function registry), `VM` (private wazero runtime +
> rt/script instances + the image mutex; `Run` holds it end-to-end),
> `RunOptions{Keys,Argv,Deadline,Seed}`, `Result`/`Value` (wire-protocol
> values), `ScriptError`/`TrapError` (script errors vs backend-bug traps).
> Runtime additions (additive to ABI v3): the `host.host_call` import +
> `rt_hostfn` (Lua→Go functions — the `redis.call` seam; args/results
> marshal through the tag-first wire protocol with expanded tables,
> errors raise via `rt_where_mark` so command texts stay verbatim) and
> `rt_encode_value` (any TValue cell → wire bytes; result/error readback
> with zero TString/Table layout knowledge). Backend contract fix:
> `lua_main` now returns the dispatch status verbatim with `want=-1`
> (multret) — the count survived the entry, so script return values are
> host-readable; engines' status checks moved `!=0`→`<0`. Hardened by the
> gates: the chaos-host suite found an unchecked-stack overrun in the
> guest decoder (65-deep table returns corrupted memory — every push now
> `luaD_checkstack`s) and a late-watchdog flag race (flagMu). Measured on
> darwin/arm64 (interpreter engine): fresh VM ≈ 44 ms vs ≈ 60 µs re-run
> on a bound image → hot paths pool VMs per script (`examples/evalserver`);
> v1 law: one script per image (per-script proto indices). Gates: `go
> test ./host/ -race` ×2 green, `CGO_ENABLED=0 go build ./host/` green,
> examples run, and every pre-existing suite re-ran green on the rebuilt
> blob (root, testdiff incl. M6c matrix + D5 determinism, `_cli-tests`
> wasmtime + WAZERO=1 legs). Ultima-side integration (M8b–M8e of
> `docs/How-To-Integrate-Gopher-Lua-with-Redis-Clone-Called-Ultima.md`)
> is the remaining M7 work.
| **M8** | Performance: v2 structured control flow (differential-tested against v1 per-function), inline caches at hot sites, instantiate-vs-reset measurement, benchmark harness vs C Lua (`examples/bench`) | Perf gate: ≥2× fork on numeric corpus; v1↔v2 differential zero diffs; no >5% regressions on any column incl. the C-Lua ratio; head-to-head **vs C Lua 5.1 native** measured, reported, and trended (§8.10) | 3–4 wk |

**Total: ~3.5–5.5 months** solo (consistent with the plan doc's estimate). M1 before M4 is deliberate: the differential harness existing *before* the backend is what makes every later milestone measurable. M2 before M3 is deliberate too: a whole-C-Lua-in-wasm oracle de-risks the runtime port with zero new compiler code.

---

## 10. Repo layout

```
wasm/            emitter (pure Go, no deps)         + wasm/*_test.go
luawasm/         backend: proto→module              + luawasm/opcode_test.go, corpus/
runtime/         C sources + wasi-sdk build + native test harness
runtime/wasm/    build artifacts (embedded via go:embed, checksummed)
host/            public Go API: Engine/Script/VM, memory-image lock, redis.* host module — the only package the clone imports
examples/        runnable examples: eval, evalserver (locked VM pool, -race), sharedvm, bench vs C Lua
testdiff/        differential harness, event-log oracle, generators, CI glue
docs/Lua-Wasm-Divergence-Ledger.md
```

---

## 11. Risks & open questions

| Risk | Mitigation |
|---|---|
| wazero maintenance pin | version-pinned; emitted modules are portable to any pure-Go engine (wasmtime-cgo would reintroduce cgo — disallowed by A8); `wasm2wat` keeps us toolchain-honest |
| Flattened control flow too slow in v1 | M8 v2 exists; measure at M4 gate with the numeric corpus before committing to v2 timing |
| Runtime ABI churn during M3–M5 | freeze at M3 gate; additive-only afterwards with minor version bump |
| Error-message parity fights (Go `fmt` vs C `sprintf` formatting of floats) | format via a single shared formatter (C side) for messages produced in both engines; ledger documents the rest | 
| Coroutine scope | Out of scope for Redis subset (decision); general-purpose build needs the Asyncify/CPS study (plan §7.7) |
| Deep-recursion wasm stack traps | enforced Lua-level limit below the engine's (measure wazero's at M0; set `stack_limit` conservatively) |
| Divergence between the two runtime sources of truth | ledger discipline (8.9); C Lua is final authority |
| ~~EH-free blob flavor for wazero (production host, A8)~~ | **Obsolete (M6a, ledger row 32):** wazero v1.12 implements the standardized EH proposal; the existing blob runs on both hosts. Asyncify remains the documented fallback should the EH path ever close |
| Embedder concurrency misuse (two goroutines into one Lua image) | the lock is the API unit (A9, §6.2): `VM.Run` holds the image mutex end-to-end; examples + host tests run under `-race` in CI (§8.6) |

**Open questions to resolve at M0–M2:** wazero instantiation cost on target hardware (drives v1-vs-v2 lifecycle); exact wazero max wasm-stack depth (drives `stack_limit`); wasi-sdk wasm-ASan maturity (drives whether wasm-mode sanitizers are CI or advisory); whether this fork's `goto` surfaces any other codegen oddities (grep `compile.go` during M4).
