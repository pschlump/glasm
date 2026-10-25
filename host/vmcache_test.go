package host

// WithVMCacheSize gates (wazero-performance-001): pool reuse per script,
// LRU capacity eviction, watermark recycling, error-state reuse, and the
// concurrency contract. White-box assertions use e.vms (NewVM count) and
// e.pool directly — the run results themselves must stay identical to the
// fresh-VM posture.

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func runInt(t *testing.T, e *Engine, s *Script, opt RunOptions) int64 {
	t.Helper()
	res, err := e.Run(context.Background(), s, opt)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Values) != 1 || res.Values[0].Kind != KindInteger {
		t.Fatalf("unexpected result: %+v", res.Values)
	}
	return res.Values[0].Int
}

func TestVMCacheReusePerScript(t *testing.T) {
	e, err := NewEngine(WithVMCacheSize(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := mustCompile(t, e, "return #KEYS + #ARGV")

	if got := runInt(t, e, s, RunOptions{Keys: []string{"a"}, Argv: []string{"1"}}); got != 2 {
		t.Fatalf("run 1 = %d", got)
	}
	if e.vms != 1 {
		t.Fatalf("vms = %d after first run", e.vms)
	}
	// same source, different arguments: pooled image, fresh staging
	if got := runInt(t, e, s, RunOptions{Keys: []string{"a", "b"}, Argv: []string{"1", "2", "3"}}); got != 5 {
		t.Fatalf("run 2 = %d", got)
	}
	if e.vms != 1 {
		t.Fatalf("vms = %d after pooled rerun (image not reused)", e.vms)
	}
	if e.pool.Len() != 1 {
		t.Fatalf("pool len = %d", e.pool.Len())
	}
}

func TestVMCacheDistinctScriptsDistinctImages(t *testing.T) {
	e, err := NewEngine(WithVMCacheSize(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	a := mustCompile(t, e, "return 11")
	b := mustCompile(t, e, "return 22")

	runInt(t, e, a, RunOptions{})
	runInt(t, e, b, RunOptions{})
	if e.vms != 2 {
		t.Fatalf("vms = %d (distinct sources must not share an image)", e.vms)
	}
	runInt(t, e, a, RunOptions{})
	runInt(t, e, b, RunOptions{})
	if e.vms != 2 {
		t.Fatalf("vms = %d after pooled reruns", e.vms)
	}
}

func TestVMCacheLRUEviction(t *testing.T) {
	e, err := NewEngine(WithVMCacheSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	a := mustCompile(t, e, "return 1")
	b := mustCompile(t, e, "return 2")

	runInt(t, e, a, RunOptions{}) // pool: {a}
	runInt(t, e, b, RunOptions{}) // full: evict a, pool: {b}
	if e.pool.Len() != 1 {
		t.Fatalf("pool len = %d", e.pool.Len())
	}
	if e.vms != 2 {
		t.Fatalf("vms = %d", e.vms)
	}
	runInt(t, e, a, RunOptions{}) // miss: a was evicted → fresh VM
	if e.vms != 3 {
		t.Fatalf("vms = %d after re-running the evicted script", e.vms)
	}
}

func TestVMCacheScriptErrorKeepsImagePooled(t *testing.T) {
	e, err := NewEngine(WithVMCacheSize(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := mustCompile(t, e, "if KEYS[1] == 'boom' then error('script failure') end return 7")

	if _, err := e.Run(context.Background(), s, RunOptions{Keys: []string{"boom"}}); err == nil {
		t.Fatal("expected script error")
	} else if _, ok := err.(*ScriptError); !ok {
		t.Fatalf("error class = %T", err)
	}
	if e.pool.Len() != 1 {
		t.Fatalf("pool len = %d after guest-level error (image should stay pooled)", e.pool.Len())
	}
	// the unwound image serves the next run correctly
	if got := runInt(t, e, s, RunOptions{Keys: []string{"ok"}}); got != 7 {
		t.Fatalf("rerun = %d", got)
	}
	if e.vms != 1 {
		t.Fatalf("vms = %d (erroring image was not reused)", e.vms)
	}
}

func TestVMCacheWatermarkRecycle(t *testing.T) {
	// budget 4 MiB → recycle at 3 MiB; the run allocates past it, so the
	// image must close instead of pooling (UsedBytes grows monotonically
	// on a frozen-GC image).
	e, err := NewEngine(WithVMCacheSize(2), WithMemoryBudgetBytes(4<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := mustCompile(t, e, "local t = string.rep('x', 3*1024*1024) return #t")

	if got := runInt(t, e, s, RunOptions{}); got != 3*1024*1024 {
		t.Fatalf("run = %d", got)
	}
	if e.pool.Len() != 0 {
		t.Fatalf("pool len = %d past the recycle watermark", e.pool.Len())
	}
	runInt(t, e, s, RunOptions{})
	if e.vms != 2 {
		t.Fatalf("vms = %d (watermark did not recycle the image)", e.vms)
	}
}

func TestVMCacheConcurrency(t *testing.T) {
	e, err := NewEngine(WithVMCacheSize(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	a := mustCompile(t, e, "local s=0 for i=1,2000 do s=s+i end return s")
	b := mustCompile(t, e, "local s='' for i=1,50 do s=s..i end return #s")

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for round := 0; round < 4; round++ {
		for i := 0; i < 8; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				if runInt(t, e, a, RunOptions{}) != 2001000 {
					errs <- errBad("a")
				}
			}()
			go func() {
				defer wg.Done()
				if runInt(t, e, b, RunOptions{}) != 91 {
					errs <- errBad("b")
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if e.pool.Len() > 2 {
		t.Fatalf("pool len = %d over capacity", e.pool.Len())
	}
}

// errBad avoids capturing loop variables in goroutines above.
func errBad(which string) error {
	return &testErr{"wrong result for script " + which}
}

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }

func TestVMCacheCloseDrains(t *testing.T) {
	e, err := NewEngine(WithVMCacheSize(2))
	if err != nil {
		t.Fatal(err)
	}
	a := mustCompile(t, e, "return 1")
	runInt(t, e, a, RunOptions{})
	if e.pool.Len() != 1 {
		t.Fatalf("pool len = %d before Close", e.pool.Len())
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if e.pool.Len() != 0 {
		t.Fatalf("pool len = %d after Close (engine-owned images must drain)", e.pool.Len())
	}
	if _, err := e.Run(context.Background(), a, RunOptions{}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Run after Close = %v", err)
	}
}

func TestVMCacheDisabledDefault(t *testing.T) {
	// the default posture is unchanged: no pool, every run a fresh VM
	e, err := NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.pool != nil {
		t.Fatal("default engine has a pool")
	}
	s := mustCompile(t, e, "return 1")
	runInt(t, e, s, RunOptions{})
	runInt(t, e, s, RunOptions{})
	if e.vms != 2 {
		t.Fatalf("vms = %d (default must not pool)", e.vms)
	}
}
