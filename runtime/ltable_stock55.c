/*
** runtime/ltable_stock55.c — U6c (rows 33/50, plan §4.4): the renamed
** second compile of the stock table engine, delegate bodies for the
** write-protection seam.
**
** Zero-patch globals lockdown: rt_seam55.c interposes the seven
** table-WRITE entry points (luaH_pset/psetstr/psetshortstr/psetint/
** setint/set/finishset) with guard+delegate replicas — a protected
** table raises "Attempt to modify a readonly table", everything else
** delegates here. The write family's own internals (finishnodeset,
** getintfromhash, luaH_newkey, the rehash path) are static in
** ltable.c, so the only drift-proof way to call a stock body under
** another name is to compile the whole file again with those seven
** renamed — plus every other luaH_* export hidden behind private
** rt_lts_h_* names, so this object never collides with the real
** ltable.o in ANY link: the wasm blob resolves duplicates by link
** order (--allow-multiple-definition, first-definition-wins — the seam
** object comes first), but the native battery links on macOS ld,
** which has no such flag.
**
** Linked ONLY into lua55_rt.wasm (after rt_seam55.c, before the stock
** $SRC list) and the native v4 battery (runtime/tests/run.sh). The
** oracle blob (lua55_sjlj.wasm) never links it — the
** zero-perturbation pin holds by construction.
**
** The hidden copies (rt_lts_h_*) are dead code by construction: every
** call that can reach a stock write path crosses a guard replica
** first. They exist only because a translation unit must define or
** rename every symbol its source references.
*/

/* the seven delegates the guard replicas in rt_seam55.c call */
#define luaH_pset rt_lts_pset
#define luaH_psetstr rt_lts_psetstr
#define luaH_psetshortstr rt_lts_psetshortstr
#define luaH_psetint rt_lts_psetint
#define luaH_setint rt_lts_setint
#define luaH_set rt_lts_set
#define luaH_finishset rt_lts_finishset

/* every other export: private, so this object and the real ltable.o
   can coexist in one link */
#define luaH_get rt_lts_h_get
#define luaH_getshortstr rt_lts_h_getshortstr
#define luaH_getstr rt_lts_h_getstr
#define luaH_getint rt_lts_h_getint
#define luaH_Hgetshortstr rt_lts_h_Hgetshortstr
#define luaH_new rt_lts_h_new
#define luaH_resize rt_lts_h_resize
#define luaH_resizearray rt_lts_h_resizearray
#define luaH_size rt_lts_h_size
#define luaH_free rt_lts_h_free
#define luaH_next rt_lts_h_next
#define luaH_getn rt_lts_h_getn

#include "lua55/src/ltable.c"
