//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd

package gpm

import "syscall"

// reusePortControl is a no-op on platforms without SO_REUSEPORT. Listeners
// still work, but multiple instances cannot share a port, so zero-downtime
// reload degrades to a brief overlap window the kernel may reject. gpm targets
// unix; this stub exists only so the package compiles everywhere.
func reusePortControl(network, address string, c syscall.RawConn) error {
	return nil
}
