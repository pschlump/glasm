local utf8 = require'utf8'
for p, c in utf8.codes("a\xc3\xa9") do print(p, c) end
