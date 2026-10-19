package main

import (
	"flag"
	"fmt"
	"github.com/ergochat/readline"
	"github.com/pschlump/glasm"
	"github.com/pschlump/glasm/parse"
	"github.com/pschlump/glasm/testdiff"
	"os"
	"runtime/pprof"
	"strings"
)

func main() {
	os.Exit(mainAux())
}

func mainAux() int {
	var opt_e, opt_l, opt_p, opt_w string
	var opt_i, opt_v, opt_dt, opt_dc, opt_W bool
	var opt_m int
	// Every option takes its single-letter and a full-word spelling (-v,
	// -version); both names bind the same variable, and Go's flag package
	// already accepts -name and --name alike.
	flag.StringVar(&opt_e, "e", "", "")
	flag.StringVar(&opt_e, "execute", "", "")
	flag.StringVar(&opt_l, "l", "", "")
	flag.StringVar(&opt_l, "require", "", "")
	flag.StringVar(&opt_p, "p", "", "")
	flag.StringVar(&opt_p, "cpuprofile", "", "")
	flag.StringVar(&opt_w, "w", "", "")
	flag.StringVar(&opt_w, "wasm", "", "")
	flag.IntVar(&opt_m, "mx", 0, "")
	flag.IntVar(&opt_m, "memlimit", 0, "")
	flag.BoolVar(&opt_i, "i", false, "")
	flag.BoolVar(&opt_i, "interactive", false, "")
	flag.BoolVar(&opt_v, "v", false, "")
	flag.BoolVar(&opt_v, "version", false, "")
	flag.BoolVar(&opt_dt, "dt", false, "")
	flag.BoolVar(&opt_dt, "dumpast", false, "")
	flag.BoolVar(&opt_dc, "dc", false, "")
	flag.BoolVar(&opt_dc, "dumpcode", false, "")
	flag.BoolVar(&opt_W, "W", false, "")
	flag.BoolVar(&opt_W, "runwasm", false, "")
	flag.Usage = func() {
		fmt.Println(`Usage: glua [options] [script [args]].
Available options are:
  -e stat,  -execute stat      execute string 'stat'
  -l name,  -require name      require library 'name'
  -mx MB,   -memlimit MB       memory limit(default: unlimited)
  -dt,      -dumpast           dump AST trees
  -dc,      -dumpcode          dump VM codes
  -i,       -interactive       enter interactive mode after executing 'script'
  -p file,  -cpuprofile file   write cpu profiles to the file
  -w file,  -wasm file         compile 'script' (or -e stat) to a wasm module, write to 'file' and exit
  -W,       -runwasm           run 'script' as a precompiled wasm module (cmd/luawasmc or -w output)
  -v,       -version           show version information (git tag/commit, build date)`)
	}
	flag.Parse()
	if len(opt_p) != 0 {
		f, err := os.Create(opt_p)
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	if len(opt_e) == 0 && !opt_i && !opt_v && flag.NArg() == 0 {
		opt_i = true
	}

	status := 0

	L := lua.NewState()
	defer L.Close()
	if opt_m > 0 {
		L.SetMx(opt_m)
	}

	// -v: version banner (release line + link-time git tag/commit/date);
	// -i prints the same banner before entering the REPL.
	if opt_v || opt_i {
		fmt.Println(versionInfo())
	}

	if len(opt_l) > 0 {
		if err := L.DoFile(opt_l); err != nil {
			fmt.Println(err.Error())
		}
	}

	// -w: compile to a wasm module and exit instead of running (the
	// luawasmc path) — source is the script, or the -e stat string.
	if len(opt_w) > 0 {
		if flag.NArg() == 0 && len(opt_e) == 0 {
			fmt.Println("glua: -w needs a script or -e stat")
			return 2
		}
		src, name := []byte(opt_e), "(command line)"
		if flag.NArg() > 0 {
			var err error
			if src, err = os.ReadFile(flag.Arg(0)); err != nil {
				fmt.Println(err.Error())
				return 1
			}
			// a file chunk names itself "@path" (auxlib's LoadFile
			// convention) so compile errors print "./x.lua:9:" like
			// every other engine leg, not "[string ...]:9:"
			name = "@" + flag.Arg(0)
		}
		bin, err := testdiff.CompileSource(src, name)
		if err != nil {
			fmt.Println(err.Error())
			return 1
		}
		if err := os.WriteFile(opt_w, bin, 0644); err != nil {
			fmt.Println(err.Error())
			return 1
		}
		fmt.Printf("%s: %d bytes wasm\n", opt_w, len(bin))
		return 0
	}

	if nargs := flag.NArg(); nargs > 0 {
		script := flag.Arg(0)
		// A precompiled module (cmd/luawasmc or -w output) runs on the
		// wasm engine: -W forces it, else detect a .wasm path carrying
		// the module magic — the way Lua loads binary chunks.
		if opt_W || strings.HasSuffix(script, ".wasm") {
			bin, err := os.ReadFile(script)
			if err != nil {
				fmt.Println(err.Error())
				return 1
			}
			if opt_W || (len(bin) >= 4 && string(bin[:4]) == "\x00asm") {
				return runWasm(bin, script, flag.Args()[1:])
			}
			// not a wasm module after all — run it as Lua source below
		}
		argtb := L.NewTable()
		// the standalone surface: arg[0] = script name, arg[1..n] = CLI
		// args — the same table the wasm engine seeds for a -W run.
		// (U5f note: this is the standalone interpreter's argv table,
		// which stock 5.5 keeps; it is NOT the 5.1 vararg 'arg' that
		// U5b removed from function bodies)
		L.RawSet(argtb, lua.LNumber(0), lua.LString(script))
		for i := 1; i < nargs; i++ {
			L.RawSet(argtb, lua.LNumber(i), lua.LString(flag.Arg(i)))
		}
		L.SetGlobal("arg", argtb)
		if opt_dt || opt_dc {
			file, err := os.Open(script)
			if err != nil {
				fmt.Println(err.Error())
				return 1
			}
			chunk, err2 := parse.Parse(file, script)
			if err2 != nil {
				fmt.Println(err2.Error())
				return 1
			}
			if opt_dt {
				fmt.Println(parse.Dump(chunk))
			}
			if opt_dc {
				proto, err3 := lua.Compile(chunk, script)
				if err3 != nil {
					fmt.Println(err3.Error())
					return 1
				}
				fmt.Println(proto.String())
			}
		}
		if err := L.DoFile(script); err != nil {
			fmt.Println(err.Error())
			status = 1
		}
	}

	if len(opt_e) > 0 {
		if err := L.DoString(opt_e); err != nil {
			fmt.Println(err.Error())
			status = 1
		}
	}

	if opt_i {
		doREPL(L)
	}
	return status
}

// runWasm executes a precompiled wasm module on the wasm engine (the
// luawasm-run path): args become the script's arg table (arg[1..n]),
// PRINT/STDOUT events go to stdout, errors to stderr. The engine is
// wasmtime (the oracle) or, when GLUA_WASM_ENGINE=wazero, the pure-Go
// production engine (ledger row 32) — the _cli-tests WAZERO=1 leg.
func runWasm(bin []byte, path string, args []string) int {
	dir, err := os.Getwd()
	if err != nil {
		fmt.Println(err.Error())
		return 1
	}
	e := testdiff.NewRunEngine("run", bin)
	log := e.Run(testdiff.Case{
		Name:   path, // arg[0] / chunkname: the path as typed, like the source path
		Dir:    dir,
		Source: bin,
		Args:   args,
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
		case "ERROR", "ENGINE-ERROR", "SKIP-UNSUPPORTED":
			fmt.Fprintf(os.Stderr, "glua: %s\n", payload)
			status = 1
		}
	}
	return status
}

// do read/eval/print/loop
func doREPL(L *lua.LState) {
	rl, err := readline.New("> ")
	if err != nil {
		panic(err)
	}
	defer rl.Close()
	for {
		if str, err := loadline(rl, L); err == nil {
			if err := L.DoString(str); err != nil {
				fmt.Println(err)
			}
		} else { // error on loadline
			fmt.Println(err)
			return
		}
	}
}

func incomplete(err error) bool {
	if lerr, ok := err.(*lua.ApiError); ok {
		if perr, ok := lerr.Cause.(*parse.Error); ok {
			return perr.Pos.Line == parse.EOF
		}
	}
	return false
}

func loadline(rl *readline.Instance, L *lua.LState) (string, error) {
	rl.SetPrompt("> ")
	if line, err := rl.Readline(); err == nil {
		if _, err := L.LoadString("return " + line); err == nil { // try add return <...> then compile
			return line, nil
		} else {
			return multiline(line, rl, L)
		}
	} else {
		return "", err
	}
}

func multiline(ml string, rl *readline.Instance, L *lua.LState) (string, error) {
	for {
		if _, err := L.LoadString(ml); err == nil { // try compile
			return ml, nil
		} else if !incomplete(err) { // syntax error , but not EOF
			return ml, nil
		} else {
			rl.SetPrompt(">> ")
			if line, err := rl.Readline(); err == nil {
				ml = ml + "\n" + line
			} else {
				return "", err
			}
		}
	}
}
