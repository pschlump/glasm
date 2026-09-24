-- _ENV basics (D3): _ENV is the environment; globals are its fields
print(_ENV == _G)
x = 42
print(x, _G.x, _ENV.x)
_ENV.y = 7
print(y, _ENV.y)
y = y + 1
print(_G.y)
function f() return x + y end
;  -- empty statement (5.2): legal anywhere a statement goes
print(f())
local function g() z = 99; return z end
print(g(), _G.z)
-- globals through one more nesting level
local function outer()
  local function inner() w = 5; return w * 2 end
  return inner()
end
print(outer(), _G.w)
