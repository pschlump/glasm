-- U5b: captures force the table model (luaK_vapar2local /
-- needvatab): a closure capturing '...t' sees the table (and edits
-- through it), '..._ENV' routes global reads through the promoted
-- vararg cell, nested capture chains, and the capture happens at the
-- owning level only (a nested function's own vararg is independent).
local function cap(a, ...t)
  local function get() return t.n, t[1] end
  local function setn(v) t.n = v end
  print(get())
  setn(1)
  print(select('#', ...), (...))
end
cap(0, "x", "y")

-- the closure and the frame share ONE table
local shared
local function cap2(...t)
  local function edit() t[1] = "edited" end
  edit()
  shared = t
  return (...)
end
print(cap2("orig"))
print(shared[1], shared.n)

-- '..._ENV': the promoted vararg cell IS the function's environment
-- table — global reads index it, index writes mutate it, and '...'
-- honors the edits; assigning the NAME itself is the const error
-- (var06 pins the error battery)
do
  local function f(..._ENV)
    print(novar)                  -- global read -> nil (table has no field)
    _ENV["n"] = 5                 -- index write on the vararg table
    print(select('#', ...))       -- '...' honors the edit: 5 (nil-padded)
    return _ENV[1]                -- direct table read: the first extra
  end
  print(f("first"))
end

-- nested capture through two levels
local function lvl1(...t)
  local function lvl2()
    local function lvl3() return t[1] + t.n end
    return lvl3()
  end
  return lvl2()
end
print(lvl1(10, 20))

-- capture at the owning level only: lvlA's own '...' is independent of
-- lvlB's
local function lvlA(...)
  local function lvlB(...)
    local more = {...}
    return select('#', ...), #more
  end
  return lvlB(1), select('#', ...)
end
print(lvlA("a", "b"))

-- unnamed '...' cannot be captured, but '{...}' can (the table is an
-- ordinary value)
local function unnamed(...)
  local more = {...}
  local function cnt() return #more end
  return cnt(), select('#', ...)
end
print(unnamed(1, 2, 3))
print("done")
