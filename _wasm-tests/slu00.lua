local utf8 = require'utf8'
print(utf8.len("h\xc3\xa9llo"), utf8.len("\xe1\x80\x80"))
