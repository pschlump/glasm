package testdiff

// The U0 gate (docs/Upgrade-from-5.2-to-5.5-of-Lua.md §6): stock C Lua
// 5.5.1 in wasm passes the official 5.5.1 suite on BOTH engine hosts —
// wasmtime (clua55) and wazero (clua55-wazero): ≥95% of _lua5.5-tests
// scripts complete with no uncaught error and no engine failure; every
// skip carries a reason. From U0 on, this oracle is the semantic authority
// (decision D6); the 5.1 gates (TestCLuaConformance & co.) stay green and
// unchanged until their milestone intentionally retires them (U8).

import (
	"strings"
	"testing"
)

var clua55Skips = map[string]string{
	"main.lua":  "suite driver: spawns the lua binary as subprocesses (RUN/auxrun); the harness runs each test file directly instead",
	"all.lua":   "suite driver: dofiles main.lua and every file in sequence (subprocess driver) — see main.lua skip",
	"heavy.lua": "stress script excluded from the official all.lua run; creates multi-hundred-MB strings that cannot fit the 256 MiB wasm memory backstop",
	"files.lua": "the /dev/full block (files.lua:474-482) asserts io.flush FAILS with ENOSPC: no WASI host here can express a write-failing device (macOS has no /dev/full to preopen, wasmtime-go's built-in WASI has no per-call hook, and a regular-file stand-in succeeds like /dev/null). Everything through line 470 is green — plain file I/O, io.lines, seeks, rename/remove, tmpname/tmpfile. Revisit if a host grows an ENOSPC-capable FS adapter (wazero could, asymmetrically).",
}

func TestCLua55Conformance(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy corpus — skipped in -short mode")
	}
	cases, err := LoadCorpus("../_lua5.5-tests")
	if err != nil {
		t.Fatal(err)
	}
	cases, skipped := FilterSkips(cases, clua55Skips)
	wasmtimeEng := NewCLua55("clua55")
	wasmtimeEng.SuiteFlags = true
	wazeroEng := NewCLua55Wazero("clua55-wazero")
	wazeroEng.SuiteFlags = true
	engines := []Engine{wasmtimeEng, wazeroEng}
	results := RunCorpus(cases, engines)

	// One blob, two hosts (ledger row 32 posture): every case's event log
	// must be byte-identical across wasmtime and wazero (DiffLogs excludes
	// GLOBALS lines and traceback tails by the cross-engine contract). A
	// mismatch is a host bug, not a Lua-semantics finding.
	for _, r := range results {
		if d := r.Diff(); d != "" {
			t.Errorf("HOST-DIVERGE %s:\n%s", r.Name, indent(d))
		}
	}

	for _, eng := range engines {
		name := eng.Name()
		pass, failed := 0, 0
		for _, r := range results {
			bad := clua55Bad(r.Logs[name])
			if bad == "" {
				pass++
				continue
			}
			failed++
			msg := bad
			if len(msg) > 200 {
				msg = msg[:200] + "..."
			}
			// last progress prints before the failure localize it
			ctx := ""
			log := r.Logs[name]
			for i := len(log) - 1; i >= 0 && i >= len(log)-3; i-- {
				if strings.HasPrefix(log[i], "PRINT\t") {
					ctx += "\n  last-print: " + log[i]
				}
			}
			t.Errorf("[%s] FAIL %s: %s%s", name, r.Name, msg, ctx)
		}
		for _, s := range skipped {
			t.Logf("[%s] skip %s", name, s)
		}
		rate := float64(pass) / float64(len(cases))
		t.Logf("[%s] %d/%d pass (%.1f%%), %d skips", name, pass, len(cases), rate*100, len(skipped))
		if rate < 0.95 {
			t.Errorf("[%s] conformance %.1f%% below the 95%% gate", name, rate*100)
		}
	}
}
