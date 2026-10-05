local function f(...) local t = table.pack(...) return t[1], t.n end
print(f(7, 8))
