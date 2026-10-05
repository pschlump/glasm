-- U5c: the '__call' chain (row 83) — full resolution with argument
-- shifting (every link becomes a leading argument), extraargs = the
-- chain count (getinfo 't'), the 16-hop cap (15 allowed, the 16th
-- insert raises), and the type error naming the LAST failing value.
-- Position texts go through strip() (the clua55 runner's arg prelude
-- shifts positions by one line).
local function strip(m) return (tostring(m):gsub("^.-:%d+: ", "")) end

-- 1: multi-hop chains shift the links in as leading arguments
local function leaf(...) return select('#', ...) end
local c = leaf
for i = 1, 5 do c = setmetatable({}, {__call = c}) end
print(c(7, 8))
print(leaf("x"))

-- 2: extraargs is the chain count, not the vararg count
local function cnt(...) return debug.getinfo(1, "t").extraargs, select('#', ...) end
local d = cnt
for i = 1, 3 do d = setmetatable({}, {__call = d}) end
print(d(1, 2, 3, 4, 5))          -- 3 hops + 5 arguments
print(cnt(1, 2, 3, 4, 5))        -- plain call: 0 hops + 5 arguments

-- 3: the cap — 15 hops fine, the 16th raises (bare: the caller is C)
local v = function() end
for i = 1, 15 do v = setmetatable({}, {__call = v}) end
print("15 hops:", v == nil, pcall(v) == true)
v = setmetatable({}, {__call = v})
local ok, msg = pcall(v)
print(ok, strip(msg))

-- 4: the cap through a Lua caller (position present, stripped here)
local v16 = function() end
for i = 1, 16 do v16 = setmetatable({}, {__call = v16}) end
local function callit(f) return f() end
ok, msg = pcall(callit, v16)
print(ok, strip(msg))

-- 5: the type error names the chain HEAD (the value being retried)
local w = setmetatable({}, {__call = 5})
ok, msg = pcall(function() return w() end)
print(ok, strip(msg))

-- 6: a tailcall through a chain (the shift crosses the tail boundary)
local function tail(...) return "T", select('#', ...) end
local e = tail
for i = 1, 2 do e = setmetatable({}, {__call = e}) end
local function outer(...) return e(...) end
print(outer(1, 2, 3))

-- 7: the counter resets per call site (a 15-hop chain right after a
--    capped one still works)
local fine = function() end
for i = 1, 15 do fine = setmetatable({}, {__call = fine}) end
print(pcall(fine))
print("done")
