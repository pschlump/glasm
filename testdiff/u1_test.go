package testdiff

// The U1 gate (docs/Upgrade-from-5.2-to-5.5-of-Lua.md §6): the 5.2
// language core in Go — _ENV (D3), \x/\z escapes + hex-float literals +
// escape/parse-error texts, break mid-block, table __len, xpcall args,
// load/loadfile mode+env, the 5.2 library deltas, and the D8 removals —
// diffed byte-exact against the stock-5.5.1 oracle (clua55, decision D6).
//
// Corpus discipline (documented blindness, row 3's precedent):
//   - integral floats are never printed directly (interp prints "3" where
//     5.5 prints "3.0" — float formatting re-pins at U5 per D7); the
//     cases assert equality instead;
//   - uncaught runtime errors with position prefixes are avoided — the
//     interp attributes error positions to the statement's line, stock
//     to the following instruction (ledger row: U1); parse errors caught
//     via load() ARE compared byte-exact (both engines say line 1);
//   - os.execute behavior is environment-dependent on the oracle (the
//     5.5 driver runs it for real) — only its presence/type is tested;
//   - arg/vararg-compat (CompatVarArg) stays until the U5 vararg rework,
//     so `arg` is untested.

import "testing"

func TestU1Gate(t *testing.T) {
	cases, err := LoadCorpus("../_u1")
	if err != nil {
		t.Fatal(err)
	}
	engines := []Engine{NewInterp("interp"), NewCLua55("clua55")}
	results := RunCorpus(cases, engines)
	bad := 0
	for _, r := range results {
		if d := r.Diff(); d != "" {
			bad++
			t.Errorf("DIFF %s:\n%s", r.Name, indent(d))
		}
	}
	if len(cases) == 0 {
		t.Fatal("no cases loaded")
	}
	t.Log("\n" + Summary(results, nil))
	if bad > 0 {
		t.Fatalf("%d/%d cases diverge between interp and clua55", bad, len(cases))
	}
}
