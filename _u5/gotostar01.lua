-- U5a: goto and the collective '*' scope — a `global *` entry counts as
-- a scoping declaration, so a goto jumping over one into its scope is
-- the "'*'" wording (goto.lua:30); plain labels/gotos around global
-- declarations keep the 5.4 rules.
local function try(src)
  local f, err = load(src, "=t")
  if f == nil then
    print((err:gsub("^t:%d+: ", "")))
  else
    f()
    print("ok")
  end
end

try([[ goto l2; global *; ::l1:: ::l2:: print(3) ]])
try([[ goto l1; global *; ::l1:: ::l2:: print(3) ]])
try([[ goto l2; global none; ::l1:: ::l2:: print(3) ]])
try([[ goto l1; local aa ::l1:: ::l2:: print(3) ]])

-- a goto INSIDE the '*' scope to a label after it leaves fine
try([[ global *; do goto l3 end; ::l3:: print(1) ]])

-- forward jump over a named global declaration is a plain scope error
try([[ goto l; global X; ::l:: ]])

-- backward goto to a label at function level after a '*' block
try([[
  ::top::
  global *
  W = (W or 0) + 1
  if W < 3 then goto top end
  print(W)
]])

-- break out of a loop whose body declared a global (close levels off
-- the register ladder — the '*' entry has no register)
try([[
  local out = 0
  for i = 1, 3 do
    global *
    G = i
    out = out + G
  end
  print(out, G)
]])
print("done")
