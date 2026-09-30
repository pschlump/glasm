-- U4a: <const> attributes — semerrors (position-stripped), assignment
-- locks on locals/upvalues/for-controls/function names.
local function try(src)
  local f, err = load(src, "=t")
  if f == nil then
    print((err:gsub("^t:%d+: ", "")))
  else
    print("compiled")
  end
end
try("local a <const> = 1")
try("local a <bogus> = 1")
try("local <close> a, b = nil, nil")
try("global <close> a")
try("local a <close> = nil <const>")
try("local a <const> <const> = 1")
try("local a <const>, b = 1, 2")
try("for i <const> = 1, 3 do end")

-- assignment battery (compile-time locks)
try("local a <const> = 1; a = 2")
try("local a <const> = 1; local function f() a = 2 end")
try("for i = 1, 3 do i = 10 end")
try("for k, v <const> in pairs({}) do v = 1 end")
try("local t = {}; function t.f <const>() end")
try("local a <close> = nil; a = 1")

-- const semantics: reads, upvalue capture, shadowing
local a <const> = 40
local b <const> = a + 2
print(a, b)
local function get() return a end
print(get())
do
  local a = 7
  print(a)
end
print(a)
-- for-controls are readable
local sum = 0
for i = 1, 4 do sum = sum + i end
print(sum)
-- _ENV as an explicit const upvalue still reads globals
local _ENV <const> = _G
print(type(print))
