//go:build linux || darwin || freebsd || netbsd || openbsd

package gpm

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePortControl is passed to net.ListenConfig.Control so every listener gpm
// creates sets SO_REUSEADDR and SO_REUSEPORT on the underlying socket before
// bind(2). SO_REUSEPORT lets multiple instances of a service bind the same
// host:port simultaneously; the kernel load-balances new connections across
// them. This is what makes zero-downtime reload possible: the old and new
// instances both listen on the port at once, so there is never a gap with no
// listener.
func reusePortControl(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		if e := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); e != nil {
			sockErr = e
			return
		}
		if e := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); e != nil {
			sockErr = e
			return
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}
