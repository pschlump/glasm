// Command luawasmc compiles a Lua 5.1 script to a WebAssembly module.
//
//	luawasmc [-o out.wasm] [-name chunkname] script.lua
//
// The emitted module imports the rt_* runtime (runtime/lua51_sjlj.wasm):
// pair it with cmd/luawasm-run to execute the saved artifact. Artifacts
// are deterministic for a given source+chunkname — cacheable by SHA,
// the Redis SCRIPT model.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pschlump/glasm/testdiff"
)

func main() {
	out := flag.String("o", "", "output .wasm path (default: input with .wasm)")
	name := flag.String("name", "", "chunk name for errors (default: input path)")
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: luawasmc [-o out.wasm] [-name chunkname] script.lua")
		os.Exit(2)
	}
	in := flag.Arg(0)
	outPath := *out
	if outPath == "" {
		outPath = strings.TrimSuffix(in, filepath.Ext(in)) + ".wasm"
	}
	chunk := *name
	if chunk == "" {
		// default chunk name follows the loadfile convention ("@path")
		// so compile errors print "in.lua:9:", not "[string ...]:9:"
		chunk = "@" + in
	}

	src, err := os.ReadFile(in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luawasmc: %v\n", err)
		os.Exit(1)
	}
	bin, err := testdiff.CompileSource(src, chunk)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luawasmc: %s: %v\n", in, err)
		os.Exit(1)
	}
	if err := os.WriteFile(outPath, bin, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "luawasmc: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s: %d bytes (%d bytes wasm)\n", outPath, len(src), len(bin))
}
