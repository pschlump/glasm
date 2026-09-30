-- U3d: table lib stock surface.
local t = {3, 1, 2}
table.sort(t)
print(table.concat(t, ","))
table.insert(t, 10)
table.insert(t, 2, 20)
print(table.concat(t, ","))
print(table.remove(t, 1), table.concat(t, ","))
local u = table.move(t, 1, 3, 2)
print(u == t, u[4])
print(table.concat(table.move({1,2,3}, 1, 3, 2), ","))
local v = table.create(2, 1)
v[1] = 9
print(v[1], #v)
print(table.unpack({1, 2, 3}), select('#', table.unpack({1, 2, 3}, 2)))
local p = table.pack(1, nil, 3)
print(p.n, p[3])
local ok, e = pcall(table.insert, t)
print(ok, e:find("wrong number of arguments to 'insert'") ~= nil)
local ok, e = pcall(table.insert, t, 99, 1)
print(ok, e:find("position out of bounds") ~= nil)
local ok, e = pcall(table.concat, {1, {}})
print(ok, e:find("invalid value %(table%) at index 1 in table for 'concat'") ~= nil)
-- metamethod-honoring access
local proxy = setmetatable({}, {
  __index = function(_, k) return k * 2 end,
  __newindex = function(t, k, v) rawset(t, k, v * 10) end,
  __len = function() return 3 end,
})
print(table.concat(proxy, ",", 1, 3))
table.insert(proxy, 5)
print(rawget(proxy, 4))
print(table.getn, table.maxn)
