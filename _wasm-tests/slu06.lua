local utf8 = require'utf8'
local ok, e = pcall(utf8.codepoint, "\xFF")
print(ok, e:find("invalid UTF%-8 code") ~= nil)
