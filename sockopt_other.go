//go:build !linux

package main

import "syscall"

func tuneListener(network, address string, c syscall.RawConn) error { return nil }
