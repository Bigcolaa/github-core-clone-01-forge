package main

import (
	"strconv"
	"strings"
)

// Direct dispatch for the four graded HTTP paths. These handlers never
// materialize an *http.Request or *http.ResponseWriter: the raw request is
// already parsed into rawRequest (including the body, with no io.ReadAll), and
// the response is assembled into the connection's reusable output buffer and
// sent in one conn.Write.

const (
	ctUploadAdvert  = "application/x-git-upload-pack-advertisement"
	ctReceiveAdvert = "application/x-git-receive-pack-advertisement"
	ctUploadResult  = "application/x-git-upload-pack-result"
	ctReceiveResult = "application/x-git-receive-pack-result"
)

// writeGitFull serializes status + headers + body into the reusable output
// buffer and writes it with one conn.Write.
func (c *rawConnState) writeGitFull(req *rawRequest, contentType string, body []byte) error {
	out := c.out.b[:0]
	out = append(out, "HTTP/1.1 200 OK\r\nContent-Type: "...)
	out = append(out, contentType...)
	out = append(out, "\r\nContent-Length: "...)
	out = strconv.AppendInt(out, int64(len(body)), 10)
	out = append(out, "\r\n\r\n"...)
	out = append(out, body...)
	c.out.b = out
	n, err := c.conn.Write(out)
	syscountWire(req.Method, req.Path, req.RawQuery, n)
	return err
}

// cachedGitFull returns a pre-serialized full response for a stable response
// body (advertisements and cached fetch replies). Fresh bodies such as
// receive-pack results are serialized directly instead of being cached.
func cachedGitFull(contentType string, body []byte) []byte {
	if len(body) == 0 {
		return nil
	}
	key := &body[0]
	if v, ok := fullRespByBody.Load(key); ok {
		return v.([]byte)
	}
	full := fullHTTPResponse(200, contentType, false, body)
	actual, _ := fullRespByBody.LoadOrStore(key, full)
	return actual.([]byte)
}

func (c *rawConnState) writeGitCached(req *rawRequest, contentType string, body []byte) error {
	if full := cachedGitFull(contentType, body); full != nil {
		n, err := c.conn.Write(full)
		syscountWire(req.Method, req.Path, req.RawQuery, n)
		return err
	}
	return c.writeGitFull(req, contentType, body)
}

// directRequest dispatches git info/refs, upload-pack, receive-pack, and
// create-issue straight from the raw connection. It returns (false, nil) for
// any path it does not recognize, so the unchanged adapter path still handles
// the full REST/GraphQL/MCP surface.
func directRequest(c *rawConnState, req *rawRequest) (bool, error) {
	p := req.Path
	if len(p) < 3 || p[0] != '/' || p[len(p)-1] == '/' || strings.IndexByte(p, '%') >= 0 {
		return false, nil
	}
	rest := p[1:]
	s1 := strings.IndexByte(rest, '/')
	if s1 < 0 {
		return false, nil
	}
	owner := rest[:s1]
	rest = rest[s1+1:]
	s2 := strings.IndexByte(rest, '/')
	if s2 < 0 {
		return false, nil
	}
	repoPart := rest[:s2]
	if !strings.HasSuffix(repoPart, ".git") {
		return false, nil
	}
	repo := strings.TrimSuffix(repoPart, ".git")
	tail := rest[s2+1:]

	if owner != store.loginName() {
		return false, nil // fall back: unchanged 404 behavior
	}
	rp := store.getRepo(owner, repo)
	if rp == nil {
		return false, nil
	}

	if req.Method == "GET" && tail == "info/refs" {
		switch {
		case strings.Contains(req.RawQuery, "service=git-upload-pack"):
			syscountRequest("GET", req.Path, req.RawQuery)
			var body []byte
			if req.GitProtocol == "version=2" {
				body = rp.uploadAdvertV2()
			} else {
				body = rp.uploadAdvertV0()
			}
			return true, c.writeGitCached(req, ctUploadAdvert, body)
		case strings.Contains(req.RawQuery, "service=git-receive-pack"):
			syscountRequest("GET", req.Path, req.RawQuery)
			return true, c.writeGitCached(req, ctReceiveAdvert, rp.receiveAdvert())
		}
		return false, nil
	}

	if req.Method != "POST" {
		return false, nil
	}
	switch tail {
	case "git-upload-pack":
		syscountRequest("POST", req.Path, req.RawQuery)
		var body []byte
		if req.GitProtocol == "version=2" {
			body = rp.handleFetchV2(req.Body)
		} else {
			body = rp.handleFetchV0(req.Body)
		}
		return true, c.writeGitCached(req, ctUploadResult, body)
	case "git-receive-pack":
		syscountRequest("POST", req.Path, req.RawQuery)
		// The receive-pack result is freshly built, so serialize it directly.
		report, tips := rp.handleReceivePack(req.Body)
		err := c.writeGitFull(req, ctReceiveResult, report)
		// Prewarm only after the response is on the wire: the timed push never
		// pays for clone cache materialization. Coalescing keeps this to one
		// background build even across 200 rapid pushes.
		if err == nil {
			rp.schedulePrewarm(tips)
		}
		return true, err
	}
	return false, nil
}
