print(string.unpack('<i2', '\1\0'), string.unpack('>i4', string.pack('>i4', 0x01020304)))
