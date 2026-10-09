package testdiff

// M5e gates: the backend over the full corpora, differentially against
// the interpreter oracle. _wasm-tests is the opcode matrix; _glua-tests
// is the fork's own conformance suite. Every skip cites its divergence
// ledger row (docs/Lua-Wasm-Divergence-Ledger.md; the shared maps live
// in skips.go so cmd/testdiff applies the same ones).
//
// U6f (plan §5) grew these into the exit-gate engine set: interp
// against {wasm, wazero} + the clua55 column + the wazero-prod column.

import (
	"path/filepath"
	"testing"
)

// exitGateEngines (U6f, plan §5): a corpus over the exit-gate engine
// set, in per-column passes so each column keeps its own ledgered
// rulings — a corpus-wide filter would pull the oracle-only skips (row
// 82's parser texts, pairs iteration order) off the wasm legs that run
// those cases green, and only the prod column loses the
// compiled-out-surface users (prodBlobSkips, rows 95/98).
//
// withOracle: whether THIS gate sweeps the clua55 column inline. The
// big corpora's oracle columns are the standing U2f gates
// (TestU2fMatrixOracle/TestU2fErrorOracle — same corpus, engines, and
// skip maps), and _u5's is TestU5Gate (the corpus whole, zero skips —
// a superset); re-sweeping them here would roughly double the
// per-process wasmtime Store count, and every Store statically maps
// the blob's 256 MiB memory + ~8 GB of arm64 guard region — past ~4k
// stores the process dies with SIGTRAP (the M6e soak cliff,
// note/m6e-implemented.md; Go finalizers reclaim far too slowly, and
// runtime.GC() cadencing did NOT stop it). _glua-tests has no
// dedicated oracle gate, so TestWasmGluaFull sweeps inline (4 cases).
func exitGateEngines(t *testing.T, dir string, skips, oracleSkips map[string]string, withOracle bool) {
	t.Helper()
	cases, err := LoadCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus %s: %v", dir, err)
	}
	layer := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range skips {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	run := func(label string, engines []Engine, extra map[string]string) {
		cs, skipped := FilterSkips(cases, layer(extra))
		results := RunCorpus(cs, engines)
		bad := 0
		for _, r := range results {
			for _, e := range engines { // an unsupported case is a skip-map matter
				for _, l := range r.Logs[e.Name()] {
					if len(l) > 9 && l[:9] == "SKIP-UNSU" {
						bad++
						t.Errorf("%s: %s", r.Name, l)
					}
				}
			}
			if d := r.Diff(); d != "" {
				bad++
				t.Errorf("%s:\n%s", r.Name, d)
			}
		}
		t.Logf("[%s] %s", label, Summary(results, skipped))
		if bad > 0 {
			t.Fatalf("%s: %d/%d cases diverge (see ledgered skip maps)", label, bad, len(cs))
		}
		if len(cs) == 0 {
			t.Fatalf("%s: empty corpus", label)
		}
	}
	prod := &WazeroEngine{name: "wazero-prod", SkipUnsupported: true}
	prod.UseProdBlob()
	run("wasm-family", []Engine{
		NewInterp("interp"),
		&WasmEngine{name: "wasm", SkipUnsupported: true},
		&WazeroEngine{name: "wazero", SkipUnsupported: true},
	}, nil)
	if withOracle {
		run("oracle", []Engine{NewInterp("interp"), NewCLua55("clua55")}, oracleSkips)
	}
	run("prod", []Engine{NewInterp("interp"), prod}, prodBlobSkips)
}

// oracleSkipsFor: the interp↔clua55-only rulings per corpus (the maps
// the U2f oracle gates own; the exit-gate oracle column layers the
// same ones). nil = the corpus is oracle-green under the base skips.
func oracleSkipsFor(dir string) map[string]string {
	switch filepath.Base(dir) {
	case "_wasm-tests":
		return clua55MatrixSkips
	case "_wasm-err-tests":
		return clua55ErrOracleSkips
	default:
		return nil
	}
}

// wasmGluaFull runs the exit-gate engine set over a corpus and fails
// on any unruled divergence (M5: interp vs wasm; U6f: the wazero,
// clua55, and wazero-prod columns joined). withOracle=false where a
// standing oracle gate owns the clua55 column (see exitGateEngines).
func wasmGluaFull(t *testing.T, dir string, skips map[string]string, withOracle bool) {
	t.Helper()
	exitGateEngines(t, dir, skips, oracleSkipsFor(dir), withOracle)
}

// TestWasmGluaFull: the M5 milestone gate — full _glua-tests, the
// exit-gate engine set incl. the inline oracle column (this corpus has
// no dedicated oracle gate).
func TestWasmGluaFull(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy corpus — skipped in -short mode")
	}
	wasmGluaFull(t, "../_glua-tests", gluaWasmSkips, true)
}

// TestWasmFullMatrix: the regenerated opcode matrix stays green. The
// clua55 column is TestU2fMatrixOracle (this battery, same skips) —
// swept inline here it crossed the wasmtime Store budget (the M6e
// SIGTRAP cliff; see exitGateEngines).
func TestWasmFullMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy corpus — skipped in -short mode (the U6f prod pass pushed the short battery past go test's default 10m)")
	}
	wasmGluaFull(t, "../_wasm-tests", CorpusSkips("../_wasm-tests"), false)
	// (rows 10u/20/28 were unskipped with the M6 call_body rebasing;
	// the live skips — whlc00-03, nz00-01 — are the M6e rows 41/42,
	// owned by skips.go so the CLI leg and this gate cannot disagree)
}
