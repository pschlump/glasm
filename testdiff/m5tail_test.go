package testdiff

// M5c gate: the tailcall trampoline. Staged tailcalls (wasm-closure
// callees) re-dispatch at the same adapter level via the -2 sentinel —
// O(1) wasm stack, O(1) frames; C-function and __call'd callees take the
// rt_call fallback (parity with _vm.go:587-650).

import (
	"strings"
	"testing"
)

func TestM5Tailcalls(t *testing.T) {
	cases := map[string]string{
		"flat":        "local function f(n) if n == 0 then return 'done' end return f(n-1) end\nprint(f(1000000))\n", // the 10⁶ gate
		"mutual":      "local odd\nlocal function even(n) if n == 0 then return true end return odd(n-1) end\nfunction odd(n) if n == 0 then return false end return even(n-1) end\nprint(even(100001), odd(100001))\n",
		"ctailcall":   "local function f(x) return print(x) end\nf('tp')\n",
		"upvalue":     "local acc = 0\nlocal function f(n) acc = acc + 1 if n == 0 then return acc end return f(n-1) end\nprint(f(200000))\n",
		"vararg":      "local function g(...) return ... end\nlocal function f(...) return g(...) end\nprint(f(1, 2, 3))\n",
		"multiarg":    "local function add(a, b) return a + b end\nlocal function go(x) return add(x, x) end\nprint(go(21))\n",
		"errchain":    "local function f(n) if n == 0 then error('boom') end return f(n-1) end\nlocal ok, e = pcall(f, 10000)\nprint(ok, e ~= nil)\n",               // wording is row 9; assert the catch itself
		"callchain":   "local mt = {__call = function(self, n) if n == 0 then return 'cd' end return self(n-1) end}\nlocal f = setmetatable({}, mt)\nprint(f(5))\n", // deep __call chains are ledger row 18
		"constvararg": "local function g(...) return ... end\nlocal function f(...) return g(2, ...) end\nprint(f(1, 2, 3))\n",
		"tailret":     "local function f(n) if n > 0 then return f(n-1) end return 'end' end\nprint(f(10))\n",
	}
	for name, src := range cases {
		diffCase(t, name+".lua", src)
	}
}

// The staged trampoline must be O(1): a 10⁶-deep tail recursion stays
// inside one adapter level (no wasm-stack growth, no depth-guard trip).
func TestM5TailO1(t *testing.T) {
	src := "local function f(n) if n == 0 then return 'ok' end return f(n-1) end\nreturn f(1000000)\n"
	log := (&WasmEngine{name: "wasm"}).Run(Case{Name: "o1.lua", Dir: ".", Source: []byte(src)})
	for _, l := range log {
		if strings.HasPrefix(l, "ERROR") && strings.Contains(l, "stack overflow") {
			t.Errorf("10⁶-deep tailcall tripped a limit: %s", l)
		}
		if strings.HasPrefix(l, "ENGINE-") {
			t.Errorf("engine failure: %s", l)
		}
	}
}
