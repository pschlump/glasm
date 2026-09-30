-- U4d: non-closable registration is a runtime error whose core text
-- carries the variable name (row 75's rt name table feeds it on wasm);
-- strip the position prefix — that is the row-60 seam — and compare the
-- text byte-exact.
local ok, err = pcall(function() local x <close> = {} end)
print(ok, (err:gsub("^.-: ", "")))
