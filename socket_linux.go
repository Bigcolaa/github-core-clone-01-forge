//go:build linux

package main

// Linux/Docker low-latency socket tuning. The Docker bridge plus gVisor adds
// delayed-ACK stalls that a plain TCP_NODELAY does not remove: Linux only
// sends an ACK immediately when QUICKACK is armed. Re-arm it at the top of
// every read so each request/response exchange gets an immediate ACK.

import (
	"net"
	"syscall"
)

// underlyingTCP unwraps measurement conns to reach the real socket.
func underlyingTCP(conn net.Conn) *net.TCPConn {
	for {
		if tc, ok := conn.(*net.TCPConn); ok {
			return tc
		}
		if u, ok := conn.(interface{ Unwrap() net.Conn }); ok {
			conn = u.Unwrap()
			continue
		}
		return nil
	}
}

// tuneConn is called once when a connection is accepted.
func tuneConn(conn net.Conn) {
	tc := underlyingTCP(conn)
	if tc == nil {
		return
	}
	_ = tc.SetNoDelay(true)
	raw, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)
	})
}

// rearmQuickAck re-arms TCP_QUICKACK before each read. Linux clears the flag
// after one ACK, so this must be repeated to keep the exchange ACK-fast.
func rearmQuickAck(conn net.Conn) {
	tc := underlyingTCP(conn)
	if tc == nil {
		return
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_QUICKACK, 1)
	})
}
