package testdiff

// U5 gate (docs/U5-detailed-implementation-plan.md §3): the `_u5`
// corpus, interp ↔ clua55 byte-exact — U5a: global declarations (forms,
// voiding, read-only, ERRNNIL redefinition, chain visibility, the
// contextual-keyword ruling), RDKCTC promotion, goto and the collective
// '*' scope, generic-for 4th-value TBC, the debug close window
// (endpc = closepc+1), and the ERRORSTACKSIZE-analog frame reserve
// (ledger row 78). U5b: the vararg model — named reads/mutations, the
// 'n' validation battery, shadowing/capture, tailcalls across the
// hidden/table models, promotion, and the official idioms (row 81;
// var01–var09).
//
// TestU5OfficialSuite (≥95%, vararg/locals/goto fully green) lands at
// U5f with the remaining chunks; until then u5probe_test.go carries the
// per-file official runs.
//
// Corpus discipline (inherited from _u1/_u4): level-1 errors only (row
// 60), no create(pcall)+yield shapes (row 22), semerror texts
// position-stripped through load(). The handler-exhausts-the-reserve
// shape (row 78e's divergence: "stack overflow" vs "error in error
// handling") is deliberately absent.

import (
	"testing"
)

func TestU5Gate(t *testing.T) {
	cases, err := LoadCorpus("../_u5")
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
		} else {
			t.Logf("[ok] %s", r.Name)
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
