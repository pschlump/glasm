-- h_tiny: minimal EVAL shape — touch KEYS/ARGV, format a reply.
local n = #KEYS + #ARGV
return string.format("n=%d", n)
