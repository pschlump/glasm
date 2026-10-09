package testdiff

// M6d gates (m6 plan D3/D4, ledger rows 36-37): the per-VM allocation
// cap (rt_set_memlimit — clean pcall-catchable "not enough memory",
// never a trap) and the deadline (host control-block flag + back-edge
// polls → rt_deadline's "context deadline exceeded").
//
// The interp oracle HAS the deadline surface (SetContext +
// mainLoopWithContext raises ctx.Err() — the wording authority for
// rt_deadline's body) but NO OOM surface (Go's allocator cannot refuse;
// SetMx os.Exit(3)s — not catchable), so deadline texts are pinned
// against the interp body and OOM texts are pinned absolutely.

import (
	"strings"
	"testing"
	"time"
)

// dlCase runs src and returns (log, wall time) — the deadline itself is
// set on the engine before Run.
func dlCase(e Engine, name, src string) ([]string, time.Duration) {
	start := time.Now()
	log := e.Run(Case{Name: name, Dir: ".", Source: []byte(src)})
	return log, time.Since(start)
}

func errorLines(log []string) []string {
	var out []string
	for _, l := range log {
		if strings.HasPrefix(l, "ERROR\t") {
			out = append(out, strings.TrimPrefix(l, "ERROR\t"))
		}
	}
	return out
}

// TestM6dDeadlineKill: `while true do end` dies with the deadline error
// within deadline+ε on every engine — a normal error return, not a trap.
// ε is asserted against a generous hard bound (2s) and reported; the
// measured value lives in note/m6d-implemented.md (ledger row 37).
func TestM6dDeadlineKill(t *testing.T) {
	const d = 80 * time.Millisecond
	src := "local i=0\nwhile true do i=i+1 end\n"
	engines := []struct {
		name string
		e    Engine
	}{
		{"interp", &Interp{name: "interp-dl", Deadline: d}},
		{"wasmtime", &WasmEngine{name: "wasm-dl", Deadline: d}},
		{"wazero", &WazeroEngine{name: "wazero-dl", Deadline: d}},
		{"wazero-prod", func() *WazeroEngine {
			e := &WazeroEngine{name: "wazero-prod-dl", Deadline: d}
			e.UseProdBlob()
			return e
		}()},
	}
	for _, tc := range engines {
		t.Run(tc.name, func(t *testing.T) {
			log, dt := dlCase(tc.e, "m6d_deadline.lua", src)
			if dt > d+2*time.Second {
				t.Errorf("kill took %v (deadline %v) — watchdog/poll failed", dt, d)
			}
			for _, l := range log {
				if strings.HasPrefix(l, "ENGINE-") {
					t.Fatalf("trap/engine error instead of deadline raise: %s", l)
				}
			}
			errs := errorLines(log)
			if len(errs) != 1 {
				t.Fatalf("error lines = %v, want exactly the deadline error", errs)
			}
			// ledger row 37: body matches the interp oracle's
			// ctx.Err() text byte-for-byte; the interp adds a
			// chunk:line prefix (its raise rides an instruction
			// boundary), the wasm engines have no poll-site line.
			if tc.name == "interp" {
				if !strings.HasPrefix(errs[0], `"m6d_deadline.lua:2: context deadline exceeded`) {
					t.Errorf("interp deadline error = %s, want the prefixed ctx text", errs[0])
				}
			} else if errs[0] != `"context deadline exceeded"` {
				t.Errorf("deadline error = %s, want bare \"context deadline exceeded\"", errs[0])
			}
			t.Logf("kill latency: %v total for %v deadline (incl. engine setup)", dt, d)
		})
	}
}

// TestM6dDeadlineTailSpin: an infinite tail-call chain never touches a
// basic block — the lua_dispatch restage-loop poll is what kills it.
func TestM6dDeadlineTailSpin(t *testing.T) {
	const d = 80 * time.Millisecond
	src := "local function f() return f() end\nf()\n"
	for _, tc := range []struct {
		name string
		e    Engine
	}{
		{"wasmtime", &WasmEngine{name: "w", Deadline: d}},
		{"wazero", &WazeroEngine{name: "z", Deadline: d}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, dt := dlCase(tc.e, "m6d_tailspin.lua", src)
			if dt > d+2*time.Second {
				t.Errorf("tail spin ran %v (deadline %v) — restage poll failed", dt, d)
			}
			errs := errorLines(log)
			if len(errs) != 1 || errs[0] != `"context deadline exceeded"` {
				t.Errorf("errors = %v, want the deadline error", errs)
			}
		})
	}
}

// TestM6dDeadlineCatchable: the deadline raise is an ordinary Lua error
// inside the script — pcall catches it, the script continues to normal
// completion, and a later back edge re-raises (the flag is sticky: no
// livelock, no progress past the deadline).
func TestM6dDeadlineCatchable(t *testing.T) {
	const d = 60 * time.Millisecond
	catch := `
local ok, err = pcall(function() local i=0 while true do i=i+1 end end)
print("caught", ok, err)
print("after")
`
	reraise := `
local ok, err = pcall(function() local i=0 while true do i=i+1 end end)
print("caught", ok, err)
local i=0 while true do i=i+1 end
`
	for _, tc := range []struct {
		name string
		e    Engine
	}{
		{"wasmtime", &WasmEngine{name: "w", Deadline: d}},
		{"wazero", &WazeroEngine{name: "z", Deadline: d}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, _ := dlCase(tc.e, "m6d_catch.lua", catch)
			if errs := errorLines(log); len(errs) != 0 {
				t.Errorf("catch leg: unexpected error lines %v", errs)
			}
			want := []string{
				"PRINT\tcaught\tfalse\tcontext deadline exceeded",
				"PRINT\tafter",
			}
			var prints []string
			for _, l := range log {
				if strings.HasPrefix(l, "PRINT\t") {
					prints = append(prints, l)
				}
			}
			if len(prints) != 2 || prints[0] != want[0] || prints[1] != want[1] {
				t.Errorf("catch prints = %v, want %v", prints, want)
			}

			log, _ = dlCase(tc.e, "m6d_reraise.lua", reraise)
			errs := errorLines(log)
			if len(errs) != 1 || errs[0] != `"context deadline exceeded"` {
				t.Errorf("reraise errors = %v, want the deadline error again", errs)
			}
		})
	}
}

// TestM6dDeadlineUnfired: an armed deadline that never expires leaves
// the run byte-identical to an unarmed one (the flag stays 0; the poll
// is pure overhead on the happy path).
func TestM6dDeadlineUnfired(t *testing.T) {
	src := "local s=0 for i=1,100000 do s=s+i end print(s)\n"
	for _, e := range []Engine{
		&WasmEngine{name: "w", Deadline: time.Hour},
		&WazeroEngine{name: "z", Deadline: time.Hour},
	} {
		log := e.Run(Case{Name: "m6d_unfired.lua", Dir: ".", Source: []byte(src)})
		var prints []string
		for _, l := range log {
			if strings.HasPrefix(l, "PRINT\t") {
				prints = append(prints, l)
			}
			if strings.HasPrefix(l, "ENGINE-") {
				t.Fatalf("engine error: %s", l)
			}
		}
		if len(prints) != 1 || prints[0] != "PRINT\t5000050000" {
			t.Errorf("%s prints = %v", e.Name(), prints)
		}
	}
}

// oomEngines: the cap configurations under test — both hosts, plus the
// production blob (dev-wasmtime, dev-wazero, prod-wazero).
func oomEngines(cap int64) []struct {
	name string
	e    Engine
} {
	return []struct {
		name string
		e    Engine
	}{
		{"wasmtime", &WasmEngine{name: "w", MaxMem: cap}},
		{"wazero", &WazeroEngine{name: "z", MaxMem: cap}},
		{"wazero-prod", func() *WazeroEngine {
			e := &WazeroEngine{name: "zp", MaxMem: cap}
			e.UseProdBlob()
			return e
		}()},
	}
}

// TestM6dMemCap: an alloc-heavy loop dies with stock 5.1's bare
// "not enough memory" (ledger row 36) — a clean error, not a trap, and
// bounded in time. The interp cannot produce this surface (Go's
// allocator cannot refuse), so the text is pinned absolutely.
func TestM6dMemCap(t *testing.T) {
	src := "local t={} for i=1,1000000 do t[i]=string.rep(\"x\",4096) end print(\"done\",#t)\n"
	for _, tc := range oomEngines(512 * 1024) {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			log := tc.e.Run(Case{Name: "m6d_oom.lua", Dir: ".", Source: []byte(src)})
			dt := time.Since(start)
			if dt > 10*time.Second {
				t.Errorf("OOM kill took %v — cap not enforced", dt)
			}
			for _, l := range log {
				if strings.HasPrefix(l, "ENGINE-") {
					t.Fatalf("trap/engine error instead of OOM raise: %s", l)
				}
			}
			errs := errorLines(log)
			if len(errs) != 1 || errs[0] != `"not enough memory"` {
				t.Errorf("errors = %v, want exactly \"not enough memory\"", errs)
			}
			for _, l := range log {
				if strings.HasPrefix(l, "PRINT\t") {
					t.Errorf("script completed past the cap: %s", l)
				}
			}
		})
	}
}

// TestM6dMemCapCatchable: pcall catches the OOM raise and the script
// keeps running (the emergency slack lets the error machinery finish;
// the catch re-arms the refusal — ledger row 36).
func TestM6dMemCapCatchable(t *testing.T) {
	src := `
local t = {}
local ok, err = pcall(function() for i=1,1000000 do t[i] = string.rep("x", 4096) end end)
print("oom-caught", ok, err)
print("still alive", type(string.rep("y", 3)))
`
	for _, tc := range oomEngines(512 * 1024) {
		t.Run(tc.name, func(t *testing.T) {
			log := tc.e.Run(Case{Name: "m6d_oomcatch.lua", Dir: ".", Source: []byte(src)})
			var prints []string
			for _, l := range log {
				if strings.HasPrefix(l, "PRINT\t") {
					prints = append(prints, l)
				}
				if strings.HasPrefix(l, "ENGINE-") {
					t.Fatalf("trap/engine error: %s", l)
				}
			}
			want := []string{
				"PRINT\toom-caught\tfalse\tnot enough memory",
				"PRINT\tstill alive\tstring",
			}
			if len(prints) != 2 || prints[0] != want[0] || prints[1] != want[1] {
				t.Errorf("prints = %v, want %v", prints, want)
			}
		})
	}
}

// TestM6dMemCapSingleCall: one C call allocating past the cap — no back
// edge ever polls, the refusal inside the library call is the only
// defense. Proves a single non-returning C path is bounded by the cap
// (the deadline's documented blind spot).
func TestM6dMemCapSingleCall(t *testing.T) {
	src := "print(#string.rep(\"x\", 512*1024*1024))\n"
	for _, tc := range oomEngines(2 * 1024 * 1024) {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			log := tc.e.Run(Case{Name: "m6d_bigone.lua", Dir: ".", Source: []byte(src)})
			if dt := time.Since(start); dt > 10*time.Second {
				t.Errorf("single-call OOM took %v — cap not enforced", dt)
			}
			errs := errorLines(log)
			if len(errs) != 1 || errs[0] != `"not enough memory"` {
				t.Errorf("errors = %v, want \"not enough memory\"", errs)
			}
		})
	}
}
