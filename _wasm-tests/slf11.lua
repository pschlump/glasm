local ok, e = pcall(string.format, '%10q', 'x')
print(not ok and e:find('cannot have modifiers') ~= nil)
