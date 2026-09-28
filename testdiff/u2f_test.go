package testdiff

// U2f gates: the regenerated int/float/bitwise matrix against the 5.5.1
// ORACLE (clua55 — the semantic authority for the upgrade, D6), and the
// closeout checks. The interp↔wasm matrix legs live in m5gate_test.go;
// this is the "× {interp, clua55, wasm, wazero}" leg the U2 exit gate
// adds (docs/U2-implementation-plan.md §6).

import (
	"testing"
)

// clua55MatrixSkips: cases whose interp↔clua55 delta has a ruling.
// These are SEPARATE from the wasm skips — the wasm legs agree on all
// of them; only the oracle sees the difference:
//   - varinfo hints: stock appends "(local 'x')" / "(field 'x')" to
//     type errors via luaG_typeerror's varinfo; the interp's raises
//     carry no debug names (and the wasm rt seam has none either), so
//     hint-bearing error cases diverge against clua55 only (ledger row
//     62; the debug-info plumbing lands with U5/U7 introspection).
//   - pairs iteration order: clua55 walks the C hash slots (seed 42),
//     the interp walks insertion order — a standing engine-specific
//     property (the corpora discipline otherwise avoids
//     order-sensitive pairs output; gfor01 predates the clua55 legs).
var clua55MatrixSkips = map[string]string{
	"err00.lua":  "clua55 appends the (local 't') varinfo hint — row 62",
	"scv05.lua":  "clua55 appends the (constant '7') varinfo hint — row 62",
	"gfor01.lua": "pairs iteration order is engine-specific (hash vs insertion) — corpus discipline",
	// U3b: mod00 un-skipped against the oracle too — the interp's
	// mathlib is stock xoshiro over the same makeseed=0 pin.
	// 5.5's read-only for-control variables (calcshape assigns one) are
	// U5 language work — the interp still allows it
	"calcshape.lua": "5.5 read-only for control variables — U5 (plan U5 row)",
	// parser error wording: the fork's recursive-descent shapes differ
	// from stock's LR messages on the same malformed input — U5 parser
	// wording pass
	"len03.lua": "parse-error wording differs (unexpected symbol vs ')' expected) — U5 parser pass",
}

// TestU2fMatrixOracle: the regenerated _wasm-tests matrix, interp vs
// clua55 — the stock-semantics statement of U2.
func TestU2fMatrixOracle(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy corpus — skipped in -short mode")
	}
	cases, err := LoadCorpus("../_wasm-tests")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	skips := CorpusSkips("../_wasm-tests")
	for k, v := range clua55MatrixSkips {
		skips[k] = v
	}
	cases, skipped := FilterSkips(cases, skips)
	engines := []Engine{NewInterp("interp"), &CLua55{name: "clua55"}}
	results := RunCorpus(cases, engines)

	bad := 0
	for _, r := range results {
		if d := r.Diff(); d != "" {
			bad++
			t.Errorf("%s:\n%s", r.Name, d)
		}
	}
	t.Log("\n" + Summary(results, skipped))
	if bad > 0 {
		t.Fatalf("%d/%d cases diverge vs the oracle (see skip maps)", bad, len(cases))
	}
	if len(cases) == 0 {
		t.Fatal("empty corpus")
	}
}

// TestU2fErrorOracle: the re-pinned error suite against clua55 — the
// wording re-pin's proof against the authority (the seam cases stay in
// the interp↔wasm skip map and skip here too).
func TestU2fErrorOracle(t *testing.T) {
	cases, err := LoadCorpus("../_wasm-err-tests")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	skips := map[string]string{}
	for k, v := range ledgeredErrSkips {
		skips[k] = v
	}
	for k, v := range clua55ErrOracleSkips {
		skips[k] = v
	}
	cases, skipped := FilterSkips(cases, skips)
	engines := []Engine{NewInterp("interp"), &CLua55{name: "clua55"}}
	results := RunCorpus(cases, engines)

	bad := 0
	for _, r := range results {
		if d := r.Diff(); d != "" {
			bad++
			t.Errorf("%s:\n%s", r.Name, d)
		}
	}
	t.Log("\n" + Summary(results, skipped))
	if bad > 0 {
		t.Fatalf("%d/%d cases diverge vs the oracle", bad, len(cases))
	}
	if len(cases) == 0 {
		t.Fatal("empty corpus")
	}
}

// clua55ErrOracleSkips: interp↔clua55-only rulings for the error suite
// (every wasm-seam case already skips via ledgeredErrSkips). Row 62:
// stock appends varinfo hints — (local 'x') / (global 'g') / (field
// 'f') / (constant 'k') — to type errors via luaG_typeerror's varinfo;
// the interp raises without debug names and the wasm rt seam cannot
// carry them either, so hint-bearing cases skip against the oracle
// until the U5/U7 introspection work plumbs names into the raises.
var clua55ErrOracleSkips = row62Hints([]string{
	// the collected hint-bearing set (deterministic against clua55):
	// arith on named non-numbers, concat/call/index/len type errors,
	// compare errors, and pcall-caught shapes naming the operand
	"ar06", "ar07", "ar08", "ar09", "ar10", "ar11", "ar12", "ar13",
	"ar14", "ar16", "ar17",
	"cc00", "cc01", "cc02", "cc03", "cc04", "cc05", "cc06", "cc07",
	"cl00", "cl01", "cl02", "cl03", "cl04", "cl05",
	"ix00", "ix01", "ix02", "ix03", "ix04", "ix05", "ix06", "ix07",
	"ix08", "ix09", "ix10", "ix11",
	"mi00", "mi01", "pc10",
	"cp00", "cp01", "cp08",
	"e000", "e002",
	"mo00",
})

func row62Hints(names []string) map[string]string {
	m := map[string]string{}
	for _, n := range names {
		m[n+".lua"] = "clua55 appends the varinfo hint — row 62 (U5/U7 debug-info)"
	}
	return m
}
