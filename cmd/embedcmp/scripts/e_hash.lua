-- e_hash: hash-table get/set loop (HSET/HGETALL-ish working set).
local h = {}
for i = 1, 40 do
  h["f" .. i] = i * 3
end
local sum = 0
for i = 1, 40 do
  sum = sum + h["f" .. i]
end
return string.format("%d", sum)
