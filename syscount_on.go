//go:build syscount

package main

// Build-tagged instrumentation used only for the local A/B measurement. It is
// never part of a submitted binary: without -tags syscount the companion
// syscount_off.go provides no-op stubs and the production code is unchanged.

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	scReads      atomic.Int64
	scWrites     atomic.Int64
	scReadBytes  atomic.Int64
	scWriteBytes atomic.Int64
	scConns      atomic.Int64
	scReqPush    atomic.Int64
	scReqClone   atomic.Int64
	scReqRead    atomic.Int64
	scReqWrite   atomic.Int64
	scReqOther   atomic.Int64
	scReqTotal   atomic.Int64
	scPathCounts sync.Map // string -> *atomic.Int64
)

type scConn struct{ net.Conn }

func (c *scConn) SetNoDelay(noDelay bool) error {
	if tc, ok := c.Conn.(*net.TCPConn); ok {
		return tc.SetNoDelay(noDelay)
	}
	return nil
}

func (c *scConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	scReads.Add(1)
	scReadBytes.Add(int64(n))
	return n, err
}

func (c *scConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	scWrites.Add(1)
	scWriteBytes.Add(int64(n))
	return n, err
}

type scListener struct{ net.Listener }

func (l *scListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	scConns.Add(1)
	return &scConn{Conn: c}, nil
}

func instrumentListener(ln net.Listener) net.Listener { return &scListener{Listener: ln} }

func syscountRequest(method, path, query string) {
	scReqTotal.Add(1)
	key := "other"
	switch {
	case strings.Contains(path, "/api/v3"):
		if method == "POST" && strings.HasSuffix(path, "/issues") {
			scReqWrite.Add(1)
			key = "write"
		} else if method == "GET" && strings.Contains(path, "/issues/") {
			scReqRead.Add(1)
			key = "read"
		} else {
			scReqOther.Add(1)
		}
	case strings.Contains(path, ".git"):
		if method == "POST" && strings.HasSuffix(path, "/git-receive-pack") {
			scReqPush.Add(1)
			key = "push"
		} else if strings.Contains(query, "service=git-receive-pack") {
			scReqPush.Add(1)
			key = "push"
		} else {
			scReqClone.Add(1)
			key = "clone"
		}
	default:
		scReqOther.Add(1)
	}
	v, _ := scPathCounts.LoadOrStore(key, &atomic.Int64{})
	v.(*atomic.Int64).Add(1)
	_ = http.MethodGet // keep net/http import stable across builds
}

func dumpSyscounts() {
	path := os.Getenv("MAKEFUN_SYSCALL_LOG")
	if path == "" {
		path = `C:\Users\ISMAIL\AppData\Local\Temp\opencode\syscount-dump.json`
	}
	byKey := map[string]int64{}
	scPathCounts.Range(func(k, v any) bool {
		byKey[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	out := map[string]any{
		"conn_read_syscalls":   scReads.Load(),
		"conn_write_syscalls":  scWrites.Load(),
		"conn_read_bytes":      scReadBytes.Load(),
		"conn_write_bytes":     scWriteBytes.Load(),
		"connections":          scConns.Load(),
		"requests_total":       scReqTotal.Load(),
		"requests_push":        scReqPush.Load(),
		"requests_clone":       scReqClone.Load(),
		"requests_read":        scReqRead.Load(),
		"requests_write":       scReqWrite.Load(),
		"requests_other":       scReqOther.Load(),
		"requests_by_category": byKey,
	}
	b, _ := json.Marshal(out)
	_ = os.WriteFile(path, b, 0o644)
}
