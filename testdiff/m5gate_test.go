package testdiff

// M5e gates: the backend over the full corpora, differentially against
// the interpreter oracle. _wasm-tests is the opcode matrix; _glua-tests
// is the fork's own conformance suite. Every skip cites its divergence
// ledger row (docs/Lua-Wasm-Divergence-Ledger.md; the shared maps live
// in skips.go so cmd/testdiff applies the same ones).

import (
	"testing"
)

// wasmGluaFull runs interp vs wasm over a corpus and fails on any
// unruled divergence.
func wasmGluaFull(t *testing.T, dir string, skips map[string]string) {
	t.Helper()
	cases, err := LoadCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus %s: %v", dir, err)
	}
	cases, skipped := FilterSkips(cases, skips)
	engines := []Engine{NewInterp("interp"), &WasmEngine{name: "wasm", SkipUnsupported: true}}
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
		t.Fatalf("%d/%d cases diverge (see ledgered skip map)", bad, len(cases))
	}
	if len(cases) == 0 {
		t.Fatal("empty corpus")
	}
}

// TestWasmGluaFull: the M5 milestone gate — full _glua-tests, interp vs
// wasm, 0 unledgered DIVERGE.
func TestWasmGluaFull(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy corpus — skipped in -short mode")
	}
	wasmGluaFull(t, "../_glua-tests", gluaWasmSkips)
}

// TestWasmFullMatrix: the regenerated opcode matrix stays green.
func TestWasmFullMatrix(t *testing.T) {
	wasmGluaFull(t, "../_wasm-tests", CorpusSkips("../_wasm-tests"))
	// (rows 10u/20/28 were unskipped with the M6 call_body rebasing;
	// the live skips — whlc00-03, nz00-01 — are the M6e rows 41/42,
	// owned by skips.go so the CLI leg and this gate cannot disagree)
}
