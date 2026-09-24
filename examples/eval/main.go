// Command eval is the minimal host-package demonstration (design §6.3):
// compile once, run with KEYS/ARGV, print the reply values — plus one
// host function so the redis.call seam is visible end to end.
//
//	go run ./examples/eval
package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/pschlump/golua55/host"
)

func main() {
	e, err := host.NewEngine()
	if err != nil {
		log.Fatal(err)
	}

	// the seam a Redis daemon installs: in-script functions backed by Go
	if err := e.RegisterGlobal("redis", "call", func(vm *host.VM, args []host.Value) ([]host.Value, error) {
		name := ""
		if len(args) > 0 {
			name = args[0].String()
		}
		return []host.Value{host.String("PONG from " + name)}, nil
	}); err != nil {
		log.Fatal(err)
	}

	s, err := e.Compile([]byte(`
		local who = ARGV[1] or 'world'
		local pong = redis.call('PING')
		return 'hello ' .. who, #KEYS, pong
	`), "=demo")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("SHA1: %s\n", s.SHA1)

	res, err := e.Run(context.Background(), s, host.RunOptions{
		Keys:     []string{"k1", "k2", "k3"},
		Argv:     []string{"ulta"},
		Seed:     42,              // deterministic math.random stream
		Deadline: 5 * time.Second, // watchdog: clean kill, never a hang
	})
	if err != nil {
		log.Fatal(err)
	}
	for i, v := range res.Values {
		fmt.Printf("result[%d] = %s\n", i, v)
	}
}
