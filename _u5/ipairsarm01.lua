-- ipairsarm01: U14 ipairs arm — ipairsaux lowered at TFORCALL. Plain
-- runs, a hole inside asize (empty slot → one nil result, loop
-- exits), the __index screen (an EMPTY in-bounds slot consults the
-- metamethod in stock — the arm must decline so the loop can
-- CONTINUE on the metamethod's value), and a state with a metatable
-- but no __index (still declines; live slots would match stock but
-- conservative is the contract).
print("ipairsarm01")
local a = {} for i=1,10 do a[i]=i*i end
local s = 0 for i,v in ipairs(a) do s = s + v end print("plain", s)
local h = {} h[1]=1 h[2]=2 h[4]=4
local c = 0 for i,v in ipairs(h) do c = c + 1 end print("hole", c)
local t = setmetatable({}, {__index=function(_,i)
  if i > 3 then return nil end return i*10 end})
local n = 0 local last
for i,v in ipairs(t) do n = n + 1 last = v end
print("mt-continue", n, last)
local mt2 = setmetatable({1,2,3}, {__tostring=function() return "x" end})
local k = 0 for i,v in ipairs(mt2) do k = k + v end print("mt-noindex", k)
