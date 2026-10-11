-- h_tablechurn: hash/array churn — array writes, holes, string-key
-- inserts and removals, then an order-independent pairs aggregate (the
-- sum is commutative, so pairs order across engines does not matter).
local n = 0
for round = 1, 20 do
  local t = {}
  for i = 1, 2000 do
    t[i] = i * round
  end
  for i = 1, 2000, 2 do
    t[i] = nil
  end
  for i = 1, 1000 do
    t["k" .. i] = i
  end
  for i = 1, 500 do
    t["k" .. i] = nil
  end
  n = 0
  for _, v in pairs(t) do
    n = n + v
  end
end
return string.format("%d", n)
