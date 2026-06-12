// Package proxy is a small TCP load balancer used for proxy-mode services —
// opaque binaries that can't import the gpm SDK. The proxy owns the public
// port; each service instance runs on a private internal port. Because the
// public listener stays open while the upstream set is swapped atomically, a
// reload or scale never closes the port: in-flight connections finish and new
// connections route only to healthy instances.
package proxy

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Proxy forwards TCP connections from a front address to a set of upstreams,
// round-robin. The upstream set can be replaced at any time without dropping
// the listener.
type Proxy struct {
	ln        net.Listener
	upstreams atomic.Value // []string
	rr        uint64

	mu     sync.Mutex
	closed bool
}

// New starts a proxy listening on frontAddr forwarding to upstreams. The
// listener is live as soon as New returns.
func New(frontAddr string, upstreams []string) (*Proxy, error) {
	ln, err := net.Listen("tcp", frontAddr)
	if err != nil {
		return nil, err
	}
	p := &Proxy{ln: ln}
	p.SetUpstreams(upstreams)
	go p.acceptLoop()
	return p, nil
}

// SetUpstreams atomically replaces the set of backend addresses.
func (p *Proxy) SetUpstreams(u []string) {
	cp := make([]string, len(u))
	copy(cp, u)
	p.upstreams.Store(cp)
}

// Upstreams returns the current backend set.
func (p *Proxy) Upstreams() []string {
	if v, ok := p.upstreams.Load().([]string); ok {
		return v
	}
	return nil
}

func (p *Proxy) acceptLoop() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go p.handle(conn)
	}
}

func (p *Proxy) handle(client net.Conn) {
	defer client.Close()

	upstream := p.dial()
	if upstream == nil {
		return
	}
	defer upstream.Close()

	// Splice both directions; return once either side finishes.
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// dial connects to an upstream, trying each one (starting round-robin) until a
// connection succeeds. Retrying past a failed dial is what makes reload truly
// zero-downtime: if we pick an instance that's mid-drain (removed from the set
// but its listener just closed), we fall through to a healthy one. It's safe
// because no client bytes have been forwarded yet.
func (p *Proxy) dial() net.Conn {
	us, _ := p.upstreams.Load().([]string)
	if len(us) == 0 {
		return nil
	}
	start := int(atomic.AddUint64(&p.rr, 1))
	for i := 0; i < len(us); i++ {
		addr := us[(start+i)%len(us)]
		if c, err := net.DialTimeout("tcp", addr, 5*time.Second); err == nil {
			return c
		}
	}
	return nil
}

// Close stops the proxy listener. In-flight connections are not forcibly cut.
func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	return p.ln.Close()
}
