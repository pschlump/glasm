-- sbx02 (U6c, ledger row 94): protected table writes raise — the
-- assignment / new-global / rawset / seti / library-mutation shapes,
-- all through the seven interposed luaH_* guards.
local function strip(e) return (e:gsub('^.-:%d+: ', '')) end
print("sbx02", strip(select(2, pcall(function() string.rep = 1 end))))
print("sbx02", strip(select(2, pcall(function() newglobal = 1 end))))
print("sbx02", strip(select(2, pcall(function() rawset(string, "x", 1) end))))
print("sbx02", strip(select(2, pcall(function() string[1] = 1 end))))
print("sbx02", strip(select(2, pcall(function() table.insert(string, 1) end))))
