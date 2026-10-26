# How to integrate gopher-lua with the Redis clone Ultima — the M8 "Lua-lite" build guide

**Purpose.** This is the working guide for the Lua-scripting portion of
Ultima's M8 milestone (`../ultima/docs/ULTIMA-DESIGN.md` §14.4:
"P3/P4 parity tail (streams, Lua-lite, bitfield, geo, PF\*)"). Everything
else in M8 is landed; `EVAL`/`SCRIPT` are the missing piece. This document
is written to be executed against: it inventories exactly what the
gopher-lua repo delivers today, specifies what must be built on both sides,
sequences the work into independently-green sub-milestones, and defines the
gates.

> **Status (2026-10-19, U8 / D12 v1.0.0): current for the 5.5 era.** The
> 5.2→5.5 upgrade (U0–U7, `docs/Upgrade-from-5.2-to-5.5-of-Lua.md`) moved
> the language core to stock 5.5.1 and the runtime to the zero-patch
> `runtime/lua55/` tree (ABI v4); U8 deleted the 5.1 tree, blobs, and
> build flavors outright (commit `01037c0`), and the D12 breaking release
> (tagged **v1.0.0**) rewrote §4 as-built against the shipped `host/`
> package, appendices A/B with it. §5 remains the Ultima-side M8 build
> plan — executable against v1.0.0 as written. The repo/module was renamed
> gopher-lua → **glasm** (`github.com/pschlump/glasm`): update paths and
> import paths accordingly.

**Repos and paths** (both on the same machine, side by side):

| Repo            | Path                                                  | Role                                                                              |
|-----------------|-------------------------------------------------------|-----------------------------------------------------------------------------------|
| glasm (the gopher-lua fork) | `~/go/src/github.com/pschlump/glasm` (this repo) | Lua 5.5.1 frontend + Lua→wasm backend + the C `rt_*` runtime blob (build-time only) |
| Ultima          | `../ultima`                                           | The pure-Go Redis clone (the daemon). Everything C-free by rule.                  |
| pluto           | `../pluto`                                            | Ultima's data-structure library (not touched by this milestone)                   |

**Compatibility target.** Ultima pins Redis semantics to **7.2.7**
(`lib/commands/engine.go` `CompatVersion`, and every gate comment in the
repo says "verified against 7.2.7"). The parity oracle is the real
`redis-server` (7.2.7) driven by `../ultima/tests/differential/` — and the
Redis 8.9.241 reference source is checked out at `../ultima/note/redis/src/`
(`eval.c`, `script.c`, `script_lua.c`) for reading the actual behavior.
When this doc and real Redis disagree, **real Redis wins and this doc gets
a correction** — that is the repo culture (probe, don't trust).

**Status snapshot (2026-09-22).**

- gopher-lua: through **M6e**, plus **M8a of this guide DONE (same day)**:
  the `host/` package is built and gated (`go test ./host/ -race` ×2
  green, `CGO_ENABLED=0 go build ./host/` green, examples run), the
  runtime blob carries the M7a additions (`host.host_call` import +
  `rt_hostfn`/`rt_encode_value` exports, §4.2), and `lua_main` returns
  the dispatch status verbatim (§4.2c). All pre-existing repo gates
  re-ran green on the rebuilt blob. What remains of gopher-lua's own M7
  ("API freeze") is field hardening from Ultima-side use.
- Ultima: M0–M7 done; M8's streams/bitfield/geo/PF\*/SSUBSCRIBE landed.
  `EVAL`/`SCRIPT` are absent from the command table and the manifest —
  **M8b–M8e of this guide are the remaining work.**

---

## 0. Scope

**In scope (M8 Lua-lite):**

- `EVAL`, `EVALSHA`, `EVAL_RO`, `EVALSHA_RO`
- `SCRIPT LOAD`, `SCRIPT EXISTS`, `SCRIPT FLUSH [ASYNC|SYNC]`, `SCRIPT KILL`
- `redis.call` / `redis.pcall` / `redis.error_reply` / `redis.status_reply`
  (the `_RO` variants reject writes)
- Atomicity, effects-only AOF propagation, timeouts, memory caps, sandbox,
  determinism, differential parity vs real Redis 7.2.7

**Out of scope for v1 (documented, deferred):**

- Redis Functions (`FCALL`/`FUNCTION`) — excluded by the Ultima design doc §1.2
- `SCRIPT DEBUG` — needs Lua debug hooks the wasm backend does not expose
- `cjson`, `cmsgpack`, `bit`, `struct` Lua libraries — the production blob
  opens base/table/string/math only (ledger rows 33–34). Scripts importing
  them fail at load with a normal "module not found"-class Lua error.
- Coroutines, `loadstring`-generated code at runtime, `debug` introspection
  inside scripts — backend v1 limits (divergence ledger rows 22–25).
  `loadstring`/`load` are nil under the sandbox; coroutine scripts compile
  (the lib exists) but are a **ledgered behavioral divergence** — the
  Ultima differential skips them with reasons, they are not silently
  trusted.
- Verbatim (whole-script) replication — single node, effects-only (§3 D2).

---

## 1. Inventory — what exists today

### 1.1 What the gopher-lua repo delivers (consume these)

| Piece | Where | Notes |
|---|---|---|
| Lua frontend | `parse/`, `ast/`, `compile.go` | 5.5.1 core (U1/U2c): `compile.go` emits the stock 5.5.1 encoding — 85 opcodes. |
| Backend (proto → wasm) | `luawasm/backend.go` `Compile(main *lua.FunctionProto, chunkName string) ([]byte, error)` | Pure Go. Imports only the root `lua` package and `wasm/` — **no wasmtime anywhere in this chain.** |
| Wasm emitter | `wasm/` | Sections, LEB128, encoders; also `wasm.Imports/Exports/HasWASIImports` helpers for blob self-checks. |
| One-call compile | `testdiff.CompileSource(source []byte, name string) ([]byte, error)` (`testdiff/wasmengine.go:161`) | `parse.Parse` → `lua.Compile` → `luawasm.Compile`. **Do not import `testdiff` from Ultima** — the package also hosts the wasmtime oracle engines (cgo). Re-implement the 12-line wrapper (§4.1) instead. |
| Production runtime blob | `testdiff/lua55_prod.wasm` (283 KiB / 289413 bytes) | Zero-WASI prod flavor of the zero-patch 5.5.1 tree (U6d, ledger row 95): **imports exactly three `host.*` functions (`event`, `wasm_dispatch`, `host_call`), zero WASI imports**, SHA-256 `77b6b7dcbde226b410cccef39d99e30ce6b305af3a297be1bcbccb5f8aad2bd5` (pinned by `host/blob.go`'s `prodBlobSHA256`, imports + WASI self-checked at `NewEngine`, byte-compared against testdiff's copy by `testdiff/m6c_test.go`). Re-pinned at U10 (int-key table probes), U11 (string/len/stdlib probes), U11c (the concat fold probe), U11d (the string.format probe), U11e (the in-wasm MOD/IDIV/DIV/POW arith arms), U11e-b (the `_ENV` GETTABUP/SETTABUP globals probe), and U11e-c (the gmatch iterator probe). |
| Dev/oracle runtime blobs | `testdiff/lua55_rt.wasm` (313 KB, ABI v4) + `testdiff/lua55_sjlj.wasm` (288 KB, oracle) | WASI-enabled flavors for the differential engines (both rebuilt by `runtime/build55.sh`). **Never ship in the daemon** (they can open files). |
| Reference host implementation | `testdiff/wazeroengine.go:118` (`WazeroEngine.Run`) | The complete, gate-green wazero embedding — instantiation order, host module, caps, watchdog, error readback. The host package (§4) is this code, productized. |
| Compiler/runner CLIs | `cmd/luawasmc`, `cmd/luawasm-run` | For manual experiments: `go run ./cmd/luawasmc -o t.wasm t.lua && GLUA_WASM_ENGINE=wazero go run ./cmd/luawasm-run t.wasm` |
| Engine version | wazero **v1.12.0** | Must instantiate with `api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling` — the blob is built in the standardized-EH/setjmp dialect (ledger row 32). |

**The `host.*` imports the prod blob expects** (you implement these in Go;
`host/blob.go`'s `hostImports` is the frozen contract, mirrored by
`testdiff/m6c_test.go`):

| Import | Signature | Purpose |
|---|---|---|
| `host.event` | `(kind, ptr, len i32)` | `print`/library event stream out of the guest. Ultima maps `kind=PRINT` to nothing (or slog at debug), `ERROR` to the error path. |
| `host.wasm_dispatch` | `(idx, frame, cl, nargs, want i32) -> i32` | The re-entrancy seam: the runtime's precall adapter dispatches a compiled proto through the host into the script module's `lua_dispatch`. **This same nested-`Call()`-on-one-goroutine mechanism is what makes `redis.call` possible.** |
| `host.host_call` | `(fnidx, argsPtr, argsLen, retPtr, retCap i32) -> i32` | The hostfn seam (U6e, ledger row 96): `RegisterGlobal` installs Go host functions; arguments/results stage over the wire buffers, and error values re-raise position-less and verbatim (Redis command texts). |

The RNG imports are gone — stock 5.5 xoshiro256\*\* runs in-process with
determinism pinned at build time. The host module must additionally export
`host.deadline_poll () -> i32` for the **script** modules it instantiates
(U7a, ledger row 100 — an import contract for every instantiating host: the
guest polls it every 2²⁰-th block transition so a Go GC mark-STW cannot
wedge the deadline).

**Runtime exports the host calls** (full list: `runtime/rt_abi55.h` + the
driver `runtime/luawasm55.c`; the ones the host package uses, amended to
ABI v4 at U8):

```
lnewstate () -> L          ldostring (L, inAddr, len, nameAddr, nres)
rt_set_state (L)           rt_abi_version () -> 4
rt_sandbox (1)             # rt_set_dialect retired at U3
rt_protect_globals (1)     rt_globals_readonly (on)   # M8d: Redis-classic globals lockdown + KEYS/ARGV staging window
rt_set_memlimit (bytes)    rt_mem_used_bytes () -> i64
rt_ctrl_addr () -> addr    rt_set_deadline / rt_deadline_flag / rt_deadline
rt_frame_alloc (nbytes) -> addr
rt_intern (ptr, len) -> ref     rt_mknumber / rt_mkbool / rt_mknil
rt_newtable / rt_gettable / rt_settable / rt_getglobal / rt_setglobal
rt_err_stage_copy (buf, cap) -> n     rt_err_value_ptr () -> addr
rt_hostfn (tbl, name, fnidx)    rt_encode_value (cell, dst, cap)   # U6e row 96
lglobals ()                lclose (L)
```

### 1.2 What Ultima already has (touch points, with anchors)

| Touch point | Where | Why it matters |
|---|---|---|
| Command dispatch | `lib/commands/engine.go:356` `Engine.Execute` | Queue gate, NOAUTH, arity, subscribe-mode gate, OOM gate → `def.Handler(e, cs, args) resp.Value`. EVAL is just four/five new table entries. |
| Command table | `lib/commands/table.go` `def(name, arity, flags, first, last, step, group, handler)` | Registration site for `eval`, `evalsha`, `eval_ro`, `evalsha_ro`, `script`. |
| Atomicity pattern | `lib/commands/tx.go:65` `cmdExec` | `tok, resume := e.Shards.PauseAll()` … `cs.tok = tok` … inner commands run `def.Handler` directly + `capturePersist` each. **EVAL copies this pattern wholesale** (§5.4). |
| Effects replication | `lib/commands/aof.go:85` `capturePersist` → `rewriteForPersist` → `p.LogCommand(hint, db, argv)` | Per-write-command AOF capture, already the EXEC model. `redis.call` writes ride the same path. |
| Shard API | `lib/shard/shard.go:305` `PauseAll() (tok, resume)`, `:339 DoTok`, `:233 ShardIndex` | Under PauseAll, shard tasks run synchronously via `DoTok`. |
| Reply values | `lib/resp/value.go` `resp.Err/Simple/Int/Bool/Arr/Map/Null` | Lua⇄RESP conversion target (§5.6). |
| Config | `lib/config/config.go` (JSON + `default:` tags + `$ENV$`) | New `script` group: time limit, hard deadline, memory cap, RNG seed base (§5.1). |
| Manifest | `lib/commands/manifest.json` (141 entries, EVAL absent) | Add the new commands; status enforced by tests. |
| Differential harness | `tests/differential/` (in-process Ultima vs `redis-server` 7.2.7, `REDIS_BIN`) | The parity gate for this milestone (§7). |
| One engine, three front-ends | RESP / gRPC generic envelope / WS generic envelope | EVAL lands once and works on all three surfaces (via the generic `CommandRequest` escape hatch; no typed proto message needed for M8). |

### 1.3 The gap list (everything that must be built)

1. **gopher-lua `host/` package** (its M7 deliverable, design doc §6):
   `Engine`/`Script`/`VM`, blob embed + SHA pin, compile cache, the run
   protocol, caps/deadline, result readback, per-VM mutex (A9). §4.
2. **Two small runtime additions** (C, in this repo, rebuilt into the blob):
   a host-function registration seam (Lua→Go calls: `redis.call`) and a
   string-bytes readback export (Go reads Lua string results). §4.2.
3. **Ultima `lib/scripting` package**: script cache (SHA1→script), the
   `redis` Lua table bridge, Lua⇄RESP conversion. §5.2.
4. **Ultima commands**: `eval.go` + `script.go` + table/manifest/INFO/ACL
   wiring, the BUSY gate, SCRIPT KILL. §5.3–§5.5.
5. **Tests**: unit, `-race` concurrency, chaos-host, differential corpus,
   soak. §7.

---

## 2. Architecture

```
                 ┌──────────────────────── Ultima daemon (pure Go) ─────────────────────────┐
 RESP / gRPC /   │  lib/commands.Engine.Execute ── table["eval"] ──► cmdEval (lib/commands)  │
 WS front-ends   │        │  PauseAll token (atomicity, like EXEC)                           │
 ───────────────►│        ▼                                                                │
                 │  lib/scripting                                                          │
                 │   ScriptCache  (sha1 → *host.Script, SCRIPT LOAD/EVAL/EVALSHA/FLUSH)     │
                 │   CallBridge   (func(argv [][]byte) resp.Value  ← re-enters commands)    │
                 │        │                                                                │
                 │        ▼                                                                │
                 │  gopher-lua host package (github.com/pschlump/gopher-lua/host)           │
                 │   Engine (wazero Runtime, blob, compile cache)                            │
                 │   VM     (script instance + rt instance + linear memory + sync.Mutex)    │
                 │        │  host module: event/wasm_dispatch/host_call (+deadline_poll) │
                 │        │               + redis.call trampoline (Go)                      │
                 └────────┼─────────────────────────────────────────────────────────────────┘
                          ▼
              wazero v1.12 (pure Go) ──► lua55_prod.wasm (rt_* ABI v4, sandboxed)
                                      ──► script.wasm    (compiled from EVAL source)
                          ▲                                 │
                          └───── lua_dispatch (host.wasm_dispatch, same goroutine)
```

Key flows:

- **EVAL**: `Execute` → `cmdEval` → compile (or cache hit by SHA1) →
  `PauseAll()` → stage `KEYS`/`ARGV` → `lua_main` → convert results →
  `resume()` → reply. The connection goroutine runs the VM end-to-end.
- **redis.call (mid-script)**: guest calls the `redis` table → host
  trampoline fires **on the same goroutine that holds the VM mutex** →
  `CallBridge` runs the command through the inner-command path (handler +
  `capturePersist`) under the pause token → reply written back into linear
  memory as a Lua value → guest continues. Nested wazero `Call()` on one
  goroutine is proven (M5a reentrancy spike; `host.wasm_dispatch` uses it
  on every C→compiled call).
- **Atomicity**: identical to EXEC — one `PauseAll` token spans the whole
  script, so no other command interleaves (Redis "scripts are atomic").
- **Parallelism**: scripts serialize globally under PauseAll (Redis parity).
  A v2 superset optimization — pure scripts (no `redis.call` ever reached)
  lazily taking the pause at first call — is sketched in §9 but is **not**
  M8.

---

## 3. Design decisions for this milestone

Numbered to coexist with the Ultima design doc's D1–D20; these are
S-series (scripting). Do not reverse them without a doc update in both
repos.

| # | Decision | Rationale |
|---|---|---|
| S1 | **The daemon consumes exactly two artifacts from glasm: the Go `host` package and the embedded, SHA-256-pinned `lua55_prod.wasm` blob.** No cgo, no C toolchain, no C sources in `../ultima` — ever. | Ultima design §1/§16 D12; glasm design A8. Enforcement: `CGO_ENABLED=0 go build ./...` in Ultima CI (§4.6). |
| S2 | **Effects-only replication.** `EVAL` is never written to the AOF; every write command executed via `redis.call` is captured individually through `capturePersist`, exactly as EXEC captures its inner commands. | Single node, no replica protocol; matches Redis 7 default (effects). Reuses a proven path (`lib/commands/aof.go:85`). |
| S3 | **EVAL runs under `PauseAll`, mirroring `cmdExec`.** `cs.tok` is set for the duration; `redis.call` inner commands dispatch via the normal handlers. | The strict cross-shard atomic path already exists and is stress-tested (M3 WATCH gates). |
| S4 | **Fresh VM per execution (v1 lifecycle), pooled wazero `Runtime` and compiled-module cache at the Engine level.** | gopher-lua M0 measured instantiation ≈ 7.4 µs — negligible vs a script run. Zero shared mutable guest state. A VM pool is a measured follow-up only if benchmarks justify it. |
| S5 | **Hard deadline via the M6d watchdog, plus a soft `lua-time-limit` BUSY phase.** Over the soft limit, other clients' normal commands get `-BUSY`; at the hard deadline the watchdog flag kills the script mid-loop. Partial effects are **kept** and already AOF-captured; the scripting client gets a script-timeout error. | Redis cannot preempt (SCRIPT KILL / SHUTDOWN NOSAVE only). Ultima can — better ops, but a **documented divergence**: a killed script's earlier writes persist. Must be recorded in the Ultima docs and probed tests must not assume Redis's all-or-nothing-by-blocking behavior. |
| S6 | **Determinism: the host seeds `math.random` per run** from a configurable base (default: derived from server run-id + a per-run counter — *not* wall clock). | gopher-lua D5 gates (5×-identical logs) require host-seeded RNG; scripts stay reproducible for testing. Redis's own seeding is unobservable to clients either way. |
| S7 | **Number formatting for RESP is host-side** (`strconv`), not Lua `tostring`. Integral floats → RESP integer; otherwise shortest round-trip decimal. | The blob's number→string dialect has known 1-ulp/wording corners (ledger rows 38-survivor, 40, 48). Conversion happens in Go on the raw f64, so those corners cannot leak into replies. |
| S8 | **No `cjson`/`bit`/`cmsgpack` in v1; `loadstring` nil; coroutines compile but stay a ledgered divergence.** The prod blob opens base/table/string/math only; `loadstring`/`load` are sandboxed to nil; coroutine scripts COMPILE (the sandbox keeps the `coroutine` lib) but their engine behavior is a ledgered differential skip (row 22) — treat them as unsupported in the Ultima gate (skip with reason), not as compile-time rejects. | Production blob surface (rows 33–34) + backend v1 limits (rows 22–25). Failures that do occur are clean errors, never traps. |
| S9 | **`redis.call` re-checks ACL, arity, `noscript`, and deny-OOM per call**, using the calling connection's identity; keys are computed from the command's own key spec at runtime (not from static FirstKey, which cannot express `numkeys`). | Redis 7 semantics; Ultima already has per-connection auth state on `ConnState`. |
| S10 | **The `host` package lives in the gopher-lua repo and knows nothing about Ultima.** The `redis` table is injected by Ultima through a host-package callback seam. | Keeps the public API generic (design §6) and avoids an import cycle / repo coupling. |

---

## 4. The gopher-lua `host` package (build this first, in this repo)

Design doc §6 (`docs/Lua-Wasm-Design-and-Test-Plan.md`) is the contract;
`testdiff/wazeroengine.go:118` is the reference implementation. The package
is that code, productized: options instead of struct fields, a compile
cache, a result readback, a host-function seam, and the A9 mutex.

### 4.1 Layout and public API

```
host/
├── host.go        Engine, Option, Script, compile cache, RegisterGlobal/RegisterValue
├── vm.go          VM: NewVM/Run/Close, the run protocol, deadline, the per-image lock (A9)
├── wire.go        the value wire (PT_* tags shared with luawasm55.c): Value, KV, kinds
├── hostfn.go      the host module's functions: event, wasm_dispatch, host_call, deadline_poll
├── blob.go        //go:embed lua55_prod.wasm + SHA-256 pin + import self-check
└── *_test.go      host_test (concurrency, caps, deadline, determinism),
                   compile_race, globals_protection, usedbytes gates
examples/          eval, evalserver (pool + -race), sharedvm
```

The API as shipped at the D12 release (v1.0.0) — `host/host.go` and
`host/wire.go` are the truth; this is the reading order:

```go
package host

func NewEngine(opts ...Option) (*Engine, error)
// Options: WithRuntimeBlob([]byte)      (override; still SHA + surface checked)
//          WithMemoryBudgetBytes(int64) (rt_set_memlimit BEFORE lnewstate —
//                                        per-image, not per-run; 0 = unlimited,
//                                        the blob's 256 MiB linear-memory max
//                                        is the hard backstop either way)
//          WithEventSink(func)          (observe every guest print(), args
//                                        wire-decoded, on the lock goroutine)
//          WithGlobalsProtection(bool)  (Redis-classic lockdown; enable after
//                                        all Register* calls — VM creation order
//                                        is sandbox → host staging → protection)
//          WithVMCacheSize(int)         (bounded pool of warm VM images keyed
//                                        by script: repeat e.Run skips NewVM
//                                        entirely — h_tiny 3.9 ms → 9 µs; n<=0,
//                                        the default, keeps fresh-VM-per-run.
//                                        Images recycle past the UsedBytes
//                                        watermark (3/4 of the budget, else
//                                        64 MiB); guest errors keep the image,
//                                        engine-level failures close it)
//          WithCompileCacheDir(string)  (on-disk compile caches under the dir,
//                                        cross-PROCESS: wazero's machine-code
//                                        cache serves the blob JIT to a fresh
//                                        process in ~6 ms instead of ~90 ms,
//                                        and compiled script artifacts persist
//                                        via pluto lru_disk_ts (dir/scripts,
//                                        256 entries, keyed by SHA-1 + name +
//                                        glasm version) — SCRIPT LOAD identity
//                                        across daemon restarts. Default "" =
//                                        in-memory. The daemon must own the dir
//                                        (0o700: a poisoned machine-code entry
//                                        is native code execution)

func (e *Engine) Compile(source []byte, name string) (*Script, error)
// parse.Parse → lua.Compile → luawasm.Compile, SHA1-keyed cache; exactly
// one *Script pointer per (source, name) — the one-script law and
// consumer-side per-script pools rely on pointer identity.

type Script struct {
    SHA1 string    // hex SHA-1 of the source — the SCRIPT LOAD / EVALSHA identity
    Wasm []byte    // durable artifact; what luawasmc writes
}                   // + Name() — the chunk name (error position prefixes)

type RunOptions struct {
    Keys, Argv []string     // staged as the KEYS / ARGV globals (copies)
    Deadline   time.Duration // watchdog + host.deadline_poll (§4.4); 0 = off
    Seed       int64         // math.randomseed(n) in-guest: stock xoshiro256**,
                             // identical stream on every engine; 0 = keep the
}                            // VM's stream (fresh VMs: the build-pinned stream)

type HostFunc func(vm *VM, args []Value) ([]Value, error)
func (e *Engine) RegisterGlobal(table, name string, f HostFunc) error  // redis.call…
func (e *Engine) RegisterValue(table, name string, v Value) error      // redis.LOG_* scalars
  // Both frozen once the first VM exists; each VM snapshots the registry.

// (The memory budget is an ENGINE option — WithMemoryBudgetBytes — because
// rt_set_memlimit must precede lnewstate: per-image, not per-run.)

func (e *Engine) NewVM() (*VM, error)
func (vm *VM) Run(ctx context.Context, s *Script, opt RunOptions) (Result, error)
func (vm *VM) Kill()              // force the deadline flag (the watchdog surface)
func (vm *VM) UsedBytes(ctx context.Context) int64
func (vm *VM) Close() error
func (e *Engine) Run(ctx context.Context, s *Script, opt RunOptions) (Result, error)
// Engine.Run = NewVM + Run + Close (S4 default deployment).

// Error taxonomy: *ScriptError is a Lua-level error (ErrValue = the exact
// wire Value, Line = the raise line); *TrapError is the backend-bug class
// (log the module SHA, never a script error); ErrReentrant; ErrScriptBound.

// As-built v1 laws (differ from the original sketch):
// • ONE SCRIPT PER VM IMAGE: proto indices are per-script (0-based) into
//   a per-image registry, so a second script would collide (ErrScriptBound).
//   Re-running the SAME script on a VM is the Redis-classic shared-globals
//   mode. Engine.Run (fresh VM) sidesteps the law entirely.
// • ONE WAZERO RUNTIME PER VM (not shared): module names resolve inside
//   a runtime's namespace at instantiation (the script imports "rt"), so
//   concurrently-live images cannot share one runtime. Script COMPILATION
//   stays cached at the Engine level; only wazero's per-instance module
//   compilation is repaid per VM.
// • Measured 2026-09 on darwin/arm64 (interpreter engine — the
//   conservative case): fresh VM ≈ 44 ms, re-run on a bound image ≈ 60 µs
//   (~700×). Hot paths pool VMs per script (see examples/evalserver); the
//   U7c bench lanes (cmd/luabench) re-measure the engines end-to-end.

type Result struct { Values []Value }   // script return values, converted

type Value struct { Kind ValueKind; Num float64; Int int64; Str string; Pairs []KV; More bool }
// Kinds: Nil/False/True/Number/String/Table/Function/Cycle/Deep/Integer(13).
// Int is LOSSLESS (U6e) — integers never narrow through Num. Constructors:
// Nil/Bool/Number/Int/String/Table; KindTable truncation flags More.
```

`Compile` must not re-derive `testdiff.CompileSource` by importing testdiff
(cgo contamination, §4.6). It is the same three calls:

```go
chunk, err := parse.Parse(strings.NewReader(string(source)), name)
proto, err := lua.Compile(chunk, name)          // root package, clean
bin, err := luawasm.Compile(proto, name)        // luawasm, clean
```

### 4.2 Runtime additions (C, this repo, rebuilt into the blob) — **as built (M8a; ABI v4 since U2d/U6d/U6e)**

Additive to the ABI surface as it stood at M8a (v3; the tree is v4 since
U2d) — new exports/imports plus one
script-module entry contract, no changes to existing C contracts. Rebuild
via `runtime/build55.sh` (it copies the prod blob into `host/` too), bump
the SHA pins (`host/blob.go`, `testdiff/m6c_test.go`), `go clean
-testcache`, re-run the full gates (`make test`, the M6c matrix).

**(a) Lua→Go host functions (`redis.call`) — `rt_hostfn` +
`host.host_call`.** Marshaling rides the *tag-first wire protocol* the
blob already speaks for print/globals events (`PT_*` tags,
`runtime/luawasm55.c`'s wire encoder), not raw TValue cells — strings travel
inline as bytes and table arguments arrive fully expanded, so the Go side
needs zero layout knowledge and `redis.error_reply({err=...})`-style
table arguments are visible to the bridge:

```text
args   = u32 count, count × value   (guest→host; tables expanded, with
                                     cycle/deep/truncation sentinels)
result = u32 count, count × value   (host→guest; decoder pushes each as
                                     an ordinary Lua value)
error  = 1 value + return -1        (decoded, pushed, lua_error'd)
value  = 0 nil | 1 false | 2 true | 3 f64 | 4 u32len+bytes |
         5 u32n + n×(key value)
```

`rt_hostfn(tablePtr, namePtr, fnidx)` installs a `lua_CFunction`
trampoline as `<table>.<name>` (names are NUL-terminated bytes in
`namebuf`; registration runs protected via `lua_cpcall`). The trampoline
encodes args into `evbuf`, calls the new import `host.host_call(fnidx,
argsPtr, argsLen, retPtr, retCap)`, and decodes returns from a dedicated
256 KB staging buffer. Three hard-won rules baked into the C:

- **Errors raise with `rt_where_mark()`** (position already decided) so
  the adapter's re-raise adds no `chunk:line:` prefix — command error
  texts propagate verbatim (Redis semantics; without the mark every
  `redis.call` failure gains a `=script:1:` prefix).
- **Every push in the guest-side decoder does `luaD_checkstack` first.**
  An unchecked 65-deep table return overran the stack block and corrupted
  adjacent memory — the guest spun forever (found by the chaos-host gate;
  shallow returns never grew the stack, so only deep ones tripped it).
- **Decode depth caps** (64 nesting) and count caps (4096 results) turn
  malformed host bytes into clean Lua errors, never traps.

The Go side (`host/hostfn.go`) implements `host.host_call`: decode args →
`HostFunc` (panics contained) → encode results. Trampolines run on the
goroutine holding the VM mutex — no new deadlock surface (A9 law). On the
zero-wasi prod flavor the import set settled at exactly three —
`host.{event, wasm_dispatch, host_call}` (U6d; the RNG imports retired at
U3b: stock xoshiro256\*\* runs in-guest) — pinned in lockstep by
`testdiff/m6c_test.go` and `host/blob.go`. The differential engines speak
the seam too (wazero fully; the wasmtime matrix leg stages a wire refusal
value).

**(b) Result readback — `rt_encode_value` (replaces the originally
sketched `rt_value_tostring`).** Rather than teaching the host the
`TString`/`Table` memory layouts, one export encodes any TValue cell in
the same wire protocol:

```c
int32_t rt_encode_value(int32_t cell, int32_t dst, int32_t cap);
/* Encode the cell's value (tables expanded, strings inline) at dst.
** Returns the FULL encoded length — the host detects truncation when
** the return exceeds cap and retries through an rt_frame_alloc buffer. */
```

The host reads script results and error values through it
(`rt_err_value_ptr`'s cell encodes the same way), so Appendix B's tag
table is the only value contract — symmetric, and the layouts stay an
implementation detail of the blob.

**(c) `lua_main` returns the dispatch status verbatim (backend emitter
change).** The as-built `emitMain` collapsed the dispatcher's return to
0/1 and passed `want=0` — the result count never survived the entry and
multret returns were truncated. `luawasm/backend.go` now emits
`dispatch(0, frame, 0, 0, want=-1)` and returns its value unchanged:
`nret ≥ 0` (nret results staged at `frame+0..16n`) or a negative error
code with the error TValue staged as before. This matches the §4.5
design sketch; the differential engines' status checks moved from
`!= 0` to `< 0` (`testdiff/wasmengine.go`, `testdiff/wazeroengine.go`),
and the full corpora re-ran green on the new contract.

### 4.3 The run protocol (normative step list)

The living reference implementation is `host/vm.go` (productized from
`testdiff/wazeroengine.go`, the differential engine). Steps below are the
v1.0.0 (ABI v4) protocol; the `:NNN` anchors still point into the M8-era
`wazeroengine.go` and have drifted — trust `host/vm.go` over any anchor:

1. **Runtime config**: `wazero.NewRuntimeConfig().WithCoreFeatures(
   api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling)` —
   non-negotiable for the EH-dialect blob (`:141-143`). One `wazero.Runtime`
   per `Engine`, `Close` on engine shutdown.
2. **Host module** `"host"`: `event`, `wasm_dispatch`, `host_call`, and
   `deadline_poll` (§4.4) — `host/hostfn.go` implements all four. `event`
   receives `kind/ptr/len` and reads guest memory via `Memory().Read`. No
   RNG imports: the stock xoshiro256\*\* stream lives in the guest
   (build-pinned; `RunOptions.Seed` reseeds via `math.randomseed`).
3. **Instantiate the runtime blob** as module `"rt"` with a bare
   `ModuleConfig` (the prod blob needs **no** WASI, no FS, no stdio — its
   absence is the sandbox guarantee; `wasm.HasWASIImports` self-check at
   engine construction) (`:215-220`).
4. **Instantiate the script module** as `"script"`; wire `wasm_dispatch`
   to `scriptInst.ExportedFunction("lua_dispatch")`, returning `-3`
   (refused) before it exists (`:175-189`).
5. **Caps before state**: `rt_set_memlimit` from the engine's
   `WithMemoryBudgetBytes` —
   must precede `lnewstate` so the whole VM lifetime counts (`:237-241`).
6. `lnewstate` → `rt_set_state(L)` (the frozen-GC law: pool/frame cells
   are not GC roots — ledger rows 61/93). `RunOptions.Seed` applies as
   `math.randomseed(n)` in the guest.
7. `rt_sandbox(1)` — globals lockdown; on the prod blob it is
   belt-and-suspenders (`:279-283`).
8. **GC stop** (v1 law: register cells are not GC roots):
   `ldostring("collectgarbage('stop')")` (`:286`).
9. **Stage KEYS/ARGV**: build `KEYS={...}` / `ARGV={...}` Lua table
   literals from the (quoted, S7-safe) option strings and `ldostring` them
   — same mechanism as the CLI `arg` staging (`:289-297`, `luaQuote` at
   `testdiff/wasmengine.go:61`). *Alternative for later:* build the tables
   via `rt_newtable`/`rt_intern`/`rt_settable` directly; v1 takes the
   ldostring path because it is the proven one.
10. **ABI check**: `rt_abi_version()` must return **4**. (The v3 dialect
    layer — `rt_set_dialect` — retired at U3: every 5.5 engine speaks
    stock `luaO_*` texts natively; no dialect call exists on v4.)
11. **Register host functions** (new, §4.2a): `rt_hostfn("redis", "call",
    0)` etc. — after sandbox, before init.
12. `luawasm_init(2)` on the script instance; read `gFrameCells`;
    `rt_frame_alloc(frameCells*16)` (`:309-316`).
13. **Deadline, three layers**: (a) the `ctx` passed to `lua_main` (wazero
    aborts the call); (b) the watchdog — `rt_ctrl_addr()` → flag address,
    `time.AfterFunc` writes the 4-byte LE `1` into linear memory from the
    timer goroutine (a plain buffer copy, no engine state; an `atomic`
    done-flag guards the teardown race); (c) **`host.deadline_poll`** —
    the emitted script module polls the import every 2²⁰-th block
    transition and raises `rt_deadline`'s ordinary Lua error at the poll.
    Layer (c) is the durable one: a Go mark-STW landing inside a watchdog
    window freezes the timer goroutine before its write (row 99 — the
    GC×JIT wedge), while the poll's import call is a safe point that lets
    the STW drain (row 100, U7a). **`deadline_poll` is an instantiation
    contract for every host that instantiates emitted modules.** `Kill()`
    forces the same flag from Go.
14. `lua_main(frame)`. A wasm **trap** here (as opposed to a `-1`
    status) is, by contract, always a backend bug — log the module SHA,
    surface as an internal error, never as a script error.
15. **Error path** (status ≠ 0): `rt_err_stage_copy` into a 4 KB frame
    buffer → message bytes; non-string errors via `rt_err_value_ptr`,
    rendered through `rt_encode_value` (the same wire — `*ScriptError`
    carries it as `ErrValue`, plus the raise `Line`).
16. **Result path** (status ≥ 0 = nret): read nret cells at `frame+0..`,
    decode per §4.2b (each cell through `rt_encode_value`). Convert
    to `Result` (§5.6 does RESP after this point).
17. `lclose(L)` on VM `Close` (v1 fresh-per-run closes eagerly).

### 4.4 Caps, deadline, determinism (the M6d/M6c guarantees, restated)

- **Memory**: `rt_set_memlimit(bytes)` before `lnewstate`; an allocation
  past the cap raises a clean `rt_oom`-class Lua error the host converts to
  a script error — never a trap, never host OOM. Ultima default cap: S8
  config (§5.1).
- **Deadline**: soft (BUSY to others) + hard kill per S5. The hard kill
  lands at the next poll — the in-guest `deadline_poll` backstop (row 100)
  plus the watchdog's flag write; a Go mark-STW can freeze the bare
  watchdog mid-window (row 99), so the poll is the layer to rely on.
  Everything else is bounded by the `ctx` cancel → treat as a refused run.
- **Determinism**: identical `(script, KEYS, ARGV, Seed)` ⇒ byte-identical
  effects and replies (D5 gates already prove 5×-identical runs on the
  corpora; the host package's tests re-prove it through the public API).

### 4.5 What the `redis` table is (and is not) in the host package

The host package ships **no Ultima concepts**. `RegisterGlobal` is generic.
Ultima registers exactly:

```go
eng.RegisterGlobal("redis", "call",  bridge.call)      // aborts script on error
eng.RegisterGlobal("redis", "pcall", bridge.pcall)     // returns {err=...} table
eng.RegisterGlobal("redis", "error_reply",  bridge.errorReply)
eng.RegisterGlobal("redis", "status_reply", bridge.statusReply)
eng.RegisterValue("redis", "LOG_WARNING", host.Int(3))   // the LOG_* scalars
```

`KEYS`/`ARGV` are plain globals staged by the protocol (step 9) — Redis
parity. (`redis.keys`/`redis.argv` clones are a follow-up; not M8.)

### 4.6 Purity enforcement (no cgo in the daemon, S1)

- `host/` (and everything it imports: `parse/`, root `lua`, `luawasm/`,
  `wasm/`, wazero) contains **zero** wasmtime references — verified by
  grep today; keep it that way.
- Ultima adds `CGO_ENABLED=0 go build ./...` (and `go vet`) to its CI gate
  for this milestone. If any cgo package ever enters the daemon's import
  graph, the build fails loudly instead of silently shipping C.
- Optional belt-and-suspenders (only if import-graph accidents actually
  happen): split `host/` into its own Go module
  `github.com/pschlump/glasm/host` requiring only wazero, with
  `replace` directives for the parent. Not needed while the grep-clean
  property holds.

### 4.7 Gates for the host package (this repo)

- `go test ./host/... ./examples/... -race` green, including: N goroutines
  × M VMs over a mixed corpus; single-shared-VM serialization observes
  strictly sequential globals; host trampolines assert they run on the
  lock-holding goroutine.
- Caps: allocation-heavy script → clean OOM error, not trap/host-OOM.
  Deadline: infinite loop `while true do end` → killed at deadline, error
  readable, VM reusable afterwards (fresh image).
- Determinism: corpus × 5 runs byte-identical; `math.random` reproducible
  from seed.
- Chaos host: a `redis.call` stub returning wrong types / huge replies /
  errors mid-iteration — engine survives all (host-behavior tests of the
  conversion layer).
- Blob: SHA pin + `wasm.Imports` self-check (no WASI, exactly the three
  host.* imports) mirrored by `testdiff/m6c_test.go` and run at every
  `NewEngine`.
- Full existing repo gates stay green after the §4.2 blob rebuild:
  `make test`, the corpus gates, `_cli-tests` (wasmtime + `WAZERO=1`).
  The host gate itself: `go test ./host/` plain + `-race`, twice.

---

## 5. The Ultima integration

### 5.1 Module wiring and config

- `go.mod`: `require github.com/pschlump/glasm v1.0.0` + a
  `replace github.com/pschlump/glasm => ../glasm` while developing
  side-by-side (same pattern as pluto). The tag exists — drop the
  replace when you want the pinned artifact.
- New config group (defaults shown; JSON + `default:` tags + `$ENV$` per
  the exsms pattern, §8 of the Ultima design):

```json
"script": {
  "lua_time_limit_ms": 5000,
  "script_hard_deadline_ms": 30000,
  "script_max_memory_mb": 64,
  "script_rng_seed": 0
}
```

  `lua_time_limit_ms` mirrors Redis's `lua-time-limit` (soft, BUSY phase);
  `script_hard_deadline_ms` is the Ultima watchdog kill (S5);
  `script_max_memory_mb` feeds `rt_set_memlimit` per VM;
  `script_rng_seed` 0 = derive from run-id (S6). All four surface in
  `CONFIG GET/SET` via the existing `configParams` machinery.

### 5.2 `lib/scripting` package

```
lib/scripting/
├── scripting.go   Manager: owns *host.Engine, script cache, config knobs
├── cache.go       sha1(hex) → *host.Script; SCRIPT FLUSH drops it
├── bridge.go      the redis.call/pcall/error_reply/status_reply HostFuncs
└── convert.go     host.Value ⇄ resp.Value  (the Redis conversion rules)
```

Shape (keeps the import direction one-way — `commands` → `scripting`,
never back; the bridge receives a closure, per S10):

```go
package scripting

type Manager struct {
    eng   *host.Engine
    mu    sync.RWMutex
    cache map[string]*host.Script
    cfg   Config
    running atomic.Int64      // SCRIPT KILL / BUSY gate state
}

func New(cfg Config) (*Manager, error)

// CallPath is supplied by lib/commands at wiring time (S10/S9): it runs
// one command under the EVAL pause token, with ACL + capturePersist, and
// returns the reply. scripts never import commands.
type CallPath func(argv [][]byte) resp.Value

func (m *Manager) Bind(call CallPath) error   // registers redis.* HostFuncs

func (m *Manager) Compile(source []byte) (sha1hex string, err error)
func (m *Manager) Run(sha1hex string, keys, argv []string) (scripting.Result, error)
func (m *Manager) Exists(sha1hex ...string) []bool
func (m *Manager) Flush(async bool)
```

`Manager.Run` maps host errors to Ultima error values: compile-time
unsupported-feature errors (coroutines etc.) → the Redis-style script
compile error text; deadline → the timeout error; OOM → the memory error;
`pcall`-caught guest errors arrive as `Value{Err}` and convert per §5.6.

### 5.3 The commands (`lib/commands/eval.go`, `lib/commands/script.go`)

Registration (mirror the surrounding `table.go` style; flags/arity
**verified against `redis-cli COMMAND INFO` on 7.2.7** — probe, then fix
the table below if the probe disagrees):

```go
def("eval",       -3, []string{"write", "denyoom", "noscript", "movablekeys"}, 0, 0, 0, "scripting", cmdEval)
def("evalsha",    -3, []string{"write", "denyoom", "noscript", "movablekeys"}, 0, 0, 0, "scripting", cmdEvalSha)
def("eval_ro",    -3, []string{"readonly", "noscript", "movablekeys"},          0, 0, 0, "scripting", cmdEvalRO)
def("evalsha_ro", -3, []string{"readonly", "noscript", "movablekeys"},          0, 0, 0, "scripting", cmdEvalShaRO)
def("script",     -2, []string{"noscript"},                                     0, 0, 0, "scripting", cmdScript)
```

(`movablekeys` because key positions depend on `numkeys`; static
First/Last stay 0 and runtime key extraction uses numkeys — S9.)

`cmdEval` skeleton (the full arity/numkeys validation order and every
error string come from the probe list, Appendix C):

```go
func cmdEval(e *Engine, cs *ConnState, args [][]byte) resp.Value {
    // args: eval script numkeys [key ...] [arg ...]
    script := args[1]
    numkeys, keys, argv, ok := parseEvalArgs(args)      // Appendix C rules
    if !ok { return evalArgsError(args) }               // probed texts
    sha, err := e.Scripts.Compile(script)               // cache-fill; compile errors here
    if err != nil { return scriptCompileError(err) }
    if isWriteScriptRun(cs) { /* EVAL_RO guard in the _RO variants: */
        // "ERR Write commands are not allowed from read-only scripts."
    }
    tok, resume := e.Shards.PauseAll()                  // S3 — copy cmdExec
    defer resume()
    defer func() { cs.tok, cs.inExec = 0, false }()
    cs.tok, cs.inExec = tok, true
    e.Scripts.markRunning(cs, readonly)                 // BUSY gate + SCRIPT KILL
    defer e.Scripts.unmarkRunning()
    res, err := e.Scripts.RunROorRW(sha, keys, argv)    // ctx carries the hard deadline
    if err != nil { return scriptRunError(err) }
    return scripting.ToReply(res, cs.Proto)             // §5.6
}
```

`cmdScript` subcommands: `LOAD` (compile + cache, reply = sha1 hex bulk),
`EXISTS sha1...` (array of 0/1), `FLUSH [ASYNC|SYNC]` (OK; ASYNC drops the
cache map under a lock swap — cheap enough that both behave the same, but
keep the flags for client compatibility), `KILL` (Appendix C texts), and
`DEBUG` answering Redis's "not supported" error unless implemented.

`SCRIPT FLUSH` **must not** flush a script that is currently running: the
wasm bytes are held by the running `*Script` value, so a drop only affects
future lookups — verify with a test, and document (Redis semantics: same
tolerance).

**BUSY gate (S5)**: while `running > 0` and over the soft limit,
`Engine.Execute`'s gate cascade gains one more check — normal commands
reply `-BUSY Redis is busy running a script. You can only call SCRIPT KILL
or SHUTDOWN NOSAVE.`; `SCRIPT KILL`/`SHUTDOWN NOSAVE` pass. Under the hard
deadline the script dies on its own, so the BUSY window is bounded.
`SCRIPT KILL` with nothing running → `NOTBUSY No scripts in execution
right now.`; after a write happened → the UNKILLABLE text (Appendix C) —
with S5's hard kill, the UNKILLABLE branch can still occur inside the
soft-limit window; keep the Redis text.

### 5.4 The `redis.call` bridge (atomicity + persistence)

`lib/commands` installs the CallPath when it builds the Manager
(startup, after `NewEngine`):

```go
scripts.Bind(func(argv [][]byte) resp.Value {
    name := lowerASCII(argv[0])
    d, ok := table[name]
    if !ok { return errUnknownCommand(string(argv[0]), argv[1:]) }
    if slices.Contains(d.Flags, "noscript") ||
       slices.Contains(d.Flags, "blocking") ||
       name == "eval" || name == "evalsha" {
        return resp.Err("ERR This Redis command is not allowed from script")
    }
    if e.Shards.OverMemory() && slices.Contains(d.Flags, "denyoom") {
        // mirror Execute's OOM gate (evict-first when a policy is active)
        if e.Shards.Policy() == shard.PolicyNoEviction || !e.Shards.EvictNow(cs.tok) {
            return errOOM
        }
    }
    // ACL: re-check the command + its runtime-extracted keys against the
    // connection's identity (S9). Extract keys via the command's key spec
    // evaluated on argv (numkeys-style dynamic specs included).
    v := d.Handler(e, cs, argv)                 // runs under cs.tok (PauseAll)
    e.capturePersist(cs, d, name, argv, v)      // effects replication (S2)
    e.notifyMonitors(cs, append([][]byte{[]byte("lua")}, argv...)) // probe the "lua" prefix form
    return v
})
```

This is `cmdExec`'s inner loop, extracted — factor it as
`runInnerCommand(e, cs, argv) resp.Value` shared by EXEC and the bridge so
the AOF drain-tracking (`persistInFlight`), monitor feed, and capture rules
can never drift apart. `redis.pcall` wraps the same call: an error reply
comes back as a Lua table `{err = "..."}` instead of aborting;
`redis.call` aborts the script by raising the error text as a Lua error
(the host `host_call` `-1` path stages it).

Blocking commands are refused (a BLPOP inside a script would park a
connection goroutine while holding PauseAll — deadlock by construction).
Flag data comes from the probe of 7.2.7's `COMMAND DOCS`/commands.def
(`../ultima/note/redis`) recorded into Ultima's `CmdDef.Flags`, not from
memory.

### 5.5 Server surfaces beyond RESP

- **Manifest**: add all five commands with status `implemented`, correct
  group (`scripting`), `since` versions (EVAL/EVALSHA/SCRIPT 2.6.0, _RO
  7.0.0 — verify from the probe).
- **`COMMAND COUNT/INFO/DOCS`** pick the table up automatically; check the
  generated docs page includes the group.
- **INFO**: new `# Script` section: `loaded_scripts:n`, `running_scripts:n`,
  `script_time_limit_ms`, `script_hard_deadline_ms`, `script_max_memory_mb`.
- **HTTP API / web UI**: nothing mandatory — the console already runs
  arbitrary commands over the generic envelope. Optional follow-up: a
  script-browser panel (list cached SHA1s, source, run count).
- **gRPC/WS**: EVAL rides `CommandRequest` (generic). No typed message in
  M8.
- **MULTI interplay**: EVAL inside MULTI queues like any command (queue
  gate already handles it); EXEC then runs it under EXEC's own PauseAll —
  nested PauseAll must be verified (the token is per-call; assert
  re-entrant acquisition is a no-op or the same token, and add a test).
  SCRIPT LOAD etc. inside MULTI: allowed (they are `fast`, no writes to
  the keyspace).
- **Loading state**: refuse EVAL while the AOF restore is replaying
  (Redis refuses scripts during loading) — the synthetic replay
  ConnStates never produce EVAL since EVAL is never logged (S2), but a
  client-facing EVAL during startup needs the gate + probed error text.

### 5.6 Conversions (the parity surface — every row probed, Appendix C)

**Lua → RESP** (script return values and `redis.pcall`-style tables):

| Lua | RESP |
|---|---|
| number, integral (and `-0`? probe) | integer reply |
| number, non-integral | bulk string, shortest round-trip decimal (S7) |
| string | bulk string |
| `true` | integer 1 |
| `false` | RESP2 null bulk / RESP3 null (probe 7.2.7 exactly) |
| `nil` | null |
| table `{ok="..."}`-shaped (from `status_reply`) | simple string |
| table `{err="..."}`-shaped (from `error_reply`) | error reply |
| table, array-like (sequential from 1) | array, recursively converted |
| table, empty (probe: null vs empty array) | probe 7.2.7 |
| table, mixed/map | error (probe text; Redis: "ERR Cannot convert table ..." family) |
| nested table deeper than N (probe N) | error (probe text) |
| function/userdata/thread | error |

**RESP → Lua** (redis.call results, host-side writer emitting Tier-3
values per design §6.4):

| RESP | Lua |
|---|---|
| integer | number |
| bulk / simple string | string |
| null | `false` (Redis converts nil replies to Lua `false` — probe) |
| error | raised as a Lua error (call) / `{err=...}` (pcall) |
| array | table, sequential from 1 |
| RESP3 map/set/bool/double/big number | map table / array with converted elements / `true`/`false` / number / string (probe each) |

**Lua → command argv** (redis.call arguments): strings pass through;
numbers render as integers when integral else decimal (S7); anything else
is an error (probe the exact text — the "must be strings or integers"
family).

All conversion corner cases land in the differential corpus (§7) so the
probe results become executable regression tests, not folklore.

---

## 6. Milestone plan

Each phase is independently green: `go build ./... && go test ./...` in
both repos, `make lint` in Ultima, and the gopher-lua repo's full gate set
after any blob rebuild. Commit prefixes follow the repo convention
(`M8a: ...` in both repos).

### M8a — gopher-lua `host/` package core (in this repo) — **done (2026-09-22)**

Deliverables: §4.1 package skeleton; blob embed + pin + self-check; run
protocol steps 1–17 minus host functions; compile cache; result readback
for numbers/bools/nil/tables; `examples/eval`. The §4.2 runtime additions
land here too (blob rebuild + pin bumps + `go clean -testcache` + full
existing gates re-run).

**Exit gate**: §4.7 all green. `CGO_ENABLED=0 go build ./host/...`
succeeds. No behavior change in any existing gate.

*As built:* `host/{blob,wire,host,vm,hostfn}.go` + `host_test.go` (21
tests: results/readback, binary-safe KEYS/ARGV, hostfn round-trips and
verbatim error texts, panic containment, chaos host incl. the 65-deep
stack-overrun regression, deadline kill + reuse + ctx cancel, memory
budget, 5× determinism, sandbox surface, concurrent fresh VMs, shared-VM
serialization, one-script law, print sink) and
`examples/{eval,evalserver,sharedvm}` (evalserver demonstrates the
per-script VM pool the measured 44 ms/60 µs split motivates). The §4.2c
`lua_main` contract change is the one deviation from "no behavior change
in existing gates" — it is invisible to every corpus (status checks moved
`!= 0` → `< 0` in the two engines) and all suites re-ran green.

### M8b — Ultima EVAL without redis.call (pure scripts)

Deliverables: `lib/scripting` (Manager, cache, convert skeleton);
`lib/commands/eval.go` + `script.go` (LOAD/EXISTS/FLUSH only); config
group; manifest + INFO; PauseAll execution path; result conversion of
scalars/arrays. Scripts in this phase compute on KEYS/ARGV only —
`redis.call` returns a not-yet-available error (temporary, M8c removes
it).

**Exit gate**: `EVAL "return 1+1" 0` → `2` over RESP, gRPC-generic, and
WS-generic; `SCRIPT LOAD`/`EXISTS`/`FLUSH` round-trip; differential corpus
cases for pure scripts green (incl. `EVAL "return {1,2,{3,'four'}}" 0`
shaping); `-race` concurrent EVAL×N from many connections; deadline kill
on `while true do end`; OOM cap on a string-churning script; INFO section
present.

### M8c — redis.call / pcall + full bridge

Deliverables: `bridge.go` + CallPath factored out of `cmdExec`
(`runInnerCommand`); `redis.error_reply`/`status_reply`; noscript/
blocking/deny-OOM/ACL gates; monitor feed with the probed `lua` prefix;
MULTI-nested-EVAL test; the BUSY gate + `SCRIPT KILL` + soft/hard deadline
wiring; EVAL_RO write rejection.

**Exit gate**: the differential scripting corpus (Appendix C, ≥60 cases)
green byte-for-byte incl. error strings; AOF replay of a script's effects
produces identical keyspace state (crash-recovery test); MONITOR output
matches Redis's script-command form; `-race` stress with mixed
EVAL/normal traffic under PauseAll.

### M8d — parity hardening

Deliverables: remaining Appendix C probes turned into corpus rows; error
dialect edge cases (non-string error values, deep nesting, huge arrays);
`SCRIPT DEBUG` refusal text; loading-state gate; CLIENT / scripting
interactions (`CLIENT KILL` of a scripting connection mid-run — the VM
dies with the connection, effects partial, documented).

**Exit gate**: full `tests/differential` suite green on scripting;
overnight soak (mixed corpus, randomized deadlines/caps — mirror the
gopher-lua M6e soak methodology) with flat RSS slope; docs updated
(Ultima design doc M8 row status, `docs/m8-detailed-plan.md` written
retro-accurately; divergence ledger rows in *this* repo for S5-class
divergences).

### M8e — performance + closeout

Deliverables: `examples/bench` numbers (EVAL throughput vs Redis 7.2.7 on
the same box: fresh-VM vs pool, with/without PauseAll on pure scripts);
perf trip-wires; final docs; manifest status flips.

**Exit gate**: Ultima M8 exit criterion "differential green on covered
tail" holds for scripting; benchmark report committed to
`../ultima/docs/benchmarks/`.

---

## 7. Test plan

Mirrors both repos' cultures (gopher-lua's L-levels; Ultima's §14.3).

1. **Unit (both repos)** — host package §4.7; Ultima `lib/scripting`
   conversion tables as table-driven tests with the probed golden values.
2. **Concurrency** — `go test -race`: N conns × EVAL mix; shared-VM
   serialization; trampoline-goroutine assertion; PauseAll nesting.
3. **Chaos host** — wrong-typed `redis.call` replies, giant arrays,
   errors mid-iteration (host-behavior tests; engine must survive).
4. **Differential (the primary gate)** — extend
   `../ultima/tests/differential/scripts*.go` with a scripting corpus
   seeded from Appendix C: conversion corners, error texts, NOSCRIPT,
   FLUSH semantics, timeout behavior (Redis side: expect its non-preemptive
   behavior only where observable within the test's time bounds), EVAL_RO,
   SCRIPT KILL/NOTBUSY. Byte-exact reply comparison incl. error strings,
   RESP2 and RESP3 (`HELLO 3`) legs.
5. **Persistence** — AOF on → run write-scripts → crash-restart → keyspace
   identical; assert no `EVAL` verb ever appears in the AOF stream (S2).
6. **Soak** — overnight mixed corpus with randomized deadlines/caps; RSS
   slope + zero trap classes (mirror `docs/` M6e soak tooling in this
   repo).

Skip discipline: every skip carries a reason string (both repos' filter
conventions panic otherwise).

---

## 8. Acceptance checklist (M8 scripting exit)

- [ ] `redis-cli EVAL "return redis.call('set',KEYS[1],ARGV[1])" 1 k v` → OK; `GET k` → v — over RESP; same over gRPC and WS generic envelopes.
- [ ] `SCRIPT LOAD`/`EVALSHA`/`SCRIPT EXISTS`/`SCRIPT FLUSH` behave byte-identically to 7.2.7 (differential rows green).
- [ ] Script effects replicated to the AOF per-command; restart-identical; no EVAL verbs in the AOF.
- [ ] Atomicity stress: concurrent writer vs script doing read-modify-write — no torn observations (WATCH-style stress ported to scripts).
- [ ] `while true do end` killed at the hard deadline with a readable error; BUSY phase observed by a second client; SCRIPT KILL texts probed.
- [ ] Memory cap: `local s='x' while true do s=s..s end` → clean script-memory error, daemon RSS unaffected.
- [ ] Sandbox: no `os`, `io`, `require`, `loadstring`, coroutines reachable; filesystem untouched (blob has zero WASI imports — CI-checked).
- [ ] Determinism: same script+args+seed → identical replies and AOF effects, 5×.
- [ ] `CGO_ENABLED=0 go build ./...` green in Ultima; `go vet` clean; `make lint` clean; manifests/INFO/COMMAND reflect the five new commands.
- [ ] Both repos' full test suites green; divergence rows written for S5 (hard-kill partial effects) and any probe-discovered corners.

---

## 9. Risks, open questions, known divergences

| # | Item | Status / mitigation |
|---|---|---|
| R1 | `host/` package does not exist yet — M8a builds it. Scope creep risk into "full gopher-lua M7". | Hold the line at §4.1's API; everything else stays in testdiff. |
| R2 | String readback + hostfn seam touch the frozen ABI (new exports/import only). | Additive-only rule; full gate re-run + pin bumps after rebuild; ledger row if anything shifts. |
| R3 | wazero perf on darwin/arm64 is the interpreter engine (15–24× slower than wasmtime was measured, trip-wire 75×). | Production linux/compiled-engine numbers expected far better; M8e benchmarks decide whether a VM pool is warranted (S4). |
| R4 | Nested PauseAll (EVAL inside EXEC). | Must be verified in M8c before release; if token re-entrancy is not a no-op, serialize scripts and EXEC against a shared "strict path" mutex. |
| R5 | Hard-kill keeps partial effects (S5 divergence from Redis's UNKILLABLE stance). | Documented decision; differential cases structured so Redis's behavior is only asserted where observable. |
| R6 | Known number-dialect corners (ledger rows 38-survivor `math.huge` rendering, 40 arith error text, 48 pow 1-ulp). | Host-side RESP formatting (S7) keeps them out of replies; scripts observing `tostring(0.1^2)` still see the ledgered behavior — carry the rows into Ultima's differential skip list with reasons. |
| R7 | Backend v1 feature gaps (coroutine behavior divergence — row 22, debug introspection — row 23, runtime `loadstring` — sandboxed nil). | Corpus/differential skips carry reasons; upgrade plan (5.2→5.5, `docs/Upgrade-from-5.2-to-5.5-of-Lua.md`) may eventually change this surface. |
| R8 | Blob freshness: Ultima embeds a copy; upstream rebuilds drift. | SHA-256 pin + a host-package self-check that fails loudly on mismatch; version the artifact when the API freezes. |
| R9 | Lazy-pause parallel scripts (pure scripts skip PauseAll) — tempting superset. | Deferred past M8 (needs a sound "no observation before first call" argument + tests); noted for M9 with the §2 sketch. |

---

## Appendix A — run-protocol skeleton (Go, abbreviated)

Distilled from `testdiff/wazeroengine.go:118-377`; the host package is
this plus options/caching/errors-as-values (sketch, not paste-ready):

```go
func (vm *VM) run(ctx context.Context, s *Script, opt RunOptions) (Result, error) {
    // sketch — the as-built code lives in host/vm.go; note the budget
    // moved to Engine construction (WithMemoryBudgetBytes: rt_set_memlimit
    // must precede lnewstate, i.e. it is per-image, not per-run)
    vm.mu.Lock(); defer vm.mu.Unlock()                       // A9

    rtInst, mem := vm.rtInst, vm.mem
    call := func(m api.Module, fn string, a ...uint64) ([]uint64, error)

    L, _ := call(rtInst, "lnewstate")       // (budget set at NewVM)
    call(rtInst, "rt_set_state", L)
    call(rtInst, "rt_sandbox", 1)
    ldostring("collectgarbage('stop')")                      // v1 GC law
    ldostring("KEYS=" + luaTableLit(opt.Keys) + ";ARGV=" + luaTableLit(opt.Argv))
    if v, _ := call(rtInst, "rt_abi_version"); int32(v[0]) != 4 { /* refuse (v3's rt_set_dialect is gone — U3) */ }
    // hostfns registered at NewVM (§4.2a, rt_hostfn per name)
    call(vm.scriptInst, "luawasm_init", 2)
    frame := frameAlloc(scriptInst.Global("gFrameCells") * 16)

    var done atomic.Bool
    if opt.Deadline > 0 {
        flagAddr := vm.ctrlAddr
        time.AfterFunc(opt.Deadline, func() {
            if !done.Load() { mem.Write(flagAddr, []byte{1, 0, 0, 0}) }
        })
    }
    // plus host.deadline_poll: the guest polls every 2^20-th block
    // transition — the STW-proof layer (§4.3 step 13); the emitted
    // module imports it, so every instantiating host must define it
    st, err := call(vm.scriptInst, "lua_main", frame)        // nret or <0 (§4.2c)
    done.Store(true)
    if err != nil { /* trap = backend bug: log the module SHA */ }
    if int32(st[0]) < 0 { return Result{}, readStagedError(rtInst, mem) }
    return readResults(rtInst, mem, frame, int32(st[0]))     // §4.2b rt_encode_value
}
```

## Appendix B — the value wire cheat sheet (PT_* tags; ABI v4)

The only value contract a host needs. Cells themselves are 16-byte
variant-tag TValues — the blob's business (`runtime/rt_abi55.h`); the
host sees values exclusively through the wire: `rt_encode_value`,
`host.host_call` args/results, and event payloads.

- Tags: `0` nil, `1` false, `2` true, `3` number (LE f64), `4` string
  (u32 len + bytes), `5` table (u32 n + n × key value), `6` function,
  `12` more (an args-side table was truncated — `Value.More`),
  `13` integer (LE i64 — lossless, U6e).
- `lua_main` return: `n ≥ 0` results staged at `frame+0..16n`;
  `-1` error (TValue staged in runtime → `rt_err_stage_copy` /
  `rt_err_value_ptr`); `-2` tailcall sentinel; `-3` host refused.
- Host writes: `rt_mknumber/mkbool/mknil`, `rt_intern(ptr,len)`,
  `rt_newtable/gettable/settable/getglobal/setglobal` — the surface
  `runtime/tests/rt_native55_test.c` exercises natively.

## Appendix C — Redis 7.2.7 probe list (turn each into a differential row)

Probed with `redis-cli` against the `REDIS_BIN` (7.2.7) before coding each
piece; texts below are reminders, **not** authoritative:

1. `EVAL` arg validation order and texts: bad numkeys ("value is not an
   integer or out of range"), numkeys > args ("Number of keys can't be
   greater than number of args"), arity texts for eval/evalsha/script.
2. `EVALSHA` miss: the exact NOSCRIPT text.
3. Compile-error and runtime-error reply shaping: `return error("boom")`
   (prefix or none?), `error({err=...})`, error objects from pcall;
   position prefixes for chunkname `=[string "..."]` forms.
4. Conversion corners (§5.6 table): `-0`, `1e30`, `2^53`, `false`, `{}`,
   `{1,nil,3}` (holes!), map tables, nesting depth limit + text, huge
   arrays, function return values.
5. `redis.call` arg type errors; call of a noscript command; call of a
   blocking command; unknown command inside a script.
6. `_RO` write rejection text; `_RO` + `redis.call('set'...)` behavior.
7. `SCRIPT FLUSH` mid-connection; `SCRIPT EXISTS` multiple SHAs;
   `SCRIPT` arity/unknown-subcommand texts; `SCRIPT DEBUG` reply.
8. BUSY phase: text seen by a second client, allowlist (SCRIPT KILL,
   SHUTDOWN NOSAVE), SCRIPT KILL before/after a write (NOTBUSY /
   UNKILLABLE texts).
9. `math.random` observability; `redis.setresp` presence/absence in 7.2.7;
   `redis.log` (if kept, match levels).
10. MONITOR rendering of commands issued from scripts (the `lua` prefix).
11. `KEYS`/`ARGV` visibility rules (numkeys=0 with extra args → they land
    in ARGV).
12. EVAL under MULTI; SCRIPT under MULTI; EVAL during loading (-LOADING
    text).

## Appendix D — file-by-file work list

**gopher-lua repo (M8a):**

| File | Work |
|---|---|
| `host/host.go`, `host/vm.go`, `host/protocol.go`, `host/convert.go`, `host/hostfn.go`, `host/blob.go` | §4.1 package |
| `runtime/rt_abi55.c`, `runtime/build55.sh` | `rt_hostfn` + `host.host_call` import; `rt_encode_value`; export list; rebuild the rt + prod flavors (over the `runtime/luawasm55.c` drivers; U2d/U6d) |
| `testdiff/lua55_prod.wasm`, `host/lua55_prod.wasm` | rebuilt blob copies + SHA pin bumps (byte-cmp in `testdiff/m6c_test.go`, pin in `host/blob.go`) |
| `host/host_test.go`, `examples/{eval,evalserver,sharedvm,bench}` | §4.7 gates |

**Ultima repo (M8b–M8e):**

| File | Work |
|---|---|
| `go.mod` | gopher-lua require + dev replace |
| `lib/config/config.go` | `script` group (§5.1) |
| `lib/scripting/{scripting,cache,bridge,convert}.go` | §5.2 |
| `lib/commands/eval.go`, `lib/commands/script.go` | §5.3 |
| `lib/commands/table.go`, `lib/commands/manifest.json` | registrations + statuses |
| `lib/commands/engine.go` | BUSY gate in `Execute`; `runInnerCommand` factoring (with `tx.go`/`aof.go`) |
| `lib/commands/server.go` (or wiring site) | construct + Bind the Manager at startup |
| `tests/m8_script_test.go`, `tests/differential/scripts_m8.go` | §7 corpus |
| `docs/m8-detailed-plan.md`, `docs/ULTIMA-DESIGN.md` M8 row, `docs/benchmarks/M8-*.md` | closeout |

---

*Verification anchors used by this guide* (re-check before relying on a
claim): `testdiff/wazeroengine.go:118-377` (run protocol),
`testdiff/m6c_test.go:26-77` (blob contract), `testdiff/wasmengine.go:61,133,161`
(luaQuote, TValue render, CompileSource), `runtime/rt_abi55.h`
(exports), `docs/Lua-Wasm-Design-and-Test-Plan.md` §4–§6 + A8/A9,
`docs/Lua-Wasm-Divergence-Ledger.md` rows 22–25, 32–34, 38, 40, 48;
`../ultima/lib/commands/engine.go:130-479`, `tx.go:65-124`,
`aof.go:85-102`, `table.go:9-36`, `../ultima/lib/shard/shard.go:233,305,339`,
`../ultima/tests/differential/harness.go:1-40`,
`../ultima/docs/ULTIMA-DESIGN.md` §4.2, §7 P3, §14.4 M8, §16 D12.
