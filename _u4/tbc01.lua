-- U4b: to-be-closed, normal paths — close order, exemptions, loops,
-- goto/break over scopes, returns, upvalue interplay, the non-closable
-- raise (row-70f naming).
local log = {}
local function mk(name)
  return setmetatable({}, {__close = function(_, e) log[#log+1] = name .. "/" .. tostring(e) end})
end
-- innermost-first at block exit
do
  local a <close> = mk("a")
  local b <close> = mk("b")
end
log[#log+1] = "after"
-- nil/false are exempt (no entry, no __close check)
local x <close> = nil
local y <close> = false
log[#log+1] = "exempt-ok"
-- loop bodies close each iteration
for i = 1, 2 do local c <close> = mk("c" .. i) end
-- break and goto out of a tbc scope close it
for i = 1, 10 do local d <close> = mk("d") break end
do local e <close> = mk("e") goto done end
::done::
-- return closes the frame's list
local function f()
  local g <close> = mk("g")
  return 42
end
log[#log+1] = f()
-- non-closable values raise at registration, naming the variable
print(pcall(function() local q <close> = {} end))
-- a value whose metatable gains __close later: checked at registration
local late = setmetatable({}, {})
print(pcall(function() local q <close> = late end))
-- upvalue interplay: the closed-over cell keeps its final value
do
  local up = "u0"
  local function get() return up end
  local function set(v) up = v end
  local c <close> = mk("c")
  set("u1")
  log[#log+1] = "in-block:" .. get()
end
log[#log+1] = "after"
-- __close returning values / erroring on the NORMAL path propagates
print(pcall(function()
  local bad <close> = setmetatable({}, {__close = function() error("normal-path") end})
end))
-- object identity: exactly the registered value closes
local seen = {}
local keeper
do
  local k <close> = setmetatable({}, {__close = function(o) seen[#seen+1] = o end})
  keeper = k
end
print(seen[1] == keeper)
for _, v in ipairs(log) do print(v) end
