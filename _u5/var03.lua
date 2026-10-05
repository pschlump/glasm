-- U5b: the vararg table's 'n' validation (stock getnumargs): only an
-- INTEGER in [0, INT_MAX/2] passes; negatives, non-integral floats,
-- integral-but-FLOAT-tagged values (1.0 — and 2^30-1/2^30 written with
-- '^', which always yields float), strings, booleans, nil all raise
-- "vararg table has no proper 'n'"; true integers past the boundary
-- (1<<30, maxinteger) fail the RANGE half. Raised at the next '...'
-- use, not at the assignment.
local function mk(n)
  return function(a, b, ...t)
    t.n = n
    return select('#', ...)
  end
end

local function run(n)
  local ok, r = pcall(mk(n), 1, 2, 3, 4)
  if ok then print("ok", r) else print("err", (r:gsub("^.*:%d+: ", ""))) end
end

run(-1)        -- negative integer
run(1.5)       -- non-integral float
run("2")       -- string
run(true)
run(nil)
run(1.0)       -- numerically integral but float-tagged: the TAG check
run(2^30 - 1)  -- '^' is float too — tag check again, not range
run(2^30)      -- same
run(1 << 30)   -- true integer > INT_MAX/2: the RANGE check
run(math.maxinteger)

-- a valid count answers as an INTEGER (stock's select('#') pushes an
-- integer — pinned after the LNumber mis-fix, row 81). The passing
-- boundary itself ((1<<30)-1, a true integer) is NOT probed: it
-- validates and then exhausts value staging with capacity texts the
-- engines own ("registry overflow" vs "stack overflow" — row 81's
-- capacity clause, excluded by corpus discipline).
local c = mk(3)(1, 2, "a", "b", "c")
print(c, math.type(c))
print("almost done")

-- 't.n = nil' (a deleted n) fails the same way at the next '...' use —
-- and the raise is IN THE OWNING FRAME ('...' is staged where the call
-- is written, before pcall could exist), so no pcall wrap can catch
-- it. Stock dies the same way on the same line; that is why this is
-- the chunk's last statement.
local function deln(a, ...t)
  t.n = nil
  print(select('#', ...))
end
deln(0, "a", "b", "c")
