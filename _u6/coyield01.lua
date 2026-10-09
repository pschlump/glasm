-- U6f (row 97): the wasm yield posture, pinned absolutely — these
-- outputs are the BACKEND's, not the interp's (which suspends the
-- same shapes; see _u5/coyield01-06, skipped on the wasm legs with
-- this row). Compiled frames are CClosures over the dispatch seam and
-- every rt_* call from compiled code runs on the rt-pinned MAIN
-- state, so a yield reaches lua_yieldk on the main thread: stock's
-- own branch (ldo.c:1015) raises the catchable, position-prefixed
-- "attempt to yield from outside a coroutine".

-- 1: a yield-free coroutine body runs to completion
local co = coroutine.create(function(a, b)
  print("coyield01", "body:", a + b)
  return "done"
end)
print(coroutine.resume(co, 20, 22))
print(coroutine.status(co))

-- 2: a direct yield raises; uncaught, resume answers false and the
-- thread is dead
local co2 = coroutine.create(function()
  coroutine.yield("y")
end)
print(coroutine.resume(co2))
print(coroutine.status(co2))

-- 3: caught inside the body — the thread survives the raise
local co3 = coroutine.create(function()
  print("caught:", pcall(coroutine.yield, "y"))
  print("alive")
end)
print(coroutine.resume(co3))
print(coroutine.status(co3))

-- 4: isyieldable answers false in a compiled body (the interp's body
-- answers true — a value divergence, row 97)
local co4 = coroutine.create(function()
  print("isyieldable:", coroutine.isyieldable())
end)
print(coroutine.resume(co4))

-- 5: the wrap flavor — the raise crosses the wrap boundary, catchable
local w = coroutine.wrap(function() coroutine.yield("w") end)
print(pcall(w))
