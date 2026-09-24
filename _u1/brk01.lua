-- break mid-block (5.2)
local n = 0
for i = 1, 10 do
  n = n + i
  break
  n = n + 1000
end
print(n)
while true do
  if n == 1 then break end
  n = 1
end
print(n)
for i = 1, 3 do
  for j = 1, 3 do
    if j == 2 then break end
    print(i, j)
  end
end
repeat
  break
  print("unreachable")
until false
local i2 = 0
while i2 < 3 do
  i2 = i2 + 1
  do break end
end
print(i2)
