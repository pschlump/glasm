print("dd06")
-- TFORCALL on the direct arm: a compiled iterator of THIS module runs
-- lua_dispatch directly (want = C raw). The iterator's bare `return`
-- (nret=0) is the load-bearing nil-pad case -- nothing runs
-- luaD_poscall between the callee and the loop's R(A+3) ~= nil check,
-- so rt_dd_ret must nil-pad the result slots or the loop reads stale
-- cells and never terminates. next() (a C function) is the decline
-- contrast: pairs() keeps the rt_call_addr path.
local function iter(t, i)
  i = i + 1
  if i <= #t then return i, t[i] end
end
local t = {"a", "b", "c"}
for i, v in iter, t, 0 do print(i, v) end
for k, v in pairs(t) do print(k, v) end
local function iter2(t, i)
  i = i + 1
  if i <= 3 then return i, i * i, i * i * i end
end
for i, sq, cu in iter2, t, 0 do print(i, sq, cu) end
-- iterator raising through the direct arm (rt_settable position, the
-- dd01 discipline: no error() level walk)
local function baditer(t, i)
  local x = nil
  x.f = 1
end
print(pcall(function()
  for i, v in baditer, t, 0 do print(i, v) end
end))
