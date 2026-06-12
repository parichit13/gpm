// Package health probes whether a service instance is ready to serve. It is
// used by the daemon's reload to confirm a freshly started instance is up
// before draining the instance it replaces.
package health

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"
)

// Config controls how an instance is probed for readiness.
type Config struct {
	Path       string        // HTTP path to GET; empty means TCP-connect only
	Interval   time.Duration // delay between attempts
	Timeout    time.Duration // per-attempt timeout
	MaxWait    time.Duration // total budget before WaitReady gives up
}

// Defaults fills any zero-valued field with a sensible default.
func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 200 * time.Millisecond
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Second
	}
	if c.MaxWait <= 0 {
		c.MaxWait = 10 * time.Second
	}
	return c
}

// Check performs a single readiness probe against addr. When path is non-empty
// it issues an HTTP GET and requires a 2xx response; otherwise it succeeds as
// soon as a TCP connection to addr is accepted.
func Check(addr, path string, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if path == "" {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}

	client := &http.Client{Timeout: timeout}
	// Tolerate a path given without a leading slash (e.g. "healthz") so the URL
	// doesn't become "http://host:porthealthz".
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	url := "http://" + addr + path
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// WaitReady polls addr until it passes a readiness Check or the budget in cfg
// (or ctx) is exhausted. It returns nil once the instance is ready, or a
// context error / DeadlineExceeded on timeout.
func WaitReady(ctx context.Context, addr string, cfg Config) error {
	cfg = cfg.withDefaults()

	ctx, cancel := context.WithTimeout(ctx, cfg.MaxWait)
	defer cancel()

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()

	// Probe immediately, then on each tick.
	for {
		if Check(addr, cfg.Path, cfg.Timeout) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
