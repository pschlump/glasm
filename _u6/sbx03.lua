-- sbx03 (U6c, ledger row 94): missing-global reads raise the Redis
-- text through the __index metatable; a sandbox-nil'd global IS an
-- absent key, so reading one bare raises the same text (the Redis
-- sandbox-nil × protect-raise stacking) while rawget still sees nil;
-- present globals read untouched.
local function strip(e) return (e:gsub('^.-:%d+: ', '')) end
print("sbx03", strip(select(2, pcall(function() return nosuch_thing end))))
print("sbx03", strip(select(2, pcall(function() return io end))))
print("sbx03", rawget(_G, "io"), rawget(_G, "debug"))
print("sbx03", _G.string ~= nil, type(pcall))
