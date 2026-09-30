local ok, e = pcall(string.format, '%y', 1)
print(not ok and e:find("invalid conversion '%%y' to 'format'") ~= nil)
