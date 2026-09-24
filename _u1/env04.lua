-- load with an explicit environment (5.2 4th arg)
local e = {k = "envvalue"}
local f = load("return k", "=chunk", "t", e)
print(f())
local g = load("k2 = 1; return k2", "=chunk2", "bt", e)
print(g(), e.k2, rawget(_G, "k2"))
-- default env: the chunk sees the global table
local h = load("return type(print)")
print(h())
-- a loaded chunk's closures capture its _ENV
local e2 = {mk = function(s) return function() return s * 2 end end}
local c = load("return mk(21)()", "=c", "t", e2)
print(c())
-- mode "b" rejects text chunks (byte-exact stock error)
print(select(2, load("x=1", "=m", "b")))
print(select(2, load("x=1", "=m", "tb")))
