package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/parichit/gpm/internal/health"
	"github.com/parichit/gpm/internal/ipc"
	"github.com/parichit/gpm/internal/process"
	"github.com/parichit/gpm/internal/proxy"
	"github.com/parichit/gpm/internal/state"
)

type Daemon struct {
	store    *state.Store
	runners   map[string][]*process.Runner // service name -> instance runners
	nextIdx   map[string]int               // service name -> next unique instance index
	proxies   map[string]*proxy.Proxy      // proxy-mode services -> front proxy
	reloading map[string]bool              // services with a reload in flight
	watchers  map[string]chan struct{}     // watch-mode services -> watcher stop signal
	mu        sync.Mutex
	stateDir  string
	logDir    string
}

func New(baseDir string) (*Daemon, error) {
	stateDir := filepath.Join(baseDir, "state")
	logDir := filepath.Join(baseDir, "logs")
	os.MkdirAll(logDir, 0755)

	store, err := state.NewStore(stateDir)
	if err != nil {
		return nil, err
	}

	return &Daemon{
		store:     store,
		runners:   make(map[string][]*process.Runner),
		nextIdx:   make(map[string]int),
		proxies:   make(map[string]*proxy.Proxy),
		reloading: make(map[string]bool),
		watchers:  make(map[string]chan struct{}),
		stateDir:  stateDir,
		logDir:    logDir,
	}, nil
}

func (d *Daemon) Run() error {
	// Remove stale socket
	os.Remove(ipc.SocketPath)

	ln, err := net.Listen("unix", ipc.SocketPath)
	if err != nil {
		return fmt.Errorf("failed to listen on socket: %w", err)
	}
	os.Chmod(ipc.SocketPath, 0660)
	defer ln.Close()

	fmt.Printf("gpm daemon started (pid=%d)\n", os.Getpid())

	// Handle OS signals
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		d.stopAll()
		ln.Close()
		os.Remove(ipc.SocketPath)
		os.Exit(0)
	}()

	// Accept connections
	for {
		conn, err := ln.Accept()
		if err != nil {
			return nil
		}
		go d.handle(conn)
	}
}

func (d *Daemon) handle(conn net.Conn) {
	defer conn.Close()

	var req ipc.Request
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		respond(conn, ipc.Response{OK: false, Error: err.Error()})
		return
	}

	var resp ipc.Response

	switch req.Action {
	case ipc.ActionPing:
		resp = ipc.Response{OK: true, Data: "pong"}

	case ipc.ActionStart:
		resp = d.cmdStart(req)

	case ipc.ActionStop:
		resp = d.cmdStop(req.Name)

	case ipc.ActionRestart:
		resp = d.cmdRestart(req)

	case ipc.ActionReload:
		resp = d.cmdReload(req)

	case ipc.ActionScale:
		resp = d.cmdScale(req)

	case ipc.ActionDelete:
		resp = d.cmdDelete(req.Name)

	case ipc.ActionList:
		resp = d.cmdList()

	case ipc.ActionLogs:
		resp = d.cmdLogs(req)

	case ipc.ActionSave:
		if err := d.store.Save(); err != nil {
			resp = ipc.Response{OK: false, Error: err.Error()}
		} else {
			resp = ipc.Response{OK: true, Data: "process list saved"}
		}

	case ipc.ActionResurrect:
		resp = d.cmdResurrect()

	default:
		resp = ipc.Response{OK: false, Error: "unknown action"}
	}

	respond(conn, resp)
}

func respond(conn net.Conn, resp ipc.Response) {
	json.NewEncoder(conn).Encode(resp)
}

// ─── start ──────────────────────────────────────────────────────────────────

func (d *Daemon) cmdStart(req ipc.Request) ipc.Response {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Already registered?
	if existing, ok := d.store.Get(req.Name); ok {
		if d.anyRunningLocked(existing.Name) {
			return ipc.Response{OK: false, Error: fmt.Sprintf("process '%s' is already running", req.Name)}
		}
		if err := checkExecutable(existing.Binary); err != nil {
			return ipc.Response{OK: false, Error: err.Error()}
		}
		existing.Restarts = 0
		existing.InstanceStates = nil
		d.store.Set(existing)
		d.startInstancesLocked(existing)
		return ipc.Response{OK: true, Data: fmt.Sprintf("started '%s' (%d instance(s))", req.Name, existing.InstanceCount())}
	}

	// Refuse to register a service whose binary doesn't exist / isn't runnable,
	// rather than registering it and letting it crash-loop.
	if err := checkExecutable(req.Binary); err != nil {
		return ipc.Response{OK: false, Error: err.Error()}
	}

	p := buildServiceSpec(req)
	d.store.Set(p)
	d.startInstancesLocked(p)
	return ipc.Response{OK: true, Data: fmt.Sprintf("started '%s' (%d instance(s), mode=%s)", req.Name, p.InstanceCount(), p.Mode)}
}

// checkExecutable verifies the binary exists, is a regular file, and is
// executable — so `gpm start` fails fast with a clear message instead of
// registering a doomed service.
func checkExecutable(path string) error {
	if path == "" {
		return fmt.Errorf("no binary specified")
	}
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("binary not found: %s", path)
		}
		return fmt.Errorf("cannot access binary %s: %v", path, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("binary path is a directory: %s", path)
	}
	if fi.Mode()&0111 == 0 {
		return fmt.Errorf("binary is not executable: %s (try: chmod +x %s)", path, path)
	}
	return nil
}

// buildServiceSpec translates a start request into a service spec with defaults.
func buildServiceSpec(req ipc.Request) *state.Process {
	p := &state.Process{
		Name:        req.Name,
		Binary:      req.Binary,
		Args:        req.Args,
		Env:         req.Env,
		WorkDir:     req.WorkDir,
		Status:      state.StatusStarting,
		AutoRestart: true,
		MaxRestarts: 0, // unlimited
		Instances:   req.Instances,
		Host:        req.Host,
		Port:        req.Port,
		Mode:        req.Mode,
		PortEnv:     req.PortEnv,
		Watch:       req.Watch,
	}
	if req.WatchInterval > 0 {
		p.WatchIntervalMS = req.WatchInterval * 1000
	}
	if p.Env == nil {
		p.Env = make(map[string]string)
	}
	if p.Instances < 1 {
		p.Instances = 1
	}
	if p.Mode == "" {
		p.Mode = state.ModeReuseport
	}
	if req.HealthPath != "" {
		p.Health.Path = req.HealthPath
	} else if p.Port > 0 && p.Mode == state.ModeReuseport {
		// SDK serves an HTTP health endpoint; default to /healthz.
		p.Health.Path = "/healthz"
	}
	if p.Port > 0 {
		if req.ShutdownTimeout > 0 {
			p.ShutdownTimeoutMS = req.ShutdownTimeout * 1000
		} else {
			p.ShutdownTimeoutMS = 30000
		}
	}
	return p
}

// startInstancesLocked launches all instances of a freshly (re)started service.
// Caller must hold d.mu.
func (d *Daemon) startInstancesLocked(p *state.Process) {
	n := p.InstanceCount()
	runners := make([]*process.Runner, 0, n)
	for slot := 0; slot < n; slot++ {
		idx := d.allocIndexLocked(p.Name)
		cfg := d.buildInstanceConfig(p, idx, slot)
		d.store.SetInstanceState(p.Name, state.InstanceState{
			Index: idx, Slot: slot, Status: state.StatusStarting, HealthAddr: cfg.HealthAddr,
		})
		r := process.NewRunner(p, d.store, d.logDir, cfg)
		runners = append(runners, r)
		r.Start()
	}
	d.runners[p.Name] = runners
	d.syncProxyLocked(p)
	d.startWatcherLocked(p)
	d.store.Flush()
}

// buildInstanceConfig wires one instance: which slot it serves (GPM_INSTANCE),
// the address it should bind, and the unique address gpm will health-probe.
func (d *Daemon) buildInstanceConfig(p *state.Process, idx, slot int) process.InstanceConfig {
	cfg := process.InstanceConfig{Index: idx, Slot: slot, ExtraEnv: map[string]string{}}

	// proxy mode: each instance binds a unique internal port; the opaque binary
	// reads it from PortEnv (default PORT). gpm fronts the public port with a
	// proxy and probes the internal addr directly.
	if p.Mode == state.ModeProxy && p.Port > 0 {
		if addr, err := freeLoopbackAddr(); err == nil {
			cfg.ListenAddr = addr
			cfg.HealthAddr = addr // serves traffic and is probed here
			if _, port, err := net.SplitHostPort(addr); err == nil {
				portEnv := p.PortEnv
				if portEnv == "" {
					portEnv = "PORT"
				}
				cfg.ExtraEnv[portEnv] = port
			}
		}
		return cfg
	}

	if p.Port > 0 {
		cfg.ListenAddr = p.ListenAddr()
	}
	// reuseport mode: the SDK serves a health endpoint on a unique loopback
	// addr; gpm probes that to confirm THIS instance (a probe to the shared
	// SO_REUSEPORT port could be answered by the old instance).
	if p.Mode != state.ModeProxy && p.Port > 0 {
		if addr, err := freeLoopbackAddr(); err == nil {
			cfg.HealthAddr = addr
		}
	}
	return cfg
}

// syncProxyLocked (re)points a proxy-mode service's front proxy at its current
// instances, creating the proxy on first use. No-op for reuseport services.
// Caller must hold d.mu.
func (d *Daemon) syncProxyLocked(p *state.Process) {
	if p.Mode != state.ModeProxy || p.Port == 0 {
		return
	}
	ups := d.upstreamsLocked(p.Name)
	if pr, ok := d.proxies[p.Name]; ok {
		pr.SetUpstreams(ups)
		return
	}
	pr, err := proxy.New(p.ListenAddr(), ups)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gpm: failed to start proxy for %q on %s: %v\n", p.Name, p.ListenAddr(), err)
		return
	}
	d.proxies[p.Name] = pr
}

// upstreamsLocked returns the internal addrs of a service's current instances.
// Caller must hold d.mu.
func (d *Daemon) upstreamsLocked(name string) []string {
	var ups []string
	for _, r := range d.runners[name] {
		if a := r.HealthAddr(); a != "" {
			ups = append(ups, a)
		}
	}
	return ups
}

// stopProxyLocked closes and forgets a service's front proxy. Caller must hold
// d.mu.
func (d *Daemon) stopProxyLocked(name string) {
	if pr, ok := d.proxies[name]; ok {
		pr.Close()
		delete(d.proxies, name)
	}
}

// ─── watch mode ─────────────────────────────────────────────────────────────

// binSig is a cheap fingerprint of a binary used to detect rebuilds.
type binSig struct {
	size  int64
	mtime int64
	valid bool
}

func statBinary(path string) binSig {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return binSig{}
	}
	return binSig{size: fi.Size(), mtime: fi.ModTime().UnixNano(), valid: true}
}

// startWatcherLocked starts (or restarts) the binary watcher for a watch-mode
// service. Caller must hold d.mu.
func (d *Daemon) startWatcherLocked(p *state.Process) {
	if !p.Watch || p.Binary == "" {
		return
	}
	d.stopWatcherLocked(p.Name) // never run two watchers for one service
	interval := time.Duration(p.WatchIntervalMS) * time.Millisecond
	if interval <= 0 {
		interval = time.Second
	}
	stop := make(chan struct{})
	d.watchers[p.Name] = stop
	go d.watchBinary(p.Name, p.Binary, interval, stop)
}

// stopWatcherLocked stops a service's watcher if present. Caller must hold d.mu.
func (d *Daemon) stopWatcherLocked(name string) {
	if stop, ok := d.watchers[name]; ok {
		close(stop)
		delete(d.watchers, name)
	}
}

// watchBinary polls a service's binary and triggers a zero-downtime reload when
// it changes. It debounces: a change must read the same fingerprint for a few
// consecutive ticks before reloading, so a half-written or mid-rename binary
// (a `go build` writes a temp file then renames over the target) is never
// reloaded. The reload itself is async, so the watcher returns immediately.
func (d *Daemon) watchBinary(name, path string, interval time.Duration, stop chan struct{}) {
	const stableTicks = 2 // consecutive equal reads before a change is "settled"

	accepted := statBinary(path) // fingerprint of the currently-running binary
	var candidate binSig
	stable := 0

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			cur := statBinary(path)
			switch {
			case !cur.valid:
				// File briefly absent (mid-rename during a rebuild) — wait.
				candidate, stable = binSig{}, 0
			case cur == accepted:
				// Unchanged (or reverted before settling).
				candidate, stable = binSig{}, 0
			case cur == candidate:
				stable++
				if stable >= stableTicks {
					fmt.Fprintf(os.Stderr, "gpm: watch: %q binary changed, reloading\n", name)
					accepted = cur
					candidate, stable = binSig{}, 0
					if resp := d.cmdReload(ipc.Request{Action: ipc.ActionReload, Name: name}); !resp.OK {
						fmt.Fprintf(os.Stderr, "gpm: watch: reload of %q failed: %s\n", name, resp.Error)
					}
				}
			default:
				// New/changed fingerprint — start (or restart) the settle count.
				candidate, stable = cur, 1
			}
		}
	}
}

// ─── stop / restart / delete ────────────────────────────────────────────────

func (d *Daemon) cmdStop(name string) ipc.Response {
	d.mu.Lock()
	runners := d.runners[name]
	_, inStore := d.store.Get(name)
	d.stopWatcherLocked(name) // don't auto-reload a service we're stopping
	d.stopProxyLocked(name)   // stop accepting public traffic first
	d.mu.Unlock()

	if len(runners) == 0 && !inStore {
		return ipc.Response{OK: false, Error: fmt.Sprintf("process '%s' not found", name)}
	}
	for _, r := range runners {
		r.Stop()
	}
	return ipc.Response{OK: true, Data: fmt.Sprintf("stopped '%s'", name)}
}

// cmdRestart is the hard (with-downtime) restart: stop every instance, then
// start fresh. For zero downtime use reload.
func (d *Daemon) cmdRestart(req ipc.Request) ipc.Response {
	d.mu.Lock()
	runners := d.runners[req.Name]
	p, exists := d.store.Get(req.Name)
	d.mu.Unlock()

	if !exists {
		return ipc.Response{OK: false, Error: fmt.Sprintf("process '%s' not found", req.Name)}
	}
	for _, r := range runners {
		r.Stop()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	p.Restarts = 0
	p.InstanceStates = nil
	d.store.Set(p)
	d.startInstancesLocked(p)
	return ipc.Response{OK: true, Data: fmt.Sprintf("restarted '%s'", req.Name)}
}

func (d *Daemon) cmdDelete(name string) ipc.Response {
	d.mu.Lock()
	runners := d.runners[name]
	delete(d.runners, name)
	delete(d.nextIdx, name)
	d.stopWatcherLocked(name)
	d.stopProxyLocked(name)
	_, ok := d.store.Get(name)
	d.mu.Unlock()

	if !ok {
		return ipc.Response{OK: false, Error: fmt.Sprintf("process '%s' not found", name)}
	}
	for _, r := range runners {
		r.Stop()
	}
	d.store.Delete(name)
	d.store.Flush()
	return ipc.Response{OK: true, Data: fmt.Sprintf("deleted '%s'", name)}
}

// ─── reload (zero-downtime) ─────────────────────────────────────────────────

// cmdReload validates the request and kicks off the rolling reload in the
// background, returning immediately. Draining the old instances can take longer
// than any reasonable IPC deadline (in-flight requests may run for minutes), so
// the CLI must not block on it — progress is observable via `gpm list`, where
// old instances show as "draining" until they exit.
func (d *Daemon) cmdReload(req ipc.Request) ipc.Response {
	d.mu.Lock()
	p, ok := d.store.Get(req.Name)
	if !ok {
		d.mu.Unlock()
		return ipc.Response{OK: false, Error: fmt.Sprintf("process '%s' not found", req.Name)}
	}

	// Non-network service: nothing to overlap, fall back to hard restart.
	if p.Port == 0 {
		d.mu.Unlock()
		resp := d.cmdRestart(req)
		if resp.OK {
			resp.Data = fmt.Sprintf("'%s' has no port; hard-restarted (no zero-downtime possible)", req.Name)
		}
		return resp
	}

	if d.reloading[req.Name] {
		d.mu.Unlock()
		return ipc.Response{OK: false, Error: fmt.Sprintf("a reload of '%s' is already in progress", req.Name)}
	}

	current := append([]*process.Runner{}, d.runners[req.Name]...)

	// Not currently running: just start it (fast, no draining involved).
	if len(current) == 0 {
		p.InstanceStates = nil
		d.store.Set(p)
		d.startInstancesLocked(p)
		d.mu.Unlock()
		return ipc.Response{OK: true, Data: fmt.Sprintf("started '%s' (%d instance(s))", req.Name, p.InstanceCount())}
	}

	d.reloading[req.Name] = true
	d.mu.Unlock()

	go d.runReload(p, current)

	return ipc.Response{OK: true, Data: fmt.Sprintf(
		"reload started for '%s' (%d instance(s)) — new instances coming up, old ones draining in the background; watch `gpm list`",
		req.Name, len(current))}
}

// runReload performs the rolling, zero-downtime reload in the background. For
// each slot it brings up a replacement, waits (bounded) for it to pass its
// health check, routes traffic to it, then drains the old instance
// asynchronously so a slow drain never holds up the next slot or the caller.
func (d *Daemon) runReload(p *state.Process, current []*process.Runner) {
	var drains sync.WaitGroup
	defer func() {
		drains.Wait() // keep the service marked "reloading" until drains finish
		d.store.Flush()
		d.mu.Lock()
		delete(d.reloading, p.Name)
		d.mu.Unlock()
	}()

	sort.Slice(current, func(i, j int) bool { return current[i].Slot() < current[j].Slot() })

	for _, old := range current {
		slot := old.Slot()

		// 1. Start a replacement instance (new unique index, same slot).
		d.mu.Lock()
		idx := d.allocIndexLocked(p.Name)
		cfg := d.buildInstanceConfig(p, idx, slot)
		d.store.SetInstanceState(p.Name, state.InstanceState{
			Index: idx, Slot: slot, Status: state.StatusStarting, HealthAddr: cfg.HealthAddr,
		})
		nr := process.NewRunner(p, d.store, d.logDir, cfg)
		d.mu.Unlock()
		nr.Start()

		// 2. Wait for the NEW instance to become healthy (bounded by the health
		//    budget — this is the only thing the rolling loop blocks on).
		probeAddr, probeCfg := d.probeTarget(p, cfg)
		if err := health.WaitReady(context.Background(), probeAddr, probeCfg); err != nil {
			// Abort: tear down the new instance, leave everything serving.
			nr.Stop()
			d.store.RemoveInstanceState(p.Name, idx)
			d.removeInstanceLogs(p.Name, idx)
			fmt.Fprintf(os.Stderr, "gpm: reload of %q aborted at slot %d: new instance not healthy: %v (old instances still serving)\n", p.Name, slot, err)
			return
		}

		// 3. Healthy — route to the new instance and mark the old one draining.
		//    In proxy mode syncProxyLocked drops the old upstream here, so new
		//    traffic never hits the draining instance.
		d.mu.Lock()
		d.replaceRunnerLocked(p.Name, old, nr)
		d.syncProxyLocked(p)
		if st, ok := d.store.GetInstanceState(p.Name, old.Index()); ok {
			st.Status = state.StatusDraining
			d.store.SetInstanceState(p.Name, st)
		}
		d.mu.Unlock()
		d.store.Flush()

		// 4. Drain the old instance in the background so the rolling loop (and
		//    the long-since-returned CLI call) never waits on slow in-flight
		//    requests.
		drains.Add(1)
		go func(old *process.Runner) {
			defer drains.Done()
			old.Stop() // SIGTERM, then waits up to the drain budget
			d.store.RemoveInstanceState(p.Name, old.Index())
			d.removeInstanceLogs(p.Name, old.Index())
			d.store.Flush()
		}(old)
	}
}

// probeTarget returns the address and health config gpm should use to confirm a
// new instance is ready. In reuseport mode it probes the instance's unique SDK
// health addr; if that's unavailable it falls back to a TCP connect on the
// shared service port (best effort).
func (d *Daemon) probeTarget(p *state.Process, cfg process.InstanceConfig) (string, health.Config) {
	hc := healthConfig(p)
	if cfg.HealthAddr != "" {
		return cfg.HealthAddr, hc
	}
	// Fallback: TCP connect on the shared port.
	hc.Path = ""
	host := p.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, p.Port), hc
}

func healthConfig(p *state.Process) health.Config {
	c := health.Config{Path: p.Health.Path}
	if p.Health.IntervalMS > 0 {
		c.Interval = time.Duration(p.Health.IntervalMS) * time.Millisecond
	}
	if p.Health.TimeoutMS > 0 {
		c.Timeout = time.Duration(p.Health.TimeoutMS) * time.Millisecond
	}
	if p.Health.MaxWaitMS > 0 {
		c.MaxWait = time.Duration(p.Health.MaxWaitMS) * time.Millisecond
	} else {
		c.MaxWait = 15 * time.Second
	}
	return c
}

// ─── scale ──────────────────────────────────────────────────────────────────

func (d *Daemon) cmdScale(req ipc.Request) ipc.Response {
	target := req.Replicas
	if target < 1 {
		return ipc.Response{OK: false, Error: "scale target must be >= 1"}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	p, ok := d.store.Get(req.Name)
	if !ok {
		return ipc.Response{OK: false, Error: fmt.Sprintf("process '%s' not found", req.Name)}
	}

	current := d.runners[req.Name]
	cur := len(current)
	if cur == 0 {
		return ipc.Response{OK: false, Error: fmt.Sprintf("process '%s' is not running; start it first", req.Name)}
	}

	if target == cur {
		return ipc.Response{OK: true, Data: fmt.Sprintf("'%s' already at %d instance(s)", req.Name, target)}
	}

	if target > cur {
		// Scale up: add instances for the new slots.
		for slot := cur; slot < target; slot++ {
			idx := d.allocIndexLocked(p.Name)
			cfg := d.buildInstanceConfig(p, idx, slot)
			d.store.SetInstanceState(p.Name, state.InstanceState{
				Index: idx, Slot: slot, Status: state.StatusStarting, HealthAddr: cfg.HealthAddr,
			})
			r := process.NewRunner(p, d.store, d.logDir, cfg)
			d.runners[p.Name] = append(d.runners[p.Name], r)
			r.Start()
		}
	} else {
		// Scale down: stop the highest slots.
		sort.Slice(current, func(i, j int) bool { return current[i].Slot() < current[j].Slot() })
		keep := current[:target]
		drop := current[target:]
		for _, r := range drop {
			r.Stop()
			d.store.RemoveInstanceState(p.Name, r.Index())
			d.removeInstanceLogs(p.Name, r.Index())
		}
		d.runners[p.Name] = keep
	}

	d.syncProxyLocked(p)
	p.Instances = target
	d.store.Set(p)
	d.store.Flush()
	return ipc.Response{OK: true, Data: fmt.Sprintf("scaled '%s' from %d to %d instance(s)", req.Name, cur, target)}
}

// ─── list / logs ────────────────────────────────────────────────────────────

type ProcessInfo struct {
	Name      string `json:"name"`
	PID       int    `json:"pid"`
	Status    string `json:"status"`
	Restarts  int    `json:"restarts"`
	Uptime    string `json:"uptime"`
	Binary    string `json:"binary"`
	Mode      string `json:"mode"`
	Port      int    `json:"port"`
	Instances int    `json:"instances"`
	Healthy   int    `json:"healthy"`
	Draining  int    `json:"draining"` // old instances still finishing in-flight requests
	Watch     bool   `json:"watch"`
}

func (d *Daemon) cmdList() ipc.Response {
	procs := d.store.List()
	infos := make([]ProcessInfo, 0, len(procs))
	for _, p := range procs {
		running, draining, restarts, pid := 0, 0, 0, 0
		var earliest time.Time
		for _, st := range p.InstanceStates {
			restarts += st.Restarts
			switch st.Status {
			case state.StatusRunning:
				running++
				if pid == 0 {
					pid = st.PID
				}
				if earliest.IsZero() || st.StartedAt.Before(earliest) {
					earliest = st.StartedAt
				}
			case state.StatusDraining:
				draining++
			}
		}
		n := p.InstanceCount()
		status := aggregateStatus(p, running, n, draining)
		uptime := "-"
		if running > 0 && !earliest.IsZero() {
			uptime = formatUptime(earliest)
		}
		infos = append(infos, ProcessInfo{
			Name:      p.Name,
			PID:       pid,
			Status:    status,
			Restarts:  restarts,
			Uptime:    uptime,
			Binary:    p.Binary,
			Mode:      p.Mode,
			Port:      p.Port,
			Instances: n,
			Healthy:   running,
			Draining:  draining,
			Watch:     p.Watch,
		})
	}
	return ipc.Response{OK: true, Data: infos}
}

func aggregateStatus(p *state.Process, running, n, draining int) string {
	// A reload in flight (old instances still draining) takes precedence so it's
	// visible even while the new instances are already fully up.
	if draining > 0 {
		return "reloading"
	}
	switch {
	case running == n:
		return state.StatusRunning
	case running > 0:
		return state.StatusStarting // partially up
	default:
		// running == 0 — distinguish a crash-looping/coming-up service from one
		// the user actually stopped, so a failing service never reads "stopped".
		hasErrored, hasStarting := false, false
		for _, st := range p.InstanceStates {
			switch st.Status {
			case state.StatusErrored:
				hasErrored = true
			case state.StatusStarting:
				hasStarting = true
			}
		}
		switch {
		case hasErrored:
			return state.StatusErrored
		case hasStarting:
			return state.StatusStarting
		default:
			return state.StatusStopped
		}
	}
}

func (d *Daemon) cmdLogs(req ipc.Request) ipc.Response {
	p, ok := d.store.Get(req.Name)
	if !ok {
		return ipc.Response{OK: false, Error: fmt.Sprintf("process '%s' not found", req.Name)}
	}

	// Return every instance's log files so the CLI can tail them all.
	var outs, errs []string
	idxs := map[int]bool{}
	for _, st := range p.InstanceStates {
		idxs[st.Index] = true
	}
	// Also include any indices that exist on disk (covers just-rotated ones).
	for idx := range idxs {
		outs = append(outs, filepath.Join(d.logDir, fmt.Sprintf("%s-%d-out.log", req.Name, idx)))
		errs = append(errs, filepath.Join(d.logDir, fmt.Sprintf("%s-%d-err.log", req.Name, idx)))
	}
	sort.Strings(outs)
	sort.Strings(errs)

	type logPaths struct {
		Out  []string `json:"out"`
		Err  []string `json:"err"`
	}
	return ipc.Response{OK: true, Data: logPaths{Out: outs, Err: errs}}
}

// ─── resurrect ──────────────────────────────────────────────────────────────

func (d *Daemon) cmdResurrect() ipc.Response {
	procs, err := d.store.LoadSaved()
	if err != nil {
		return ipc.Response{OK: false, Error: fmt.Sprintf("no saved process list: %v", err)}
	}

	started := 0
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, p := range procs {
		p.Restarts = 0
		p.InstanceStates = nil
		d.store.Set(p)
		d.startInstancesLocked(p)
		started++
	}
	return ipc.Response{OK: true, Data: fmt.Sprintf("resurrected %d processes", started)}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func (d *Daemon) stopAll() {
	d.mu.Lock()
	all := make([][]*process.Runner, 0, len(d.runners))
	for _, rs := range d.runners {
		all = append(all, rs)
	}
	for name := range d.proxies {
		d.stopProxyLocked(name)
	}
	for name := range d.watchers {
		d.stopWatcherLocked(name)
	}
	d.mu.Unlock()
	for _, rs := range all {
		for _, r := range rs {
			r.Stop()
		}
	}
}

// allocIndexLocked returns a unique, monotonically increasing instance index
// for a service. Caller must hold d.mu.
func (d *Daemon) allocIndexLocked(name string) int {
	idx := d.nextIdx[name]
	d.nextIdx[name] = idx + 1
	return idx
}

// replaceRunnerLocked swaps old for new in a service's runner slice. Caller
// must hold d.mu.
func (d *Daemon) replaceRunnerLocked(name string, old, nw *process.Runner) {
	rs := d.runners[name]
	for i, r := range rs {
		if r == old {
			rs[i] = nw
			return
		}
	}
	// old not found (shouldn't happen) — append to keep the new one tracked.
	d.runners[name] = append(rs, nw)
}

func (d *Daemon) anyRunningLocked(name string) bool {
	for _, r := range d.runners[name] {
		_ = r
		return true
	}
	if p, ok := d.store.Get(name); ok {
		for _, st := range p.InstanceStates {
			if st.Status == state.StatusRunning || st.Status == state.StatusStarting {
				return true
			}
		}
	}
	return false
}

func (d *Daemon) removeInstanceLogs(name string, idx int) {
	os.Remove(filepath.Join(d.logDir, fmt.Sprintf("%s-%d-out.log", name, idx)))
	os.Remove(filepath.Join(d.logDir, fmt.Sprintf("%s-%d-err.log", name, idx)))
}

// freeLoopbackAddr returns an unused 127.0.0.1 address for an instance's health
// server. There's a tiny window between closing this listener and the child
// binding it, but the child binds immediately on startup.
func freeLoopbackAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}
