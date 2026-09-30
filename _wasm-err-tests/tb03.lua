-- U4d: a __close error with no error in flight (normal exit) — the
-- close error itself is the uncaught outcome.
local c <close> = setmetatable({}, {__close = function() error('plain-bad') end})
