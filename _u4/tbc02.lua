-- U4d: to-be-closed on error paths — 2-arg __close with the in-flight
-- error object, last-error-wins (luaD_closeprotected), closes continuing
-- after a failing one, error(nil) inside __close, nested pcall levels.
local log = {}
local function mk(name)
  return setmetatable({}, {__close = function(_, e) log[#log+1] = name .. "/" .. tostring(e) end})
end
local function bad(name)
  return setmetatable({}, {__close = function(_, e) log[#log+1] = name .. "!/" .. tostring(e) error(name .. "-bad") end})
end
-- clean closes with the error object, innermost first
print(pcall(function()
  local a <close> = mk("a")
  local b <close> = mk("b")
  error("orig")
end))
-- a failing __close replaces the in-flight error; closing continues
print(pcall(function()
  local a <close> = mk("a")
  local b <close> = bad("b")
  error("orig")
end))
-- two failing closers: the FIRST failure reported (last raised wins =
-- the one raised earliest in the unwind)
print(pcall(function()
  local a <close> = bad("a")
  local b <close> = bad("b")
  error("orig")
end))
-- a __close error with NO in-flight error (normal exit) propagates
print(pcall(function()
  local c <close> = bad("c")
end))
-- error(nil) inside __close: the object is the no-error-object string
print(pcall(function()
  local d <close> = setmetatable({}, {__close = function() error(nil) end})
end))
-- nested pcall: the inner recovers, the outer sees clean closes
print(pcall(function()
  local e <close> = mk("e")
  print(pcall(error, "inner"))
  log[#log+1] = "survived"
end))
-- a tbc var registered then the function returns normally via pcall
print(pcall(function()
  local f <close> = mk("f")
  return "ret"
end))
-- non-closable under an in-flight error: still the registration raise
print(pcall(function()
  local g <close> = {}
  error("orig2")
end))
for _, v in ipairs(log) do print(v) end
