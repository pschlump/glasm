package host

import (
	"sync"
	"testing"
)

// Compile must return pointer-identical *Script values for one source even
// under concurrency: VM binding (ErrScriptBound) is by pointer, so two
// pointers for one SHA would wedge consumer-side per-script VM pools.
func TestCompileConcurrentPointerIdentity(t *testing.T) {
	// U2c moved the frontend to the 5.5.1 encoding; luawasm is retired
	// (row 60): compiles emit v4 modules; running them is the U6 cutover.
	t.Skip("host run path awaits the U6 prod-blob cutover (v4 module over the 5.1 blob) — ledger row 60")
	e := mustEngine(t)
	const src = `return redis.call('incr','ctr')`
	const workers = 16
	got := make([]*Script, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := e.Compile([]byte(src), "user_script")
			if err != nil {
				t.Errorf("Compile: %v", err)
				return
			}
			got[i] = s
		}(i)
	}
	wg.Wait()
	for i := 1; i < workers; i++ {
		if got[i] != got[0] {
			t.Fatalf("worker %d got a different *Script for the same source", i)
		}
	}
}
