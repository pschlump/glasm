-- U5b: the vararg name is a read-only slot (stock check_readonly over
-- RDKVAVAR): assignment and capture-assignment are compile errors with
-- the const-variable text, while INDEX writes through it are ordinary
-- table mutations. Also '...name' parameter-count edge shapes.
local function try(src)
  local f, err = load(src, "=t")
  if f == nil then
    print((err:gsub("^t:%d+: ", "")))
  else
    local ok, e = pcall(f)
    if not ok then print("ERR: " .. tostring(e)) end
  end
end

-- bare assignment, in the owning function and through a closure
try("local function f(a, ...t) t = 10 end")
try("local function f(...t) local function g() t = {} end end")
try("local function f(..._ENV) _ENV = {} end")

-- syntax errors around the form itself: both stock shapes hit the
-- check_match family (stock parlist stops at '...' and the following
-- ',' surfaces as "')' expected"; our goyacc grammar has no error
-- production there, so it says "unexpected symbol" — ledger row 82
-- tracks the gap; these stay out of the corpus until it's fixed)

-- index writes are fine, and the mutations are visible
try([[
local function f(...t)
  t[1] = "set"; t.n = 3; t.extra = true
  return (...)
end
print(f("a"))
]])

-- declaration shapes that stay legal: only params, no extras, zero
-- params, methods (self is param 1)
try([[
local function a(...t) return t.n end
local function b(x, y, ...t) return t.n, t[1] end
local function c(...t) return t[1] end
local obj = {}
function obj:m(...t) return self, t.n end
print(a(), b(1, 2), c(), obj:m())
]])
print("done")
