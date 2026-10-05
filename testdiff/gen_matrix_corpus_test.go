package testdiff

// The M4 gate corpus generator: writes ~200 small scripts covering the
// v1-supported opcode surface (no closures/varargs — those SKIP and belong
// to M5). Run once via `go test ./testdiff -run TestGenMatrixCorpus`; the
// files land in _wasm-tests/ and are diffed by
//
//	go run ./cmd/testdiff -corpus _wasm-tests -engines interp,wasm

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestGenMatrixCorpus(t *testing.T) {
	dir := "../_wasm-tests"
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	var cases []string
	add := func(name, src string) {
		cases = append(cases, src)
		if err := os.WriteFile(filepath.Join(dir, name+".lua"), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// ---- literals & prints ----
	for i, v := range []string{"42", "3.25", "-7", "0.5", "1e3", "'s'", "\"d\"", "true", "false", "nil"} {
		add(fmt.Sprintf("lit%02d", i), fmt.Sprintf("print(%s)\n", v))
	}
	// ---- arithmetic over a value grid ----
	vals := []string{"1", "2.5", "-3", "100", "0", "7"}
	ops := []string{"+", "-", "*", "/", "%", "^"}
	n := 0
	for _, a := range vals {
		for _, b := range vals {
			for _, op := range ops {
				add(fmt.Sprintf("ari%03d", n), fmt.Sprintf("print(%s %s %s)\n", a, op, b))
				n++
			}
		}
	}
	// string coercion arithmetic
	for i, s := range []string{"'10' + 1", "'2' * '3'", "'7' .. 7", "1 .. 2", "'x' .. 0"} {
		add(fmt.Sprintf("coerce%02d", i), fmt.Sprintf("print(%s)\n", s))
	}

	// ---- U2f (D11): the integer/bitwise/float matrix ----
	// integer arith over a 5.5-shaped grid (wrap-around int64 semantics;
	// values printed directly stay numeric at the print boundary, so the
	// harness's %.14g mask keeps the engines byte-exact — integral
	// FLOATS must not be stringified in-case (gnf discipline, D7/U5))
	ivals := []string{"7", "-13", "0", "1", "-1", "0x10", "9223372036854775807", "-9223372036854775807", "9007199254740993", "1000000000000"}
	iops := []string{"+", "-", "*", "//", "%"}
	n = 0
	for _, a := range ivals {
		for _, b := range ivals {
			for _, op := range iops {
				if (op == "//" || op == "%") && b == "0" {
					continue // div-by-zero family lives in the error corpus
				}
				add(fmt.Sprintf("iari%03d", n), fmt.Sprintf("print(%s %s %s)\n", a, op, b))
				n++
			}
		}
	}
	// bitwise + shifts over the integer grid
	bops := []string{"&", "|", "~"}
	for _, a := range ivals {
		for _, b := range ivals {
			for _, op := range bops {
				add(fmt.Sprintf("bit%03d", n), fmt.Sprintf("print(%s %s %s)\n", a, op, b))
				n++
			}
		}
	}
	// shift corners: negative counts shift the other way, |y|>=64 => 0
	for _, a := range []string{"1", "-1", "9223372036854775807", "-9223372036854775807", "0x55", "3"} {
		for _, y := range []string{"0", "1", "63", "64", "65", "-1", "-63", "-64", "-65"} {
			add(fmt.Sprintf("shf%03d", n), fmt.Sprintf("print(%s << %s, %s >> %s)\n", a, y, a, y))
			n++
		}
	}
	// mixed int/float promotion: any float infects; / and ^ are always float
	fvals := []string{"2.5", "-0.5", "3.0", "1e15", "0.1", "4503599627370497.0"}
	for _, a := range []string{"7", "9007199254740993", "-13"} {
		for _, b := range fvals {
			add(fmt.Sprintf("mix%03d", n), fmt.Sprintf("print(%s + %s, %s - %s, %s * %s, %s // %s, %s %% %s)\n", a, b, a, b, a, b, a, b, a, b))
			n++
			add(fmt.Sprintf("mix%03d", n), fmt.Sprintf("print(%s / %s, %s ^ %s)\n", a, b, b, a))
			n++
		}
	}
	// integer division/modulo sign semantics (floor convention)
	for _, pair := range [][2]string{{"7", "2"}, {"-7", "2"}, {"7", "-2"}, {"-7", "-2"}, {"9223372036854775807", "3"}, {"-9223372036854775807", "3"}} {
		add(fmt.Sprintf("idv%03d", n), fmt.Sprintf("print(%s // %s, %s %% %s, %s - (%s // %s) * %s)\n", pair[0], pair[1], pair[0], pair[1], pair[0], pair[0], pair[1], pair[1]))
		n++
	}
	// wrap-around + minint edges
	add("wrp00", "print(9223372036854775807 + 1, -9223372036854775807 - 2, 9223372036854775807 * 2)\n")
	add("wrp01", "print(-9223372036854775807 - 1 == -9223372036854775808, -(-9223372036854775807 - 1))\n")
	add("wrp02", "print(9007199254740993 + 1, 9007199254740993 - 1)\n")
	add("wrp03", "print(0x7fffffffffffffff & -1, ~0, ~(-1))\n")
	// mixed int/float comparisons incl. the 2^53 exact edges
	n = 0
	for _, pair := range [][2]string{{"9007199254740993", "9007199254740992.0"}, {"9007199254740993", "9007199254740994.0"}, {"-9007199254740993", "-9007199254740992.0"}, {"7", "7.0"}, {"-1", "-1.5"}, {"9223372036854775807", "1e308"}, {"-9223372036854775807", "-1e308"}, {"0", "0.5"}} {
		a, b := pair[0], pair[1]
		add(fmt.Sprintf("icmp%03d", n), fmt.Sprintf("print(%s < %s, %s <= %s, %s > %s, %s >= %s, %s == %s)\n", a, b, a, b, a, b, a, b, a, b))
		n++
	}
	// string coercion subtype results (integer strings stay integer)
	for i, x := range []string{"'7' // 2", "'0x10' + 0", "'2.5' + 0", "'10' / 4", "'-13' % 5", "'7' << 2 == 28"} {
		add(fmt.Sprintf("scv%02d", i), fmt.Sprintf("print(%s)\n", x))
	}
	// integer for: descending, big steps, minint/maxint neighborhoods
	add("forI00", "for i = 10, 1, -3 do print(i) end\n")
	add("forI01", "for i = 1, 10, 4 do print(i) end\n")
	add("forI02", "for i = 9223372036854775804, 9223372036854775807 do print(i) end\n")
	add("forI03", "for i = -9223372036854775807, -9223372036854775804, 2 do print(i) end\n")
	add("forI04", "local n = 0\nfor i = 1, 1000000000 do n = n + 1 if n == 5 then break end end\nprint(n, 'early')\n")
	add("forI05", "for i = 5, 5 do print(i) end\nfor i = 5, 4 do print('never') end\nprint('done')\n")
	add("forI06", "for i = 0, -10, -5 do print(i) end\n")
	add("forI07", "local s = 0\nfor i = 1, 100 do s = s + i end\nprint(s)\n")
	// float for: fractional steps, mixed int/float flavors
	add("forF00", "for i = 0.0, 1.0, 0.25 do print(i) end\n")
	add("forF01", "for i = 1, 2.5, 0.5 do print(i) end\n")
	add("forF02", "for i = 2.5, 1, -0.5 do print(i) end\n")
	add("forF03", "for i = 0.1, 0.3, 0.1 do print(i) end\n")
	// for-loop integer-exactness: the float that could be an int isn't one
	add("forX00", "for i = 1, 3, 1.0 do print(i, i == 2) end\n")
	// unary
	for i, s := range []string{"-5", "-(-5)", "- 2.5"} {
		add(fmt.Sprintf("unm%02d", i), fmt.Sprintf("print(%s)\n", s))
	}
	// ---- comparisons ----
	cvals := []string{"1", "2", "'a'", "'b'", "true", "false", "nil"}
	n = 0
	for _, a := range []string{"1", "2", "'a'", "'b'", "1.5"} {
		for _, b := range []string{"1", "2", "'a'", "2.5"} {
			for _, op := range []string{"==", "~=", "<", "<=", ">", ">="} {
				add(fmt.Sprintf("cmp%03d", n), fmt.Sprintf("print(%s %s %s)\n", a, op, b))
				n++
			}
		}
	}
	_ = cvals
	// ---- logic (and/or/not) ----
	lvals := []string{"nil", "false", "true", "0", "1", "''", "'x'"}
	n = 0
	for _, a := range lvals {
		for _, b := range lvals {
			add(fmt.Sprintf("logic%03d", n), fmt.Sprintf("print(%s and %s, %s or %s, not %s)\n", a, b, a, b, a))
			n++
		}
	}
	// ---- length / concat ----
	for i, s := range []string{"#'hello'", "#{1,2,3}", "'a'..'b'..'c'", "#{1,2,3}[2]"} {
		add(fmt.Sprintf("len%02d", i), fmt.Sprintf("print(%s)\n", s))
	}
	// ---- locals & assignment ----
	add("loc00", "local a, b, c = 1, 2, 3\nprint(a, b, c)\n")
	add("loc01", "local a = 1\nlocal b = a\na = 2\nprint(a, b)\n")
	add("loc02", "local a, b = 1\nprint(a, b)\n")
	add("loc03", "local a = 1\na, a = 2, 3\nprint(a)\n")
	add("loc04", "local x\nprint(x)\nx = 9\nprint(x)\n")
	// ---- if / elseif / else ----
	add("if00", "if 1 < 2 then print('y') else print('n') end\n")
	add("if01", "if 2 < 1 then print('y') else print('n') end\n")
	add("if02", "if 1 < 0 then print('a') elseif 1 < 2 then print('b') else print('c') end\n")
	add("if03", "if 1 < 0 then print('a') elseif 2 < 1 then print('b') else print('c') end\n")
	add("if04", "local x = 5\nif x > 0 then print('p') end\nprint('after')\n")
	add("if05", "if nil then print('t') elseif false then print('f') elseif 0 then print('z') end\n")
	// ---- while / repeat / break ----
	add("whl00", "local i = 0\nwhile i < 3 do i = i + 1 end\nprint(i)\n")
	add("whl01", "local i = 0\nwhile true do i = i + 1 if i > 2 then break end end\nprint(i)\n")
	add("whl02", "local i = 3\nwhile i > 0 do i = i - 1 end\nprint(i)\n")
	add("rep00", "local i = 0\nrepeat i = i + 1 until i >= 3\nprint(i)\n")
	add("rep01", "local i = 0\nrepeat local j = i i = i + 1 until j >= 2\nprint(i)\n")
	// ---- numeric for ----
	add("for00", "for i = 1, 3 do print(i) end\n")
	add("for01", "for i = 1, 6, 2 do print(i) end\n")
	add("for02", "for i = 3, 1, -1 do print(i) end\n")
	add("for03", "local s = 0\nfor i = 1, 10 do s = s + i end\nprint(s)\n")
	add("for04", "for i = 1, 0 do print('never') end\nprint('done')\n")
	add("for05", "for i = 1.5, 3.5, 0.5 do print(i) end\n")
	// ---- generic for ----
	add("gfor00", "for i, v in ipairs({10, 20, 30}) do print(i, v) end\n")
	add("gfor01", "for k, v in pairs({a=1, b=2}) do print(k, v) end\n")
	add("gfor02", "local n = 0\nfor k, v in pairs({5, 6, 7}) do n = n + v end\nprint(n)\n")
	// ---- tables ----
	add("tbl00", "local t = {}\nt.x = 1\nprint(t.x)\n")
	add("tbl01", "local t = {1, 2, 3}\nprint(#t, t[1], t[3])\n")
	add("tbl02", "local t = {a=1, b=2}\nprint(t.a, t.b)\n")
	add("tbl03", "local t = {1, 2, 3, 4, 5}\nt[2] = 20\nprint(t[2], #t)\n")
	add("tbl04", "local t = {}\nfor i = 1, 60 do t[i] = i * 2 end\nprint(#t, t[50], t[60])\n")
	add("tbl05", "local t = {1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20,21,22,23,24,25,26,27,28,29,30,31,32,33,34,35,36,37,38,39,40,41,42,43,44,45,46,47,48,49,50,51,52,53,54,55,56,57,58,59,60}\nprint(#t, t[1], t[60])\n")
	add("tbl06", "local t = {'a','b','c'}\ntable.insert(t, 'd')\nprint(#t, t[4])\n")
	// tbl07 (table.sort) restored (A4): the M5a precall adapter is the
	// callback machinery — sort's comparator now dispatches through it.
	add("tbl07", "local t = {3, 1, 2}\ntable.sort(t)\nprint(t[1], t[2], t[3])\n")
	add("tbl08", "local t = {x = {y = {z = 7}}}\nprint(t.x.y.z)\n")
	add("tbl09", "local t = {}\nt[1] = 'a' t[2] = 'b'\nprint(table.concat(t, '-'))\n")
	// ---- strings ----
	add("str00", "print(string.sub('hello', 2, 3))\n")
	add("str01", "print(string.upper('abc') .. string.rep('x', 3))\n")
	add("str02", "print(string.format('%d-%s', 7, 'q'))\n")
	add("str03", "print(#('a'..'b'), ('ab'):sub(2,2))\n")
	add("str04", "print(string.byte('A'), string.char(66))\n")
	// ---- library math (deterministic subset) ----
	add("math00", "print(math.floor(3.7), math.ceil(3.2), math.abs(-4))\n")
	add("math01", "print(math.max(1, 5, 3), math.min(1, 5, 3))\n")
	add("math02", "print(math.sqrt(16), math.fmod(7, 3))\n")
	// ---- calls & multret ----
	add("cal00", "print(select('#', 1, 2, 3))\n")
	add("cal01", "print(select(2, 'a', 'b', 'c'))\n")
	// U1 (row 54): base unpack removed; the vararg-spread shape it
	// exercised now uses a multret call through a helper
	add("cal02", "local function u(t) return t[1], t[2], t[3] end\nprint(u({7, 8, 9}))\n")
	add("cal03", "print(tostring(42), tonumber('13'), type(nil), type(1), type('s'), type({}))\n")
	add("cal04", "print(pcall(function() end))\n") // function → SKIP (closure) — keep for the skip path
	add("cal05", "print(tonumber('x') == nil)\n")
	// ---- error paths (byte-exact messages) ----
	add("err00", "local t = nil\nprint(t.x)\n")
	add("err01", "print(1 + {})\n")
	add("err02", "print(nil .. 'x')\n")
	add("err03", "for i = 1, 'x' do end\n")
	// ---- nested control flow ----
	add("nest00", "for i = 1, 3 do for j = 1, 2 do print(i, j) end end\n")
	add("nest01", "local s = 0\nfor i = 1, 4 do if i % 2 == 0 then s = s + i end end\nprint(s)\n")
	add("nest02", "local i = 0\nwhile i < 5 do i = i + 1 if i == 3 then break end end\nprint(i)\n")
	add("nest03", "for i = 1, 3 do if i == 2 then break end print(i) end\n")

	// ---- closures (M5a A4) ----
	add("clo00", "local f = function() return 7 end\nprint(f())\n")
	add("clo01", "local function add(a, b) return a + b end\nprint(add(3, 4))\n")
	add("clo02", "local fs = {}\nfor i = 1, 3 do fs[i] = function() return i end end\nprint(fs[1](), fs[2](), fs[3]())\n")                                                                                                                       // distinct per-iteration closures
	add("clo03", "local function counter()\n  local n = 0\n  return function() n = n + 1 return n end\nend\nlocal c = counter()\nc() c()\nprint(c())\n")                                                                                         // shared upvalue write-through
	add("clo04", "local function outer()\n  local x = 1\n  local function inner() x = x + 10 return x end\n  inner() inner()\n  return x\nend\nprint(outer())\n")                                                                                // write-through visible to creator
	add("clo05", "local fs = {}\nfor i = 1, 3 do\n  local j = i * 10\n  fs[i] = function() return j end\nend\nprint(fs[1](), fs[2](), fs[3]())\n")                                                                                               // closure over body local
	add("clo06", "local f\nfor i = 1, 3 do\n  f = function() return i end\n  if i == 1 then break end\nend\nprint(f())\n")                                                                                                                       // break closes the upvalue
	add("clo07", "local function a()\n  local x = 1\n  return function()\n    local y = 2\n    return function() return x + y end\n  end\nend\nprint(a()()())\n")                                                                                // nested 3 deep, captures at both levels
	add("clo08", "local function make(n)\n  return function() return n * 2 end\nend\nprint(make(5)(), make(21)())\n")                                                                                                                            // capture a param
	add("clo09", "local t = {}\nlocal function get() return t end\nget().k = 5\nprint(t.k)\n")                                                                                                                                                   // closure-returned table stays identity-equal
	add("clo10", "local function fib(n)\n  if n < 2 then return n end\n  return fib(n - 1) + fib(n - 2)\nend\nprint(fib(10))\n")                                                                                                                 // self-recursive local (upvalue capture of own name)
	add("clo11", "local function g() return 1, 2, 3 end\nlocal a, b, c = g()\nprint(a, b, c)\n")                                                                                                                                                 // multret from a closure
	add("clo12", "local fns = {}\nfor i = 1, 2 do\n  for j = 1, 2 do\n    fns[#fns + 1] = function() return i * 10 + j end\n  end\nend\nlocal out = {}\nfor k, f in ipairs(fns) do out[k] = tostring(f()) end\nprint(table.concat(out, ' '))\n") // two captured loop vars
	add("clo13", "local function apply(f, v) return f(v) end\nprint(apply(function(x) return x * 3 end, 5))\n")                                                                                                                                  // closure as argument
	// ---- C→wasm callbacks through the adapter (M5a A4 matrix) ----
	add("cb00", "local t = {3, 1, 2}\ntable.sort(t, function(a, b) return a > b end)\nprint(t[1], t[2], t[3])\n")                   // sort comparator (was ledger row 10)
	add("cb01", "print(('hello world'):gsub('o', function(m) return m:upper() end))\n")                                             // gsub function replacement
	add("cb02", "print(pcall(function() return 1, 2 end))\n")                                                                       // pcall of a wasm closure
	add("cb03", "local mt = {__index = function(t, k) return k .. '!' end}\nlocal t = setmetatable({}, mt)\nprint(t.foo, t.bar)\n") // __index function metamethod
	// ---- varargs (M5b) ----
	add("var00", "local function f(...) return ... end\nprint(f(1, 2, 3))\n")
	add("var01", "local function f(...) local a, b = ... return a, b end\nprint(f(7))\n")   // nil-padding
	add("var02", "local function f(...) return select('#', ...) end\nprint(f(nil, nil))\n") // count with nils
	add("var03", "local function f(...) return select(-1, ...) end\nprint(f('a', 'b', 'c'))\n")
	add("var04", "local function f(...) local t = {...} return #t, t[1], t[3] end\nprint(f('x', 'y', 'z'))\n")         // constructor
	add("var05", "local function g(a, b) return a + b end\nlocal function f(...) return g(...) end\nprint(f(3, 4))\n") // ... as sole call args
	add("var06", "local t = {}\nfunction t.f(...) return ... end\nprint(t.f(1, 2))\n")
	add("var07", "local function f(...) local n = 0\nfor _, v in ipairs({...}) do n = n + v end\nreturn n end\nprint(f(1, 2, 3))\n")                               // vararg iterator
	add("var08", "local function f(...) local t = table.pack(...) return t[1], t.n end\nprint(f(7, 8))\n")                                                        // vararg contents via pack (U5b: compat `arg` is dead — row 60 closes)
	add("var09", "local function f(a, ...) return a, select('#', ...) end\nprint(f(1, 2, 3))\n")                                                                   // params + varargs split
	add("var10", "local function f(...) return (...) end\nprint(f(9))\n")                                                                                          // single value
	add("var11", "local function f(...) return ... end\nprint(f())\n")                                                                                             // zero varargs
	add("var12", "local function f(...) local a, b = ... return b end\nprint(f(1, 2, 3))\n")                                                                       // truncation
	add("var13", "local function h(...) return ... end\nlocal function g(...) return h(...) end\nlocal function f(...) return g(2, ...) end\nprint(f(1, 2, 3))\n") // const+varargs, two levels (the M5b scratch-collision repro)
	add("var14", "local function f(...) return select('#', ...) end\nprint(f('p', 'q'))\n")
	add("var15", "local function f(a, b, ...) return a, b, ... end\nprint(f(1, 2, 3, 4))\n") // mixed params + varargs through
	add("var16", "local function f(...) local t = table.pack(...) return t.n end\nprint(f())\n") // pack count with zero varargs (U5b)
	add("var17", "local function f(...) return select(2, ...) end\nprint(f(1, 2, 3))\n")
	add("var18", "local function f(...) return table.concat({...}, '-') end\nprint(f('a', 'b', 'c'))\n")
	add("var19", "local function f(fmt, ...) return string.format(fmt, ...) end\nprint(f('%d-%s', 7, 'q'))\n") // leading fixed arg then varargs to a C function
	add("var20", "local function f(...) local x = ... return x == nil end\nprint(f(1, 2))\n")   // first extra's nil-ness (U5b: no compat arg local anymore)
	// ---- tailcalls (M5c: the trampoline; staged for wasm-closure callees,
	// rt_call fallback otherwise) ----
	add("tco00", "local function f(n) if n == 0 then return 'done' end return f(n-1) end\nprint(f(100000))\n")                                                                                                     // flat 10⁵
	add("tco01", "local odd\nlocal function even(n) if n == 0 then return true end return odd(n-1) end\nfunction odd(n) if n == 0 then return false end return even(n-1) end\nprint(even(100001), odd(100001))\n") // mutual (pre-declared locals)
	add("tco02", "local function f(x) return print(x) end\nf('tp')\n")                                                                                                                                             // C-function tailcall → rt_call fallback
	add("tco03", "local acc = 0\nlocal function f(n) acc = acc + 1 if n == 0 then return acc end return f(n-1) end\nprint(f(50000))\n")                                                                            // upvalue writes across the chain
	add("tco04", "local function g(...) return ... end\nlocal function f(...) return g(...) end\nprint(f(1, 2, 3))\n")                                                                                             // vararg tailcall
	add("tco05", "local function add(a, b) return a + b end\nlocal function go(x) return add(x, x) end\nprint(go(21))\n")                                                                                          // multi-arg staged tailcall
	add("tco06", "local function f(n) if n == 0 then error('deep') end return f(n-1) end\nprint(pcall(f, 10000))\n")                                                                                               // error through a 10⁴ staged chain (message heads compared; wording is row 9 → skip-annotated if it diverges)
	add("tco07", "local mt = {__call = function(self, n) if n == 0 then return 'cd' end return self(n-1) end}\nlocal f = setmetatable({}, mt)\nprint(f(5))\n")                                                     // __call chain, shallow
	add("tco08", "local function g(...) return ... end\nlocal function f(...) return g(2, ...) end\nprint(f(1, 2, 3))\n")                                                                                          // const+varargs tailcall
	add("tco09", "local mt = {__call = function(self, n) if n == 0 then return 'cd' end return self(n-1) end}\nlocal f = setmetatable({}, mt)\nprint(f(120))\n")                                                   // deep __call+tailcall chain (row 18 pin — fixed with row 31's call_body rebasing; 120 < RTW_MAX_DEPTH=150, whose depth divergence is row 20)

	// ---- U3 stdlib grid (row 70): the 5.3/5.4/5.5 library surface on the
	// wasm legs — engine-portable basics only (probe-verified interp↔wasm
	// before landing). Discipline: error texts are asserted with substring
	// finds (the _u3 idiom), never by printing hint-bearing pcall messages
	// (the row-71 seam raises hint-less); no %p of live objects (Go
	// pseudo-addresses vs C pointers); no pairs-order, io, or os surfaces.
	// slm: math — the stock 5.5.1 surface + the xoshiro pin (row 64)
	n = 0
	for _, s := range []string{
		"print(math.type(1), math.type(1.0), math.type('1'), math.type('x'))\n",
		"print(math.tointeger(3.0), math.tointeger('7'), math.tointeger(3.5), math.tointeger('x'))\n",
		"print(math.maxinteger, math.mininteger, math.ult(1, -1), math.ult(-1, 1))\n",
		"print(math.floor(-3.5), math.ceil(-3.5), math.floor(2.5), math.ceil(2.5))\n",
		"print(math.floor(2^53), math.floor(-2^53), math.abs(math.mininteger))\n",
		"print(math.fmod(7, 3), math.fmod(-7, 3), math.fmod(7, -3))\n",
		"print(math.fmod(7.5, 2), math.fmod(-7.5, 2), math.fmod(1e300, 1e-300))\n",
		"print(math.modf(3.7))\nprint(math.modf(-3.7))\nprint(math.modf(5))\n",
		"print(math.max(1, 2.5, 2), math.min(1, 0.5, 1))\n",
		"print(math.atan(1), math.atan(1, 1), math.atan(-1, -1))\n",
		"print(math.log(8, 2), math.log(100, 10), math.log(math.exp(1)))\n",
		"print(math.frexp(3.5))\nprint(math.ldexp(0.875, 2))\n",
	} {
		add(fmt.Sprintf("slm%02d", n), s)
		n++
	}
	add("slm20", "math.randomseed(42)\nprint(math.random(100), math.random(100), math.random())\n")
	add("slm21", "math.randomseed(42)\nlocal a = math.random(100)\nmath.randomseed(42)\nprint(a == math.random(100))\n")
	add("slm22", "math.randomseed(1007)\nprint(math.random(0))\n") // the math.lua anchor draw (row 64)
	add("slm23", "local ok, e = pcall(math.random, 5, 2)\nprint(not ok and e:find(\"interval is empty\") ~= nil)\n")
	// slt: table — move/create + the 5.2 pack/unpack (row 65)
	n = 0
	for _, s := range []string{
		"local t = {1, 2, 3, 4, 5}\ntable.move(t, 1, 3, 3)\nprint(t[3], t[4], t[5])\n",
		"local t = {1, 2, 3, 4, 5}\ntable.move(t, 2, 4, 1)\nprint(t[1], t[2], t[3], #t)\n",
		"local d = table.move({1, 2, 3}, 1, 3, 1, {})\nprint(#d, d[1], d == table.move(d, 1, 3, 1, d))\n",
		"local c = table.create(4, 0)\nprint(#c, c[1], c[4])\n",
		"print(table.unpack({1, 2, 3}, 2, 3))\nprint(table.unpack({1, 2, 3}, 2))\n",
		"print(table.pack(1, nil, 3).n, select('#', table.unpack(table.pack(1, nil, 3), 1, 3)))\n",
		"print(table.concat({1, 2, 3}, '-', 2, 3), table.concat({}, '-'))\n",
		"local t = setmetatable({}, {__index = function(_, k) return k * 10 end, __len = function() return 2 end})\nprint(t[1], #t)\n",
	} {
		add(fmt.Sprintf("slt%02d", n), s)
		n++
	}
	// slu: utf8 (row 65)
	n = 0
	for _, s := range []string{
		"local utf8 = require'utf8'\nprint(utf8.len(\"h\\xc3\\xa9llo\"), utf8.len(\"\\xe1\\x80\\x80\"))\n",
		"local utf8 = require'utf8'\nprint(utf8.char(0x41, 0xE9, 0x10FFFF))\n",
		"local utf8 = require'utf8'\nprint(utf8.codepoint(\"\\xc3\\xa9\", 1, -1))\n",
		"local utf8 = require'utf8'\nprint(utf8.offset(\"h\\xc3\\xa9llo\", 3), utf8.offset(\"h\\xc3\\xa9llo\", -1), select('#', utf8.offset(\"h\\xc3\\xa9llo\", 2)))\n",
		"local utf8 = require'utf8'\nfor p, c in utf8.codes(\"a\\xc3\\xa9\") do print(p, c) end\n",
		"local utf8 = require'utf8'\nprint(utf8.charpattern == \"[\\0-\\127\\194-\\253][\\128-\\191]*\")\n",
		"local utf8 = require'utf8'\nlocal ok, e = pcall(utf8.codepoint, \"\\xFF\")\nprint(ok, e:find(\"invalid UTF%-8 code\") ~= nil)\n",
		"local utf8 = require'utf8'\nlocal ok, e = pcall(utf8.len, \"a\\xFFb\")\nprint(ok, e)\n", // fail → offending position
	} {
		add(fmt.Sprintf("slu%02d", n), s)
		n++
	}
	// slp: string.pack family on the wasm32 size model (row 68)
	n = 0
	for _, s := range []string{
		"print(string.packsize('h'), string.packsize('i'), string.packsize('l'), string.packsize('T'))\n",
		"print(string.packsize('j'), string.packsize('f'), string.packsize('d'), string.packsize('n'))\n",
		"print(string.unpack('i4', string.pack('i4', -2)))\n",
		"print(string.unpack('<i2', '\\1\\0'), string.unpack('>i4', string.pack('>i4', 0x01020304)))\n",
		"print(string.unpack('n', string.pack('n', 0.5)), string.unpack('d', string.pack('d', -0.25)))\n",
		"print(string.unpack('z', 'abc\\0de'), string.unpack('s1', '\\3abc'))\n",
		"print((string.pack('i4', 0x01020304):gsub('.', function(c) return string.format('%02x', c:byte()) end)))\n",
		"print(string.unpack('i2i2', '\\1\\0\\2\\0'))\nprint(select('#', string.unpack('i2i2', '\\1\\0\\2\\0')))\n",
		"print(string.packsize('!xXi16'), string.packsize('=i4i4'))\n",
		"local s = string.pack('i4', 7)\nprint(string.unpack('i4', s, #s + 1 - 4))\n", // negative init position
		"local ok, e = pcall(string.pack, 'i17', 1)\nprint(not ok and e:find(\"out of limits %[1,16%]\") ~= nil)\n",
		"local ok, e = pcall(string.packsize, 'z')\nprint(not ok and e:find('variable%-length format') ~= nil)\n",
	} {
		add(fmt.Sprintf("slp%02d", n), s)
		n++
	}
	// slf: string.format — the stock port (row 67)
	n = 0
	for _, s := range []string{
		"print(string.format('%d %i %u', 42, -42, 42))\n",
		"print(string.format('%x %X %o %#x %#o', 255, 255, 8, 255, 8))\n",
		"print(string.format('%e %E', 1234.5, 1234.5))\n",
		"print(string.format('%f %.2f %5.1f', 1.5, 1.5, 1.5))\n",
		"print(string.format('%g %G %.3g', 0.0001, 0.0001, 12345))\n",
		"print(string.format('%a %A %.3a', 0.5, 0.5, 0.5))\n",
		"print(string.format('%g %g %g', math.huge, -math.huge, 0/0))\n",
		"print(string.format('%q', 'a\\nb'), string.format('%q', 0/0) == '\"(0/0)\"')\n",
		"print(string.format('%5.2s|%-5d|%+d| %d', 'abcdef', 42, 42, 42))\n",
		"print(string.format('%c%c', 65, 66))\n",
		"print(string.format('%%'))\nprint(string.format('%s %s', 1.0, 3))\n",
		"local ok, e = pcall(string.format, '%10q', 'x')\nprint(not ok and e:find('cannot have modifiers') ~= nil)\n",
		"local ok, e = pcall(string.format, '%y', 1)\nprint(not ok and e:find(\"invalid conversion '%%y' to 'format'\") ~= nil)\n",
	} {
		add(fmt.Sprintf("slf%02d", n), s)
		n++
	}
	// sls: string-metatable arithmetic, the 5.4 coercion model (row 66)
	n = 0
	for _, s := range []string{
		"print('7' // 2, '0x10' + 0, '2.5' + 0, '-13' % 5)\n",
		"print(math.type('7' + 1), math.type('7.0' + 0))\n", // subtype preserved
		"print(getmetatable('') == getmetatable('x'), getmetatable('').__index == string)\n",
		"local ok, e = pcall(function() return '3' & 5 end)\nprint(ok, e:find('bitwise operation on a string value') ~= nil)\n",
		"local ok, e = pcall(function() return ('x') + 1 end)\nprint(ok, e:find('arithmetic on a string value') ~= nil)\n",
		"local ok, e = pcall(function() return -('x') end)\nprint(ok, e:find('arith on a string value') ~= nil)\n",
	} {
		add(fmt.Sprintf("sls%02d", n), s)
		n++
	}

	t.Logf("wrote %d cases to %s", len(cases), dir)
}
