-- U3b re-pin: table.maxn/getn are gone (5.2/5.3 removals, D8); the
-- surface asserts move to #/move/create, and insert's arity error is
-- stock's "to 'insert'" wording.
local a = {}
a["key"] = 1
assert(#a == 0)
table.insert(a, 10)
table.insert(a, 2, 20)
table.insert(a, 1, 5)
assert(#a == 3 and a[1] == 5 and a[3] == 20)

local ok, msg = pcall(function()
  table.insert(a)
end)
assert(not ok and string.find(msg, "wrong number of arguments to 'insert'"))

ok, msg = pcall(function()
  table.insert(a, 9, 1)
end)
assert(not ok and string.find(msg, "position out of bounds"))

assert(table.move({1, 2, 3}, 1, 3, 2)[4] == 3)
assert(#table.create(4) == 0)

a = {}
a["key0"] = "0"
a["key1"] = "1"
a[1] = 1
a[2] = 2
a[true] = "true"
a[false] = "false"
for k, v in pairs(a) do
  if k == "key0" then
    assert(v == "0")
  elseif k == "key1" then
    assert(v == "1")
  elseif k == 1 then
    assert(v == 1)
  elseif k == 2 then
    assert(v == 2)
  elseif k == true then
    assert(v == "true")
  elseif k == false then
    assert(v == "false")
  else
    error("unexpected key:" .. tostring(k))
  end
end
