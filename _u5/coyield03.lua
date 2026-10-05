-- U5d: coroutine.create over the marker builtins (row 84) — the entry
-- function IS pcall/xpcall: the base frame becomes the protected marker
-- and the payload (staged at register 1 by the resume) is its body, so
-- a yield inside suspends the thread; errors surface as the resume's
-- false+object instead of killing it. The wrap flavor composes.

-- create(pcall): official coroutine.lua:277 shape
local co = coroutine.create(pcall)
print(coroutine.resume(co, function() coroutine.yield("inside"); return 1 end))
print(coroutine.resume(co, 10))
print(coroutine.status(co))

-- create(xpcall): msgh at raise, error object through the resume
local co2 = coroutine.create(xpcall)
print(coroutine.resume(co2, function(a) coroutine.yield(a); error("e2") end,
  function(m) return "H:" .. tostring(m) end, "z"))
print(coroutine.resume(co2))
print(coroutine.status(co2))

-- the wrap flavor over pcall
local f = coroutine.wrap(pcall)
print(f(function() coroutine.yield("w1") end))
print(f())

-- a non-yielding create(pcall) still delivers the pcall protocol
local co4 = coroutine.create(pcall)
print(coroutine.resume(co4, function() error("plain") end))
print(coroutine.status(co4))
