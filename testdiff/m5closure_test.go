package testdiff

// M5a A4 gate: closures/upvalues through the wasm backend, differentially
// against the interpreter oracle. The _wasm-tests corpus carries the same
// cases (clo*/cb*); this test keeps them in `go test ./...` without the
// CLI. Sort-comparator callbacks stay ledgered (row 10, M5d).

import "testing"

func diffCase(t *testing.T, name, src string) {
	t.Helper()
	interpLog := NewInterp("interp").Run(Case{Name: name, Dir: ".", Source: []byte(src)})
	wasmLog := (&WasmEngine{name: "wasm", SkipUnsupported: true}).Run(Case{Name: name, Dir: ".", Source: []byte(src)})
	if d := DiffLogs(interpLog, wasmLog); d != "" {
		t.Errorf("%s: %s", name, d)
	}
}

func TestM5Closures(t *testing.T) {
	cases := map[string]string{
		"basic":      "local f = function() return 7 end\nprint(f())\n",
		"params":     "local function add(a, b) return a + b end\nprint(add(3, 4))\n",
		"forloop":    "local fs = {}\nfor i = 1, 3 do fs[i] = function() return i end end\nprint(fs[1](), fs[2](), fs[3]())\n",
		"counter":    "local function counter()\n  local n = 0\n  return function() n = n + 1 return n end\nend\nlocal c = counter()\nc() c()\nprint(c())\n",
		"writethru":  "local function outer()\n  local x = 1\n  local function inner() x = x + 10 return x end\n  inner() inner()\n  return x\nend\nprint(outer())\n",
		"bodylocal":  "local fs = {}\nfor i = 1, 3 do\n  local j = i * 10\n  fs[i] = function() return j end\nend\nprint(fs[1](), fs[2](), fs[3]())\n",
		"breakclose": "local f\nfor i = 1, 3 do\n  f = function() return i end\n  if i == 1 then break end\nend\nprint(f())\n",
		"nested3":    "local function a()\n  local x = 1\n  return function()\n    local y = 2\n    return function() return x + y end\n  end\nend\nprint(a()()())\n",
		"paramcap":   "local function make(n)\n  return function() return n * 2 end\nend\nprint(make(5)(), make(21)())\n",
		"identity":   "local t = {}\nlocal function get() return t end\nget().k = 5\nprint(t.k)\n",
		"recurse":    "local function fib(n)\n  if n < 2 then return n end\n  return fib(n - 1) + fib(n - 2)\nend\nprint(fib(10))\n",
		"multret":    "local function g() return 1, 2, 3 end\nlocal a, b, c = g()\nprint(a, b, c)\n",
		"twoloops":   "local fns = {}\nfor i = 1, 2 do\n  for j = 1, 2 do\n    fns[#fns + 1] = function() return i * 10 + j end\n  end\nend\nlocal out = {}\nfor k, f in ipairs(fns) do out[k] = tostring(f()) end\nprint(table.concat(out, ' '))\n",
		"argfun":     "local function apply(f, v) return f(v) end\nprint(apply(function(x) return x * 3 end, 5))\n",
		"upcall":     "local function id(x) return x end\nlocal function apply() return id(5) end\nprint(apply())\n",
		// C→wasm callbacks through the adapter (ledger row 10 exempt: sort)
		"gsub":  "print(('hello world'):gsub('o', function(m) return m:upper() end))\n",
		"pcall": "print(pcall(function() return 1, 2 end))\n",
		"index": "local mt = {__index = function(t, k) return k .. '!' end}\nlocal t = setmetatable({}, mt)\nprint(t.foo, t.bar)\n",
	}
	for name, src := range cases {
		diffCase(t, name+".lua", src)
	}
}
