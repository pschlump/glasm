package testdiff

// U4 gates (docs/U4-implementation-plan.md): the 5.4 language core —
//   - TestU4Gate: the `_u4` corpus, interp ↔ clua55 byte-exact (the
//     per-chunk slices: <const>/<close> attributes, TBC normal + error
//     paths, the 5.4 goto/label rules, the __le-mirror-removal order
//     family).
//   - TestU4OfficialSuite: the milestone's official gate — attrib.lua,
//     events.lua, errors.lua, coroutine.lua run in the suite's own
//     posture (_port/_soft/_nomsg) on BOTH engines and diffed; the two
//     documented skips cite their rows (errors.lua → row 77's U5
//     light-udata block; coroutine.lua → row 22).
//
// Corpus discipline (inherited from _u1/_u3): level-1 errors only (row
// 60), no create(pcall)+yield shapes (row 22), no integral-float
// tostring, no %p, semerror texts position-stripped through load().

import (
	"os"
	"path/filepath"
	"testing"
)

func TestU4Gate(t *testing.T) {
	cases, err := LoadCorpus("../_u4")
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

// u4OfficialSkips: interp↔clua55 rulings for the U4 official gate
// files. Each skip cites its row; U5's gate (full suite ≥95%) un-skips.
var u4OfficialSkips = map[string]string{
	// errors.lua:262-268 — the light-udata block: debug.upvalueid must
	// return light userdata and setuservalue on it errors "light
	// userdata". The fork has no light userdata yet (U5, with the
	// sethook precedent); the U4-relevant error battery is mirrored in
	// _u4/tbc02.lua and the _wasm-err-tests additions instead (row 77).
	"errors.lua": "row 77: light userdata (debug.upvalueid/setuservalue, :262-268) is U5 — sethook precedent",
	// coroutine.lua:236-326 — the pcall-vs-close battery needs a yield
	// to unwind through a Go/G boundary frame and CONTINUE it on resume
	// (true continuation semantics); the fork's yield surgically returns
	// from the nested L.Call instead. The panic-shaped half of this seam
	// is fixed (row 77: the zombie-thread crash became a clean "cannot
	// resume dead coroutine"); the semantic half rides row 22 until U5.
	"coroutine.lua": "row 22: yield across the Go/G seam — pcall-vs-close continuation (:236-326)",
}

// TestU4OfficialSuite: the U4 milestone gate — the official 5.5.1
// attrib/events files green interp↔clua55, errors/coroutine carried as
// documented row-cited skips.
func TestU4OfficialSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("official-suite legs — skipped in -short mode")
	}
	files := []string{"attrib.lua", "events.lua", "errors.lua", "coroutine.lua"}
	var cases []Case
	dir := "../_lua5.5-tests"
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	skipped := 0
	for _, f := range files {
		src, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if _, skip := u4OfficialSkips[f]; skip {
			skipped++
			t.Logf("[skip] %s — %s", f, u4OfficialSkips[f])
			continue
		}
		cases = append(cases, Case{Name: f, Dir: abs, Source: src})
	}
	if len(cases) == 0 {
		t.Fatal("all official gate files skipped")
	}
	interp := NewInterp("interp")
	interp.SuiteFlags = true
	oracle := NewCLua55("clua55")
	oracle.SuiteFlags = true
	results := RunCorpus(cases, []Engine{interp, oracle})
	bad := 0
	for _, r := range results {
		if d := r.Diff(); d != "" {
			bad++
			t.Errorf("DIFF %s:\n%s", r.Name, indent(d))
		} else {
			t.Logf("[ok] %s", r.Name)
		}
	}
	if bad > 0 {
		t.Fatalf("%d/%d official files diverge between interp and clua55 (%d skipped, row-cited)", bad, len(cases), skipped)
	}
}
