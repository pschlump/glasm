local ok, e = pcall(function() return ('x') + 1 end)
print(ok, e:find('arithmetic on a string value') ~= nil)
