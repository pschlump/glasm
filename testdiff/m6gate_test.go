package testdiff

// M6 gates (wazero legs): the same corpora the M5e gates run, interp vs
// the wazero-hosted engine — the production engine (design A8: pure Go,
// no cgo; EH enabled via experimental.CoreFeaturesExceptionHandling). The
// wasmtime engine remains the differential oracle; these gates prove the
// production host agrees with the interp oracle over the full surface.

import (
	"os"
	"path/filepath"
	"testing"
)

func wazeroEngine() Engine {
	return &WazeroEngine{name: "wazero", SkipUnsupported: true}
}

// wazeroCorpus runs interp vs wazero over a corpus and fails on any
// unruled divergence (same contract as wasmGluaFull).
func wazeroCorpus(t *testing.T, dir string, skips map[string]string) {
	t.Helper()
	cases, err := LoadCorpus(dir)
	if err != nil {
		t.Fatalf("load corpus %s: %v", dir, err)
	}
	cases, skipped := FilterSkips(cases, skips)
	engines := []Engine{NewInterp("interp"), wazeroEngine()}
	results := RunCorpus(cases, engines)

	bad := 0
	for _, r := range results {
		if d := r.Diff(); d != "" {
			bad++
			t.Errorf("%s:\n%s", r.Name, d)
		}
	}
	t.Log("\n" + Summary(results, skipped))
	if bad > 0 {
		t.Fatalf("%d/%d cases diverge", bad, len(cases))
	}
	if len(cases) == 0 {
		t.Fatal("empty corpus")
	}
}

// TestWazeroFullMatrix: the opcode matrix green on the production engine.
func TestWazeroFullMatrix(t *testing.T) {
	wazeroCorpus(t, "../_wasm-tests", CorpusSkips("../_wasm-tests"))
}

// TestWazeroGluaFull: the fork conformance suite on the production engine
// (same ledgered skip map as the wasmtime leg).
func TestWazeroGluaFull(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy corpus — skipped in -short mode")
	}
	wazeroCorpus(t, "../_glua-tests", gluaWasmSkips)
}

// TestWazeroErrorSuite: the byte-exact error suite on the production
// engine (same shape as TestWasmErrorSuite).
func TestWazeroErrorSuite(t *testing.T) {
	dir := "../_wasm-err-tests"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no error suite corpus: %v", err)
	}
	var names []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".lua" {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Skip("error suite corpus empty — run TestGenM5ErrCorpus")
	}
	skipped := 0
	for _, name := range names {
		src, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if why, ok := ledgeredErrSkips[name]; ok {
			skipped++
			t.Logf("skip %s (%s)", name, why)
			continue
		}
		c := Case{Name: name, Dir: dir, Source: src}
		a := NewInterp("interp").Run(c)
		b := wazeroEngine().Run(c)
		for _, l := range b {
			if len(l) > 9 && l[:9] == "SKIP-UNSU" {
				t.Errorf("%s: %s", name, l)
			}
		}
		if d := DiffLogs(a, b); d != "" {
			t.Errorf("%s:\n%s", name, d)
		}
	}
	t.Logf("%d comparable, %d ledgered skips", len(names)-skipped, skipped)
}
