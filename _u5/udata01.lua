-- U5c: light userdata (row 83) — debug.upvalueid produces one (type
-- "userdata", distinct from every other type tag), full-userdata
-- argument checks report "light userdata" for it, and the io files
-- (the fork's only stock-visible full userdata) answer the zero-slot
-- uservalue surface. No addresses printed (they differ per engine).
local function strip(m) return (tostring(m):gsub("^.-:%d+: ", "")) end

-- 1: upvalueid: same cell -> same id; distinct cells differ;
--    out-of-range or a builtin answers nil
local function mk()
  local up = 1
  return function() return up end, function() up = up + 1; return up end
end
local a1, a2 = mk()
local b1 = mk()
local lid = debug.upvalueid(a1, 1)
print(type(lid))
print(debug.upvalueid(a1, 1) == debug.upvalueid(a2, 1))
print(debug.upvalueid(a1, 1) == debug.upvalueid(b1, 1))
print(debug.upvalueid(a1, 0), debug.upvalueid(a1, 2))
print(debug.upvalueid(print, 1))
print(getmetatable(lid))

-- 2: the light tag in argument checks
local ok, msg = pcall(debug.setuservalue, lid, {})
print(ok, strip(msg))
ok, msg = pcall(setmetatable, lid, {})
print(ok, strip(msg))
ok, msg = pcall(table.insert, lid, 1)
print(ok, strip(msg))
ok, msg = pcall(string.rep, lid, 2)
print(ok, strip(msg))

-- 3: base setmetatable refuses light userdata outright
ok, msg = pcall(setmetatable, lid, {__index = {}})
print(ok, strip(msg))

-- 4: full userdata (the io files) — zero uservalue slots
print(debug.getuservalue(io.stdout))
print(debug.setuservalue(io.stdout, 42) == 42)
print(debug.getuservalue(io.stdout))
print(debug.getuservalue(io.stdout, 1))

-- 5: non-userdata answers a single nil; a bad first arg raises
print(debug.getuservalue({}))
ok, msg = pcall(debug.setuservalue, 42, 1)
print(ok, strip(msg))
print("done")
