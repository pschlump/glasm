local utf8 = require'utf8'
local ok, e = pcall(utf8.len, "a\xFFb")
print(ok, e)
