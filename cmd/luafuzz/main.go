// Command luafuzz is the M6e differential fuzzer CLI: a deterministic
// seeded generator driving the testdiff engines, with a chunk-
// resumable soak designed for a laptop that sleeps and moves between
// networks daily (all state under -dir; every interruption is safe).
//
// Typical chunk workflow (any number of chunks per day, in any
// terminal, interrupted at will — Ctrl-C finishes the in-flight case,
// snapshots, and exits cleanly; a hard kill loses nothing):
//
//	go run ./cmd/luafuzz -soak -max 90m             # a chunk bounded by time
//	go run ./cmd/luafuzz -soak -n 500               # ...or by cases
//	go run ./cmd/luafuzz -soak                      # until interrupted
//	go run ./cmd/luafuzz -status                    # cumulative progress + nights
//	go run ./cmd/luafuzz -replay -seed S -from A -to B
//	go run ./cmd/luafuzz -min findings/c0000123-DIVERGE.lua
//	go run ./cmd/luafuzz -reset                     # archive + start a fresh soak
//
// Exit codes: 0 = chunk clean, 1 = findings this chunk (or diffs on
// replay), 2 = usage/lock error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/pschlump/glasm/luafuzz"
)

var enginesFlag, netsFlag string

func main() {
	o := luafuzz.Options{}
	soak := flag.Bool("soak", false, "run a soak chunk (resumes automatically)")
	status := flag.Bool("status", false, "print cumulative soak status")
	reset := flag.Bool("reset", false, "archive the current soak and start fresh")
	replay := flag.Bool("replay", false, "regenerate and rerun specific cases")
	dumpGen := flag.Bool("gen", false, "dump generated sources (-seed/-from/-to) without running")
	minFile := flag.String("min", "", "minimize a finding file (path)")

	flag.StringVar(&o.Dir, "dir", "note/fuzz", "soak state directory")
	seedFlag := flag.Int64("seed", 0, "master seed (new soak only; 0 = time-derived; pinned on resume)")
	flag.StringVar(&enginesFlag, "engines", "interp,wasm", "engines: interp, wasm, wazero, wazero-prod")
	flag.DurationVar(&o.Deadline, "deadline", luafuzz.DefaultDeadline, "per-case engine deadline")
	flag.IntVar(&o.MaxCases, "n", 0, "stop after N cases this chunk (0 = no cap)")
	flag.DurationVar(&o.MaxDur, "max", 0, "stop after this duration this chunk (0 = until interrupted)")
	flag.DurationVar(&o.Checkpoint, "checkpoint", luafuzz.DefaultCheckpoint, "state snapshot interval")
	flag.DurationVar(&o.NightMinWall, "night-min", luafuzz.DefaultNightMin, "min active fuzz time for a night to qualify")
	flag.StringVar(&netsFlag, "nets", "192.168.1.143=work,192.168.0.195=home", "ip=label,... network labels for session logs")
	fromFlag := flag.Int("from", 0, "replay/gen: first case index")
	toFlag := flag.Int("to", 0, "replay/gen: last case index (0 = from)")
	minRuns := flag.Int("min-runs", 400, "minimize: max proving runs")
	verbose := flag.Bool("v", false, "replay: print event logs")
	flag.Parse()

	o.Engines = splitCSV(enginesFlag)
	o.Nets = parseNets(netsFlag)
	o.MasterSeed = *seedFlag

	switch {
	case *status:
		st, err := luafuzz.LoadState(o.Dir, time.Now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "luafuzz: no soak at %s (%v)\n", o.Dir, err)
			os.Exit(2)
		}
		printStatus(st, o)
	case *reset:
		if err := resetSoak(o.Dir); err != nil {
			fmt.Fprintf(os.Stderr, "luafuzz: reset: %v\n", err)
			os.Exit(2)
		}
		fmt.Printf("archived %s → %s (fresh soak starts on the next -soak)\n", o.Dir, archivePath(o.Dir))
	case *dumpGen:
		if *seedFlag == 0 || *fromFlag < 0 {
			fmt.Fprintln(os.Stderr, "luafuzz: -gen needs -seed and -from")
			os.Exit(2)
		}
		dumpGenCmd(*seedFlag, *fromFlag, *toFlag)
	case *replay:
		if *seedFlag == 0 {
			fmt.Fprintln(os.Stderr, "luafuzz: -replay needs -seed (from soak.json / the case header)")
			os.Exit(2)
		}
		os.Exit(replayCmd(o, *seedFlag, *fromFlag, *toFlag, *verbose))
	case *minFile != "":
		os.Exit(minCmd(o, *minFile, *minRuns))
	case *soak:
		os.Exit(soakCmd(o))
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func soakCmd(o luafuzz.Options) int {
	lock, err := luafuzz.AcquireLock(o.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luafuzz: %v\n", err)
		return 2
	}
	defer luafuzz.ReleaseLock(lock, o.Dir)

	// Process-bounded chunks (M6e overnight lesson): every wasm-engine
	// run creates a wasmtime Store that statically maps the blob's
	// 256 MiB max-memory plus arm64 guard regions and spawns rayon /
	// trap-handler threads — resources Go finalizers reclaim far too
	// slowly. Two overnight runs and a diagnostic run all degraded at
	// a few thousand cases (RSS ~6-12 GB, VSZ ~32 TB, one hard wedge
	// with parked wasmtime threads at ~10k). The soak therefore runs
	// in ≤ chunkLen-case processes and RE-EXECS itself for the next
	// chunk — the OS reclaims everything at exit, and the journal
	// makes the restart seamless (the design's crash-anywhere rule).
	const chunkLen = 2000
	userN := o.MaxCases
	chunkN := userN
	if chunkN <= 0 || chunkN > chunkLen {
		chunkN = chunkLen
	}
	o.MaxCases = chunkN

	engines, err := luafuzz.BuildEngines(o.Engines, o.Deadline)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luafuzz: %v\n", err)
		return 2
	}

	// Ctrl-C / kill: first signal ends the chunk cleanly (finish the
	// in-flight case, snapshot, exit 0); a second is immediate.
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	interrupted := false
	go func() {
		if _, ok := <-sig; ok {
			fmt.Println("\nluafuzz: interrupt — finishing case, snapshotting…")
			interrupted = true
			cancel()
			<-sig // second signal: drop everything (journal is durable)
			os.Exit(130)
		}
	}()

	start := time.Now()
	rep, err := luafuzz.RunChunk(ctx, o, engines)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luafuzz: %v\n", err)
		return 2
	}
	signal.Stop(sig)

	fmt.Printf("chunk #%d: %d cases, %d execs, %.1fs fuzz time, %d findings (stop: %s, %.1fs wall)\n",
		rep.Session.ID, rep.Cases, rep.Execs, rep.WallMs/1e3, rep.Findings,
		orDefault(rep.StopReason, "clean end"), time.Since(start).Seconds())

	// next chunk? Only when THIS chunk ended on its internal case cap
	// while the user's own budget (-n / -max / bare) still wants more.
	// Interrupts, duration caps, errors, and explicit single-chunk -n
	// bounds all end here instead.
	more := !interrupted && rep.StopReason == "case cap" && ctx.Err() == nil
	remainingN := userN - int(rep.Cases)
	remainingD := o.MaxDur - time.Since(start)
	if userN > 0 && remainingN <= 0 {
		more = false
	}
	if o.MaxDur > 0 && remainingD <= 0 {
		more = false
	}
	if more {
		// rewrite only the budget flags of the ORIGINAL argv and exec;
		// everything durable (journals, snapshot, lock pid) is on disk
		args := chunkArgs(os.Args, remainingN, remainingD)
		luafuzz.ReleaseLock(lock, o.Dir)
		if err := syscall.Exec(selfPath(), args, os.Environ()); err != nil {
			fmt.Fprintf(os.Stderr, "luafuzz: re-exec: %v\n", err)
			return 2
		}
	}

	if st, err := luafuzz.LoadState(o.Dir, time.Now); err == nil {
		printStatus(st, o)
	}
	if rep.Findings > 0 {
		return 1
	}
	return 0
}

// selfPath: the running binary's absolute path (os.Args[0] may be
// PATH-relative).
func selfPath() string {
	if p, err := os.Executable(); err == nil {
		return p
	}
	return os.Args[0]
}

// chunkArgs: the next chunk's argv = the ORIGINAL argv with only the
// budget flags (-n/-max, both forms) removed, then fresh ones appended.
// Everything else — -dir, -engines, -deadline, -nets, … — carries
// through unchanged. Zero remaining budgets drop their flag.
func chunkArgs(orig []string, remainingN int, remainingD time.Duration) []string {
	var args []string
	skip := 0
	for _, a := range orig {
		if skip > 0 {
			skip--
			continue
		}
		if a == "-n" || a == "-max" {
			skip = 1 // drop the value too; re-appended below
			continue
		}
		if strings.HasPrefix(a, "-n=") || strings.HasPrefix(a, "-max=") {
			continue
		}
		args = append(args, a)
	}
	if remainingN > 0 {
		args = append(args, "-n", fmt.Sprint(remainingN))
	}
	if remainingD > 0 {
		args = append(args, "-max", remainingD.Round(time.Second).String())
	}
	return args
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func replayCmd(o luafuzz.Options, seed int64, from, to int, verbose bool) int {
	if to < from {
		to = from
	}
	engines, err := luafuzz.BuildEngines(o.Engines, o.Deadline)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luafuzz: %v\n", err)
		return 2
	}
	caseDir, err := luafuzz.SoakDirLayout(o.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luafuzz: %v\n", err)
		return 2
	}
	findings := 0
	for idx := from; idx <= to; idx++ {
		src := luafuzz.Generate(seed, idx)
		if err := luafuzz.ValidSource(src, luafuzz.CaseName(idx)); err != nil {
			fmt.Printf("c%07d GENBUG  %v\n", idx, err)
			findings++
			continue
		}
		out := luafuzz.RunCase(engines, caseDir, idx, src)
		mark := "  ok"
		if out.Class != luafuzz.ClassPass {
			mark = string(out.Class)
			findings++
		}
		fmt.Printf("c%07d %-6s %5.0fms %d execs %s\n", idx, mark, out.Ms, out.Execs, oneLine(out.Detail))
		if verbose {
			fmt.Println(string(src))
		}
	}
	if findings > 0 {
		return 1
	}
	return 0
}

func minCmd(o luafuzz.Options, path string, maxRuns int) int {
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luafuzz: %v\n", err)
		return 2
	}
	idx := luafuzz.IdxFromName(filepath.Base(path))
	engines, err := luafuzz.BuildEngines(o.Engines, o.Deadline)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luafuzz: %v\n", err)
		return 2
	}
	caseDir, err := luafuzz.SoakDirLayout(o.Dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "luafuzz: %v\n", err)
		return 2
	}
	min, ok, runs := luafuzz.Minimize(src, engines, caseDir, idx, maxRuns)
	out := path[:len(path)-len(filepath.Ext(path))] + ".min.lua"
	if err := os.WriteFile(out, min, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "luafuzz: %v\n", err)
		return 2
	}
	if !ok {
		fmt.Printf("did not reproduce under %v — nothing minimized (wrote %s anyway)\n", o.Engines, out)
		return 1
	}
	fmt.Printf("minimized %d → %d lines in %d runs → %s\n",
		strings.Count(string(src), "\n"), strings.Count(string(min), "\n"), runs, out)
	return 0
}

func dumpGenCmd(seed int64, from, to int) {
	if to < from {
		to = from
	}
	for idx := from; idx <= to; idx++ {
		fmt.Printf("-- ===== case %d =====\n%s", idx, luafuzz.Generate(seed, idx))
	}
}

// ---- status rendering ----

func printStatus(st *luafuzz.State, o luafuzz.Options) {
	nights, streak := luafuzz.NightReport(st, o.NightMinWall)
	const goalExecs = 10_000_000
	fmt.Printf("soak    seed %d, started %s, updated %s\n", st.MasterSeed, st.Created, st.Updated)
	fmt.Printf("cases   %s (%d findings)   next case %d\n",
		comma(st.Totals.Cases), st.Totals.Findings, st.NextCase)
	fmt.Printf("execs   %s / %s (%.3f%% of the ×10⁷ gate)   fuzz time %.1fh\n",
		comma(st.Totals.Execs), comma(goalExecs),
		100*float64(st.Totals.Execs)/float64(goalExecs), st.Totals.WallMs/3.6e6)

	cls := make([]string, 0, len(st.Classes))
	for k := range st.Classes {
		cls = append(cls, k)
	}
	sort.Strings(cls)
	var parts []string
	for _, k := range cls {
		parts = append(parts, fmt.Sprintf("%s %s", k, comma(st.Classes[k])))
	}
	fmt.Printf("classes %s\n", strings.Join(parts, ", "))

	// sessions and their networks
	netCount := map[string]int{}
	for _, s := range st.Sessions {
		for _, n := range s.Nets {
			netCount[n.Net]++
		}
	}
	var nets []string
	for k, v := range netCount {
		nets = append(nets, fmt.Sprintf("%s×%d", k, v))
	}
	sort.Strings(nets)
	fmt.Printf("chunks  %d (%s)\n", len(st.Sessions), strings.Join(nets, ", "))

	fmt.Printf("nights  streak %d/7 clean", streak)
	if len(nights) > 0 && !nights[0].Clean {
		fmt.Printf(" — most recent night %s has %d findings", nights[0].Day, nights[0].Findings)
	}
	fmt.Println()
	for i, n := range nights {
		if i >= 10 {
			fmt.Printf("        … %d more\n", len(nights)-i)
			break
		}
		mark := "✗"
		if n.Clean && n.Qualifies {
			mark = "✓"
		} else if !n.Qualifies && n.Clean {
			mark = "·"
		}
		fmt.Printf("        %s %s  %5s cases %8s execs %6.0fs fuzz  findings %d (new classes %d)\n",
			mark, n.Day, comma(n.Cases), comma(n.Execs), n.WallMs/1e3, n.Findings, n.NewSigs)
	}

	if len(st.Signatures) > 0 {
		fmt.Printf("signatures (%d distinct):\n", len(st.Signatures))
		keys := make([]string, 0, len(st.Signatures))
		for k := range st.Signatures {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return st.Signatures[keys[i]].FirstCase < st.Signatures[keys[j]].FirstCase })
		for i, k := range keys {
			if i >= 20 {
				fmt.Printf("        … %d more\n", len(keys)-i)
				break
			}
			sg := st.Signatures[k]
			fmt.Printf("        c%07d %-13s ×%-4d first %s  %s\n",
				sg.FirstCase, sg.Class, sg.Count, sg.FirstDay,
				oneLine(strings.TrimPrefix(k, sg.Class+"|")))
		}
	}
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > 110 {
		s = s[:110] + "…"
	}
	return s
}

func comma(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var parts []string
	for len(s) > 3 { // groups of 3 from the right, commas excluded
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	parts = append([]string{s}, parts...)
	out := strings.Join(parts, ",")
	if neg {
		out = "-" + out
	}
	return out
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseNets(s string) map[string]string {
	m := map[string]string{}
	for _, kv := range splitCSV(s) {
		if i := strings.Index(kv, "="); i > 0 {
			m[strings.TrimSpace(kv[:i])] = strings.TrimSpace(kv[i+1:])
		}
	}
	return m
}

func resetSoak(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "soak.json")); err != nil {
		return fmt.Errorf("no soak at %s", dir)
	}
	return os.Rename(dir, archivePath(dir))
}

func archivePath(dir string) string {
	return fmt.Sprintf("%s-retired-%s", dir, time.Now().Format("20060102-150405"))
}
