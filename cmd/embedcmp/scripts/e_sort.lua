-- e_sort: the compute-ish tail — 500-element build + table.sort +
-- aggregate — so the corpus is not all sub-10µs shapes.
local t = {}
for i = 1, 500 do
  t[i] = (i * 7919) % 100003
end
table.sort(t)
return string.format("%d %d", t[1], t[500])
