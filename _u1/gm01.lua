print("gm01")
-- gmatch constructs lazily (stock's gmatch_aux compiles the pattern at
-- the FIRST iterator call): a malformed pattern must not raise at
-- construction, and must raise at the iteration with the call site's
-- position — pins the eager-pm.Find divergence the U11e-c smoke found
local ok, it = pcall(string.gmatch, "ac", "a(")
print(ok, type(it))
print(pcall(function()
  for c in it do end
end))
-- the lazy compile serves well-formed patterns identically: captures,
-- position captures, the empty-match dance
local a, b = string.gmatch("alpha=7 bravo=42", "(%a+)=(%d+)")()
print(a, b)
local ps = {}
for p in string.gmatch("ab", "()") do ps[#ps + 1] = p end
print(#ps, ps[1], ps[2], ps[3])
local n = 0
for w in string.gmatch(" x  y ", "%a*") do n = n + 1 end
print("steps", n)
