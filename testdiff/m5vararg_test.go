package testdiff

// M5b gate: varargs + the compat `arg` table through the wasm backend,
// differentially against the interpreter oracle (same cases as the
// _wasm-tests var* batch, kept in `go test ./...`).

import "testing"

func TestM5Varargs(t *testing.T) {
	cases := map[string]string{
		"passthrough":  "local function f(...) return ... end\nprint(f(1, 2, 3))\n",
		"nilpad":       "local function f(...) local a, b = ... return a, b end\nprint(f(7))\n",
		"countnils":    "local function f(...) return select('#', ...) end\nprint(f(nil, nil))\n",
		"negselect":    "local function f(...) return select(-1, ...) end\nprint(f('a', 'b', 'c'))\n",
		"constructor":  "local function f(...) local t = {...} return #t, t[1], t[3] end\nprint(f('x', 'y', 'z'))\n",
		"soleargs":     "local function g(a, b) return a + b end\nlocal function f(...) return g(...) end\nprint(f(3, 4))\n",
		"method":       "local t = {}\nfunction t.f(...) return ... end\nprint(t.f(1, 2))\n",
		"iterator":     "local function f(...) local n = 0\nfor _, v in ipairs({...}) do n = n + v end\nreturn n end\nprint(f(1, 2, 3))\n",
		"argcontents":  "local function f(...) return arg[1], arg.n end\nprint(f(7, 8))\n",
		"paramsplit":   "local function f(a, ...) return a, select('#', ...) end\nprint(f(1, 2, 3))\n",
		"single":       "local function f(...) return (...) end\nprint(f(9))\n",
		"zero":         "local function f(...) return ... end\nprint(f())\n",
		"truncate":     "local function f(...) local a, b = ... return b end\nprint(f(1, 2, 3))\n",
		"constvarargs": "local function h(...) return ... end\nlocal function g(...) return h(...) end\nlocal function f(...) return g(2, ...) end\nprint(f(1, 2, 3))\n", // the M5b scratch-collision repro
		// was: unpack({...}) — base unpack removed at U1 (row 54; the wasm
		// engine's 5.1 C stdlib still has it, so the bare name had to go)
		"unpack":       "local function f(...) return select('#', ...) end\nprint(f('p', 'q'))\n",
		"mixedthrough": "local function f(a, b, ...) return a, b, ... end\nprint(f(1, 2, 3, 4))\n",
		"argnzero":     "local function f(...) return arg.n end\nprint(f())\n",
		"select2":      "local function f(...) return select(2, ...) end\nprint(f(1, 2, 3))\n",
		"concat":       "local function f(...) return table.concat({...}, '-') end\nprint(f('a', 'b', 'c'))\n",
		"fmt":          "local function f(fmt, ...) return string.format(fmt, ...) end\nprint(f('%d-%s', 7, 'q'))\n",
		"argnil":       "local function f(...) local x = ... return arg == nil end\nprint(f(1, 2))\n", // compile.go:1188 clears NeedsArg on `...` use
	}
	for name, src := range cases {
		diffCase(t, name+".lua", src)
	}
}
