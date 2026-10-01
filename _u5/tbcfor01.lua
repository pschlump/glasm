-- U5a: generic-for 4th-value TBC — the closing variable registers at
-- TFORPREP (swap + newtbcupval), closes on normal exit and break, and
-- EVERY generic-for carries the marktobeclosed block flags (tailcall
-- suppression, RETURN k bit — the numopen battery, locals.lua:1202).
local numopen = 0
local function open(n)
  numopen = numopen + 1
  return setmetatable({}, {__close = function(_, e)
    numopen = numopen - 1
    print("close", n, e == nil)
  end})
end

-- next with a closing 4th value
do
  local t = {10, 20, 30}
  for i, v in next, t, nil, open("fourth") do
    print("iter", i, v)
  end
  print("after", numopen)
end

-- break closes it too
do
  for i, v in next, {1, 2, 3}, nil, open("brk") do
    if i == 2 then break end
  end
  print("after break", numopen)
end

-- a goto out of the loop closes it
do
  for i, v in next, {1, 2, 3}, nil, open("goto") do
    if i == 2 then goto out end
  end
  ::out::
  print("after goto", numopen)
end

-- error inside the body closes it (pcall boundary)
do
  local ok, e = pcall(function()
    for i, v in next, {1}, nil, open("err") do
      error("boom")
    end
  end)
  print(ok, e, numopen)
end

-- nil and false 4th values are exempt
for i, v in next, {1}, nil, nil do print("nil4th", i) end
for i, v in next, {1}, nil, false do print("false4th", i) end

-- the return value can BE the 4th explist slot
do
  local function iter4()
    local i = 0
    return function() i = i + 1; if i <= 2 then return i, i * 10 end end,
           nil, nil, open("ret")
  end
  for i, v in iter4() do print("r", i, v) end
  print("after ret", numopen)
end

-- no 4th value: still marktobeclosed-blocked but nothing registered
for i, v in next, {7} do print("plain", i, v) end

-- error rotation: a bad __close after a body error surfaces the close error
do
  local ok, e = pcall(function()
    local bad <close> = setmetatable({}, {__close = function() error("in close") end})
    for i, v in next, {1}, nil, open("rot") do
      error("first")
    end
  end)
  print("rotated:", e)
end
print("done", numopen)
