local ok, e = pcall(string.pack, 'i17', 1)
print(not ok and e:find("out of limits %[1,16%]") ~= nil)
