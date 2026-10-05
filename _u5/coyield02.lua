-- U5d: the noyield depth — a yield inside a Go-driven call region
-- (sort comparator, gsub replacement, the tostring/__tostring
-- dispatch) raises the CATCHABLE boundary error; a pcall marker or a
-- plain pcall in the coroutine catches it and the THREAD SURVIVES.
-- coroutine.isyieldable consults the same depth. The boundary and
-- outside-coroutine texts are bare (luaG_runerror at the yield frame).

-- 1: sort comparator
local co = coroutine.create(function()
  print(pcall(function()
    local t = {3, 1, 2}
    table.sort(t, function(a, b) coroutine.yield("nope") return a < b end)
  end))
  print("after sort-pcall")
  coroutine.yield("still alive")
end)
print(coroutine.resume(co))
print(coroutine.resume(co))
print(coroutine.status(co))

-- 2: isyieldable inside the same callback is false
local co2 = coroutine.create(function()
  local t = {3, 1, 2}
  table.sort(t, function(a, b) print("in sort:", coroutine.isyieldable()) return a < b end)
  coroutine.yield("sorted")
end)
print(coroutine.resume(co2))
print(coroutine.isyieldable())            -- main thread: false

-- 3: gsub replacement + the __tostring dispatch
local co3 = coroutine.create(function()
  print(pcall(string.gsub, "axb", "x", function() coroutine.yield() end))
  print(pcall(tostring, setmetatable({}, {__tostring = function() coroutine.yield() end})))
  coroutine.yield("alive3")
end)
print(coroutine.resume(co3))
print(coroutine.resume(co3))

-- 4: yield from the main thread: the outside-coroutine error
print(pcall(coroutine.yield, "x"))

-- 5: the boundary error through create(pcall) — the entry marker catches
local co5 = coroutine.create(pcall)
print(coroutine.resume(co5, function() return coroutine.yield("y") end))
print(coroutine.resume(co5, "r"))
