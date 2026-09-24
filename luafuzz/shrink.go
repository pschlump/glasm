package luafuzz

// The finding minimizer (design §8.4: "any mismatch minimizes the seed
// and files a regression test automatically"). Line-granularity ddmin:
// generated sources are one-statement-per-line, and every candidate is
// gated through the repo parser (an unbalanced removal fails parse and
// is rejected), so removing a whole inner block keeps it valid while
// removing a lone `end` does not. The predicate is the classification
// itself: a removal survives iff the same finding class persists.
// Determinism: engines run seeded (row 35), so re-classification is
// stable.

import (
	"strings"

	"github.com/pschlump/golua55/testdiff"
)

// Minimize shrinks src while the original finding class persists.
// Returns the minimized source, whether the finding reproduced at all,
// and the number of proving runs used.
func Minimize(src []byte, engines []testdiff.Engine, dir string, idx int, maxRuns int) ([]byte, bool, int) {
	if maxRuns <= 0 {
		maxRuns = 400
	}
	base := RunCase(engines, dir, idx, src)
	if !isFindingClass(base.Class) {
		return src, false, 1
	}
	target := base.Class
	// A class-only predicate drifts: shrinking often lands on a nearby
	// but unrelated divergence (the first luafuzz shrink walked a
	// signed-zero arith diff into a pairs(nil) lib-wording diff). The
	// predicate therefore also pins the diff payload — line numbers
	// excluded, first three payload tokens per side — so a removal only
	// survives when the SAME divergence is still on stage.
	targetSig := diffSig(base.Detail)

	lines := strings.Split(strings.TrimRight(string(src), "\n"), "\n")
	runs := 1

	// repro: does the candidate still trigger the same finding?
	repro := func(cand []string) bool {
		if len(cand) == 0 {
			return false
		}
		out := []byte(strings.Join(cand, "\n") + "\n")
		if err := ValidSource(out, CaseName(idx)); err != nil {
			return false // unbalanced removal
		}
		runs++
		o := RunCase(engines, dir, idx, out)
		if o.Class != target {
			return false
		}
		return diffSig(o.Detail) == targetSig || targetSig == ""
	}

	n := len(lines) / 2
	if n < 1 {
		n = 1
	}
	for n >= 1 {
		progressed := false
		for i := 0; i+n <= len(lines); {
			if runs >= maxRuns {
				return joinLines(lines), true, runs
			}
			cand := append(append([]string{}, lines[:i]...), lines[i+n:]...)
			if repro(cand) {
				lines = cand
				progressed = true
				continue // same window, same i — bite again
			}
			i++
		}
		if progressed {
			if nn := len(lines) / 2; nn >= 1 { // restart coarse
				n = nn
			} else {
				break
			}
			continue
		}
		if n == 1 {
			break
		}
		n /= 2
	}
	return joinLines(lines), true, runs
}

func joinLines(lines []string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

// diffSig distills a DIVERGE detail into its payload signature: the
// first three tokens of each engine's differing line, ignoring the
// line number. Neutral classes and engine-marker findings return ""
// (class-only matching there).
func diffSig(detail string) string {
	var a, b string
	for _, l := range strings.Split(detail, "\n") {
		l = strings.TrimSpace(l)
		if t, ok := strings.CutPrefix(l, "A: "); ok && a == "" {
			a = firstN(t, 3)
		}
		if t, ok := strings.CutPrefix(l, "B: "); ok && b == "" {
			b = firstN(t, 3)
		}
	}
	if a == "" && b == "" {
		return ""
	}
	return a + " ## " + b
}

func firstN(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}
