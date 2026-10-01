package testdiff

// TEMPORARY U5 planning probe (deleted at U5a): run the whole official
// 5.5.1 suite interp↔clua55 and report per-file PASS/DIFF/SKIP so the
// U5 plan's chunk inventory is grounded in the real failure set.

import (
	"os"
	"path/filepath"
	"testing"
)

// u5ProbeSkips layers the interp-only rulings on clua55Skips (U5a
// closeout triage): closure/gc/gengc need real GC surface (row 79),
// nextvar's yield-in-__pairs is the U5d row-22 seam, db.lua needs the
// sethook family, and cstack.lua:29 is row 78e's handler-overflow
// dialect. Oracle-side these all stay in TestCLua55Conformance (green).
var u5ProbeSkips = func() map[string]string {
	m := map[string]string{
		"closure.lua":   "row 79: weak tables unimplemented — closure.lua:39 repeat-until-GC cannot terminate (hangs, not fails)",
		"nextvar.lua":   "row 22 (U5d): yield inside __pairs (nextvar.lua:937) crosses the pairs() Go-function boundary and crashes the coroutine machinery — plain __pairs/__index ipairs are fixed; yields are U5d",
		"gc.lua":        "row 79 (U5c): collectgarbage deep surface — the 5.4 \"param\" get/set protocol (gc.lua:31) and weak tables (__mode clearing) later in the file",
		"gengc.lua":     "row 79 (U5c): generational-GC surface — gengc.lua:122 needs a minor collection to clear a __mode=\"kv\" table's entries",
		"db.lua":        "debug.sethook/gethook family unimplemented (db.lua:16's gethook) — sethook precedent (row 77's errors.lua skip)",
		"cstack.lua":    "row 78e: xpcall(loop, loop) at cstack.lua:29 — the handler itself overflows; the fork's frame reserve re-raises 'stack overflow' where stock says 'error in error handling'",
		"calls.lua":     "row 78e: xpcall(loop, loop) at calls.lua:168 — the handler itself overflows; same dialect as cstack.lua:29 (fork re-raises 'stack overflow', stock says 'error in error handling')",
		"goto.lua":      "row 77 precedent (U5c): debug.upvalueid at goto.lua:197 needs light userdata — same family as the errors.lua upvalueid skip",
		"errors.lua":    "row 63 (U5b): named vararg — errors.lua:163's checkmessage runs 'local function foo (...t) return t.xx + 1' before anything else diverges (upvalueid at :262 is the U5c blocker behind it)",
		"vararg.lua":    "row 63 (U5b): the vararg model rework — vararg.lua:5's 'local function f (a, ...t)' binds a placeholder local (GETVARG/opcode 81 is the U5b core); this file is the U5b gate",
		"locals.lua":    "row 63 (U5b): named vararg — locals.lua:315's func2close(function (...t)) battery (the <close> function receives the vararg table)",
		"coroutine.lua": "row 22 (U5d): yield inside a Go-function boundary — coroutine.create(pcall) + foo yields (coroutine.lua:277): resume returns the yield but the status reads 'dead' instead of 'suspended' (same seam as nextvar.lua:937's __pairs yield)",
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
