-- pairsarm01: U14 pairs/next arm — stock luaH_next lowered at
-- TFORCALL. String keys (bucket walk + slot-sequential hash scan),
-- array part, mixed, a DELETED key's node still anchoring the resume
-- (findindex deadok=1), int control inside the array (resume at k),
-- a shadowed next (compares unequal to the pin — C path), and
-- mutation during iteration staying stock.
print("pairsarm01")
local t = {} t.b=1 t.a=2 t.c=3
local n = 0 for k,v in pairs(t) do n = n + v end print("str", n)
local a = {} for i=1,10 do a[i]=i end
local s = 0 for i,v in pairs(a) do s = s + v end print("arr", s)
local m = {} m.x=1 for i=1,5 do m[i]=i end m.y=2
local q = 0 for k,v in pairs(m) do q = q + 1 end print("mixed", q)
-- deleted anchor: remove the CURRENT key mid-loop — the next resume
-- walks the dead node (deadok), exactly stock
local d = {} d.k1=1 d.k2=2 d.k3=3
local c = 0 for k,v in pairs(d) do c = c + 1; d[k] = nil end
print("after-del", c, next(d))
-- int control via a plain next() start inside the array part
local z = {} for i=1,4 do z[i]=i end
print("intctl", next(z, 2))
-- shadowed global next: pairs uses the static luaB_next, results
-- must be unchanged (the arm pins the static, not the global)
local realnext = next
next = function(tt, k) return realnext(tt, k) end
local w = 0 for k,v in pairs({p=1,q=2}) do w = w + v end
print("shadow", w)
next = realnext
