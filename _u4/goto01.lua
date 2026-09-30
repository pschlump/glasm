-- U4c: goto/label, the 5.4/5.5 rules — duplicates (same/enclusing block,
-- sibling fine), pending-goto resolution (own block only), the
-- jumps-into-scope wording, tbc-aware CLOSE on goto/break, the
-- last-statement label rule.
local function try(src)
  local f, err = load(src, "=t")
  if f == nil then
    print((err:gsub("^t:%d+: ", "")))
  else
    f()
    print("ok")
  end
end
-- duplicates
try("do ::l:: do ::l:: end end")
try([[
do
  ::l::
  do
    local x
    ::l::
  end
end
]])
try("do ::l:: end do ::l:: end")
-- visible label in an ENCLOSING block: legal (forward jump out of the
-- inner block; a backward shape would be an infinite self-loop, which
-- stock also never exits)
try("do do goto out end ::out:: print('hit') end print('jumped')")
-- a goto before the block never matches a label inside it (scope error)
try("goto l; do local a = 1; ::l:: end")
try("do goto l; local a = 1; ::l:: end")
-- forward over the declaration is the scope error; backward after it is legal
try("do local a <close> = nil; goto l; ::l:: end")
try("do goto l; local a <close> = nil; ::l:: end")
try([[
local n = 0
do
  local a <close> = nil
  ::l::
  n = n + 1
  if n < 3 then goto l end
end
print('backwards-ok')
]])
-- gotos resolve only against the finishing block's own labels
try([[
local x = 1
do
  goto cont
end
::cont::
print('hoisted')
]])
-- loop continue shape
local n = 0
for i = 1, 5 do
  if i % 2 == 0 then goto continue end
  n = n + i
  ::continue::
end
print(n)
-- a break out of a tbc scope closes it
local log = {}
do
  local i = 0
  while true do
    i = i + 1
    local c <close> = setmetatable({}, {__close = function() log[#log+1] = "closed" .. i end})
    if i == 2 then break end
  end
end
print(log[1], log[2], #log)
-- a goto escaping a tbc scope closes it exactly once, and the closed
-- cell keeps its final value
local up = "u0"
do
  local function set(v) up = v end
  local c <close> = setmetatable({}, {__close = function() log[#log+1] = "gone/" .. up end})
  set("u1")
  goto out
end
::out::
print(log[3], #log)
-- goto to a label at the function level from inside a loop
local acc = {}
for i = 1, 3 do
  acc[#acc+1] = i
  if i == 3 then goto finish end
end
::finish::
print(#acc, acc[3])
