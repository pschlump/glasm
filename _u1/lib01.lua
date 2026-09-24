-- xpcall(f, msgh, ...) passes arguments through (5.2)
print(xpcall(function(a, b) return a + b, a * b end, function(m) return m end, 3, 4))
print(xpcall(function(a) error("E:" .. a) end, function(m) return "H<" .. m .. ">" end, "z"))
local ok, e = xpcall(function(...) return ... end, print, "a", nil, "c")
print(ok, e)
print(xpcall(function() end, print))
-- pcall unchanged
print(pcall(function(a, b) return a - b end, 10, 4))
