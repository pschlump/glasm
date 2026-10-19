// Command luabench compares the modified glasm fork against the
// near-original baseline, side by side in one process:
//
//   - parse+compile time      (orig vs new frontend)
//   - interpreter run time    (precompiled proto, fresh state)
//   - interpreter total time  (NewState + DoString + Close — CLI equivalent)
//   - wasm compile time       (testdiff.CompileSource: frontend + wasm emit)
//   - wasm run time           (cold VM: engine create + instantiate + run)
//
// Usage: luabench script.lua [more.lua ...]      (GLUA_WASM_ENGINE honored)
package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	newlua "github.com/pschlump/glasm"
	newparse "github.com/pschlump/glasm/parse"
	"github.com/pschlump/glasm/testdiff"

	origlua "github.com/pschlump/gopher-lua-orig"
	origparse "github.com/pschlump/gopher-lua-orig/parse"
)

const (
	warmups     = 2
	targetTotal = 1200 * time.Millisecond
	minIters    = 3
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "luabench: fatal:", err)
		os.Exit(1)
	}
}

type stats struct {
	n              int
	min, med, mean time.Duration
}

func benchIt(fn func(), maxIters int) stats {
	for i := 0; i < warmups; i++ {
		fn()
	}
	runtime.GC()
	t0 := time.Now()
	fn()
	probe := time.Since(t0)
	n := int(targetTotal/max(probe, time.Microsecond)) + 1
	if n < minIters {
		n = minIters
	}
	if n > maxIters {
		n = maxIters
	}
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		runtime.GC()
		t0 = time.Now()
		fn()
		ds = append(ds, time.Since(t0))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	sum := time.Duration(0)
	for _, d := range ds {
		sum += d
	}
	return stats{n: n, min: ds[0], med: ds[len(ds)/2], mean: sum / time.Duration(n)}
}

func max(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%6.1fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%6.2fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%6.2fs ", float64(d)/float64(time.Second))
	}
}

// ---- stdout silencing (fmt.Print reads os.Stdout at call time) ----

var realStdout = os.Stdout

func silence() { os.Stdout = devNull }
func restore() { os.Stdout = realStdout }

var devNull *os.File

func init() {
	var err error
	devNull, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		panic(err)
	}
}

func captureStdout(fn func()) string {
	r, w, err := os.Pipe()
	must(err)
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	os.Stdout = old
	must(w.Close())
	return <-done
}

// ---- one-shot operations per flavor ----

func origParseOnce(src []byte, name string) {
	chunk, err := origparse.Parse(bytes.NewReader(src), name)
	must(err)
	_, err = origlua.Compile(chunk, name)
	must(err)
}

func newParseOnce(src []byte, name string) {
	chunk, err := newparse.Parse(bytes.NewReader(src), name)
	must(err)
	_, err = newlua.Compile(chunk, name)
	must(err)
}

func origRunOnce(proto *origlua.FunctionProto) {
	L := origlua.NewState()
	L.Push(L.NewFunctionFromProto(proto))
	must(L.PCall(0, 0, nil))
	L.Close()
}

func newRunOnce(proto *newlua.FunctionProto) {
	L := newlua.NewState()
	L.Push(L.NewFunctionFromProto(proto))
	must(L.PCall(0, 0, nil))
	L.Close()
}

func origTotalOnce(src []byte) {
	L := origlua.NewState()
	must(L.DoString(string(src)))
	L.Close()
}

func newTotalOnce(src []byte) {
	L := newlua.NewState()
	must(L.DoString(string(src)))
	L.Close()
}

func wasmCompileOnce(src []byte, name string) []byte {
	bin, err := testdiff.CompileSource(src, name)
	must(err)
	return bin
}

func wasmRunOnce(bin []byte, path, dir string) []string {
	e := testdiff.NewRunEngine("bench", bin)
	return e.Run(testdiff.Case{Name: path, Dir: dir, Source: bin})
}

func wasmStdout(log []string) string {
	var out []string
	for _, line := range log {
		if ev, payload, ok := strings.Cut(line, "\t"); ok && (ev == "STDOUT" || ev == "PRINT") {
			out = append(out, payload)
		} else if !ok && strings.HasPrefix(line, "STDOUT") {
			out = append(out, "")
		}
	}
	return strings.Join(out, "\n")
}

func wasmHasError(log []string) string {
	for _, line := range log {
		if ev, payload, ok := strings.Cut(line, "\t"); ok {
			if ev == "ERROR" || ev == "ENGINE-ERROR" || ev == "ENGINE-PANIC" {
				return payload
			}
		}
	}
	return ""
}

// ---- driver ----

type row struct {
	name string
	st   stats
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: luabench script.lua [more.lua ...]")
		os.Exit(2)
	}
	engine := "wasmtime (oracle)"
	if strings.EqualFold(os.Getenv("GLUA_WASM_ENGINE"), "wazero") {
		engine = "wazero (production)"
	}
	fmt.Printf("# luabench — %s | %s | GOMAXPROCS=%d | %s\n",
		runtime.Version(), engine, runtime.GOMAXPROCS(0), time.Now().Format("2006-01-02 15:04"))

	for _, path := range os.Args[1:] {
		src, err := os.ReadFile(path)
		must(err)
		name := filepath.Base(path)
		dir := filepath.Dir(path)
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}

		// ---- verification pass: same output from all three legs ----
		outOrig := strings.TrimSpace(captureStdout(func() { origTotalOnce(src) }))
		outNew := strings.TrimSpace(captureStdout(func() { newTotalOnce(src) }))
		wlog := wasmRunOnce(wasmCompileOnce(src, name), path, dir)
		outWasm := strings.TrimSpace(wasmStdout(wlog))
		check := "OK"
		if outOrig != outNew || outOrig != outWasm {
			check = "MISMATCH!"
			fmt.Printf("=== %s — OUTPUT MISMATCH\n  orig: %q\n  new:  %q\n  wasm: %q\n",
				name, outOrig, outNew, outWasm)
		}
		if e := wasmHasError(wlog); e != "" {
			fmt.Printf("=== %s — WASM ENGINE ERROR: %s\n", name, e)
			os.Exit(1)
		}

		// protos for the run-only legs (compiled once, outside timing)
		ochunk, err := origparse.Parse(bytes.NewReader(src), name)
		must(err)
		oproto, err := origlua.Compile(ochunk, name)
		must(err)
		nchunk, err := newparse.Parse(bytes.NewReader(src), name)
		must(err)
		nproto, err := newlua.Compile(nchunk, name)
		must(err)
		bin := wasmCompileOnce(src, name)

		rows := []row{}
		add := func(name string, fn func(), maxIters int) {
			rows = append(rows, row{name, benchIt(fn, maxIters)})
		}

		sil := func(f func()) func() {
			return func() { silence(); defer restore(); f() }
		}

		add("orig parse+compile", func() { origParseOnce(src, name) }, 400)
		add("new  parse+compile", func() { newParseOnce(src, name) }, 400)
		add("orig run (interp)", sil(func() { origRunOnce(oproto) }), 300)
		add("new  run (interp)", sil(func() { newRunOnce(nproto) }), 300)
		add("orig TOTAL (CLI)", sil(func() { origTotalOnce(src) }), 300)
		add("new  TOTAL (CLI)", sil(func() { newTotalOnce(src) }), 300)
		add("wasm compile", func() { wasmCompileOnce(src, name) }, 200)
		add("wasm run (VM start+run)", func() { wasmRunOnce(bin, path, dir) }, 60)

		// baseline for ratios: orig TOTAL median
		var base time.Duration
		for _, r := range rows {
			if r.name == "orig TOTAL (CLI)" {
				base = r.st.med
			}
		}

		fmt.Printf("\n=== %s  (%d bytes src, %d bytes wasm)  outputs: %s\n",
			name, len(src), len(bin), check)
		fmt.Printf("%-24s %5s  %9s  %9s  %9s  %s\n", "measurement", "n", "min", "median", "mean", "vs orig TOTAL")
		for _, r := range rows {
			ratio := ""
			if base > 0 {
				ratio = fmt.Sprintf("%6.2fx", float64(r.st.med)/float64(base))
			}
			fmt.Printf("%-24s %5d  %s  %s  %s  %s\n", r.name, r.st.n,
				fmtDur(r.st.min), fmtDur(r.st.med), fmtDur(r.st.mean), ratio)
		}
	}
	fmt.Println()
}
