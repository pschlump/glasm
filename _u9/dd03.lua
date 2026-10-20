print("dd03")
-- vararg callees on the direct arm: rt_dd_enter's rt_push_frame_from
-- packs the extras BELOW the window (row 86); truncation to a fixed
-- want and extra-argument drops ride rt_dd_ret.
local function v(...)
  return select('#', ...), ...
end
local function caller(a, b)
  return v(a, b, 9)
end
print(caller(1, 2))
local p, q = caller(1, 2)
print(p, q)
local function passthrough(...)
  return v(...)
end
print(passthrough(4, 5, 6))
local function fixed(x) return x end
local function call2()
  return fixed(10, 20, 30)
end
print(call2())
local function named(a, ...t)
  return a, t.n, t[1]
end
local function calln()
  return named(7, 8, 9)
end
print(calln())
