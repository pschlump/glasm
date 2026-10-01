-- U5a: the ERRORSTACKSIZE analog (row 78e) — a stack overflow leaves
-- the frame stack full; the message handler still has room to run and
-- to create its own to-be-closed variable, and a tbc variable down the
-- stack still closes at the pcall boundary. (The divergent boundary —
-- a handler exhausting the reserve — is documented row 78e and NOT
-- pinned here: stock says "error in error handling" there.)
local function overflow(n) overflow(n + 1) end

-- 1: plain handler runs and reports
local function h1(m)
  print("h1:", (tostring(m)):find("stack overflow") ~= nil)
  return 42
end
local st, res = xpcall(overflow, h1, 0)
print(st, res)

-- 2: handler creates a tbc variable high in the dying stack; a tbc
--    variable down the stack closes with the boundary (no message)
local flag
local function h2(m)
  local x <close> = setmetatable({}, {__close = function(o) o.v = 10 end})
  return x
end
local co = coroutine.wrap(function()
  local y <close> = setmetatable({}, {__close = function(o, e)
    print("y-close:", e == nil)
    o.v = 100
    flag = o
  end})
  st, res = xpcall(overflow, h2, 0)
  print("h2:", st, res.v)
end)
co()
print("flag:", flag.v)

-- 3: a print-only handler (reserve consumed by the call itself)
local function h3(m)
  print("h3:", type(m))
  return m
end
st, res = xpcall(overflow, h3, 0)
print("h3 done:", st, type(res))

-- 4: overflow inside pcall WITHOUT a handler still reports cleanly
st, res = pcall(overflow, 0)
print(st, (tostring(res)):find("stack overflow") ~= nil)

-- 5: after all that, the machine is still sane
print("alive", (function() return 1 + 1 end)())
print("done")
