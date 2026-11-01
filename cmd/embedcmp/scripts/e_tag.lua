-- e_tag: small table build + table.concat reply (the classic
-- build-a-reply-array EVAL shape).
local t = {}
for i = 1, #KEYS do
  t[i] = KEYS[i] .. "=" .. tostring(i * i)
end
return table.concat(t, ",")
