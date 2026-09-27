package testdiff

// M6a spike record: the existing SJLJ/new-EH runtime blob
// (lua51_sjlj.wasm) runs on wazero — the pure-Go production engine —
// behind experimental.CoreFeaturesExceptionHandling. The case set leans
// hard on the EH-crossing seams (pcall, core raises, sort comparators,
// gsub callbacks, error propagation) because the M5 error-safety
// invariant is what makes EH-on-wazero possible: no throw ever crosses a
// host or script boundary.
//
// The iowrite case asserts engine PARITY (wazero ≡ wasmtime), not interp
// equality: interp's io.write goes to the real process stdout and emits no
// STDOUT log lines, while every WASI-hosted engine captures them (ledger
// row 29's engine obligation; the _cli-tests harness compares process
// stdout and covers the interp side).

import (
	"testing"
)

func TestWazeroSpike(t *testing.T) {
	cases := []struct{ name, src string }{
		{"hello", `print("hello")`},
		{"arith", `local x = 40
local y = 2
print(x + y, x * y, x / y, x - y, x % y, x ^ y)`},
		{"loopconcat", `for i = 1, 3 do print("i", i, "#" .. i) end`},
		{"closure", `local fns = {}
for i = 1, 3 do fns[i] = function() return i * 10 end end
for i = 1, 3 do print(fns[i]()) end`},
		{"pcall_error", `print(pcall(function() error("boom") end))`},
		{"pcall_core_raise", `print(pcall(function() local t = nil return t.x end))`},
		{"uncaught", `error("x")`},
		{"sort", `local t = {}
for i = 1, 50 do t[i] = 51 - i end
table.sort(t)
print(t[1], t[25], t[50])`},
		{"sort_comparator", `local t = {}
for i = 1, 50 do t[i] = 51 - i end
table.sort(t, function(a, b) return a < b end)
print(t[1], t[50])`},
		{"sort_error_through_pcall", `local t = {3, 1, 2}
print(pcall(function()
  table.sort(t, function(a, b) if a == 2 then error("cmp") end return a < b end)
end))`},
		{"gsub_fn", `print((string.gsub("hello world", "(%w+)", function(w) return w:upper() end)))`},
		{"tailcall", `local function f(n) if n == 0 then return "done" end return f(n - 1) end
print(f(100000))`},
		{"callmeta", `local mt = {__call = function(self, a, b) return a + b end}
local f = setmetatable({}, mt)
print(f(3, 4))`},
		{"iowrite", `io.write("line one\n")
io.write("line two\n")`},
	}
	interp := NewInterp("interp")
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "pcall_core_raise" {
				// U2e: the interp's index-error wording is still gopher
				// dialect; the v4 blob speaks stock 5.5 ("attempt to
				// index a nil value") — the wording re-pins with the
				// corpora at U2f (row 56)
				t.Skip("index error text is gopher dialect on interp — row 56 (re-pin at U2f)")
			}
			c := Case{Name: tc.name + ".lua", Dir: ".", Source: []byte(tc.src)}
			wazero := (&WazeroEngine{name: "wazero", SkipUnsupported: true}).Run(c)
			if tc.name == "iowrite" {
				wasmtime := (&WasmEngine{name: "wasm"}).Run(c)
				if d := DiffLogs(wasmtime, wazero); d != "" {
					t.Errorf("ENGINE PARITY DIFF wasmtime vs wazero: %s\n  wasmtime: %v\n  wazero: %v", d, wasmtime, wazero)
				}
				return
			}
			want := interp.Run(c)
			if d := DiffLogs(want, wazero); d != "" {
				t.Errorf("DIFF interp vs wazero: %s\n  interp: %v\n  wazero: %v", d, want, wazero)
			}
		})
	}
}
