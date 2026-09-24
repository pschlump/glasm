-- U2b: numeric-for flavors (integer count loop vs float loop)
local function collect(a, b, c)
  local t = {}
  for i = a, b, c or 1 do t[#t+1] = i end
  return table.concat(t, ",")
end
print(collect(1, 5))            -- 1,2,3,4,5
print(collect(1, 5, 2))         -- 1,3,5
print(collect(5, 1, -1))        -- 5,4,3,2,1
print(collect(5, 1, -2))        -- 5,3,1
print(collect(3, 3))            -- 3
print(collect(1, 0))            -- (empty)
print(collect(1, 2, 10))        -- 1
print(collect(0.5, 2.5))        -- 0.5,1.5,2.5 (float flavor)
-- float flavor (1.0 init): values are floats; avoid tostring/concat of
-- integral floats (interp prints "1", stock "1.0" until the U5 re-pin)
local fs, fn = 0, 0
for i = 1.0, 3 do fs = fs + i fn = fn + 1 end
print(fs == 6, fn == 3)
local hs = 0
for i = 1, 3, 0.5 do hs = hs + 1 end   -- float step: float flavor
print(hs == 5)
-- integer flavor survives steps that float accumulation would fumble
local n = 0
for i = 9007199254740990, 9007199254740992 do n = n + 1 end
print(n == 3)
-- limit conversions: exact floats and numeric strings
print(collect(1, 3.0) == "1,2,3")
print(collect(1, "3") == "1,2,3")
-- a string init is not integer-TYPED: float flavor (assert numerically)
local ss, sn = 0, 0
for i = "1", 3, 1 do ss = ss + i sn = sn + 1 end
print(ss == 6, sn == 3)
print(collect(1, 2.5) == "1,2")
-- huge float limit clamps to maxint (break out immediately)
local seen
for i = 1, 1e100 do seen = i break end
print(seen)
-- (writing the control variable is a 5.5 read-only error — U5 material;
-- the corpus avoids it until then)
-- step zero errors (both flavors)
print((select(2, pcall(function() for i = 1, 10, 0 do end end))):find("'for' step is zero") ~= nil)
print((select(2, pcall(function() for i = 1.0, 10, 0.0 do end end))):find("'for' step is zero") ~= nil)
-- bad operands (stock texts via find — row 52 position discipline)
print((select(2, pcall(function() for i = 1, {} do end end))):find("bad 'for' limit %(number expected, got table%)") ~= nil)
print((select(2, pcall(function() for i = {}, 1 do end end))):find("bad 'for' initial value %(number expected, got table%)") ~= nil)
print((select(2, pcall(function() for i = 1, 10, "x" do end end))):find("bad 'for' step %(number expected, got string%)") ~= nil)
-- numeric-string steps work (string step -> float flavor)
local ts = 0
for i = 1, 2, "1" do ts = ts + 1 end
print(ts == 2)
