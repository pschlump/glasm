
local ok, msg = pcall(function()
  string.dump()
end)
assert(not ok and string.find(msg, "bad argument #1 to 'dump'"))
assert(string.find("","aaa") == nil)
-- U5a closeout re-pin (row 80): stock add_s — a non-digit after '%' in a
-- replacement raises (the old fork copied it through literally)
assert(string.gsub("hello world", "(%w+)", "%1 %1") == "hello hello world world")
local ok, msg = pcall(string.gsub, "hello world", "(%w+)", "%1 %1 %c")
assert(not ok and string.find(msg, "invalid use of '%%' in replacement string"))

local ret1, ret2, ret3, ret4 = string.find("aaa bbb", "(%w+())")
assert(ret1 == 1)
assert(ret2 == 3)
assert(ret3 == "aaa")
assert(ret4 == 4)
