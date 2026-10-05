-- U5d: VM-called __close continuations (kinds 4/5, row 84) — a Lua
-- __close runs as a VM frame tagged with the interrupted owner
-- instruction's resume state: 4 = OP_CLOSE (a do-block exit), 5 = a
-- RETURN (results in flight). A yield inside the __close suspends;
-- the resume completes the close and the interrupted instruction.

-- locals.lua:858 shape: yield inside a VM-called __close (OP_CLOSE of a do-block)
local co = coroutine.create(function()
  do
    local x <close> = setmetatable({}, {__close=function(_, e) coroutine.yield("x1") end})
    coroutine.yield("body")
  end
  coroutine.yield("closed")
  return "done"
end)
print(coroutine.resume(co))   -- yields in body
print(coroutine.resume(co))   -- hits OP_CLOSE -> __close yields "x1"
print(coroutine.resume(co))   -- __close returns, block done, yields "closed"
print(coroutine.resume(co))   -- returns "done"
print(coroutine.status(co))

-- kind 5: __close yields while the owner is RETURNING
local function mk(name)
  return setmetatable({}, {__close=function(_, e) coroutine.yield(name.."1") end})
end
local co = coroutine.create(function()
  local function inner()
    local y <close> = mk("y")
    return "res"
  end
  local r = inner()          -- inner's RETURN triggers __close -> yields
  coroutine.yield("inner returned "..r)
  return "outer-done"
end)
print(coroutine.resume(co))
print(coroutine.resume(co))
print(coroutine.resume(co))
-- RETURN of a vararg/multret form with a close (results in flight)
local co2 = coroutine.create(function()
  local function inner2()
    local z <close> = mk("z")
    return 1, 2, 3
  end
  local a, b, c = inner2()
  coroutine.yield(a..b..c)
end)
print(coroutine.resume(co2))
print(coroutine.resume(co2))
print(coroutine.status(co2))
