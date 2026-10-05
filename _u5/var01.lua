-- U5b: the named vararg parameter's indexed reads (luaT_getvararg):
-- t.n (integer subtype), integral keys incl. exact-int floats, the
-- "n" string key, out-of-range/negative/zero keys, non-integral
-- floats, arbitrary other keys -> nil, empty extras.
local function f(a, b, ...t)
  print(t.n, math.type(t.n))
  print(t[1], t[2], t[3])
  print(t[0], t[-1], t[4])
  print(t[1.0], t[2.0], t[1.5])
  print(t["n"], t.n)
  print(t["x"], t[true], t[nil])
  print(select('#', ...), ...)
end

f(10, 20, 30, 40, 50)
f(10, 20)
f(10, 20, 30)

-- reads only see the fixed extras; nothing was copied at call time
local function g(...t)
  local n = t.n
  local s = 0
  for i = 1, n do s = s + (t[i] or 0) end
  print(n, s)
end
g(1, 2, 3, 4)
g()

-- a float key with an exact integer value converts (tointegerns); a
-- huge float key that is integral but out of range -> nil
local function h(...t)
  print(t[2.0], t[1e15])
end
h("a", "b", "c")
print("done")
