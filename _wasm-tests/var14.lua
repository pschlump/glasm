local function f(...) return select('#', ...) end
print(f('p', 'q'))
