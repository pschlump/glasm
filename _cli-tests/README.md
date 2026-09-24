# _cli-tests — black-box CLI integration tests

Standalone Lua programs run as real CLI invocations across **three
engines**, with stdout diffed byte-for-byte against golden files in
`ref/`:

| engine | invocation                                                      | what it is                                           |
|--------|-----------------------------------------------------------------|------------------------------------------------------|
| interp | `../cmd/glua/glua prog.lua args...`                             | the golua55 interpreter                              |
| wasm   | `glua -w tmp/p.wasm prog.lua` then `glua -W tmp/p.wasm args...` | the Lua→wasm backend (compile, then run precompiled) |
| clua   | `tmp/clua-run prog.lua args...`                                 | stock C Lua 5.1 in wasm — the semantic authority     |

`make test` runs everything (~35 targets). `make refs` regenerates the
golden files — from interp (the primary oracle) except the per-engine
refs (`err1.*`, `syntaxerr.*`), which each engine generates itself.
Review `git diff ref/` before committing a regenerated ref: a changed
ref is a behavior change.

`make test WAZERO=1` (M6b, ledger row 32 — one blob, two hosts) swaps
every wasm leg from the wasmtime oracle to the pure-Go wazero
production engine (`GLUA_WASM_ENGINE=wazero`, understood by `glua -W`
and `luawasm-run`): same golden files, same diffs — the production
engine is byte-for-byte with the oracle over the whole CLI surface.

The three-engine shape is the point: interp and wasm share the
frontend, so a frontend bug leaves their diff green — only the C Lua
leg disagrees. Six real bugs were found this way on day one (ledger
rows 27–31 plus the io.write flush and the upstream multi-assignment
compiler bug); rows 27, 28 and 31 are fixed — the `xfailw` Makefile
macro remains for pinning future open rows.

## Programs

Unix-style tools: `cat` (-n), `grep` (-nvci, Lua patterns), `sortl`
(-nru, merge sort in plain Lua), `uniq` (-cd), `caesar` (ROT-N),
`wfreq` (word frequency). Compute: `primes` (sieve), `fib` (iterative
+ modular), `q8` (N-queens), `mandel` (ASCII Mandelbrot), `life`
(Conway on a torus), `bf` (brainfuck interpreter), `calc` (recursive-
descent expression evaluator), `kvdb` (persistent key-value store,
Redis-flavored). Error surfaces: `err1` (runtime errors, per-engine
refs), `syntaxerr` (compile refusal). `concatstress` pins ledger row
27. `lwc`/`lwc2` are the original wc clones (tests 000–002, two
engines — they predate the clua leg).

Data files live in `data/` (poem, names, nums, brainfuck programs,
a Life seed, calc expressions).

## Rules for cross-engine programs

These constraints keep one golden file valid for all three engines.
Each exists because of a measured divergence (see the divergence
ledger):

- **Numbers:** format explicitly — `string.format("%d")`,
  `"%.14g"`, `"%.Nf"`. Never bare `print(float)` or float `..` concat:
  interp's default number formatting is Go-shortest, the wasm engines'
  is C `%.14g` (row: default Number repr). Keep `%d` values < 2^31
  (wasm32). Avoid inf/nan reaching output.
- **One output channel per program:** `print` OR `io.write`, not both —
  the wasm engines buffer WASI stdout and emit it after the event log,
  so interleaving order is not preserved. End `io.write` output with a
  newline.
- **`table.sort` is fine** (row 10u was fixed with the M6 stack
  rebasing — `tsort.lua` gates it). `sortl.lua`/`kvdb.lua` keep
  hand-rolled sorts for recursion/closure coverage.
- **`..` concat is fine** (row 27's ≥16-in-a-frame corruption was a
  runtime Lua-stack leak, fixed); `string.format` and `table.concat`
  were always safe.
- **No coroutines** (row 22, out of scope), **no `os.exit`** in shared
  tests (wasm: traps and swallows buffered output), **no
  `io.stderr`** in shared tests (wasm engine drops it),
  **no `math.random`/`os.time`/`os.clock`/`os.getenv`** (interp is
  real, wasm engines are pinned/deterministic).
- **Don't print raw `io.open`/`os.*` error strings** — errno wording
  differs (interp gopher dialect vs C strerror). Print fixed text.
- **Don't print functions/tables/userdata** (addresses or engine
  markers), no embedded NULs, `%q` only on printable text.
- **Iterate hash tables only in sorted key order** — `pairs` order is
  engine-specific.
- **Error messages a program surfaces:** raise with `error(msg, 0)`
  (no position prefix) and print via pcall — clua's arg prelude shifts
  positions by one line and stock lib-error wording differs (row 25).
  `err1.lua` is the deliberate exception: it pins each engine's raw
  error surface against its own ref (interp and wasm now match
  byte-for-byte, including pcall-caught position prefixes — row 28
  fixed).
- **arg[0] differs** (`-W` passes the .wasm path) — print a fixed
  program name in usage messages instead.

## clua-run

`cmd/clua-run` (built into `tmp/clua-run`) wraps the testdiff C-Lua
oracle for CLI use. The engine has no args seam, so args are baked
into a one-line `arg={...}` prelude — shifting error positions by one
line relative to the other engines (another reason programs strip
position prefixes).
