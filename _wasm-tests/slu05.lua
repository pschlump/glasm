local utf8 = require'utf8'
print(utf8.charpattern == "[\0-\127\194-\253][\128-\191]*")
