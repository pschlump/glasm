// Command clua-run runs a Lua script under stock C Lua 5.1 (the wasm
// oracle blob, lua51_sjlj.wasm) with the CLI-arg surface the other
// runners provide — the third engine for _cli-tests:
//
//	glua prog.lua args...        # golua55 interpreter
//	glua -w p.wasm prog.lua ...  # luawasm backend
//	glua -W p.wasm args...       #   (run precompiled)
//	clua-run prog.lua args...    # stock C Lua 5.1 in wasm
//
// The testdiff CLua engine has no Case.Args seam (the C driver installs
// arg = {[0]=chunkname} inside ldostring), so args are baked into the
// source as a one-line prelude — which shifts error positions by one
// line relative to the other engines; programs that surface messages
// strip the position prefix themselves (see _cli-tests/README.md).
//
// PRINT/STDOUT events go to stdout (matching glua's channels); errors go
// to stderr with exit status 1.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/pschlump/golua55/testdiff"
)

func main() {
	os.Exit(mainAux())
}

func mainAux() int {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: clua-run script.lua [args...]")
		return 2
	}
	script := os.Args[1]
	args := os.Args[2:]

	src, err := os.ReadFile(script)
	if err != nil {
		fmt.Println(err.Error())
		return 1
	}
	wd, err := os.Getwd()
	if err != nil {
		fmt.Println(err.Error())
		return 1
	}

	// arg prelude: arg[0] = script path as typed, arg[1..n] = CLI args —
	// the same surface glua -W seeds via setArgGlobal.
	var b strings.Builder
	b.WriteString("arg={[0]=")
	b.WriteString(luaQuote(script))
	for _, a := range args {
		b.WriteByte(',')
		b.WriteString(luaQuote(a))
	}
	b.WriteString("}\n")

	e := testdiff.NewCLua("clua-run")
	log := e.Run(testdiff.Case{
		Name:   script,
		Dir:    wd,
		Source: []byte(b.String() + string(src)),
	})

	status := 0
	for _, line := range log {
		tab := strings.IndexByte(line, '\t')
		event, payload := line, ""
		if tab >= 0 {
			event, payload = line[:tab], line[tab+1:]
		}
		switch event {
		case "PRINT", "STDOUT":
			fmt.Println(payload)
		case "ERROR":
			if msg, err := strconv.Unquote(payload); err == nil {
				payload = msg
			}
			fmt.Fprintf(os.Stderr, "clua: %s\n", payload)
			status = 1
		case "ENGINE-ERROR", "SKIP-UNSUPPORTED":
			fmt.Fprintf(os.Stderr, "clua: %s\n", payload)
			status = 1
		}
	}
	return status
}

// luaQuote renders s as a Lua 5.1 double-quoted string literal (the
// testdiff luaQuote contract: \\ and \" escaped, control bytes as
// 3-digit decimal \ddd).
func luaQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			fmt.Fprintf(&b, "\\%03d", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}
