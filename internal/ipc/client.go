package ipc

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// EnsureDaemon, if set, is invoked when a request can't reach the daemon. It
// should start the daemon and return nil once it's up; Send then retries once.
// This lets any gpm command transparently bring the daemon up (after install or
// a reboot) without the user running `gpm daemon start` first. Set by the cmd
// package.
var EnsureDaemon func() error

// Send issues a request with the default 10s deadline.
func Send(req Request) (*Response, error) {
	return SendTimeout(req, 10*time.Second)
}

// SendTimeout issues a request with a caller-supplied deadline. Long-running
// actions like reload (which health-checks and drains each instance in turn)
// need more than the default budget. If the daemon isn't reachable, it auto-
// starts it (via EnsureDaemon) and retries once — except for pings, so daemon
// status checks report truthfully instead of starting the daemon.
func SendTimeout(req Request, timeout time.Duration) (*Response, error) {
	resp, err := dialSend(req, timeout)
	if err == nil {
		return resp, nil
	}
	if EnsureDaemon != nil && req.Action != ActionPing {
		if startErr := EnsureDaemon(); startErr == nil {
			return dialSend(req, timeout)
		}
	}
	return resp, err
}

// PingRaw reports whether the daemon is reachable, without triggering an
// auto-start. Used by the daemon-start poll loop to avoid recursion.
func PingRaw() bool {
	resp, err := dialSend(Request{Action: ActionPing}, 2*time.Second)
	return err == nil && resp != nil && resp.OK
}

// dialSend performs a single request/response over the socket with no retry or
// auto-start.
func dialSend(req Request, timeout time.Duration) (*Response, error) {
	conn, err := net.DialTimeout("unix", SocketPath, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to gpm daemon (is it running? try: gpm daemon start): %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(timeout))

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("send error: %w", err)
	}

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("receive error: %w", err)
	}
	return &resp, nil
}
