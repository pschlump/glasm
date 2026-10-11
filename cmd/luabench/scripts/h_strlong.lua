-- h_strlong: l3_stringwork's shape (scaled 1/5), returning the digest —
-- the "significant string processing" end of the EVAL spectrum.
local words = { "alpha", "bravo", "charlie", "delta", "echo", "foxtrot",
  "golf", "hotel", "india", "juliet", "kilo", "lima", "mike", "november",
  "oscar", "papa", "quebec", "romeo", "sierra", "tango", "uniform",
  "victor", "whiskey", "xray", "yankee", "zulu" }
local lines = {}
for i = 1, 600 do
  local a = words[(i % 26) + 1]
  local b = words[((i * 7) % 26) + 1]
  local c = words[((i * 13) % 26) + 1]
  lines[i] = string.format("%04d %s %s %s", i, a, b, c)
end
-- block-concat (registry overflow guard, see l3_stringwork.lua)
local text = ""
for blk = 1, #lines, 128 do
  local part = {}
  for k = blk, math.min(blk + 127, #lines) do
    part[#part + 1] = lines[k]
  end
  text = text .. table.concat(part, "\n") .. "\n"
end
local totalBytes, linesWithEcho = 0, 0
for ln in string.gmatch(text, "[^\n]+") do
  totalBytes = totalBytes + #ln
  if string.find(ln, "echo", 1, true) then
    linesWithEcho = linesWithEcho + 1
  end
end
local digest = 0
for i = 1, #text, 3 do
  digest = (digest + string.byte(text, i)) % 1000003
end
return string.format("%d %d %d", #text, totalBytes, digest)
