package testdiff

// The _u6 corpus gate (U6c, ledger row 94): the sandbox + globals
// lockdown on the wasm backend. These are HOST-SURFACE cases — the
// interp oracle has no rt_sandbox/rt_protect_globals counterpart — so
// outputs are pinned absolutely per engine (the rows 36/37 rule), not
// diffed against an oracle leg. The heavier in-place pins (surface
// lists, the Redis texts over every write shape, both hosts) live in
// m6c_test.go; this corpus keeps the shapes runnable for the U6f gate
// expansion.

import (
	"strings"
	"testing"
)

func u6Prints(log []string) []string {
	var out []string
	for _, l := range log {
		if strings.HasPrefix(l, "PRINT\t") {
			out = append(out, l)
		}
	}
	return out
}

func TestU6SandboxCorpus(t *testing.T) {
	cases, err := LoadCorpus("../_u6")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("empty corpus")
	}
	want := map[string][]string{
		"sbx01.lua": {
			"PRINT\tsbx01\t0\ttable\ttable",
		},
		"sbx02.lua": {
			"PRINT\tsbx02\tAttempt to modify a readonly table",
			"PRINT\tsbx02\tAttempt to modify a readonly table",
			"PRINT\tsbx02\tAttempt to modify a readonly table",
			"PRINT\tsbx02\tAttempt to modify a readonly table",
			"PRINT\tsbx02\tAttempt to modify a readonly table",
		},
		"sbx03.lua": {
			"PRINT\tsbx03\tScript attempted to access nonexistent global variable 'nosuch_thing'",
			"PRINT\tsbx03\tScript attempted to access nonexistent global variable 'io'",
			"PRINT\tsbx03\tnil\tnil",
			"PRINT\tsbx03\ttrue\tfunction",
		},
		"sbx04.lua": {
			"PRINT\tsbx04\t2\t3\t1",
		},
	}
	engines := []Engine{
		&WasmEngine{name: "wasm-u6", Sandbox: true, Protect: true, SkipUnsupported: true},
		&WazeroEngine{name: "wazero-u6", Sandbox: true, Protect: true, SkipUnsupported: true},
	}
	for _, e := range engines {
		t.Run(e.Name(), func(t *testing.T) {
			for _, c := range cases {
				w, ok := want[c.Name]
				if !ok {
					t.Fatalf("%s: no pinned want (add it — absolute pins only)", c.Name)
				}
				log := e.Run(c)
				for _, l := range log {
					if strings.HasPrefix(l, "ENGINE-") {
						t.Fatalf("%s: engine error: %s", c.Name, l)
					}
				}
				got := u6Prints(log)
				if !equalLogs(got, w) {
					t.Errorf("%s: prints\n got  %v\n want %v", c.Name, got, w)
				}
			}
		})
	}
}
