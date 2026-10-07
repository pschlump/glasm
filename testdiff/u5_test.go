package testdiff

// U5 gate (docs/U5-detailed-implementation-plan.md §3): the `_u5`
// corpus, interp ↔ clua55 byte-exact — U5a: global declarations (forms,
// voiding, read-only, ERRNNIL redefinition, chain visibility, the
// contextual-keyword ruling), RDKCTC promotion, goto and the collective
// '*' scope, generic-for 4th-value TBC, the debug close window
// (endpc = closepc+1), and the ERRORSTACKSIZE-analog frame reserve
// (ledger row 78). U5b: the vararg model — named reads/mutations, the
// 'n' validation battery, shadowing/capture, tailcalls across the
// hidden/table models, promotion, and the official idioms (row 81;
// var01–var09).
//
// TestU5OfficialSuite (U5f): the whole official 5.5.1 suite, interp ↔
// clua55 — ≥95% of the non-skipped files, with vararg/locals/goto (and
// code/calls/errors/coroutine/db) REQUIRED green. Skips are u5SuiteSkips
// (clua55Skips' driver/files entries plus the interp-only rows 78e/79);
// oracle-side the files all stay in TestCLua55Conformance.
//
// Corpus discipline (inherited from _u1/_u4): level-1 errors only (row
// 60), semerror texts position-stripped through load(). U5d adds the
// yield slice (coyield01–06, row 84): pcall/xpcall/pairs marker frames
// (yields suspend through them; create(pcall)/wrap flavors; the
// tailcall-into-marker slide), the noyield depth (the catchable
// boundary error through sort/gsub/__tostring callbacks, isyieldable),
// the pairs continuation marker + yielding for-iterators, kind-4/5
// close continuations, and the kind-6 metamethod battery. The
// handler-exhausts-the-reserve shape (row 78e's divergence: "stack
// overflow" vs "error in error handling") and the C-metamethod-yield
// shapes (row 84's divergence: stock lets a C-function metamethod yield
// when dispatched from Lua code; the fork raises the boundary error)
// are deliberately absent — coyield05.lua's header carries the excluded
// shapes.

import (
	"testing"
)

func TestU5Gate(t *testing.T) {
	cases, err := LoadCorpus("../_u5")
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

// u5WasmLeg: interp vs one wasm engine over _u5 (row 85's gate — U5e
// brought the backend onto the 5.5 surface: the Stage A opcodes, the
// position seam, the hint registry, the msgh seam). Skips are the
// u5WasmSkips map (CorpusSkips) — every one engine-seam, none language.
func u5WasmLeg(t *testing.T, eng Engine) {
	t.Helper()
	cases, err := LoadCorpus("../_u5")
	if err != nil {
		t.Fatal(err)
	}
	cases, skipped := FilterSkips(cases, CorpusSkips("../_u5"))
	engines := []Engine{NewInterp("interp"), eng}
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
	t.Log("\n" + Summary(results, skipped))
	if bad > 0 {
		t.Fatalf("%d/%d cases diverge between interp and %s", bad, len(cases), engines[1].Name())
	}
}

// TestU5WasmGate: the _u5 corpus on the U2e backend over wasmtime.
func TestU5WasmGate(t *testing.T) {
	u5WasmLeg(t, NewWasmEngine("wasm"))
}

// TestU5WazeroGate: the same leg on the production engine.
func TestU5WazeroGate(t *testing.T) {
	u5WasmLeg(t, wazeroEngine())
}

// u5SuiteSkips layers the interp-only rulings on clua55Skips (the U5a
// closeout triage, carried from the retired u5probe_test.go): closure/
// gc/gengc need real GC surface (row 79) — collectgarbage's deep
// surface, weak tables, generational mode; cstack.lua:29 is row 78e's
// handler-overflow dialect. db.lua went green at U5f (sethook family +
// string.dump + the finalizer battery) and is not skipped. Oracle-side
// all of these stay in TestCLua55Conformance.
var u5SuiteSkips = func() map[string]string {
	m := map[string]string{
		"closure.lua": "row 79: no automatic GC cycles — closure.lua:39's repeat-until-GC waits for an unsolicited collection the fork never runs (weak tables DO clear on collectgarbage('collect'), gc.go; hangs, not fails)",
		"gc.lua":      "row 79 (U5f): the full-collection weak walk (gc.go) is in — gc.lua:31's param battery and :309's weak block now pass — but the fine-grained cycle semantics diverge: :316 (a finalizable object's references keep weak entries past stock's atomic-step clearing — the finalizer asserts C.key==nil) and :340 (register residue above stock's ci->top keeps key-weak entries reachable)",
		"gengc.lua":   "row 79 (U5c): generational-GC surface — gengc.lua:122 needs a minor collection to clear a __mode=\"kv\" table's entries",
		"cstack.lua":  "row 78e (U5c closeout): xpcall(loop, loop) at cstack.lua:29 — the handler itself overflows and the fork's frame-reserve dialect still re-raises 'C stack overflow' where stock says 'error in error handling'; the row-78e fix covers the failing-handler raise/panic shapes (calls.lua:168 is green)",
	}
	for k, v := range clua55Skips {
		m[k] = v
	}
	return m
}()

// u5RequiredGreen are the official files the U5 exit gate (plan §5)
// names as required-green whatever the overall rate.
var u5RequiredGreen = []string{"vararg.lua", "locals.lua", "goto.lua", "code.lua", "calls.lua", "errors.lua", "coroutine.lua", "db.lua"}

// TestU5OfficialSuite: the U5 exit gate's official-suite leg — ≥95% of
// the non-skipped files interp↔clua55 byte-exact, with the required set
// (vararg/locals/goto/code/calls/errors/coroutine/db) green outright.
func TestU5OfficialSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("official-suite legs — skipped in -short mode")
	}
	cases, err := LoadCorpus("../_lua5.5-tests")
	if err != nil {
		t.Fatal(err)
	}
	cases, skipped := FilterSkips(cases, u5SuiteSkips)
	interp := NewInterp("interp")
	interp.SuiteFlags = true
	oracle := NewCLua55("clua55")
	oracle.SuiteFlags = true
	results := RunCorpus(cases, []Engine{interp, oracle})
	pass := 0
	skipSet := map[string]bool{}
	for _, s := range skipped {
		skipSet[s] = true
		t.Logf("[skip] %s", s)
	}
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
	rate := float64(pass) / float64(len(cases))
	t.Logf("%d/%d pass (%.1f%%)", pass, len(cases), rate*100)
	for _, req := range u5RequiredGreen {
		if skipSet[req] {
			t.Errorf("%s is a skip — the U5 exit gate requires it green", req)
		}
	}
	if rate < 0.95 {
		t.Fatalf("%.1f%% is below the 95%% U5 gate", rate*100)
	}
}
