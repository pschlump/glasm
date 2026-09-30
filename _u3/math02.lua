-- U3b: strings through arith reach the 5.5 surface via tonumber/tostring
-- round-trips only (corpus discipline: no integral-float printing).
print(tonumber(tostring(math.maxinteger)) == math.maxinteger)
print(tonumber(tostring(math.mininteger)) == math.mininteger)
for i = 1, 40 do
  local x = i / 7
  assert(x == tonumber(tostring(x)))
end
