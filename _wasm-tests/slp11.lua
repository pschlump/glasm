local ok, e = pcall(string.packsize, 'z')
print(not ok and e:find('variable%-length format') ~= nil)
