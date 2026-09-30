print((string.pack('i4', 0x01020304):gsub('.', function(c) return string.format('%02x', c:byte()) end)))
