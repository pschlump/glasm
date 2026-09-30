local ok, e = pcall(function() return '3' & 5 end)
print(ok, e:find('bitwise operation on a string value') ~= nil)
