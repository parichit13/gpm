package process

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/parichit/gpm/internal/state"
)

const (
	MaxLogBytes     = 10 * 1024 * 1024 // 10MB per log file
	restartDelay    = 1 * time.Second
	defaultDrainSec = 5 // graceful-stop wait when no shutdown budget is configured

	// Crash-loop handling: a run shorter than minStableUptime counts as an
	// "unstable" exit. After crashLoopThreshold consecutive unstable exits the
	// instance is marked errored and restarts back off exponentially (up to
	// maxRestartBackoff) instead of hammering once per second.
	minStableUptime    = 3 * time.Second
	crashLoopThreshold = 4
	maxRestartBackoff  = 30 * time.Second
)

// crashBackoff returns the restart delay for an instance that has failed
// `failures` times in a row, growing exponentially past the threshold.
func crashBackoff(failures int) time.Duration {
	n := failures - crashLoopThreshold
	if n < 0 {
		return restartDelay
	}
	if n > 5 { // cap the shift to avoid overflow
		return maxRestartBackoff
	}
	d := restartDelay << uint(n)
	if d > maxRestartBackoff {
		return maxRestartBackoff
	}
	return d
}

// InstanceConfig is the per-instance wiring the daemon hands to a Runner: which
// index it is, the address to inject as GPM_LISTEN_ADDR, the unique health addr
// to inject and later probe, and any extra env (e.g. proxy-mode internal port).
type InstanceConfig struct {
	Index      int               // unique key for logs/state (monotonic across reloads)
	Slot       int               // logical slot 0..N-1, exposed to the service as GPM_INSTANCE
	ListenAddr string            // GPM_LISTEN_ADDR; empty for non-network services
	HealthAddr string            // GPM_HEALTH_ADDR; also probed by reload
	ExtraEnv   map[string]string // additional env vars to inject
}

type Runner struct {
	proc   *state.Process
	store  *state.Store
	logDir string
	cfg    InstanceConfig

	mu      sync.Mutex
	cmd     *exec.Cmd
	stopped bool // intentionally stopped, don't restart
}

func NewRunner(proc *state.Process, store *state.Store, logDir string, cfg InstanceConfig) *Runner {
	return &Runner{proc: proc, store: store, logDir: logDir, cfg: cfg}
}

// Index reports this runner's unique instance index (log/state key).
func (r *Runner) Index() int { return r.cfg.Index }

// Slot reports the logical instance slot (0..N-1) this runner serves; this is
// what the service sees as GPM_INSTANCE.
func (r *Runner) Slot() int { return r.cfg.Slot }

// HealthAddr reports the address gpm should probe to confirm this instance is
// ready (reuseport mode), or "" if the service has no SDK health server.
func (r *Runner) HealthAddr() string { return r.cfg.HealthAddr }

func (r *Runner) logFile(suffix string) string {
	return filepath.Join(r.logDir, fmt.Sprintf("%s-%d-%s.log", r.proc.Name, r.cfg.Index, suffix))
}

func (r *Runner) Start() error {
	r.mu.Lock()
	r.stopped = false
	r.mu.Unlock()
	go r.supervise()
	return nil
}

func (r *Runner) isStopped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopped
}

func (r *Runner) supervise() {
	failures := 0 // consecutive unstable (fast-exiting) runs
	for {
		if r.isStopped() {
			return
		}

		start := time.Now()
		_ = r.runOnce()
		ranFor := time.Since(start)

		if r.isStopped() {
			return
		}

		p, ok := r.store.Get(r.proc.Name)
		if !ok {
			return
		}

		if !p.AutoRestart {
			r.setInstanceStatus(state.StatusStopped)
			return
		}

		st, _ := r.store.GetInstanceState(p.Name, r.cfg.Index)
		if p.MaxRestarts > 0 && st.Restarts >= p.MaxRestarts {
			r.setInstanceStatus(state.StatusErrored)
			return
		}

		// The instance exited; schedule a restart. Track whether it stayed up
		// long enough to be considered stable — a string of fast exits is a
		// crash loop, which we surface as "errored" and slow down with backoff
		// so it neither hammers the machine nor masquerades as "stopped".
		if ranFor < minStableUptime {
			failures++
		} else {
			failures = 0
		}

		st.Index = r.cfg.Index
		st.Restarts++
		delay := restartDelay
		if failures >= crashLoopThreshold {
			st.Status = state.StatusErrored
			delay = crashBackoff(failures)
		} else {
			st.Status = state.StatusStarting
		}
		r.store.SetInstanceState(p.Name, st)
		r.store.Flush()
		time.Sleep(delay)
	}
}

func (r *Runner) runOnce() error {
	p, ok := r.store.Get(r.proc.Name)
	if !ok {
		return fmt.Errorf("process not found")
	}

	cmd := exec.Command(p.Binary, p.Args...)
	if p.WorkDir != "" {
		cmd.Dir = p.WorkDir
	}
	cmd.Env = r.buildEnv(p)

	// Log files (per instance)
	os.MkdirAll(r.logDir, 0755)
	outF, _ := os.OpenFile(r.logFile("out"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	errF, _ := os.OpenFile(r.logFile("err"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	defer func() {
		if outF != nil {
			outF.Close()
		}
		if errF != nil {
			errF.Close()
		}
	}()

	var outW, errW io.Writer = outF, errF
	if outF == nil {
		outW = os.Stdout
	}
	if errF == nil {
		errW = os.Stderr
	}

	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	r.mu.Lock()
	r.cmd = cmd
	r.mu.Unlock()

	// Preserve the restart counter; flip to running.
	st, _ := r.store.GetInstanceState(p.Name, r.cfg.Index)
	st.Index = r.cfg.Index
	st.HealthAddr = r.cfg.HealthAddr
	st.Status = state.StatusRunning
	runStart := time.Now()
	st.StartedAt = runStart
	st.StoppedAt = time.Time{}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(errW, "gpm: instance %d failed to start: %v\n", r.cfg.Index, err)
		st.Status = state.StatusErrored
		r.store.SetInstanceState(p.Name, st)
		r.store.Flush()
		return err
	}
	st.PID = cmd.Process.Pid
	r.store.SetInstanceState(p.Name, st)
	r.store.Flush()

	err := cmd.Wait()
	ran := time.Since(runStart).Round(time.Millisecond)

	r.mu.Lock()
	r.cmd = nil
	r.mu.Unlock()

	// Record why the instance exited, so `gpm logs` explains a crash even when
	// the binary itself printed nothing — e.g. it was killed at exec by the OS,
	// the classic macOS code-signing "signal: killed" with empty output.
	if err != nil {
		fmt.Fprintf(errW, "gpm: instance %d exited after %s: %v\n", r.cfg.Index, ran, err)
	} else {
		fmt.Fprintf(errW, "gpm: instance %d exited after %s (status 0)\n", r.cfg.Index, ran)
	}

	if st2, ok := r.store.GetInstanceState(p.Name, r.cfg.Index); ok {
		st2.StoppedAt = time.Now()
		st2.PID = 0
		r.store.SetInstanceState(p.Name, st2)
		r.store.Flush()
	}
	return err
}

// buildEnv assembles the child environment: inherited env, the service's own
// env, then the GPM_* variables the SDK reads.
func (r *Runner) buildEnv(p *state.Process) []string {
	env := os.Environ()
	for k, v := range p.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	env = append(env, "GPM_SERVICE="+p.Name)
	env = append(env, fmt.Sprintf("GPM_INSTANCE=%d", r.cfg.Slot))
	if r.cfg.ListenAddr != "" {
		env = append(env, "GPM_LISTEN_ADDR="+r.cfg.ListenAddr)
	}
	if r.cfg.HealthAddr != "" {
		env = append(env, "GPM_HEALTH_ADDR="+r.cfg.HealthAddr)
		if p.Health.Path != "" {
			env = append(env, "GPM_HEALTH_PATH="+p.Health.Path)
		}
	}
	if p.ShutdownTimeoutMS > 0 {
		env = append(env, fmt.Sprintf("GPM_SHUTDOWN_TIMEOUT=%dms", p.ShutdownTimeoutMS))
	}
	for k, v := range r.cfg.ExtraEnv {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	return env
}

func (r *Runner) setInstanceStatus(status string) {
	st, _ := r.store.GetInstanceState(r.proc.Name, r.cfg.Index)
	st.Index = r.cfg.Index
	st.Status = status
	st.PID = 0
	r.store.SetInstanceState(r.proc.Name, st)
	r.store.Flush()
}

// Stop gracefully terminates this instance: SIGTERM to the process group, wait
// for the drain budget, then SIGKILL. It marks the runner stopped so supervise
// won't restart it.
func (r *Runner) Stop() error {
	r.mu.Lock()
	r.stopped = true
	cmd := r.cmd
	r.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			syscall.Kill(-pgid, syscall.SIGTERM)
		} else {
			cmd.Process.Signal(syscall.SIGTERM)
		}
		done := make(chan struct{})
		go func() {
			cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(r.drainTimeout()):
			cmd.Process.Kill()
		}
	}

	r.setInstanceStatus(state.StatusStopped)
	return nil
}

// drainTimeout is how long Stop waits for a graceful exit before SIGKILL,
// honoring the service's configured shutdown budget.
func (r *Runner) drainTimeout() time.Duration {
	if p, ok := r.store.Get(r.proc.Name); ok && p.ShutdownTimeoutMS > 0 {
		// Allow a small grace beyond the service's own drain budget.
		return time.Duration(p.ShutdownTimeoutMS)*time.Millisecond + 2*time.Second
	}
	return defaultDrainSec * time.Second
}

func (r *Runner) LogFile(stream string) string {
	return r.logFile(stream)
}
