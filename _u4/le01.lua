-- U4e: the order family after the __le-mirror removal — first-found
-- metamethod lookup (lhs then rhs), no not-__lt(swapped) mirroring,
-- single-sided __le, order errors for type pairs without metamethods,
-- the immediate LTI/LEI/GTI/GEI dispatch, raw equality via __eq.
local mtA = {__le = function(a, b) return true end, __lt = function(a, b) return false end}
local mtB = {__le = function(a, b) return false end, __lt = function(a, b) return true end}
local a = setmetatable({}, mtA)
local b = setmetatable({}, mtB)
-- first-found: the LHS's metamethod wins
print(a <= b, a < b, b <= a, b < a)
-- single-sided: only the RHS has __le → it is used (events.lua:416)
local plain = {}
print(plain <= b, plain < b, b <= plain, b < plain)
-- a type with only __lt has no <= (the mirror is gone)
local ltOnly = setmetatable({}, {__lt = function(x, y) return true end})
print(pcall(function() return ltOnly <= plain end))
print(ltOnly < plain)
-- mixed types reach metamethods (no early type mismatch)
local numMt = {__lt = function(x, y) return true end, __le = function(x, y) return false end}
local mixed = setmetatable({}, numMt)
print(5.0 > mixed, 5 >= mixed, mixed < 5, mixed <= 5)
-- numbers and strings stay primitive; tables without metamethods error
print(pcall(function() return {} < {} end))
print(pcall(function() return {} <= {} end))
print(pcall(function() return "a" < 1 end))
print(pcall(function() return 1 <= "a" end))
-- the immediate family against an __lt/__le type
print(3 < mixed, 3 <= mixed, 3 > mixed, 3 >= mixed)
-- __eq: first-found, both-tables rule, raw mode skips it
local eqMt = {__eq = function() return true end}
local e1, e2 = setmetatable({}, eqMt), setmetatable({}, eqMt)
print(e1 == e2, e1 == 1, rawequal(e1, e2), e1 == e1)
-- rawlen + the basic-type metatable surface (5.2+/5.5)
print(rawlen({1, 2, 3}), rawlen("abcd"))
print(pcall(rawlen, 42))
