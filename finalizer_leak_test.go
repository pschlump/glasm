package lua

import (
	"runtime"
	"testing"
)

// TestClosedStatesNotPinnedByFinalizers: every finalizer registration
// holds a runtime.SetFinalizer closure that captures the Global, which
// transitively references the finalized object through the globals
// table — runtime.SetFinalizer's self-reference trap. Before Close's
// clearRegisteredFinalizers sweep (docs/Perf-vs-C-Lua.md's interp open
// item), even one pending finalizer pinned the whole closed state graph
// (~150KB/run: the io standard streams alone were enough; ~44MB at 300
// fresh-state runs). The full-library nop leg covers io; the __gc leg
// covers the script-visible general case.
func TestClosedStatesNotPinnedByFinalizers(t *testing.T) {
	scripts := []string{
		// full-open nop: the io standard streams' __gc registrations
		"local n = 1 return n",
		// a table with __gc left reachable from the globals table
		"local t = setmetatable({}, {__gc = function() end}) _G.pinned = t return 1",
	}
	for _, src := range scripts {
		measure := func(states int) uint64 {
			for i := 0; i < states; i++ {
				L := NewState()
				if err := L.DoString(src); err != nil {
					t.Fatal(err)
				}
				L.Close()
			}
			runtime.GC()
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			return ms.HeapAlloc
		}
		base := measure(25) // allocator/GC high-water settles here
		after := measure(300)
		if growth := after - base; growth > 8<<20 {
			t.Fatalf("script %q: %d bytes retained across 300 closed states (base %d, after %d) — finalizer pin regression",
				src, growth, base, after)
		}
	}
}
