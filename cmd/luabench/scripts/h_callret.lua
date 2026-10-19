-- h_callret: non-recursive call churn — 300k calls of a two-integer
-- leaf function. Isolates the per-call seam (dispatch + argument move
-- + result move) from recursion depth and from fib's register
-- pressure, so the delta vs h_tco/h_fib attributes cost to the seam
-- itself. 45000150000 fits i64 so both legs format identically.
local function add(a, b)
  return a + b
end
local s = 0
for i = 1, 300000 do
  s = add(s, i)
end
return string.format("%d", s)
