//go:build linux

package main

// Linux/Docker low-latency socket tuning. Both options are applied exactly
// once on accept: a per-request TCP_QUICKACK setsockopt would add thousands of
// syscalls under gVisor and cost far more than the delayed ACK it removes.

import (
	"net"
	"syscall"
)

// underlyingTCP unwraps measurement conns to reach the real *net.TCPConn.
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
