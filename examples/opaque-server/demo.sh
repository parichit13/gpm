#!/usr/bin/env bash
#
# demo.sh — guided tour of gpm's PROXY mode using an opaque binary (no SDK).
#
# gpm owns the public port and load-balances to a pool of instances on private
# $PORT addresses, swapping the pool atomically on reload/scale (retrying past a
# draining backend). Same scenarios as the SDK demo, but for binaries you can't
# modify:
#
#   1. cluster start          (N instances behind gpm's front proxy)
#   2. zero-downtime update   (v1 -> v2 reload, upstreams swapped one at a time)
#   3. request draining       (a slow request survives a reload)
#   4. failover + auto-restart(crash an instance; proxy retries, gpm restarts it)
#   5. health-gated reload    (warmup: gpm only routes to a new instance once ready)
#   6. scale up / down
#
# Usage:
#   ./demo.sh                 # uses `gpm` from PATH
#   GPM=../../gpm ./demo.sh   # use a locally built gpm binary
#
set -u

GPM="${GPM:-gpm}"
SVC="opaquedemo"
PORT="8080"
URL="http://localhost:${PORT}"
BIN="/tmp/opaque-server"
HERE="$(cd "$(dirname "$0")" && pwd)"

say()  { printf '\n\033[1;36m== %s\033[0m\n' "$*"; }
note() { printf '   \033[2m%s\033[0m\n' "$*"; }

build() { # build <version>
  ( cd "$HERE" && go build -ldflags "-X main.Version=$1" -o "$BIN" . )
}

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
  printf '   dropped/failed requests: \033[1m%s\033[0m\n' "$(grep -c FAIL "$1")"
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

say "1. CLUSTER START — 3 opaque instances behind gpm's proxy on :$PORT"
build v1
$GPM start "$BIN" "$SVC" -i 3 --port "$PORT" --mode proxy --port-env PORT --health-path /healthz
sleep 2
$GPM list
note "a few requests (round-robined by gpm's proxy — note the rotating private ports):"
for i in 1 2 3; do curl -fs "$URL/" && echo; done

say "2. ZERO-DOWNTIME UPDATE — reload v1 -> v2 while under load"
build v2
start_probe 10 /tmp/opaquedemo_update.out
sleep 2
note "running: gpm reload $SVC  (proxy adds the new upstream, drops the old, per instance)"
$GPM reload "$SVC"
wait "$PROBE_PID"
tally /tmp/opaquedemo_update.out
note "expect: an even mix of v1 and v2, and ZERO failures"

say "3. REQUEST DRAINING — an 8s request must survive a reload"
build v3
note "firing GET /work?ms=8000 (served by a v2 instance)..."
( curl -fs --max-time 30 "$URL/work?ms=8000" > /tmp/opaquedemo_slow.out 2>&1 ) &
SLOW=$!
sleep 1
note "now reloading to v3 while that request is in flight..."
$GPM reload "$SVC"
note "waiting for the slow request to return..."
wait "$SLOW"
note "slow request response:"
sed 's/^/     /' /tmp/opaquedemo_slow.out; echo
note "expect: HTTP 200 with version v2 — the proxy kept the connection open while the old instance drained"

say "4. FAILOVER + AUTO-RESTART — crash an instance under load"
note "restart counts before:"; $GPM list
start_probe 8 /tmp/opaquedemo_crash.out
sleep 2
note "running: curl $URL/crash  (kills whichever instance answers)"
curl -fs "$URL/crash" && echo
wait "$PROBE_PID"
tally /tmp/opaquedemo_crash.out
sleep 2
note "restart counts after:"; $GPM list
note "expect: the proxy retried past the dead backend; gpm restarted it on the same private port"

say "5. HEALTH-GATED RELOAD — new instances need a 4s warmup before they're ready"
note "re-registering '$SVC' with WARMUP_MS=4000 (proxy only routes to an instance once /healthz passes)"
$GPM delete "$SVC"; sleep 1
build v4
$GPM start "$BIN" "$SVC" -i 2 --port "$PORT" --mode proxy --port-env PORT --health-path /healthz --env WARMUP_MS=4000
note "waiting out the initial warmup..."; sleep 5
$GPM list
build v5
start_probe 18 /tmp/opaquedemo_warm.out
sleep 1
note "running: gpm reload $SVC"
time $GPM reload "$SVC"
wait "$PROBE_PID"
tally /tmp/opaquedemo_warm.out
note "expect: reload waits out the per-instance warmup; traffic only reaches a new instance after it's ready; ZERO failures"

say "6. SCALE — 2 -> 5 -> 2 instances"
$GPM scale "$SVC" 5; sleep 1; $GPM list
$GPM scale "$SVC" 2; sleep 1; $GPM list

say "DONE — all proxy-mode scenarios complete"
