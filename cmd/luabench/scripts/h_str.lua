-- h_str: short string-heavy EVAL shape (format/concat/sub/byte over ~40 keys).
local parts = {}
for i = 1, 40 do
  parts[#parts + 1] = string.format("%s%03d=%d", KEYS[1] or "k", i, i * i)
end
local joined = table.concat(parts, ",")
local sum = 0
for i = 1, #joined do
  local b = string.byte(joined, i)
  if b >= 48 and b <= 57 then
    sum = sum + (b - 48)
  end
end
return string.format("%d:%s", sum, string.sub(joined, -8))
