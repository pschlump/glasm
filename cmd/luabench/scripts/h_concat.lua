-- h_concat: concat-operator-heavy (U7c) — thousands of small concats
-- with INTEGER operands (the 5.5 int→string coercion path) plus
-- string-key building at table scale. Kept small-allocation on purpose:
-- a pooled VM's C heap is frozen per run (row 61), so per-run garbage
-- must stay ~100KB-class for a 400-iteration warm leg to fit the
-- memlimit.
local digest = 0
for round = 1, 60 do
  local acc = "s"
  for i = 1, 32 do
    acc = acc .. i
    if #acc > 512 then
      acc = "s"
    end
  end
  digest = (digest + #acc) % 1000003
end
local t = {}
for i = 1, 2000 do
  t["k" .. i] = i
end
local n = 0
for i = 1, 2000 do
  n = n + t["k" .. i]
end
return string.format("%d %d", digest, n)
