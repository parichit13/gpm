# gpm — Go Process Manager

A lightweight, PM2-inspired process manager for Go (and any) binaries.  
No Node.js required. No source compilation. Just point it at a binary.

## Features

- **Start, stop, restart, delete** processes by name
- **Zero-downtime reload** — roll a new version in with no dropped connections
- **Watch mode** — auto-reload (zero-downtime) when the binary at the run path changes
- **Cluster mode** — run N instances of a service (`-i N`), `scale` up/down live
- **Auto-restart** on crash (configurable max retries)
- **Log capture** — per-instance stdout/stderr written to `~/.gpm/logs/`
- **Log tailing** with `--follow` / `-f`
- **Save & resurrect** — persist your process list across reboots
- **Daemon mode** — background supervisor over a Unix socket
- **Any binary** — Go, Rust, Python, Node, shell scripts — anything executable

## Zero-downtime reload

`gpm reload <name>` replaces a service one instance at a time: it starts a new
instance, waits for it to pass a health check, then drains the old one — so the
listening port is never down and in-flight requests are never cut. If a new
instance fails its health check, the reload **aborts** and the old instances
keep serving.

The command **returns immediately**: the daemon runs the rolling reload in the
background and drains the old instances asynchronously (a slow in-flight request
can take minutes, and shouldn't block your terminal). Watch progress with
`gpm list` — old instances draining their in-flight requests show as
`reloading (N draining)` until they exit. The `INSTANCES` numerator counts every
live process (running + draining), so during the overlap it exceeds the
configured count and reconciles with what you see in `ps`:

```
NAME  INSTANCES  STATUS                  MODE   PORT  ...
web   3/2        reloading (1 draining)  proxy  8080  ...
```

Here `3/2` means 2 instances at the configured target plus 1 old instance still
draining; it drops back to `2/2` once the drain completes.

There are two modes, set with `--mode`:

- **`reuseport` (default)** — for Go services you control. Import the tiny
  [`pkg/gpm`](pkg/gpm) SDK so instances share the port via `SO_REUSEPORT`:

  ```go
  ln, _ := gpm.Listen(":8080") // SO_REUSEPORT listener
  gpm.Serve(srv, ln)           // graceful SIGTERM shutdown + health endpoint
  ```

  ```bash
  gpm start ./myservice api -i 3 --port 8080 --health-path /healthz
  gpm reload api      # rolling, zero-downtime
  ```

- **`proxy`** — for opaque binaries you can't modify. gpm fronts the public port
  with a built-in TCP load balancer; each instance runs on a private port it
  reads from `$PORT` (override with `--port-env`):

  ```bash
  gpm start ./legacy web -i 3 --port 9090 --mode proxy
  gpm reload web      # swaps proxy upstreams instance-by-instance
  ```

See [pkg/gpm/README.md](pkg/gpm/README.md) for the SDK details.

## Watch mode

Add `--watch` (`-w`) to `gpm start` and gpm auto-runs a zero-downtime `reload`
whenever the binary at the run path changes — so you just rebuild and traffic
rolls onto the new version on its own:

```bash
gpm start ./api api -i 3 --port 8080 --watch
# ... later, in your editor/CI:
go build -o ./api ./cmd/api     # gpm notices and reloads, no manual step
```

The daemon polls the binary (default every 1s, `--watch-interval <seconds>`) and
waits for it to stop changing before reloading, so a half-written or
mid-rename binary (a `go build` writes a temp file then renames over the target)
never triggers a partial reload. The watcher is paused while a service is
stopped and resumes on start; `gpm list` shows a `WATCH` column when any service
has it enabled. Because it reuses `reload`, a failed health check aborts and the
old version keeps serving.

## Install

```bash
git clone https://github.com/you/gpm
cd gpm
make install        # builds and copies to /usr/local/bin
```

Or just build:

```bash
go build -o gpm .
```

## Quick Start

```bash
# 1. Start the daemon (once per boot, or add to startup)
gpm daemon start

# 2. Start your app
gpm start ./myserver myserver

# 3. Check status
gpm list

# 4. View logs
gpm logs myserver
gpm logs myserver --follow      # tail -f style

# 5. Save list so it survives reboots
gpm save

# On next boot:
gpm daemon start
gpm resurrect
```

## Commands

| Command | Description |
|---|---|
| `gpm daemon start` | Start the background daemon |
| `gpm daemon stop` | Stop the daemon (also stops all processes) |
| `gpm daemon status` | Check if the daemon is running |
| `gpm start <binary> [name]` | Start a process (see Start Options for cluster/reload flags) |
| `gpm stop <name>` | Stop a process gracefully (SIGTERM → SIGKILL) |
| `gpm restart <name>` | Hard restart (with downtime) |
| `gpm reload <name>` | **Zero-downtime** rolling reload |
| `gpm scale <name> <n>` | Change the number of running instances |
| `gpm delete <name>` | Stop and remove a process |
| `gpm list` (or `ps`) | List all processes with instances/status/mode/port |
| `gpm logs <name>` | Show last 50 lines of stdout+stderr |
| `gpm logs <name> -f` | Follow logs live |
| `gpm logs <name> -n 100` | Show last N lines |
| `gpm save` | Save current process list |
| `gpm resurrect` | Restore previously saved processes |

## Start Options

```bash
# Pass arguments
gpm start ./myapp myapp --args "--port=8080 --env=prod"

# Set environment variables
gpm start ./myapp myapp --env "DB_URL=postgres://..." --env "PORT=8080"

# Set working directory
gpm start ./myapp myapp --cwd /opt/myapp
```

### Cluster & zero-downtime flags

| Flag | Description |
|---|---|
| `-i, --instances <n>` | Run N instances of the service (cluster mode) |
| `--port <p>` | Shared public port — required for zero-downtime reload |
| `--mode <m>` | `reuseport` (SDK, default) or `proxy` (opaque binary) |
| `--health-path <p>` | HTTP readiness path (default `/healthz` in reuseport mode) |
| `--shutdown-timeout <s>` | Graceful drain budget in seconds (default 30) |
| `--port-env <NAME>` | proxy mode: env var carrying each instance's internal port (default `PORT`) |
| `--host <h>` | Bind host (default: all interfaces) |
| `-w, --watch` | Auto-reload when the binary at the run path changes |
| `--watch-interval <s>` | Seconds between binary checks in watch mode (default 1) |

```bash
# 3 instances, zero-downtime reloadable, custom health path
gpm start ./api api -i 3 --port 8080 --health-path /ready
```

## Files

All state lives under `~/.gpm/`:

```
~/.gpm/
├── daemon.pid       # daemon PID
├── daemon.log       # daemon stdout/stderr
├── state/
│   ├── state.json   # live process state
│   └── save.json    # saved process list (for resurrect)
└── logs/
    ├── myapp-0-out.log   # per instance: <name>-<index>-out/err.log
    └── myapp-0-err.log
```

## Auto-restart on system boot

Add to your `/etc/rc.local` or a systemd unit:

```bash
# rc.local style
su - youruser -c "gpm daemon start && gpm resurrect"
```

Or as a systemd unit (`/etc/systemd/system/gpm.service`):

```ini
[Unit]
Description=GPM Process Manager
After=network.target

[Service]
Type=forking
User=youruser
ExecStart=/usr/local/bin/gpm daemon start
ExecStartPost=/bin/sleep 1
ExecStartPost=/usr/local/bin/gpm resurrect
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## Architecture

```
gpm CLI  ──[unix socket /tmp/gpm.sock]──►  gpm daemon
                                              │
                                     ┌────────┼────────┐
                                   runner   runner   runner   (one per instance)
                                     │        │        │
                                  process  process  process
                                  (+ logs) (+ logs) (+ logs)
```

- The **daemon** is a single long-running process managing all supervised services.
- A **service** has N **instances**, each owned by a **runner** goroutine that starts it, waits for exit, and restarts on crash.
- The **CLI** sends JSON requests over a Unix socket and prints the response — it has no direct knowledge of processes.
- State is flushed to `~/.gpm/state/state.json` after every change.

### How zero-downtime reload works

Each instance has a stable **slot** (`0..N-1`, exposed to the service as
`GPM_INSTANCE`) plus a unique monotonic **index** used for its log/state keys —
so a new instance for a slot can run alongside the old one during the swap
without clashing. `reload` walks the slots one at a time:

1. start a replacement instance (reuseport: same shared port; proxy: a new internal port)
2. wait for the new instance's health check to pass (probed on its own unique address)
3. route traffic to the new instance, then SIGTERM-drain the old one
4. if step 2 times out, tear down the new instance and keep the old one serving

In **reuseport** mode the kernel keeps both old and new bound to the port via
`SO_REUSEPORT`. In **proxy** mode the built-in TCP load balancer swaps the new
upstream in and the old one out atomically (retrying past a draining backend),
so the public port is never closed.
