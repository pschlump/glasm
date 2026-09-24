local osname = "linux"
if string.find(os.getenv("OS") or "", "Windows") then
  osname = "windows"
end

-- 5.2 os.execute returns true|nil, "exit", code (5.1 returned a number) —
-- accept both shapes
local function execok(cmd)
  local a = os.execute(cmd)
  return a == true or a == 0
end
if osname == "linux" then
  assert(not execok("date -a"))
else
  assert(execok("date /T"))
  assert(not execok("md"))
end

assert(os.getenv("PATH") ~= "")
assert(os.getenv("_____GLUATEST______") == nil)
assert(os.setenv("_____GLUATEST______", "1"))
assert(os.getenv("_____GLUATEST______") == "1")
