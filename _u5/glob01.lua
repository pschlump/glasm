-- U5a: `global` declarations — forms, voiding, read-only, ERRNNIL
-- redefinition, chain visibility, named-global shadowing, the
-- contextual-keyword ruling (row 78a), `global function`.
local function try(src)
  local f, err = load(src, "=t")
  if f == nil then
    print((err:gsub("^t:%d+: ", "")))
  else
    local ok, e = pcall(f)
    if not ok then print("ERR: " .. tostring(e)) end
  end
end

-- plain declarations and initializers
global a
a = 1
print(a)
global b, c = 2, 3
print(b, c)
print(a + b + c)

-- collective preamble stays open
global *
d = 4
print(d)

-- a NAMED declaration voids the implicit preamble below it
try("global none; X = 1")
try("global none; function XX() end")
try("global none; return XX + 1")

-- reads are voided too
try("global none; return X")

-- a collective re-opens it
try("global none; global *; Y = 1; print(Y)")

-- read-only globals
global ro <const> = 42
print(ro)
try([[global ro2 <const> = 1; ro2 = 2]])
try([[global <const> *; anyname = 1]])
try([[global <const> *; local function f() othername = 1 end]])

-- collective <const> still allows fresh named declarations to be READ
try([[global <const> *; global e = 5; print(e)]])

-- ERRNNIL redefinition (runtime)
try("global f1 = 1; global f1 = 2")
try("global function g1() end; global function g1() end")
-- nil-valued redefinition is fine
global h1
global h1 = 10
print(h1)

-- global function recursion sees the name
global function fib(n)
  if n < 2 then return n end
  return fib(n-1) + fib(n-2)
end
print(fib(10))

-- global function bodies can use globals declared before them
global u, v = 20, 22
global function sum() return u + v end
print(sum())

-- global variables cannot be to-be-closed
try("global X<close>")
try("global <close> *")

-- '_ENV' cannot be declared global
try("global _ENV, a2; a2 = 10")

-- contextual 'global' (not reserved — row 78a, the oracle default build)
local global = 42
print(global)
do
  local function shadow() local global = 7 return global end
  print(shadow())
end

-- named global shadows an outer local (goto.lua:311 shape)
do
  local X = 10
  do global X; X = 20 end
  assert(X == 10)             -- the local
  assert(_ENV.X == 20)        -- the global
  print(X, _ENV.X)
end

-- chain visibility: an enclosing function's declaration locks nested
-- writes and rescues reads from not-declared (locals.lua:217 shape)
try([[
  global a9, var1 <const>, z9;
  local function foo() a9 = 20; z9 = function () var1 = 12; end end
]])
try([[
  global m1;
  local function foo() return m1 + 1 end
  foo()
]])
print("done")
