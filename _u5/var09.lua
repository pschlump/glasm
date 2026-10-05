-- U5b: the official vararg.lua idioms end-to-end — methods with
-- unnamed vararg, self[...] single-value uses, table.pack/unpack round
-- trips over named varargs, next-style probes, and mixed promotion
-- inside one function (indexed reads BEFORE and AFTER the whole-value
-- use that promotes it).
local t = {}
t[1], t[2], t[3] = 10, 20, 30
function t:f (...) local arg = {...}; return self[...]+#arg end
print(t:f(1, "a", "b"))
print(t:f(3))
print(t:f(2, 1))

-- c12-style helpers (the official file's battery, count-first)
local function c12 (...)
  local a, b, c = ...
  if a == nil then return end
  local n = select('#', ...)
  if n == 2 then return 55, 2 end
  return false
end
print(c12(1, 2))
print(c12())
print(c12(1, 2, 3))

-- vararg-of-vararg probe (the official 'vararg' helper): pack the
-- extras of a named vararg, forward them, read them back
local function vararg(...)
  return table.pack(...)
end
local v = vararg(nil, nil)
print(v.n, v[1], v[2])
local v2 = vararg(vararg(1, 2))
print(v2.n, v2[1].n, v2[1][1], v2[1][2])

-- mixed use inside ONE function: indexed reads on both sides of the
-- whole-value use — after promotion the earlier GETVARG sites became
-- GETTABLEs on the same table, so results are unchanged
local function mixed(a, ...t)
  local n1 = t.n          -- read (GETVARG at compile, GETTABLE after)
  local more = {...}      -- whole-value use: promotes
  local n2 = t.n          -- still the same table
  local first = t[1]
  return n1, n2, #more, first
end
print(mixed(0, "x", "y", "z"))
print(mixed(0))

-- promotion through a RETURN value (the whole vararg leaves the frame)
local function ret(...) return ... end
local function outer(...t) return ret(...) end
print(outer("a", "b", "c"))
print(select('#', outer()))

-- unpack round-trip: extras -> table -> extras -> named vararg
local function round(...t)
  return table.unpack(t, 1, t.n)
end
print(round("p", "q", "r"))
print("done")
