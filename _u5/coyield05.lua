-- U5d: yieldable VM-dispatched metamethods (kind-6 continuation
-- frames, row 84 — stock luaV_finishOp): every metamethod opcode
-- family dispatching a Lua metamethod that yields suspends the owner's
-- instruction; the resume value becomes the metamethod result and the
-- instruction completes exactly as stock's arms.
--
-- EXCLUDED (row 84's divergence): a C-FUNCTION metamethod that yields,
-- dispatched from Lua code —
--   local a = setmetatable({}, {__eq = coroutine.yield})
--   local b = setmetatable({}, {__eq = coroutine.yield})
--   local co = coroutine.create(function()
--     print("eq result:", a == b)   -- __eq(yield) dispatched from Lua code
--   end)
--   print(coroutine.resume(co))       -- stock: true <table> <table>
--                                     -- fork:  false "attempt to yield across a
--                                     --        C-call boundary" (thread dies)
--   print(coroutine.resume(co,"yes")) -- stock: eq result: true / true done
--                                     -- fork:  false "cannot resume dead coroutine"
-- and the __index flavor: setmetatable({}, {__index = coroutine.yield})
-- read from Lua code (stock: true <table> k, then idx: V; fork: the
-- same boundary error + dead thread). Stock's luaT_callTM discriminates
-- on the CALLER being Lua code, not the callee's type (the resume
-- completes the instruction through luaV_finishOp); the fork's callMM G
-- arm is noyield — a C frame has no continuation to suspend through.
-- Keeping these shapes would pin the wrong behavior, so they stay out.

-- U5d kind-6: a VM-dispatched Lua metamethod that yields — the owner's
-- interrupted instruction completes when it returns (vmFinishMM).
-- Every metamethod body yields (nil, "ev") first; val() answers the
-- resume value. Checks the completed instruction's RESULT.
local function mmbody(kind)
  return function(a, b)
    local x = coroutine.yield(nil, kind)
    return val(x, a, b)
  end
end
val = function(x) return x end

local out = {}
local function run(f, t, xval)
  local co = coroutine.wrap(f)
  local r1, r2 = co(t)
  while r2 ~= nil do
    r1, r2 = co(xval)
  end
  return r1
end

-- __index family: GETFIELD / GETTABLE / GETI / SELF / GETTABUP(upvalue)
do
  local mt = {__index = function(t, k)
    local x = coroutine.yield(nil, "index")
    if k == "baz" then
      return function(self) return self == t end
    end
    return tostring(k) .. "-x"
  end}
  local t = setmetatable({}, mt)
  out[1] = run(function(tt) return tt.foo end, t)          -- GETFIELD
  out[2] = run(function(tt) return tt[3] end, t)           -- GETI
  out[3] = run(function(tt) return tt["bar"] end, t)       -- GETTABLE
  out[4] = run(function(tt) return tt:baz() end, t)        -- SELF: ra+1=t set pre-dispatch
end

-- __newindex family: SETFIELD / SETI / SETTABLE
do
  local store = {}
  local mt = {__newindex = function(t, k, v)
    local x = coroutine.yield(nil, "newindex")
    store[k] = x
  end}
  local t = setmetatable({}, mt)
  run(function(tt) tt.a = 1 end, t)
  out[6] = store.a
  run(function(tt) tt[2] = 1 end, t)
  out[7] = store[2]
  run(function(tt) tt["b"] = 1 end, t)
  out[8] = store.b
end

-- arith: __add (MMBIN), __add with immediate (MMBINI), __mul const (MMBINK)
do
  local mt = {__add = function(a, b)
    local x = coroutine.yield(nil, "add")
    return x + 100
  end, __mul = function(a, b)
    local x = coroutine.yield(nil, "mul")
    return x * 100
  end}
  local t = setmetatable({}, mt)
  out[9] = run(function(tt) return tt + 1 end, t, 42)
  out[10] = run(function(tt) return 5 + tt end, t, 42)
  out[11] = run(function(tt) return tt * 3 end, t, 42)
  out[12] = run(function(tt) return 2 * tt end, t, 42)
end

-- unary: __unm / __bnot / __len
do
  local mt = {}
  function mt:__unm() local x = coroutine.yield(nil, "unm") return -x end
  function mt:__bnot() local x = coroutine.yield(nil, "bnot") return ~x end
  function mt:__len() local x = coroutine.yield(nil, "len") return x + 1 end
  local t = setmetatable({}, mt)
  out[13] = run(function(tt) return -tt end, t, 42)
  out[14] = run(function(tt) return ~tt end, t, 42)
  out[15] = run(function(tt) return #tt end, t, 42)
end

-- __concat with the fold resume: lhs string, mm in the middle, rhs string
-- "s" .. mm .. "t" — the fold must resume and prepend/append the rest
do
  local mt = {__concat = function(a, b)
    local x = coroutine.yield(nil, "concat")
    return "<" .. x .. ">"
  end}
  local t = setmetatable({}, mt)
  out[16] = run(function(tt) return "L-" .. tt end, t, 42)
  out[17] = run(function(tt) return tt .. "-R" end, t, 42)
  out[18] = run(function(tt) return "a" .. "b" .. tt .. "c" .. "d" end, t, 42)
end

-- order: __lt / __le (register form + immediate form GTI/GEI/LTI/LEI)
do
  local mt = {__lt = function(a, b)
    local x = coroutine.yield(nil, "lt")
    return x == "yes"
  end, __le = function(a, b)
    local x = coroutine.yield(nil, "le")
    return x == "yes"
  end}
  local t = setmetatable({}, mt)
  out[19] = run(function(tt) return tt < 10 end, t, "yes") and "ltT" or "ltF"
  out[20] = run(function(tt) return 10 < tt end, t, "yes") and "ltT" or "ltF"
  out[21] = run(function(tt) return tt <= 10 end, t, "yes") and "leT" or "leF"
  out[22] = run(function(tt) return tt > 10 end, t, "yes") and "gtT" or "gtF"   -- __lt(10, tt)
  out[23] = run(function(tt) return tt >= 10 end, t, "yes") and "geT" or "geF"  -- __le(tt, 10)
end

-- __eq
do
  local mt = {__eq = function(a, b)
    local x = coroutine.yield(nil, "eq")
    return x == "yes"
  end}
  local a = setmetatable({}, mt)
  local b = setmetatable({}, mt)
  out[24] = run(function() return a == b end, nil, "yes") and "eqT" or "eqF"
  out[25] = run(function() return a ~= b end, nil, "yes") and "eqT" or "eqF"
end

-- __eq with the result INSIDE a comparison chain (OP_EQ answers a jump)
do
  local mt = {__eq = function(a, b)
    local x = coroutine.yield(nil, "eq")
    return x == "yes"
  end}
  local a = setmetatable({}, mt)
  local b = setmetatable({}, mt)
  if run(function() if a == b then return "T" end return "F" end, nil, "yes") ~= "T" then
    out[24] = "BROKEN"
  end
end

-- GETTABUP: __index on _ENV itself (an undefined global read through
-- the frame's _ENV upvalue) — last so the _G metatable disturbs nothing
do
  setmetatable(_G, {__index = function(t, k)
    local x = coroutine.yield(nil, "gtu")
    return "MISSING-" .. tostring(k)
  end})
  out[26] = run(function() return someundefinedglobal end, nil)
  setmetatable(_G, nil)
end

for i = 1, 26 do
  print(i, out[i])
end
