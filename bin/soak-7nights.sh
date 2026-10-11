#!/bin/sh
# U7d counting soak: keep bin/run-fuzzer.sh armed across a multi-night window.
#
# run-fuzzer.sh budgets one invocation to FUZZ_BUDGET (default 390m) and
# handles chunk wedges (luafuzz wall-guard exit 99) and battery pauses
# internally; it exits 0 when the budget is spent and >=2 on usage/lock
# errors or a wedge. This wrapper re-arms it until the counting window
# closes, pinning the deadline in note/fuzz-window-end so a restart
# (reboot, logout, kill) resumes the SAME window instead of extending it.
# Touch note/fuzz-stop to end the window early; delete note/fuzz-window-end
# first to start a fresh one.
#
# usage:  nohup bin/soak-7nights.sh > note/fuzz-counting-u7d.log 2>&1 &
# status: bin/check-fuzz-status.sh  (nightly totals land in note/fuzz/soak.json)
#
# No GODEBUG here on purpose: the soak's process-bounded chunks (~6.5 min
# each) are the wedge mitigation, and prior clocks ran without it; a wedge
# surfaces as run-fuzzer exit 99 and is re-armed below.
set -u
cd "$(dirname "$0")/.." || exit 2

NIGHTS=7
MAX_FASTFAILS=5     # consecutive short/failed arms before giving up
WINDOW_FILE=note/fuzz-window-end
STOP_FILE=note/fuzz-stop

now() { date +%s; }
log() { echo "soak-7nights: $(date '+%F %T') $*"; }

# rend EPOCH — cosmetic readable rendering; homebrew coreutils shadows
# /bin/date with GNU date (where -r means --reference=FILE), so try BSD
# then GNU then fall back to the raw epoch.
rend() {
  r=$(date -r "$1" '+%F %T' 2>/dev/null) ||
  r=$(date -d "@$1" '+%F %T' 2>/dev/null) ||
  r="epoch $1"
  echo "$r"
}

if [ -f "$WINDOW_FILE" ]; then
  end=$(cat "$WINDOW_FILE" 2>/dev/null)
  case "$end" in ''|*[!0-9]*) end='' ;; esac
fi
if [ -z "${end:-}" ] || [ "$end" -le "$(now)" ]; then
  end=$(( $(now) + NIGHTS * 24 * 3600 ))
  echo "$end" > "$WINDOW_FILE"
  log "new counting window ends $(rend "$end")"
else
  log "resuming counting window ending $(rend "$end")"
fi

fastfails=0
while [ "$(now)" -lt "$end" ]; do
  if [ -f "$STOP_FILE" ]; then
    log "stop file present — ending early"
    break
  fi
  t0=$(now)
  # caffeinate -i ties sleep-prevention to actual fuzzing activity only.
  caffeinate -i bin/run-fuzzer.sh -engines interp,wasm,wazero,wazero-prod
  rc=$?
  dt=$(( $(now) - t0 ))
  if [ "$rc" -ne 0 ] || [ "$dt" -lt 300 ]; then
    fastfails=$(( fastfails + 1 ))
    log "run-fuzzer exited $rc after ${dt}s (fastfail $fastfails/$MAX_FASTFAILS)"
    if [ "$fastfails" -ge "$MAX_FASTFAILS" ]; then
      log "giving up — check note/fuzz-counting-u7d.log and soak state"
      exit 1
    fi
    sleep 120
    continue
  fi
  fastfails=0
  sleep 30
done

if [ "$(now)" -ge "$end" ]; then
  log "counting window complete"
  rm -f "$WINDOW_FILE"
fi
