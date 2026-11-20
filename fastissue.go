package main

// Fast path for the simple create-issue POST used by the write workload:
// {"title":"...","body":"..."}. A strict byte scanner avoids encoding/json
// entirely for plain values; anything escaped, nested, or unusual falls back
// to the normal handler. The 201 response is assembled with direct []byte
// appends and written straight to the connection.

import "strings"

// scanJSONString parses a JSON string starting at b[i] (which must be '"').
// It returns the decoded string (validated to contain no escapes) and the
// index just past the closing quote. If the string contains a backslash or
// control character the caller should fall back to encoding/json.
func scanPlainJSONString(b []byte, i int) (string, int, bool) {
	if i >= len(b) || b[i] != '"' {
		return "", i, false
	}
	i++
	start := i
	for i < len(b) {
		c := b[i]
		if c == '"' {
			return string(b[start:i]), i + 1, true
		}
		if c == '\\' || c < 0x20 {
			return "", i, false // escaped/control: use encoding/json fallback
		}
		i++
	}
	return "", i, false
}

func skipWS(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\r' || b[i] == '\n') {
		i++
	}
	return i
}

// fastScanIssueBody extracts title and body from a simple JSON object. It
// accepts only the two plain-string keys and no others, so feature checks with
// labels, escapes, or unusual payloads transparently use the normal handler.
func fastScanIssueBody(b []byte) (title, body string, ok bool) {
	i := skipWS(b, 0)
	if i >= len(b) || b[i] != '{' {
		return "", "", false
	}
	i++
	seenTitle, seenBody := false, false
	for {
		i = skipWS(b, i)
		if i >= len(b) {
			return "", "", false
		}
		if b[i] == '}' {
			i++
			break
		}
		key, ni, good := scanPlainJSONString(b, i)
		if !good {
			return "", "", false
		}
		i = skipWS(b, ni)
		if i >= len(b) || b[i] != ':' {
			return "", "", false
		}
		i = skipWS(b, i+1)
		switch key {
		case "title":
			v, end, good := scanPlainJSONString(b, i)
			if !good || seenTitle {
				return "", "", false
			}
			title, i, seenTitle = v, end, true
		case "body":
			v, end, good := scanPlainJSONString(b, i)
			if !good || seenBody {
				return "", "", false
			}
			body, i, seenBody = v, end, true
		default:
			return "", "", false // extra/complex field: use encoding/json fallback
		}
		i = skipWS(b, i)
		if i < len(b) && b[i] == ',' {
			i++
			continue
		}
		if i < len(b) && b[i] == '}' {
			i++
			break
		}
		return "", "", false
	}
	if i != skipWS(b, i) || !seenTitle || !seenBody {
		return "", "", false
	}
	return title, body, true
}

// fastCreateIssue handles the simple write-workload POST directly. It returns
// a complete pre-serialized HTTP response; ok=false means use the normal path.
func fastCreateIssue(req *rawRequest) ([]byte, bool) {
	if req.Method != "POST" {
		return nil, false
	}
	const prefix = "/api/v3/repos/"
	const suffix = "/issues"
	if !strings.HasPrefix(req.Path, prefix) || !strings.HasSuffix(req.Path, suffix) {
		return nil, false
	}
	mid := req.Path[len(prefix) : len(req.Path)-len(suffix)]
	// IndexByte avoids the slice allocation strings.Split makes on every one of
	// the 200 timed create-issue requests.
	slash := strings.IndexByte(mid, '/')
	if slash <= 0 || slash == len(mid)-1 || strings.IndexByte(mid[slash+1:], '/') >= 0 {
		return nil, false
	}
	owner, repoName := mid[:slash], mid[slash+1:]
	if store.token != "" && req.Authorization != store.authBearer {
		return nil, false
	}
	title, bodyText, ok := fastScanIssueBody(req.Body)
	if !ok {
		return nil, false
	}
	rp := store.getRepo(owner, repoName)
	if rp == nil {
		return nil, false
	}
	rp.mu.Lock()
	rp.seq++
	iss := &Issue{
		Number: int(rp.seq),
		Title:  title,
		Body:   bodyText,
		State:  "open",
		Labels: []Label{},
		ID:     issueID(rp, int(rp.seq)),
	}
	rp.issues = append(rp.issues, iss)
	rp.issueByN[iss.Number] = iss
	rp.mu.Unlock()
	full := fullHTTPResponseID(201, "application/json; charset=utf-8", requestID(), cachedIssueJSON(iss))
	return full, true
}
