-- l3_stringwork: string-heavy processing — build ~150KB of text, then scan,
-- split and re-pool it (concat / rep / sub / find / byte / format).
local words = { "alpha", "bravo", "charlie", "delta", "echo", "foxtrot",
  "golf", "hotel", "india", "juliet", "kilo", "lima", "mike", "november",
  "oscar", "papa", "quebec", "romeo", "sierra", "tango", "uniform",
  "victor", "whiskey", "xray", "yankee", "zulu" }
local lines = {}
for i = 1, 3000 do
  local a = words[(i % 26) + 1]
  local b = words[((i * 7) % 26) + 1]
  local c = words[((i * 13) % 26) + 1]
  lines[i] = string.format("%04d %s %s %s", i, a, b, c)
end
-- block-concat: golua55's registry (both orig and fork) overflows when
-- table.concat pushes thousands of entries at once; blocks of 128 keep the
-- registry flat while producing the identical text.
local text = ""
for blk = 1, #lines, 128 do
  local part = {}
  for k = blk, math.min(blk + 127, #lines) do
    part[#part + 1] = lines[k]
  end
  text = text .. table.concat(part, "\n") .. "\n"
end
local totalBytes, linesWithEcho, firstXray = 0, 0, 0
for ln in string.gmatch(text, "[^\n]+") do
  totalBytes = totalBytes + #ln
  if string.find(ln, "echo", 1, true) then
    linesWithEcho = linesWithEcho + 1
  end
  if firstXray == 0 and string.find(ln, "xray", 1, true) then
    firstXray = tonumber(string.sub(ln, 1, 4))
  end
end
local pool = {}
for i = 1, 800 do
  pool[i] = string.sub(text, ((i * 173) % (#text - 40)) + 1, ((i * 173) % (#text - 40)) + 20)
end
local digest = 0
for i = 1, #pool do
  local s = pool[i]
  for j = 1, #s do
    digest = (digest + string.byte(s, j)) % 1000003
  end
end
print(#text, totalBytes, linesWithEcho, firstXray, digest)
