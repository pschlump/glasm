-- U5d: pcall/xpcall as protected marker frames (row 22/84) — a yield
-- crossing them suspends the thread instead of unwinding the Go stack.
-- The body continues after the resume; pcall's true-prefix completes at
-- the marker; a raise after a resumed yield still finds the marker.
-- Bare errors here (the boundary/pcall texts carry no position).

-- 1: yield inside a pcall body; resume values become the yield's returns
local co = coroutine.create(function()
  local ok, v = pcall(function()
    local a, b = coroutine.yield("inner")
    return (a or 0) + (b or 0)
  end)
  coroutine.yield("pcall returned: "..tostring(ok).." "..tostring(v))
end)
print(coroutine.resume(co))        -- true inner
print(coroutine.resume(co, 20, 22)) -- body continues, pcall completes -> yield
print(coroutine.resume(co))        -- true (thread done)
print(coroutine.status(co))

-- 2: coroutine.yield AS the pcall payload (G body over the marker)
local co2 = coroutine.create(function()
  local ok, v = pcall(coroutine.yield, "y1")
  print("after", ok, v)
  return "done"
end)
print(coroutine.resume(co2))
print(coroutine.resume(co2, "r1"))
print(coroutine.status(co2))

-- 3: nested pcall markers — the inner one catches
local co3 = coroutine.create(function()
  print(pcall(pcall, function()
    coroutine.yield("deep")
    error("boom")
  end))
  print("survived")
end)
print(coroutine.resume(co3))
print(coroutine.resume(co3))
print(coroutine.status(co3))

-- 4: error AFTER a resumed yield inside xpcall — msgh runs at raise
local co4 = coroutine.create(function()
  local ok, e = xpcall(function() coroutine.yield("y1"); error("boom") end,
    function(m) return "handled: "..m end)
  print("xpcall:", ok, e)
end)
print(coroutine.resume(co4))
print(coroutine.resume(co4))

-- 5: tailcall INTO a pcall marker (the frame is replaced wholesale)
local co5 = coroutine.create(function()
  local function tail(f) return pcall(f) end
  local ok, v = tail(function() coroutine.yield("t") return 7 end)
  coroutine.yield("got "..tostring(ok).." "..tostring(v))
end)
print(coroutine.resume(co5))
print(coroutine.resume(co5))
print(coroutine.status(co5))
