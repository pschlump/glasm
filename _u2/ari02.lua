-- U2b: integer arithmetic semantics — wrap, floor div/mod, promotion
print(7 // 2, -7 // 2, 7 // -2, -7 // -2)
print(7 % 3, -7 % 3, 7 % -3, -7 % -3)
print(7.0 // 2 == 3.0, -7.5 % 3 == 1.5)
print(10 // 2.0 == 5, 3.5 % 2 == 1.5)
-- division always promotes to float (compare, never print)
print(10 / 4 == 2.5, 10 / 5 == 2, 7 / 2 == 3.5)
-- exponentiation is float even for integers
print(2^3 == 8, 2^-1 == 0.5, 2^0.5 > 1.41)
-- wrap-around addition/subtraction/multiplication
print(9223372036854775807 + 1 == 0x8000000000000000)
print(0x8000000000000000 - 1 == 9223372036854775807)
print(0x4000000000000000 * 4 == 0x8000000000000000)
print(4611686018427387904 + 4611686018427387904 == 0x8000000000000000)
-- unary minus on integers keeps the subtype
print(-0 == 0, -(0x8000000000000000) == 0x8000000000000000)
print(-7 // 1, -(7 // 2))
-- mixed int/float promotes
print(1 + 2.5 == 3.5, 10 - 0.5 == 9.5, 3 * 0.5 == 1.5)
-- integer mod/div errors (5.5.1 texts, byte-matched via pcall+find to
-- dodge the position-attribution row)
print((select(2, pcall(function() return 1 // 0 end))):find("attempt to divide by zero") ~= nil)
print((select(2, pcall(function() return 1 % 0 end))):find("attempt to perform 'n%%0'") ~= nil)
print((select(2, pcall(function() return 1.5 // 0 == 1.5 end))))
print(1.5 // 0 == math.huge)
-- string coercion keeps subtype: "10" + 5 is the integer 15
print("10" + 5 == 15, "10" // 3 == 3, "0x10" % 5 == 2)
print(("%d"):format("10" + 5))
-- pow with exponent 2 squares exactly
print(2^2 == 4, (1/3)^2 == (1/3)*(1/3))
