print("dd04")
-- deep PURE-DIRECT recursion caught by pcall, then execution CONTINUES:
-- the depth counter must heal across the catch (a leaked budget would
-- fail the follow-up direct calls, not the second pcall -- g(21) is the
-- detector) and the catch text must be the stock "stack overflow"
-- (matched loosely: the line prefix is engine-positioned, the wording
-- is the pinned surface). The budget arm fires inside the -1 cascade
-- and stages prefixed (row 104); pcall sees it verbatim.
local function f(n)
  return 1 + f(n + 1)
end
print((select(2, pcall(f, 0))):match("stack overflow") ~= nil)
local function g(x) return x * 2 end
print(g(21))
print((select(2, pcall(f, 0))):match("stack overflow") ~= nil)
print(g(20))
-- and once more through a mixed chain: trampoline (gsub callback) with
-- direct recursion inside it
local function deep(n)
  if n <= 0 then return 0 end
  return 1 + deep(n - 1)
end
local s = string.gsub("x", "x", function(w)
  return tostring(deep(50))
end)
print(s, deep(60))
