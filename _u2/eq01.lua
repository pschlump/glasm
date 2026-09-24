-- U2a: value-model plumbing through the interpreter (float-fallback
-- arithmetic until U2b — every expression here is value-identical under
-- either evaluation, and printing is %.14g-masked cross-engine)
local x = 10
print(x == 10, x + 5 == 15, x * 2 == 20, x - 4 == 6)
print(x / 4 == 2.5, x / 5 == 2, x % 3 == 1, x % 7 == 3)
print(-x == -10, 0 - x == -10)
-- arithmetic on integer operands is value-correct via the float path;
-- (integer WRAP cases — 2^62 + 2^62 == minint — are U2b corpus material:
-- they differ between the float fallback and real integer arithmetic)
local big = 4611686018427387904  -- 2^62
print(big == 2.0^62, big + 0.0 == 2.0^62, big > 0)
-- string coercion in arithmetic keeps values
print("10" + 5 == 15, "10" * "3" == 30, "0x10" + 0 == 16)
-- for loops run (float flavor until U2b)
local n, s = 0, 0
for i = 1, 10 do n = n + 1; s = s + i end
print(n, s)
for i = 0.5, 2.5, 1 do n = n + 1 end
print(n)
for i = 10, 1, -2 do s = s - i end
print(s)
-- concat of integers is exact decimal
print(1 .. 2 .. -3)
print("n=" .. 42)
-- length operator (still float-view at U2a; compares equal)
print(#"hello", #{10, 20, 30})
-- table library basics over integer keys
local t = {10, 20, 30}
print(t[1] + t[2] + t[3], #t)
