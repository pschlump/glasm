-- vgap01: the minimal wasm-frame multret-window divergence (row 86 —
-- interp/clua55 both correct; the wasm backend's vararg extras live ABOVE
-- the register window, so a multret VARARG flush whose destination window
-- reaches past nregs tramples extra[0]'s cell and a later fixed `...`
-- read observes the flush tail). No tailcall, no table.unpack needed:
-- select("#", ...) flushes R(9)..R(14) over the extras base (framecell
-- nregs+5), then (...) reads extra[0]'s clobbered home. Deliberately
-- included in _u5 even though the wasm gate must skip it (row 82's var06
-- precedent: a corpus case pins the divergence for the fix).
local function g(p, q, r, s, ...)
  return p, q, select("#", ...), (...)
end
print(g("a", "b", "c", "d", "s5", "s6", "s7", "s8", "s9", "s10"))

-- the same shape with a call between two fixed reads: the FIRST read is
-- correct on every engine (the clobber needs the intervening flush)
local function h(p, q, r, s, ...)
  local a1 = (...)
  local n = select("#", ...)
  local a2 = (...)
  return a1, a2, n
end
print(h("a", "b", "c", "d", "s5", "s6", "s7", "s8", "s9", "s10"))

-- and the unpack-fed dynamic tailcall (var07.lua:37's shape) — the
-- second manifestation: `call`'s arg shift reads the saved-callee cell
-- (scratchDyn0 at frame+16*framecells for the non-vararg proto) as its
-- last source, so f's LAST extra arrives as the saved callee; the flush
-- then surfaces it as (...). count 10 keeps every window inside the
-- frame allocation (count >= 11 additionally clobbers scratchDyn0
-- itself — var07's "attempt to call a number value" shape).
local call = function(f, args) return f(table.unpack(args, 1, args.n)) end
local a = {}
for i = 1, 10 do a[i] = "s" .. i end
print(call(g, a))
