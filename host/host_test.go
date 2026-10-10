package host

// M7a/M8a gates (design §6.3, guide §4.7): the run protocol, host
// functions, caps, deadline, determinism, and the A9 concurrency
// contract — the last under -race, which is the CI requirement.

import (
	"context"
	"errors"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
)

// noHostGC disables Go GC for the test's duration (ledger row 99): a Go
// GC mark-STW landing inside a guest watchdog window wedges the process —
// the spinning wasm loop runs as unpreemptible wazero-JIT code, the STW
// waits on its P forever, and the scheduler freeze takes the timer
// goroutines down with it, so the deadline flag write that would end the
// spin never happens. Probabilistic (~30% here in plain mode; -race only
// shifts the GC phase). Watchdog tests quiesce the host this way so they
// assert the kill contract deterministically; the daemon-side durable fix
// is U7.
func noHostGC(t *testing.T) {
	t.Helper()
	old := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(old) })
}

func mustEngine(t *testing.T, opts ...Option) *Engine {
	t.Helper()
	e, err := NewEngine(opts...)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func mustCompile(t *testing.T, e *Engine, src string) *Script {
	t.Helper()
	s, err := e.Compile([]byte(src), "=test")
	if err != nil {
		t.Fatalf("Compile(%q): %v", src, err)
	}
	return s
}

func runScript(t *testing.T, e *Engine, src string, opt RunOptions) (Result, error) {
	t.Helper()
	s := mustCompile(t, e, src)
	return e.Run(context.Background(), s, opt)
}

// --- blob contract ---------------------------------------------------------

func TestBlobPinned(t *testing.T) {
	if err := verifyBlob(prodBlob); err != nil {
		t.Fatalf("embedded blob: %v", err)
	}
	if len(prodBlob) < 100<<10 {
		t.Fatalf("embedded blob suspiciously small: %d", len(prodBlob))
	}
}

// --- basics: compile, run, results, KEYS/ARGV ------------------------------

func TestEvalBasics(t *testing.T) {
	e := mustEngine(t)
	cases := []struct {
		src  string
		want []Value
	}{
		{"return 1+1", []Value{Int(2)}},
		{"return 'hello'", []Value{String("hello")}},
		{"return 3.5", []Value{Number(3.5)}},
		{"return true", []Value{Bool(true)}},
		{"return nil", []Value{Nil()}},
		{"return", nil},
		{"return 1, 'two', true, nil", []Value{Int(1), String("two"), Bool(true), Nil()}},
		{"return KEYS[1]..ARGV[1]", []Value{String("ab")}},
		{"return #KEYS, #ARGV", []Value{Int(1), Int(2)}},
		{"return {1, 2, {3, 'four'}}", []Value{Table(
			KV{Key: Int(1), Val: Int(1)},
			KV{Key: Int(2), Val: Int(2)},
			KV{Key: Int(3), Val: Table(
				KV{Key: Int(1), Val: Int(3)},
				KV{Key: Int(2), Val: String("four")},
			)},
		)}},
	}
	for _, c := range cases {
		res, err := runScript(t, e, c.src, RunOptions{Keys: []string{"a"}, Argv: []string{"b", "c"}})
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if len(res.Values) != len(c.want) {
			t.Errorf("%q: got %d values, want %d", c.src, len(res.Values), len(c.want))
			continue
		}
		for i := range c.want {
			if !valuesEqual(res.Values[i], c.want[i]) {
				t.Errorf("%q: value %d: got %#v, want %#v", c.src, i, res.Values[i], c.want[i])
			}
		}
	}
}

// binary-safety of KEYS/ARGV staging (control bytes, quotes, UTF-8-ish
// bytes, embedded NULs)
func TestEvalBinaryArgs(t *testing.T) {
	e := mustEngine(t)
	res, err := runScript(t, e, `return KEYS[1]..ARGV[1]`, RunOptions{
		Keys: []string{"a\x00\xff\"b\\"},
		Argv: []string{"\n\t\r\x01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Values[0].Str
	want := "a\x00\xff\"b\\\n\t\r\x01"
	if got != want {
		t.Fatalf("binary round-trip: got %q, want %q", got, want)
	}
}

func TestCompileErrors(t *testing.T) {
	e := mustEngine(t)
	if _, err := e.Compile([]byte("return (("), "=bad"); err == nil {
		t.Fatal("syntax error not reported")
	}
	// note: coroutines DO compile (the sandbox keeps the coroutine lib);
	// their engine divergence is a ledgered differential skip (row 22),
	// not a v1 compile rejection
}

func TestScriptRuntimeError(t *testing.T) {
	e := mustEngine(t)
	_, err := runScript(t, e, `error("boom")`, RunOptions{})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.ErrValue.String(), "boom") {
		t.Fatalf("error value %q missing boom", se.ErrValue.String())
	}
	// pcall catches; ok=false + the message survive the round trip. The
	// level-0 raise gains the position prefix on the v4 blob — the ruled
	// row-85b residual (luaB_error's level-0 arm calls no luaL_where, so
	// the seam's nopos verdict never fires and the catching rt_run
	// prefixes; stock Lua would answer bare "inner")
	res, err := runScript(t, e, `local ok, e = pcall(function() error("inner", 0) end) return ok, e`, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[0].BoolVal() || res.Values[1].Str != "=test:1: inner" {
		t.Fatalf("pcall round-trip: %#v", res.Values)
	}
}

// --- host functions (the redis.call seam) ----------------------------------

func registerEcho(t *testing.T, e *Engine) {
	t.Helper()
	err := e.RegisterGlobal("redis", "call", func(vm *VM, args []Value) ([]Value, error) {
		return args, nil // echo
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHostFunctionRoundTrip(t *testing.T) {
	e := mustEngine(t)
	registerEcho(t, e)
	res, err := runScript(t, e,
		`return redis.call('SET', KEYS[1], 42, true)`, RunOptions{Keys: []string{"k"}})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Values
	want := []Value{String("SET"), String("k"), Int(42), Bool(true)}
	if len(got) != len(want) {
		t.Fatalf("got %#v", got)
	}
	for i := range want {
		if !valuesEqual(got[i], want[i]) {
			t.Fatalf("arg %d: got %#v want %#v", i, got[i], want[i])
		}
	}
}

func TestHostFunctionReturns(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "getall", func(vm *VM, args []Value) ([]Value, error) {
		return []Value{
			Table(KV{Key: String("ok"), Val: String("fine")}),                     // status-reply shape
			Table(KV{Key: Int(1), Val: String("a")}, KV{Key: Int(2), Val: Nil()}), // array with hole
			Nil(), Number(1.25), Bool(false),
		}, nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := runScript(t, e, `
		local t, arr = redis.getall()
		return t.ok, arr[1], arr[2], #arr, 1.25, false
	`, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[0].Str != "fine" || res.Values[1].Str != "a" || res.Values[2].Kind != KindNil {
		t.Fatalf("table return: %#v", res.Values)
	}
	// arr[2] = nil is an ABSENT key in Lua — the table has one entry
	if !res.Values[3].Equal(Int(1)) {
		t.Fatalf("array length: %#v", res.Values[3])
	}
}

func TestHostFunctionErrors(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "call", func(vm *VM, args []Value) ([]Value, error) {
		return nil, errFake("ERR wrong number of arguments for 'get' command")
	}); err != nil {
		t.Fatal(err)
	}
	// redis.call semantics: the error aborts the script, text verbatim
	_, err := runScript(t, e, `redis.call('GET')`, RunOptions{})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if se.ErrValue.Str != "ERR wrong number of arguments for 'get' command" {
		t.Fatalf("verbatim error text: %q", se.ErrValue.Str)
	}
	// pcall catches it as a string value
	res, err := runScript(t, e, `local ok, e = pcall(redis.call, 'GET') return ok, e`, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[0].BoolVal() || res.Values[1].Str != "ERR wrong number of arguments for 'get' command" {
		t.Fatalf("pcall of failing hostfn: %#v", res.Values)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

func TestHostFunctionPanicContained(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "boom", func(vm *VM, args []Value) ([]Value, error) {
		panic("kaboom")
	}); err != nil {
		t.Fatal(err)
	}
	_, err := runScript(t, e, `redis.boom()`, RunOptions{})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.ErrValue.String(), "host function panic: kaboom") {
		t.Fatalf("panic containment: %q", se.ErrValue.String())
	}
}

// Trampolines execute on the lock-holding goroutine (A9): the image
// mutex must be held for the whole trampoline.
func TestTrampolinesHoldImageLock(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "call", func(vm *VM, args []Value) ([]Value, error) {
		if vm.mu.TryLock() {
			vm.mu.Unlock()
			return nil, errFake("FATAL: image lock NOT held during hostfn trampoline")
		}
		return []Value{String("ok")}, nil
	}); err != nil {
		t.Fatal(err)
	}
	res, err := runScript(t, e, `return redis.call('PING')`, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Values[0].Str != "ok" {
		t.Fatalf("lock assertion fired: %#v", res.Values)
	}
}

// A HostFunc re-entering its own VM must get ErrReentrant as a clean
// script error, not a deadlock.
func TestHostFunctionReentrancyRefused(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "nest", func(vm *VM, args []Value) ([]Value, error) {
		s, err := vm.engine().Compile([]byte("return 7"), "=nested")
		if err != nil {
			return nil, err
		}
		res, err := vm.Run(context.Background(), s, RunOptions{})
		if err != nil {
			return nil, err
		}
		return res.Values, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := runScript(t, e, `return redis.nest()`, RunOptions{})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.Error(), ErrReentrant.Error()) {
		t.Fatalf("reentrancy refusal: %q", se.Error())
	}
}

// --- chaos host: malformed host behavior must not trap ----------------------

func TestChaosHost(t *testing.T) {
	e := mustEngine(t)
	if err := e.RegisterGlobal("redis", "chaos", func(vm *VM, args []Value) ([]Value, error) {
		switch args[0].Str {
		case "many":
			vals := make([]Value, 5000) // over the guest's 4096 cap
			for i := range vals {
				vals[i] = Nil()
			}
			return vals, nil
		case "deep":
			v := Nil()
			for i := 0; i < 100; i++ { // over the guest's 64-deep decode guard
				v = Table(KV{Key: Int(1), Val: v})
			}
			return []Value{v}, nil
		case "big":
			return []Value{String(strings.Repeat("x", 300<<10))}, nil // 300 KB > the 256 KB guest staging cap
		case "okbig":
			return []Value{String(strings.Repeat("x", 100<<10))}, nil // fits
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{
		`return redis.chaos('many')`,
		`return redis.chaos('deep')`,
		`return redis.chaos('big')`, // over the staging cap: clean error
	} {
		_, err := runScript(t, e, src, RunOptions{})
		if _, ok := err.(*ScriptError); !ok {
			t.Errorf("%q: want clean ScriptError, got %T %v", src, err, err)
		}
	}
	res, err := runScript(t, e, `return #redis.chaos('okbig')`, RunOptions{})
	if err != nil {
		t.Fatalf("big string: %v", err)
	}
	if !res.Values[0].Equal(Int(100 << 10)) {
		t.Fatalf("big string length: %#v", res.Values[0])
	}
}

// --- deadline ----------------------------------------------------------------

func TestDeadlineKill(t *testing.T) {
	noHostGC(t)
	e := mustEngine(t)
	s := mustCompile(t, e, `while true do end`)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	start := time.Now()
	_, err = vm.Run(context.Background(), s, RunOptions{Deadline: 250 * time.Millisecond})
	if time.Since(start) > 5*time.Second {
		t.Fatalf("deadline did not fire promptly (%v)", time.Since(start))
	}
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.Error(), "context deadline exceeded") {
		t.Fatalf("deadline text: %q", se.Error())
	}
	// the image is reusable: the sticky flag and staged error were
	// cleared, and the same script can be killed again cleanly (a re-run
	// with no deadline would loop forever — same script, one-script law)
	_, err = vm.Run(context.Background(), s, RunOptions{Deadline: 250 * time.Millisecond})
	se2, ok := err.(*ScriptError)
	if !ok || !strings.Contains(se2.Error(), "context deadline exceeded") {
		t.Fatalf("second run after kill: %T %v — image wedged", err, err)
	}
}

// TestDeadlineSTWWedge is row 100's covering probe (the U7a fix): force
// the row-99 conditions — a spinning unbounded guest loop with GC
// mark-STWs landing throughout its watchdog window — and require the
// deadline kill anyway. Pre-fix this wedged ~30% per process even
// without forcing (the pending STW freezes the scheduler and starves the
// flag-writing timer); with GC percent pinned at 1 plus a churn
// goroutine the collision becomes near-certain. The row-100 backstop
// answers from the only goroutine still executing (the guest itself,
// via host.deadline_poll), the kill unwinds through pure-Go frames, and
// the post-run liveness check proves any pending STW drained — the
// process recovers instead of wedging.
func TestDeadlineSTWWedge(t *testing.T) {
	old := debug.SetGCPercent(1)
	t.Cleanup(func() { debug.SetGCPercent(old) })
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() { // churn: keep the heap moving so mark phases keep starting
		buf := make([]byte, 1<<20)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for i := range buf {
				buf[i] = byte(i)
			}
			buf = append(buf, make([]byte, 1<<20)...)
			if len(buf) >= 8<<20 {
				buf = make([]byte, 1<<20)
			}
		}
	}()
	e := mustEngine(t)
	s := mustCompile(t, e, `while true do end`)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	// self-verification (the row-100 evidence requirement): bracket the
	// run with GC counters and prove the collision this probe exists to
	// force actually happened. With the guest spinning unpreemptibly for
	// the whole window, a cycle COMPLETING in-window means its mark STW
	// was satisfied through the poll's Go-frames safe point — the row-99
	// wedge, survived. NumGC/PauseTotalNs deltas, not the pauses
	// histogram (its STW-wait boundary semantics are the disputed part;
	// these counters are unambiguous). Without this the probe can pass
	// green having exercised nothing (GC never engaged).
	var ms0, ms1 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	start := time.Now()
	_, err = vm.Run(context.Background(), s, RunOptions{Deadline: 250 * time.Millisecond})
	if dt := time.Since(start); dt > 5*time.Second {
		t.Fatalf("deadline did not fire promptly (%v)", dt)
	}
	runtime.ReadMemStats(&ms1)
	t.Logf("row-100 collision evidence: %d GC cycles completed in-window, %d pause-ns total (kill at %v)",
		ms1.NumGC-ms0.NumGC, ms1.PauseTotalNs-ms0.PauseTotalNs, time.Since(start))
	if ms1.NumGC-ms0.NumGC < 1 {
		t.Fatal("no GC cycle completed during the run window — the probe proved nothing (collision not forced)")
	}
	if ms1.PauseTotalNs <= ms0.PauseTotalNs {
		t.Fatal("no STW pause recorded during the run window — the probe proved nothing (collision not forced)")
	}
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.Error(), "context deadline exceeded") {
		t.Fatalf("deadline text: %q", se.Error())
	}
	// post-run liveness: a fresh goroutine must round-trip — a pending
	// STW that never drained would freeze it (and this test's own timer)
	// with it
	live := make(chan struct{})
	go func() { close(live) }()
	select {
	case <-live:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler did not recover after the kill — pending STW never drained")
	}
}

// The second half of the deadline contract: a bounded script under a
// generous deadline completes normally, and ctx cancellation arms the
// same watchdog.
func TestDeadlineNormalAndCancel(t *testing.T) {
	noHostGC(t) // the cancel leg spins the same unbounded guest loop
	e := mustEngine(t)
	if _, err := runScript(t, e, `local s=0 for i=1,100000 do s=s+i end return s`,
		RunOptions{Deadline: 30 * time.Second}); err != nil {
		t.Fatalf("bounded script failed: %v", err)
	}
	s := mustCompile(t, e, `while true do end`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	_, err := e.Run(ctx, s, RunOptions{})
	// cancellation kills either way: wazero's Call(ctx) aborts outright
	// (context.Canceled), or the watchdog flips the guest flag and the
	// back-edge poll raises the deadline error — both are clean kills
	if se, ok := err.(*ScriptError); ok {
		if !strings.Contains(se.Error(), "context deadline exceeded") {
			t.Fatalf("ctx cancel kill (script error): %q", se.Error())
		}
		return
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx cancel kill: %T %v", err, err)
	}
}

// --- memory cap --------------------------------------------------------------

func TestMemoryBudget(t *testing.T) {
	noHostGC(t) // spins `while true do s = s .. s end` until the budget raises
	e := mustEngine(t, WithMemoryBudgetBytes(4<<20))
	_, err := runScript(t, e, `local s = 'x' while true do s = s .. s end`,
		RunOptions{Deadline: 60 * time.Second})
	se, ok := err.(*ScriptError)
	if !ok {
		t.Fatalf("want ScriptError, got %T %v", err, err)
	}
	if !strings.Contains(se.Error(), "not enough memory") {
		t.Fatalf("oom text: %q", se.Error())
	}
}

// --- determinism ---------------------------------------------------------------

func TestDeterminism(t *testing.T) {
	e := mustEngine(t)
	src := `local t = {} for i = 1, 16 do t[i] = math.random(1, 1000000) end return t`
	var first *Result
	for i := 0; i < 5; i++ {
		res, err := runScript(t, e, src, RunOptions{Seed: 99})
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			r := res
			first = &r
			continue
		}
		for j := range first.Values[0].Pairs {
			// math.random yields integers — they cross as KindInteger
			// (U6e wire: payload in .Int, .Num stays 0)
			a, b := first.Values[0].Pairs[j].Val, res.Values[0].Pairs[j].Val
			if a.Int != b.Int {
				t.Fatalf("run %d value %d diverged: %v vs %v", i, j, a.Int, b.Int)
			}
		}
	}
	// a different seed must (virtually certainly) move the stream
	res2, err := runScript(t, e, src, RunOptions{Seed: 100})
	if err != nil {
		t.Fatal(err)
	}
	same := true
	for j := range res2.Values[0].Pairs {
		if first.Values[0].Pairs[j].Val.Int != res2.Values[0].Pairs[j].Val.Int {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different seed produced identical stream")
	}
}

// --- sandbox --------------------------------------------------------------------

func TestSandbox(t *testing.T) {
	e := mustEngine(t)
	for _, name := range []string{"os", "io", "require", "loadstring", "load", "dofile", "debug"} {
		_, err := runScript(t, e, `return `+name, RunOptions{})
		if err != nil {
			t.Errorf("checking %s: %v", name, err)
			continue
		}
		res, err := runScript(t, e, `return tostring(`+name+`)`, RunOptions{})
		if err != nil {
			t.Errorf("tostring(%s): %v", name, err)
			continue
		}
		if !strings.Contains(res.Values[0].Str, "nil") {
			t.Errorf("%s is not nil in the sandbox: %q", name, res.Values[0].Str)
		}
	}
}

// --- concurrency (A9) — the -race CI requirement -------------------------------

func TestConcurrentFreshVMs(t *testing.T) {
	e := mustEngine(t)
	const W, N = 8, 15 // goroutines × runs
	var wg sync.WaitGroup
	errs := make(chan error, W*N)
	for w := 0; w < W; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			s, err := e.Compile([]byte(strings.Repeat("local x = 1 ", 50)+
				`return KEYS[1]..':'..ARGV[1]..':'..#KEYS`), "=w")
			if err != nil {
				errs <- err
				return
			}
			for i := 0; i < N; i++ {
				res, err := e.Run(context.Background(), s, RunOptions{
					Keys: []string{"k" + string(rune('a'+w))},
					Argv: []string{string(rune('A' + w))},
				})
				if err != nil {
					errs <- err
					return
				}
				if got := res.Values[0].Str; got != "k"+string(rune('a'+w))+":"+string(rune('A'+w))+":1" {
					errs <- errFake("bad result " + got)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestSharedVMSerialization(t *testing.T) {
	e := mustEngine(t)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	s := mustCompile(t, e, `x = (x or 0) + 1 return x`)
	const N = 16
	var wg sync.WaitGroup
	results := make([]int64, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := vm.Run(context.Background(), s, RunOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			// the counter is integer arithmetic end-to-end: KindInteger
			// crosses lossless (U6e), so read .Int — not .Num (0 for
			// integer kinds)
			results[i] = res.Values[0].Int
		}(i)
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, r := range results {
		if seen[r] {
			t.Fatalf("duplicate counter value %d — runs interleaved", r)
		}
		seen[r] = true
	}
	for i := int64(1); i <= N; i++ {
		if !seen[i] {
			t.Fatalf("missing counter value %d (got %v)", i, results)
		}
	}
}

func TestOneScriptLaw(t *testing.T) {
	e := mustEngine(t)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	defer vm.Close()
	a := mustCompile(t, e, `return 1`)
	b := mustCompile(t, e, `return 2`)
	if _, err := vm.Run(context.Background(), a, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.Run(context.Background(), b, RunOptions{}); err != ErrScriptBound {
		t.Fatalf("want ErrScriptBound, got %v", err)
	}
	// the same script re-runs fine (shared-state mode)
	res, err := vm.Run(context.Background(), a, RunOptions{})
	if err != nil || !res.Values[0].Equal(Int(1)) {
		t.Fatalf("re-run: %v %#v", err, res.Values)
	}
}

// --- print event sink ------------------------------------------------------------

func TestEventSink(t *testing.T) {
	var mu sync.Mutex
	var got [][]Value
	e := mustEngine(t, WithEventSink(func(vm *VM, args []Value) {
		mu.Lock()
		got = append(got, args)
		mu.Unlock()
	}))
	if _, err := runScript(t, e, `print('hello', 42, true, nil)`, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || len(got[0]) != 4 {
		t.Fatalf("print events: %#v", got)
	}
	if got[0][0].Str != "hello" || !got[0][1].Equal(Int(42)) || !got[0][2].BoolVal() ||
		got[0][3].Kind != KindNil {
		t.Fatalf("print args: %#v", got[0])
	}
}

// --- helpers ----------------------------------------------------------------------

func valuesEqual(a, b Value) bool { return a.Equal(b) }

func (v Value) Equal(o Value) bool {
	if v.Kind != o.Kind {
		return false
	}
	switch v.Kind {
	case KindNumber:
		return v.Num == o.Num
	case KindInteger:
		return v.Int == o.Int // U6e: integers cross lossless — compare the payload
	case KindString:
		return v.Str == o.Str
	case KindTable:
		if len(v.Pairs) != len(o.Pairs) || v.More != o.More {
			return false
		}
		for i := range v.Pairs {
			if !v.Pairs[i].Key.Equal(o.Pairs[i].Key) || !v.Pairs[i].Val.Equal(o.Pairs[i].Val) {
				return false
			}
		}
		return true
	}
	return true
}

// engine returns the VM's engine (test helper for the reentrancy probe).
func (vm *VM) engine() *Engine { return vm.e }
