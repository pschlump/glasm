print("dd01")
-- tbc across direct-dispatched frames. inner/middle/boom are all called
-- by OP_CALL from compiled bodies (the direct arm); their <close> vars
-- must run on BOTH retire paths: the clean return (rt_dd_ret after the
-- callee's own rt_close) and the error cascade (rt_dd_err's
-- rt_close_err, which closes the dying DIRECT frames' tbc before the
-- region goes away). The raise is an rt_settable error, not error():
-- its position comes from the rt line parameter (the caller's own line
-- cell), which is exact on the direct path -- error()'s level walk is
-- the row-104 ruling (dd02).
local function inner()
  local a <close> = setmetatable({}, {__close = function() print("close-a") end})
  return 41
end
local function outer()
  local b <close> = setmetatable({}, {__close = function() print("close-b") end})
  return inner() + 1
end
print(outer())
local function boom()
  local t = nil
  t.x = 1
end
local function middle()
  local c <close> = setmetatable({}, {__close = function() print("close-c") end})
  return boom() + 1
end
print(pcall(middle))
