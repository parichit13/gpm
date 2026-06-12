#!/usr/bin/env bash
#
# demo.sh — guided tour of gpm's zero-downtime features using the Gin server.
#
# Walks through, with a live load probe running against the service the whole
# time so you can see exactly how many requests (if any) are dropped:
#
#   1. cluster start          (N instances behind one port)
#   2. zero-downtime update   (v1 -> v2 reload under load)
#   3. request draining       (a slow request survives a reload)
#   4. failover + auto-restart(crash an instance; peers keep serving)
#   5. health-gated reload    (warmup keeps an instance unready)
#   6. scale up / down
#
# Usage:
#   ./demo.sh                 # uses `gpm` from PATH
#   GPM=../../gpm ./demo.sh   # use a locally built gpm binary
#
set -u

GPM="${GPM:-gpm}"
SVC="gindemo"
PORT="8080"
URL="http://localhost:${PORT}"
BIN="/tmp/gin-server"
HERE="$(cd "$(dirname "$0")" && pwd)"

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
note() { printf '   \033[2m%s\033[0m\n' "$*"; }

build() { # build <version> [extra go env...]
  local ver="$1"
  ( cd "$HERE" && go build -ldflags "-X main.Version=${ver}" -o "$BIN" . )
}

# Run a fast load probe for $1 seconds in the background, logging each result
# (version=vX or FAIL) to $2. Sets the global PROBE_PID. Must be called directly
# (not in $(...)) so the background job is a child of this shell and waitable.
PROBE_PID=""
start_probe() { # start_probe <seconds> <outfile>
  local secs="$1" out="$2"
  {
    end=$(( $(date +%s) + secs ))
    while [ "$(date +%s)" -lt "$end" ]; do
      r=$(curl -fs --max-time 2 "$URL/" 2>/dev/null)
      if [ $? -ne 0 ] || [ -z "$r" ]; then
        echo "FAIL"
      else
        echo "$r" | grep -o '"version":"[^"]*"'
      fi
    done
  } > "$out" 2>&1 &
  PROBE_PID=$!
}

tally() { # tally <outfile>
  note "responses during the window:"
  sort "$1" | uniq -c | sed 's/^/     /'
  local fails
  fails=$(grep -c FAIL "$1")
  printf '   dropped/failed requests: \033[1m%s\033[0m\n' "$fails"
}

require_daemon() {
  if ! $GPM daemon status 2>/dev/null | grep -q running; then
    say "starting gpm daemon"
    $GPM daemon start
    sleep 1
  fi
}

cleanup() {
  say "cleanup"
  $GPM delete "$SVC" 2>/dev/null && note "deleted service '$SVC'" || true
}
trap cleanup EXIT

# ──────────────────────────────────────────────────────────────────────────

require_daemon
$GPM delete "$SVC" 2>/dev/null || true

say "1. CLUSTER START — 3 instances of v1 on :$PORT"
build v1
$GPM start "$BIN" "$SVC" -i 3 --port "$PORT" --health-path /healthz
sleep 2
$GPM list
note "a few requests (each shows the serving instance):"
for i in 1 2 3; do curl -fs "$URL/" && echo; done

say "2. ZERO-DOWNTIME UPDATE — reload v1 -> v2 while under load"
build v2
start_probe 10 /tmp/gindemo_update.out
sleep 2
note "running: gpm reload $SVC"
$GPM reload "$SVC"
wait "$PROBE_PID"
tally /tmp/gindemo_update.out
note "expect: a mix of v1 and v2, and ZERO failures (version rolled over live)"

say "3. REQUEST DRAINING — an 8s request must survive a reload"
build v3
note "firing GET /work?ms=8000 (will be served by a v2 instance)..."
( curl -fs --max-time 30 "$URL/work?ms=8000" > /tmp/gindemo_slow.out 2>&1 ) &
SLOW=$!
sleep 1
note "now reloading to v3 while that request is in flight..."
$GPM reload "$SVC"
note "waiting for the slow request to return..."
wait "$SLOW"
note "slow request response:"
sed 's/^/     /' /tmp/gindemo_slow.out; echo
note "expect: HTTP 200 with version v2 — the old instance drained it instead of dropping it"

say "4. FAILOVER + AUTO-RESTART — crash an instance under load"
note "restart counts before:"; $GPM list
start_probe 8 /tmp/gindemo_crash.out
sleep 2
note "running: curl $URL/crash  (kills whichever instance answers)"
curl -fs "$URL/crash" && echo
wait "$PROBE_PID"
tally /tmp/gindemo_crash.out
sleep 2
note "restart counts after (the crashed instance was auto-restarted):"; $GPM list
note "expect: peers absorbed the traffic and RESTARTS incremented"

say "5. HEALTH-GATED RELOAD — new instances need a 4s warmup before they're ready"
note "re-registering '$SVC' with WARMUP_MS=4000 in its env (reload inherits service env)"
$GPM delete "$SVC"; sleep 1
build v4
$GPM start "$BIN" "$SVC" -i 2 --port "$PORT" --health-path /healthz --env WARMUP_MS=4000
note "waiting out the initial warmup..."; sleep 5
$GPM list
build v5
start_probe 18 /tmp/gindemo_warm.out
sleep 1
note "running: gpm reload $SVC  (gpm holds each swap until the new instance reports ready)"
time $GPM reload "$SVC"
wait "$PROBE_PID"
tally /tmp/gindemo_warm.out
note "expect: reload takes noticeably longer (it waits out the per-instance warmup) but still ZERO failures"
note "(reuseport caveat: the kernel routes to a new instance as soon as it binds; readiness gates"
note " when gpm drains the OLD instance. In --mode proxy, readiness also gates traffic to the new one.)"

say "6. SCALE — 3 -> 5 -> 2 instances"
$GPM scale "$SVC" 5; sleep 1; $GPM list
$GPM scale "$SVC" 2; sleep 1; $GPM list

say "DONE — all scenarios complete"
