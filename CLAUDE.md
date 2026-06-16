# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

gpm is a PM2-style process manager for Go (and any) network services whose headline feature is
**true zero-downtime reloads**. It's a single Go binary that runs a background **daemon**; the same
binary invoked as a CLI is a thin client that talks to the daemon over a Unix socket.

Module path: `github.com/parichit13/gpm` (must match the public repo — it's baked into imports and
the SDK's import path).

## Commands

```bash
make build                 # build ./gpm  (ldflags set cmd.Version — see "Version" gotcha)
go vet ./...
go test ./... -count=1     # only internal/proxy and internal/health have tests
go test ./internal/proxy/ -run TestProxyRetriesPastDeadUpstream -count=1   # a single test
make release               # cross-compile dist/gpm_<os>_<arch> for darwin/linux × amd64/arm64
make install               # build + install to ~/.gpm/bin, no sudo (override: INSTALL_DIR=...)
```

`examples/gin-server` is a **separate Go module** (it pulls in Gin, kept out of the SDK's deps via a
`replace` directive). `go build ./...` from the root does **not** include it — build it from its own
dir: `cd examples/gin-server && go build ./...`. `examples/opaque-server` and `pkg/gpm/example` are
in the root module (stdlib / SDK only).

### Dev loop (important)

`reload`, instance supervision, the proxy, and the watcher all run **inside the daemon**. After
changing daemon-side code you must restart the daemon for it to take effect:

```bash
go build -o gpm . && ./gpm daemon stop && ./gpm daemon start
```

CLI-only changes (flags, list rendering) take effect on the next command without a daemon restart.
The two example demo scripts (`examples/*/demo.sh`) drive a service through every scenario
(zero-downtime reload, draining, crash failover, warmup, scale) with a live load probe.

## Architecture

**Process model.** `main.go` dispatches: the hidden `__daemon_run` subcommand runs the daemon
(`internal/daemon`); everything else is the cobra CLI (`cmd/`). The CLI never touches processes
directly — it sends JSON `ipc.Request`s over `/tmp/gpm.sock` (`internal/ipc`) and the daemon owns all
state. State and logs live under `~/.gpm/` (`state/state.json`, `state/save.json`, `logs/`,
`daemon.pid`, `daemon.log`).

**Service → instances.** A registered service (`state.Process`) has N **instances**, each supervised
by its own `process.Runner` goroutine (`d.runners` is `map[serviceName][]*Runner`). This split is the
crux of most logic:

- Each instance has a **slot** (`0..N-1`, exposed to the child as `GPM_INSTANCE`) *and* a unique,
  monotonic **index** (the key for its log files `<name>-<index>-{out,err}.log` and its
  `state.InstanceState`). During a reload the new instance for a slot gets a fresh index so it
  coexists with the old one without clashing on logs/state. Don't conflate slot and index.
- Per-instance runtime lives in `Process.InstanceStates` (written via `store.SetInstanceState`), not
  the top-level `Process` fields — so N runners never race on shared fields. The top-level
  status/uptime shown in `list` is *aggregated* from instance states in `cmdList`/`aggregateStatus`.

**Two reload modes** (`--mode`, the central design axis):

- `reuseport` (default, for services that import the SDK): the daemon injects `GPM_LISTEN_ADDR` and a
  per-instance `GPM_HEALTH_ADDR`; instances bind the same port via `SO_REUSEPORT` (in `pkg/gpm`) and
  the kernel load-balances. gpm probes the per-instance health addr — **not** the shared port,
  because a probe to the shared port could be answered by the old instance.
- `proxy` (for opaque binaries): the daemon owns the public port and runs `internal/proxy` (a TCP
  load balancer with an atomically swappable upstream set). Each instance runs on a private port it
  reads from `$PORT` (configurable via `--port-env`). The proxy retries past a dead/draining upstream,
  which is what makes proxy-mode reload truly zero-downtime.

**Reload is async** (`cmdReload` → `runReload` goroutine). The CLI returns immediately because
draining can outlast any IPC deadline. Per slot: start replacement → health-gate
(`internal/health.WaitReady`, bounded) → route traffic to it → SIGTERM-drain the old one **in the
background**. Old instances show as `draining` in `list` (status `reloading (N draining)`); the
`INSTANCES` numerator counts live processes (running + draining) so it reconciles with `ps`. A failed
health check aborts the reload and leaves the old instances serving.

**Crash handling.** `Runner.supervise` restarts on exit; a run shorter than `minStableUptime` counts
toward a crash-loop, which after `crashLoopThreshold` flips the instance to `errored` with
exponential backoff (so a flapping binary shows as `errored`, not `stopped`, and doesn't hammer the
machine). `runOnce` writes the exit reason to the instance err-log (`signal: killed`, `exit status 1`)
so failures are diagnosable even when the binary printed nothing.

**Daemon auto-start.** `ipc.Send` calls the `ipc.EnsureDaemon` hook (set by `cmd` to
`ensureDaemonRunning`) when it can't reach the socket, so any command transparently starts the daemon
(post-install / post-reboot). Pings deliberately skip this so `daemon status` reports truthfully.

**Integer IDs.** Each service gets a stable id (lowest free, reused after delete — `store.NextFreeID`).
`handle()` normalizes an incoming id-or-name to the canonical name via `store.Resolve` for all
targeting commands, so `stop`/`reload`/`logs`/etc. accept either.

## Non-obvious gotchas

- **macOS code-signing / "Killed: 9".** Overwriting a previously-executed binary *in place* (e.g.
  `cp` over it) leaves the kernel's per-vnode cdhash stale and the new binary is SIGKILLed on exec.
  Always replace binaries by writing a temp file and `rename`-ing over the target (fresh inode) — this
  is why `make install` does `rm -f` first and `cmd/update.go` / `install.sh` use atomic rename +
  ad-hoc `codesign`. `go build` is safe (it renames).
- **Version wiring.** The version is baked into `cmd.Version` via
  `-ldflags "-X github.com/parichit13/gpm/cmd.Version=..."` (NOT `main.Version`). GoReleaser sets it
  to the git tag; `gpm version` and self-update read it.
- **Release asset names must stay in sync** across three places: `.goreleaser.yaml`
  (`gpm_{{.Os}}_{{.Arch}}`), `install.sh`, and `cmd/update.go` (`assetName()`). Self-update and the
  installer download these by exact name + verify against `checksums.txt`.
- **Branches.** `main` is curl-install-only. The Homebrew channel (GoReleaser `brews:` block, tap
  token, README section) is parked on the **`homebrew-support`** branch — see `HOMEBREW.md` there for
  how to finish setup and merge. Don't add Homebrew bits to `main`.
- **Releasing.** Push a `v*` tag → `.github/workflows/release.yml` runs GoReleaser (build + checksums
  + GitHub Release). The website is GitHub Pages from `main`/`docs` (static, no build step).
