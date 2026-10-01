-- U5a: RDKCTC promotion — register-less compile-time <const> locals:
-- chained folding, string/bool/nil values, assignment lock, debug
-- invisibility, capture-free nesting, mixed declarations.
local function try(src)
  local f, err = load(src, "=t")
  if f == nil then
    print((err:gsub("^t:%d+: ", "")))
  else
    local ok, e = pcall(f)
    if not ok then print("ERR: " .. tostring(e)) end
  end
end

-- the code.lua folding chain
local k0 <const> = 0
local k1 <const> = 1
local k3 <const> = 3
local k6 <const> = k3 + (k3 << k0)
local k16 <const> = k6 << (k1 + k0)
print(k0, k1, k3, k6, k16)

-- non-numeric constants
local s <const> = "hi"
local t <const> = true
local f <const> = false
local n <const> = nil
local f2 <const> = 1.5
print(s, t, f, n, f2)

-- negative and float fold chains keep their subtype
local m <const> = -3
local mm <const> = m * m
local ff <const> = 2.0 * k1
print(m, mm, ff, ff == 2.0, math.type(ff))

-- uses everywhere a literal goes: table keys, arith, comparisons
local tt = {[s] = 1, [k3] = 2}
print(tt[s], tt[k3], k6 > k3, k1 + k16)

-- assignment is a compile error
try("local q <const> = 1; q = 2")
try("local q <const> = 1; local function g() q = 3 end")

-- a NON-foldable initializer keeps the register (plain <const>)
local nf <const> = k0 + math.floor(2.5)
print(nf)

-- only the LAST name of an equal-count decl promotes; the others stay
local p1, p2 <const> = 10, 20
local p3 <const> = p2 + 1
print(p1, p2, p3)
-- p1 is a real local: assignable, visible to debug.getlocal
p1 = 11
print(p1, debug.getlocal(1, 1))

-- RDKCTC entries are invisible to debug.getlocal (stock records
-- locvars only for stack-resident variables)
local before = debug.getlocal(1, 2)   -- the function value? no: locals of this chunk
local x <const> = 99
print(x, debug.getlocal(1, 2), before)

-- nested functions fold the constant without capturing an upvalue
local outer <const> = 1234
local function cap() return outer end
print(cap(), debug.getupvalue(cap, 1))

-- RDKCTC shadows correctly and restores after its block
local name = 1
do
  local name <const> = 2
  print(name)
end
print(name)

-- reference through TWO levels (VCONST propagates at any chain depth)
local lvl1 <const> = 5
local function mid()
  local function inner() return lvl1 * 2 end
  return inner()
end
print(mid())
print("done")
