package testdiff

// The U2a gate (docs/U2-implementation-plan.md §4): the integer value
// model (D1) in the Go interpreter, diffed byte-exact against the
// stock-5.5.1 oracle (clua55, decision D6). Slice A covers the
// subtype-visible surfaces that exist before the U2b operator work:
// literal subtype, tonumber/tostring coercion, table-key normalization,
// mixed int/float comparison, and value-model plumbing through the VM
// (arithmetic still evaluates through the float path at this stage —
// every case asserts values that are identical either way).
//
// Corpus discipline (inherited from _u1):
//   - integral FLOATS are never tostring'd or concated (float formatting
//     re-pins at U5 per D7; interp prints "3", stock prints "3.0");
//     direct print() of numbers is fine — both engines canonicalize to
//     %.14g at the event boundary;
//   - uncaught position-bearing runtime errors are avoided on multi-line
//     statements (ledger row: U1); single-line pcall probes compare fine;
//   - integer results print exactly on every engine.

import "testing"

func TestU2Gate(t *testing.T) {
	cases, err := LoadCorpus("../_u2")
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
