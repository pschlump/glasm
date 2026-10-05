-- U5b: whole-value materialization in both models. Unnamed '...' stays
-- hidden (VARARG k=0); a named '...t' used as a whole value promotes
-- the function to the table model (VARARG k=1 reads the vararg table),
-- and user edits of t.n / t[i] are honored by '...' afterwards.
local function unnamed(a, ...)
  local more = {...}
  print(#more, more[1], more[2])
  print(select('#', ...))
end
unnamed(1, 2, 3)
unnamed(1)

-- named, still hidden: only indexed reads, no promotion
local function named(a, ...t)
  print(t.n, t[1], ...)
end
named(1, 2, 3)

-- promotion: '{...}' materializes THROUGH the table
local function whole(a, ...t)
  local more = {...}
  print(#more, more[1], more[2], more[3])
end
whole(1, 2, 3, 4)

-- user edits honored by the next '...' use: growing past the real
-- count nil-pads from the table, shrinking truncates
local function mutate(a, ...t)
  print(select('#', ...), ...)
  t.n = 4
  print(select('#', ...), ...)
  t[1] = "x"
  print((...))
  t.n = 1
  print(select('#', ...), ...)
  t.n = 0
  print(select('#', ...))
  t.n = 2
  local more = {...}
  print(more[1], more[2])
end
mutate(0, "a", "b")

-- the table is a real table (metatable access, plain field deletion);
-- 'n' is just a field
local function real(...t)
  print(t.n, t[1], getmetatable(t))
  t.n = nil
  print(t.n)
end
real("only")

-- '...' after 't.n = nil' is the "no proper 'n'" error (var03 pins the
-- full battery); setting a VALID n back restores the same table. The
-- raise lives in the frame OWNING the '...' staging, so the wrap must
-- be around the whole vararg function — `pcall(select, '#', ...)`
-- stages '...' in THIS frame and dies uncaught before pcall runs.
local function broken(...t)
  t.n = nil
  return select('#', ...)
end
local ok, err = pcall(broken, "v1", "v2")
print(ok, (err:gsub("^.*:%d+: ", "")))
local function restored(...t)
  t.n = nil
  t.n = 1
  return (...)
end
print(pcall(restored, "v1", "v2"))
print("done")
