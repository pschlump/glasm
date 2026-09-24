package luafuzz

// M6e gates (m6 plan §M6e):
//
//   - generator determinism + validity-by-construction (every case
//     parses through the repo frontend)
//   - chunk-resume integrity: interrupt anywhere, resume, no case is
//     double-counted or lost; a journal tail past the last snapshot is
//     replayed
//   - night accounting: date buckets, qualification, clean streak
//   - classifier: each §8.7 class from synthetic logs
//   - minimizer: noise removal while the finding persists
//   - the harness self-diff (interp vs interp — the M1-style oracle
//     check) and a short-skippable interp-vs-wasm leg.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pschlump/golua55/testdiff"
)

// ---- generator ----

func TestGenerateDeterministicAndValid(t *testing.T) {
	const seed = int64(20260917)
	for idx := 0; idx < 300; idx++ {
		a := Generate(seed, idx)
		b := Generate(seed, idx)
		if string(a) != string(b) {
			t.Fatalf("case %d: Generate not a pure function", idx)
		}
		if n := strings.Count(string(a), "\n"); n > 120 {
			t.Fatalf("case %d: %d lines (cap 120)", idx, n)
		}
		if err := ValidSource(a, CaseName(idx)); err != nil {
			t.Fatalf("case %d does not parse (GENBUG): %v\n%s", idx, err, a)
		}
	}
}

func TestGenerateDistinct(t *testing.T) {
	seen := map[string]int{}
	for idx := 0; idx < 500; idx++ {
		s := string(Generate(1, idx))
		if prev, ok := seen[s]; ok {
			t.Fatalf("cases %d and %d identical", prev, idx)
		}
		seen[s] = idx
	}
}

// ---- classifier ----

type stubEngine struct {
	name string
	log  []string
}

func (e *stubEngine) Name() string                 { return e.name }
func (e *stubEngine) Run(c testdiff.Case) []string { return e.log }

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		logs [][]string
		want Class
	}{
		{"agree", [][]string{{"PRINT\tx"}, {"PRINT\tx"}}, ClassPass},
		{"diverge", [][]string{{"PRINT\tx"}, {"PRINT\ty"}}, ClassDiverge},
		{"trap", [][]string{{"PRINT\tx"}, {`ENGINE-ERROR	trap: oob`}}, ClassTrap},
		{"engine-err", [][]string{{"PRINT\tx"}, {`ENGINE-ERROR	init: boom`}}, ClassEngine},
		{"panic", [][]string{{"PRINT\tx"}, {`ENGINE-PANIC	runtime error`}}, ClassPanic},
		{"skip", [][]string{{"PRINT\tx"}, {`SKIP-UNSUPPORTED	backend v1`}}, ClassSkip},
		{"dl-both", [][]string{
			{`ERROR	"c1.lua:3: context deadline exceeded"`},
			{`ERROR	"context deadline exceeded"`}}, ClassDLTie},
		{"dl-one", [][]string{
			{`ERROR	"c1.lua:3: context deadline exceeded"`},
			{"PRINT\tdone"}}, ClassHang},
		{"globals-ignored", [][]string{
			{"PRINT\tx", "GLOBALS\t{a=1}"},
			{"PRINT\tx", "GLOBALS\t{a=2}"}}, ClassPass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engines := []testdiff.Engine{&stubEngine{"a", tc.logs[0]}, &stubEngine{"b", tc.logs[1]}}
			got := RunCase(engines, t.TempDir(), 1, []byte("return 1\n"))
			if got.Class != tc.want {
				t.Fatalf("class = %s (%s), want %s", got.Class, got.Detail, tc.want)
			}
			if (tc.want == ClassPass || tc.want == ClassDLTie) && got.Sig != "" {
				t.Fatalf("neutral class carried a signature: %q", got.Sig)
			}
		})
	}
}

// ---- soak resume integrity ----

// echoEngine: PASS for every case whose source contains PASS; TRAP
// when it contains TRAP — deterministic per-source, so replays agree.
type echoEngine struct {
	name string
}

func (e *echoEngine) Name() string { return e.name }
func (e *echoEngine) Run(c testdiff.Case) []string {
	if strings.Contains(string(c.Source), "TRAPMARK") {
		return []string{`ENGINE-ERROR	trap: synthetic`}
	}
	return []string{"PRINT\tok"}
}

func newTestOptions(t *testing.T, dir string) Options {
	t.Helper()
	return Options{
		Dir: dir, Engines: []string{"stub", "stub"},
		Deadline: 2 * time.Second, Checkpoint: 5 * time.Second,
		NightMinWall: time.Nanosecond, // every test night qualifies
	}
}

func TestSoakResumeIntegrity(t *testing.T) {
	dir := t.TempDir()
	o := newTestOptions(t, dir)
	o.MaxCases = 7
	engines := []testdiff.Engine{&echoEngine{"a"}, &echoEngine{"b"}}

	rep, err := RunChunk(context.Background(), o, engines)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cases != 7 {
		t.Fatalf("chunk 1 ran %d cases, want 7", rep.Cases)
	}
	st, err := LoadState(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if st.NextCase != 7 || st.Totals.Cases != 7 || st.Totals.Execs != 14 {
		t.Fatalf("after chunk 1: next=%d cases=%d execs=%d", st.NextCase, st.Totals.Cases, st.Totals.Execs)
	}

	// second chunk resumes at 7
	o.MaxCases = 5
	rep, err = RunChunk(context.Background(), o, engines)
	if err != nil {
		t.Fatal(err)
	}
	if rep.LastIdx != 11 {
		t.Fatalf("chunk 2 last idx = %d, want 11", rep.LastIdx)
	}
	st, err = LoadState(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if st.NextCase != 12 || st.Totals.Cases != 12 || st.Totals.Execs != 24 {
		t.Fatalf("after chunk 2: next=%d cases=%d execs=%d", st.NextCase, st.Totals.Cases, st.Totals.Execs)
	}
	if len(st.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2", len(st.Sessions))
	}
	// no case index appears twice across journals
	seen := map[int]bool{}
	paths, _ := filepath.Glob(filepath.Join(dir, "chunks", "*.jsonl"))
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			var l journalLine
			if json.Unmarshal([]byte(line), &l) != nil {
				t.Fatalf("bad journal line %q", line)
			}
			if seen[l.I] {
				t.Fatalf("case %d journaled twice", l.I)
			}
			seen[l.I] = true
		}
	}
}

func TestJournalReplayPastSnapshot(t *testing.T) {
	dir := t.TempDir()
	o := newTestOptions(t, dir)
	o.MaxCases = 4
	engines := []testdiff.Engine{&echoEngine{"a"}, &echoEngine{"b"}}
	if _, err := RunChunk(context.Background(), o, engines); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if st.NextCase != 4 {
		t.Fatalf("setup: next=%d, want 4", st.NextCase)
	}

	// simulate a crash 3 cases into the next chunk, before any
	// snapshot: the chunk journal holds idx 4..6, soak.json still says
	// next=4 (exactly the state a kill -9 between checkpoints leaves)
	cw, err := openChunk(dir, 99, 4)
	if err != nil {
		t.Fatal(err)
	}
	for i := 4; i < 7; i++ {
		if err := cw.append(journalLine{I: i, Cls: "PASS", Ms: 1, X: 2, T: nowISO(time.Now())}, true); err != nil {
			t.Fatal(err)
		}
	}
	cw.close()

	st, err = LoadState(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if st.NextCase != 7 {
		t.Fatalf("replay recovered next=%d, want 7", st.NextCase)
	}
	if st.Totals.Cases != 7 || st.Totals.Execs != 14 {
		t.Fatalf("replay totals = %d/%d, want 7/14", st.Totals.Cases, st.Totals.Execs)
	}
}

// ---- night accounting ----

func TestNightStreak(t *testing.T) {
	mk := func(finding bool) NightInfo {
		n := NightInfo{Cases: 10, WallMs: float64(time.Hour / time.Millisecond)}
		if finding {
			n.Findings, n.NewSigs = 1, 1
		}
		return n
	}

	// scenario A: sub-minimal attempt today (lunch chunk), clean nights
	// before it — skipped, doesn't break
	s := &State{Nights: map[string]NightInfo{
		"2026-09-11": mk(false), "2026-09-12": mk(false),
		"2026-09-14": mk(false), "2026-09-15": mk(false), "2026-09-16": mk(false),
		"2026-09-17": {Cases: 2, WallMs: 1000},
	}}
	_, streak := NightReport(s, 30*time.Minute)
	if streak != 5 {
		t.Fatalf("scenario A: streak = %d, want 5", streak)
	}

	// scenario B: a finding day resets — streak 0 even with a clean today
	s = &State{Nights: map[string]NightInfo{
		"2026-09-15": mk(false), "2026-09-16": mk(true), "2026-09-17": mk(false),
	}}
	_, streak = NightReport(s, 30*time.Minute)
	if streak != 1 {
		t.Fatalf("scenario B: streak = %d, want 1 (only today counts — 16th resets)", streak)
	}

	// scenario C: sub-minimal night WITH a finding still resets
	s = &State{Nights: map[string]NightInfo{
		"2026-09-15": mk(false), "2026-09-17": mk(false),
		"2026-09-16": {Cases: 2, WallMs: 1000, Findings: 1, NewSigs: 1},
	}}
	_, streak = NightReport(s, 30*time.Minute)
	if streak != 1 {
		t.Fatalf("scenario C: streak = %d, want 1", streak)
	}
}

func TestNightBucketsFromRun(t *testing.T) {
	dir := t.TempDir()
	o := newTestOptions(t, dir)
	o.MaxCases = 3
	now := time.Date(2026, 9, 17, 10, 0, 0, 0, time.Local)
	o.Now = func() time.Time { return now }
	if _, err := RunChunk(context.Background(), o, []testdiff.Engine{&echoEngine{"a"}, &echoEngine{"b"}}); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	n, ok := st.Nights[now.Format("2006-01-02")]
	if !ok || n.Cases != 3 {
		t.Fatalf("night bucket for today = %+v (ok=%v)", n, ok)
	}
}

// ---- minimizer ----

// markEngine pair: a and b agree exactly when the source lacks MARK.
type markEngine struct {
	name  string
	markL string
}

func (e *markEngine) Name() string { return e.name }
func (e *markEngine) Run(c testdiff.Case) []string {
	if strings.Contains(string(c.Source), "MARK") {
		return []string{"PRINT\t" + e.markL}
	}
	return []string{"PRINT\tb"}
}

func TestMinimize(t *testing.T) {
	noise := strings.Repeat("print(\"noise\")\n", 6)
	src := noise + "print(\"MARK\")\n" + noise
	// the pair diverges only on MARK
	engines := []testdiff.Engine{&markEngine{"a", "a"}, &markEngine{"b", "c"}}
	min, ok, runs := Minimize([]byte(src), engines, t.TempDir(), 1, 200)
	if !ok {
		t.Fatal("finding did not reproduce")
	}
	got := string(min)
	if strings.Count(got, "noise") != 0 {
		t.Fatalf("noise survived: %q", got)
	}
	if !strings.Contains(got, "MARK") {
		t.Fatalf("MARK (the divergence) was minimized away:\n%s", got)
	}
	if runs > 200 {
		t.Fatalf("runs = %d over cap", runs)
	}
}

// ---- harness gates ----

// TestLuafuzzSelfDiff is the M6e harness gate (the M1 pattern applied
// to generated programs): the oracle diffed against itself must be 0
// diffs — generated programs run, produce events, and never engine-
// panic on the interpreter.
func TestLuafuzzSelfDiff(t *testing.T) {
	a := testdiff.NewInterp("self-a")
	a.Deadline = 2 * time.Second
	b := testdiff.NewInterp("self-b")
	b.Deadline = 2 * time.Second
	engines := []testdiff.Engine{a, b}
	dir := t.TempDir()
	const seed = int64(777)
	for idx := 0; idx < 120; idx++ {
		src := Generate(seed, idx)
		out := RunCase(engines, dir, idx, src)
		if out.Class != ClassPass {
			t.Errorf("case %d: self-diff class %s (%s)\n%s", idx, out.Class, out.Detail, src)
		}
	}
}

// TestLuafuzzWasmMini: the interp-vs-wasm same-day-bug leg over a
// fresh generated slice (short-skipped: the wasmtime engine is slow to
// instantiate per case).
func TestLuafuzzWasmMini(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy: wasmtime per-case instantiation (CI -short skips)")
	}
	engines, err := BuildEngines([]string{"interp", "wasm"}, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	const seed = int64(778)
	for idx := 0; idx < 80; idx++ {
		src := Generate(seed, idx)
		out := RunCase(engines, dir, idx, src)
		if isFindingClass(out.Class) {
			t.Errorf("case %d: %s (%s)\n%s", idx, out.Class, out.Detail, src)
		}
	}
}

// TestGenerateEventBudget: the per-case print volume must stay far
// under the wasm leg's throughput at the 1.5 s soak deadline (~60k
// events ≈ 23 µs each through the rt_* ABI). The compound-nest HANGs
// (c0238352, c0003075, c0158534 — the last one a repeat that never
// counted toward loopDepth, so its nested loops were sized one level
// shallower) are all generator-budget bugs, not engine bugs; this
// pins the distribution's tail: max PRINT events over a generated
// slice stays an order of magnitude under the deadline's capacity.
func TestGenerateEventBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy: 3000 interp runs (CI -short skips)")
	}
	const (
		seed = int64(999)
		n    = 3000
		cap  = 20000 // ~1/3 of the wasm leg's 1.5 s capacity
	)
	eng := testdiff.NewInterp("budget")
	dir := t.TempDir()
	worst, worstIdx := 0, -1
	for idx := 0; idx < n; idx++ {
		src := Generate(seed, idx)
		log := eng.Run(testdiff.Case{Name: CaseName(idx), Dir: dir, Source: src})
		c := 0
		for _, l := range log {
			if strings.HasPrefix(l, "PRINT") {
				c++
			}
		}
		if c > worst {
			worst, worstIdx = c, idx
		}
	}
	if worst > cap {
		t.Errorf("case %d produced %d print events (cap %d) — a loop-nest budget regressed", worstIdx, worst, cap)
	}
}
