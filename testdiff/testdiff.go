// Package testdiff is the differential testing harness of the Lua→wasm
// project (docs/Lua-Wasm-Design-and-Test-Plan.md §7–§8, milestone M1).
//
// An Engine executes one Lua test case and produces a normalized event
// log. Two (or more) engines run the same corpus and their logs are
// compared byte-for-byte; any mismatch is a DIVERGE (or, once the C oracle
// is wired in at M2, a C-DIVERGE) per §8.7. The M1 gate validates the
// oracle itself: the interpreter diffed against a second interpreter
// instance must produce zero diffs across the corpus.
//
// Event log shape (one entry per line, "EVENT<TAB>payload"):
//
//	PRINT	<tab-joined print args, normalized>
//	ERROR	<uncaught error value, normalized>
//	GLOBALS	<serialized globals table>
//
// Normalization rules are fixed by normalize.go and are part of the
// cross-engine contract: canonical %.14g numbers, quoted strings, sorted
// table keys, no addresses, placeholders for functions/userdata/threads.
package testdiff

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Engine executes a single test case and returns its event log.
type Engine interface {
	Name() string
	Run(c Case) []string
}

// Case is one corpus script.
type Case struct {
	Name   string // e.g. "base.lua"
	Dir    string // working directory for the run (may hold files the script reads)
	Source []byte
	// Args are exposed to the script as arg[1..n] (arg[0] stays the
	// chunkname, the C driver contract) — the standalone runners (glua
	// -W, luawasm-run) pass their CLI args through. Corpus cases set none.
	Args []string
}

// Result is the outcome of running one case across all engines.
type Result struct {
	Name    string
	Logs    map[string][]string // engine name → event log
	Engines []string            // engine order
	Skipped bool
	SkipWhy string
}

// LoadCorpus reads every *.lua file in dir (sorted) as cases.
func LoadCorpus(dir string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var cases []Case
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".lua") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
		cases = append(cases, Case{Name: e.Name(), Dir: abs, Source: src})
	}
	return cases, nil
}

// FilterSkips removes skipped cases; every skip must carry a reason
// (a skip without a reason is a bug — §8.9 ledger discipline).
func FilterSkips(cases []Case, skips map[string]string) ([]Case, []string) {
	var kept []Case
	var skipped []string
	for _, c := range cases {
		why, ok := skips[c.Name]
		if !ok {
			kept = append(kept, c)
			continue
		}
		if why == "" {
			panic("testdiff: skip without reason: " + c.Name)
		}
		skipped = append(skipped, fmt.Sprintf("%s (%s)", c.Name, why))
	}
	return kept, skipped
}

// RunCorpus executes all cases on all engines sequentially (cases may
// chdir, so no parallelism) and returns the results.
func RunCorpus(cases []Case, engines []Engine) []Result {
	results := make([]Result, 0, len(cases))
	for _, c := range cases {
		r := Result{Name: c.Name, Logs: map[string][]string{}}
		for _, e := range engines {
			r.Engines = append(r.Engines, e.Name())
			log := e.Run(c)
			if log == nil { // an engine paniced — keep the corpus running
				log = []string{"ENGINE-PANIC\t" + e.Name()}
			}
			r.Logs[e.Name()] = log
		}
		results = append(results, r)
	}
	return results
}

// Diff compares a result across engines. It returns the first differing
// engine pair and a human-readable description ("" when all agree).
func (r Result) Diff() string {
	if len(r.Engines) < 2 {
		return ""
	}
	base := r.Engines[0]
	for _, other := range r.Engines[1:] {
		if d := DiffLogs(r.Logs[base], r.Logs[other]); d != "" {
			return fmt.Sprintf("%s vs %s:\n%s", base, other, d)
		}
	}
	return ""
}

// DiffLogs returns a description of the first difference between two event
// logs ("" when equal). GLOBALS lines are excluded: the engines' library
// sets differ by construction (golua55 vs C Lua 5.1 — ledger row: the
// globals surface aligns with the host module in M7); the differential
// contract covers script-observable PRINT/ERROR/STDOUT events.
//
// U5e retired the row-71 seam strip: the backend now precomputes the
// interp's varinfo answers per raise site and passes them through the rt
// seam (rt_seam55.c's hint registry), so varinfo suffixes compare
// byte-exact on every leg, PRINTED pcall messages included.
func DiffLogs(a, b []string) string {
	a, b = dropGlobals(a), dropGlobals(b)
	a, b = stripTracebacks(a), stripTracebacks(b)
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return diffAt(a, b, i)
		}
	}
	if len(a) != len(b) {
		return diffAt(a, b, n)
	}
	return ""
}

func diffAt(a, b []string, i int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "line %d differs:\n", i+1)
	fmt.Fprintf(&sb, "  A: %s\n", lineAt(a, i))
	fmt.Fprintf(&sb, "  B: %s\n", lineAt(b, i))
	lo := i - 2
	if lo < 0 {
		lo = 0
	}
	for j := lo; j < i; j++ {
		fmt.Fprintf(&sb, "  context: %s\n", a[j])
	}
	return sb.String()
}

func dropGlobals(log []string) []string {
	out := make([]string, 0, len(log))
	for _, l := range log {
		if strings.HasPrefix(l, "GLOBALS\t") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// stripTracebacks: the interp engine's uncaught errors carry a Go-side
// "stack traceback:" tail the C engines cannot produce — engine surface,
// not semantics. Removed from cross-engine comparison (M5's byte-exact
// gate compares message HEADS, before the traceback).
func stripTracebacks(log []string) []string {
	out := make([]string, 0, len(log))
	for _, l := range log {
		if strings.HasPrefix(l, "ERROR\t") {
			if i := strings.Index(l, "\\nstack traceback:"); i >= 0 {
				l = l[:i] + `"`
			}
		}
		out = append(out, l)
	}
	return out
}

func lineAt(log []string, i int) string {
	if i < len(log) {
		return log[i]
	}
	return "<end of log>"
}

// Summary renders a one-line-per-case report; used by the gate test and CLI.
func Summary(results []Result, skipped []string) string {
	var sb strings.Builder
	okCount, diffCount := 0, 0
	for _, r := range results {
		if r.Skipped {
			continue
		}
		if d := r.Diff(); d != "" {
			diffCount++
			fmt.Fprintf(&sb, "DIFF    %s\n%s", r.Name, indent(d))
		} else {
			okCount++
			fmt.Fprintf(&sb, "ok      %s (%d events)\n", r.Name, len(r.Logs[r.Engines[0]]))
		}
	}
	for _, s := range skipped {
		fmt.Fprintf(&sb, "skip    %s\n", s)
	}
	fmt.Fprintf(&sb, "%d/%d cases, %d diffs, %d skips\n",
		okCount, len(results), diffCount, len(skipped))
	return sb.String()
}

func indent(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = "        " + lines[i]
	}
	return strings.Join(lines, "\n") + "\n"
}
