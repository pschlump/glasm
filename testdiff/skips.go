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
	case "_u9":
		return u9WasmSkips
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
	// U6f (row 97): the wasm yield posture, ruled. Interp/clua55 suspend
	// these shapes (TestU5Gate); compiled frames are CClosures over the
	// dispatch seam AND every rt_* call from compiled code runs on the
	// rt-pinned MAIN state, so a yield reaches lua_yieldk on the main
	// thread — stock's own branch (ldo.c:1015) raises the catchable,
	// position-prefixed "attempt to yield from outside a coroutine".
	// _u6/coyield01.lua pins the wasm behavior absolutely.
	"coyield01.lua": "yield from compiled code raises stock's mainthread text (interp suspends) — row 97",
	"coyield02.lua": "noyield shapes raise the prefixed mainthread text, not the bare C-boundary wording — row 97",
	"coyield03.lua": "yield from compiled code raises stock's mainthread text (interp suspends) — row 97",
	"coyield04.lua": "yield from compiled code raises stock's mainthread text (interp suspends) — row 97",
	"coyield05.lua": "kind-6 metamethod yields raise the mainthread text — row 97",
	"coyield06.lua": "close-continuation yields raise the mainthread text — row 97",
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

// u9WasmSkips (U9c, row 104): _u9 cases whose interp↔wasm divergence
// is ruled. The oracle gate (TestU9Gate) runs the corpus whole, zero
// skips. Empty as of the stack-presence fix: every direct frame now
// pushes a minimal real CallInfo + sea entry (row 104), so the level
// walk counts direct frames exactly like trampoline ones — dd02 ran
// un-skipped and byte-exact after it (and e003/e005/e011 below came
// back the same way). Only past the Lua-stack headroom do direct frames
// run ci-less (the ruled degraded posture, row 104) — no corpus case
// pins introspection at that depth.
var u9WasmSkips = map[string]string{}

// ProdBlobSkips (U6f row 98, exported at U8): the row-95 prod-flavor
// skips — the compiled-out surface of lua55_prod.wasm — layered over the
// corpus map by BOTH the go-test exit gate's prod pass (exitGateEngines)
// and cmd/testdiff's prod legs, so the two can never disagree about what
// is excluded (the skips.go single-source rule).
func ProdBlobSkips() map[string]string { return prodBlobSkips }

// CorpusSkipsFor was retired with the row-71 seam strip (U5e): the seam
// engines now raise with the same varinfo hints as the oracles, so there
// are no wasm-leg-only skips — callers use CorpusSkips directly.

// gluaWasmSkips: _glua-tests cases with a ruled divergence (each cites
// its row in docs/Lua-Wasm-Divergence-Ledger.md).
var gluaWasmSkips = map[string]string{
	"issues.lua": "yield through coroutine.resume from a compiled body raises stock's mainthread text (interp suspends) — row 97",
	"db.lua":     "debug.getinfo introspection over wasm frames — row 23",
	// U8 audit: coroutine.lua/goto.lua/vm.lua un-skipped — all three run
	// byte-exact on the wasm, wazero, AND clua55 legs (probed; coroutine.lua
	// was re-pinned to wrap/resume shapes; row 87 re-pinned load parity and
	// the parser-limit texts). Rows 22/24 close; issues.lua re-cites row 97
	// (the yield posture), goto/vm move to prodBlobSkips (load is nil on
	// the prod flavor — row 95, same as _u5's gotostar01/var06/var08).
	// U3b: math.lua un-skipped — re-pinned to the 5.5 surface (pow
	// removed, stock arity text); row 25's math clause closes.
	// U3d: table.lua un-skipped — the table lib is on the stock surface
	// and the corpus case was re-pinned with it (no maxn/getn).
	// U8 audit: strings.lua re-cited off the stale row 25 (string.dump
	// landed U5f-1, row 87). The live divergence is the seam's argerror
	// NAME arm — row 60(5): stock names a raising C function via its
	// caller, a compiled caller is a CClosure, so stock's C-caller
	// fallback yields the qualified 'string.dump' where interp+clua55
	// say 'dump' (the case dies at strings.lua:5).
	"strings.lua": "argerror names the function through the compiled caller ('string.dump' vs 'dump') — row 60(5)",
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
	// U3b: mod00 un-skipped — the D5 host-RNG shim retired; every engine
	// runs stock xoshiro over the makeseed=0 pin.
	//
	// U8 audit: EMPTY. tsort01.lua — the last entry — was un-skipped:
	// it runs byte-exact on the wasm, wazero, and clua55 legs (probed).
	// The U6a frame-layout surgery (row 86) fixed the return-path
	// corruption; row 10 closes and the un-skipped case is its covering
	// test. Earlier families left only comments above.
}

// ledgeredErrSkips: _wasm-err-tests cases whose divergence has a ruling
// (each skip cites its row in docs/Lua-Wasm-Divergence-Ledger.md).
var ledgeredErrSkips = map[string]string{
	// U5e landed the row-60 error-position seam (rt_seam55.c: shadow line
	// stack + interposed luaL_where/lua_error/luaG_runerror): error()'s
	// level walk now resolves compiled frames through the shadow stack —
	// e003/e005/e011 ran un-skipped and byte-exact until U9c, were
	// re-ruled under row 104 (a direct frame had no CallInfo and pushed
	// no shadow entry), and came BACK byte-exact with row 104's
	// stack-presence fix: every direct frame now pushes a minimal real
	// CallInfo + sea entry, so the level walk counts it exactly like a
	// trampoline frame (the three entries stay listed here only in this
	// comment). What remains from U5e: level-0 raises still gain the rt
	// prefix (luaB_error's level-0 arm calls no where(), so the seam's
	// nopos verdict never fires — the position would have to come from
	// interposing the static luaB_error itself), and nil error objects
	// render "<no error object>" + the rt prefix where the interp renders
	// "nil".
	"e001.lua": "level-0 error gains the rt prefix on wasm — row 60 seam (level-0 arm calls no where)",
	"e006.lua": "nil error object renders differently on wasm — row 60 seam (U5 re-pin)",
	"e007.lua": "nil error object renders differently on wasm — row 60 seam (U5 re-pin)",
}

// prodBlobSkips (U6f, row 98): corpus cases exercising the prod
// flavor's compiled-out surface (row 95 — io/os/debug/package/require/
// loadfile/load/dofile/warn are nil there). Applied ONLY to the
// wazero-prod column of the exit-gate engine set; every other engine
// runs them (dead-code users like calcshape's io.open pass and stay —
// the audit was the empirical interp-vs-prod probe over each corpus).
var prodBlobSkips = map[string]string{
	// _wasm-tests: the utf8 battery loads its library require'utf8'
	"slu00.lua": "require'utf8' — require is nil on the prod flavor — row 95",
	"slu01.lua": "require'utf8' — require is nil on the prod flavor — row 95",
	"slu02.lua": "require'utf8' — require is nil on the prod flavor — row 95",
	"slu03.lua": "require'utf8' — require is nil on the prod flavor — row 95",
	"slu04.lua": "require'utf8' — require is nil on the prod flavor — row 95",
	"slu05.lua": "require'utf8' — require is nil on the prod flavor — row 95",
	"slu06.lua": "require'utf8' — require is nil on the prod flavor — row 95",
	"slu07.lua": "require'utf8' — require is nil on the prod flavor — row 95",
	// _glua-tests
	"base.lua": "dofile('notexist') — dofile is nil on the prod flavor — row 95",
	"os.lua":   "the os library is compiled out of the prod flavor — row 95",
	// U8 audit: goto.lua/vm.lua un-skipped off the stale row 24 (the
	// dev-blob legs and the oracle column run them byte-exact — row 87's
	// load/parser-text re-pins) and re-landed HERE: load() is nil on the
	// prod flavor (vm.lua:84 pins it), the same ruling as _u5's cases.
	"goto.lua": "load() — nil on the prod flavor — row 95",
	"vm.lua":   "load() — nil on the prod flavor — row 95",
	// _u5 (the corpus discipline strips semerrors through load())
	"call01.lua":     "debug.getinfo — debug is nil on the prod flavor — row 95",
	"gotostar01.lua": "load() — nil on the prod flavor — row 95",
	"limits01.lua":   "load() — nil on the prod flavor — row 95",
	"var06.lua":      "load() — nil on the prod flavor — row 95",
	"var08.lua":      "load() — nil on the prod flavor — row 95",
}

// skipProdBlobUntilU6d was deleted at U6d (ledger row 95): lua55_prod.wasm
// shipped and every prod leg runs live — the guards came out of
// m6c/m6cdet/m6d wholesale. skipRtSurfacesUntilU6 had gone at U6c (row 94).
