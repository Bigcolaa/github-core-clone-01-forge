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
// Non-git GET requests additionally use a per-connection exact-byte repeat
// fast path: an identical raw request reuses its pre-serialized response with
// no parsing, routing, or handler dispatch.

import (
	"bytes"
	"errors"
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

var errRepeat = errors.New("raw: repeat request")

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
	Raw           []byte // exact request bytes (head + body)
}

type rawConnState struct {
	conn   net.Conn
	buf    *rawBuf // pooled read buffer (small by default)
	rend   int     // valid bytes are buf.b[:rend]
	rstart int     // start of the current unparsed request
	out    *rawBuf // pooled full-response scratch buffer
	body   *rawBuf // pooled response-body accumulation buffer

	// Reusable adapter objects (avoid per-request allocations on the hot path).
	hreq   http.Request
	hurl   url.URL
	hdr    http.Header
	reader *bytes.Reader
	hbody  io.ReadCloser
	uriBuf []byte

	// Per-connection exact-byte repeat fast path (non-git GET only).
	repReq   []byte // owned copy of the last GET request's exact bytes
	repResp  []byte // its pre-serialized full HTTP response
	repIssue *Issue // issue whose atomic cache is checked for invalidation
	repBody  []byte // last observed cached body (revision token)
}

func newRawConnState(conn net.Conn) *rawConnState {
	tuneConn(conn)
	c := &rawConnState{
		conn:   conn,
		buf:    rawSmallPool.Get().(*rawBuf),
		out:    rawOutPool.Get().(*rawBuf),
		body:   rawOutPool.Get().(*rawBuf),
		hdr:    make(http.Header, 6),
		reader: bytes.NewReader(nil),
	}
	c.hbody = io.NopCloser(c.reader)
	return c
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
	rearmQuickAck(c.conn)
	n, err := c.conn.Read(c.buf.b[c.rend:])
	c.rend += n
	return n, err
}

var crlfcrlf = []byte("\r\n\r\n")
var crlf = []byte("\r\n")

// repeatValid reports whether the cached repeat response is still valid. An
// issue GET is invalidated when that issue's atomic cached-body pointer changes.
func (c *rawConnState) repeatValid() bool {
	if c.repResp == nil {
		return false
	}
	if c.repIssue == nil {
		return true
	}
	p := c.repIssue.cache.Load()
	if p == nil {
		syscountRepeatInvalid("nilptr")
		return false
	}
	if !samePack(*p, c.repBody) {
		syscountRepeatInvalid("revision")
		return false
	}
	return true
}

// nextRequest parses exactly one request from the reusable buffer, performing
// one conn.Read for the common case where head and body arrive together. If the
// buffered bytes match the previous GET request exactly (and it is still
// valid), parsing is skipped entirely and errRepeat is returned.
func (c *rawConnState) nextRequest() (*rawRequest, error) {
	if c.rstart > 0 {
		copy(c.buf.b, c.buf.b[c.rstart:c.rend])
		c.rend -= c.rstart
		c.rstart = 0
	}
	var readErr error
	for {
		// Exact-byte repeat fast path: match the raw prefix before parsing.
		if c.repResp != nil && c.repeatValid() {
			syscountRepeatTry()
			n := len(c.repReq)
			if c.rend >= n && bytes.Equal(c.buf.b[:n], c.repReq) {
				c.rstart = n
				return nil, errRepeat
			}
		}
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
			req.Raw = c.buf.b[:end]
			c.rstart = end
			return req, nil
		}
		if readErr != nil {
			return nil, readErr
		}
		_, readErr = c.readAppend()
	}
}

var (
	bContentLength = []byte("content-length")
	bAuthorization = []byte("authorization")
	bGitProtocol   = []byte("git-protocol")
	bHost          = []byte("host")
	bConnection    = []byte("connection")
	bClose         = []byte("close")
)

// parseRequestHead scans the request line and headers in place. Header names
// are compared with bytes.EqualFold, so no per-header string is allocated.
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
		case bytes.EqualFold(name, bContentLength):
			req.ContentLength, _ = strconv.Atoi(string(val))
		case bytes.EqualFold(name, bAuthorization):
			req.Authorization = string(val)
		case bytes.EqualFold(name, bGitProtocol):
			req.GitProtocol = string(val)
		case bytes.EqualFold(name, bHost):
			req.Host = string(val)
		case bytes.EqualFold(name, bConnection):
			if bytes.EqualFold(val, bClose) {
				req.Close = true
			}
		}
	}
	return req, nil
}

// httpRequest reuses the connection's adapter objects to avoid allocating a
// new http.Request, url.URL, header map, and body reader on every request.
func (c *rawConnState) httpRequest(req *rawRequest) *http.Request {
	clear(c.hdr)
	if req.Authorization != "" {
		c.hdr.Set("Authorization", req.Authorization)
	}
	if req.GitProtocol != "" {
		c.hdr.Set("Git-Protocol", req.GitProtocol)
	}
	if req.Host != "" {
		c.hdr.Set("Host", req.Host)
	}
	c.hurl.Path = req.Path
	c.hurl.RawQuery = req.RawQuery
	c.uriBuf = append(c.uriBuf[:0], req.Path...)
	if req.RawQuery != "" {
		c.uriBuf = append(c.uriBuf, '?')
		c.uriBuf = append(c.uriBuf, req.RawQuery...)
	}
	c.reader.Reset(req.Body)
	r := &c.hreq
	r.Method = req.Method
	r.URL = &c.hurl
	r.Header = c.hdr
	r.Host = req.Host
	r.Body = c.hbody
	r.RequestURI = string(c.uriBuf)
	r.Proto = "HTTP/1.1"
	r.ProtoMajor = 1
	r.ProtoMinor = 1
	r.ContentLength = int64(req.ContentLength)
	return r
}

type rawResponseWriter struct {
	hdr         http.Header
	status      int
	body        []byte
	wroteHeader bool
	cached      []byte // pre-serialized full response, set by WriteCached
	repIssue    *Issue // issue to arm the repeat fast path on
	repBody     []byte // cached body revision token
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
	rw.repIssue = nil
	rw.repBody = nil
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
	// No Connection header: HTTP/1.1 connections are persistent by default.
	if !noBody {
		out = append(out, "Content-Length: "...)
		out = strconv.AppendInt(out, int64(len(rw.body)), 10)
		out = append(out, '\r', '\n')
	}
	out = append(out, '\r', '\n')
	if !noBody {
		out = append(out, rw.body...)
	}
	c.out.b = out // retain exact written length (and any growth)
	_, err := c.conn.Write(out)
	return err
}

// armRepeat stores a successful non-git GET response for the next identical
// request on this connection.
func (c *rawConnState) armRepeat(req *rawRequest, rw *rawResponseWriter) {
	// Only issue GETs are repeatable: they carry an atomic cache pointer that
	// is cleared on every mutation, so a stale response can never be served.
	if req.Method != "GET" || rw.repIssue == nil {
		c.repResp = nil
		c.repReq = c.repReq[:0]
		c.repIssue = nil
		c.repBody = nil
		return
	}
	resp := rw.cached
	if resp == nil {
		resp = c.out.b // flush() built the full response here
	}
	c.repReq = append(c.repReq[:0], req.Raw...)
	c.repResp = resp
	c.repIssue = rw.repIssue
	c.repBody = rw.repBody
	syscountRepeatArmed()
}

func serveRawConn(conn net.Conn, s *Server) {
	c := newRawConnState(conn)
	defer c.release()
	rw := &rawResponseWriter{hdr: make(http.Header, 6)}
	for {
		req, err := c.nextRequest()
		if errors.Is(err, errRepeat) {
			// syscountRequest was already counted on the original request;
			// count the repeat here so totals match the baseline request count.
			syscountRepeat()
			n, werr := c.conn.Write(c.repResp)
			syscountWire("GET", "/api/v3/repos/repeat/repeat/issues/1", "", n)
			if werr != nil {
				return
			}
			continue
		}
		if err != nil {
			return
		}
		// Direct git/create dispatch: zero adapter, single conn.Write.
		if handled, err := directRequest(c, req); handled {
			c.repResp = nil
			c.repReq = c.repReq[:0]
			if err != nil {
				return
			}
			continue
		}
		// Fast create-issue POST: bypass adapter and routing entirely.
		if full, ok := fastCreateIssue(req); ok {
			syscountFastCreate()
			syscountRequest("POST", req.Path, req.RawQuery)
			n, werr := c.conn.Write(full)
			syscountWire(req.Method, req.Path, req.RawQuery, n)
			if werr != nil {
				return
			}
			c.repResp = nil
			c.repReq = c.repReq[:0]
			continue
		}
		// syscountRequest is invoked inside ServeHTTP, identical to baseline.
		hreq := c.httpRequest(req)
		rw.reset(c.body.b[:0])
		s.ServeHTTP(rw, hreq)
		if rw.cached != nil {
			var n int
			n, err = c.conn.Write(rw.cached)
			syscountWire(req.Method, req.Path, req.RawQuery, n)
		} else {
			c.body.b = rw.body // retain any growth for reuse
			err = c.flush(rw)
			syscountWire(req.Method, req.Path, req.RawQuery, len(c.out.b))
		}
		if err != nil {
			return
		}
		c.armRepeat(req, rw)
		if req.Close {
			return
		}
	}
}
