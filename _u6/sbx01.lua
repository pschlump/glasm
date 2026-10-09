-- sbx01 (U6c, ledger row 94): the 5.5 sandbox surface — exactly the
-- nil'd list, the kept library tables still present.
local bad = 0
for _, g in ipairs{"io","os","package","require","dofile","loadfile","load","debug","warn"} do
  -- rawget: under Protect a nil'd global is an ABSENT key, and a bare
  -- _G[g] read would raise through the __index metatable instead of
  -- comparing nil (Redis stacks sandbox-nil on top of protect-raise).
  if rawget(_G, g) ~= nil then bad = bad + 1 end
end
print("sbx01", bad, type(string), type(utf8))
