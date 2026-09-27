package testdiff

import (
	"path/filepath"
	"testing"
)

// CorpusSkips is the single source of truth for ledgered corpus skips,
// shared by the gate tests and cmd/testdiff (M6b): a skip map that lived
// only on one side would let the CLI leg and the go-test leg disagree
// about what is excluded. Every skip must cite its row in
// docs/Lua-Wasm-Divergence-Ledger.md (§8.9: a divergence without a row is
// a bug; FilterSkips panics on a reasonless skip).
func CorpusSkips(dir string) map[string]string {
	switch filepath.Base(dir) {
	case "_glua-tests":
		return gluaWasmSkips
	case "_wasm-tests":
		return wasmTestsSkips
	case "_wasm-err-tests":
		return ledgeredErrSkips
	default:
		return nil
	}
}

// gluaWasmSkips: _glua-tests cases with a ruled divergence (each cites
// its row in docs/Lua-Wasm-Divergence-Ledger.md).
var gluaWasmSkips = map[string]string{
	"coroutine.lua": "coroutines out of scope for the Redis subset — row 22",
	"issues.lua":    "yield across C boundary (coroutines) — row 22",
	"db.lua":        "debug.getinfo introspection over wasm frames — row 23",
	"goto.lua":      "loadstring-mixed C-interpreted chunks — row 24",
	"vm.lua":        "loadstring + gopher parser-error wording (register overflow) — row 24",
	"math.lua":      "library-error wording/behavior (math.max arity text) — row 25",
	"strings.lua":   "library-error wording/behavior (string.dump unsupported in the fork) — row 25",
	// U2e: the wasm engine is the stock-5.5 stdlib now; the interp keeps
	// 5.1-era extras (D8's removal list re-pins with the corpora at U2f)
	"table.lua": "table.maxn is a 5.1-era extra on interp, removed in 5.2+ — row 61 (re-pin at U2f)",
}

// wasmTestsSkips: _wasm-tests cases with a ruled divergence (each cites
// its row in docs/Lua-Wasm-Divergence-Ledger.md). Rows 41/42 were
// fixed (branchPartner for the NOP'd jump partner; LNumber2I/ConstIndex
// -0.0 sign) — whlc00-03 and nz00-01 were unskipped with the fix.
// U2e un-skipped the row-56 integer/text families: the ABI v4 blob
// speaks stock 5.5 semantics + texts, matching the interp's re-pins.
var wasmTestsSkips = map[string]string{
	// U2e (row 60): the 5.0-compat `arg` table is a 5.1-ism the v4 blob
	// does not carry (rt_compat_arg dropped until U6); the interp's
	// CompatVarArg dies at U5's vararg rework. The corpus regenerates
	// with the U5/U6 vararg work.
	"var08.lua": "compat `arg` table not on the v4 blob until U6 — row 60",
	"var16.lua": "compat `arg` table not on the v4 blob until U6 — row 60",
	"var20.lua": "compat `arg` table not on the v4 blob until U6 — row 60",
	// row 56 — interp wording never re-pinned to stock; corpus re-pins
	// from clua55 at U2f
	"err00.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"err02.lua": "concat error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	// row 60(4) — the blob formats floats with stock %.14g; the interp
	// prints Go-dialect shortest-float (D7 unification at U5)
	"gnf00.lua": "float printing: stock %.14g on wasm, Go dialect on interp — row 60(4) / D7 (U5)",
	"gnf01.lua": "float printing: stock %.14g on wasm, Go dialect on interp — row 60(4) / D7 (U5)",
	// row 10 — table.sort + deep __call/tailcall chains corrupt the
	// return path (pre-existing M5d/M6 bucket)
	"tsort01.lua": "sort comparator + __call chains corrupt the return path — row 10",
}

// ledgeredErrSkips: _wasm-err-tests cases whose divergence has a ruling
// (each skip cites its row in docs/Lua-Wasm-Divergence-Ledger.md).
var ledgeredErrSkips = map[string]string{
	// U2e un-skipped the ar*/fo*/mi00-mi01 row-56 families (the v4 blob
	// speaks the stock texts the interp was re-pinned to). What remains:
	//
	// row 56 — interp wording never re-pinned to stock (assert/concat/
	// call/compare/index texts, error() position dialect, nil/absent
	// error objects): the corpus re-pins mechanically from clua55 at U2f.
	"as03.lua": "assert position text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"as04.lua": "assert strict-string message arg is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cc00.lua": "concat error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cc01.lua": "concat error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cc02.lua": "concat error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cc03.lua": "concat error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cc04.lua": "concat error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cc05.lua": "concat error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cc06.lua": "concat error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cc07.lua": "concat error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cl00.lua": "call error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cl01.lua": "call error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cl02.lua": "call error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cl03.lua": "call error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cl04.lua": "call error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cl05.lua": "call error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cp04.lua": "compare error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cp05.lua": "compare error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cp06.lua": "compare error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"cp07.lua": "compare error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"e001.lua": "error() position dialect differs from stock — row 56 (re-pin at U2f)",
	"e006.lua": "nil/absent error-object rendering differs from stock — row 56 (re-pin at U2f)",
	"e007.lua": "nil/absent error-object rendering differs from stock — row 56 (re-pin at U2f)",
	"ix00.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix01.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix02.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix03.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix04.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix05.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix06.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix07.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix08.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix09.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix10.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"ix11.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	"pc10.lua": "index error text is gopher dialect on interp — row 56 (re-pin at U2f)",
	// row 60(2) — the error-position seam: luaL_where over a wasm
	// C-closure frame has no line-stack hook (zero-patch tree), so
	// error() levels walk to the nearest real Lua frame and pcall'd
	// core raises lose the caller prefix; the re-pin lands with the U5
	// error/EH pass (row 52 family).
	"e005.lua": "error() level walks stop at wasm C frames — row 60 (U5 re-pin)",
	"e010.lua": "pcall(error, ...) prefix lost over the C frame — row 60 (U5 re-pin)",
	"pc02.lua": "pcall(error, ...) prefix lost over the C frame — row 60 (U5 re-pin)",
	// row 56 — string-operand arith texts: stock 5.5 names the operator
	// ("attempt to add a 'string' with a 'number'"); the interp kept the
	// 5.4-era generic wording. Re-pins from clua55 at U2f.
	"ar10.lua": "string-arith text: 5.5 names the operator, interp is 5.4-era — row 56 (re-pin at U2f)",
	"ar14.lua": "string-arith text: 5.5 names the operator, interp is 5.4-era — row 56 (re-pin at U2f)",
	"ar15.lua": "string-arith text: 5.5 names the operator, interp is 5.4-era — row 56 (re-pin at U2f)",
}

// skipRtSurfacesUntilU6: the row-59 retirement window closed with the
// U2e backend rewrite (the wasm/wazero gates run on the ABI v4
// lua55_rt.wasm blob). What still waits is the U6 runtime-cutover tail:
// rt_sandbox, the memlimit/deadline block, and the 5.1 prod-blob flavor
// are v3-only surfaces (row 60) — the gates over them stay skipped
// until the prod blob moves to the 5.5 tree.
func skipRtSurfacesUntilU6(t *testing.T) {
	t.Skip("rt sandbox/memlimit/deadline + prod-blob surfaces are v3-only until the U6 cutover — ledger row 60")
}
