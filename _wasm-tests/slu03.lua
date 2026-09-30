local utf8 = require'utf8'
print(utf8.offset("h\xc3\xa9llo", 3), utf8.offset("h\xc3\xa9llo", -1), select('#', utf8.offset("h\xc3\xa9llo", 2)))
