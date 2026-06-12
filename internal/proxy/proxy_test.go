package proxy

import (
	"bufio"
	"fmt"
	"net"
	"testing"
	"time"
)

// echoServer starts a TCP server that responds to a one-line request with a
// fixed id, so tests can tell which backend served a connection.
func echoServer(t *testing.T, id string) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, "%s\n", id)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func request(t *testing.T, addr string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return "ERR"
	}
	defer c.Close()
	fmt.Fprintf(c, "ping\n")
	c.SetReadDeadline(time.Now().Add(time.Second))
	line, _ := bufio.NewReader(c).ReadString('\n')
	return line
}

func TestProxyRoundRobins(t *testing.T) {
	a, stopA := echoServer(t, "A")
	defer stopA()
	b, stopB := echoServer(t, "B")
	defer stopB()

	p, err := New("127.0.0.1:0", []string{a, b})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	front := p.ln.Addr().String()

	seen := map[string]int{}
	for i := 0; i < 10; i++ {
		seen[request(t, front)]++
	}
	if seen["A\n"] == 0 || seen["B\n"] == 0 {
		t.Fatalf("expected both backends to serve, got %v", seen)
	}
}

func TestProxyRetriesPastDeadUpstream(t *testing.T) {
	good, stopGood := echoServer(t, "GOOD")
	defer stopGood()

	// Reserve a port then close it so dials are refused — a stand-in for an
	// instance mid-drain.
	dl, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := dl.Addr().String()
	dl.Close()

	p, err := New("127.0.0.1:0", []string{dead, good})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	front := p.ln.Addr().String()

	// Every request must succeed despite one upstream being dead.
	for i := 0; i < 10; i++ {
		if got := request(t, front); got != "GOOD\n" {
			t.Fatalf("request %d: expected GOOD, got %q", i, got)
		}
	}
}

func TestProxySwapUpstreams(t *testing.T) {
	a, stopA := echoServer(t, "A")
	defer stopA()
	b, stopB := echoServer(t, "B")
	defer stopB()

	p, err := New("127.0.0.1:0", []string{a})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()
	front := p.ln.Addr().String()

	if got := request(t, front); got != "A\n" {
		t.Fatalf("expected A, got %q", got)
	}
	p.SetUpstreams([]string{b})
	if got := request(t, front); got != "B\n" {
		t.Fatalf("after swap expected B, got %q", got)
	}
}
