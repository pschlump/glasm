-- escape errors: byte-exact stock texts (caught via load)
local function perr(s) local f, m = load(s) print(m) end
perr("return \"\\q\"")
perr("return \"\\x1\"")
perr("return \"\\xg1\"")
perr("return \"\\256\"")
perr("return \"\\999\"")
perr("return \"abc")
perr("return 'unfinished")
