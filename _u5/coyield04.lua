-- U5d: the pairs continuation marker and yielding for-iterators —
-- __pairs runs as the marker body (a yield suspends MID-pairs; the
-- resume values are its results), and the generic for's iterator call
-- is a VM call (stock TFORCALL's luaD_call is yieldable), so a yielding
-- iterator suspends through the loop with the resume values landing in
-- the result window the TFORLOOP reads.

-- __pairs that yields (nextvar.lua:937 shape)
local co = coroutine.create(function()
  local t = setmetatable({}, {__pairs = function(tt)
    coroutine.yield(10)
    return next, tt, 30
  end})
  local iter, s, ctl
  iter, s, ctl = pairs(t)     -- the __pairs call yields MID-pairs
  coroutine.yield("got iters")
  local res = {iter(s, ctl)}
  coroutine.yield(#res)
end)
print(coroutine.resume(co))
print(coroutine.resume(co, 20))
print(coroutine.resume(co))

-- a yielding generic-for iterator
local co2 = coroutine.create(function()
  local items = {"a", "b", "c"}
  for i, v in ipairs(items) do
    coroutine.yield(i, v)
  end
  return "loop-done"
end)
print(coroutine.resume(co2))
print(coroutine.resume(co2))
print(coroutine.resume(co2))
print(coroutine.resume(co2))
print(coroutine.status(co2))

-- a custom iterator that yields INSIDE the loop body call
local co3 = coroutine.create(function()
  local function iter(s, ctl)
    if ctl >= 2 then return nil end
    local got = coroutine.yield("iter", ctl)
    return ctl + 1, got
  end
  for k, v in iter, nil, 0 do
    print("visited", k, v)
  end
  return "custom-done"
end)
print(coroutine.resume(co3))
print(coroutine.resume(co3, "V1"))
print(coroutine.resume(co3, "V2"))
print(coroutine.status(co3))
