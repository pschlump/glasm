local ok, e = pcall(function() return -('x') end)
print(ok, e:find('arith on a string value') ~= nil)
