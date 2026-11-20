//go:build !syscount

package main

import "net"

// No-op stubs for production builds. The build-tagged measurements never ship.
func instrumentListener(ln net.Listener) net.Listener { return ln }
func syscountRequest(method, path, query string)      {}
func syscountCachedHit()                              {}
func syscountRepeat()                                 {}
func syscountRepeatArmed()                            {}
func syscountRepeatTry()                              {}
func syscountRepeatInvalid(reason string)             {}
func syscountFastCreate()                             {}
func dumpSyscounts()                                  {}
