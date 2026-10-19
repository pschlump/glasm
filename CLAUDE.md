# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

A fork of yuin/golua55-lua (Lua VM + compiler in Go, moved to the **5.2 language core as of U1** and the **stock 5.5.1 instruction encoding as of U2c** — `_ENV`, 5.2 escapes/numerals/libs, D8 removals, 85-opcode format; the full 5.2→5.5 program is `docs/Upgrade-from-5.2-to-5.5-of-Lua.md`) extended with a **Lua → WebAssembly compiler**. The end goal is running untrusted client scripts inside a **pure-Go Redis clone** (no cgo, no C in the daemon repo): the daemon consumes only a Go host package plus prebuilt, embedded wasm blobs.

Development is milestone-driven (M0–M8). The authoritative docs:

- `docs/Lua-Wasm-Design-and-Test-Plan.md` — architecture, decisions A1–A9, test pyramid, milestone history/gates
- `docs/Lua-Wasm-Divergence-Ledger.md` — every known engine divergence and its ruling
- `docs/M*-summary.md`, `note/` (gitignored) — working notes and current status

## Critical: generated files

**`state.go` and `vm.go` are GENERATED** from `_state.go` / `_vm.go` by `_tools/go-inline` (it inlines functions marked `// +inline-start` … `// +inline-end` at `// +inline-call` sites). Never edit `state.go`/`vm.go` directly — edit the `_`-prefixed source and regenerate via `make build` (or `make test`).

`_vm.go`/`_state.go` are the **executable spec** for the wasm path: every `rt_*` runtime contract mirrors what the interpreter does there (e.g. `opArith`, `stringConcat`, `lessThan`, `equals`, the vararg frame shuffle in `_state.go`).

## Commands

```sh
make build            # go-inline *.go && go fmt . && go build
make test             # go-inline + go test
go test ./...         # everything (root pkg = interpreter tests; testdiff = differential gates)
go test -short ./...  # skips heavy corpora (_lua5.1-tests etc. check testing.Short())
go test ./testdiff/ -run TestWasmFullMatrix -v   # run a single differential gate
# main gates: TestWasmFullMatrix (_wasm-tests), TestWasmErrorSuite (_wasm-err-tests),
#             TestWasmGluaFull (_glua-tests), TestCLuaConformance (_lua5.1-tests),
#             TestCLua55Conformance (_lua5.5-tests, 5.5 oracle), TestU1Gate/TestU2Gate (_u1/_u2: interp <-> clua55 byte-exact),
#             TestU2fMatrixOracle/TestU2fErrorOracle (the regenerated matrix + error suite interp <-> clua55, row 62),
#             TestU5WasmGate/TestU5Gate (_u5), TestU6SandboxCorpus (_u6 — absolute pins)
# The four heavy corpus gates run the U6f exit-gate engine set × {interp, clua55, wasm,
# wazero, wazero-prod} as three passes (row 98).

# CLI differential runner (exit 1 on any diff). wasm = wasmtime oracle
# host; wazero = the pure-Go production engine (same blob — ledger row 32);
# wazero-prod = the zero-wasi lua55_prod.wasm flavor (row 95).
# The wasm/wazero/wazero-prod legs run the U2e backend on the ABI v4 blobs
# (ledger row 61); interp/clua/clua55 legs unchanged.
go run ./cmd/testdiff -corpus _wasm-tests -engines interp,wasm
go run ./cmd/testdiff -corpus _wasm-err-tests -engines interp,wasm
go run ./cmd/testdiff -corpus _glua-tests -engines interp,clua
go run ./cmd/testdiff -corpus _wasm-tests -engines interp,wazero       # production-engine legs
go run ./cmd/testdiff -corpus _wasm-tests -engines interp,wazero-prod  # prod-blob legs

# Benchmarks (U7c) and the differential fuzz soak (U7b/U7d)
go run ./cmd/luabench [-only substr] [-benchstat] [-cpuprofile dir]  # interp TOTAL / wasm cold+warm / wazero-prod × 7 scripts
nohup bin/soak-7nights.sh > note/fuzz-counting.log 2>&1 &  # multi-night counting soak; window pinned in note/fuzz-window-end, note/fuzz-stop ends it
bin/check-fuzz-status.sh        # = make fuzz-status; nightly totals in note/fuzz/soak.json
go run ./cmd/luafuzz -replay -seed S -from N   # replay soak case N for triage

# 5.5 oracle (U0 of the 5.2→5.5 upgrade, docs/Upgrade-from-5.2-to-5.5-of-Lua.md)
go test ./testdiff/ -run TestCLua55Conformance -v   # official 5.5.1 suite on wasmtime + wazero
go test ./testdiff/ -run TestU1Gate -v            # U1: 5.2 core, interp vs the 5.5 oracle
go run ./cmd/clua55-run prog.lua                    # stock C Lua 5.5.1 in wasm (GLUA55_ENGINE=wazero: pure-Go host)

# Compile / run wasm artifacts
go run ./cmd/luawasmc [-o out.wasm] script.lua
go run ./cmd/luawasm-run [-v] artifact.wasm [args...]   # GLUA_WASM_ENGINE=wazero selects the production engine
cmd/glua/glua -w out.wasm script.lua   # compile (or -e 'stat')
cmd/glua/glua -W artifact.wasm         # run precompiled (also auto-detected by \x00asm magic)

# C runtime (needs wasi-sdk; default WASI_SDK=$HOME/wasi-sdk-dl/wasi-sdk-34.0-arm64-macos)
runtime/build.sh            # rebuild lua51_sjlj.wasm + lua51_prod.wasm → copies into testdiff/
runtime/build55.sh          # rebuild lua55_sjlj.wasm (5.5.1 oracle, U0) + lua55_rt.wasm (ABI v4, U2d) + lua55_prod.wasm (zero-wasi prod flavor, U6d — copies into testdiff/ and host/)
runtime/tests/run.sh        # native rt_* ABI unit tests: plain + ASan + UBSan

# CLI black-box tests (wc-clone in Lua, interp vs wasm paths)
cd _cli-tests && make                 # default: wasm legs on wasmtime (row-59 skip until U2e)
cd _cli-tests && make test WAZERO=1   # M6b: wasm legs on the wazero production engine
```

After rebuilding the runtime blob, run `go clean -testcache` — go-test caching can mask a stale embedded `lua51_sjlj.wasm`.

## Architecture

Three code-producing pieces plus a test harness and the daemon-facing package, sharing the golua55-lua frontend (`parse/`, `ast/`, `compile.go` → `FunctionProto` — reused; `compile.go` was rewritten onto the 5.5.1 encoding at U2c):

- **`wasm/`** — standalone pure-Go wasm binary emitter (sections, LEB128, instruction encoders).
- **`luawasm/`** — the backend: `FunctionProto` tree → wasm module. **Rewritten at U2e (ledger row 61)** onto the stock 5.5.1 encoding via the exported `lua.OpGet*` decoders and ABI v4 (`runtime/lua55_rt.wasm`): one wasm function per proto with common signature `(frame, cl, nargs, want) → nret`; registers as TValue cells at `frame+16*k` (frame window `nregs+5` + multret headroom, row 92); flattened control flow (`loop` + `br_table`); int64/float fast paths with every miss folding into the MMBIN* slot's `rt_arith`; the M5c tailcall restage over the CClosure seam. The row-86 frame-layout surgery (U6a) put the vararg extras BELOW the window and made calls callee-addressed (`rt_call_addr`) — life1/tsort1 run un-xfailed.
- **`runtime/`** — vendored stock C Lua 5.1.5 (`runtime/lua51/`) + glue (`luawasm.c`, `rt_abi.c`) compiled to freestanding wasm32. Exports the frozen `rt_*` ABI the backend calls; hosts C functions and metamethod/coercion slow paths. The SJLJ/EH blob (`lua51_sjlj.wasm`) runs on wasmtime only — **wazero (pure Go) is the production engine**; an EH-free blob flavor is the tracked M6 item. `runtime/lua55/` (U0) vendors stock C Lua 5.5.1 with **zero patches** — driven by `luawasm55.c` (public C API only; determinism pinned by `build55.sh` flags) into `lua55_sjlj.wasm`, the new-oracle blob. U2d adds **ABI v4 on that same zero-patch tree**: `rt_abi55.c` (`lua55_rt.wasm` flavor) — variant-tag cells, `luaO_arith` semantics, and the CClosure dispatch seam replacing the 5.1 tree's patched-ldo.c adapter (ledger row 60); the U2e backend targets it. U6d adds the zero-wasi production flavor `lua55_prod.wasm` (row 95): source exclusion (liolib/loslib/loadlib/ldblib + linit.c) + `runtime/linit55.c` (defines `luaL_openselectedlibs` — 5.5's `luaL_openlibs` is a macro) + `-D`' write macros + driver-level libc preemption. U6e adds the **hostfn seam** (row 96): `rt_hostfn`/`rt_encode_value` exports + the `host.host_call` import (rt + prod flavors import exactly `host.{event, wasm_dispatch, host_call}`; the oracle blob keeps two).
- **`testdiff/`** — differential harness. `Engine` interface with the engines: `interp` (the golua55-lua interpreter, primary oracle), `clua` (stock C Lua 5.1 in wasm on wasmtime), `clua55`/`clua55-wazero` (stock C Lua 5.5.1 — the semantic authority for the 5.2→5.5 upgrade, U0/D6), and `wasm`/`wazero` (the luawasm backend on the v4 `lua55_rt.wasm` blob — row 61; the wazero-prod legs run the zero-wasi `lua55_prod.wasm` flavor, row 95). Engines produce normalized event logs (`PRINT`/`ERROR`/`STDOUT`/`GLOBALS` lines, `normalize.go`) that are diffed byte-for-byte; GLOBALS lines and interp traceback tails are excluded by contract. The heavy corpus gates run the U6f exit-gate engine set — × {interp, clua55, wasm, wazero, wazero-prod} as per-column RunCorpus passes (wasm-family / oracle / prod, each with its own skip layer — row 98). The clua55 column sweeps inline only on `_glua-tests`; the big corpora's oracle columns are TestU2fMatrixOracle/TestU2fErrorOracle and `_u5`'s is TestU5Gate — every wasmtime Store statically maps the blob's 256 MiB memory + ~8 GB of arm64 guard region, and past ~4k stores per process the test binary dies with SIGTRAP (the M6e cliff; Go finalizers reclaim far too slowly), so oracle sweeps are not duplicated. U0 adds `clua55`/`clua55-wazero` (stock C Lua 5.5.1 on both hosts — the semantic authority for the 5.2→5.5 upgrade per plan decision D6) and the `_lua5.5-tests` corpus (official suite, `TestCLua55Conformance`).
- **`host/`** — the daemon-facing production package (what a pure-Go Redis clone links): `Engine` (compile cache + hostfn registry) / `VM` (one locked Lua image — the A9 mutex law, one script per image) / `Script`, over wazero. Embeds `lua55_prod.wasm` (SHA-pinned, byte-cmp'd against testdiff's copy — the embed-dance drift lock). The value wire (`wire.go`, PT_* tags shared with `luawasm55.c`) carries integers lossless (`KindInteger`, U6e); `RegisterGlobal` installs Go host functions via the `rt_hostfn`/`host.host_call` seam (errors re-raise verbatim — Redis command texts); `WithGlobalsProtection`/`WithMemoryBudgetBytes`/`Deadline`/`Seed` map onto the rt surfaces. **Row 99 (U6f):** a Go GC mark-STW landing inside a guest watchdog window wedges the process — the spinning loop is unpreemptible wazero-JIT code and pending STW freezes the timer goroutine that writes the deadline flag; the watchdog tests quiesce the host GC (`noHostGC`), and the daemon-durable writer landed at U7a (row 100: the guest polls `host.deadline_poll` every 2²⁰-th block transition — an import contract every instantiating host must now satisfy). Gate: `go test ./host/` plain + `-race` ×2.

Corpora (all `_`-prefixed so the Go tooling ignores them): `_glua-tests`, `_lua5.1-tests` (conformance), `_wasm-tests` (~1440 generated opcode-matrix cases incl. the U2f int/float/bitwise grids), `_wasm-err-tests` (~93 byte-exact error cases), `_cli-tests` (CLI black-box), `_lua5.5-tests` (official 5.5.1 suite), `_u1` (U1's 5.2-core cases, interp↔clua55).

Key invariants:

- **nret convention (ABI v3 + v4):** `>=0` = result count staged at `frame+0..`, `-1` = error (TValue staged in runtime), `-2` = tailcall sentinel, `-3` = host-refused. **ABI v4 (U2d, `runtime/rt_abi55.{c,h}` → `lua55_rt.wasm`)** is the stock-5.5-tree port: variant-tag cells (lossless int64), `luaO_arith` semantics/texts, `RT_OP_*` = LUA_OP* numbers, and the zero-patch CClosure dispatch seam (compiled protos = CClosures over `host.wasm_dispatch`; staging clears at the re-raise — ledger row 60). The U2e backend targets v4; the 5.1 blob stays on v3 permanently (the `clua` oracle's — that tree retires at U8).
- **Reentrancy law:** any `rt_*` body that `luaD_call`s can re-enter compiled code through the precall adapter — argument statics read after the call must live on the C stack.
- **Error dialect:** the v3 dialect layer is **retired** (U3, row 56 — the interp oracle was re-pinned to stock texts, so every 5.5 engine speaks `luaO_*` wording natively; no `rt_set_dialect` exists on ABI v4). The 5.1 oracle (`clua`) keeps stock C 5.1 texts, as it must. Hostfn error values re-raise position-less and verbatim (row 96).
- **GC is frozen per run** (`rt_set_state` sets GCSTPGC + gcstopem — pool/frame cells are not GC roots, and 5.5's emergency GC would sweep the frozen heap on the OOM path, row 93; ledger row 61). Ruled the durable posture at U6 (U6-plan §4.3): real rooting needs patches or a parallel GC (both out — zero-patch/D10), and the memlimit bounds frozen-GC growth.
- Upvalue capture info lives in `FunctionProto.Upvalues []Upvaldesc{InStack, Idx, Name}` (U2c — stock's shape; the fork's old pseudo-instructions after OP_CLOSURE are gone); the rt-owned upvalue registry must never touch `L->openupval`.
- **The instruction set is stock 5.5.1 (U2c/D2): 85 opcodes**, 7-bit opcode field, A/B/C 8 bits + 1-bit `k`, `Bx` 17, `iAx`/`isJ` 25, `ivABC` (vB 6 + vC 10) for NEWTABLE/SETLIST — `opcode.go` mirrors the vendored `runtime/lua55/src/lopcodes.h` exactly; every arith op carries an MMBIN* fallback slot; exported `OpGet*`/`OpCreate*` are the decode API for the U2e backend.
- **`_ENV` (U1/D3, U2c shape):** every main proto declares `_ENV` as upvalue 0 (`FunctionProto.EnvUpvalueIndex` records the slot for the loader seeding); globals read/write as fields of the frame function's `_ENV` upvalue CELL via GETTABUP/SETTABUP (so `_ENV = t` swaps propagate through shared cells), and `local _ENV` shadowing lowers to GETFIELD/SETFIELD on the local. On the wasm side, parentless captures (main runs cl=0) seed the globals table (ledger row 53) — real 5.2 environments land with the U2e/U6 rt work.

## Conventions

- **Ledger discipline:** a divergence between engines without a row in `docs/Lua-Wasm-Divergence-Ledger.md` is a bug; a row without a covering test is a bug. Every test skip must carry a reason string (`FilterSkips` panics otherwise).
- The interpreter (`_vm.go`/`_state.go`) is the spec: when changing backend or runtime behavior, match it exactly — error messages byte-for-byte, `luaV_*` semantics included.
- Commit style follows the milestone history: short prefix naming the milestone/area (e.g. `M5d: error/EH fidelity`, `M5e: full gates + docs`).
