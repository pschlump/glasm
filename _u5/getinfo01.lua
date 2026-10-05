-- U5c: debug.getinfo's option filtering (stock db_getinfo) — the
-- default "flnSrtu" key set, each option's fields, the thread-first
-- form, and the two argerrors. No raw sources printed (chunkname
-- differs per engine); positions are stripped where raised.
local function strip(m) return (tostring(m):gsub("^.-:%d+: ", "")) end

-- 1: the default option set is exactly "flnSrtu"
local function f() return debug.getinfo(1) end
local keys = {}
for k in pairs(f()) do keys[#keys+1] = k end
table.sort(keys)
print(table.concat(keys, " "))

-- 2: 'u' — nparams/isvararg/nups (a named-vararg function, U5b)
local function g(a, b, ...) return debug.getinfo(1, "u") end
local i2 = g(1, 2)
print(i2.nparams, i2.isvararg, i2.nups)
print(debug.getinfo(print, "u").nparams,
      debug.getinfo(print, "u").isvararg,
      debug.getinfo(print, "u").nups)

-- 3: 't' — istailcall/extraargs; extraargs is the chain count
local function t1() return debug.getinfo(1, "t") end
print(t1().istailcall, t1().extraargs)
local cnt = function(...) return debug.getinfo(1, "t").extraargs end
local chain = cnt
for i = 1, 2 do chain = setmetatable({}, {__call = chain}) end
print(chain())
local function caller() return t1() end   -- a real tail call
print(caller().istailcall)

-- 4: 'r' — ftransfer/ntransfer (always 0 without hooks)
local function r1() return debug.getinfo(1, "r") end
print(r1().ftransfer, r1().ntransfer)

-- 5: 'L' — activelines of a Lua function; nil for a C function
local function L1()
  local x = 1
  x = x + 1
  return x
end
local al = debug.getinfo(L1, "L").activelines
local ls = {}
for k in pairs(al) do ls[#ls+1] = k end
table.sort(ls)
print(table.concat(ls, ","))
print(debug.getinfo(print, "L").activelines)

-- 6: 'f' — the function value itself
local function f6() return debug.getinfo(1, "f").func end
print(f6() == f6)
print(debug.getinfo(print, "f").func == print)

-- 7: 'S' — what/linedefined for C, Lua, and main
print(debug.getinfo(print, "S").what,
      debug.getinfo(print, "S").linedefined,
      debug.getinfo(print, "S").lastlinedefined)
local function f7() end
local s7 = debug.getinfo(f7, "S")
print(s7.what, s7.linedefined > 0, s7.lastlinedefined >= s7.linedefined)
print(debug.getinfo(1, "S").what)   -- level 1 = the main chunk

-- 8: 'n' — the running frame's name from its call site; a queried
--    function (no frame) answers nil
local function f8() return debug.getinfo(1, "n") end
print(f8().name, f8().namewhat)
print(debug.getinfo(f8, "n").name)

-- 9: the thread-first form
local co = coroutine.create(function() coroutine.yield() end)
coroutine.resume(co)
local ti = debug.getinfo(co, 1, "t")
print(type(ti), ti.istailcall, ti.extraargs)
print(debug.getinfo(co, 5, "t"))

-- 10: out-of-range and negative levels answer nil
print(debug.getinfo(100))
print(debug.getinfo(-1))

-- 11: the argerrors (positions stripped)
local ok, msg = pcall(debug.getinfo, 1, "a")
print(ok, strip(msg))
ok, msg = pcall(debug.getinfo, 1, ">f")
print(ok, strip(msg))
print("done")
