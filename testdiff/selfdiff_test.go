package testdiff

import "testing"

// The M1 gate (docs/Lua-Wasm-Design-and-Test-Plan.md §9): the interpreter
// diffed against a second interpreter instance must produce zero diffs
// across the corpus. This validates the oracle itself — event capture,
// normalization, globals serialization — before any second engine exists.
// A failure here means the harness is nondeterministic, not that Lua code
// is wrong.

// gluaSkips lists _glua-tests cases that cannot participate in the diff,
// each with a mandatory reason (§8.9: a skip without a reason is a bug).
var gluaSkips = map[string]string{}

// The _lua5.1-tests self-diff leg (TestSelfDiffLua51Tests) was retired
// with the corpus at U8 — the 5.1 oracle, its engine, and the vendored
// tree went together (ledger row 55's executed retirement).

func TestSelfDiffGluaTests(t *testing.T) {
	runSelfDiff(t, "../_glua-tests", gluaSkips)
}

func runSelfDiff(t *testing.T, dir string, skips map[string]string) {
	t.Helper()
	cases, err := LoadCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus %s: %v", dir, err)
	}
	cases, skipped := FilterSkips(cases, skips)
	results := RunCorpus(cases, []Engine{NewInterp("interp-a"), NewInterp("interp-b")})

	bad := 0
	for _, r := range results {
		if d := r.Diff(); d != "" {
			bad++
			t.Errorf("self-diff mismatch in %s:\n%s", r.Name, d)
		}
	}
	t.Log("\n" + Summary(results, skipped))
	if bad > 0 {
		t.Fatalf("oracle is nondeterministic: %d/%d cases differ between two interpreter instances", bad, len(cases))
	}
	if len(cases) == 0 {
		t.Fatal("empty corpus")
	}
}
