print(string.unpack('i2i2', '\1\0\2\0'))
print(select('#', string.unpack('i2i2', '\1\0\2\0')))
