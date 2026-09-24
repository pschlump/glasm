-- _ENV shadowing by a local: global reads AND writes go through it
local printf, rawget, type = print, rawget, type
local t = setmetatable({}, {
  __index = function(_, k) if k == "num" then return 21 end; return k .. "!" end,
  __newindex = function(tb, k, v) rawset(tb, k, v * 2) end,
})
local _ENV = t
v = num
printf(v, rawget(t, "v"))
printf(world)
printf(hello .. " " .. num)
do
  local _ENV = {printf2 = function(a) printf(a) end, val = 3}
  printf2(val)
end
printf(rawget(t, "v"))
for i = 1, 2 do
  local _ENV = {pf = function(a) printf(a) end}
  pf(i)
end
-- a global function definition under the shadow defines in the shadow env
local mt = {}
local _ENV = mt
function defined_here() return "fn" end
printf(type(defined_here), mt.defined_here ~= nil, defined_here())
