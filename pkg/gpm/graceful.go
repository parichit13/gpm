package gpm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

const defaultShutdownTimeout = 30 * time.Second

// ready gates the health endpoint. It starts false so gpm's readiness probe
// only passes once the service is actually serving; Serve flips it true right
// before it begins accepting connections, UNLESS the app is managing readiness
// itself (see readyManaged). Call SetReady(false) to drain.
var ready atomic.Bool

// readyManaged records whether the app has taken control of readiness via
// Ready()/SetReady(). When it has, Serve won't override the app's choice — this
// is what lets a service stay unready during a slow warmup so gpm holds the
// reload (and keeps the old instance serving) until warmup completes.
var readyManaged atomic.Bool

// Ready marks this instance as ready to serve (health endpoint returns 200) and
// takes over readiness management from Serve.
func Ready() {
	readyManaged.Store(true)
	ready.Store(true)
}

// SetReady sets the readiness state explicitly and takes over readiness
// management from Serve. Set false during a slow warmup and call Ready() once
// initialization finishes; gpm will hold the reload until the health probe
// passes (or abort it if warmup exceeds the health budget).
func SetReady(v bool) {
	readyManaged.Store(true)
	ready.Store(v)
}

// Serve runs an *http.Server on ln and blocks until the process receives
// SIGTERM/SIGINT, at which point it gracefully shuts the server down within the
// gpm shutdown budget (GPM_SHUTDOWN_TIMEOUT, default 30s). It also starts the
// per-instance health server gpm probes during a reload. Returns nil on a clean
// graceful shutdown.
func Serve(srv *http.Server, ln net.Listener) error {
	stopHealth := startHealthServer()
	defer stopHealth()

	serveErr := make(chan error, 1)
	go func() {
		if !readyManaged.Load() {
			ready.Store(true)
		}
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)

	select {
	case err := <-serveErr:
		// Server stopped on its own (bind failure, etc.) before any signal.
		return err
	case <-sig:
		ready.Store(false) // fail health probes immediately so we stop receiving new traffic
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout())
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			// Drain budget exceeded: force-close remaining connections.
			_ = srv.Close()
			return err
		}
		return nil
	}
}

// WaitForShutdown blocks until SIGTERM/SIGINT, then calls onShutdown with a
// context carrying the gpm drain budget. Use this for non-HTTP services that
// manage their own listener accept loop. The health endpoint reports ready
// until the signal arrives.
func WaitForShutdown(onShutdown func(context.Context)) {
	stopHealth := startHealthServer()
	defer stopHealth()
	if !readyManaged.Load() {
		ready.Store(true)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	ready.Store(false)

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout())
	defer cancel()
	if onShutdown != nil {
		onShutdown(ctx)
	}
}

// startHealthServer launches the per-instance health server on GPM_HEALTH_ADDR
// (a unique loopback address gpm assigns to this instance, distinct from the
// shared service port). gpm probes this address to confirm THIS specific
// instance is up before draining the old one during a reload — probing the
// shared SO_REUSEPORT port wouldn't work, since the kernel could route the
// probe to the old instance. Returns a stop func; a no-op when unmanaged.
func startHealthServer() func() {
	addr := os.Getenv(EnvHealthAddr)
	if addr == "" {
		return func() {}
	}
	path := os.Getenv(EnvHealthPath)
	if path == "" {
		path = "/healthz"
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	hs := &http.Server{Addr: addr, Handler: mux}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Can't bind the health addr; gpm will fall back to TCP probing the
		// service port. Don't take the service down over this.
		return func() {}
	}
	go func() { _ = hs.Serve(ln) }()

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = hs.Shutdown(ctx)
	}
}

func shutdownTimeout() time.Duration {
	if v := os.Getenv(EnvShutdown); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultShutdownTimeout
}
