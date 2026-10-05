package testdiff

// TEMPORARY U5 planning probe (deleted at U5f, when TestU5OfficialSuite
// takes over): run the whole official 5.5.1 suite interp↔clua55 and
// report per-file PASS/DIFF/SKIP so the U5 plan's chunk inventory is
// grounded in the real failure set.

import (
	"os"
	"path/filepath"
	"testing"
)

// u5ProbeSkips layers the interp-only rulings on clua55Skips (U5a
// closeout triage): closure/gc/gengc need real GC surface (row 79),
// db.lua needs the sethook family, and cstack.lua:29 is row 78e's
// handler-overflow dialect. U5d un-skipped nextvar.lua (the __pairs
// yield, :937) and coroutine.lua's yield battery (create(pcall) :277
// and the mm-yield shapes — green under the marker-frame machinery);
// coroutine.lua's remaining blocker is sethook :410, narrowed below.
// Oracle-side these all stay in TestCLua55Conformance (green).
var u5ProbeSkips = func() map[string]string {
	m := map[string]string{
		"closure.lua": "row 79: weak tables unimplemented — closure.lua:39 repeat-until-GC cannot terminate (hangs, not fails)",
		"gc.lua":      "row 79 (U5c): collectgarbage deep surface — the 5.4 \"param\" get/set protocol (gc.lua:31) and weak tables (__mode clearing) later in the file",
		"gengc.lua":   "row 79 (U5c): generational-GC surface — gengc.lua:122 needs a minor collection to clear a __mode=\"kv\" table's entries",
		"db.lua":      "debug.sethook/gethook family unimplemented (db.lua:16's gethook) — sethook precedent (row 77's errors.lua skip)",
		"locals.lua":  "db.lua precedent: debug.sethook at locals.lua:842 (the to-be-closed-in-coroutines block) — green through :841, including the U5b vararg surface (the old :315 named-vararg blocker is fixed)",
		"coroutine.lua": "db.lua precedent: debug.sethook at coroutine.lua:410 — green through :409 including the U5d yield battery (create(pcall) :277, the mm-yield shapes; the old row-22 blockers are fixed)",
		"cstack.lua":  "row 78e (U5c closeout): xpcall(loop, loop) at cstack.lua:29 — the handler itself overflows and the fork's frame-reserve dialect still re-raises 'C stack overflow' where stock says 'error in error handling'; the row-78e fix covers the failing-handler raise/panic shapes (calls.lua:168 is green)",
		"calls.lua":   "row 25 (U5c closeout): the __call-chain/errerr battery (:195-240, :168) is green; the remaining blocker is string.dump at :361 (binary chunks — the fork rejects it)",
		"errors.lua":  "row 25 (U5c closeout): light userdata (:258-268) is green; the remaining blocker is string.dump at :307 (binary chunks — the fork rejects it)",
	}
	for k, v := range clua55Skips {
		m[k] = v
	}
	return m
}()

func TestU5ProbeOfficialSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("official-suite legs — skipped in -short mode")
	}
	cases, err := LoadCorpus("../_lua5.5-tests")
	if err != nil {
		t.Fatal(err)
	}
	cases, skipped := FilterSkips(cases, u5ProbeSkips)
	interp := NewInterp("interp")
	interp.SuiteFlags = true
	oracle := NewCLua55("clua55")
	oracle.SuiteFlags = true
	results := RunCorpus(cases, []Engine{interp, oracle})
	pass := 0
	for _, r := range results {
		if d := r.Diff(); d != "" {
			head := d
			if len(head) > 300 {
				head = head[:300] + "..."
			}
			t.Errorf("DIFF %s:\n%s", r.Name, indent(head))
		} else {
			pass++
			t.Logf("[ok] %s", r.Name)
		}
	}
	for _, s := range skipped {
		t.Logf("[skip] %s", s)
	}
	rate := float64(pass) / float64(len(cases))
	t.Logf("%d/%d pass (%.1f%%)", pass, len(cases), rate*100)
	if rate < 0.95 {
		t.Logf("below the 95%% U5 gate (expected pre-U5)")
	}
}

// Probe a single file quickly: U5PROBE=goto.lua go test ./testdiff/ -run TestU5ProbeFile -v
func TestU5ProbeFile(t *testing.T) {
	file := "pm.lua"
	if v := os.Getenv("U5PROBE"); v != "" {
		file = v
	}
	if reason, skip := u5ProbeSkips[file]; skip {
		t.Skipf("%s is a skip (%s) — probe the oracle engine alone or pick a non-skipped file", file, reason)
	}
	src, err := os.ReadFile(filepath.Join("../_lua5.5-tests", file))
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs("../_lua5.5-tests")
	if err != nil {
		t.Fatal(err)
	}
	cases := []Case{{Name: file, Dir: abs, Source: src}}
	interp := NewInterp("interp")
	interp.SuiteFlags = true
	oracle := NewCLua55("clua55")
	oracle.SuiteFlags = true
	results := RunCorpus(cases, []Engine{interp, oracle})
	for _, r := range results {
		if d := r.Diff(); d != "" {
			t.Errorf("DIFF %s:\n%s", r.Name, indent(d))
		} else {
			t.Logf("[ok] %s", r.Name)
		}
	}
}
