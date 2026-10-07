-- U5f: load's env/upvalue model (stock f_parser's luaF_initupvals +
-- lua_load's upvalue-#1 seeding + load_aux's explicit-env overwrite):
-- every upvalue of a fresh main is a closed nil cell; upvalue #1 gets
-- the global table when the env argument is ABSENT and the explicit
-- value — even nil — when present. A dumped non-main loaded as a chunk
-- keeps its own upvalue order (calls.lua:418: x is #1, _ENV #2).
-- Plus debug.upvaluejoin's cell sharing and the reader-error shapes
-- (calls.lua:412: an erroring reader becomes load's own (nil, object)).
local n, v = debug.getupvalue(load("return 1"), 1)
print(n, v == _G)
local f = load("return _ENV")
print(f() == _G)
local g = load("x = 7; return x")
print(g(), _G.x)
_G.x = nil

-- a dumped closure whose _ENV is NOT upvalue #1
local xx
local function h ()
  local y = xx   -- xx becomes upvalue #1
  return XX      -- _ENV is upvalue #2
end
local d = string.dump(h)
local t = load(d, "", "b")
local n1, v1 = debug.getupvalue(t, 1)
local n2, v2 = debug.getupvalue(t, 2)
print(n1, n2, v1 == _G, v2)   -- xx, _ENV, true (globals seeded), nil
XX = 123
debug.setupvalue(t, 2, _G)
print(t())                    -- 123 through the swapped _ENV
XX = nil

-- an explicit env overwrites upvalue #1 — which is xx, not _ENV
local envt = {dummy = true}
local t2 = load(d, "", "b", envt)
local a, b = debug.getupvalue(t2, 1)
print(a, b == envt)
print(select(2, debug.getupvalue(t2, 2)))   -- _ENV cell stays nil

-- an EXPLICIT nil env seeds nil into upvalue #1 (calls.lua:460)
local aa, bb = 20, 30
local q = load(string.dump(function (x)
  if x == "set" then aa = 10+bb; bb = bb+1 else
  return aa
  end
end), "", "b", nil)
print(q())                                  -- nil
assert(debug.setupvalue(q, 1, "hi") == "aa")
print(q())                                  -- hi
assert(debug.setupvalue(q, 2, 13) == "bb")
print(debug.setupvalue(q, 3, 10))           -- nil: only 2 upvalues
q("set")
print(q())                                  -- 23
q("set")
print(q())                                  -- 24

-- upvaluejoin: shared-cell aliasing across loaded closures
local shared = 1
local getter = function() return shared end
local setter = function(v2) shared = v2 end
local g2 = load(string.dump(getter))
local s2 = load(string.dump(setter))
print(g2())                       -- nil: a fresh cell, not shared's
debug.upvaluejoin(g2, 1, s2, 1)
s2(42)
print(g2())                       -- 42 through the joined cell
-- joining onto an OPEN cell (the owner's register, calls.lua:489 shape)
local a2 = 10
local probe = function() return a2 end   -- captures a2: its only upvalue
local h2 = function() return a2 end      -- same register, same open cell
local g3 = load(string.dump(probe))
print(g3())                       -- nil: a fresh closed cell
debug.upvaluejoin(g3, 1, h2, 1)
print(g3())                       -- 10 through h2's open cell
a2 = 99
print(g3())                       -- 99: the alias tracks the register
-- out-of-range and builtin argchecks
print(pcall(debug.upvaluejoin, g3, 2, h2, 1))
print(pcall(debug.upvaluejoin, g3, 1, print, 1))

-- reader errors become load's own (nil, object) return
local st, msg = load(function() error("hhi") end)
print(st, string.find(msg, "hhi") ~= nil)
local st2, msg2 = load(function() return {} end)
print(st2, msg2)
print("load01 ok")
