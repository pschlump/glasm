# _cli-tests — black-box CLI integration tests.
#
# Three engines run the same standalone Lua programs and their stdout is
# diffed byte-for-byte against golden files in ref/:
#
#   interp  ../cmd/glua/glua prog.lua args...      golua55 interpreter
#   wasm    ../cmd/glua/glua -w + -W               Lua->wasm backend
#   clua    tmp/clua-run prog.lua args...          stock C Lua 5.1 in wasm
#
# (the lwc2 tests 000-002 predate the clua leg and stay two-engine).
#
# Known-broken wasm legs (divergence ledger rows) run as XFAIL targets:
# they assert the wasm output still DIFFERS, and fail the suite when the
# engine starts matching — a nudge to promote them back to check3.
#
# WAZERO=1 (M6b engine-parity leg, ledger row 32: one blob, two hosts)
# swaps every wasm leg from the wasmtime oracle to the wazero production
# engine — same golden files, same diffs:
#
#   make test WAZERO=1

GLUA = ../cmd/glua/glua
CLUA = tmp/clua-run

ifdef WAZERO
export GLUA_WASM_ENGINE = wazero
WASMENG = wazero
else
WASMENG = wasmtime
endif

.PHONY: test setup tools fin refs \
	test000 test001 test002 \
	test003 test004 test005 test006 test007 test008 test009 \
	test010 test011 test012 test013 test014 test015 test016 \
	test017 test018 test019 test020 test021 test022 test023 \
	test024 test025 test026 test027 test028 test029 test030 \
	test031 test032 test033 test034 test035 test036 test037 test038

test: setup tools
	@if $(GLUA) -w tmp/_probe.wasm ./lwc2.lua >/dev/null 2>&1; then \
		$(MAKE) test-engines; \
	else \
		echo "SKIP _cli-tests: wasm backend retired at the U2c encoding cutover until U2e (ledger row 59)"; \
	fi

test-engines: \
	test000 test001 test002 \
	test003 test004 test005 test006 test007 test008 test009 \
	test010 test011 test012 test013 test014 test015 test016 \
	test017 test018 test019 test020 test021 test022 test023 \
	test024 test025 test026 test027 test028 test029 test030 \
	test031 test032 test033 test034 test035 test036 test037 test038 \
	fin

setup:
	mkdir -p out ref tmp data

tools:
	@cd .. && go build -o cmd/glua/glua ./cmd/glua
	@cd .. && go build -o _cli-tests/tmp/clua-run ./cmd/clua-run

fin:
	@echo PASS

# check3 NAME PROG ARGS — all three engines must match ref/NAME.out.
define check3
	@echo "== $(1) (interp / wasm-$(WASMENG) / clua)"
	$(GLUA) -w tmp/$(2).wasm ./$(2).lua > /dev/null
	$(GLUA) ./$(2).lua $(3) > out/$(1).i.out
	$(GLUA) -W tmp/$(2).wasm $(3) > out/$(1).w.out
	$(CLUA) ./$(2).lua $(3) > out/$(1).c.out
	diff out/$(1).i.out ref/$(1).out
	diff out/$(1).w.out ref/$(1).out
	diff out/$(1).c.out ref/$(1).out
endef

# xfailw NAME PROG ARGS — interp and clua match the shared ref; the wasm
# leg must still DIFFER (ledger row 31: silent exit, garbage error, trap,
# or hang). timeout + || true so every failure mode reaches the flip
# check. Promote to check3 when the row is fixed.
define xfailw
	@echo "== $(1) (xfail wasm: ledger row 31)"
	$(GLUA) -w tmp/$(2).wasm ./$(2).lua > /dev/null
	$(GLUA) ./$(2).lua $(3) > out/$(1).i.out
	$(CLUA) ./$(2).lua $(3) > out/$(1).c.out
	diff out/$(1).i.out ref/$(1).out
	diff out/$(1).c.out ref/$(1).out
	timeout 30 $(GLUA) -W tmp/$(2).wasm $(3) > out/$(1).w.out 2>&1 || true
	if diff -q out/$(1).w.out ref/$(1).out; then \
		echo "$(1): wasm now matches — promote to check3"; exit 1; \
	else \
		echo "  wasm diverges as expected (ledger row 31)"; \
	fi
endef

# ---- original wc-clone tests (two engines). The file list is pinned to
# the two files the refs were generated from — data/* would silently
# pick up every new corpus file. ----

LWC_DATA = data/license.txt data/ls.out

test000:
	@echo test000
	../cmd/glua/glua -w tmp/lwc2.wasm ./lwc2.lua
	../cmd/glua/glua -W tmp/lwc2.wasm $(LWC_DATA) >./out/out001.out
	diff ./out/out001.out ./ref/out001.out

test001:
	@echo test001
	../cmd/glua/glua ./lwc2.lua $(LWC_DATA) >./out/out002.out
	diff ./out/out002.out ./ref/out002.out

test002:
	@echo test000
	../cmd/glua/glua -w tmp/lwc2.wasm ./lwc2.lua
	../cmd/glua/glua -W tmp/lwc2.wasm -w $(LWC_DATA) >./out/out003.out
	diff ./out/out003.out ./ref/out003.out

# ---- cat ----

test003:
	$(call check3,cat1,cat,data/poem.txt data/names.txt)
test004:
	$(call check3,cat2,cat,-n data/license.txt)
test005:
	$(call check3,cat3,cat,data/poem.txt data/nope.txt)

# ---- grep ----

test006:
	$(call check3,grep1,grep,dog data/poem.txt)
test007:
	$(call check3,grep2,grep,-n moon data/poem.txt data/ls.out)
test008:
	$(call check3,grep3,grep,-c the data/poem.txt data/names.txt)
test009:
	$(call check3,grep4,grep,-nv the data/poem.txt)

# ---- sortl (pure-Lua merge sort) ----

test010:
	$(call check3,sortl1,sortl,data/nums.txt)
test011:
	$(call check3,sortl2,sortl,-n data/nums.txt)
test012:
	$(call check3,sortl3,sortl,-ru data/names.txt)
test013:
	$(call check3,sortl4,sortl,-u data/poem.txt)

# ---- uniq ----

test014:
	$(call check3,uniq1,uniq,data/poem.txt)
test015:
	$(call check3,uniq2,uniq,-c data/poem.txt)
test016:
	$(call check3,uniq3,uniq,-cd data/poem.txt)

# ---- caesar (13 twice = identity) ----

test017:
	$(call check3,caesar1,caesar,13 data/poem.txt data/poem.txt)
test018:
	$(call check3,caesar2,caesar,3 data/names.txt)

# ---- primes / fib ----

test019:
	$(call check3,primes1,primes,100)
test020:
	$(call check3,primes2,primes,10000)
test021:
	$(call check3,fib1,fib,10)
test022:
	$(call check3,fib2,fib,90)

# ---- brainfuck ----

test023:
	$(call check3,bf1,bf,data/hello.bf data/abc.bf)

# ---- mandelbrot ----

test024:
	$(call check3,mandel1,mandel,70 22 60)
test025:
	$(call check3,mandel2,mandel,30 12 30)

# ---- life ----

test026:
	$(call check3,life1,life,data/glider.life 8 4)

# ---- kvdb: one command sequence, three engines, one shared db ----

KVDB_OUT = out/kvdb.out

# kvdb_seq ENGINE REDIR — one command per shell line (make runs each
# recipe line separately), every line appending through $(2).
define kvdb_seq
	$(1) tmp/kvdb.db set alpha one $(2)
	$(1) tmp/kvdb.db set beta two $(2)
	$(1) tmp/kvdb.db set gamma 3 $(2)
	$(1) tmp/kvdb.db incr gamma $(2)
	$(1) tmp/kvdb.db incr gamma 10 $(2)
	$(1) tmp/kvdb.db get alpha $(2)
	$(1) tmp/kvdb.db get gamma $(2)
	$(1) tmp/kvdb.db get zeta $(2)
	$(1) tmp/kvdb.db keys $(2)
	$(1) tmp/kvdb.db keys gam $(2)
	$(1) tmp/kvdb.db del beta $(2)
	$(1) tmp/kvdb.db get beta $(2)
	$(1) tmp/kvdb.db keys $(2)
	$(1) tmp/kvdb.db incr nope $(2)
	$(1) tmp/kvdb.db set bad=x $(2)
	$(1) tmp/kvdb.db frobnicate $(2)
endef

test027:
	@echo "== kvdb (interp / wasm-$(WASMENG) / clua, one shared db)"
	$(GLUA) -w tmp/kvdb.wasm ./kvdb.lua > /dev/null
	rm -f tmp/kvdb.db $(KVDB_OUT)
	$(call kvdb_seq,$(GLUA) ./kvdb.lua,>> $(KVDB_OUT))
	diff $(KVDB_OUT) ref/kvdb.out
	rm -f tmp/kvdb.db $(KVDB_OUT)
	$(call kvdb_seq,$(GLUA) -W tmp/kvdb.wasm,>> $(KVDB_OUT))
	diff $(KVDB_OUT) ref/kvdb.out
	rm -f tmp/kvdb.db $(KVDB_OUT)
	$(call kvdb_seq,$(CLUA) ./kvdb.lua,>> $(KVDB_OUT))
	diff $(KVDB_OUT) ref/kvdb.out
	rm -f tmp/kvdb.db $(KVDB_OUT)

# ---- calc (row 31 fixed: call_body rebasing across stack growth) ----

test028:
	$(call check3,calc1,calc,-f data/exprs.txt)
test029:
	$(call check3,calc2,calc,'2*(3+4)' '10 % 3' '2^0.5' '-3^-2')

# ---- queens / word frequency ----

test030:
	$(call check3,q8-1,q8,8)
test031:
	$(call check3,q8-2,q8,6)
test032:
	$(call check3,wfreq1,wfreq,-8 data/poem.txt)
test033:
	$(call check3,wfreq2,wfreq,-5 data/poem.txt data/names.txt)

# ---- error surfaces: each engine keeps its own golden file ----

test034:
	@echo "== err1 (uncaught-error surfaces, per-engine refs)"
	$(GLUA) -w tmp/err1.wasm ./err1.lua > /dev/null
	$(GLUA) ./err1.lua > out/err1.i.out 2>&1; echo "exit=$$?" >> out/err1.i.out
	$(GLUA) -W tmp/err1.wasm > out/err1.w.out 2>&1; echo "exit=$$?" >> out/err1.w.out
	$(CLUA) ./err1.lua > out/err1.c.out 2>&1; echo "exit=$$?" >> out/err1.c.out
	diff out/err1.i.out ref/err1.i.out
	diff out/err1.w.out ref/err1.w.out
	diff out/err1.c.out ref/err1.c.out

test035:
	@echo "== syntaxerr (compile-refusal surfaces, per-engine refs)"
	if $(GLUA) -w tmp/syntaxerr.wasm ./syntaxerr.lua > out/syntaxerr.compile.out 2>&1; then \
		echo "compile unexpectedly succeeded"; exit 1; \
	fi
	test ! -e tmp/syntaxerr.wasm
	$(GLUA) ./syntaxerr.lua > out/syntaxerr.i.out 2>&1; echo "exit=$$?" >> out/syntaxerr.i.out
	$(CLUA) ./syntaxerr.lua > out/syntaxerr.c.out 2>&1; echo "exit=$$?" >> out/syntaxerr.c.out
	diff out/syntaxerr.compile.out ref/syntaxerr.compile.out
	diff out/syntaxerr.i.out ref/syntaxerr.i.out
	diff out/syntaxerr.c.out ref/syntaxerr.c.out

# ---- concat stress (was the row 27 pinner; now a plain gate) ----

test036:
	$(call check3,concatstress,concatstress,)

# ---- table.sort (row 10u regression) ----

test037:
	$(call check3,tsort1,tsort,300)
test038:
	$(call check3,tsort2,tsort,1000)

# ---- ref regeneration (from interp, the primary oracle; per-engine
# refs regenerated from their own engine). Review the diff before
# committing: a changed ref is a behavior change. ----

refs:
	$(GLUA) ./cat.lua data/poem.txt data/names.txt > ref/cat1.out
	$(GLUA) ./cat.lua -n data/license.txt > ref/cat2.out
	$(GLUA) ./cat.lua data/poem.txt data/nope.txt > ref/cat3.out
	$(GLUA) ./grep.lua dog data/poem.txt > ref/grep1.out
	$(GLUA) ./grep.lua -n moon data/poem.txt data/ls.out > ref/grep2.out
	$(GLUA) ./grep.lua -c the data/poem.txt data/names.txt > ref/grep3.out
	$(GLUA) ./grep.lua -nv the data/poem.txt > ref/grep4.out
	$(GLUA) ./sortl.lua data/nums.txt > ref/sortl1.out
	$(GLUA) ./sortl.lua -n data/nums.txt > ref/sortl2.out
	$(GLUA) ./sortl.lua -ru data/names.txt > ref/sortl3.out
	$(GLUA) ./sortl.lua -u data/poem.txt > ref/sortl4.out
	$(GLUA) ./uniq.lua data/poem.txt > ref/uniq1.out
	$(GLUA) ./uniq.lua -c data/poem.txt > ref/uniq2.out
	$(GLUA) ./uniq.lua -cd data/poem.txt > ref/uniq3.out
	$(GLUA) ./caesar.lua 13 data/poem.txt data/poem.txt > ref/caesar1.out
	$(GLUA) ./caesar.lua 3 data/names.txt > ref/caesar2.out
	$(GLUA) ./primes.lua 100 > ref/primes1.out
	$(GLUA) ./primes.lua 10000 > ref/primes2.out
	$(GLUA) ./fib.lua 10 > ref/fib1.out
	$(GLUA) ./fib.lua 90 > ref/fib2.out
	$(GLUA) ./bf.lua data/hello.bf data/abc.bf > ref/bf1.out
	$(GLUA) ./mandel.lua 70 22 60 > ref/mandel1.out
	$(GLUA) ./mandel.lua 30 12 30 > ref/mandel2.out
	$(GLUA) ./life.lua data/glider.life 8 4 > ref/life1.out
	$(GLUA) ./calc.lua -f data/exprs.txt > ref/calc1.out
	$(GLUA) ./calc.lua '2*(3+4)' '10 % 3' '2^0.5' '-3^-2' > ref/calc2.out
	$(GLUA) ./q8.lua 8 > ref/q8-1.out
	$(GLUA) ./q8.lua 6 > ref/q8-2.out
	$(GLUA) ./wfreq.lua -8 data/poem.txt > ref/wfreq1.out
	$(GLUA) ./wfreq.lua -5 data/poem.txt data/names.txt > ref/wfreq2.out
	$(GLUA) ./concatstress.lua > ref/concatstress.out
	$(GLUA) ./tsort.lua 300 > ref/tsort1.out
	$(GLUA) ./tsort.lua 1000 > ref/tsort2.out
	rm -f tmp/kvdb.db ref/kvdb.out
	$(call kvdb_seq,$(GLUA) ./kvdb.lua,>> ref/kvdb.out)
	rm -f tmp/kvdb.db
	$(GLUA) ./err1.lua > ref/err1.i.out 2>&1; echo "exit=$$?" >> ref/err1.i.out
	$(GLUA) -w tmp/err1.wasm ./err1.lua > /dev/null
	$(GLUA) -W tmp/err1.wasm > ref/err1.w.out 2>&1; echo "exit=$$?" >> ref/err1.w.out
	$(CLUA) ./err1.lua > ref/err1.c.out 2>&1; echo "exit=$$?" >> ref/err1.c.out
	if $(GLUA) -w tmp/syntaxerr.wasm ./syntaxerr.lua > ref/syntaxerr.compile.out 2>&1; then \
		echo "compile unexpectedly succeeded"; exit 1; \
	fi
	$(GLUA) ./syntaxerr.lua > ref/syntaxerr.i.out 2>&1; echo "exit=$$?" >> ref/syntaxerr.i.out
	$(CLUA) ./syntaxerr.lua > ref/syntaxerr.c.out 2>&1; echo "exit=$$?" >> ref/syntaxerr.c.out
	@echo "refs regenerated — review with git diff before committing"
