-- U4d/U4g: uncaught error unwinds through a to-be-closed scope — the
-- clean __close runs (observable) and the ORIGINAL error is the outcome.
-- The main chunk has no C trampoline: lua_main's error arm runs the
-- protected close (row 77).
local c <close> = setmetatable({}, {__close = function() print('closed') end})
error('boom')
