package testdiff

import (
	"path/filepath"
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
	case "_u5":
		return u5WasmSkips
	default:
		return nil
	}
}

// u5WasmSkips (U5e, row 85): _u5 cases whose interp↔wasm divergence is
// ruled — everything else on the corpus must be byte-exact on the wasm
// legs, the Stage A/B/C surfaces included. The oracle gate (TestU5Gate,
// interp↔clua55) runs the corpus whole: every skip here is an
// engine-seam divergence, not a language one.
var u5WasmSkips = map[string]string{
	// the U5d yield surface is interp-side; the backend grows it at U6
	// (row 84's wasm clause)
	"coyield01.lua": "yield through marker frames is interp-side; backend at U6 — row 84",
	"coyield02.lua": "noyield boundary error is interp-side; backend at U6 — row 84",
	"coyield03.lua": "yield through marker frames is interp-side; backend at U6 — row 84",
	"coyield04.lua": "yield through marker frames is interp-side; backend at U6 — row 84",
	"coyield05.lua": "kind-6 metamethod yields are interp-side; backend at U6 — row 84",
	"coyield06.lua": "kinds 4/5 close continuations are interp-side; backend at U6 — row 84",
	// debug introspection over wasm frames (the row-23 family)
	"dbg01.lua":    "debug.getlocal level walk over wasm frames — row 23",
	"rdkctc01.lua": "debug.getlocal level walk over wasm frames (:54) — row 23",
	// the row-61 GC freeze
	"gcparam01.lua": "collectgarbage inert under the per-run GC freeze — row 61",
	// the row-60 CClosure seam presentation
	"getinfo01.lua": "getinfo over the CClosure seam (no `name`, isvararg false) — row 60",
	"udata01.lua":   "upvalueid identity across the rt-owned upvalue registry — row 60",
	// U5f-1: dump/binary-load. A backend-compiled function is a CClosure
	// over host.wasm_dispatch on the rt side (row 60's seam) — there is no
	// L closure, no proto to serialize, so string.dump of one is refused
	// ("Lua function expected") and the dump/load round trips cannot run.
	"dump01.lua": "string.dump of a backend-compiled function (a CClosure over the dispatch seam — no rt-side proto) — row 60",
	"load01.lua": "string.dump of a backend-compiled function (a CClosure over the dispatch seam — no rt-side proto) — row 60",
	// U6a closed row 86 (extras below the window + rt_call_addr): var07/
	// vgap01/limits01 run on the wasm legs now.
}

// CorpusSkipsFor was retired with the row-71 seam strip (U5e): the seam
// engines now raise with the same varinfo hints as the oracles, so there
// are no wasm-leg-only skips — callers use CorpusSkips directly.

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
	// U5b un-skipped var08/var16/var20 — the compat `arg` table is dead
	// (row 60's clause closes with it); the corpus regenerated onto the
	// pack-table / first-extra shapes.
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
	// U5e landed the row-60 error-position seam (rt_seam55.c: shadow line
	// stack + interposed luaL_where/lua_error/luaG_runerror): error()'s
	// level walk now resolves compiled frames through the shadow stack —
	// e003/e005/e011 run un-skipped and byte-exact. What remains: level-0
	// raises still gain the rt prefix (luaB_error's level-0 arm calls no
	// where(), so the seam's nopos verdict never fires — the position
	// would have to come from interposing the static luaB_error itself),
	// and nil error objects render "<no error object>" + the rt prefix
	// where the interp renders "nil".
	"e001.lua": "level-0 error gains the rt prefix on wasm — row 60 seam (level-0 arm calls no where)",
	"e006.lua": "nil error object renders differently on wasm — row 60 seam (U5 re-pin)",
	"e007.lua": "nil error object renders differently on wasm — row 60 seam (U5 re-pin)",
}

// skipProdBlobUntilU6d was deleted at U6d (ledger row 95): lua55_prod.wasm
// shipped and every prod leg runs live — the guards came out of
// m6c/m6cdet/m6d wholesale. skipRtSurfacesUntilU6 had gone at U6c (row 94).
