// Command testdiff runs a corpus across engines and reports log diffs.
// CI entry point for the differential harness (design doc §7, §8):
//
//	go run ./cmd/testdiff -corpus _glua-tests -engines interp,interp
//
// Exit code 1 on any diff. Engine names: interp (the glasm oracle),
// clua55 (stock C Lua 5.5.1 in wasm — the semantic authority), wasm (the
// Lua→wasm backend on wasmtime, the dev oracle host), wazero (the same
// backend on the pure-Go production engine — M6b's engine-parity leg,
// ledger row 32), wazero-prod (the M6c sandbox leg: the zero-wasi prod
// blob + rt_sandbox(1), ledger row 33).
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/pschlump/glasm/testdiff"
)

func main() {
	corpus := flag.String("corpus", "_glua-tests", "corpus directory")
	enginesFlag := flag.String("engines", "interp,interp", "comma-separated engine list")
	verbose := flag.Bool("v", false, "print full event logs")
	flag.Parse()

	var engines []testdiff.Engine
	prodLeg := false
	for i, name := range strings.Split(*enginesFlag, ",") {
		switch name {
		case "interp":
			engines = append(engines, testdiff.NewInterp(fmt.Sprintf("interp-%c", 'a'+i)))
		case "clua55":
			engines = append(engines, testdiff.NewCLua55(fmt.Sprintf("clua55-%c", 'a'+i)))
		case "wasm":
			e := testdiff.NewWasmEngine(fmt.Sprintf("wasm-%c", 'a'+i))
			e.SkipUnsupported = true
			engines = append(engines, e)
		case "wazero":
			e := testdiff.NewWazeroEngine(fmt.Sprintf("wazero-%c", 'a'+i))
			e.SkipUnsupported = true
			engines = append(engines, e)
		case "wazero-prod":
			e := testdiff.NewWazeroEngine(fmt.Sprintf("wazero-prod-%c", 'a'+i))
			e.SkipUnsupported = true
			e.UseProdBlob()
			prodLeg = true
			engines = append(engines, e)
		default:
			fmt.Fprintf(os.Stderr, "unknown engine %q (known: interp, clua55, wasm, wazero, wazero-prod)\n", name)
			os.Exit(2)
		}
	}

	cases, err := testdiff.LoadCorpus(*corpus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load corpus: %v\n", err)
		os.Exit(2)
	}
	// the ledgered skip maps live in one place (skips.go) so this CLI leg
	// and the go-test gates can never disagree about what is excluded
	// (row-71 seam skips retired at U5e — the seam engines now raise
	// with the full varinfo hints, so there are no wasm-leg-only skips)
	// A prod leg layers the row-95 prodBlobSkips over the corpus map —
	// the same merge the go-test exit gate's prod pass (exitGateEngines)
	// does — so the compiled-out surface (io/os/debug/package/require/
	// load nil on lua55_prod.wasm) is excluded identically here (U8
	// audit: this leg silently missed the layer and failed on the
	// row-95-ruled slu00/base/os cases).
	skips := testdiff.CorpusSkips(*corpus)
	if prodLeg {
		layer := map[string]string{}
		for k, v := range skips {
			layer[k] = v
		}
		for k, v := range testdiff.ProdBlobSkips() {
			layer[k] = v
		}
		skips = layer
	}
	cases, skippedList := testdiff.FilterSkips(cases, skips)
	results := testdiff.RunCorpus(cases, engines)

	diffs := 0
	for i := range results {
		r := &results[i]
		if skipUnsupported(r) {
			r.Skipped = true
			skippedList = append(skippedList, r.Name+": "+r.SkipWhy)
			if *verbose {
				fmt.Printf("SKIP-UNSUPPORTED %s: %s\n", r.Name, r.SkipWhy)
			}
			continue
		}
		if d := r.Diff(); d != "" {
			diffs++
			fmt.Printf("DIFF    %s\n%s", r.Name, d)
		} else if *verbose {
			fmt.Printf("== %s ==\n", r.Name)
			for _, line := range r.Logs[r.Engines[0]] {
				fmt.Println(line)
			}
		}
	}
	fmt.Print(testdiff.Summary(results, skippedList))
	if diffs > 0 {
		os.Exit(1)
	}
}

// skipUnsupported marks a result skipped when any engine log is a
// SKIP-UNSUPPORTED marker (v1 backend: closures/varargs arrive with M5).
func skipUnsupported(r *testdiff.Result) bool {
	for _, eng := range r.Engines {
		if log := r.Logs[eng]; len(log) > 0 && strings.HasPrefix(log[0], "SKIP-UNSUPPORTED") {
			r.SkipWhy = strings.TrimPrefix(log[0], "SKIP-UNSUPPORTED\t")
			return true
		}
	}
	return false
}
