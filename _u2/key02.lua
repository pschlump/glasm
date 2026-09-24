-- U2a: next() with explicit numeric keys (normalization at the Next
-- boundary) and iteration order stability
local t = {}
t[1] = "a"; t[2] = "b"; t[3] = "c"
print(next(t))                 -- array part first
print(next(t, 1))
print(next(t, 2))
-- the float spelling of an integer slot is an invalid next key in stock
-- 5.5 (keyinarray is integer-only, getgeneric does not normalize)
print(pcall(next, t, 2.0))
-- a string key that is not in the table is invalid too
print(pcall(next, t, "1"))
local k = next(t, 3)
print(k)                       -- end of array: hash part or nil
-- hash-part keys: iteration ORDER is engine-specific (interp keeps
-- insertion order, stock walks hash slots), so assert the SET, not the
-- sequence
local h = {}
h[9223372036854775807] = "big"
h[1.5] = "frac"
h[0] = "zero"
local n = 0
for k, v in pairs(h) do n = n + 1; assert(h[k] == v) end
print(n, h[9223372036854775807], h[1.5], h[0])
-- (next with an explicit HASH-part key continues in engine-specific slot
-- order — pairs covers the set semantics above; only the invalid-key
-- shape is asserted here)
print(pcall(next, h, 2.0))
-- pairs key round trip: the key pairs yields feeds straight back
for k, v in pairs(t) do
  print(k, t[k])
end
-- ipairs over the array part
for i, v in ipairs(t) do print(i, v) end
