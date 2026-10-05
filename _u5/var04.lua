-- U5b: shadowing and the death of the implicit 'arg' table. A local
-- named 'arg' is a plain local; without one, 'arg' resolves as a
-- global (nil unless set); _G.arg is nil; vararg params shadow and
-- restore like any local; nested functions with their own '...' hide
-- the outer one.
local function try(src)
  local f, err = load(src, "=t")
  if f == nil then
    print((err:gsub("^t:%d+: ", "")))
  else
    local ok, e = pcall(f)
    if not ok then print("ERR: " .. tostring(e)) end
  end
end

-- no implicit arg table anywhere (the harness seeds a host-surface
-- global arg = {[0]=chunkname} in every engine; clear it so the
-- language surface is the real one)
arg = nil
print(arg, _G.arg, type(_G.arg))

-- the official suite's idiom: 'local arg = {...}' is a fresh local
local function f(...)
  local arg = {...}
  return #arg, arg.n, arg[1]
end
print(f(7, 8, 9))
print(arg)

-- a user-set global named arg is visible like any global
arg = {n = 2, [0] = "prog"}
local function g(...)
  print(arg.n, arg[0], select('#', ...))
end
g(1)
arg = nil

-- the vararg parameter itself shadows/extends: '...t' then inner scope
local function shadow(a, ...t)
  do
    local t = {"plain local"}
    print(t[1], t.n)
  end
  print(t.n, t[1])
  do
    local a = 99
    print(a)
  end
  print(a)
end
shadow(1, 2, 3)

-- nested '...' hides the outer vararg entirely
local function outer(...)
  local function inner(...)
    return select('#', ...), ...
  end
  return select('#', ...), inner(1, 2)
end
print(outer("a", "b", "c"))

-- 'local t = 1' AFTER '...t' is a shadow of the const vararg name:
-- allowed (a fresh local), while a bare assignment is the const error
-- (var06 pins the error battery)
local function rebind(...t)
  print(t.n)
  local t = {"shadowed"}
  print(t[1], t.n)
end
rebind(1, 2)
print("done")
