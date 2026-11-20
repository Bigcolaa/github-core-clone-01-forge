package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

type Server struct{}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header()["Date"] = nil
	switch {
	case r.URL.Path == "/healthz":
		writeJSON(w, 200, map[string]any{"ok": true})
	case r.URL.Path == "/api/graphql":
		s.handleGraphQL(w, r)
	case r.URL.Path == "/mcp":
		s.handleMCP(w, r)
	case strings.HasPrefix(r.URL.Path, "/api/v3"):
		s.handleREST(w, r)
	default:
		s.handleGitRoute(w, r)
	}
}

func (s *Server) handleGitRoute(w http.ResponseWriter, r *http.Request) {
	parts := pathParts(r.URL.Path)
	if len(parts) < 2 || !strings.HasSuffix(parts[1], ".git") {
		writeErr(w, 404, "Not Found")
		return
	}
	owner := parts[0]
	repo := strings.TrimSuffix(parts[1], ".git")
	store.getRepo(owner, repo).gitRoute(w, r, owner, repo, parts[2:])
}

func main() {
	// The trial is short-lived and shares a 2-CPU pod with the harness. Avoid
	// reporting a host-sized CPU count and avoid frequent GC cycles while the
	// benchmark is running; peak heap stays far below the 1 GiB pod limit.
	runtime.GOMAXPROCS(2)
	debug.SetGCPercent(1000)

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "MAKEFUN_ENV_URL is required")
		os.Exit(1)
	}
	envURL = strings.TrimRight(envURL, "/")
	store.token = os.Getenv("MAKEFUN_SHOP_TOKEN")
	store.authBearer = "Bearer " + store.token

	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{
		Handler:           &Server{},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		_ = srv.Serve(ln)
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	registerBody, _ := json.Marshal(map[string]any{"port": port})
	for {
		resp, err := client.Post(envURL+"/register", "application/json", bytes.NewReader(registerBody))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	for {
		resp, err := client.Get(envURL + "/done")
		if err == nil {
			var done struct {
				Done bool `json:"done"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&done)
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if done.Done {
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = srv.Close()
	os.Exit(0)
}
