package testdiff

// U3 gates (docs/U3-implementation-plan.md): the 5.3/5.4 stdlib in the
// Go interpreter —
//   - TestU3Gate: the `_u3` corpus, interp ↔ clua55 byte-exact (the
//     per-chunk slices: math/xoshiro, string-metatable arithmetic, table
//     lib, utf8, string.format, string.pack, io/base/coroutine smalls).
//   - TestU3OfficialSuite: the milestone's official gate — strings.lua,
//     tpack.lua, utf8.lua, math.lua, bwcoercion.lua run in the suite's
//     own posture (_port/_soft/_nomsg) on BOTH engines and diffed; every
//     remaining skip carries a row.
//
// Corpus discipline (inherited from _u1/_u2): integral floats are never
// tostring'd or concated (float printing re-pins at U5 per D7); %p
// values never enter printed output (property asserts only); the
// varinfo-hint (row 62) and U5 parse-wording families stay out.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestU3Gate(t *testing.T) {
	cases, err := LoadCorpus("../_u3")
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

// u3OfficialSkips: interp↔clua55 rulings for the five official gate
// files. Each skip cites its row; the goal is for this map to be empty
// or nearly so at U3 close (the whole point of the gate).
var u3OfficialSkips = map[string]string{}

// TestU3OfficialSuite: the U3 milestone gate — the five official 5.5.1
// suite files green interp↔clua55.
func TestU3OfficialSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("official-suite legs — skipped in -short mode")
	}
	files := []string{"strings.lua", "tpack.lua", "utf8.lua", "math.lua", "bwcoercion.lua"}
	var cases []Case
	dir := "../_lua5.5-tests"
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		src, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if _, skip := u3OfficialSkips[f]; !skip {
			cases = append(cases, Case{Name: f, Dir: abs, Source: src})
		}
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
		t.Fatalf("%d/%d official files diverge between interp and clua55", bad, len(cases))
	}
}
