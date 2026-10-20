package testdiff

// U9 gate (docs/U9-perf.md; ledger row 104): the `_u9` corpus pins the
// direct-dispatch call path — a non-tail OP_CALL/TFORCALL on one of the
// module's own compiled closures runs lua_dispatch in-module (probe ->
// rt_dd_enter -> wasm call -> rt_dd_ret/rt_dd_err), everything else
// declining to rt_call_addr. dd01: tbc closes across direct frames on
// both retire paths. dd02: the error() level walk across direct frames
// — every direct frame pushes a real CallInfo + sea entry (row 104), so
// where() resolves direct frames like trampoline ones (was to be a
// ruled skip; the stack-presence fix un-skipped it). dd03: vararg callees (extras below the
// window) + fixed-want truncation. dd04: deep pure-direct recursion
// caught by pcall with execution CONTINUING (the depth-counter heal
// detector) plus a mixed gsub/direct chain. dd05: trampoline (C-invoked
// callback) and direct levels interleaved — the dd_disp_save bracket.
// dd06: TFORCALL direct iterators incl. the load-bearing nil-pad exit
// (bare `return` from the iterator) and the next() decline contrast.
// dd07: multret off the direct arm into C calls/select/pack/paren
// truncation, and out of the main chunk into the RESULTS event.
//
// The deadline-mid-direct-call surface is a host/ test (the m6d dlCase
// mirror), not a corpus case — it needs a host deadline, not a script.

import (
	"strings"
	"testing"
	"time"
)

func TestU9Gate(t *testing.T) {
	cases, err := LoadCorpus("../_u9")
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

// TestU9WasmGate: the _u9 corpus over the exit-gate engine set (the
// TestU5WasmGate shape — interp + the three wasm-family columns, the
// clua55 oracle column covered whole by TestU9Gate above).
func TestU9WasmGate(t *testing.T) {
	exitGateEngines(t, "../_u9", CorpusSkips("../_u9"), nil, false)
}

// TestU9DeadlineMidDirect: the row-100 deadline poll must fire inside a
// direct-dispatch chain — every OP_CALL enters new blocks in the callee
// body, so the poll rides block transitions however deep the chain.
// The chains stay under RT_DD_MAX_DEPTH (the budget must NOT fire);
// a mid-chain raise unwinds through rt_dd_err's rt_close_err with the
// <close> vars of the dying frames (the TestM6dDeadlineKill assertions).
func TestU9DeadlineMidDirect(t *testing.T) {
	const d = 80 * time.Millisecond
	src := "local function f(n)\n" +
		"  if n <= 0 then return 0 end\n" +
		"  local c <close> = setmetatable({}, {__close = function() end})\n" +
		"  return 1 + f(n - 1)\n" +
		"end\n" +
		"local i = 0\n" +
		"while true do i = i + 1 + f(64) end\n"
	for _, tc := range []struct {
		name string
		e    Engine
	}{
		{"wasmtime", &WasmEngine{name: "w", Deadline: d}},
		{"wazero", &WazeroEngine{name: "z", Deadline: d}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, dt := dlCase(tc.e, "u9_deadline_direct.lua", src)
			if dt > d+2*time.Second {
				t.Errorf("kill took %v (deadline %v) — mid-chain poll failed", dt, d)
			}
			for _, l := range log {
				if strings.HasPrefix(l, "ENGINE-") {
					t.Fatalf("trap/engine error instead of deadline raise: %s", l)
				}
			}
			errs := errorLines(log)
			if len(errs) != 1 || errs[0] != `"context deadline exceeded"` {
				t.Errorf("errors = %v, want the bare deadline text", errs)
			}
			t.Logf("kill latency: %v total for %v deadline (incl. engine setup)", dt, d)
		})
	}
}
