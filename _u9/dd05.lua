print("dd05")
-- mixed trampoline + direct depth: the C-invoked callbacks (sort
-- comparator, gsub repl) run as trampolines; every call they make from
-- their compiled bodies takes the direct arm. The dd_disp_save
-- save/restore must keep each trampoline level's rt_disp_frame adopt
-- intact while direct callees nest below it.
local t = {5, 2, 8, 1, 9, 3}
table.sort(t, function(a, b)
  local function key(x) return x * 10 end
  return key(a) < key(b)
end)
print(table.concat(t, ","))
local s = string.gsub("hello world", "(%a+)", function(w)
  local function up(x) return x:upper() end
  return up(w)
end)
print(s)
-- a direct call from a sort comparator whose result feeds the
-- comparison itself (no invocation counting: comparator call COUNTS
-- are implementation-defined across sort algorithms; the ORDER is the
-- deterministic surface)
table.sort(t, function(a, b)
  local function tag(x) return (x % 2) .. ":" .. x end
  return tag(a) < tag(b)
end)
print(table.concat(t, ","))
