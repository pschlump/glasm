# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

A fork of yuin/golua55-lua (Lua 5.1 VM + compiler in Go) extended with a **Lua 5.1 → WebAssembly compiler**. The end goal is running untrusted client scripts inside a **pure-Go Redis clone** (no cgo, no C in the daemon repo): the daemon consumes only a Go host package plus prebuilt, embedded wasm blobs.

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
#             TestWasmGluaFull (_glua-tests), TestCLuaConformance (_lua5.1-tests)

# CLI differential runner (exit 1 on any diff). wasm = wasmtime oracle
# host; wazero = the pure-Go production engine (same blob — ledger row 32)
go run ./cmd/testdiff -corpus _wasm-tests -engines interp,wasm
go run ./cmd/testdiff -corpus _wasm-err-tests -engines interp,wasm
go run ./cmd/testdiff -corpus _glua-tests -engines interp,clua
go run ./cmd/testdiff -corpus _wasm-tests -engines interp,wazero   # production-engine legs

# 5.5 oracle (U0 of the 5.2→5.5 upgrade, docs/Upgrade-from-5.2-to-5.5-of-Lua.md)
go test ./testdiff/ -run TestCLua55Conformance -v   # official 5.5.1 suite on wasmtime + wazero
go run ./cmd/clua55-run prog.lua                    # stock C Lua 5.5.1 in wasm (GLUA55_ENGINE=wazero: pure-Go host)

# Compile / run wasm artifacts
go run ./cmd/luawasmc [-o out.wasm] script.lua
go run ./cmd/luawasm-run [-v] artifact.wasm [args...]   # GLUA_WASM_ENGINE=wazero selects the production engine
cmd/glua/glua -w out.wasm script.lua   # compile (or -e 'stat')
cmd/glua/glua -W artifact.wasm         # run precompiled (also auto-detected by \x00asm magic)

# C runtime (needs wasi-sdk; default WASI_SDK=$HOME/wasi-sdk-dl/wasi-sdk-34.0-arm64-macos)
runtime/build.sh            # rebuild lua51_sjlj.wasm + lua51_prod.wasm → copies into testdiff/
runtime/build55.sh          # rebuild lua55_sjlj.wasm (5.5.1 oracle, U0) → copies into testdiff/
runtime/tests/run.sh        # native rt_* ABI unit tests: plain + ASan + UBSan

# CLI black-box tests (wc-clone in Lua, interp vs wasm paths)
cd _cli-tests && make                 # default: wasm legs on wasmtime
cd _cli-tests && make test WAZERO=1   # M6b: wasm legs on the wazero production engine
```

After rebuilding the runtime blob, run `go clean -testcache` — go-test caching can mask a stale embedded `lua51_sjlj.wasm`.

## Architecture

Three code-producing pieces plus a test harness, sharing the golua55-lua frontend (`parse/`, `ast/`, `compile.go` → `FunctionProto`, 41 opcodes — all reused untouched):

- **`wasm/`** — standalone pure-Go wasm binary emitter (sections, LEB128, instruction encoders).
- **`luawasm/`** — the backend: `FunctionProto` tree → wasm module. One wasm function per proto with common signature `(frame, cl, nargs, want) → nret`; registers are TValue cells at `frame+16*k`; control flow is flattened (`loop` + `br_table` over basic blocks). `backend.go` (module skeleton/init/dispatch), `emit.go` (function emitter), `ops.go` (opcode lowering).
- **`runtime/`** — vendored stock C Lua 5.1.5 (`runtime/lua51/`) + glue (`luawasm.c`, `rt_abi.c`) compiled to freestanding wasm32. Exports the frozen `rt_*` ABI the backend calls; hosts C functions and metamethod/coercion slow paths. The SJLJ/EH blob (`lua51_sjlj.wasm`) runs on wasmtime only — **wazero (pure Go) is the production engine**; an EH-free blob flavor is the tracked M6 item. `runtime/lua55/` (U0) vendors stock C Lua 5.5.1 with **zero patches** — driven by `luawasm55.c` (public C API only; determinism pinned by `build55.sh` flags) into `lua55_sjlj.wasm`, the new-oracle blob.
- **`testdiff/`** — differential harness. `Engine` interface with three engines: `interp` (the golua55-lua interpreter, primary oracle), `clua` (stock C Lua 5.1 in wasm on wasmtime, semantic authority), `wasm` (the luawasm backend on wasmtime). Engines produce normalized event logs (`PRINT`/`ERROR`/`STDOUT`/`GLOBALS` lines, `normalize.go`) that are diffed byte-for-byte; GLOBALS lines and interp traceback tails are excluded by contract. U0 adds `clua55`/`clua55-wazero` (stock C Lua 5.5.1 on both hosts — the semantic authority for the 5.2→5.5 upgrade per plan decision D6) and the `_lua5.5-tests` corpus (official suite, `TestCLua55Conformance`).

Corpora (all `_`-prefixed so the Go tooling ignores them): `_glua-tests`, `_lua5.1-tests` (conformance), `_wasm-tests` (~510 generated opcode-matrix cases), `_wasm-err-tests` (~93 byte-exact error cases), `_cli-tests` (CLI black-box).

Key invariants:

- **nret convention (ABI v3):** `>=0` = result count staged at `frame+0..`, `-1` = error (TValue staged in runtime), `-2` = tailcall sentinel, `-3` = host-refused.
- **Reentrancy law:** any `rt_*` body that `luaD_call`s can re-enter compiled code through the precall adapter — argument statics read after the call must live on the C stack.
- **Error dialect:** the wasm engine sets `rt_set_dialect(1)` so runtime error messages use golua55-lua wording (byte-exact vs the interp oracle); the clua oracle keeps stock C 5.1 texts.
- **GC is stopped per run in v1** (register cells are not GC roots); the M6 arena lifecycle is the durable fix.
- Upvalue capture info is NOT in `FunctionProto` — it lives in pseudo-instructions after `OP_CLOSURE`; the rt-owned upvalue registry must never touch `L->openupval`.

## Conventions

- **Ledger discipline:** a divergence between engines without a row in `docs/Lua-Wasm-Divergence-Ledger.md` is a bug; a row without a covering test is a bug. Every test skip must carry a reason string (`FilterSkips` panics otherwise).
- The interpreter (`_vm.go`/`_state.go`) is the spec: when changing backend or runtime behavior, match it exactly — error messages byte-for-byte, `luaV_*` semantics included.
- Commit style follows the milestone history: short prefix naming the milestone/area (e.g. `M5d: error/EH fidelity`, `M5e: full gates + docs`).
