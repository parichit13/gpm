// Package gpm is the SDK that Go services import to get zero-downtime reloads
// under the gpm process manager.
//
// A minimal HTTP service looks like:
//
//	func main() {
//	    mux := http.NewServeMux()
//	    mux.HandleFunc("/", handler)
//	    srv := &http.Server{Handler: mux}
//
//	    ln, err := gpm.Listen(":8080")
//	    if err != nil {
//	        log.Fatal(err)
//	    }
//	    log.Fatal(gpm.Serve(srv, ln))
//	}
//
// When launched by gpm, Listen binds the gpm-assigned address with SO_REUSEPORT
// (so a new instance can come up on the same port before the old one drains)
// and Serve installs a SIGTERM handler that gracefully shuts the server down.
// When run directly (outside gpm) it falls back to the default address, so the
// same binary works in development and under gpm unchanged.
package gpm

import (
	"context"
	"net"
	"os"
)

// Environment variables gpm injects into each managed instance.
const (
	EnvService     = "GPM_SERVICE"          // service name
	EnvInstance    = "GPM_INSTANCE"         // instance index (0-based)
	EnvListenAddr  = "GPM_LISTEN_ADDR"      // host:port the service should bind
	EnvHealthAddr  = "GPM_HEALTH_ADDR"      // per-instance unique addr for health probes
	EnvHealthPath  = "GPM_HEALTH_PATH"      // path the health server should answer on
	EnvShutdown    = "GPM_SHUTDOWN_TIMEOUT" // graceful drain budget, e.g. "30s"
)

// Listen returns a TCP listener for the service.
//
// If gpm set GPM_LISTEN_ADDR, that address is used; otherwise defaultAddr is
// used (so the binary runs the same way outside gpm). The socket is created
// with SO_REUSEPORT, which is what allows old and new instances to share the
// port during a reload.
func Listen(defaultAddr string) (net.Listener, error) {
	addr := os.Getenv(EnvListenAddr)
	if addr == "" {
		addr = defaultAddr
	}
	lc := net.ListenConfig{Control: reusePortControl}
	return lc.Listen(context.Background(), "tcp", addr)
}

// ListenAddr reports the address Listen will bind: the gpm-assigned address if
// present, else defaultAddr. Useful for logging.
func ListenAddr(defaultAddr string) string {
	if addr := os.Getenv(EnvListenAddr); addr != "" {
		return addr
	}
	return defaultAddr
}

// Instance reports the gpm instance index for this process (0 if unmanaged).
func Instance() string {
	if v := os.Getenv(EnvInstance); v != "" {
		return v
	}
	return "0"
}
