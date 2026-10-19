// Command sharedvm is the single-shared-VM deployment mode (design
// §6.3): one Lua memory image, many goroutines — VM.Run serializes on
// the image lock (A9) and scripts observe each other's globals, exactly
// like Redis-classic single-state script execution.
//
//	go run ./examples/sharedvm
package main

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/pschlump/glasm/host"
)

func main() {
	e, err := host.NewEngine()
	if err != nil {
		log.Fatal(err)
	}
	vm, err := e.NewVM()
	if err != nil {
		log.Fatal(err)
	}
	defer vm.Close()

	// v1 law: one script per image (proto indices are per-script) — the
	// shared mode re-runs the same script against persistent globals
	s, err := e.Compile([]byte(`
		hits = (hits or 0) + 1
		return hits
	`), "=counter")
	if err != nil {
		log.Fatal(err)
	}

	const workers = 32
	results := make(chan int64, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := vm.Run(context.Background(), s, host.RunOptions{})
			if err != nil {
				log.Print(err)
				return
			}
			// integer arithmetic end-to-end: the counter crosses as
			// KindInteger (payload in .Int — .Num stays 0 for integers)
			results <- res.Values[0].Int
		}()
	}
	wg.Wait()
	close(results)

	sum := int64(0)
	for r := range results {
		sum += r
	}
	// serialized execution: the counter went 1..workers exactly once
	// each — the sum proves no lost update and no interleave
	if want := int64(workers) * (workers + 1) / 2; sum != want {
		log.Fatalf("serialized globals violated: sum=%d want=%d", sum, want)
	}
	fmt.Printf("shared VM: %d serialized runs, hits=%d — Redis-classic globals\n",
		workers, workers)
}
