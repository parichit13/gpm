package ipc

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Send issues a request with the default 10s deadline.
func Send(req Request) (*Response, error) {
	return SendTimeout(req, 10*time.Second)
}

// SendTimeout issues a request with a caller-supplied deadline. Long-running
// actions like reload (which health-checks and drains each instance in turn)
// need more than the default budget.
func SendTimeout(req Request, timeout time.Duration) (*Response, error) {
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
