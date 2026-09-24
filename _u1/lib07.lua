-- package.searchers (renamed from loaders, 5.2) and package.searchpath
print(type(package.searchers), package.loaders)
print(type(package.searchers[1]), type(package.searchers[2]))
-- searchpath over real files
local wf = assert(io.open("u1sp.lua", "w")); wf:write("return 1\n"); wf:close()
local p, err = package.searchpath("u1sp", "./?.lua")
print(p)
print(package.searchpath("u1sp", "?"))
print(package.searchpath("nope", "./?.lua"))
os.remove("u1sp.lua")
