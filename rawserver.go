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

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const rawReadBufSize = 512 << 10 // one read captures head + body of any benchmark request

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
	rbuf   []byte // reusable read buffer
	rend   int    // valid bytes are rbuf[:rend]
	rstart int    // start of the current unparsed request
	wbuf   []byte // reusable response-body buffer
	outbuf []byte // reusable full-response buffer
}

func newRawConnState(conn net.Conn) *rawConnState {
	if nd, ok := conn.(interface{ SetNoDelay(bool) error }); ok {
		_ = nd.SetNoDelay(true)
	}
	return &rawConnState{
		conn:   conn,
		rbuf:   make([]byte, rawReadBufSize),
		wbuf:   make([]byte, 0, 4096),
		outbuf: make([]byte, 0, rawReadBufSize+4096),
	}
}

// readAppend appends bytes from the connection to the reusable buffer,
// growing it only when a single request exceeds the initial capacity.
func (c *rawConnState) readAppend() (int, error) {
	if c.rend == len(c.rbuf) {
		nb := make([]byte, len(c.rbuf)*2)
		copy(nb, c.rbuf[:c.rend])
		c.rbuf = nb
	}
	n, err := c.conn.Read(c.rbuf[c.rend:])
	c.rend += n
	return n, err
}

var crlfcrlf = []byte("\r\n\r\n")
var crlf = []byte("\r\n")

// nextRequest parses exactly one request from the reusable buffer, performing
// one conn.Read for the common case where head and body arrive together.
func (c *rawConnState) nextRequest() (*rawRequest, error) {
	// Move any leftover bytes from a previous request to the front.
	if c.rstart > 0 {
		copy(c.rbuf, c.rbuf[c.rstart:c.rend])
		c.rend -= c.rstart
		c.rstart = 0
	}
	var readErr error
	for {
		if idx := bytes.Index(c.rbuf[:c.rend], crlfcrlf); idx >= 0 {
			he := idx + len(crlfcrlf)
			req, err := parseRequestHead(c.rbuf[:he])
			if err != nil {
				return nil, err
			}
			end := he + req.ContentLength
			// Complete the body. Normally already present from the first read.
			for c.rend < end {
				n, err := c.readAppend()
				if err != nil && n == 0 {
					return nil, err
				}
				if n == 0 {
					return nil, io.ErrUnexpectedEOF
				}
			}
			req.Body = c.rbuf[he:end]
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

// httpRequest materializes just enough of *http.Request for the existing
// handlers. Body is a reader over the reusable buffer; handlers consume it
// synchronously before the buffer is reused for the next request.
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

// rawResponseWriter is the minimal http.ResponseWriter the handlers use.
// WriteHeader only records the status; Write accumulates the body. The
// connection flushes status + headers + body together in one syscall.
type rawResponseWriter struct {
	hdr         http.Header
	status      int
	body        []byte
	wroteHeader bool
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

func (rw *rawResponseWriter) reset(bodyBuf []byte) {
	clear(rw.hdr)
	rw.status = 0
	rw.body = bodyBuf[:0]
	rw.wroteHeader = false
}

// flush serializes the complete response into one buffer and issues exactly
// one conn.Write. Content-Length is always provided so keep-alive clients know
// where the body ends without waiting for connection close.
func (c *rawConnState) flush(rw *rawResponseWriter) error {
	status := rw.status
	if status == 0 {
		status = http.StatusOK
	}
	noBody := status == http.StatusNoContent || status == http.StatusNotModified

	out := c.outbuf[:0]
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
	if !noBody {
		out = append(out, "Content-Length: "...)
		out = strconv.AppendInt(out, int64(len(rw.body)), 10)
		out = append(out, '\r', '\n')
		out = append(out, "Connection: keep-alive\r\n"...)
	} else {
		out = append(out, "Connection: keep-alive\r\n"...)
	}
	out = append(out, '\r', '\n')
	if !noBody {
		out = append(out, rw.body...)
	}
	c.outbuf = out
	_, err := c.conn.Write(out)
	return err
}

func serveRawConn(conn net.Conn, s *Server) {
	c := newRawConnState(conn)
	rw := &rawResponseWriter{hdr: make(http.Header, 6)}
	for {
		req, err := c.nextRequest()
		if err != nil {
			return
		}
		// syscountRequest is invoked inside ServeHTTP, identical to baseline.
		hreq := c.httpRequest(req)
		rw.reset(c.wbuf)
		s.ServeHTTP(rw, hreq)
		c.wbuf = rw.body // retain any growth for reuse
		if err := c.flush(rw); err != nil {
			return
		}
		if req.Close {
			return
		}
	}
}
