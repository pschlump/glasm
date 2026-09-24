-- U2a: table key normalization — t[2.0] and t[2] are the same slot
local t = {}
t[2] = "int"
print(t[2.0])            -- float key with exact int value normalizes
t[3.0] = "float"
print(t[3])
print(t[2^31] ~= nil)    -- 2^31 is a float, exactly integral
local n = 0
for k, v in pairs(t) do n = n + 1 end
print(n)                 -- exactly 3 slots
-- keys survive pairs as integers (printed exactly; masked cross-engine)
for k, v in pairs(t) do print(k, v) end
t[2] = nil
print(t[2.0])            -- deletion through the other spelling
-- array part vs hash: integral keys up to 2^53 normalize
local a = {}
a[1.0] = "one"
print(a[1], #a)
a[2.5] = "x"
print(a[2.5], a[2])
-- string keys never coerce
a["1"] = "str"
print(a["1"], a[1])
-- huge integer keys (beyond MaxArrayIndex) live in the hash part
a[9223372036854775807] = "max"
print(a[9223372036854775807], a[2^63 - 1024.0])
