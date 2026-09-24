-- U2a: mixed int/float comparisons (exact, including beyond 2^53)
local maxint = 9223372036854775807
local minint = -9223372036854775808
print(1 < 1.5, 1.5 < 2, 1 <= 1.0, 1.0 <= 1, 2^53 == 2^53 + 0.0)
-- beyond 2^53 the comparison goes through the exact floor/ceil path
local f53 = 2.0^53
print(maxint > f53, minint < -f53, maxint + 0.0 == maxint)
print(f53 + 2.0 > maxint)          -- float beyond int64 range
print(1/0 > maxint, -1/0 < minint) -- infinities order against integers
print(2^63 == maxint, 2^63 > maxint)
-- equality across subtypes
print(3 == 3.0, 3.0 == 3, 3 ~= 3.5, 2^62 + 1.0 ~= 2^62 + 1)
-- numeric strings do NOT compare as numbers (5.3 rule)
print(pcall(function() return 2 < "15" end))
-- integers still compare within type
print(1 < 2, 2 <= 2, 3 > 2, 5 >= 5, minint < maxint)
