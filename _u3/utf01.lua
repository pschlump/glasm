-- U3e: utf8 library.
local utf8 = require'utf8'
print(utf8.len("héllo"), utf8.len("\xE1\x80\x80"))
print(utf8.char(0x41, 0xE9, 0x10FFFF))
print(utf8.codepoint("é"))
print(utf8.codepoint("éé", 1, 3))
print(utf8.offset("héllo", 3))
print(utf8.offset("héllo", -1))
print(select('#', utf8.offset("héllo", 2)))
local n = 0
for p, c in utf8.codes("héllo") do n = n + 1 end
print(n)
local ok, e = pcall(utf8.codepoint, "\xFF")
print(ok, e:find("invalid UTF%-8 code") ~= nil)
print(utf8.charpattern == "[\0-\127\194-\253][\128-\191]*")
print(utf8.len("\xE0\x80\x80", 1, -1, true))
