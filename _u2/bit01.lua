-- U2b: bitwise operators, shifts, and integer conversions
print(3 & 5, 3 | 5, 3 ~ 5, ~0)
print(0xF0 | 0xCC ~ 0xAA & 0xFD)   -- precedence: | lowest, ~, & highest
a, b = 0xF0, 0xCC
print(a | b, a & b, a ~ b)
-- variables defeat constant folding (values still exact)
x, y = 3, 5
print(x & y, x | y, x ~ y)
-- wrap and identities
print(0xFFFFFFFFFFFFFFFF == -1)
print(-1 & 0xFFFFFFFF, -1 | 0, -1 ~ -1)
print(~~x, ~x == -1 - x)
-- shifts: logical right, negative counts shift the other way, OOB -> 0
print(1 << 3, 8 >> 2, 1 << 63, 1 << 64, 1 << 65)
print(-1 >> 1, -1 >> 63, -1 >> 64)
print(1 << -1, 1 << -63, 1 << -64)  -- negative left = right shift
print(256 >> -2, 2 >> -62, 2 >> -63)
print(-1 >> 9223372036854775807, -1 >> -9223372036854775808)
-- shifts with variable counts
n = 4
print(0xF0 >> n, 1 << n, 0x100 << n)
-- integral floats convert; fractional and out-of-range error
print(3.0 | 1, 2.0 ~ 5, 8.0 >> 1, 16.0 << 1)
print(pcall(function() return 2.5 & 1 end))
print(pcall(function() return 1e100 | 0 end))
-- numeric strings do NOT coerce in bitwise ops (stock stringmetamethods
-- carry no bitwise entries); the message suffix (varinfo) differs on the
-- interp until the D7 varinfo work — assert the stable prefix
print((select(2, pcall(function() return "3" & 5 end))):find("attempt to perform bitwise operation on a string value") ~= nil)
print((select(2, pcall(function() return ~"3" end))):find("attempt to perform bitwise operation on a string value") ~= nil)
-- print in binary-ish checks
print((1 << 8) - 1 == 0xFF, ~0xFF == -256)
-- shift by exactly 63
print(1 << 63 == 0x8000000000000000, 0x8000000000000000 >> 63)
print(0x8000000000000000 >> 1, 0x8000000000000000 >> 62)
-- constant folding matches runtime (same value either way)
print(1 << 4, (function() local s = 4 return 1 << s end)())
print(-2 // 1, 0x8000000000000000 // -1 == 0x8000000000000000)
print(0x8000000000000000 % -1)
