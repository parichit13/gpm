package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitReadyGatesOnHTTPStatus(t *testing.T) {
	var ready atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ready.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	// Not ready yet → should time out.
	err := WaitReady(context.Background(), addr, Config{Path: "/healthz", Interval: 20 * time.Millisecond, MaxWait: 200 * time.Millisecond})
	if err == nil {
		t.Fatal("expected timeout while unhealthy, got nil")
	}

	// Flip to ready, then it should pass quickly.
	ready.Store(true)
	if err := WaitReady(context.Background(), addr, Config{Path: "/healthz", Interval: 20 * time.Millisecond, MaxWait: time.Second}); err != nil {
		t.Fatalf("expected ready, got %v", err)
	}
}

func TestWaitReadyTCPFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	// Empty path → TCP connect probe should succeed against an open port.
	if err := WaitReady(context.Background(), ln.Addr().String(), Config{MaxWait: time.Second}); err != nil {
		t.Fatalf("expected TCP probe to pass, got %v", err)
	}

	// A closed port should time out.
	dl, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := dl.Addr().String()
	dl.Close()
	if err := WaitReady(context.Background(), closed, Config{Interval: 20 * time.Millisecond, MaxWait: 200 * time.Millisecond}); err == nil {
		t.Fatal("expected timeout against closed port, got nil")
	}
}
