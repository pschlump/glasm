-- U5f: string.dump + binary-load round trips (the calls.lua/errors.lua
-- shapes): stripped dumps (?:?: positions, nameless operands), mode/env
-- arguments, reader-fed blobs, mode-error and argcheck texts, and the
-- dump(load(dump(f))) identity.
local function checkerr(msg, f, ...)
  local st, err = pcall(f, ...)
  assert(not st and string.find(err, msg), err)
  print("+")
end

-- strip: an arithmetic error through a stripped dump renders "?:?:"
local f = function (a) return a + 1 end
f = assert(load(string.dump(f, true)))
assert(f(3) == 4)
checkerr("^%?:%?:", f, {})

-- strip: an OP_MOV local keeps no debug info — the nameless table error
local f2 = function () local a; a = {}; return a + 2 end
f2 = assert(load(string.dump(f2, true)))
checkerr("table value", f2)

-- mode 'b' + a custom env
local f3 = load(string.dump(function () return 1 end), nil, "b", {})
assert(type(f3) == "function" and f3() == 1)
print("+")

-- a reader function over the blob (one byte per call)
local function read1(s)
  local i = 1
  return function()
    if i <= #s then local c = s:sub(i, i); i = i + 1; return c end
  end
end
local f4 = string.dump(function ()
  return '01234567890123456789012345678901234567890123456789'
end)
f4 = load(read1(f4))
assert(f4() == '01234567890123456789012345678901234567890123456789')
print("+")

-- mode errors: binary under 't', text under 'b'
local _, msg = load(string.dump(function() end), nil, "t")
assert(msg == "attempt to load a binary chunk (mode is 't')", msg)
local _, msg2 = load("x=1", nil, "b")
assert(msg2 == "attempt to load a text chunk (mode is 'b')", msg2)
print("+")

-- argchecks (stock texts)
local ok, err = pcall(string.dump, print)
assert(not ok and err:find("Lua function expected"), err)
local ok2, err2 = pcall(string.dump, 42)
assert(not ok2 and err2:find("bad argument #1 to 'string.dump' %(Lua function expected%)"), err2)
print("+")

-- round-trip identity: dump(load(dump(f))) loads and behaves
local g = function(...) return select('#', ...), ... end
local h = load(string.dump(g))
assert(h(1, nil, 3) == 3)
local j = load(string.dump(h), "=second", "bt", _G)
assert(j(7, 8) == 2)
print("dump01 ok")
