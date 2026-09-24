-- debug lib tests
-- debug stuff are  partially implemented; hooks are not supported.

local function f1()
end
local mt = {}
-- (5.2 removed debug.setfenv/getfenv — keep the line count stable; the
-- getinfo asserts below are position-sensitive)
--
--
debug.setmetatable(f1, mt)
assert(debug.getmetatable(f1) == mt)

local function f2()
  local info = debug.getinfo(1, "Slunf")
  assert(info.currentline == 15)
  assert(info.linedefined == 14)
  assert(info.func == f2)
  assert(info.lastlinedefined == 26)
  assert(info.nups == 2) -- +_ENV (5.2 globals resolve through the _ENV upvalue)
  assert(info.name == "f2")
  assert(info.what == "Lua")
  if string.find(_VERSION, "GopherLua") then
    assert(info.source == "db.lua")
  end
end
f2()

local function f3()
end
local info = debug.getinfo(f3)
assert(info.currentline == -1)
assert(info.linedefined == 29)
assert(info.func == f3)
assert(info.lastlinedefined == 30)
assert(info.nups == 0) -- f3 references no globals, so no _ENV capture either
assert(info.name == nil)
assert(info.what == "Lua")
if string.find(_VERSION, "GopherLua") then
  assert(info.source == "db.lua")
end

local function f4()
  local a,b,c = 1,2,3
  local function f5()
    local name, value = debug.getlocal(2, 2)
    assert(debug.getlocal(2, 10) == nil)
    assert(name == "b")
    assert(value == 2)
    name = debug.setlocal(2, 2, 10)
    assert(debug.setlocal(2, 10, 10) == nil)
    assert(name == "b")

    local d = a
    local e = c

    local tb = debug.traceback("--msg--")
    assert(string.find(tb, "\\-\\-msg\\-\\-"))
    assert(string.find(tb, "in.*f5"))
    assert(string.find(tb, "in.*f4"))
  end
  f5()
  local name, value = debug.getupvalue(f5, 1)
  assert(debug.getupvalue(f5, 10) == nil)
  assert(name == "_ENV") -- 5.2: globals give f5 an _ENV upvalue at slot 1
  name, value = debug.getupvalue(f5, 2)
  assert(name == "a")
  assert(value == 1)
  name = debug.setupvalue(f5, 2, 11)
  assert(debug.setupvalue(f5, 10, 11) == nil)
  assert(name == "a")
  assert(a == 11)

  assert(b == 10) -- changed by debug.setlocal in f4
end
f4()

local ok, msg = pcall(function()
  debug.getlocal(10, 1)
end)
assert(not ok and string.find(msg, "level out of range"))

local ok, msg = pcall(function()
  debug.setlocal(10, 1, 1)
end)
assert(not ok and string.find(msg, "level out of range"))

assert(debug.getinfo(100) == nil)
assert(debug.getinfo(1, "a") == nil)
