// Command gin-server is a Gin (gin-gonic) HTTP service wired up with the gpm
// SDK. It exists to exercise every gpm capability end-to-end:
//
//   - zero-downtime reload    — see the version flip with no dropped requests
//   - request draining        — GET /work?ms=N holds a request open across a reload
//   - failover + auto-restart — GET /crash kills an instance; peers keep serving
//   - cluster mode            — run N instances behind one port
//   - health-gated reload     — WARMUP_MS keeps an instance unready until warm
//
// Run it under gpm:
//
//	go build -o /tmp/gin-server .
//	gpm start /tmp/gin-server gin -i 3 --port 8080 --health-path /healthz
//	gpm reload gin     # zero-downtime
//
// Or just run ./demo.sh for a guided tour of all the scenarios.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/parichit/gpm/pkg/gpm"
)

// Version is set at build time with -ldflags "-X main.Version=v2", or at run
// time with the VERSION env var. The demo flips it to show reloads rolling over.
var Version = "v1"

func main() {
	if v := os.Getenv("VERSION"); v != "" {
		Version = v
	}

	// Optional warmup: stay NOT-ready until it elapses. gpm probes the SDK's
	// health endpoint and won't drain the old instance until this one reports
	// ready — so a reload visibly waits out the warmup (and aborts, leaving the
	// old instance serving, if warmup exceeds gpm's health budget).
	if warmup := envDuration("WARMUP_MS"); warmup > 0 {
		gpm.SetReady(false)
		go func() {
			time.Sleep(warmup)
			gpm.Ready()
			fmt.Printf("gin-server %s (instance %s) warmed up, now ready\n", Version, gpm.Instance())
		}()
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	started := time.Now()
	instance := gpm.Instance()

	info := func() gin.H {
		host, _ := os.Hostname()
		return gin.H{
			"version":  Version,
			"instance": instance,
			"pid":      os.Getpid(),
			"host":     host,
			"uptime":   time.Since(started).Round(time.Millisecond).String(),
		}
	}

	// Fast endpoint — used by the load probe to detect dropped requests.
	r.GET("/", func(c *gin.Context) { c.JSON(http.StatusOK, info()) })

	// Slow endpoint for REQUEST DRAINING: start GET /work?ms=8000, trigger a
	// reload, and watch this request still complete (with the OLD version)
	// because gpm SIGTERMs the instance and the SDK drains in-flight requests
	// before exiting.
	r.GET("/work", func(c *gin.Context) {
		ms, _ := strconv.Atoi(c.DefaultQuery("ms", "1000"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		resp := info()
		resp["slept_ms"] = ms
		c.JSON(http.StatusOK, resp)
	})

	// Manual health view. Note: gpm probes the SDK's own health server on
	// GPM_HEALTH_ADDR (a separate per-instance port), not this route — this is
	// just here so you can inspect readiness by hand.
	r.GET("/healthz", func(c *gin.Context) {
		c.String(http.StatusOK, "ok version=%s instance=%s\n", Version, instance)
	})

	// FAILOVER / AUTO-RESTART: crash this instance. With N instances the kernel
	// (reuseport) routes around the dead one while gpm auto-restarts it. The
	// response flushes first, then the process exits non-zero.
	r.GET("/crash", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"crashing": true, "instance": instance, "version": Version})
		go func() {
			time.Sleep(150 * time.Millisecond) // let the response flush
			fmt.Printf("gin-server %s (instance %s) crashing on request\n", Version, instance)
			os.Exit(1)
		}()
	})

	// Inspect the wiring gpm injected into this instance.
	r.GET("/env", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"GPM_SERVICE":          os.Getenv("GPM_SERVICE"),
			"GPM_INSTANCE":         os.Getenv("GPM_INSTANCE"),
			"GPM_LISTEN_ADDR":      os.Getenv("GPM_LISTEN_ADDR"),
			"GPM_HEALTH_ADDR":      os.Getenv("GPM_HEALTH_ADDR"),
			"GPM_HEALTH_PATH":      os.Getenv("GPM_HEALTH_PATH"),
			"GPM_SHUTDOWN_TIMEOUT": os.Getenv("GPM_SHUTDOWN_TIMEOUT"),
		})
	})

	srv := &http.Server{Handler: r}

	ln, err := gpm.Listen(":8080")
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("gin-server %s listening on %s (instance %s, pid %d)\n",
		Version, gpm.ListenAddr(":8080"), instance, os.Getpid())

	// gpm.Serve runs the server and, on SIGTERM, gracefully drains in-flight
	// requests within GPM_SHUTDOWN_TIMEOUT before returning.
	if err := gpm.Serve(srv, ln); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("gin-server %s (instance %s) drained and exited cleanly\n", Version, instance)
}

func envDuration(key string) time.Duration {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Millisecond
		}
	}
	return 0
}
