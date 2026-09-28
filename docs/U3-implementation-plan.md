# U3 implementation plan — 5.3/5.4 stdlib

> Parent plan: `docs/Upgrade-from-5.2-to-5.5-of-Lua.md` §6 (U3 row), decisions D6, D7, D8, D11.
> Same working-breakdown convention as `docs/U2-implementation-plan.md`: independently gated
> chunks, documented deviations, status log. The divergence-ledger discipline applies to every
> chunk: a divergence without a row is a bug; a row without a covering test is a bug.

## 0. What U3 is (from the parent plan)

The 5.3/5.4 standard library in the Go interpreter, gated against the stock-5.5.1 oracle:

- `string.pack`/`unpack`/`packsize` (new Go implementation — the long pole),
- `utf8` library (new Go file),
- `table.move`,
- `math.type` family + 5.3 removals + integer-exact floor/ceil/fmod/modf,
- string-metatable arithmetic (the 5.4 coercion model — closes ledger row 58),
- hardwired `print`, xoshiro `math.random`, `coroutine.close`, `warn`,
- `io.lines`/`io.read` 5.3/5.4 shapes.

Gate (milestone table): official `strings.lua`, `tpack.lua`, `utf8.lua`, `math.lua`,
`bwcoercion.lua` green `interp`↔`clua55`; stdlib corpus regenerated for the wasm legs.
Effort estimate: 2–5 person-weeks, `string.pack` the long pole.

## 1. Ground-truth inventory (verified 2026-09-28)

- **The five gate files parse 5.5 syntax the interp does not accept yet.** All five use the
  `global` statement (`global <const> *`, `global none` — which is just an attnamelist
  declaring a name `none` —, `global<const> print, assert, ...`, `global ipairs`);
  math.lua/strings.lua/tpack.lua use `local` **attributes** (`local<const> a, b = ...`,
  `local x <const> = ...`); strings.lua has one **named vararg param** (`local function aux
  (...t)`) inside a `T`-guarded dead branch (parse+compile acceptance only). The 5.5 grammar:
  `attnamelist ::= [attrib] Name [attrib] {',' Name [attrib]}`, `attrib ::= '<' Name '>'`,
  `varargparam ::= '...' [Name]`.
- **strings.lua's float-format block is dialect-tolerant by construction**: line 151 probes
  `if tostring(0.0) == "0.0"` and only tests the `%.14g`-with-`.0` spellings inside that
  branch. math.lua's tostring asserts are *round-trip properties*
  (`tonumber(tostring(x)) == x`, distinct floats print distinctly, 5-digit numerals print as
  themselves) — all satisfied by the Go shortest-repr dialect. **So the D7 float-printing
  re-pin stays at U5**; U3's format work needs C-parity only inside `string.format`.
- **`%p` never enters the event log** — the whole block is asserts (null-ness, per-object
  distinctness/stability, short-string interning observability, `io.stdin` stability), so the
  interp's `%p` spelling need not match clua55, only the properties. Same for table/function
  addresses in `tostring` (asserted as prefixes `'table:'`/`'function:'`).
- **math.lua itself needs `string.pack`** (the tostring-precision section packs random bits
  as `"j"` and reads them back as `"n"`), and needs the *exact* xoshiro stream: seed-1007
  first draw `0x7a7040a5a323c9d6`, `randomseed()` no-arg returning the two state words (the
  oracle's `luai_makeseed()` is pinned to `0u` by `build55.sh:67` — the interp must mirror
  that pin: initial state `setseed(0, 0)`, no-arg `randomseed()` → `n1=0`,
  `n2=next-draw-continuing-the-stream`), `random(0)` full 64-bit draws, `randomseed(x, y)`
  returning both seeds.
- **Stock 5.5.1 keeps `math.frexp`/`math.ldexp`** (re-added after removal in 5.3/5.4;
  lmathlib.c registers both; math.lua:688-697 exercises them). The parent plan's Appendix B
  ("remove frexp/ldexp") is wrong for 5.5.1 — the oracle is the authority (D6); they stay.
- **table lib is metamethod-honoring since 5.3** (ltablib `checktab`/`TAB_R|TAB_W|TAB_L`,
  `lua_geti`/`lua_seti`/`luaL_len` element access), `table.move` and `table.create` are both
  registered in 5.5.1 (`tmove`, `tcreate`).
- **`string.format` must be a real port** (the fork's is `fmt.Sprintf` passthrough): strings.lua
  asserts `%q` round-trips incl. `format("%q", 0/0) == "(0/0)"`, `%a`/`%A` hex-float
  spellings + round-trips, `%.99f` round-trips of 10^308 floats, `%p`, `%u`/`%o`/`%c`,
  truncation of `__tostring` results by `%.3s`, `__name` in `%s`, and the exact error texts
  ("invalid conversion '%s' to 'format'", "invalid conversion %s to 'format'"?? — see §4
  U3f; "width/precision too long", "cannot have modifiers", …).
- **The string metatable in 5.5.1 carries only `__add __sub __mul __mod __pow __div __idiv
  __unm __index`** (lstrlib `stringmetamethods` — no bitwise metamethods, no `__len`);
  coercion is subtype-preserving (`tonum` via `luaO_str2num`, whole-string), the failure path
  is `trymt`: if arg 2 is not a string *and* has its own metamethod → call it, else
  `"attempt to %s a '%s' with a '%s'"` with opname = metamethod name minus `__`. The
  metatable is a **separate table** from the library (`createmetatable`), `__index` = the
  library table — the fork currently uses the module table itself as the mt.
- **The interp engine has no SuiteFlags equivalent** (the official files consult
  `_port`/`_soft`; the clua55 engine runs `_port=true _soft=true _nomsg=true` as a separate
  prelude chunk). The interp leg sets the globals before the run (not source prefixing —
  that would shift line numbers).
- **utf8 loads via `require'utf8'`** — works once the lib registers through
  `RegisterModule` (which records `_LOADED` in the registry, the interp's require path).
- wasm32 size profile for pack (the oracle blob's C type model — the Go port must match the
  ORACLE, not native Go): `j/J`=8, `n`=8 (double), `T`=**4** (wasm32 `size_t`), `s` default
  length prefix = **4**, `l/L`=4, `i/I` default 4, `h/H`=2, `f`=4, `d`=8, native little-endian,
  `!` default maxalign = **8** (offsetof of LUAI_MAXALIGN's double member), `!n` cap 16,
  MAXINTSIZE 16.
- 5.5.1 `math.random` details: `random(0)` → full 64-bit integer (5.5 feature), `random(a,b)`
  via `project` (Mersenne rejection), `>2` args → `"wrong number of arguments"`, empty
  interval → argcheck 1 `"interval is empty"` (the only interval text);
  `randomseed()` no-arg is *not* nil-accepting (`lua_isnone` — `randomseed(nil)` errors).

## 2. The chunks

| # | Name | Scope | Gate |
|---|---|---|---|
| U3a | Parse acceptance: `global`, attributes, named varargs | contextual `global` statement, `local`/`global` attnamelist with `<const>`/`<close>` ignored, `...name` vararg param (hidden local, U5 semantics later) | the five official files compile on interp; ledger row "no-op until U5"; existing corpora unchanged |
| U3b | math library, stock surface + xoshiro | function set re-pin (removals per D8; frexp/ldexp stay — §1), integer-exact abs/floor/ceil/fmod/modf, tointeger/type/ult/maxinteger/mininteger, atan(y[,x]), log(x[,base]), min/max compare order, xoshiro256** port with the makeseed=0 pin; interp engine random shim removal + rt-blob D5 rng shim retirement (rebuild) | `_u3` math slice interp↔clua55 byte-exact; **math.lua fully green**; `mod00` un-skipped on all legs |
| U3c | string-metatable arithmetic (row 58 close) | separate mt table (stock `createmetatable` shape), `tonum`/`trymt`/`arith` as Go functions, `objectArith` drops the VM-level coercion, UNM path re-routed | bwcoercion.lua green; `_u3` string-arith slice |
| U3d | table library, stock surface | `move`, `create`, metamethod-honoring insert/remove/concat/unpack/sort (`checktab` model), concat error texts, wrong-arity texts | `_u3` table slice |
| U3e | utf8 library | new `utf8lib.go`: char/charpattern/codepoint/codes/len/offset (5.5 offset 2-return), strict/lax decode | utf8.lua green |
| U3f | string.format port | full `str_format`: flags/width/precision, `%c d i u o x X a A e E f F g G q s p %%`, C-parity float rendering (incl. hex floats, `(0/0)`), `__tostring`/`__name`, exact error texts | strings.lua format block |
| U3g | string.pack/unpack/packsize | wasm32 size profile (§1), full option set `b B h H i I j J l L T n f d s z c x X ! = < >`, alignment, sign extension, exact texts | tpack.lua green |
| U3h | io/base/coroutine smalls | io.read unstarred per-arg options + `"n"` subtype + integer counts, io.lines/file:lines 5.4 shape, hardwired print text (`'__tostring' must return a string to 'print'`, `__name` in tostring), `warn`, `coroutine.close`, `coroutine.isyieldable` (+ 5.5 no-arg forms) | `_u3` slices; existing gates stay green |
| U3i | corpora + gates + ledger + closeout | `_u3` corpus, `u3_test.go` (TestU3Gate byte-exact + TestU3OfficialSuite over the five files, interp engine SuiteFlags), matrix stdlib slice + regeneration, skip-map updates, ledger rows, docs, memory | full U3 exit gate |

Dependencies: U3a unblocks everything gated by the official files (b/e/f/g). U3b..U3h are
otherwise independent; U3i last. Each chunk lands green (or with documented, ledgered
triage) before the next starts.

## 3. Chunk details

### U3a — parse acceptance (contextual, no-op)

`parse/lexer.go` unchanged (`global` stays an identifier). The parser (statement dispatch)
recognizes a statement starting with `NAME=="global"` **followed by** `*`, `<`, `function`,
or another NAME — `global = 1`, `global, x = 1, 2`, `global.f()` stay expression statements
(the 5.1-era corpora keep working). Accepted forms parse to the existing AST nodes and
lower as no-ops: `global attnamelist [= explist]` evaluates the explist and assigns the
names as plain globals (assignment observable, declaration not); `global function f() end`
= plain function assignment; `global [attrib] *` = nothing. Ledger row: **`global`
declarations accepted-as-no-op until U5** (resurrecting the U2 plan's ruling 2 — U2f's
closeout took the port-into-`_u2` path instead; U3's gate forces the files to run
unmodified).

Attributes: the local/global attnamelist parser accepts `<const>`/`<close>` (any `<NAME>`)
in both grammar positions and **ignores** them (U4 does `<const>`/`<close>` for real;
read-only enforcement of const locals is a compile error there). Named vararg param
`...name`: parses, lowers as `...` plus a hidden `local name = table.pack-like` **only when
the body references `name`** — simplest compile-valid shape: hidden `local name` bound to a
lazy vararg view is U5 work; at U3a a plain `local name = {}`-style placeholder compiles
the dead branches. (strings.lua's `aux (...t)` sits in a `T`-guarded branch that never
runs; only parse+compile must succeed.) Ledger row carries the approximation.

### U3b — math library

`mathlib.go` rewrite against lmathlib.c (reference quotes in the working notes):
removals per D8 — `pow log10 atan2 cosh sinh tanh mod` (the deprecated block is not
compiled; `math.mod` was 5.1-only); **kept**: `frexp`/`ldexp` (§1). Additions:
`tointeger` (lua_tointegerx semantics: numeric strings convert, integral floats convert,
else **false** — `luaL_pushfail`, not nil), `type`, `ult`, `maxinteger`, `mininteger`;
`atan(y[,x])` → atan2; `log(x[,base])` incl. log2/log10 fast paths; min/max via
subtype-aware compare (returns the first operand on ties — stock index semantics), arity
error `"value expected"`. Integer-exact: `abs` (mininteger → mininteger), `floor`/`ceil`
(integer in → integer out; float in → integer when representable else float, via
`pushnumint`), `fmod` (int/int → C `%` truncated, d==-1 → 0, d==0 → argcheck 2 `"zero"`;
any float → float fmod), `modf` (integer in → (int, 0.0); float in → (int-if-fits,
fraction)). `huge` = +Inf (row-56 fix already in), `pi`.

xoshiro256** (the 64-bit `Rand64` path — wasm32 uses `unsigned long long` but the math is
word-width-agnostic): `nextrand`, `I2d` (FIGS=53: `sx = int64(x >> 11)`,
`res = float64(sx) * 2^-53; if sx < 0 { res += 1 }`), `project` (Mersenne spread +
rejection), `math.random` arg handling (`0` → full integer; `low > up` →
`bad argument #1 to 'random' (interval is empty)`), `setseed` (state[0]=n1, state[1]=0xff,
state[2]=n2, state[3]=0, 16 discarded draws; returns n1, n2), `math.randomseed`
(x[,y] → both seeds returned; none → n1=0 per the makeseed pin, n2=continuing draw).
State lives on the *GlobalState*; **initial state = setseed(0, 0)** matching the blob pin
(`-D'luai_makeseed()=0u'`).

Engine alignment: the interp engine's `installShim` random/randomseed replacements are
**deleted** (the lib is now the deterministic stock surface); the rt flavor's D5 host-RNG
shim is **retired** (`runtime/luawasm55.c` `install_g_rng` block dies; blob rebuild via
`build55.sh`; both blobs stay byte-deterministic). Ledger row closes the D5 shim (row 61's
rng clause) — all four engines now speak stock xoshiro over the same initial state.
`go clean -testcache` after the rebuild.

### U3c — string-metatable arithmetic (row 58)

`OpenString` builds the stock shape: `mt := L.NewTable()`, `mt.__index = stringlib table`,
arith metamethods as Go functions; `L.G.builtinMts[LTString] = mt` (the module table stops
being the mt — `getmetatable("")` returns the mt with `__index` pointing at the library).
Each `__add`-class function is `arith(L, LUA_OP*, "__add")`: `tonum` both operands
(numbers pass verbatim — subtype preserved; strings convert iff `parseNumber` consumes the
whole string), compute via the existing subtype-correct `arithCompute`; on coercion failure
`trymt`: if operand 2 is not a string and `metaOp1(p2, mtkey)` yields a function → call it
with both operands; else `RaiseError("attempt to %s a '%s' with a '%s'", opname, t1, t2)`.
`objectArith` loses its VM-level string coercion (and the U2f text workaround — the texts
now come from the metamethod itself, exactly like stock); the UNM/BNOT string paths route
through `metaOp1(v, "__unm")` (BNOT stays a type error — no `__bnot` on the stock string
mt). `coerceStringNumber` survives only where stock has real str2num coercion (for-loop
operands).

### U3d — table library

Port the `checktab` model: every function resolves element access through
`__index`/`__newindex` metamethods when present and length through `__len` (`luaL_len`
error `"object length is not an integer"`), per-function read/write/len requirements from
ltablib (`insert/remove/sort` RW+L, `concat/unpack` R+L, `move` source R / dest W, no
checks for `pack/create`). `tmove` verbatim (overflow argchecks `"too many elements to
move"` / `"destination wrap around"`, copy direction by range overlap, returns dest).
`tcreate` (sizeseq/sizerest hints, `"out of range"` argchecks). `tinsert`/`tremove`
bounds texts `"position out of bounds"`, wrong-arity `"wrong number of arguments to
'insert'"`. `tconcat` text `"invalid value (X) at index %d in table for 'concat'"`
(integer-formatted index — maxint/minint spelling must match). `sort` keeps the existing
Go sort but through the metamethod-aware accessors + `"invalid order function for
sorting"` on comparator errors; the blob's pivot randomization is makeseed-pinned (0) and
hash-order — already engine-specific (standing corpus discipline, insertion-order note in
row 62's map).

### U3e — utf8 library

New `utf8lib.go`, registered in `linit.go` (so `require'utf8'` resolves). Port
lutf8lib.c: `utf8_decode` (up to 5 continuation bytes, overlong `limits[]` rejection,
MAXUTF 0x7FFFFFFF lax ceiling, strict rejects >MAXUNICODE and surrogates), `len`
(fail + offending byte position), `codepoint` (j defaults to i), `char` (up to MAXUTF,
`"value out of range"`), `offset` (n==0 → char start; walk back/forward; **5.5 two-value
return** posi and last-continuation-byte position), `codes` (iterator + string + 0;
iterator errors `"invalid UTF-8 code"`), `charpattern`
`"[\0-\x7F\xC2-\xFD][\x80-\xBF]*"`. Error texts: `"invalid UTF-8 code"`,
`"initial position is a continuation byte"`, `"position out of bounds"`,
`"out of bounds"`, `"string slice too long"`, `"value out of range"`.

### U3f — string.format

Replace the `fmt.Sprintf` passthrough with a port of lstrlib `str_format`/`checkformat`:
flag/width/precision parsing (incl. `*`), verbs `%c d i u o x X f F e E g G q s a A p %%`;
integer verbs accept integral floats via `lua_tointegerx`-style conversion else
`"number has no integer representation"`; floats render through a C-parity layer:
`%e/%f/%g` families via `strconv.FormatFloat` (matches C sprintf for these forms),
special-casing `inf`/`-inf`/`nan` spellings and **hex floats `%a/%A`** (hand-rolled:
C spelling `0x1.9p+3`, exponent digits minimal, `0x0p+0` for zeros, normalization
differences vs Go's `p+03`); `%q` = stock quoting (escapes, `\ddd` for embedded zeros,
floats → hex form via `quotefloat`, `(0/0)` for NaN, `"no literal"` for
non-number/string/boolean); `%s` via the stock tolstring path (`__tostring` with
`"'__tostring' must return a string"`, `__name` prefix, precision truncates); `%p`
(numbers/bool/nil → `"(null)"`, strings → content-stable pseudo-address (satisfies the
interning-observability asserts), tables/functions/etc. → Go `%p`). Error texts:
`"invalid conversion '%s' to 'format'"`, `"invalid conversion '%s' to 'format'"` for
width/precision overflow (`"invalid conversion ... too long"`? — pinned from the oracle at
gating), `"cannot have modifiers"` (`%10q`), `"invalid option '%s' to 'format'"`,
`"width and precision are too long"`, `"value has no literal form"`? — exact texts
probed against clua55 during gating (the corpus discipline: probe first, then pin).

### U3g — string.pack family

New file (`packlib.go` or inside stringlib.go): port `getoption`/`getdetails`/
`packint`/`unpackint`/`copywithendian`/`str_pack`/`str_packsize`/`str_unpack` with the
wasm32 size profile from §1 (sizes table constants; `T`/`s` = 4 — the oracle's model, not
Go's). Options `b B h H i I j J l L T n f d s z c x X ! = < >`; alignment rules (default 1
per option except integers/floats, `!n` maxalign cap, `X` aligns to the *next* option's
alignment, Kchar never aligns); sign-extension for oversized ints (pack and unpack);
endian handling incl. mixed-endian formats; string forms `s` (size-prefixed, overflow
checks), `z` (`"unfinished string for format 'z'"`? — oracle-pinned), `c` (padding with
`\0`, `"string longer than given size"`); unpack init positions (negative = from end,
alignment rounding, `"out of string"`); error texts from tpack.lua's checkerror set
(`"out of limits [1,16]"`, `"16-byte integer"`, `"integer overflow"`, `"not power of 2"`,
`"missing size"`, `"variable-length format"`, `"too large"`, `"too short"`, `"does not
fit"`, `"contains zeros"`, `"invalid next option"`, `"invalid format option"`). The final
unpack return is the next read position.

### U3h — io/base/coroutine smalls

io: `fileReadAux` rewritten to the 5.3+ shape — each argument is one option; strings
accept an optional leading `*`; `n` reads a numeral via the stock scanner returning int or
float subtype; `l`/`L`/`a` as today; integer option reads N bytes; default option `"l"`;
error text `"invalid format"`? — probed from the oracle. `io.lines`/`file:lines` adopt the
5.4 shape: multiple format arguments, iterator + the generic-for closure values returned
(4 results), EOF-close behavior, error returns nil+msg. base: `print` stays hardwired
(already `ToStringMeta`) but `ToStringMeta` gains the stock checks — `__tostring` must
return a string (`"'__tostring' must return a string to 'print'"` — the "to 'print'"
suffix is added by print's `luaL_tolstring`-with-msgid call; probe the exact oracle text)
and `__name`-prefixed default rendering (`"name: 0x…"`). `warn` + `@on`/`@off` control
messages writing to stderr (uncaptured by the engines — unobservable in diffs, matching
the oracle). coroutine: `close` (dead/suspended → reset + true, or false+error object if
the coroutine died with an error; normal → `"cannot close a normal coroutine"`;
running-main → `"cannot close main thread"`; 5.5 no-arg form closes the running
coroutine's bookkeeping only — errors since it's the running one unless main), needs the
thread model to record its terminal error object; `isyieldable` (+ 5.5 no-arg = current
thread semantics).

### U3i — corpora + gates + closeout

- `_u3/` corpus (per-chunk slices, the `_u1`/`_u2` discipline: no integral-float prints,
  no uncaught position-bearing errors, no engine-specific iteration order).
- `testdiff/u3_test.go`: `TestU3Gate` (interp↔clua55 byte-exact over `_u3`) and
  `TestU3OfficialSuite` — the five official files, interp (with SuiteFlags globals set
  pre-run) ↔ clua55 (SuiteFlags), diffed with a ruled skip map (varinfo hints row 62,
  parse-error wording U5, anything found at gating).
- `gen_matrix_corpus_test.go`: a stdlib grid (math/table/utf8/pack/format basics that are
  engine-portable) → regenerated `_wasm-tests` runs interp↔wasm↔wazero↔clua55.
- Skip-map updates: `mod00` un-skip (row 61 rng clause closes), `_glua-tests`
  math.lua/strings.lua/table.lua skips re-triaged (row 25 / row 61 clauses — string.dump
  stays), `_lua5.1-tests` strings.lua interp leg re-triaged if the format re-pin breaks
  its 5.1 asserts.
- Ledger rows: U3 closeout row (the chunk set + rulings: global/attribute no-op acceptance,
  named-vararg parse approximation, D5 rng shim retirement + makeseed pin alignment,
  frexp/ldexp keep, `%p` property-parity note, pack size profile = wasm32 model).
- Docs: parent plan status block + milestone history; this file's status log; CLAUDE.md
  if commands change; memory update.

## 4. Deviations from the milestone table (documented rulings)

1. **`global`/attribute parse acceptance moves into U3** (the U2 plan had it at U2b as
   parse-and-ignore; U2f took the port-into-`_u2` path instead). The five official gate
   files all use `global`/`<const>` — they cannot run unmodified without it. Enforcement
   (reserved word, const read-only, implicit `global *` voiding) stays at U5.
2. **`table.create` lands at U3, not U5** — it is registered in stock 5.5.1 and is ~10
   lines; the parent plan's U5 placement was conservative. (If gating shows no oracle
   delta, the U5 row just loses the item.)
3. **math.frexp/ldexp stay** despite Appendix B's removal list — the oracle registers and
   tests them (D6).
4. **Float printing stays Go-dialect (D7 at U5)** — the gate files' float asserts are
   dialect-tolerant by construction (§1); `string.format`'s internal rendering is C-parity
   regardless.
5. **The D5 host-RNG shim is retired at U3b** (both interp engine and rt flavor): with the
   interp speaking stock xoshiro from the makeseed=0 pin, the shim is the only
   non-xoshiro stream left; retiring it turns math.random into an all-engine
   byte-exact surface and closes the mod00 skip.
6. **string.format is unlisted in the milestone row but required by its gate**
   (strings.lua's format block); same for the `__name`/tolstring checks — Appendix B's
   "format %a/%p audit" is read as the full port.

## 5. U3 exit gate (consolidated)

- `go test ./...` green; `TestU1Gate`/`TestU2Gate` byte-exact (behavior preservation);
  `TestU3Gate` green.
- `TestU3OfficialSuite`: strings.lua, tpack.lua, utf8.lua, math.lua, bwcoercion.lua green
  interp↔clua55 (ruled skips only).
- `TestWasmFullMatrix` + wazero twin green on the regenerated matrix (incl. the stdlib
  grid) × {interp, clua55, wasm, wazero}; error suites green; M5/M6 gates green (blob
  rebuild verified: RT-NATIVE55 ×3, byte-determinism cmp, `go clean -testcache`).
- Ledger: zero un-rowed divergences; every new skip carries its row.
- Docs updated (this file's status log, parent plan status block, milestone history).

## 6. Status log

- **2026-09-28** — plan written; ground truth gathered (two exploration reports: the
  official-gate-file inventory; the C reference extract for lstrlib metatables/pack,
  lmathlib, ltablib, lcorolib, lbaselib, lutf8lib). U3a started.
