-- e_norm: string manipulation over ARGV (upper/gsub/pattern) — the
-- short-string lane.
local s = string.upper(ARGV[3])
local out = string.gsub(s, "%s+", "_")
return string.format("%s:%d:%d", out, #s, #ARGV[1])
