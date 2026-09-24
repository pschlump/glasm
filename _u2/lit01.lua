-- U2a: literal subtype — integer syntax yields integers (D1). Corpus
-- discipline: nothing here prints an integral FLOAT directly or concats
-- one (float printing stays Go-dialect until the U5 re-pin); integers
-- print exactly on every engine.
print(1, 42, 0, 7)
print(0777, 0010, 0)          -- leading zeros stay decimal
print(-5)
-- (-0 stays U2b material: unary minus on the integer 0 must fold to the
-- integer 0, not the float -0.0; const-folding is float-view at U2a)
print(0x10, 0Xff, 0x0, 0x7fffFFFFffffFFFF)
print(0xffffffffffffffff)     -- hex wraps as unsigned: -1
print(0x8000000000000000)     -- minint
print(9223372036854775807)    -- maxint
print(-9223372036854775808)
-- decimal overflow reads as a float (masked by %.14g on both engines)
print(9223372036854775808 > 9223372036854775807)
-- float literals stay floats (compare numerically, never tostring them)
print(1.5 == 1.5, 0.5 > 0, 3.0 == 3, 2.5 + 0.5 == 3)
print(0x1p4 == 16, 0x.8p1 == 4, 1e2 == 100, .5 == 0.5)
