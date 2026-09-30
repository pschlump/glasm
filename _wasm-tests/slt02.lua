local d = table.move({1, 2, 3}, 1, 3, 1, {})
print(#d, d[1], d == table.move(d, 1, 3, 1, d))
