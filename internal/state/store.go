package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const (
	StatusRunning  = "running"
	StatusStopped  = "stopped"
	StatusErrored  = "errored"
	StatusStarting = "starting"
	StatusDraining = "draining" // old instance finishing in-flight requests during a reload
)

// Reload modes.
const (
	ModeReuseport = "reuseport" // service imports the gpm SDK and binds with SO_REUSEPORT
	ModeProxy     = "proxy"     // opaque binary; gpm proxies a front port to internal instances
)

// Health describes how gpm probes an instance for readiness during reload.
type Health struct {
	Path       string `json:"path,omitempty"`        // HTTP path; empty = TCP-connect probe
	IntervalMS int    `json:"interval_ms,omitempty"`  // delay between attempts
	TimeoutMS  int    `json:"timeout_ms,omitempty"`   // per-attempt timeout
	MaxWaitMS  int    `json:"max_wait_ms,omitempty"`  // total budget before giving up
}

// InstanceState is the runtime of a single instance of a service. It is the
// source of truth for per-instance status so that N instance runners never race
// on shared top-level fields. Not persisted to the resurrect file.
type InstanceState struct {
	Index      int       `json:"index"`                 // unique key (monotonic across reloads)
	Slot       int       `json:"slot"`                  // logical slot 0..N-1 (GPM_INSTANCE)
	PID        int       `json:"pid"`
	Status     string    `json:"status"`
	Restarts   int       `json:"restarts"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at,omitempty"`
	HealthAddr string    `json:"health_addr,omitempty"` // per-instance addr gpm probes
}

type Process struct {
	ID       int               `json:"id"` // stable integer handle, usable in place of the name
	Name     string            `json:"name"`
	Binary   string            `json:"binary"`
	Args     []string          `json:"args"`
	Env      map[string]string `json:"env"`
	WorkDir  string            `json:"work_dir"`

	// Runtime (not saved to resurrect file, rebuilt on resurrect)
	PID        int       `json:"pid"`
	Status     string    `json:"status"`
	Restarts   int       `json:"restarts"`
	StartedAt  time.Time `json:"started_at"`
	StoppedAt  time.Time `json:"stopped_at,omitempty"`

	// Config
	MaxRestarts int  `json:"max_restarts"` // 0 = unlimited
	AutoRestart bool `json:"auto_restart"`

	// Service / cluster config (zero-downtime reload)
	Instances         int    `json:"instances,omitempty"`           // number of instances (default 1)
	Host              string `json:"host,omitempty"`                // bind host (default all interfaces)
	Port              int    `json:"port,omitempty"`                // shared listen port; 0 = non-network
	Mode              string `json:"mode,omitempty"`                // ModeReuseport | ModeProxy
	Health            Health `json:"health,omitempty"`              // readiness probe config
	ShutdownTimeoutMS int    `json:"shutdown_timeout_ms,omitempty"` // graceful drain budget
	PortEnv           string `json:"port_env,omitempty"`            // proxy mode: env var for the internal port
	Watch             bool   `json:"watch,omitempty"`               // auto-reload when the binary changes
	WatchIntervalMS   int    `json:"watch_interval_ms,omitempty"`   // poll interval for watch mode

	// Per-instance runtime (not persisted to resurrect file)
	InstanceStates []InstanceState `json:"instance_states,omitempty"`
}

// InstanceCount returns the configured instance count, treating 0 as 1.
func (p *Process) InstanceCount() int {
	if p.Instances < 1 {
		return 1
	}
	return p.Instances
}

// ListenAddr returns the host:port the service binds (e.g. ":8080").
func (p *Process) ListenAddr() string {
	return fmt.Sprintf("%s:%d", p.Host, p.Port)
}

type Store struct {
	mu        sync.RWMutex
	processes map[string]*Process
	stateDir  string
}

func NewStore(stateDir string) (*Store, error) {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, err
	}
	s := &Store{
		processes: make(map[string]*Process),
		stateDir:  stateDir,
	}
	// Load existing state if present
	_ = s.load()
	return s, nil
}

func (s *Store) stateFile() string  { return filepath.Join(s.stateDir, "state.json") }
func (s *Store) saveFile() string   { return filepath.Join(s.stateDir, "save.json") }

func (s *Store) load() error {
	data, err := os.ReadFile(s.stateFile())
	if err != nil {
		return err
	}
	var procs []*Process
	if err := json.Unmarshal(data, &procs); err != nil {
		return err
	}
	for _, p := range procs {
		s.processes[p.Name] = p
	}
	return nil
}

func (s *Store) Flush() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.writeFile(s.stateFile())
}

func (s *Store) writeFile(path string) error {
	procs := make([]*Process, 0, len(s.processes))
	for _, p := range s.processes {
		procs = append(procs, p)
	}
	data, err := json.MarshalIndent(procs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func (s *Store) Save() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Save only running/startable processes (strip runtime PID)
	procs := make([]*Process, 0)
	for _, p := range s.processes {
		copy := *p
		copy.PID = 0
		copy.Status = StatusStopped
		copy.InstanceStates = nil // runtime; rebuilt on resurrect
		procs = append(procs, &copy)
	}
	data, err := json.MarshalIndent(procs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.saveFile(), data, 0644)
}

func (s *Store) LoadSaved() ([]*Process, error) {
	data, err := os.ReadFile(s.saveFile())
	if err != nil {
		return nil, err
	}
	var procs []*Process
	if err := json.Unmarshal(data, &procs); err != nil {
		return nil, err
	}
	return procs, nil
}

func (s *Store) Set(p *Process) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.processes[p.Name] = p
}

func (s *Store) Get(name string) (*Process, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.processes[name]
	return p, ok
}

// NextFreeID returns the lowest non-negative integer not currently used as a
// service ID, so IDs stay small and gaps from deletions get reused (PM2-style).
func (s *Store) NextFreeID() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	used := make(map[int]bool, len(s.processes))
	for _, p := range s.processes {
		used[p.ID] = true
	}
	for id := 0; ; id++ {
		if !used[id] {
			return id
		}
	}
}

// Resolve looks a process up by integer ID (if idOrName is all digits) or by
// name, returning the canonical name so callers can proceed uniformly.
func (s *Store) Resolve(idOrName string) (*Process, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id, err := strconv.Atoi(idOrName); err == nil {
		for _, p := range s.processes {
			if p.ID == id {
				return p, true
			}
		}
		return nil, false
	}
	p, ok := s.processes[idOrName]
	return p, ok
}

func (s *Store) Delete(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.processes, name)
}

func (s *Store) List() []*Process {
	s.mu.RLock()
	defer s.mu.RUnlock()
	procs := make([]*Process, 0, len(s.processes))
	for _, p := range s.processes {
		procs = append(procs, p)
	}
	return procs
}

// SetInstanceState upserts the runtime of a single instance (keyed by Index)
// under lock, so concurrent instance runners don't race on the slice.
func (s *Store) SetInstanceState(name string, st InstanceState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.processes[name]
	if !ok {
		return
	}
	for i := range p.InstanceStates {
		if p.InstanceStates[i].Index == st.Index {
			p.InstanceStates[i] = st
			return
		}
	}
	p.InstanceStates = append(p.InstanceStates, st)
}

// GetInstanceState returns a copy of an instance's runtime.
func (s *Store) GetInstanceState(name string, idx int) (InstanceState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.processes[name]
	if !ok {
		return InstanceState{}, false
	}
	for _, st := range p.InstanceStates {
		if st.Index == idx {
			return st, true
		}
	}
	return InstanceState{}, false
}

// RemoveInstanceState drops an instance's runtime (used by scale-down).
func (s *Store) RemoveInstanceState(name string, idx int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.processes[name]
	if !ok {
		return
	}
	out := p.InstanceStates[:0]
	for _, st := range p.InstanceStates {
		if st.Index != idx {
			out = append(out, st)
		}
	}
	p.InstanceStates = out
}
