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

// CorpusSkipsFor: CorpusSkips plus the skips that apply only when the
// engine set includes a hintless seam engine (row 71 — the wasm legs).
func CorpusSkipsFor(dir string, engines []Engine) map[string]string {
	skips := CorpusSkips(dir)
	seam := false
	for _, e := range engines {
		if seamHintless(e) {
			seam = true
			break
		}
	}
	if !seam {
		return skips
	}
	merged := make(map[string]string, len(skips)+len(seamHintPrintSkips))
	for k, v := range skips {
		merged[k] = v
	}
	for k, v := range seamHintPrintSkips {
		merged[k] = v
	}
	return merged
}

// seamHintPrintSkips: cases that PRINT a pcall-captured runtime error
// whose message carries a row-70 varinfo hint. The hint rides a PRINT
// line, beyond the row-71 ERROR-line seam strip, and the wasm seam
// raises hint-less — so these skip on wasm-family legs only; the oracle
// legs (interp ↔ clua55, both hint-bearing) run them in full.
var seamHintPrintSkips = map[string]string{
	"pc10.lua": "printed error message carries a varinfo hint on interp only — row 71 seam (U6)",
}

// gluaWasmSkips: _glua-tests cases with a ruled divergence (each cites
// its row in docs/Lua-Wasm-Divergence-Ledger.md).
var gluaWasmSkips = map[string]string{
	"coroutine.lua": "coroutines out of scope for the Redis subset — row 22",
	"issues.lua":    "yield across C boundary (coroutines) — row 22",
	"db.lua":        "debug.getinfo introspection over wasm frames — row 23",
	"goto.lua":      "loadstring-mixed C-interpreted chunks — row 24",
	"vm.lua":        "loadstring + gopher parser-error wording (register overflow) — row 24",
	// U3b: math.lua un-skipped — re-pinned to the 5.5 surface (pow
	// removed, stock arity text); row 25's math clause closes.
	// U3d: table.lua un-skipped — the table lib is on the stock surface
	// and the corpus case was re-pinned with it (no maxn/getn).
	"strings.lua": "library-error wording/behavior (string.dump unsupported in the fork) — row 25",
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
	// U2f: err00/err02 un-skipped — the interp's index/concat texts are
	// re-pinned to stock, matching the blob.
	// U3i (row 70d): gnf00/gnf01 un-skipped — the interp's number
	// tostring is numberToStringStock (the stock %.14g-with-".0"
	// spelling), matching the v4 blob; row 60(4)'s clause closes.
	// row 10 — table.sort + deep __call/tailcall chains corrupt the
	// return path (pre-existing M5d/M6 bucket)
	"tsort01.lua": "sort comparator + __call chains corrupt the return path — row 10",
	// U3b: mod00 un-skipped — the D5 host-RNG shim retired; every engine
	// runs stock xoshiro over the makeseed=0 pin.
}

// ledgeredErrSkips: _wasm-err-tests cases whose divergence has a ruling
// (each skip cites its row in docs/Lua-Wasm-Divergence-Ledger.md).
var ledgeredErrSkips = map[string]string{
	// U2f re-pinned the interp's error texts to stock (concat/index/call/
	// compare shapes, string-arith opnames, assert passthrough, error()
	// level semantics) — those families run un-skipped now. What remains
	// is the row 60 error-position seam: the wasm rt layer cannot see
	// error()'s level target (its walk stops at the C frame) and cannot
	// distinguish a level-0 raise from a core raise, so positions land
	// on the CALL line instead of the level-target frame, level-0 errors
	// gain a prefix, and nil error objects render "<no error object>"
	// with a prefix where the interp renders "nil". The U5 error/EH pass
	// (marker seam, row 52 family) re-pins these.
	"e001.lua": "level-0 error gains the rt prefix on wasm — row 60 seam (U5 re-pin)",
	"e003.lua": "error() level walk stops at wasm C frames — row 60 seam (U5 re-pin)",
	"e005.lua": "error() level walk stops at wasm C frames — row 60 seam (U5 re-pin)",
	"e006.lua": "nil error object renders differently on wasm — row 60 seam (U5 re-pin)",
	"e007.lua": "nil error object renders differently on wasm — row 60 seam (U5 re-pin)",
	"e011.lua": "error() level walk stops at wasm C frames — row 60 seam (U5 re-pin)",
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
