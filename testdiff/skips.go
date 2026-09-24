package testdiff

import "path/filepath"

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
}

// wasmTestsSkips: _wasm-tests cases with a ruled divergence (each cites
// its row in docs/Lua-Wasm-Divergence-Ledger.md). Rows 41/42 were
// fixed (branchPartner for the NOP'd jump partner; LNumber2I/ConstIndex
// -0.0 sign) — whlc00-03 and nz00-01 were unskipped with the fix.
var wasmTestsSkips = map[string]string{
	// U2b (row 56): interp has 5.3 integer semantics + stock-5.5 error
	// texts; the 5.1 rt blob errors nowhere near these until ABI v4
	// (U2e/U2f) — corpus regenerates then.
	"ari028.lua": "integer % 0 errors on interp (5.5), nan on wasm — row 56",
	"ari100.lua": "integer % 0 errors on interp (5.5), nan on wasm — row 56",
	"ari136.lua": "integer % 0 errors on interp (5.5), nan on wasm — row 56",
	"ari172.lua": "integer % 0 errors on interp (5.5), nan on wasm — row 56",
	"ari208.lua": "integer % 0 errors on interp (5.5), nan on wasm — row 56",
	"err01.lua":  "arith-on-table text re-pinned to stock 5.5 on interp — row 56",
	"err03.lua":  "for-error text re-pinned to stock 5.5 on interp — row 56",
}

// ledgeredErrSkips: _wasm-err-tests cases whose divergence has a ruling
// (each skip cites its row in docs/Lua-Wasm-Divergence-Ledger.md).
var ledgeredErrSkips = map[string]string{
	// (rows 20/28 were fixed with the M6 call_body rebasing; so00/so01/
	// pc10 pass. U2b row 56: the ar*/fo*/mi00-mi01 families pin error
	// texts the interp re-pinned to stock 5.5 while the 5.1 rt blob
	// still speaks gopher wording — they skip until the 5.5 rt port
	// (U2e) and the corpus regeneration (U2f).)
	"ar00.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar01.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar02.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar03.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar04.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar05.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar06.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar07.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar08.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar09.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar10.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar11.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar12.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar13.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar14.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar15.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar16.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"ar17.lua": "arith error text re-pinned to stock 5.5 on interp — row 56",
	"fo00.lua": "for-error text re-pinned to stock 5.5 on interp — row 56",
	"fo01.lua": "for-error text re-pinned to stock 5.5 on interp — row 56",
	"fo02.lua": "for-error text re-pinned to stock 5.5 on interp — row 56",
	"fo03.lua": "for-error text re-pinned to stock 5.5 on interp — row 56",
	"fo04.lua": "for-error text re-pinned to stock 5.5 on interp — row 56",
	"fo05.lua": "for-error text re-pinned to stock 5.5 on interp — row 56",
	"mi00.lua": "len-error text re-pinned to stock 5.5 on interp — row 56",
	"mi01.lua": "len-error text re-pinned to stock 5.5 on interp — row 56",
}
