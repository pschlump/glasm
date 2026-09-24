-- break leaving local scopes with open upvalues (OP_CLOSE correctness)
for i = 1, 3 do
  do
    local t = {i}
    local f = function() return t[1] end
    if i == 1 then break end
    print(f())
  end
end
print("ok1")
local fs = {}
for i = 1, 4 do
  local x = i * 10
  fs[#fs+1] = function() return x end
  if i == 2 then break end
end
for _, f in ipairs(fs) do print(f()) end
-- break out of while with captured local
local k = 0
while true do
  k = k + 1
  local v = k * 2
  local g = function() return v end
  if k == 2 then print(g()) break end
end
print(k)
