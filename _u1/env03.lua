-- _ENV reassignment: closures share the upvalue cell
local printf = print
_ENV = {w = 10}
printf(w)
local function late() return w + 1 end
printf(late())
local function early() return w end
_ENV = {w = 20}
printf(early(), late())
local function make() return function() return w end end
local c1 = make()
_ENV = {w = 30}
printf(c1())
-- setting _ENV inside a function is local to that function's _ENV binding
local orig = _ENV
local function swap()
  _ENV = {inner = 1}
  return inner
end
printf(swap(), orig.w)
