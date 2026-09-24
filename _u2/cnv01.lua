-- U2a: tonumber/tostring subtype and string coercion
print(tonumber("10"))           -- integer
print(tonumber("0x10"))         -- 16, integer
print(tonumber("  -0x.41  ") ~= nil)
print(tonumber("10.0") == 10)   -- float, compares equal
print(tonumber("3.") == 3)
print(tonumber("0x1p-2") == 0.25)
print(tonumber("bad"), tonumber(""), tonumber("0x"))
print(tonumber(" 10 "), tonumber("\n9\n"))
print(tonumber("ff", 16))       -- base form: integer
print(tonumber("FF", 16))
print(tonumber("10", 2), tonumber("777", 8), tonumber("zz", 36))
print(tonumber("7f", 8))        -- 'f' invalid in base 8
-- base out of range: an error on both engines; the message wording is a
-- D7 re-pin item, so only the common substring is asserted
local ok, msg = pcall(tonumber, "10", 1)
print(ok, msg:find("base out of range") ~= nil)
