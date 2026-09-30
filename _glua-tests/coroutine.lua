co = coroutine.wrap(function()
  co()
end)

local ok, msg = pcall(function()
  co()
end)
assert(not ok and string.find(msg, "cannot resume non%-suspended coroutine"))

co = coroutine.wrap(function()
  return 1
end)
assert(co() == 1)
local ok, msg = pcall(function()
  co()
end)
assert(not ok and string.find(msg, "cannot resume dead coroutine"))
