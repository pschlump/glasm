// Command perfcmp compares the glasm engines against native stock C Lua
// 5.5.1 on the luabench script set — wall time AND process memory. The
// record of a run lives in docs/Perf-vs-C-Lua.md.
//
// Engines:
//
//	clua     native stock C Lua 5.5.1 (runtime/build-native55.sh →
//	         note/native-lua55/lua; lua.c from U0's saved tarball)
//	interp   the glasm interpreter, fresh LState per run
//	wazero   the wasm backend on wazero over host/ — the production
//	         posture: cold = e.Run (fresh VM), warm = pooled VM rerun
//	wasmtime the wasm backend on wasmtime (the differential oracle host;
//	         shared Engine, fresh Store per run)
//
// Two postures:
//
//	cold  one fresh process per run (the child does exactly one run);
//	      wall = whole process under /usr/bin/time -l, RSS = its maxrss
//	warm  one process, N timed in-process runs (clua: loadfile+call
//	      reruns in one state via warm55.lua; interp: fresh state per
//	      run; wazero: pooled VM; wasmtime: fresh Store, shared Engine)
//
// Every leg consumes the SAME source file — the script with a
// "KEYS={}ARGV={}" prelude (the luabench engine prelude) — so no engine
// sees a differently-staged global environment, and every leg's return
// values are cross-checked across engines.
//
// Usage (from the repo root):
//
//	runtime/build-native55.sh
//	go run ./cmd/perfcmp [-only substr]
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	lua "github.com/pschlump/glasm"
	"github.com/pschlump/glasm/host"
	"github.com/pschlump/glasm/testdiff"
)

// prelude is prepended to every script for every engine (KEYS/ARGV
// staging via Lua itself — identical bytes on all legs).
const prelude = "KEYS={}ARGV={}\n"

type scriptDef struct {
	name            string
	coldReps, warmN int
}

// h_hostfn is excluded: it needs the row-96 hostfn seam, which the
// wasmtime engine leg has no surface for (same skip as luabench).
var scripts = []scriptDef{
	{"h_tiny.lua", 12, 300},
	{"h_str.lua", 12, 200},
	{"h_strlong.lua", 4, 40},
	{"h_numloop.lua", 5, 12},
	{"h_tablechurn.lua", 8, 60},
	{"h_concat.lua", 8, 60},
	{"h_fib.lua", 3, 4},
	{"h_nsieve.lua", 5, 10},
	{"h_callret.lua", 5, 12},
	{"h_tco.lua", 5, 12},
}

// wasmtimeWarmCap: every testdiff wasmtime Run static-maps the blob's
// 256 MiB linear memory (+ arm64 guard region) into a fresh Store, and Go
// finalizers reclaim dead Stores far too slowly (the M6e cliff) — warm
// loops cap the per-child Store count so the oracle-host leg measures
// run cost, not finalizer backlog.
const wasmtimeWarmCap = 40

var (
	childFlag  = flag.String("child", "", "child mode: interp|wazero|wasmtime")
	posture    = flag.String("posture", "warm", "child posture: cold (1 run, no warmup) or warm")
	srcFlag    = flag.String("src", "", "child: script source file (prelude applied)")
	runsFlag   = flag.Int("runs", 1, "child: timed runs")
	nameFlag   = flag.String("name", "bench.lua", "child: chunk name")
	only       = flag.String("only", "", "restrict to scripts whose name contains this substring")
	luaBin     = flag.String("lua", "note/native-lua55/lua", "native C Lua 5.5.1 binary (runtime/build-native55.sh)")
	scriptsDir = flag.String("scripts", "cmd/luabench/scripts", "bench script dir (repo-root relative)")
	warmDrv    = flag.String("warmdriver", "cmd/perfcmp/warm55.lua", "native warm rerun driver")
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "perfcmp: fatal:", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- children

// report is the child protocol tail: per-run timing (median, min), the
// last run's return values, and post-run Go memory stats.
func report(ret string, ds []time.Duration) {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fmt.Printf("PERFRUN %d %d\n", ds[len(ds)/2].Nanoseconds(), ds[0].Nanoseconds())
	fmt.Printf("RET %s\n", strings.ReplaceAll(ret, "\n", `\n`))
	fmt.Printf("GOMEM %d %d %d\n", ms.HeapSys, ms.Sys, ms.HeapAlloc)
}

// childInterp: fresh LState per run (luabench's interp TOTAL posture —
// what a fork-style EVAL host pays per run).
func childInterp(src []byte, warmup, runs int) {
	s := string(src)
	ret := ""
	var ds []time.Duration
	for i := 0; i < warmup+runs; i++ {
		runtime.GC()
		t0 := time.Now()
		L := lua.NewState()
		must(L.DoString(s))
		vals := make([]string, 0, L.GetTop())
		for j := 1; j <= L.GetTop(); j++ {
			vals = append(vals, L.Get(j).String())
		}
		ret = strings.Join(vals, " ")
		L.Close()
		if i >= warmup {
			ds = append(ds, time.Since(t0))
		}
	}
	report(ret, ds)
}

func resultString(res host.Result) string {
	vals := make([]string, 0, len(res.Values))
	for _, v := range res.Values {
		vals = append(vals, v.String())
	}
	return strings.Join(vals, " ")
}

// childWazero: the production host package. pooled=false → e.Run (fresh
// VM per run, the cold posture); pooled=true → one pooled VM bound to
// the script (the warm posture).
func childWazero(src []byte, name string, warmup, runs int, pooled bool) {
	e, err := host.NewEngine()
	must(err)
	s, err := e.Compile(src, name)
	must(err)
	ctx := context.Background()

	var vm *host.VM
	if pooled {
		vm, err = e.NewVM()
		must(err)
		defer vm.Close()
	}
	exec := func() string {
		if pooled {
			res, err := vm.Run(ctx, s, host.RunOptions{})
			must(err)
			return resultString(res)
		}
		res, err := e.Run(ctx, s, host.RunOptions{})
		must(err)
		return resultString(res)
	}
	ret := ""
	var ds []time.Duration
	for i := 0; i < warmup; i++ {
		exec()
	}
	for i := 0; i < runs; i++ {
		runtime.GC()
		t0 := time.Now()
		ret = exec()
		ds = append(ds, time.Since(t0))
	}
	report(ret, ds)
	must(e.Close())
}

// childWasmtime: the differential oracle host. One wasmtime Engine per
// child (the compilation cache), fresh Store per run.
func childWasmtime(src []byte, name string, warmup, runs int) {
	bin, err := testdiff.CompileSource(src, "@"+name)
	must(err)
	eng := testdiff.NewWasmEngine("perfcmp")
	eng.Precompiled = bin
	wd, _ := os.Getwd()
	c := testdiff.Case{Name: name, Dir: wd, Source: src}
	ret := ""
	var ds []time.Duration
	for i := 0; i < warmup+runs; i++ {
		runtime.GC()
		t0 := time.Now()
		log := eng.Run(c)
		d := time.Since(t0)
		for _, l := range log {
			if strings.HasPrefix(l, "ERROR") || strings.HasPrefix(l, "ENGINE-") {
				must(fmt.Errorf("wasmtime leg: %s", l))
			}
			if strings.HasPrefix(l, "RESULTS\t") {
				// payload is "<count>\t<repr>..." with quoted strings
				// (cwire rendering) — keep the reprs only
				ret = strings.TrimPrefix(l, "RESULTS\t")
				if i := strings.IndexByte(ret, '\t'); i >= 0 {
					ret = ret[i+1:]
				}
				ret = strings.ReplaceAll(ret, `"`, "")
			}
		}
		if i >= warmup {
			ds = append(ds, d)
		}
	}
	report(ret, ds)
}

func childMain() {
	src, err := os.ReadFile(*srcFlag)
	must(err)
	warmup, runs := 0, *runsFlag
	if *posture == "warm" {
		warmup = 2
	} else {
		runs = 1
	}
	switch *childFlag {
	case "interp":
		childInterp(src, warmup, runs)
	case "wazero":
		childWazero(src, *nameFlag, warmup, runs, *posture == "warm")
	case "wasmtime":
		childWasmtime(src, *nameFlag, warmup, runs)
	default:
		must(fmt.Errorf("unknown -child %q", *childFlag))
	}
}

// ----------------------------------------------------------------- parent

type cell struct {
	wall  time.Duration // cold: median process wall; warm: child-reported median
	rss   int64         // bytes; cold: median across reps; warm: the loop process
	ret   string
	gomem string // "heapsys sys heapalloc" (Go children, post-run)
	heap  string // collectgarbage("count") KB at exit (clua warm)
}

type leg struct {
	engine string
	cold   cell
	warm   cell
}

type row struct {
	name string
	legs []leg
}

var engineOrder = []string{"clua", "interp", "wazero", "wasmtime"}

var rssRe = regexp.MustCompile(`(\d+)\s+maximum resident set size`)

// childEnv pins the darwin-27 wedge-family mitigation on every child
// (memory: long-lived cgo+Go processes wedge without asyncpreemptoff).
func childEnv() []string {
	env := os.Environ()
	for i, kv := range env {
		if strings.HasPrefix(kv, "GODEBUG=") {
			if !strings.Contains(kv, "asyncpreemptoff=1") {
				env[i] += ",asyncpreemptoff=1"
			}
			return env
		}
	}
	return append(env, "GODEBUG=asyncpreemptoff=1")
}

// runTimed runs argv under /usr/bin/time -l and returns the process wall
// time, maxrss (bytes), and the child's stdout.
func runTimed(argv []string) (time.Duration, int64, string) {
	cmd := exec.Command("/usr/bin/time", append([]string{"-l"}, argv...)...)
	cmd.Env = childEnv()
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	t0 := time.Now()
	err := cmd.Run()
	wall := time.Since(t0)
	if err != nil {
		must(fmt.Errorf("%s: %v\n%s", strings.Join(argv, " "), err, errb.String()))
	}
	var rss int64
	if m := rssRe.FindStringSubmatch(errb.String()); m != nil {
		rss, _ = strconv.ParseInt(m[1], 10, 64)
	}
	return wall, rss, out.String()
}

func parseChildOut(out string) (med time.Duration, ret, gomem string) {
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "PERFRUN "):
			f := strings.Fields(line)
			ns, _ := strconv.ParseInt(f[1], 10, 64)
			med = time.Duration(ns) * time.Nanosecond
		case strings.HasPrefix(line, "RET "):
			ret = strings.TrimPrefix(line, "RET ")
		case strings.HasPrefix(line, "GOMEM "):
			gomem = strings.TrimPrefix(line, "GOMEM ")
		}
	}
	return
}

func medDur(ds []time.Duration) time.Duration {
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return ds[len(ds)/2]
}

func medInt(xs []int64) int64 {
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	return xs[len(xs)/2]
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fµs", float64(d)/float64(time.Microsecond))
	case d < time.Second:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	default:
		return fmt.Sprintf("%.2fs", float64(d)/float64(time.Second))
	}
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func fmtRSS(b int64) string {
	switch {
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dKB", b>>10)
	}
}

// runLegs measures every engine on one prelude'd script file.
func runLegs(sc scriptDef, tmp, self string) []leg {
	byName := map[string]*leg{}
	legs := make([]leg, len(engineOrder))
	for i, e := range engineOrder {
		legs[i].engine = e
		byName[e] = &legs[i]
	}

	// clua cold: fresh native process per rep (its only CLI posture).
	{
		var walls []time.Duration
		var rsses []int64
		for r := 0; r < sc.coldReps; r++ {
			w, rss, _ := runTimed([]string{*luaBin, tmp})
			walls = append(walls, w)
			rsses = append(rsses, rss)
		}
		byName["clua"].cold = cell{wall: medDur(walls), rss: medInt(rsses)}
	}
	// clua warm: loadfile+call reruns in one state (warm55.lua).
	{
		_, rss, out := runTimed([]string{*luaBin, *warmDrv, tmp, strconv.Itoa(sc.warmN)})
		var iters []time.Duration
		ret, heap := "", ""
		for _, line := range strings.Split(out, "\n") {
			switch {
			case strings.HasPrefix(line, "ITER "):
				ns, _ := strconv.ParseInt(strings.TrimPrefix(line, "ITER "), 10, 64)
				iters = append(iters, time.Duration(ns)*time.Nanosecond)
			case strings.HasPrefix(line, "RET "):
				ret = strings.TrimPrefix(line, "RET ")
			case strings.HasPrefix(line, "HEAPKB "):
				heap = strings.TrimPrefix(line, "HEAPKB ")
			}
		}
		if len(iters) == 0 {
			must(fmt.Errorf("clua warm leg produced no ITER lines:\n%s", out))
		}
		byName["clua"].warm = cell{wall: medDur(iters), rss: rss, ret: ret, heap: heap}
	}

	// Go engines: cold = fresh child process per rep (exactly one run);
	// warm = one child, in-process loop.
	for _, eng := range []string{"interp", "wazero", "wasmtime"} {
		var walls []time.Duration
		var rsses []int64
		var lastRet, lastGomem string
		for r := 0; r < sc.coldReps; r++ {
			w, rss, out := runTimed([]string{self,
				"-child=" + eng, "-posture=cold", "-src=" + tmp, "-name=" + sc.name, "-runs=1"})
			walls = append(walls, w)
			rsses = append(rsses, rss)
			_, lastRet, lastGomem = parseChildOut(out)
		}
		byName[eng].cold = cell{wall: medDur(walls), rss: medInt(rsses), ret: lastRet, gomem: lastGomem}

		warmN := sc.warmN
		if eng == "wasmtime" && warmN > wasmtimeWarmCap {
			warmN = wasmtimeWarmCap
		}
		_, rss, out := runTimed([]string{self,
			"-child=" + eng, "-posture=warm", "-src=" + tmp, "-name=" + sc.name, "-runs=" + strconv.Itoa(warmN)})
		med, ret, gomem := parseChildOut(out)
		byName[eng].warm = cell{wall: med, rss: rss, ret: ret, gomem: gomem}
	}
	return legs
}

// normRet compares return values across engines: multi-line strings are
// newline-escaped by the native driver, so fold both spellings into
// single-space-joined tokens.
func normRet(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, `\n`, " ")), " ")
}

// emitMarkdown prints paste-ready tables for the doc.
func emitMarkdown(rows []row) {
	pick := func(r row, eng string) *leg {
		for i := range r.legs {
			if r.legs[i].engine == eng {
				return &r.legs[i]
			}
		}
		return nil
	}
	table := func(title string, val func(r row, l *leg) string) {
		fmt.Printf("\n## %s\n\n", title)
		fmt.Printf("| script | clua | interp | wazero | wasmtime |\n|---|---|---|---|---|\n")
		for _, r := range rows {
			fmt.Printf("| %s |", strings.TrimSuffix(r.name, ".lua"))
			for _, e := range engineOrder {
				fmt.Printf(" %s |", val(r, pick(r, e)))
			}
			fmt.Println()
		}
	}
	table("Cold wall (one-shot process, median)", func(_ row, l *leg) string { return fmtDur(l.cold.wall) })
	table("Cold maxRSS", func(_ row, l *leg) string { return fmtRSS(l.cold.rss) })
	table("Warm wall (per run, steady state, median)", func(_ row, l *leg) string { return fmtDur(l.warm.wall) })
	table("Warm maxRSS (loop process)", func(_ row, l *leg) string { return fmtRSS(l.warm.rss) })
	table("Warm wall × native C", func(r row, l *leg) string {
		return fmt.Sprintf("%.1f×", float64(l.warm.wall)/float64(pick(r, "clua").warm.wall))
	})
}

func main() {
	flag.Parse()
	if *childFlag != "" {
		childMain()
		return
	}

	if _, err := os.Stat(filepath.Join(*scriptsDir, "h_tiny.lua")); err != nil {
		must(fmt.Errorf("cannot read %s — run perfcmp from the repo root: %v", *scriptsDir, err))
	}
	if _, err := os.Stat(*luaBin); err != nil {
		must(fmt.Errorf("native lua missing at %s — run runtime/build-native55.sh first: %v", *luaBin, err))
	}
	self, err := os.Executable()
	must(err)

	fmt.Printf("# perfcmp — glasm vs native stock C Lua 5.5.1 | %s | GOMAXPROCS=%d | %s\n",
		runtime.Version(), runtime.GOMAXPROCS(0), time.Now().Format("2006-01-02 15:04"))
	fmt.Printf("# clua=%s warmdriver=%s children GODEBUG=asyncpreemptoff=1\n", *luaBin, *warmDrv)

	tmpdir, err := os.MkdirTemp("", "perfcmp")
	must(err)
	defer os.RemoveAll(tmpdir)

	var rows []row
	for _, sc := range scripts {
		stem := strings.TrimSuffix(sc.name, ".lua")
		if *only != "" && !strings.Contains(stem, *only) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(*scriptsDir, sc.name))
		must(err)
		tmp := filepath.Join(tmpdir, sc.name)
		must(os.WriteFile(tmp, append([]byte(prelude), src...), 0o644))

		legs := runLegs(sc, tmp, self)
		rows = append(rows, row{name: sc.name, legs: legs})

		fmt.Printf("\n=== %s (%d bytes + prelude)\n", sc.name, len(src))
		fmt.Printf("  %-8s %8s %9s   %8s %9s\n", "engine", "cold", "maxRSS", "warm", "maxRSS")
		rets := map[string]string{}
		for _, lg := range legs {
			fmt.Printf("  %-8s %8s %9s   %8s %9s",
				lg.engine, fmtDur(lg.cold.wall), fmtRSS(lg.cold.rss), fmtDur(lg.warm.wall), fmtRSS(lg.warm.rss))
			if lg.warm.heap != "" {
				fmt.Printf("  lua-heap %sKB", lg.warm.heap)
			}
			if f := strings.Fields(lg.warm.gomem); len(f) == 3 {
				hs, sy, ha := atoi64(f[0]), atoi64(f[1]), atoi64(f[2])
				fmt.Printf("  goheap heapsys=%s sys=%s live=%s", fmtRSS(hs), fmtRSS(sy), fmtRSS(ha))
			}
			fmt.Println()
			rets[lg.engine] = lg.warm.ret
		}
		ref := normRet(rets["clua"])
		for _, e := range []string{"interp", "wazero", "wasmtime"} {
			if normRet(rets[e]) != ref {
				fmt.Printf("  RET MISMATCH %s: %q vs clua %q\n", e, rets[e], rets["clua"])
			}
		}
	}
	emitMarkdown(rows)
	fmt.Println()
}
