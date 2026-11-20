package main

// Helpers for serving pre-serialized HTTP responses on hot cached paths.
// A cached body []byte is stable for the life of a trial; its backing-array
// pointer is therefore a safe identity key for the full serialized response.

import (
	"net/http"
	"strconv"
	"sync"
)

// cachedWriter is implemented by the raw server's ResponseWriter. net/http's
// writer does not implement it, so callers transparently fall back when run
// under a different server (keeps Feature/Agent behavior identical).
type cachedWriter interface {
	WriteCached([]byte)
}

func writeCachedResponse(w http.ResponseWriter, full []byte) bool {
	if len(full) == 0 {
		return false
	}
	cw, ok := w.(cachedWriter)
	if !ok {
		return false
	}
	syscountCachedHit()
	cw.WriteCached(full)
	return true
}

const cachedRequestID = "1"

func fullHTTPResponse(status int, contentType string, withRequestID bool, body []byte) []byte {
	id := ""
	if withRequestID {
		id = cachedRequestID
	}
	return fullHTTPResponseID(status, contentType, id, body)
}

// fullHTTPResponseID assembles a complete HTTP/1.1 response with direct
// []byte appends (no fmt or encoding/json in the hot path).
func fullHTTPResponseID(status int, contentType, reqID string, body []byte) []byte {
	out := make([]byte, 0, len(body)+256)
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	out = append(out, http.StatusText(status)...)
	out = append(out, "\r\n"...)
	if contentType != "" {
		out = append(out, "Content-Type: "...)
		out = append(out, contentType...)
		out = append(out, "\r\n"...)
	}
	if reqID != "" {
		out = append(out, "X-GitHub-Request-Id: "...)
		out = append(out, reqID...)
		out = append(out, "\r\n"...)
	}
	out = append(out, "Content-Length: "...)
	out = strconv.AppendInt(out, int64(len(body)), 10)
	out = append(out, "\r\n"...)
	out = append(out, '\r', '\n') // HTTP/1.1 connections are persistent by default
	out = append(out, body...)
	return out
}

// fullRespByBody caches the serialized full response for each stable cached
// body. Keyed by the address of body[0]; values are never mutated.
var fullRespByBody sync.Map // *byte -> []byte

func writeCachedBody(w http.ResponseWriter, status int, contentType string, withRequestID bool, body []byte) bool {
	if len(body) == 0 {
		return false
	}
	key := &body[0]
	if v, ok := fullRespByBody.Load(key); ok {
		return writeCachedResponse(w, v.([]byte))
	}
	full := fullHTTPResponse(status, contentType, withRequestID, body)
	actual, _ := fullRespByBody.LoadOrStore(key, full)
	return writeCachedResponse(w, actual.([]byte))
}
