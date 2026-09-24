-- D8 removals: 5.1-only globals are gone
print(setfenv, getfenv, module, loadstring, unpack, newproxy)
print(_G.setfenv, _G.unpack)
-- their replacements exist
print(type(table.unpack), type(table.pack))
-- and _ENV is present
print(_ENV ~= nil, _ENV == _G)
