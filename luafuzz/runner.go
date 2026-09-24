package luafuzz

// The soak runner: engine construction, per-case execution, failure
// classification (design doc §8.7), finding persistence, and the
// interruptible chunk loop.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/pschlump/golua55/testdiff"
)

// Class is a case outcome (§8.7 plus soak-specific bookkeeping classes).
type Class string

const (
	ClassPass    Class = "PASS"       // all engines agree
	ClassDLTie   Class = "DL-TIE"     // deadline on every engine (neutral; guards should prevent it)
	ClassSleep   Class = "SLEEP-DROP" // laptop slept mid-case — not a signal
	ClassDiverge Class = "DIVERGE"    // event-log mismatch (§8.7 — blocks)
	ClassTrap    Class = "TRAP"       // wasm trap (§8.7 — always a backend bug)
	ClassEngine  Class = "ENGINE-ERR" // engine failed to run the case at all
	ClassPanic   Class = "ENGINE-PANIC"
	ClassHang    Class = "HANG"   // deadline on exactly one engine
	ClassGenBug  Class = "GENBUG" // generator emitted invalid source
	ClassSkip    Class = "SKIP"   // backend refused (scope leak — post-M5 this is a bug)
	ClassWedge   Class = "WEDGE"  // hung past the wall guard in two fresh processes
)

// isFindingClass: outcomes that dirty a night and need a ledger row
// (or a generator-scoping note) before the 7-night clock counts.
func isFindingClass(c Class) bool {
	switch c {
	case ClassDiverge, ClassTrap, ClassEngine, ClassPanic, ClassHang, ClassGenBug, ClassSkip, ClassWedge:
		return true
	}
	return false
}

// ---- wall guard / wedge marker ----
//
// A wedged engine call never returns, so the guard is a timer that
// marks and hard-exits; the outer runner loop (bin/run-fuzzer.sh)
// restarts the chunk. First wedge on a case = assumed process-state
// (the overnight wasmtime accumulation); second wedge in a FRESH
// process = a real case-level hang → WEDGE finding, skipped.

type wedgeMarker struct {
	Idx   int `json:"idx"`
	Count int `json:"count"`
}

func wedgePath(dir string) string { return filepath.Join(dir, "wedge-marker") }

func readWedgeMarker(dir string) *wedgeMarker {
	raw, err := os.ReadFile(wedgePath(dir))
	if err != nil {
		return nil
	}
	var m wedgeMarker
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return &m
}

func clearWedgeMarker(dir string) { os.Remove(wedgePath(dir)) }

func armWallGuard(dir string, idx int, wall time.Duration) *time.Timer {
	return time.AfterFunc(wall, func() {
		count := 1
		if m := readWedgeMarker(dir); m != nil && m.Idx == idx {
			count = m.Count + 1
		}
		b, _ := json.Marshal(wedgeMarker{Idx: idx, Count: count})
		_ = atomicWrite(wedgePath(dir), b, 0o644)
		fmt.Fprintf(os.Stderr, "luafuzz: wall guard — case %d wedged %ds (mark %d), exiting for chunk restart\n",
			idx, int(wall.Seconds()), count)
		os.Exit(99)
	})
}

const deadlineText = "context deadline exceeded"

// IdxFromName extracts the case index from "cNNNNNNN-*.lua" (0 when
// unparseable — the minimizer then just uses a neutral chunkname).
func IdxFromName(base string) int {
	var idx int
	if strings.HasPrefix(base, "c") {
		fmt.Sscanf(base[1:], "%d", &idx)
	}
	return idx
}

// CaseOutcome is one classified case.
type CaseOutcome struct {
	Idx    int
	Class  Class
	Ms     float64
	Execs  int
	Sig    string // finding signature ("" when neutral)
	Detail string
}

// FindingRecord is one findings/index.jsonl line.
type FindingRecord struct {
	Idx   int    `json:"idx"`
	Case  string `json:"case"`
	Class string `json:"class"`
	Sig   string `json:"sig"`
	Day   string `json:"day"`
	File  string `json:"file"`
	Note  string `json:"note,omitempty"`
}

// BuildEngines mirrors cmd/testdiff's engine registry for the soak
// legs. Seed stays 0 (the harness's 42 contract, row 35); the deadline
// is the per-case safety net.
func BuildEngines(spec []string, deadline time.Duration) ([]testdiff.Engine, error) {
	var engines []testdiff.Engine
	for i, name := range spec {
		switch name {
		case "interp":
			e := testdiff.NewInterp(fmt.Sprintf("interp-%c", 'a'+i))
			e.Deadline = deadline
			engines = append(engines, e)
		case "wasm":
			e := testdiff.NewWasmEngine(fmt.Sprintf("wasm-%c", 'a'+i))
			e.Deadline = deadline
			engines = append(engines, e)
		case "wazero":
			e := testdiff.NewWazeroEngine(fmt.Sprintf("wazero-%c", 'a'+i))
			e.Deadline = deadline
			engines = append(engines, e)
		case "wazero-prod":
			e := testdiff.NewWazeroEngine(fmt.Sprintf("wazero-prod-%c", 'a'+i))
			e.Deadline = deadline
			e.UseProdBlob()
			engines = append(engines, e)
		default:
			return nil, fmt.Errorf("unknown engine %q (soak legs: interp, wasm, wazero, wazero-prod)", name)
		}
	}
	if len(engines) < 2 {
		return nil, fmt.Errorf("a soak needs ≥2 engines (got %v)", spec)
	}
	return engines, nil
}

// RunCase executes one case on every engine and classifies it.
// Engines run sequentially (they chdir per case).
func RunCase(engines []testdiff.Engine, dir string, idx int, src []byte) CaseOutcome {
	name := CaseName(idx)
	c := testdiff.Case{Name: name, Dir: dir, Source: src}
	start := time.Now()
	logs := make([][]string, len(engines))
	names := make([]string, len(engines))
	for i, e := range engines {
		names[i] = e.Name()
		logs[i] = e.Run(c)
	}
	ms := float64(time.Since(start).Milliseconds())
	out := CaseOutcome{Idx: idx, Ms: ms, Execs: len(engines)}

	// engine health first — a panic/err invalidates diffing
	for i, log := range logs {
		if len(log) == 0 {
			out.Class, out.Detail = ClassPanic, names[i]+": nil log"
			out.Sig = sigOf(out.Detail)
			return out
		}
		switch {
		case strings.HasPrefix(log[0], "ENGINE-PANIC\t"):
			out.Class, out.Detail = ClassPanic, firstLine(log)
			out.Sig = sigOf(names[i] + " panic")
			return out
		case strings.HasPrefix(log[0], "SKIP-UNSUPPORTED\t"):
			out.Class, out.Detail = ClassSkip, firstLine(log)
			out.Sig = sigOf(out.Detail)
			return out
		}
		for _, l := range log {
			if strings.HasPrefix(l, "ENGINE-ERROR\t") {
				cls := ClassEngine
				if strings.HasPrefix(l, "ENGINE-ERROR\ttrap:") {
					cls = ClassTrap
				}
				out.Class, out.Detail = cls, l
				out.Sig = sigOf(firstWords(l, 8))
				return out
			}
		}
	}

	// deadline detection before diffing: interp's raise is position-
	// prefixed, the wasm poll's is not (row 37) — a plain diff would
	// misread a both-engine expiry as a DIVERGE.
	deadlineEngines := 0
	for _, log := range logs {
		for _, l := range log {
			if strings.HasPrefix(l, "ERROR\t") && strings.Contains(l, deadlineText) {
				deadlineEngines++
				break
			}
		}
	}
	if deadlineEngines == len(logs) {
		out.Class = ClassDLTie
		if time.Duration(ms)*time.Millisecond > sleepDropGap {
			out.Class = ClassSleep // machine slept mid-case; timers fired stale
		}
		return out
	}
	if deadlineEngines > 0 {
		out.Class = ClassHang
		out.Detail = fmt.Sprintf("deadline fired on %d/%d engines", deadlineEngines, len(logs))
		out.Sig = sigOf(out.Detail)
		return out
	}

	// byte-for-byte event-log diff (GLOBALS/traceback exclusions are
	// the harness contract)
	base := logs[0]
	for i := 1; i < len(logs); i++ {
		if d := testdiff.DiffLogs(base, logs[i]); d != "" {
			out.Class = ClassDiverge
			out.Detail = fmt.Sprintf("%s vs %s:\n%s", names[0], names[i], d)
			out.Sig = sigOf(firstWords(d, 6))
			return out
		}
	}
	out.Class = ClassPass
	return out
}

func firstLine(log []string) string {
	if len(log) == 0 {
		return ""
	}
	return log[0]
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

func sigOf(detail string) string {
	d := strings.ReplaceAll(detail, "\n", " ")
	if len(d) > 160 {
		d = d[:160]
	}
	return d
}

// ---- finding persistence ----

type findingsLog struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func openFindings(dir string) (*findingsLog, error) {
	fd := filepath.Join(dir, "findings")
	if err := os.MkdirAll(fd, 0755); err != nil {
		return nil, err
	}
	p := filepath.Join(fd, "index.jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &findingsLog{f: f, path: p}, nil
}

func (fl *findingsLog) record(dir string, o CaseOutcome, src []byte, day string) (string, error) {
	base := fmt.Sprintf("c%07d-%s", o.Idx, o.Class)
	luaPath := filepath.Join(dir, "findings", base+".lua")
	if err := atomicWrite(luaPath, src, 0o644); err != nil {
		return "", err
	}
	// re-run for the .txt? No — persist what the soak already saw by
	// re-running cheaply is wasteful; store the classification and the
	// repro command (the case regenerates deterministically).
	txt := fmt.Sprintf("case:   %s (idx %d)\nclass:  %s\ndetail: %s\nrepro:  luafuzz -replay -seed ? -from %d -to %d\n",
		CaseName(o.Idx), o.Idx, o.Class, o.Detail, o.Idx, o.Idx)
	if err := atomicWrite(filepath.Join(dir, "findings", base+".txt"), []byte(txt), 0o644); err != nil {
		return "", err
	}
	rec := FindingRecord{Idx: o.Idx, Case: CaseName(o.Idx), Class: string(o.Class),
		Sig: o.Sig, Day: day, File: base + ".lua"}
	b, _ := json.Marshal(rec)
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if _, err := fl.f.Write(append(b, '\n')); err != nil {
		return "", err
	}
	if err := fl.f.Sync(); err != nil {
		return "", err
	}
	return base, nil
}

func (fl *findingsLog) close() error {
	if err := fl.f.Sync(); err != nil {
		return err
	}
	return fl.f.Close()
}

func atomicWrite(path string, b []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- the soak chunk loop ----

// ChunkReport summarizes one chunk invocation.
type ChunkReport struct {
	Session    *Session
	Cases      int64
	Execs      int64
	Findings   int64
	WallMs     float64
	LastIdx    int
	CleanExit  bool
	StopReason string
}

// SoakDirLayout creates the state dirs and returns the per-case
// working directory (absolute — the wasm engine chdirs into it before
// its WASI preopen, so a relative path would re-resolve wrong).
func SoakDirLayout(dir string) (caseDir string, err error) {
	for _, d := range []string{dir, filepath.Join(dir, "chunks"), filepath.Join(dir, "findings"), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return "", err
		}
	}
	return filepath.Abs(filepath.Join(dir, "tmp"))
}

// RunChunk drives one soak chunk: load state (resume), fuzz until a
// stop condition (ctx cancel, MaxCases, MaxDur), snapshot, return.
// Interruptions at any point are safe — the chunk journal is the
// durable record and LoadState replays past the snapshot.
func RunChunk(ctx context.Context, o Options, engines []testdiff.Engine) (*ChunkReport, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Checkpoint == 0 {
		o.Checkpoint = DefaultCheckpoint
	}
	if o.Deadline == 0 {
		o.Deadline = DefaultDeadline
	}
	if o.NightMinWall == 0 {
		o.NightMinWall = DefaultNightMin
	}
	caseDir, err := SoakDirLayout(o.Dir)
	if err != nil {
		return nil, err
	}

	// load or initialize
	var st *State
	if _, err := os.Stat(filepath.Join(o.Dir, stateFile)); err == nil {
		st, err = LoadState(o.Dir, o.Now)
		if err != nil {
			return nil, fmt.Errorf("load soak: %w", err)
		}
	} else {
		st = newState(o)
		if err := SaveState(o.Dir, st); err != nil {
			return nil, err
		}
	}

	sessID := int64(len(st.Sessions)) + 1
	host, _ := os.Hostname()
	sess := &Session{
		ID: sessID, Start: nowISO(o.Now()), Engines: strings.Join(o.Engines, ","),
		Host: host,
		Nets: []NetPoint{{Time: nowISO(o.Now()), IP: OutboundIP(), Net: NetLabel(OutboundIP(), o.Nets)}},
	}
	st.Sessions = append(st.Sessions, *sess)

	cw, err := openChunk(o.Dir, sessID, st.NextCase)
	if err != nil {
		return nil, err
	}
	defer cw.close()
	fl, err := openFindings(o.Dir)
	if err != nil {
		return nil, err
	}
	defer fl.close()

	rep := &ChunkReport{Session: sess}
	chunkStart := o.Now()
	lastSnap := chunkStart
	lastIP := sess.Nets[0].IP
	stopped := false

	// A wedge marker left by a wall-guarded exit for a case this chunk
	// has already passed (it completed elsewhere) is stale — drop it.
	if m := readWedgeMarker(o.Dir); m != nil && m.Idx < st.NextCase {
		clearWedgeMarker(o.Dir)
	}

	for !stopped {
		select {
		case <-ctx.Done():
			stopped, rep.StopReason = true, ctx.Err().Error()
		default:
		}
		if stopped {
			break
		}
		if o.MaxCases > 0 && int(rep.Cases) >= o.MaxCases {
			stopped, rep.StopReason = true, "case cap"
			break
		}
		if o.MaxDur > 0 && o.Now().Sub(chunkStart) >= o.MaxDur {
			stopped, rep.StopReason = true, "duration cap"
			break
		}

		idx := st.NextCase
		src := Generate(st.MasterSeed, idx)

		var out CaseOutcome
		if err := ValidSource(src, CaseName(idx)); err != nil {
			out = CaseOutcome{Idx: idx, Class: ClassGenBug, Execs: 0,
				Detail: err.Error()}
			out.Sig = sigOf(firstWords(err.Error(), 8))
		} else if m := readWedgeMarker(o.Dir); m != nil && m.Idx == idx && m.Count >= 2 {
			// The case wedged a FRESH process too — not process-state
			// noise but a real un-pollable hang (the M6d blind spot
			// made visible). Record it as a finding and move on.
			out = CaseOutcome{Idx: idx, Class: ClassWedge, Execs: 0,
				Detail: "case wedged past the wall guard in two processes"}
			out.Sig = sigOf(out.Detail)
			clearWedgeMarker(o.Dir)
		} else {
			// Wall guard: a legit case is bounded by the per-case
			// deadline (a few seconds worst case). Anything still
			// running after 90 s means the PROCESS is wedged (the
			// overnight failure mode: wasmtime resource accumulation
			// past ~10k runs). Mark, exit hard; the outer runner loop
			// restarts the chunk — a fresh process replays this case
			// fine (verified); a second wedge on the same case (the
			// marker survives, armWallGuard increments it) promotes it
			// to a WEDGE finding in the branch above.
			guard := armWallGuard(o.Dir, idx, 90*time.Second)
			out = RunCase(engines, caseDir, idx, src)
			guard.Stop()
			clearWedgeMarker(o.Dir) // completed: any wedge mark is stale
		}
		day := nowISO(o.Now())

		// Bound the process: every wasm-engine run creates a wasmtime
		// Store that statically maps the blob's 256 MiB max-memory plus
		// ~8 GB of arm64 guard region — freed only by Go finalizers.
		// At the GC's own cadence ~4k stores pile up (RSS ~6 GB, VSZ
		// ~32 TB) and the kernel silently kills the process at ~case
		// 3980 (three independent runs, no traceback even under
		// GOTRACEBACK=crash). Forcing collection every 64 cases keeps
		// the mapping count flat; every 512 also returns RSS.
		if rep.Cases%64 == 63 {
			runtime.GC()
			if rep.Cases%512 == 511 {
				debug.FreeOSMemory()
			}
		}

		// journal first — the durable record survives anything
		jl := journalLine{I: idx, Cls: string(out.Class), Ms: out.Ms, X: out.Execs, T: day}
		force := isFindingClass(out.Class)
		if err := cw.append(jl, force); err != nil {
			return rep, fmt.Errorf("journal append: %w", err)
		}

		st.applyOutcome(idx, out.Class, out.Ms, out.Execs, day)
		st.NextCase = idx + 1
		rep.Cases++
		rep.Execs += int64(out.Execs)
		rep.WallMs += out.Ms
		rep.LastIdx = idx

		if isFindingClass(out.Class) {
			rep.Findings++
			if _, err := fl.record(o.Dir, out, src, day); err != nil {
				return rep, fmt.Errorf("record finding: %w", err)
			}
			st.registerSig(string(out.Class), out.Sig, idx, day, "")
		}

		// snapshot periodically; also pick up network moves
		if o.Now().Sub(lastSnap) >= o.Checkpoint {
			ip := OutboundIP()
			if ip != lastIP {
				sess.Nets = append(sess.Nets, NetPoint{Time: nowISO(o.Now()), IP: ip, Net: NetLabel(ip, o.Nets)})
				lastIP = ip
			}
			cur := &st.Sessions[len(st.Sessions)-1]
			cur.Cases, cur.Execs, cur.WallMs, cur.Findings = rep.Cases, rep.Execs, rep.WallMs, rep.Findings
			if err := SaveState(o.Dir, st); err != nil {
				return rep, err
			}
			lastSnap = o.Now()
		}
	}

	// session close-out (a hard kill never reaches here — the journal
	// carries those cases instead)
	rep.CleanExit = true
	sess.End = nowISO(o.Now())
	sess.Cases, sess.Execs, sess.WallMs, sess.Findings = rep.Cases, rep.Execs, rep.WallMs, rep.Findings
	sess.CleanExit = true
	st.Sessions[len(st.Sessions)-1] = *sess
	if err := SaveState(o.Dir, st); err != nil {
		return rep, err
	}
	return rep, nil
}

func newState(o Options) *State {
	now := o.Now()
	seed := o.MasterSeed
	if seed == 0 {
		seed = now.UnixNano()
	}
	return &State{
		Version: stateVersion, MasterSeed: seed, NextCase: 0,
		Created: nowISO(now), Updated: nowISO(now),
		Classes: map[string]int64{}, Signatures: map[string]SigInfo{},
		Nights: map[string]NightInfo{}, Sessions: []Session{},
	}
}

func openChunk(dir string, sessID int64, startIdx int) (*chunkWriter, error) {
	p := filepath.Join(dir, "chunks", fmt.Sprintf("s%04d-c%07d.jsonl", sessID, startIdx))
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	return &chunkWriter{f: f, path: p, lastSync: time.Now()}, nil
}
