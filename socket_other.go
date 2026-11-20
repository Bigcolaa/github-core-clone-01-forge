//go:build !linux

package main

import "net"

// Non-Linux builds keep v13's behavior: TCP_NODELAY only. The Docker/gVisor
// QUICKACK tuning is Linux-specific and has no portable equivalent.
func tuneConn(conn net.Conn) {
	if nd, ok := conn.(interface{ SetNoDelay(bool) error }); ok {
		_ = nd.SetNoDelay(true)
	}
}

func rearmQuickAck(conn net.Conn) {}
