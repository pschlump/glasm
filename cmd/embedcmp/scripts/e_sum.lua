-- e_sum: aggregate over KEYS (MGET/aggregation shape) — string
-- lengths + suffix concat.
local n, s = 0, ""
for i = 1, #KEYS do
  n = n + #KEYS[i]
  s = s .. string.sub(KEYS[i], -2)
end
return string.format("%d %s", n, s)
