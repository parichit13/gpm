// Command example is a tiny HTTP service that uses the gpm SDK. It exists to
// demonstrate and verify zero-downtime reloads.
//
// Build it, then run under gpm:
//
//	go build -o /tmp/gpm-example ./pkg/gpm/example
//	gpm start /tmp/gpm-example api -i 3 --port 8080 --health-path /healthz
//
// Each response reports the build VERSION and the gpm instance index, so a
// reload's instance-by-instance rollover is visible. Set VERSION at build time
// with -ldflags "-X main.Version=v2" or at run time with the VERSION env var.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/parichit13/gpm/pkg/gpm"
)

// Version is overridable at build time via -ldflags "-X main.Version=...".
var Version = "v1"

func main() {
	if v := os.Getenv("VERSION"); v != "" {
		Version = v
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Simulate a little work so in-flight requests must be drained, not
		// dropped, on reload.
		time.Sleep(20 * time.Millisecond)
		fmt.Fprintf(w, "version=%s instance=%s\n", Version, gpm.Instance())
	})

	srv := &http.Server{Handler: mux}

	ln, err := gpm.Listen(":8080")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("example %s serving on %s (instance %s)", Version, gpm.ListenAddr(":8080"), gpm.Instance())
	if err := gpm.Serve(srv, ln); err != nil {
		log.Fatalf("serve: %v", err)
	}
	log.Printf("example %s (instance %s) shut down cleanly", Version, gpm.Instance())
}
