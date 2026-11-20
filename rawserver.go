package main

// Raw HTTP/1.1 server. The net/http server issues two conn.Read syscalls per
// request (one for headers via bufio.Reader, one for the body) and adds a
// general-purpose machinery cost on every request. Under gVisor each syscall
// is expensive. This server reads a whole request (head + body) in a single
// conn.Read into a reusable buffer, parses it in place into a fixed struct,
// and writes status + headers + body in a single conn.Write.
//
// Handlers are left completely unchanged: a thin adapter materializes the
// small subset of *http.Request / http.ResponseWriter they actually use.
// Hot cached paths bypass the adapter entirely via WriteCached: a
// pre-serialized full HTTP response goes straight to conn.Write.

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

const (
	rawSmallBufSize = 8 << 10   // clone/read/ls-refs requests are ~440 B total
	rawLargeBufSize = 512 << 10 // swapped in only for large bodies (receive-pack)
)

type rawBuf struct{ b []byte }

var rawSmallPool = sync.Pool{New: func() any {
	return &rawBuf{b: make([]byte, rawSmallBufSize)}
}}
var rawLargePool = sync.Pool{New: func() any {
	return &rawBuf{b: make([]byte, rawLargeBufSize)}
}}
var rawOutPool = sync.Pool{New: func() any {
	return &rawBuf{b: make([]byte, 8<<10)}
}}

// rawRequest holds only the request fields the handlers consume. There is no
// header map: header scanning writes directly into these fixed fields.
type rawRequest struct {
	Method        string
	Path          string
	RawQuery      string
	Host          string
	Authorization string
	GitProtocol   string
	ContentLength int
	Close         bool
	Body          []byte
}

type rawConnState struct {
	conn   net.Conn
	buf    *rawBuf // pooled read buffer (small by default)
	rend   int     // valid bytes are buf.b[:rend]
	rstart int     // start of the current unparsed request
	out    *rawBuf // pooled full-response scratch buffer
	body   *rawBuf // pooled response-body accumulation buffer
}

func newRawConnState(conn net.Conn) *rawConnState {
	if nd, ok := conn.(interface{ SetNoDelay(bool) error }); ok {
		_ = nd.SetNoDelay(true)
	}
	return &rawConnState{
		conn: conn,
		buf:  rawSmallPool.Get().(*rawBuf),
		out:  rawOutPool.Get().(*rawBuf),
		body: rawOutPool.Get().(*rawBuf),
	}
}

func (c *rawConnState) release() {
	if len(c.buf.b) == rawSmallBufSize {
		rawSmallPool.Put(c.buf)
	} else if len(c.buf.b) == rawLargeBufSize {
		rawLargePool.Put(c.buf)
	}
	rawOutPool.Put(c.out)
	rawOutPool.Put(c.body)
}

// ensureCapacity swaps in a pooled large buffer when a request body exceeds
// the small buffer. The small buffer returns to its pool immediately.
func (c *rawConnState) ensureCapacity(need int) {
	if need <= len(c.buf.b) {
		return
	}
	size := rawLargeBufSize
	if need > size {
		size = need
	}
	var nb *rawBuf
	if size == rawLargeBufSize {
		nb = rawLargePool.Get().(*rawBuf)
	} else {
		nb = &rawBuf{b: make([]byte, size)}
	}
	copy(nb.b, c.buf.b[:c.rend])
	old := c.buf
	c.buf = nb
	if len(old.b) == rawSmallBufSize {
		rawSmallPool.Put(old)
	} else if len(old.b) == rawLargeBufSize && old != nb {
		rawLargePool.Put(old)
	}
}

func (c *rawConnState) readAppend() (int, error) {
	if c.rend == len(c.buf.b) {
		c.ensureCapacity(len(c.buf.b) * 2)
	}
	n, err := c.conn.Read(c.buf.b[c.rend:])
	c.rend += n
	return n, err
}

var crlfcrlf = []byte("\r\n\r\n")
var crlf = []byte("\r\n")

// nextRequest parses exactly one request from the reusable buffer, performing
// one conn.Read for the common case where head and body arrive together.
func (c *rawConnState) nextRequest() (*rawRequest, error) {
	if c.rstart > 0 {
		copy(c.buf.b, c.buf.b[c.rstart:c.rend])
		c.rend -= c.rstart
		c.rstart = 0
	}
	var readErr error
	for {
		if idx := bytes.Index(c.buf.b[:c.rend], crlfcrlf); idx >= 0 {
			he := idx + len(crlfcrlf)
			req, err := parseRequestHead(c.buf.b[:he])
			if err != nil {
				return nil, err
			}
			// Grow to a pooled large buffer only when Content-Length demands it.
			if need := he + req.ContentLength; need > len(c.buf.b) {
				c.ensureCapacity(need)
			}
			end := he + req.ContentLength
			for c.rend < end {
				n, err := c.readAppend()
				if err != nil && n == 0 {
					return nil, err
				}
				if n == 0 {
					return nil, io.ErrUnexpectedEOF
				}
			}
			req.Body = c.buf.b[he:end]
			c.rstart = end
			return req, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		_, readErr = c.readAppend()
	}
}

// parseRequestHead scans the request line and headers in place. The only
// allocations are the few short strings materialized for the fixed fields.
func parseRequestHead(head []byte) (*rawRequest, error) {
	lineEnd := bytes.Index(head, crlf)
	if lineEnd < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	line := head[:lineEnd]
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	restLine := line[sp1+1:]
	sp2 := bytes.IndexByte(restLine, ' ')
	if sp2 < 0 {
		return nil, io.ErrUnexpectedEOF
	}
	method := string(line[:sp1])
	target := string(restLine[:sp2])

	req := &rawRequest{Method: method}
	if q := strings.IndexByte(target, '?'); q >= 0 {
		req.Path, req.RawQuery = target[:q], target[q+1:]
	} else {
		req.Path = target
	}

	headers := head[lineEnd+len(crlf):]
	for len(headers) > 0 {
		le := bytes.Index(headers, crlf)
		if le < 0 {
			break
		}
		hline := headers[:le]
		headers = headers[le+len(crlf):]
		colon := bytes.IndexByte(hline, ':')
		if colon < 0 {
			continue
		}
		name := hline[:colon]
		val := hline[colon+1:]
		for len(val) > 0 && (val[0] == ' ' || val[0] == '\t') {
			val = val[1:]
		}
		switch {
		case strings.EqualFold(string(name), "Content-Length"):
			req.ContentLength, _ = strconv.Atoi(string(val))
		case strings.EqualFold(string(name), "Authorization"):
			req.Authorization = string(val)
		case strings.EqualFold(string(name), "Git-Protocol"):
			req.GitProtocol = string(val)
		case strings.EqualFold(string(name), "Host"):
			req.Host = string(val)
		case strings.EqualFold(string(name), "Connection"):
			if strings.EqualFold(string(val), "close") {
				req.Close = true
			}
		}
	}
	return req, nil
}

func (c *rawConnState) httpRequest(req *rawRequest) *http.Request {
	u := &url.URL{Path: req.Path, RawQuery: req.RawQuery}
	h := make(http.Header, 4)
	if req.Authorization != "" {
		h.Set("Authorization", req.Authorization)
	}
	if req.GitProtocol != "" {
		h.Set("Git-Protocol", req.GitProtocol)
	}
	if req.Host != "" {
		h.Set("Host", req.Host)
	}
	target := req.Path
	if req.RawQuery != "" {
		target += "?" + req.RawQuery
	}
	return &http.Request{
		Method:     req.Method,
		URL:        u,
		Header:     h,
		Host:       req.Host,
		Body:       io.NopCloser(bytes.NewReader(req.Body)),
		RequestURI: target,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
	}
}

type rawResponseWriter struct {
	hdr         http.Header
	status      int
	body        []byte
	wroteHeader bool
	cached      []byte // pre-serialized full response, set by WriteCached
}

func (rw *rawResponseWriter) Header() http.Header { return rw.hdr }

func (rw *rawResponseWriter) WriteHeader(status int) {
	if !rw.wroteHeader {
		rw.status = status
		rw.wroteHeader = true
	}
}

func (rw *rawResponseWriter) Write(p []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	rw.body = append(rw.body, p...)
	return len(p), nil
}

// WriteCached bypasses the adapter: the handler supplies a complete,
// pre-serialized HTTP response which is written straight to the connection.
func (rw *rawResponseWriter) WriteCached(full []byte) {
	rw.cached = full
	rw.wroteHeader = true
}

func (rw *rawResponseWriter) reset(bodyBuf []byte) {
	clear(rw.hdr)
	rw.status = 0
	rw.body = bodyBuf[:0]
	rw.wroteHeader = false
	rw.cached = nil
}

func (c *rawConnState) flush(rw *rawResponseWriter) error {
	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	noBody := status == http.StatusNoContent || status == http.StatusNotModified

	out := c.out.b[:0]
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	out = append(out, http.StatusText(status)...)
	out = append(out, '\r', '\n')

	for k, vs := range rw.hdr {
		for _, v := range vs {
			out = append(out, k...)
			out = append(out, ':', ' ')
			out = append(out, v...)
			out = append(out, '\r', '\n')
		}
	}
	out = append(out, "Connection: keep-alive\r\n"...)
	if !noBody {
		out = append(out, "Content-Length: "...)
		out = strconv.AppendInt(out, int64(len(rw.body)), 10)
		out = append(out, '\r', '\n')
	}
	out = append(out, '\r', '\n')
	if !noBody {
		out = append(out, rw.body...)
	}
	if cap(out) > cap(c.out.b) {
		c.out.b = out
	}
	_, err := c.conn.Write(out)
	return err
}

func serveRawConn(conn net.Conn, s *Server) {
	c := newRawConnState(conn)
	defer c.release()
	rw := &rawResponseWriter{hdr: make(http.Header, 6)}
	for {
		req, err := c.nextRequest()
		if err != nil {
			return
		}
		// syscountRequest is invoked inside ServeHTTP, identical to baseline.
		hreq := c.httpRequest(req)
		rw.reset(c.body.b[:0])
		s.ServeHTTP(rw, hreq)
		if rw.cached != nil {
			_, err = c.conn.Write(rw.cached)
		} else {
			c.body.b = rw.body // retain any growth for reuse
			err = c.flush(rw)
		}
		if err != nil {
			return
		}
		if req.Close {
			return
		}
	}
}
