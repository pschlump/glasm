assert(math.fmod(13.5, 2) == 1.5)

-- U3b re-pin: math.pow is removed (5.3 removals, D8) and the zero-arg
-- arity error is stock's "value expected" text (the old corpus asserted
-- the fork's "wrong number of arguments" wording — row 25 closes).
local ok, msg = pcall(function()
  math.max()
end)
assert(not ok and string.find(msg, "value expected"))

local ok, msg = pcall(function()
  math.min()
end)
assert(not ok and string.find(msg, "value expected"))

assert(math.pow == nil)
