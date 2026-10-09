-- sbx04 (U6c, ledger row 94): the lockdown is the globals surface only
-- script-built tables stay writable, and a swapped _ENV (a private
-- table) is not protected — Redis semantics where scripts may build
-- their own environments.
local t = {}
t[1] = 1
rawset(t, 2, 2)
t.x = 3
local function priv()
  local _ENV = {}
  x = 1
  return x
end
print("sbx04", #t, t.x, priv())
