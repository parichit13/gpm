// Command opaque-server is a plain net/http service that does NOT import the
// gpm SDK — it represents an "opaque" binary you can't or won't modify. It only
// follows two conventions a process manager can rely on: it reads its listen
// port from $PORT, and it shuts down gracefully on SIGTERM.
//
// gpm runs it in proxy mode: gpm owns the public port and load-balances to a
// pool of these instances on private $PORT addresses, swapping the pool on
// reload/scale.
//
//	go build -o /tmp/opaque-server ./examples/opaque-server
//	gpm start /tmp/opaque-server web -i 3 --port 8080 --mode proxy --health-path /healthz
//	gpm reload web    # zero-downtime, via the front proxy
//
// Run ./demo.sh for a guided tour of the proxy-mode scenarios.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

// Version is set at build time with -ldflags "-X main.Version=v2" or via the
// VERSION env var.
var Version = "v1"

func main() {
	if v := os.Getenv("VERSION"); v != "" {
		Version = v
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	// gpm's proxy dials 127.0.0.1:<port>, so bind loopback.
	addr := "127.0.0.1:" + port

	// Readiness: serve traffic immediately, but report unready on /healthz until
	// warmup elapses. In proxy mode gpm only adds an instance to the load
	// balancer once /healthz passes, so warmup genuinely gates traffic.
	var ready atomic.Bool
	ready.Store(true)
	if w := envMS("WARMUP_MS"); w > 0 {
		ready.Store(false)
		go func() {
			time.Sleep(w)
			ready.Store(true)
			fmt.Printf("opaque %s (instance %s) warmed up, now ready\n", Version, instance())
		}()
	}

	started := time.Now()
	inst := instance()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":%q,"instance":%q,"pid":%d,"port":%q,"uptime":%q}`+"\n",
			Version, inst, os.Getpid(), port, time.Since(started).Round(time.Millisecond).String())
	})
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		if ms == 0 {
			ms = 1000
		}
		time.Sleep(time.Duration(ms) * time.Millisecond)
		fmt.Fprintf(w, `{"version":%q,"instance":%q,"slept_ms":%d}`+"\n", Version, inst, ms)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ok\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/crash", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"crashing":true,"instance":%q,"version":%q}`+"\n", inst, Version)
		go func() {
			time.Sleep(150 * time.Millisecond) // let the response flush
			fmt.Printf("opaque %s (instance %s) crashing on request\n", Version, inst)
			os.Exit(1)
		}()
	})

	srv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		fmt.Printf("opaque %s listening on %s (instance %s, pid %d)\n", Version, addr, inst, os.Getpid())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "listen: %v\n", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown: the contract that makes draining work. gpm passes the
	// drain budget via GPM_SHUTDOWN_TIMEOUT.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), shutdownBudget())
	defer cancel()
	srv.Shutdown(ctx)
	fmt.Printf("opaque %s (instance %s) drained and exited cleanly\n", Version, inst)
}

func instance() string {
	if v := os.Getenv("GPM_INSTANCE"); v != "" {
		return v
	}
	return "0"
}

func envMS(key string) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 0
}

func shutdownBudget() time.Duration {
	if v := os.Getenv("GPM_SHUTDOWN_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}
