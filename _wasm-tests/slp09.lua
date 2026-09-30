local s = string.pack('i4', 7)
print(string.unpack('i4', s, #s + 1 - 4))
