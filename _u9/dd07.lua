print("dd07")
-- multret results off the direct arm: into a C call's argument list,
-- through select/#/table.pack, paren-truncated, and finally out of the
-- MAIN chunk itself (the last statement's results ride rt_dd_ret at
-- cl=0 into the RESULTS event -- U9a2's differential surface).
local function three() return 7, 8, 9 end
local function wrap()
  return string.format("%d-%d-%d", three())
end
print(wrap())
print(select('#', three()), select(3, three()))
local p = table.pack(three())
print(p.n, p[1], p[2], p[3])
local function tail(...) return ... end
print((tail(three())))
print(select('#', tail(three())))
local function pick(n)
  if n == 1 then return three() end
  return tail(three())
end
print(pick(1))
return select('#', three()), three()
