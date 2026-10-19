-- h_nsieve: sieve of Eratosthenes over a table-as-bitarray, rounds
-- 2^16..2^18 (benchmark-game nsieve shape). Table stores + integer
-- arithmetic with negligible call traffic — the lane shape that
-- exposed the wazero codegen gap on rt table code (note/u7-perf.md
-- top U7+ item). Sizes are 1 << n so every index is an integer
-- (i64-exact on both legs); one concatenated string comes back
-- (scalars only on the wire).
local N = 16
local function sieve(n)
  local size = 1 << n
  local t = {}
  local count = 0
  for i = 0, size - 1 do
    t[i] = true
  end
  for i = 2, size - 1 do
    if t[i] then
      count = count + 1
      for j = i * i, size - 1, i do
        t[j] = false
      end
    end
  end
  return count
end
local r = {}
for n = N, N + 2 do
  r[#r + 1] = string.format("2^%d %d", n, sieve(n))
end
return table.concat(r, "\n")
