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
// 60), semerror texts position-stripped through load(). U5d adds the
// yield slice (coyield01–06, row 84): pcall/xpcall/pairs marker frames
// (yields suspend through them; create(pcall)/wrap flavors; the
// tailcall-into-marker slide), the noyield depth (the catchable
// boundary error through sort/gsub/__tostring callbacks, isyieldable),
// the pairs continuation marker + yielding for-iterators, kind-4/5
// close continuations, and the kind-6 metamethod battery. The
// handler-exhausts-the-reserve shape (row 78e's divergence: "stack
// overflow" vs "error in error handling") and the C-metamethod-yield
// shapes (row 84's divergence: stock lets a C-function metamethod yield
// when dispatched from Lua code; the fork raises the boundary error)
// are deliberately absent — coyield05.lua's header carries the excluded
// shapes.

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

// u5WasmLeg: interp vs one wasm engine over _u5 (row 85's gate — U5e
// brought the backend onto the 5.5 surface: the Stage A opcodes, the
// position seam, the hint registry, the msgh seam). Skips are the
// u5WasmSkips map (CorpusSkips) — every one engine-seam, none language.
func u5WasmLeg(t *testing.T, eng Engine) {
	t.Helper()
	cases, err := LoadCorpus("../_u5")
	if err != nil {
		t.Fatal(err)
	}
	cases, skipped := FilterSkips(cases, CorpusSkips("../_u5"))
	engines := []Engine{NewInterp("interp"), eng}
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
	t.Log("\n" + Summary(results, skipped))
	if bad > 0 {
		t.Fatalf("%d/%d cases diverge between interp and %s", bad, len(cases), engines[1].Name())
	}
}

// TestU5WasmGate: the _u5 corpus on the U2e backend over wasmtime.
func TestU5WasmGate(t *testing.T) {
	u5WasmLeg(t, NewWasmEngine("wasm"))
}

// TestU5WazeroGate: the same leg on the production engine.
func TestU5WazeroGate(t *testing.T) {
	u5WasmLeg(t, wazeroEngine())
}
