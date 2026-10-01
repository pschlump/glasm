-- U5a: the debug close window — stock removevars sets endpc AFTER the
-- block's OP_CLOSE, so a CLOSE's (and the final RETURN's) own __close
-- dispatch still sees the block's locals through debug.getlocal; the
-- repeat-until repetition close (emitted after the block leaves) stays
-- inside the window too (row 78d).
local res = {}

-- close at a block exit
do
  local x
  local c <close> = setmetatable({}, {__close = function()
    res[#res + 1] = debug.getlocal(2, 1)
    res[#res + 1] = debug.getlocal(2, 2)
  end})
  x = 1
end

-- close at a repeat-until exit AND on repetition
local function foo(a)
  repeat
    local x
    local i <close> = setmetatable({}, {__close = function()
      res[#res + 1] = debug.getlocal(2, 1)
      res[#res + 1] = debug.getlocal(2, 2)
      a = true
    end})
  until a
end
foo(false)

-- close at the function's final RETURN (the k-bit close)
local function bar()
  local y <close> = setmetatable({}, {__close = function()
    res[#res + 1] = debug.getlocal(2, 1)
    res[#res + 1] = debug.getlocal(2, 2)
  end})
  return 42
end
print(bar())

-- close at an error-path return (pcall unwinds through the trampoline)
local function baz()
  local z <close> = setmetatable({}, {__close = function()
    res[#res + 1] = debug.getlocal(2, 1)
  end})
  error("inside baz")
end
pcall(baz)

-- goto out of a scope with a close
do
  do
    local g <close> = setmetatable({}, {__close = function()
      res[#res + 1] = debug.getlocal(2, 1)
    end})
    goto skip
  end
  ::skip::
end

for i = 1, #res do print(i, res[i]) end
print("done")
