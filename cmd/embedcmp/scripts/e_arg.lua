-- e_arg: the minimal EVAL — counts + argument arith, multi-value-ish
-- single reply. The shortest per-run shape in the corpus.
return string.format("%d %d", #KEYS + #ARGV, tonumber(ARGV[1]) + tonumber(ARGV[2]))
