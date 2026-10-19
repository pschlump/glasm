// Command evalserver is the thread-proofing demonstration (design §6.3):
// concurrent EVALs against a pool of locked VMs — the deployment shape a
// daemon actually uses. Measured on darwin/arm64 (the interpreter engine,
// the conservative case): a fresh VM costs ~44 ms (wazero compiles the
// runtime blob per instance) while a re-run on a bound image costs ~60 µs
// — so hot paths pool VMs per script (the v1 one-script-per-image law
// makes the pool per-script by construction). A long-running script is
// killed by the deadline watchdog and the pool keeps serving.
//
//	go run ./examples/evalserver
package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pschlump/glasm/host"
)

func main() {
	e, err := host.NewEngine(host.WithMemoryBudgetBytes(32 << 20))
	if err != nil {
		log.Fatal(err)
	}
	if err := e.RegisterGlobal("redis", "call", func(vm *host.VM, args []host.Value) ([]host.Value, error) {
		// a stand-in for the daemon bridge: echo the command name back
		if len(args) == 0 {
			return nil, fmt.Errorf("ERR wrong number of arguments")
		}
		return []host.Value{host.String(args[0].String() + ":ok")}, nil
	}); err != nil {
		log.Fatal(err)
	}

	fast, err := e.Compile([]byte(`
		local acc = 0
		for i = 1, 1000 do acc = acc + i end
		return KEYS[1], acc, redis.call('INCR')
	`), "=fast")
	if err != nil {
		log.Fatal(err)
	}
	slow, err := e.Compile([]byte(`while true do end`), "=slow")
	if err != nil {
		log.Fatal(err)
	}

	// the pool: M VMs, each bound to the fast script at first Run
	const poolSize = 8
	pool := make(chan *host.VM, poolSize)
	bound := make(chan struct{})
	for i := 0; i < poolSize; i++ {
		vm, err := e.NewVM()
		if err != nil {
			log.Fatal(err)
		}
		defer vm.Close()
		// bind now (one-script law): the first Run fixes the image
		if _, err := vm.Run(context.Background(), fast, host.RunOptions{Keys: []string{"warm"}}); err != nil {
			log.Fatal(err)
		}
		pool <- vm
	}
	close(bound)
	_ = bound

	lease := func() *host.VM { return <-pool }
	release := func(vm *host.VM) { pool <- vm }

	const clients = 16
	const runs = 200
	var wg sync.WaitGroup
	start := time.Now()
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for i := 0; i < runs; i++ {
				vm := lease()
				res, err := vm.Run(context.Background(), fast, host.RunOptions{
					Keys:     []string{fmt.Sprintf("k%d", c)},
					Deadline: 10 * time.Second,
				})
				release(vm)
				if err != nil {
					log.Printf("client %d: %v", c, err)
					return
				}
				if got := res.Values[0].Str; got != fmt.Sprintf("k%d", c) {
					log.Printf("client %d: bad key %q", c, got)
					return
				}
			}
		}(c)
	}

	// a client that gets killed: a 2s deadline on an infinite loop (a
	// fresh VM — the slow script is not the pooled one) — a clean
	// ScriptError while every pooled client keeps running
	_, err = e.Run(context.Background(), slow, host.RunOptions{Deadline: 2 * time.Second})
	if se, ok := err.(*host.ScriptError); ok {
		fmt.Printf("slow script killed cleanly: %s\n", se.Error())
	} else {
		log.Printf("slow script: %v", err)
	}

	wg.Wait()
	fmt.Printf("%d pooled runs over %d VMs in %s (%.0f µs/run)\n",
		clients*runs, poolSize, time.Since(start),
		float64(time.Since(start).Microseconds())/float64(clients*runs))
}
