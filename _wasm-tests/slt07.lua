local t = setmetatable({}, {__index = function(_, k) return k * 10 end, __len = function() return 2 end})
print(t[1], #t)
