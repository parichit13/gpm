# gpm SDK (`github.com/parichit/gpm/pkg/gpm`)

Import this package in a Go service to get **zero-downtime reloads** under the
gpm process manager.

## Why it's needed

A process manager can't give an opaque binary zero-downtime restarts: when the
old process is killed and a new one starts, there's a window where nothing is
listening and connections drop. The fix is for the service to (1) bind its port
with `SO_REUSEPORT` so a new instance can come up on the same port *before* the
old one exits, and (2) shut down gracefully on `SIGTERM` so in-flight requests
drain instead of being cut. This SDK does both in ~3 lines.

## Usage

```go
package main

import (
	"net/http"

	"github.com/parichit/gpm/pkg/gpm"
)

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handler)
	srv := &http.Server{Handler: mux}

	ln, err := gpm.Listen(":8080") // SO_REUSEPORT listener; :8080 used when run outside gpm
	if err != nil {
		panic(err)
	}
	_ = gpm.Serve(srv, ln) // serves, then graceful-shuts-down on SIGTERM
}
```

Run it:

```sh
gpm start ./myservice api -i 3 --port 8080 --health-path /healthz
gpm reload api      # rolling, zero-downtime
```

## What gpm injects

| Env var                | Meaning                                            |
|------------------------|----------------------------------------------------|
| `GPM_LISTEN_ADDR`      | Address the service should bind (overrides default)|
| `GPM_INSTANCE`         | Instance index (0-based)                           |
| `GPM_HEALTH_ADDR`      | Per-instance addr the SDK serves health on         |
| `GPM_HEALTH_PATH`      | Health path (default `/healthz`)                   |
| `GPM_SHUTDOWN_TIMEOUT` | Graceful drain budget (e.g. `30s`)                 |

The SDK serves the health endpoint on `GPM_HEALTH_ADDR` (a unique per-instance
loopback port) rather than the shared service port — during a reload gpm probes
that address to confirm the *new* instance is ready, which a probe to the shared
`SO_REUSEPORT` port couldn't guarantee (the kernel might answer it from the old
instance).

## Non-HTTP services

Use `gpm.Listen` for the listener and `gpm.WaitForShutdown(func(ctx){...})` to
run your own accept loop and drain on `SIGTERM`. Call `gpm.Ready()` once warmup
finishes if your service isn't ready to serve the moment it starts.
