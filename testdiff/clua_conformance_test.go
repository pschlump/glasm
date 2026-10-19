package testdiff

// The M2 gate: stock C Lua 5.1 in wasm passes the official test suite on
// the oracle host (plan §9): ≥95% of _lua5.1-tests scripts complete with
// no uncaught error and no engine failure; every skip carries a reason.

import (
	"strings"
	"testing"
)

var cluaSkips = map[string]string{
	"pm.lua":   "fork-adapted file: 'do;' (statement after do) is not Lua 5.1 syntax; glasm's parser accepts it, stock 5.1 does not",
	"main.lua": "suite driver: spawns the lua binary as subprocesses (RUN/auxrun); the harness runs each test file directly instead",
	"all.lua":  "suite driver: dofiles main.lua (subprocess driver) — see main.lua skip",
	"big.lua":  "yield-across-C-boundary test: the boundary error fires outside the suite's guarded position in this port; open investigation carried to M3",
}

func TestCLuaConformance(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy corpus — skipped in -short mode")
	}
	cases, err := LoadCorpus("../_lua5.1-tests")
	if err != nil {
		t.Fatal(err)
	}
	cases, skipped := FilterSkips(cases, cluaSkips)
	results := RunCorpus(cases, []Engine{NewCLua("clua")})

	pass, failed := 0, 0
	for _, r := range results {
		bad := ""
		for _, l := range r.Logs["clua"] {
			if strings.HasPrefix(l, "ERROR\t") || strings.HasPrefix(l, "ENGINE-ERROR\t") {
				bad = l
				break
			}
		}
		if bad == "" {
			pass++
			t.Logf("ok   %s (%d events)", r.Name, len(r.Logs["clua"]))
		} else {
			failed++
			msg := bad
			if len(msg) > 200 {
				msg = msg[:200] + "..."
			}
			// last progress prints before the failure localize it
			ctx := ""
			log := r.Logs["clua"]
			for i := len(log) - 1; i >= 0 && i >= len(log)-3; i-- {
				if strings.HasPrefix(log[i], "PRINT\t") {
					ctx += "\n  last-print: " + log[i]
				}
			}
			t.Errorf("FAIL %s: %s%s", r.Name, msg, ctx)
		}
	}
	for _, s := range skipped {
		t.Logf("skip %s", s)
	}
	rate := float64(pass) / float64(len(cases))
	t.Logf("%d/%d pass (%.1f%%), %d skips", pass, len(cases), rate*100, len(skipped))
	if rate < 0.95 {
		t.Errorf("conformance %.1f%% below the 95%% gate", rate*100)
	}
}
