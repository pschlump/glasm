local ok, e = pcall(math.random, 5, 2)
print(not ok and e:find("interval is empty") ~= nil)
