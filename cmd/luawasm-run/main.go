// Command luawasm-run executes a module saved by cmd/luawasmc against
// the C-Lua rt_* runtime (embedded lua55_rt.wasm, ABI v4) on wasmtime —
// or on the pure-Go wazero production engine when GLUA_WASM_ENGINE=wazero
// (ledger row 32: one blob, two hosts).
//
//	luawasm-run [-v] artifact.wasm [args...]
//
// args become the script's arg table (arg[1..n]). PRINT/STDOUT events go
// to stdout; errors to stderr (exit 1). -v prints the full event log
// (STEP/GLOBALS/...) exactly as the differential harness sees it.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/pschlump/glasm/testdiff"
)

func main() {
	verbose := flag.Bool("v", false, "print the full event log")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: luawasm-run [-v] artifact.wasm [args...]")
		os.Exit(2)
	}
	in := flag.Arg(0)
	bin, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luawasm-run: %v\n", err)
		os.Exit(1)
	}

	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "luawasm-run: %v\n", err)
		os.Exit(1)
	}
	e := testdiff.NewRunEngine("run", bin)
	log := e.Run(testdiff.Case{
		Name:   in, // arg[0] / chunkname: the path as typed
		Dir:    dir,
		Source: bin,
		Args:   flag.Args()[1:],
	})

	exit := 0
	for _, line := range log {
		if *verbose {
			fmt.Println(line)
			continue
		}
		tab := strings.IndexByte(line, '\t')
		event, payload := line, ""
		if tab >= 0 {
			event, payload = line[:tab], line[tab+1:]
		}
		switch event {
		case "PRINT", "STDOUT":
			fmt.Println(payload)
		case "ERROR", "ENGINE-ERROR", "SKIP-UNSUPPORTED":
			fmt.Fprintf(os.Stderr, "luawasm-run: %s\n", payload)
			exit = 1
		}
	}
	os.Exit(exit)
}
