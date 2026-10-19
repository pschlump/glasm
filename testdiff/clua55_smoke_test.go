package testdiff

import "testing"

// U0 bring-up: both hosts run the same lua55_sjlj.wasm and must produce
// identical event logs on a 5.5-shaped script (integers, //, bitwise,
// goto, <close> — constructs the 5.1 engines cannot even parse).
func TestCLua55Smoke(t *testing.T) {
	src := []byte(`local t = {x=1, y='two'}
print('hello', 1+2, true, t)
print(3 // 2, 7 & 3, 1 << 10, 2^53 + 1 == 2^53 + 0.0)
do
  local x <close> = setmetatable({}, {__close=function() print('closed') end})
  goto done
  print('unreached')
end
::done::
zz = 42
`)
	// temp dir: clua55Prep provisions its dev/null stand-in in the case
	// dir — keep that scratch out of the package tree.
	c := Case{Name: "smoke55.lua", Dir: t.TempDir(), Source: src}
	want := []string{
		"PRINT\thello\t3\ttrue\t<table>",
		// 2^53 is a float; +1 rounds back to 2^53, so == holds (5.3+
		// float arithmetic — the classic integer-subtype trap this oracle
		// now pins down)
		"PRINT\t1\t3\t1024\ttrue",
		"PRINT\tclosed",
	}
	for _, e := range []Engine{NewCLua55("clua55-smoke"), NewCLua55Wazero("clua55-wazero-smoke")} {
		log := e.Run(c)
		if len(log) != 5 { // 3 prints + RESULTS (U9a2) + GLOBALS
			t.Fatalf("%s: expected 3 PRINT + RESULTS + GLOBALS events, got %d: %q", e.Name(), len(log), log)
		}
		// GLOBALS lands at index 3 (emitted DURING ldostring via
		// want_globals=1) and RESULTS after the pull — index 4.
		if log[4] != "RESULTS\t0" {
			t.Errorf("%s: RESULTS mismatch: got %q", e.Name(), log[4])
		}
		for i, w := range want {
			if log[i] != w {
				t.Errorf("%s: event %d mismatch:\n got %q\nwant %q", e.Name(), i, log[i], w)
			}
		}
	}
}
