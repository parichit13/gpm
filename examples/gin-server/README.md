# gin-server — gpm example

A [Gin](https://github.com/gin-gonic/gin) HTTP service wired to the
[`gpm` SDK](../../pkg/gpm), used to exercise every gpm capability end-to-end:
zero-downtime reload, request draining, crash failover + auto-restart, cluster
mode, and health-gated (warmup) reloads.

This is a **separate Go module** (its own `go.mod` with a `replace` pointing at
the repo root) so Gin's dependency tree stays out of the lightweight SDK.

> This example covers **reuseport (SDK) mode**. For the **proxy mode** equivalent
> — the same scenarios with an opaque binary gpm fronts with a load balancer —
> see [`examples/opaque-server`](../opaque-server/).

## Quick start

```sh
# from the repo root, make sure gpm is built and the daemon is running
go build -o gpm .
./gpm daemon start

# build and run the Gin service as a 3-instance cluster
go build -o /tmp/gin-server ./examples/gin-server
./gpm start /tmp/gin-server gin -i 3 --port 8080 --health-path /healthz

curl localhost:8080/
./gpm reload gin     # zero-downtime
```

## Run the guided demo

`demo.sh` walks through all six scenarios with a live load probe so you can see
exactly how many requests are dropped (spoiler: zero, except a hard crash):

```sh
# uses gpm from PATH, or pass a locally built one:
GPM=./gpm ./examples/gin-server/demo.sh
```

> The demo talks to the running daemon — make sure the daemon you started is the
> current build (`./gpm daemon stop && ./gpm daemon start` after rebuilding gpm).

## Endpoints

| Route | Purpose |
|---|---|
| `GET /` | Fast response with `{version, instance, pid, host, uptime}` — used by the load probe |
| `GET /work?ms=N` | Sleeps N ms then responds — for **request-draining** tests |
| `GET /crash` | Flushes a response, then `os.Exit(1)` — for **failover / auto-restart** tests |
| `GET /healthz` | Manual health view (gpm probes the SDK's own health port, not this route) |
| `GET /env` | Shows the `GPM_*` variables gpm injected into the instance |

## Build-time / runtime knobs

- `-ldflags "-X main.Version=v2"` or `VERSION=v2` — set the reported version, so
  reloads visibly roll over.
- `WARMUP_MS=4000` (env) — stay **not-ready** for 4s after start. gpm holds a
  reload until the new instance reports ready, demonstrating the health gate.

## What each scenario proves

1. **Cluster start** — `-i 3` runs three instances behind one port (`gpm list`
   shows `3/3`).
2. **Zero-downtime update** — rebuild with a new `VERSION`, `gpm reload`; the
   served version rolls over with **0 dropped requests**.
3. **Request draining** — a `GET /work?ms=8000` started before a reload still
   returns `200` with the *old* version: gpm SIGTERMs the old instance and the
   SDK drains in-flight requests (within `--shutdown-timeout`) before exiting.
4. **Failover + auto-restart** — `GET /crash` kills an instance; peers keep
   serving and gpm restarts the dead one (`RESTARTS` increments). A hard crash
   may drop the few requests already accepted by that instance — unlike a
   graceful reload, which drops none.
5. **Health-gated reload** — with `WARMUP_MS` set, each reload step waits for the
   new instance to report ready before draining the old one, so the reload takes
   longer but stays at zero failures.
6. **Scale** — `gpm scale gin 5` / `gpm scale gin 2` adds/removes instances live.

## Notes

- On **macOS**, `SO_REUSEPORT` does not load-balance as evenly as Linux; you may
  see one instance receive most traffic. The zero-downtime guarantee still holds
  because the port stays continuously bound. For even distribution and routing
  that gpm fully controls, use `--mode proxy` (see the root README).
- Graceful draining requires the service to handle `SIGTERM` — the gpm SDK's
  `Serve`/`WaitForShutdown` does this for you.
