-- \x, \z, \ddd escapes (5.2)
print(("\x41\x42\x2e"):byte(1, 3))
print("\x00" == "\0")
print(("\065\066\067"):byte(1, 3))
print(("a\z
      b") == "ab")
local s = "one\z --[[ comment ]] two"
print(s)
print(("\n"):byte())
print(("\x09"):byte())
print(('\x27'))
print(("\x5c"))
