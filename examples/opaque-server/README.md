# opaque-server — gpm proxy-mode example

A plain `net/http` service that **does not import the gpm SDK**. It represents
an "opaque" binary you can't (or don't want to) modify — it only follows two
conventions any process manager can rely on:

1. it reads its listen port from **`$PORT`**, and
2. it shuts down gracefully on **`SIGTERM`**.

gpm runs it in **proxy mode**: gpm owns the public port and load-balances across
a pool of these instances on private `$PORT` addresses, swapping the pool
atomically on reload/scale (and retrying past a draining backend), so the public
port is never closed.

Stdlib only — no external dependencies, so it lives in the main module.

## Quick start

```sh
# from the repo root, with gpm built and the daemon running
go build -o /tmp/opaque-server ./examples/opaque-server
./gpm start /tmp/opaque-server web -i 3 --port 8080 --mode proxy --health-path /healthz

curl localhost:8080/        # round-robined across private ports by gpm's proxy
./gpm reload web            # zero-downtime
```

## Run the guided demo

```sh
GPM=./gpm ./examples/opaque-server/demo.sh
```

It covers the same scenarios as the SDK demo, in proxy mode: cluster start,
zero-downtime update, request draining, crash failover + auto-restart,
health-gated (warmup) reload, and scaling.

## Endpoints

| Route | Purpose |
|---|---|
| `GET /` | `{version, instance, pid, port, uptime}` — note the private `port` rotates |
| `GET /work?ms=N` | Sleeps N ms — for request-draining tests |
| `GET /crash` | Flushes a response then `os.Exit(1)` — for failover tests |
| `GET /healthz` | `200` when ready, `503` during warmup — what gpm probes |

## Knobs

- `VERSION=v2` (or `-ldflags "-X main.Version=v2"`) — reported version.
- `WARMUP_MS=4000` — report unready for 4s. In proxy mode gpm only adds the
  instance to the load balancer **after** `/healthz` passes, so warmup genuinely
  gates traffic to the new instance (a stronger guarantee than reuseport, where
  the kernel routes on bind).

## reuseport vs proxy

| | reuseport (SDK) | proxy (this example) |
|---|---|---|
| Service changes | import `pkg/gpm` | none (reads `$PORT`, handles SIGTERM) |
| Who binds the public port | every instance (`SO_REUSEPORT`) | gpm |
| Load balancing | kernel (uneven on macOS) | gpm, even round-robin |
| Readiness gates traffic | no (gates drain-of-old only) | yes (only routes when ready) |
| Hot path | direct to instance | one extra proxy hop |

See the [SDK example](../gin-server/) for the reuseport side, and the
[root README](../../README.md) for the full picture.
