// Package luafuzz is the M6e differential fuzzer: a deterministic Lua
// program generator (design doc §8.4 L5) plus an interruptible,
// chunk-resumable soak runner (m6 plan §4.5/D6).
//
// Generator contract: Generate(masterSeed, idx) is a pure function —
// the same (seed, idx) always produces byte-identical source. A soak
// therefore never needs to store generated programs; resuming is
// "continue at the next index".
//
// Programs are valid Lua 5.5 by construction (one statement per line,
// block keywords balanced) and are additionally gated through the
// repo's own parser before reaching any engine; a parse failure is a
// GENBUG finding (generator bug), never an engine result.
//
// Ledger-driven avoidance list — corners the generator deliberately
// does NOT emit, each pinned to its divergence-ledger row. Extending
// the surface means extending this list's counterpart rows first:
//
//	row 20: recursion depth ≤ 50 (the wasm adapter guard fires at 150
//	        frames vs the interp harness's 1024 — depth itself diverges)
//	row 22: no coroutines
//	row 23: no debug library
//	row 24: no load/loadstring/dofile/loadfile/require
//	row 25: library calls are valid-by-construction only (%d values
//	        < 2^31, tonumber over plain-integer strings only, no
//	        zero-arg math.max, no string.dump, select index ≥ 1)
//	row 33: no io/os surface (prod sandbox nils it; dev shim covers it)
//	row 38: math.huge is never emitted (finite MaxFloat64 in the fork,
//	        true infinity in C — a VALUE divergence, still ruled). The
//	        row's number→string FORMATTING family is FIXED (row 49,
//	        2026-09-19): the runtime now renders numbers exactly like
//	        LNumber.String() in the gopher dialect (runtime/gnumfmt.c),
//	        so tostring/concat/table.concat over ANY finite double
//	        agree — the safeNumStrPool below stays as defense in depth.
//	row 39: pairs only over dense sequences (hash-part iteration order
//	        differs: interp sorted vs C ltable slot order)
//	row 40: binary-arith operands never scalar-local/t[k]/call reads —
//	        the "coerced-type" error-text corner is an open dialect gap
//	§8.2:   # only over dense sequences (border semantics unspecified
//	        with holes/trailing nils)
//
// U7b (2026-10-10) — the 5.5 construct surface is IN (integer duality,
// bitwise, <close>/<const>, global decls, named varargs); every engine
// in the soak set speaks 5.5 natively now (interp + lua55_rt/prod), so
// the new corners are stock semantics on all legs. Additional pins:
//
//	- %d format args stay < 2^31 (row 25): the 64-bit edges live in
//	  intLitPool, which never feeds string.format — print renders
//	  int64 exactly on every engine (row 49's lesson: integers are the
//	  SAFE subtype; floats are the hazard safeNumStrPool guards).
//	- math.huge stays out (row 38 value divergence) and so does any
//	  float ÷-0-literal shape: computed ±inf from runtime operands
//	  renders engine-identically (row 49 + numRepr), but a literal
//	  `x / 0` mid-case is needlessly close to the row-38 family —
//	  div/mod-by-zero coverage is the INTEGER raise corner via
//	  riskyExpr (`1 // 0`, `1 % 0` — "attempt to perform 'n//0'",
//	  byte-identical on the 5.5 set).
//	- `//` and `%` in num()-shaped expressions take a nonzero literal
//	  RHS only (the old 5.1-rt modulo ruling, kept as budget/raise
//	  discipline now that the rt is 5.5).
//	- the named-vararg table is READ-only in generation (t.n, t[k],
//	  t["n"]): writes are legal (table model, var02) but the t.n raise
//	  family ("vararg table has no proper 'n'") is a dedicated corner,
//	  not random surface; #t stays out (§8.2 border semantics over the
//	  synthesized table).
//	- global <close> is a semantic error (U5a) and never emitted;
//	  writes to <const> names (locals or globals) are compile errors —
//	  const decls are read-only pools.
//	- no uncaught error()/error(nil) (row 60 seam residual, the
//	  e006/e007 family): nil error objects render "<no error object>"
//	  + the rt prefix on wasm vs the interp's form. Found live by the
//	  U7b shakeout (seed 424242, 6 of the first 120 cases) — the
//	  2026-09-19 soak died before U5e made varinfo/error texts compare
//	  raw, so the residual had never been fuzz-exercised.
//	- no kind-"" calls of non-function values (`(5)()`, `("s")()`):
//	  row 85(c)'s residual — the interp's register VALUE scan at the
//	  raise names any local holding the callee ("(local 'v0')"),
//	  coincidence-dependent and unprecomputable on wasm (first live
//	  instance: seed 424242 case 54). Method-call shapes
//	  (t.missing_method_q()) keep their precomputed field-name hints.
//
// Rows 41/42 (condition-chain mislowering; -0.0 sign loss) are FIXED —
// their shapes (const and/or conditions) are back in the fuzz surface
// and pinned by _wasm-tests whlc00-03, nz00-01. Since row 49 fixed
// number→string rendering, the -0.0 LITERAL is back in numPool
// (2026-09-19): tostring/print of a negative zero is "0" on both
// engines (pinned by _wasm-tests/gnf01.lua + the native gnumfmt
// goldens).
//
// row 48 (2026-09-18 findings): no "^" — Go math.Pow (interp) vs musl's
// correctly-rounded pow disagree by 1 ulp on ~a third of operand pairs,
// even 0.1^2.
//
// The M6d deadline is armed on every run as the safety net: guarded
// loops should never need it, a both-engine expiry is DL-TIE (neutral),
// a one-engine expiry is a HANG finding.
package luafuzz

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"

	"github.com/pschlump/glasm/parse"
)

// caseRNG derives a per-case deterministic RNG stream from the soak's
// master seed and the case index (splitmix64-style mixing). math/rand
// streams are stable for a given Go toolchain; replaying a seed on a
// different major Go version may shift cases (documented, acceptable —
// findings persist their .lua regardless).
func caseRNG(master int64, idx int) *rand.Rand {
	z := uint64(master) + uint64(idx)*0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return rand.New(rand.NewSource(int64(z ^ (z >> 31))))
}

// CaseName is the chunkname for a case (it appears inside error
// position prefixes, so it is part of the differential contract).
func CaseName(idx int) string { return fmt.Sprintf("c%07d.lua", idx) }

// Generate produces the source of case idx under masterSeed.
func Generate(masterSeed int64, idx int) []byte {
	g := &gen{
		rnd:     caseRNG(masterSeed, idx),
		seed:    masterSeed,
		idx:     idx,
		budget:  18 + caseRNG(masterSeed, idx^0x51).Intn(20), // 18–37 statements
		maxLine: 120,
	}
	g.linef("-- luafuzz case=%d seed=%d (valid Lua 5.5 by construction)", idx, masterSeed)
	g.linef("-- repro: luafuzz -replay -seed %d -from %d -to %d", masterSeed, idx, idx)
	g.defs()
	for g.budget > 0 {
		g.stmt(true)
	}
	// Occasionally end with a bare uncaught core error: drives the
	// ERROR event + uncaught-unwind path with a full preceding log.
	if g.rnd.Intn(100) < 12 {
		g.bareRisk()
	}
	return g.render()
}

type tblInfo struct {
	name  string
	dense bool // dense array part: constructor 1..n or insert-built (#/ipairs safe)
	hasMT bool
	// pairsable: never gains a hash key (sequence constructor, only
	// table.insert appends since). pairs order over an array part with
	// ≤1 hash key is deterministic across engines; ≥2 hash keys is the
	// row-39 iteration-order divergence — any t[k]= / t.ident= write
	// that can add a hash key clears the flag.
	pairsable bool
}

type fnInfo struct {
	name   string
	arity  int
	vararg bool
	multi  bool // returns 2+ values (or passthrough) → multret callsites
}

type gen struct {
	rnd     *rand.Rand
	seed    int64
	idx     int
	out     []string
	ind     string
	budget  int
	maxLine int

	nScalar int
	nTable  int
	nFunc   int
	nGuard  int
	nMT     int
	nFor    int

	scalars []string
	tables  []tblInfo
	funcs   []fnInfo

	// loopDepth: active loop nesting. Bounds scale with depth — the
	// product across a nest must stay far under the case deadline
	// (interp finishes ~5M iterations inside 1.5 s; the wasm side pays
	// the rt_* ABI per operation and does not — the first soak filed
	// that as a HANG before this budget existed).
	loopDepth int

	// funcDepth: nesting inside a function BODY. A function's loops are
	// sized one level deeper than their lexical depth — the body can't
	// see the loops its CALL SITES sit in (overnight HANG c0238352: a
	// 236-trip downward for at a function's top level ran inside a
	// 31×13×4 caller nest = 765k events, deadlining one engine).
	funcDepth int

	// iterTables: tables under an active pairs/ipairs loop — assigning
	// NEW keys during traversal is unspecified (Lua 5.1 manual: only
	// modifying existing fields is allowed) and the engines legitimately
	// differ (interp visited the added key, C's rehashing next did not —
	// found by the fuzzer, minimized from soak case 59). Key-assignment
	// to these tables is suppressed while the loop is open.
	iterTables []string

	// U7b: 5.5-surface registries.
	nTBC     int    // closeable decl counter (names cx%d)
	tbcOpen  int    // live <close> decls at the current point (≤3, row 44)
	nGlobal  int    // global decl counter (gg%d / gk%d)
	nVarName int    // named-vararg table counter (nv%d)
	nConst   int    // <const> local counter (names kc%d)
	consts   []string // <const> local names — read-only pool (a write is a compile error)
}

// effLoopDepth: the depth loop bounds are selected for — one deeper
// inside function bodies (see funcDepth).
func (g *gen) effLoopDepth() int {
	if g.funcDepth > 0 {
		return g.loopDepth + 1
	}
	return g.loopDepth
}

func (g *gen) isIterated(name string) bool {
	for _, n := range g.iterTables {
		if n == name {
			return true
		}
	}
	return false
}

// clearPairsable: a key write that can add a hash key bans the table
// from later pairs iteration (row 39).
func (g *gen) clearPairsable(name string) {
	for i := range g.tables {
		if g.tables[i].name == name {
			g.tables[i].pairsable = false
		}
	}
}

// pickNonIteratedTable: a table not under an active pairs/ipairs loop
// (key-assignment to the iterated table itself is the unspecified
// mutation corner). Nil when every in-scope table is being iterated.
func (g *gen) pickNonIteratedTable() *tblInfo {
	if len(g.tables) == 0 {
		return nil
	}
	for try := 0; try < 4; try++ {
		t := g.pickT()
		if !g.isIterated(t.name) {
			return &t
		}
	}
	for i := len(g.tables) - 1; i >= 0; i-- {
		if !g.isIterated(g.tables[i].name) {
			t := g.tables[i]
			return &t
		}
	}
	return nil
}

// ---- emit helpers ----

func (g *gen) linef(format string, args ...any) {
	g.out = append(g.out, g.ind+fmt.Sprintf(format, args...))
}

func (g *gen) render() []byte {
	return []byte(strings.Join(g.out, "\n") + "\n")
}

func (g *gen) push() { g.ind += "  " }
func (g *gen) pop()  { g.ind = g.ind[:len(g.ind)-2] }

// ---- lexical scoping ----
//
// The name registries must mirror Lua's blocks: a local declared
// inside a block is invisible after its `end`. Referencing an
// out-of-scope name compiles fine but reads a nil GLOBAL — which is
// how the first wasm-mini leg found `pairs(nil)` and
// math.max(nil)-shaped library arg errors (row 25 wording territory).
// Blocks snapshot the registries and truncate on exit; names are
// globally unique counters, so truncation is exact restoration.

type scopeMark struct{ ns, nt, nf, nc, tbc int }

func (g *gen) mark() scopeMark {
	return scopeMark{len(g.scalars), len(g.tables), len(g.funcs), len(g.consts), g.tbcOpen}
}

func (g *gen) restore(m scopeMark) {
	g.scalars = g.scalars[:m.ns]
	g.tables = g.tables[:m.nt]
	g.funcs = g.funcs[:m.nf]
	g.consts = g.consts[:m.nc]
	g.tbcOpen = m.tbc
}

// ---- pools (slices only — iteration order feeds output; never maps) ----

var numPool = []string{
	"0", "1", "2", "3", "5", "7", "10", "42", "100", "255", "256", "1000",
	"65536", "0.5", "-1", "-2", "-2.5", "1e3", "1e15", "1e-9",
	"0.1", "3.5", "1e308", "9007199254740993", "-0.0",
	// "-0.0" restored (2026-09-19): row 42 fixed its arithmetic, row 49
	// fixed its rendering (tostring/print of -0.0 is "0" on both
	// engines — gnumfmt.c). Corpus pins: nz00/01 (row 42), gnf01
	// (row 49).
	// U7b: hex int literals + integral-float literals (subtype corners;
	// num() consumers are subtype-agnostic — 0xff + 1 is integer 256,
	// 3.0 + 1 is float 4.0, both render engine-exactly).
	"0x10", "0xff", "3.0", "7.0", "9007199254740992",
}

// intLitPool (U7b): integer-subtype literals for int-valued expression
// positions — the 64-bit edges (maxint/minint via the math.* constants:
// the minint DECIMAL/HEX literal form is float-shaped in 5.5, 2^63 does
// not fit lua_Integer), the 2^53/2^53+1 float-representability edge, hex
// ints. NEVER feeds string.format %d (row 25 keeps %d args < 2^31 —
// that stays intPool); print/tostring render int64 exactly everywhere
// (row 49: integers are the exactly-representable subtype).
var intLitPool = []string{
	"0x0", "0x1", "0x10", "0xff", "0Xf0", "0x7fff",
	"7", "-7", "100", "2147483647", "-2147483648", "4294967296",
	"4611686018427387904", "9007199254740992", "9007199254740993",
	"9223372036854775807", "-9223372036854775807",
	"math.maxinteger", "math.mininteger",
}

// integralFloatPool (U7b): floats whose value is an in-range integer —
// bitwise/`//` operands coerce through tointegerns ("3.0 & 1" is 1;
// "2.5 & 1" would raise, which is riskyExpr's job). 1e15 < 2^53 fits.
var integralFloatPool = []string{"1.0", "2.0", "3.0", "7.0", "-1.0", "1e15", "255.0"}

// intCoerceStrPool (U7b): strings that convert to in-range integers —
// the 5.2+ string-coercion corner ("0x10" + 1 = 17 integer; bitwise on
// coercible strings is bwcoercion.lua's corner).
var intCoerceStrPool = []string{`"10"`, `"0x10"`, `"-3"`, `"3.0"`, `"0x1f"`}

// shiftPool (U7b): shift displacements — negative shifts go the other
// way and ≥64 (well, ≥64 for int64) shifts yield 0 (5.5 manual);
// 1<<63 is the sign bit (defined, not UB); >> on negatives is
// arithmetic (sign-filling). All pinned by the U2f grids.
var shiftPool = []string{"0", "1", "3", "7", "31", "62", "63", "64", "100", "-1", "-2", "-63"}

// floorArgPool (U7b): math.floor/ceil arguments — in 5.3+ they return
// the INTEGER subtype, so they are intOperand suppliers; the args must
// floor/ceil into int64 range (1e308's floor does not — "number has no
// integer representation" is a raise, kept out of operand position).
var floorArgPool = []string{"-3.5", "2.9", "0.5", "7.0", "255.9", "1e15", "-0.0"}

// intPool: %d-format-safe integers (< 2^31, row 25) and loop bounds.
var intPool = []string{"0", "1", "2", "3", "4", "5", "7", "10", "12", "20", "42", "99", "255"}

var numStrPool = []string{`"10"`, `"2"`, `"100"`, `"-3"`, `"3.5"`, `"0x10"`} // arith-coercible strings

var strPool = []string{`"abc"`, `"x"`, `""`, `"hello"`, `"A"`, `"hello world"`, `"10"`, `"lua"`}

var identPool = []string{"zz", "k1", "name", "Q"} // table string keys

// posIntPool: math.random bounds (Intn(≤0) panics Go's rand).
var posIntPool = []string{"1", "2", "3", "6", "10", "42"}

// safeNumStrPool: numeric literals for `..`/tostring/table.concat
// LITERAL operand position. Since row 49 (2026-09-19) made the
// runtime's number→string conversion fork-exact for every finite
// double, this pool is defense in depth rather than a divergence
// boundary — it stays narrow so a formatting regression surfaces as a
// small diff (and computed values — the row-49 finding path — reach
// these positions regardless of the pool).
var safeNumStrPool = []string{
	"0", "1", "2", "3", "5", "7", "10", "42", "100", "255", "256", "1000",
	"65536", "0.5", "-1", "-2", "-2.5", "0.1", "3.5", "1e-9",
}

// lit: a non-nil literal. Sequence-critical table values (array parts,
// table.insert args) must be non-nil BY CONSTRUCTION — a nil would
// open a hole and #/pairs/ipairs would cross into unspecified border
// semantics (§8.2) or hash-order divergence (row 39). Numbers come
// from the coercion-safe pool: table values reach tostring() via
// str()'s tostring(t[k]) case.
func (g *gen) lit() string {
	switch g.rnd.Intn(3) {
	case 0:
		return g.pick(safeNumStrPool)
	case 1:
		return g.pick(strPool)
	default:
		return g.pick([]string{"true", "false"})
	}
}

func (g *gen) pick(pool []string) string { return pool[g.rnd.Intn(len(pool))] }

// fmtSpec pairs a format string with matching argument count (row 25:
// a missing format arg is undefined behavior in C).
type fmtSpec struct {
	f string
	n int
}

var fmtSpecs = []fmtSpec{{"%s", 1}, {"%s-%s", 2}, {"[%s]", 1}, {"%q", 1}, {"%5s", 1}}

// ---- names ----

func (g *gen) newScalar() string { g.nScalar++; return fmt.Sprintf("v%d", g.nScalar-1) }
func (g *gen) newTableN() string { g.nTable++; return fmt.Sprintf("t%d", g.nTable-1) }
func (g *gen) newFunc() string   { g.nFunc++; return fmt.Sprintf("f%d", g.nFunc-1) }
func (g *gen) newGuard() string  { g.nGuard++; return fmt.Sprintf("gd%d", g.nGuard-1) }
func (g *gen) newMT() string     { g.nMT++; return fmt.Sprintf("mt%d", g.nMT-1) }
func (g *gen) newFor() string    { g.nFor++; return fmt.Sprintf("i%d", g.nFor-1) }

// ---- expressions ----

// num: a number-valued (or arithmetic-coercible) expression.
func (g *gen) num(d int) string {
	if d <= 0 {
		switch r := g.rnd.Intn(10); {
		case r < 7:
			return g.pick(numPool)
		case r < 9:
			return g.pick(numStrPool)
		default:
			if len(g.scalars) > 0 {
				return g.pick(g.scalars)
			}
			return g.pick(numPool)
		}
	}
	// U7b: the integer-subtype surface — int64-wrap arith, //, bitwise,
	// shifts (math.type/print render ints exactly; row 49). One pick in
	// five routes here so float-shaped arithmetic keeps its mass.
	if g.rnd.Intn(5) == 0 {
		return g.intExpr(d)
	}
	switch r := g.rnd.Intn(20); {
	case r < 6:
		// no "^": Go's math.Pow (interp) and musl's correctly-rounded pow
		// (runtime) disagree by 1 ulp on ~a third of operand pairs — even
		// 0.1^2 (repeated squaring vs correct rounding). Ledgered libm
		// ruling (row 48); the durable fix is porting Go's
		// exp/log/pow into the runtime for byte-parity.
		// "//" and "%": nonzero literal RHS only — a zero RHS on an
		// integer operand raises (the riskyExpr corner) and on a float
		// operand goes ±inf (row-38 adjacent); a nonzero literal keeps
		// num()-shaped expressions evaluation-safe by construction.
		if r := g.rnd.Intn(5); r == 0 {
			return fmt.Sprintf("(%s %% %s)", g.arithOperand(), g.pick([]string{"1", "2", "3", "7", "0.5", "1.5"}))
		} else if r == 1 {
			return fmt.Sprintf("(%s // %s)", g.arithOperand(), g.pick([]string{"1", "2", "3", "7", "0.5", "1.5"}))
		}
		op := g.pick([]string{"+", "-", "*", "/"})
		return fmt.Sprintf("(%s %s %s)", g.arithOperand(), op, g.arithOperand())
	case r < 8:
		// operand parenthesized: a pool literal like "-2" after the
		// unary minus would render "(--2)" — a comment, not an expr
		return fmt.Sprintf("(-(%s))", g.num(d-1))
	case r < 10:
		if len(g.tables) > 0 {
			t := g.pickT()
			return fmt.Sprintf("%s[%s]", t.name, g.pick(intPool))
		}
		return g.pick(numPool)
	case r < 12:
		fn := g.pickF()
		if fn == nil {
			return g.pick(numPool)
		}
		return g.call(fn)
	case r < 14:
		return fmt.Sprintf("math.%s(%s, %s)", g.pick([]string{"max", "min"}),
			g.pick(numPool), g.pick(numPool))
	case r < 15:
		return fmt.Sprintf("math.%s(%s)", g.pick([]string{"floor", "ceil", "abs"}), g.pick(numPool))
	case r < 16:
		return fmt.Sprintf("math.fmod(%s, %s)", g.pick(numPool), g.pick([]string{"1", "2", "3", "7"}))
	case r < 17:
		return fmt.Sprintf("math.random(%s)", g.pick(posIntPool))
	case r < 18:
		return fmt.Sprintf("tonumber(%s)", g.pick([]string{`"10"`, `"-3"`, `"abc"`, `""`, `"0x10"`}))
	default:
		return g.pick(numPool)
	}
}

// arithOperand: binary-arith operands are restricted to numeric
// literals, coercible-string literals, and always-numeric lib calls —
// never plain scalar locals / t[k] reads / arbitrary calls, which can
// hold non-coercible strings. The error-text corner "non-coercible
// string LHS × coercible string RHS" (interp names the RHS after its
// coercion, C names the raw type — row 40) is unreachable this way;
// arith error paths keep coverage through riskyExpr's deterministic
// raises ("x" + 1, {} + 1, …).
func (g *gen) arithOperand() string {
	switch g.rnd.Intn(10) {
	case 0:
		return g.pick(numStrPool)
	case 1:
		return fmt.Sprintf("math.%s(%s, %s)", g.pick([]string{"max", "min"}),
			g.pick(numPool), g.pick(numPool))
	case 2:
		return fmt.Sprintf("math.%s(%s)", g.pick([]string{"floor", "ceil", "abs"}), g.pick(numPool))
	case 3:
		return fmt.Sprintf("math.random(%s)", g.pick(posIntPool))
	default:
		return g.pick(numPool)
	}
}

// intOperand: an operand guaranteed to hold (or coerce to) an in-range
// integer — literals, the math.*integer constants, integral-float and
// coercible-string literals, and the always-integer 5.3+ lib calls
// (floor/ceil/fmod-int/max/min over int args). The row-40 discipline
// (no scalar locals / t[k] / arbitrary calls) applies: an operand that
// could hold a non-integral float or a table would turn the wrap/shift
// corners into raise corners mid-case.
func (g *gen) intOperand() string {
	switch r := g.rnd.Intn(14); {
	case r < 2: // integral floats coerce (tointegerns)
		return g.pick(integralFloatPool)
	case r < 3: // coercible strings (bwcoercion.lua corner)
		return g.pick(intCoerceStrPool)
	case r < 5:
		return fmt.Sprintf("math.%s(%s)", g.pick([]string{"floor", "ceil"}), g.pick(floorArgPool))
	case r < 6:
		return fmt.Sprintf("math.fmod(%s, %s)", g.pick(intLitPool), g.pick([]string{"2", "3", "7"}))
	case r < 7:
		return fmt.Sprintf("math.%s(%s, %s)", g.pick([]string{"max", "min"}),
			g.pick(intLitPool), g.pick(intLitPool))
	case r < 8:
		return fmt.Sprintf("math.abs(%s)", g.pick([]string{"-7", "-100", "math.mininteger"}))
	default:
		return g.pick(intLitPool)
	}
}

// intExpr (U7b): an integer-valued expression — int64 arith (overflow
// WRAPS in 5.5, defined and engine-pinned by the U2f grids: maxint+1 is
// minint), floor division/modulo with nonzero literal RHS, bitwise
// and/or/xor, shifts (negative = other direction, ≥64 → 0, <<63 = sign
// bit, >> is arithmetic on negatives), unary bnot (~x == -x-1).
func (g *gen) intExpr(d int) string {
	if d <= 0 {
		return g.intOperand()
	}
	switch r := g.rnd.Intn(16); {
	case r < 4:
		op := g.pick([]string{"+", "-", "*"})
		return fmt.Sprintf("(%s %s %s)", g.intOperand(), op, g.intOperand())
	case r < 6:
		return fmt.Sprintf("(%s // %s)", g.intOperand(), g.pick([]string{"1", "2", "3", "7"}))
	case r < 8:
		return fmt.Sprintf("(%s %% %s)", g.intOperand(), g.pick([]string{"1", "2", "3", "7"}))
	case r < 11:
		op := g.pick([]string{"&", "|", "~"})
		return fmt.Sprintf("(%s %s %s)", g.intOperand(), op, g.intOperand())
	case r < 13:
		op := g.pick([]string{"<<", ">>"})
		return fmt.Sprintf("(%s %s %s)", g.intOperand(), op, g.pick(shiftPool))
	default:
		return fmt.Sprintf("(~%s)", g.intOperand())
	}
}

// str: a string-valued expression.
func (g *gen) str(d int) string {
	if d <= 0 || g.rnd.Intn(3) == 0 {
		return g.pick(strPool)
	}
	switch r := g.rnd.Intn(12); {
	case r < 3:
		return fmt.Sprintf("(%s .. %s)", g.concatOperand(), g.concatOperand())
	case r < 4:
		sp := g.pickFmt()
		args := make([]string, sp.n)
		for i := range args {
			args[i] = g.pick(strPool)
		}
		return fmt.Sprintf("string.format(%q, %s)", sp.f, strings.Join(args, ", "))
	case r < 5:
		return fmt.Sprintf("string.sub(%s, %s)", g.pick(strPool), g.pick(intPool))
	case r < 6:
		return fmt.Sprintf("string.rep(%s, %s)", g.pick(strPool), g.pick([]string{"1", "2", "3"}))
	case r < 7:
		return fmt.Sprintf("string.upper(%s)", g.pick(strPool))
	case r < 8:
		return fmt.Sprintf("string.reverse(%s)", g.pick(strPool))
	case r < 9:
		if len(g.tables) > 0 {
			t := g.pickT()
			return fmt.Sprintf("tostring(%s[%s])", t.name, g.pick(intPool))
		}
		return g.pick(strPool)
	case r < 10:
		return fmt.Sprintf("tostring(%s)", g.pick(safeNumStrPool))
	case r < 11:
		// U7b: subtype interrogation — "integer"/"float"/nil render
		// engine-identically (numPool mixes both subtypes now).
		return fmt.Sprintf("math.type(%s)", g.pick(numPool))
	case r < 12:
		return fmt.Sprintf("string.char(%s, %s)", g.pick(smallInts), g.pick(smallInts))
	default:
		return g.pick(strPool)
	}
}

var smallInts = []string{"65", "66", "97", "98", "48", "10"}

func (g *gen) pickFmt() fmtSpec { return fmtSpecs[g.rnd.Intn(len(fmtSpecs))] }

// concatOperand: only literals participate in `..`, numbers from the
// conservative safeNumStrPool (see its comment — defense in depth
// since row 49, not a divergence boundary).
func (g *gen) concatOperand() string {
	if g.rnd.Intn(2) == 0 {
		return g.pick(strPool)
	}
	return g.pick(safeNumStrPool)
}

// boolExpr: condition-shaped.
func (g *gen) boolExpr(d int) string {
	if d <= 0 || g.rnd.Intn(6) == 0 {
		switch r := g.rnd.Intn(8); {
		case r < 2:
			return "true"
		case r < 3:
			return "false"
		default:
			return g.anyScalar(0)
		}
	}
	switch r := g.rnd.Intn(10); {
	case r < 3:
		op := g.pick([]string{"==", "~=", "<", "<=", ">", ">="})
		return fmt.Sprintf("(%s %s %s)", g.num(d-1), op, g.num(d-1))
	case r < 4:
		// U7b mixed int/float compare edge: the int64 converts to
		// double first (9007199254740993 == 9007199254740993.0 is
		// false — 2^53+1 rounds; 1 == 1.0 is true). IEEE-exact on
		// every engine.
		op := g.pick([]string{"==", "~=", "<", "<=", ">", ">="})
		return fmt.Sprintf("(%s %s %s)", g.pick(intLitPool), op, g.pick([]string{"1.0", "2.0", "3.5", "0.5", "-1.0", "1e15", "9007199254740993.0"}))
	case r < 5:
		op := g.pick([]string{"==", "~=", "<", "<=", ">", ">="})
		return fmt.Sprintf("(%s %s %s)", g.str(d-1), op, g.str(d-1))
	case r < 6:
		return fmt.Sprintf("(not %s)", g.anyScalar(d-1))
	case r < 8:
		return fmt.Sprintf("(%s and %s)", g.anyScalar(d-1), g.anyScalar(d-1))
	default:
		return fmt.Sprintf("(%s or %s)", g.anyScalar(d-1), g.anyScalar(d-1))
	}
}

// anyScalar: number/string/bool/nil-ish scalar expression.
func (g *gen) anyScalar(d int) string {
	// U7b: <const> locals join the readable pool (writes never target
	// them — stmtAssign only writes g.scalars).
	if len(g.consts) > 0 && g.rnd.Intn(10) == 0 {
		return g.pick(g.consts)
	}
	switch r := g.rnd.Intn(10); {
	case r < 4:
		return g.num(d)
	case r < 7:
		return g.str(d)
	case r < 8:
		return g.pick([]string{"true", "false", "nil"})
	default:
		if len(g.scalars) > 0 {
			return g.pick(g.scalars)
		}
		return g.num(d)
	}
}

func (g *gen) pickT() tblInfo { return g.tables[g.rnd.Intn(len(g.tables))] }

func (g *gen) pickF() *fnInfo {
	if len(g.funcs) == 0 {
		return nil
	}
	f := &g.funcs[g.rnd.Intn(len(g.funcs))]
	return f
}

// call renders a well-formed call of fn (arity- and spread-aware).
// A vararg fn's arity counts the "..." pseudo-param: fixed args are
// arity-1, plus a spread beyond them. One call in four drops the
// spread — the empty-extras corner (t.n == 0, t[1] == nil; var01).
func (g *gen) call(fn *fnInfo) string {
	nfixed := fn.arity
	if fn.vararg {
		nfixed--
	}
	var args []string
	for i := 0; i < nfixed; i++ {
		args = append(args, g.anyScalar(1))
	}
	if fn.vararg && g.rnd.Intn(4) != 0 {
		args = append(args, "1", `"s"`) // extra spread beyond fixed arity
	}
	return fmt.Sprintf("%s(%s)", fn.name, strings.Join(args, ", "))
}

// multretCall: a call placed to expand (last arg of print, RHS of a
// multi-assign).
func (g *gen) multretCall(d int) string {
	fn := g.pickF()
	if fn == nil || !fn.multi {
		return g.num(d)
	}
	return g.call(fn)
}

// ---- statements ----

func (g *gen) spend(n int) {
	g.budget -= n
	if g.budget < 0 {
		g.budget = 0
	}
}

// stmt emits one statement; top=true at chunk body level (bare risky
// statements and global decls are chunk-level only — uncaught raises
// inside function bodies would truncate every callsite; bodies use
// pcall wraps; `global` stays top-level per the U7b plan).
func (g *gen) stmt(top bool) {
	if len(g.out) >= g.maxLine-12 {
		g.spend(g.budget)
		return
	}
	r := g.rnd.Intn(100)
	switch {
	case r < 11:
		g.stmtLocalDecl()
	case r < 22:
		g.stmtPrint()
	case r < 30:
		g.stmtAssign()
	case r < 38:
		g.stmtIf()
	case r < 46:
		g.stmtNumFor()
	case r < 53:
		g.stmtGenFor()
	case r < 58:
		g.stmtWhile()
	case r < 62:
		g.stmtRepeat()
	case r < 69:
		g.stmtPcall()
	case r < 75:
		g.stmtFuncDef()
	case r < 80:
		g.stmtTableDef()
	case r < 84:
		g.stmtMetatable()
	case r < 88:
		g.stmtLib()
	case r < 91:
		g.stmtTBC()
	case r < 93:
		if top {
			g.stmtGlobal()
		} else {
			g.stmtPrint()
		}
	default:
		if top {
			g.bareRisk()
		} else {
			g.stmtPrint()
		}
	}
}

func (g *gen) defs() {
	n := 2 + g.rnd.Intn(5)
	for i := 0; i < n && g.budget > 0; i++ {
		switch g.rnd.Intn(3) {
		case 0:
			g.stmtTableDef()
		case 1:
			g.stmtFuncDef()
		default:
			g.stmtLocalDecl()
		}
	}
}

func (g *gen) stmtLocalDecl() {
	g.spend(1)
	// U7b: <const> local (U4a attribs). Name goes to the read-only
	// consts pool — a write target must never pick it (compile error);
	// consts are block-scoped via scopeMark's nc truncation.
	if g.rnd.Intn(8) == 0 {
		g.nConst++
		name := fmt.Sprintf("kc%d", g.nConst-1)
		g.consts = append(g.consts, name)
		g.linef("local %s <const> = %s", name, g.anyScalar(1))
		return
	}
	// RHS evaluated against the OUTER scope, names registered after —
	// `local v0 = f(v0)` must read the outer v0, not itself
	if g.rnd.Intn(5) == 0 {
		name := g.newScalar()
		g.scalars = append(g.scalars, name)
		g.linef("local %s", name)
		return
	}
	if g.rnd.Intn(3) == 0 {
		n1, n2 := g.newScalar(), g.newScalar()
		r1, r2 := g.anyScalar(1), g.anyScalar(1)
		g.scalars = append(g.scalars, n1, n2)
		g.linef("local %s, %s = %s, %s", n1, n2, r1, r2)
		return
	}
	name := g.newScalar()
	rhs := g.anyScalar(2)
	g.scalars = append(g.scalars, name)
	g.linef("local %s = %s", name, rhs)
}

func (g *gen) stmtPrint() {
	g.spend(1)
	n := 1 + g.rnd.Intn(3)
	args := make([]string, 0, n)
	for i := 0; i < n-1; i++ {
		args = append(args, g.anyScalar(1))
	}
	// last arg: multret expansion one time in three
	if g.rnd.Intn(3) == 0 && len(g.funcs) > 0 {
		args = append(args, g.multretCall(1))
	} else {
		args = append(args, g.anyScalar(1))
	}
	g.linef("print(%s)", strings.Join(args, ", "))
}

func (g *gen) stmtAssign() {
	g.spend(1)
	switch r := g.rnd.Intn(10); {
	case r < 5:
		if len(g.scalars) == 0 {
			g.stmtLocalDecl()
			return
		}
		g.linef("%s = %s", g.pick(g.scalars), g.anyScalar(1))
	case r < 7:
		t := g.pickNonIteratedTable()
		if t == nil {
			g.stmtLocalDecl()
			return
		}
		g.clearPairsable(t.name)
		if g.rnd.Intn(2) == 0 {
			g.linef("%s[%s] = %s", t.name, g.pick(intPool), g.anyScalar(1))
		} else {
			g.linef("%s.%s = %s", t.name, g.pick(identPool), g.anyScalar(1))
		}
	default:
		g.linef("gx%d = %s", g.nScalar, g.anyScalar(1))
	}
}

// stmtTBC (U7b): to-be-closed variables. Bounded per row 44's
// semantics: at most 3 live TBCs per emission, close bodies are tiny
// marker prints, and the shapes never contain blockBody() — so TBC
// emissions cannot nest and max simultaneously-live stays ≤3. Covers:
// the reverse-order close of multiple TBCs, nil/false TBCs (no __close
// runs), the error-path close (pcall catch — the close ran first),
// __close itself raising at scope exit (single fault — the double
// fault's "error in error handling" is row-78e territory, not random
// surface), and the non-closable decl raise.
func (g *gen) stmtTBC() {
	g.spend(2)
	if g.tbcOpen >= 3 {
		g.stmtPrint() // row 44 bound — never more than 3 live
		return
	}
	n := 1 + g.rnd.Intn(3) // 1–3 TBCs in this block (reverse-close corner)
	switch g.rnd.Intn(6) {
	case 0: // nil/false TBC: legal, no __close call
		g.linef("do")
		g.push()
		g.tbcOpen++
		g.linef("local cx%d <close> = %s", g.nTBC, g.pick([]string{"nil", "false"}))
		g.linef("print(\"tbc-nil-ok\")")
		g.tbcOpen--
		g.pop()
		g.linef("end")
	case 1, 2: // closables; 2+ shows reverse-order closing
		g.linef("do")
		g.push()
		m := g.mark()
		for i := 0; i < n; i++ {
			g.tbcOpen++
			g.linef("local cx%d <close> = setmetatable({}, {__close = function() print(\"cl%d\") end})",
				g.nTBC, g.nTBC)
			g.nTBC++
		}
		g.linef("print(\"in-tbc-block\")")
		g.restore(m)
		g.pop()
		g.linef("end")
	case 3: // error-path close: the catch proves the close ran
		g.linef("do")
		g.push()
		m := g.mark()
		g.linef("local ok, e = pcall(function()")
		g.push()
		g.tbcOpen++
		g.linef("local cx%d <close> = setmetatable({}, {__close = function(o, err) print(\"clerr\", tostring(err)) end})",
			g.nTBC)
		g.nTBC++
		g.linef("return %s", g.riskyExpr())
		g.pop()
		g.tbcOpen--
		g.linef("end)")
		g.linef("print(ok, tostring(e))")
		g.restore(m)
		g.pop()
		g.linef("end")
	case 4: // __close raising at clean scope exit (single fault)
		g.linef("do")
		g.push()
		m := g.mark()
		g.linef("local ok, e = pcall(function()")
		g.push()
		g.tbcOpen++
		g.linef("local cx%d <close> = setmetatable({}, {__close = function() error(\"inclose\") end})",
			g.nTBC)
		g.nTBC++
		g.pop()
		g.tbcOpen--
		g.linef("end)")
		g.linef("print(ok, tostring(e))")
		g.restore(m)
		g.pop()
		g.linef("end")
	default: // non-closable value: "variable 'cxN' got a non-closable value"
		g.linef("do")
		g.push()
		m := g.mark()
		g.linef("local ok, e = pcall(function() local cx%d <close> = %s end)",
			g.nTBC, g.pick([]string{"42", `"s"`, "true"}))
		g.nTBC++
		g.linef("print(ok, tostring(e))")
		g.restore(m)
		g.pop()
		g.linef("end")
	}
}

// stmtGlobal (U7b): the 5.5 `global` declaration system (U5a,
// contextual keyword). Top-level only (see stmt). Two discipline rules
// from glob01's ruled semantics: a NAMED declaration voids the
// implicit preamble for the rest of the chunk — reads too, so even
// `print`/`math` would be not-declared compile errors — and `global *`
// re-opens it (decls persist). Every window therefore closes with
// `global *` before anything else is emitted, and a window's interior
// holds only literals (the <const> initializer sits inside it).
// `global <close>` is a semantic error and never emitted; writes to
// <const> names are compile errors — they never enter a writable pool.
// The _ENV leg exercises the local-shadow lowering (GETFIELD/SETFIELD
// on the local, vs the chunk-level GETTABUP/SETTABUP path) — names
// under a local _ENV are field reads, legal under any preamble state.
func (g *gen) stmtGlobal() {
	g.spend(1)
	plain := func() {
		n := g.nGlobal
		g.nGlobal++
		g.linef("global gg%d", n)
		g.linef("global *")
		g.linef("gg%d = %s", n, g.anyScalar(1))
		g.linef("print(gg%d)", n)
	}
	switch g.rnd.Intn(4) {
	case 0, 1: // plain global: declare, re-open, assign, read
		plain()
	case 2: // const global: literal initializer (inside the window),
		// reads only after the re-open
		n := g.nGlobal
		g.nGlobal++
		g.linef("global <const> gk%d = %s", n, g.lit())
		g.linef("global *")
		g.linef("print(gk%d, math.type(gk%d))", n, n)
	default: // read through a local _ENV shadow
		// No named-decl dependency (a fresh case's first window must be
		// able to take this arm — the stmtGlobal rate is low enough that
		// second windows are rare): with no decls yet, read a stock
		// global through the shadow; otherwise a declared gg/gk.
		if g.nGlobal > 0 {
			g.linef("do local _ENV = _G print(gg%d, gk%d) end", g.rnd.Intn(g.nGlobal), g.rnd.Intn(g.nGlobal))
		} else {
			g.linef("do local _ENV = _G print(math.type(7)) end")
		}
	}
}

func (g *gen) stmtIf() {
	g.spend(2)
	hasElseIf := g.rnd.Intn(3) == 0
	hasElse := g.rnd.Intn(3) != 0
	g.linef("if %s then", g.boolExpr(2))
	m := g.mark()
	g.push()
	g.blockBody(1 + g.rnd.Intn(2))
	if hasElseIf {
		g.pop()
		g.restore(m)
		g.linef("elseif %s then", g.boolExpr(1))
		m = g.mark()
		g.push()
		g.blockBody(1)
	}
	if hasElse {
		g.pop()
		g.restore(m)
		g.linef("else")
		m = g.mark()
		g.push()
		g.blockBody(1)
	}
	g.pop()
	g.restore(m)
	g.linef("end")
}

func (g *gen) blockBody(n int) {
	for i := 0; i < n && g.budget > 0; i++ {
		g.stmt(false)
	}
}

func (g *gen) stmtNumFor() {
	g.spend(2)
	if g.effLoopDepth() >= 3 {
		g.stmtPrint() // nest cap — the iteration product must stay bounded
		return
	}
	iv := g.newFor()
	a := g.pick(intPool)
	if g.effLoopDepth() == 0 {
		// the OUTERMOST numeric for is the product nothing else
		// shrinks — a 255-start downward for under a 13-trip while
		// with a depth-2 for is ~60k events, inside the wasm leg's
		// deadline noise (c0158534 family; trips stay ≤ 43 here).
		a = g.pick([]string{"3", "5", "7", "10", "12", "20", "42"})
	}
	k := g.pick([]string{"0", "3", "5", "10", "20"})
	if d := g.effLoopDepth(); d == 1 {
		k = g.pick([]string{"0", "2", "3"})
	} else if d == 2 {
		k = g.pick([]string{"0", "1", "2"})
	}
	if g.effLoopDepth() >= 1 {
		// DOWNWARD loops count a-k iterations (the overnight HANG at
		// c0003075: `for i2 = 255, 1, -1` at depth 2 — shrinking k
		// alone left 255 trips; ~1.16M events deadlined the wasm leg
		// while interp finished). Shrink the start bound too.
		a = g.pick([]string{"2", "3", "5", "7"})
	}
	if g.rnd.Intn(6) == 0 && g.effLoopDepth() == 0 { // float flavor
		g.linef("for %s = 0, 2.5, 0.5 do", iv)
	} else if g.rnd.Intn(2) == 0 {
		g.linef("for %s = %s, %s, -1 do", iv, a, k) // downward
	} else {
		g.linef("for %s = %s, %s do", iv, a, k)
	}
	m := g.mark()
	g.push()
	g.loopDepth++
	g.blockBody(1 + g.rnd.Intn(2))
	g.loopDepth--
	g.linef("print(%s, %s)", iv, iv)
	g.pop()
	g.restore(m)
	g.linef("end")
}

func (g *gen) stmtGenFor() {
	g.spend(2)
	if g.loopDepth >= 3 {
		g.stmtPrint() // nest cap
		return
	}
	if len(g.tables) == 0 {
		g.stmtTableDef()
	}
	t := g.pickT()
	if !t.dense || !t.pairsable {
		// row 39: build a fresh dense sequence instead
		name := g.newTableN()
		g.tables = append(g.tables, tblInfo{name: name, dense: true, pairsable: true})
		g.linef("local %s = {%s, %s, %s}", name, g.lit(), g.lit(), g.lit())
		t = tblInfo{name: name, dense: true, pairsable: true}
	}
	fn := "ipairs"
	if g.rnd.Intn(3) == 0 {
		fn = "pairs" // hash-free sequence → identical order across engines
	}
	g.linef("for kk%d, vv%d in %s(%s) do", g.nFor, g.nFor, fn, t.name)
	m := g.mark()
	g.push()
	g.iterTables = append(g.iterTables, t.name)
	g.loopDepth++
	g.linef("print(kk%d, vv%d)", g.nFor, g.nFor)
	g.blockBody(1)
	g.loopDepth--
	g.iterTables = g.iterTables[:len(g.iterTables)-1]
	g.pop()
	g.restore(m)
	g.linef("end")
	g.nFor++
}

func (g *gen) stmtWhile() {
	g.spend(2)
	if g.effLoopDepth() >= 3 {
		g.stmtPrint() // nest cap
		return
	}
	guard := g.newGuard()
	limit := g.pick([]string{"3", "5", "7", "12", "30"})
	if d := g.effLoopDepth(); d == 1 {
		// 30 stays a TOP-LEVEL-only luxury: a 31-trip while is the
		// biggest per-level multiplier left, and at depth 1+ it sits
		// under another loop's product (fn bodies size their loops
		// at eff 1 too — a 31-trip body while under a hot call site
		// is the c0158534 compound shape).
		limit = g.pick([]string{"3", "5", "7", "12"})
	} else if d >= 2 {
		limit = "3"
	}
	g.linef("local %s = 0", guard)
	g.linef("while %s do", g.boolExpr(1))
	m := g.mark()
	g.push()
	g.loopDepth++
	g.linef("%s = %s + 1", guard, guard)
	g.blockBody(1 + g.rnd.Intn(2))
	g.loopDepth--
	g.linef("if %s > %s then break end", guard, limit)
	g.pop()
	g.restore(m)
	g.linef("end")
	g.linef("print(%s)", guard)
}

func (g *gen) stmtRepeat() {
	g.spend(2)
	if g.effLoopDepth() >= 3 {
		g.stmtPrint() // nest cap
		return
	}
	guard := g.newGuard()
	limit := g.pick([]string{"2", "4", "6"})
	g.linef("local %s = 0", guard)
	g.linef("repeat")
	m := g.mark()
	g.push()
	// A repeat IS a loop for nesting: it must count toward
	// effLoopDepth like while/for do. Until 2026-09-19 it didn't —
	// loops nested under it were sized one level shallower (and the
	// depth-2/3 caps never triggered), so `repeat(7) × for(43) ×
	// while(31) × for(5)` = ~100k print events deadlined the wasm leg
	// while interp finished (HANG c0158534).
	g.loopDepth++
	g.linef("%s = %s + 1", guard, guard)
	g.blockBody(1)
	g.linef("if %s > %s then break end", guard, limit)
	g.loopDepth--
	g.pop()
	g.restore(m)
	g.linef("until %s or %s > %s", g.boolExpr(0), guard, limit)
	g.linef("print(%s)", guard)
}

// stmtPcall: pcall-wrapped core-error corner; prints ok + caught value.
// Error-raising pcalls are throttled by loop depth: the raise path is
// the expensive part (Go panic/recover vs the runtime's SJLJ staging
// ≈5-10× slower), and a pcall-of-error inside a 100k-iteration nest
// deadlines the wasm leg while interp finishes — a HANG finding that
// is throughput, not correctness (fuzzer find, first 10k soak).
func (g *gen) stmtPcall() {
	g.spend(1)
	risky := g.riskyExpr()
	if g.loopDepth >= 2 || (g.loopDepth == 1 && g.rnd.Intn(4) != 0) {
		risky = g.anyScalar(0) // pcall machinery still runs, no raise
	}
	g.linef("do")
	m := g.mark()
	g.push()
	g.linef("local ok, e1, e2 = pcall(function() return %s, %s end)", risky, g.anyScalar(0))
	g.linef("print(ok, e1, e2)")
	g.pop()
	g.restore(m)
	g.linef("end")
}

// riskyExpr: expressions that raise core errors (row 9 families).
func (g *gen) riskyExpr() string {
	switch g.rnd.Intn(14) {
	case 0:
		return `(nil).x`
	case 1:
		return `{} + 1`
	case 2:
		return `1 < "2"`
	case 3:
		return `"x" + 1`
	case 4:
		return `{} .. "s"`
	case 5:
		// no kind-"" calls of non-function VALUES (`(5)()`): the interp
		// runs a register VALUE scan at the raise and names any local
		// holding the callee ("(local 'v0')") — coincidence-dependent,
		// unprecomputable on wasm (row 85(c) residual; first live
		// instances found by the U7b shakeout, cases 54 of seed 424242)
		return `error("boom")`
	case 6:
		return `#missing_global_q`
	case 7:
		return `(-{})`
	case 8:
		if len(g.tables) > 0 {
			t := g.pickT()
			return fmt.Sprintf("%s.missing_method_q()", t.name)
		}
		return `error("boom")`
	case 9: // U7b: integer div/mod by zero — "attempt to perform
		// 'n//0'" / "'n%%0'", byte-identical on the all-5.5 set
		return g.pick([]string{`(1 // 0)`, `(0x10 % 0)`, `(math.maxinteger // 0)`})
	case 10: // U7b: bitwise on a non-integer — the BITERR family
		return g.pick([]string{`{} & 1`, `~{}`, `("q") | 1`, `("q") ~ 1`})
	case 11: // U7b: float with no integer representation
		return g.pick([]string{`(2.5 & 1)`, `(1e308 // 1) & 1`})
	default:
		return `error("boom")`
	}
}

// bareRisk: an unwrapped raise at chunk level — the uncaught ERROR
// event ends the log deterministically on every engine. No error()/
// error(nil) arm: nil error objects render "<no error object>" + the
// rt prefix on wasm vs the interp's form — the row-60 seam residual
// ruled for e006/e007 (the dead 2026-09-19 soak never saw it; hints
// compare raw since U5e, and the U7b shakeout's first 120 cases found
// six). Number/string error objects are row-21 deterministic.
func (g *gen) bareRisk() {
	g.spend(1)
	switch g.rnd.Intn(5) {
	case 0:
		g.linef("error(%q)", strings.Trim(g.pick(strPool), `"`))
	case 1:
		g.linef("error(%s)", g.pick(intPool))
	case 2:
		g.linef("print(%s)", g.riskyExpr())
	case 3:
		g.linef("assert(false)")
	default:
		g.linef("assert(nil, %q)", strings.Trim(g.pick(strPool), `"`))
	}
}

func (g *gen) stmtFuncDef() {
	g.spend(2)
	name := g.newFunc()
	arity := g.rnd.Intn(3)
	vararg := g.rnd.Intn(4) == 0
	rec := g.rnd.Intn(6) == 0 // guarded recursion (row 20: depth ≤ 50)

	// U7b: named vararg parameter — `...t` (U5b model; GETVARG reads,
	// the table never enters g.tables: no writes, no pairs/# — see the
	// avoidance notes in the package doc).
	var varName string
	if vararg && !rec && g.rnd.Intn(3) == 0 {
		varName = fmt.Sprintf("nv%d", g.nVarName)
		g.nVarName++
	}

	var params []string
	if rec {
		params = []string{"n"}
	} else {
		pnames := []string{"a", "b", "c"}
		params = append([]string{}, pnames[:arity]...)
		if vararg {
			if varName != "" {
				params = append(params, "..."+varName)
			} else {
				params = append(params, "...")
			}
		}
	}
	// vararg fns always return a passthrough/multi shape — set before
	// the copy lands in g.funcs (mutating fi after append is lost).
	// fi is appended AFTER the body is emitted: a body must only call
	// previously-defined functions (a DAG) — self/mutual recursion
	// would be unbounded, walking straight into the row-20 depth
	// divergence (wasm adapter guard 150 vs interp harness 1024) and,
	// with a pcall in each frame, a catch-and-recurse ping-pong that
	// spins to the deadline (found by the self-diff gate, case 71).
	fi := fnInfo{name: name, arity: len(params), vararg: vararg,
		multi: vararg || g.rnd.Intn(2) == 0}

	g.linef("local function %s(%s)", name, strings.Join(params, ", "))
	m := g.mark()
	g.push()
	if !rec { // params (a, b, c / ...) are body-locals
		for _, p := range params {
			if !strings.HasPrefix(p, "...") { // named vararg tables are read-only (U7b)
				g.scalars = append(g.scalars, p)
			}
		}
	}
	if rec {
		g.linef("if n <= 0 then return %s end", g.pick(intPool))
		if g.rnd.Intn(2) == 0 {
			g.linef("return %s(n - 1) + n", name) // non-tail recursion
		} else {
			g.linef("return %s(n - 1)", name) // tailcall (M5c trampoline)
		}
	} else {
		g.funcDepth++
		if varName != "" {
			// read-only vararg-table corners (var01 shapes): the count
			// (integer subtype), indexed reads incl. out-of-range and
			// exact-float keys, the "n" string key, other keys → nil
			switch g.rnd.Intn(4) {
			case 0:
				g.linef("print(%s.n, math.type(%s.n), %s[1], %s[2])", varName, varName, varName, varName)
			case 1:
				g.linef("print(%s[\"n\"], %s[2.0], %s[0], %s[4], %s[\"x\"])", varName, varName, varName, varName, varName)
			case 2:
				g.linef("print(select('#', ...), %s.n)", varName)
			default:
				g.linef("print(%s.n, %s[%s])", varName, varName, g.pick(intPool))
			}
		}
		g.blockBody(1 + g.rnd.Intn(2))
		g.funcDepth--
		if vararg {
			switch g.rnd.Intn(4) {
			case 0:
				g.linef("return select('#', ...), ...")
			case 1:
				g.linef("return %s, ...", g.anyScalar(0))
			case 2:
				g.linef("return select(%s, ...)", g.pick([]string{"1", "2", "3"}))
			default:
				if varName != "" {
					g.linef("return %s.n, %s[1]", varName, varName)
				} else {
					g.linef("return select(%s, ...)", g.pick([]string{"1", "2", "3"}))
				}
			}
		} else if fi.multi {
			g.linef("return %s, %s", g.anyScalar(0), g.anyScalar(0))
		} else if g.rnd.Intn(2) == 0 {
			g.linef("return %s", g.anyScalar(0))
		}
	}
	g.pop()
	g.restore(m) // params go out of scope with the body
	g.linef("end")

	// now (body fully emitted) the function becomes callable — a body
	// referencing itself could only recurse unboundedly (see note above).
	// Guarded-recursive functions are NEVER registered: a general
	// call site would pass an unbounded n (f1(255)) straight into the
	// row-20 depth divergence; their dedicated ≤50-literal callsite is
	// the only invocation.
	if !rec {
		g.funcs = append(g.funcs, fi)
	}

	if rec {
		g.linef("print(%s(%s))", name, g.pick([]string{"7", "12", "23", "40", "50"}))
		return
	}
	// a callsite so the function is exercised
	switch g.rnd.Intn(3) {
	case 0:
		g.linef("print(%s)", g.call(&fi))
	case 1:
		if fi.multi {
			g.linef("local r1, r2, r3 = %s", g.call(&fi))
			g.linef("print(r1, r2, r3)")
		} else {
			g.stmtPrint()
		}
	default:
		g.linef("%s", g.call(&fi))
	}
}

func (g *gen) stmtTableDef() {
	g.spend(1)
	name := g.newTableN()
	dense := false
	pairsable := false
	switch g.rnd.Intn(3) {
	case 0: // dense sequence
		n := 2 + g.rnd.Intn(4)
		vals := make([]string, n)
		for i := range vals {
			vals[i] = g.lit()
		}
		g.linef("local %s = {%s}", name, strings.Join(vals, ", "))
		dense = true
		pairsable = true
	case 1: // hash-only string keys (never pairs/#-ed)
		g.linef("local %s = {[%q] = %s, [%q] = %s}", name,
			strings.Trim(g.pick(identPool), `"`), g.lit(),
			strings.Trim(g.pick(identPool), `"`), g.lit())
	default: // mixed: dense array part + string extras
		g.linef("local %s = {%s, %s, %s = %s}", name,
			g.lit(), g.lit(),
			strings.Trim(g.pick(identPool), `"`), g.lit())
		dense = true
	}
	g.tables = append(g.tables, tblInfo{name: name, dense: dense, pairsable: pairsable})
	if dense {
		switch g.rnd.Intn(3) {
		case 0:
			g.linef("print(#%s, %s[1])", name, name)
		case 1:
			g.linef("table.insert(%s, %s)", name, g.lit()) // append keeps pairsable
			g.linef("print(#%s, %s[#%s])", name, name, name)
		default:
			g.linef("print(%s[%s])", name, g.pick(intPool))
		}
	} else {
		g.linef("print(%s[%q])", name, strings.Trim(g.pick(identPool), `"`))
	}
}

// mmMeta: one metamethod (name + body source).
type mmMeta struct {
	name string
	body string
}

var metamethods = []mmMeta{
	{"__index", `__index = function(t, k) return "IDX" end`},
	{"__newindex", `__newindex = function(t, k, v) rawset(t, k, v * 2) end`},
	{"__call", `__call = function(s, x) return x * 2 end`},
	{"__unm", `__unm = function(a) return -7 end`},
	{"__len", `__len = function(a) return 9 end`},
	{"__concat", `__concat = function(a, b) return "cc" end`},
	{"__eq", `__eq = function(a, b) return true end`},
	{"__lt", `__lt = function(a, b) return false end`},
	{"__le", `__le = function(a, b) return true end`},
	{"__add", `__add = function(a, b) return 100 end`},
	{"__sub", `__sub = function(a, b) return 50 end`},
	{"__mul", `__mul = function(a, b) return 7 end`},
}

// stmtMetatable: build a metatable with a random metamethod subset,
// then exercise exactly those ops (r2-probe shapes, all core-parity).
func (g *gen) stmtMetatable() {
	g.spend(2)
	mt := g.newMT()
	n := 2 + g.rnd.Intn(4)
	chosen := make([]mmMeta, 0, n)
	used := map[string]bool{}
	for len(chosen) < n {
		c := metamethods[g.rnd.Intn(len(metamethods))]
		if used[c.name] {
			continue
		}
		used[c.name] = true
		chosen = append(chosen, c)
	}
	parts := make([]string, len(chosen))
	for i, c := range chosen {
		parts[i] = c.body
	}
	g.linef("local %s = { %s }", mt, strings.Join(parts, ", "))
	t1 := g.newTableN()
	t2 := g.newTableN()
	g.linef("local %s = setmetatable({%s, %s}, %s)", t1, g.lit(), g.lit(), mt)
	g.linef("local %s = setmetatable({}, %s)", t2, mt)
	g.tables = append(g.tables, tblInfo{name: t1, dense: false, hasMT: true},
		tblInfo{name: t2, dense: false, hasMT: true})

	var ops []string
	for _, c := range chosen {
		switch c.name {
		case "__index":
			ops = append(ops, fmt.Sprintf("%s.q_key", t1))
		case "__newindex":
			ops = append(ops, fmt.Sprintf("(function() %s.brand_new_q = 21 return %s.brand_new_q end)()", t1, t1))
		case "__call":
			ops = append(ops, fmt.Sprintf("%s(%s)", t1, g.pick(intPool)))
		case "__unm":
			ops = append(ops, fmt.Sprintf("(-%s)", t1))
		case "__len":
			ops = append(ops, fmt.Sprintf("#%s", t1))
		case "__concat":
			ops = append(ops, fmt.Sprintf("(%q .. %s)", strings.Trim(g.pick(strPool), `"`), t1))
		case "__eq":
			ops = append(ops, fmt.Sprintf("(%s == %s)", t1, t2))
		case "__lt":
			ops = append(ops, fmt.Sprintf("(%s < %s)", t1, t2))
		case "__le":
			ops = append(ops, fmt.Sprintf("(%s <= %s)", t1, t2))
		case "__add":
			ops = append(ops, fmt.Sprintf("(%s + %s)", t1, t2))
		case "__sub":
			ops = append(ops, fmt.Sprintf("(%s - %s)", t1, t2))
		case "__mul":
			ops = append(ops, fmt.Sprintf("(%s * %s)", t1, t2))
		}
	}
	g.linef("print(%s)", strings.Join(ops, ", "))
}

// stmtLib: valid-by-construction library exercise (row 25 rules).
func (g *gen) stmtLib() {
	g.spend(1)
	switch g.rnd.Intn(8) {
	case 6: // U7b: subtype duality — math.type over both subtypes +
		// string coercion (hex strings convert to integers)
		g.linef("print(math.type(%s), math.type(%s), math.type(%s))",
			g.pick(intLitPool), g.pick([]string{"1.0", "2.5", "1e15", "-0.0"}), g.pick(strPool))
	case 7: // U7b: string→number coercion by subtype ("0x10" + 1 is
		// integer 17; "3.5" * 2 is float 7.0)
		g.linef("print((%s) + 1, (%s) * 2, math.type((%s) + 1))",
			g.pick(intCoerceStrPool), g.pick(numStrPool), g.pick(intCoerceStrPool))
	case 0:
		g.linef("print(string.format(\"%%d %%s %%5.2f %%q\", %s, %s, %s, %s))",
			g.pick(intPool), g.pick(strPool),
			g.pick([]string{"1.5", "2.25", "-3.75", "0.125"}), g.pick(strPool))
	case 1:
		g.linef("print((%s):sub(%s, %s), (%s):rep(%s), (%s):upper())",
			g.pick(strPool), g.pick(intPool), g.pick(intPool), g.pick(strPool),
			g.pick([]string{"2", "3"}), g.pick(strPool))
	case 2:
		g.linef("print(string.find(%s, %s), string.byte(%s), string.len(%s))",
			g.pick(strPool), g.pick(strPool), g.pick(strPool), g.pick(strPool))
	case 3:
		g.linef("print(math.floor(%s), math.sqrt(%s), math.abs(%s))",
			g.pick([]string{"-3.5", "2.9"}), g.pick([]string{"2", "16", "0.25"}), g.pick(intPool))
	case 4:
		name := g.newTableN()
		g.tables = append(g.tables, tblInfo{name: name, dense: true})
		// safe pool: table.concat renders numbers engine-natively; the
		// pool is conservative since row 49 (see safeNumStrPool)
		g.linef("local %s = {%s, %s, %s}", name, g.pick(safeNumStrPool), g.pick(safeNumStrPool), g.pick(safeNumStrPool))
		switch g.rnd.Intn(3) {
		case 0:
			g.linef("table.sort(%s)", name)
			g.linef("print(%s[1], %s[2], %s[3])", name, name, name)
		case 1:
			g.linef("print(table.concat(%s, %s))", name, g.pick([]string{`","`, `"-"`, `""`}))
		default:
			// U1 (D8): base unpack removed — 5.1 C runtime still has it,
			// so the bare name can no longer be emitted (row 54)
			g.linef("print(table.remove(%s), #%s, %s[#%s])", name, name, name, name)
		}
	default:
		g.linef("print(math.random(6), math.random(2, 9), math.fmod(%s, %s))",
			g.pick(intPool), g.pick([]string{"2", "3", "5"}))
	}
}

// ValidSource parses src with the repo's frontend; the soak uses it as
// the GENBUG gate before any engine sees the program.
func ValidSource(src []byte, name string) error {
	_, err := parse.Parse(bytes.NewReader(src), name)
	return err
}
