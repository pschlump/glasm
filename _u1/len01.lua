-- # honors __len on tables (5.2)
local t = setmetatable({}, {__len = function(x) return 42 end})
print(#t)
local u = setmetatable({1,2,3}, {__len = function() return 10 end})
print(#u, u[1])
local obj = setmetatable({}, {__len = function() return "str" end})
print(#obj == "str", type(#obj))
-- recursion through __len metatable chain
local base = setmetatable({}, {__len = function() return 7 end})
print(#base)
-- plain tables unaffected
print(#{10,20,30}, #{"a"}, #{})
