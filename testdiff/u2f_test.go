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
// of them; only the oracle sees the difference. The row-62 varinfo-hint
// clause closed at U3 (varinfo.go — the interp appends stock's
// debug-name hints): err00/scv05 and the 43 error-suite cases run
// against the oracle again.
//   - pairs iteration order: clua55 walks the C hash slots (seed 42),
//     the interp walks insertion order — a standing engine-specific
//     property (the corpora discipline otherwise avoids
//     order-sensitive pairs output; gfor01 predates the clua55 legs).
var clua55MatrixSkips = map[string]string{
	"gfor01.lua": "pairs iteration order is engine-specific (hash vs insertion) — corpus discipline",
	// U3b: mod00 un-skipped against the oracle too — the interp's
	// mathlib is stock xoshiro over the same makeseed=0 pin.
	// U6f: calcshape un-skipped — U5 landed 5.5's read-only for-control
	// variables and the case passes against the oracle now (the stale
	// skip fell to the refreshed interp-vs-clua55 probe).
	// Parser error wording: the fork's goyacc has no error productions,
	// so its near-token texts differ from stock's check_match family on
	// the same malformed input — the ruled standing divergence.
	"len03.lua": "goyacc's 'unexpected symbol' vs stock check_match's ')' expected — row 82",
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
// (every wasm-seam case already skips via ledgeredErrSkips). The row-62
// varinfo-hint deferral closed at U3: the interp now appends stock's
// debug-name hints (varinfo.go), so the 43 hint-bearing cases run
// against the oracle. mo00 stays skipped on the parse-error family —
// goyacc's near-token on `t.a t.a t.a` ('.') differs from stock's
// exprstat error at the following 't' — the U5 parser pass (row 62).
var clua55ErrOracleSkips = map[string]string{
	"mo00.lua": "goyacc near-token '.' vs stock's exprstat 't' — U5 parser pass (row 62)",
}
