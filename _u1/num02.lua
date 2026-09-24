-- malformed numbers: byte-exact stock texts
local function perr(s) local f, m = load("return " .. s) print(m) end
perr("0x")
perr("1e")
perr("1e+")
perr("0x1p")
perr("0b1")
perr("0o7")
perr("12x34")
