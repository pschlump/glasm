-- U9a/U9a2: main-chunk tailcall returns (ledger row 103). Before U9a the
-- wasm legs returned the closure in R(A-1) instead of the value for a
-- chunk ending `return f(x)`; RESULTS (U9a2) is the event that makes the
-- shape diffable at the corpus level. Every print is also an assert-grade
-- oracle so a broken leg fails loudly even without RESULTS.
print("u9a-tailret")

-- 1. compiled-closure call result (the value the bug shape misplaced)
print("t1", (function(x) return x * 2 end)(21))

-- 2. tailcall returning MULTIPLE values
local function two(x, y) return x + 1, y + 1 end
local r1, r2 = two(1, 2)
print("t2", r1, r2)

-- 3. vararg tailcall: main ends in return g(...) on a VARARG proto
local function passthru(...) return ... end
print("t3", passthru(1, nil, 3))

-- 4. tbc live across the tailcall: __close fires, then results stage
local closed = {}
do
  local guard <close> = setmetatable({}, {__close = function() closed[#closed+1] = "x" end})
  print("t4-close", #closed) -- 0: close runs at scope exit, not yet
end
print("t4", #closed) -- 1

-- 5. TCO chain then main-chunk tailcall (the restage-to-entry copy path)
local function rec(n, acc)
  if n == 0 then return acc end
  return rec(n - 1, acc + n) -- self tailcall
end
print("t5", rec(50, 0))

-- 6. tailcall through a vararg restage into multiple returns
local function packback(...)
  local t = {...}
  return #t, t[1], t[#t]
end
print("t6", packback(10, 20, 30))

-- the chunk returns are the point: a VARARG tailcall expanding to the
-- full result set (multi-result chunk return + the M5c restage path)
local function fin(x) return x .. "!" end
local function spread(...) return ... end
return spread(fin("u9a-tailret-done"), rec(10, 0), #closed)
