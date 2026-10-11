-- h_numloop: numeric-loop heavy (U7c, generalized from the U7a hot
-- probe). Two 2M-iteration lanes: an integer accumulator and a float
-- accumulator fed by int*floatK (the MULK mixed-type shape). The int
-- lane resets the accumulator every 60000 adds (sum 1..60000 =
-- 1800003000 < 2^31) so the OUTPUT CHECK passes on the 9b89643
-- baseline too: its string.format("%d", n) follows stock 5.1 (C int
-- cast) and clamps n > 2^31 to 2147483647 — its arithmetic is honest
-- (verified: the raw sum value crosses the host wire correct), so the
-- timing comparison measures real compute either way; only the
-- formatted string would differ. With this shape both trees return
-- identical, correct outputs.
local s = 0
for r = 1, 34 do
  s = 0
  for i = 1, 60000 do s = s + i end
end
local f = 0.0
for i = 1, 2000000 do f = f + i * 0.5 end
return string.format("%d %.3f", s, f)
