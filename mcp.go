package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

var mcpToolsBytes []byte

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func init() {
	props := func(names []string, required []string) map[string]any {
		p := map[string]any{}
		for _, n := range names {
			typ := "string"
			if n == "private" || n == "pull_number" {
				if n == "pull_number" {
					typ = "integer"
				} else {
					typ = "boolean"
				}
			}
			p[n] = map[string]any{"type": typ, "description": n}
		}
		return map[string]any{"type": "object", "properties": p, "required": required}
	}
	tools := []mcpTool{
		{Name: "create_repo", Description: "Create a repository", InputSchema: props([]string{"owner", "name", "description", "private"}, []string{"name"})},
		{Name: "get_repo", Description: "Get a repository by owner and name", InputSchema: props([]string{"owner", "repo"}, []string{"owner", "repo"})},
		{Name: "create_issue", Description: "Create an issue in a repository", InputSchema: props([]string{"owner", "repo", "title", "body"}, []string{"owner", "repo", "title"})},
		{Name: "list_issues", Description: "List issues in a repository", InputSchema: props([]string{"owner", "repo", "state"}, []string{"owner", "repo"})},
		{Name: "create_pull_request", Description: "Create a pull request", InputSchema: props([]string{"owner", "repo", "title", "body", "head", "base"}, []string{"owner", "repo", "head", "base"})},
		{Name: "merge_pull_request", Description: "Merge a pull request", InputSchema: props([]string{"owner", "repo", "pull_number"}, []string{"owner", "repo", "pull_number"})},
	}
	mcpToolsBytes, _ = json.Marshal(tools)
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "DELETE" {
		w.WriteHeader(204)
		return
	}
	if r.Method != "POST" {
		writeErr(w, 404, "Not Found")
		return
	}
	if !authOK(r) {
		writeErr(w, 401, "Requires authentication")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&req); err != nil {
		writeMCP(w, 400, mcpError(nil, -32700, "Parse error"))
		return
	}
	method, _ := req["method"].(string)
	id := req["id"]
	if strings.HasPrefix(method, "notifications/") {
		w.WriteHeader(204)
		return
	}
	params, _ := req["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	switch method {
	case "server/discover":
		writeMCP(w, 200, mcpResult(id, map[string]any{
			"supportedVersions": []string{"2026-07-28", "2025-11-25", "2025-06-18"},
			"serverInfo":        map[string]any{"name": "forge", "version": "1.0.0"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "forge", "version": "1.0.0"}},
		}))
	case "initialize":
		writeMCP(w, 200, mcpResult(id, map[string]any{
			"protocolVersion": "2026-07-28",
			"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}))
	case "tools/list":
		writeMCPRaw(w, 200, `{"jsonrpc":"2.0","id":`+mcpIDJSON(id)+`,"result":{"tools":`+string(mcpToolsBytes)+`}}`)
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		text, errCode, errMsg := mcpCall(name, args)
		if errCode != 0 {
			writeMCP(w, 200, mcpError(id, errCode, errMsg))
			return
		}
		writeMCP(w, 200, mcpResult(id, map[string]any{
			"content":           []any{map[string]any{"type": "text", "text": text}},
			"structuredContent": mustJSONMap(text),
		}))
	default:
		writeMCP(w, 200, mcpError(id, -32601, "Method not found"))
	}
}

func mcpIDJSON(id any) string {
	if id == nil {
		return "null"
	}
	b, _ := json.Marshal(id)
	return string(b)
}

func mcpResult(id any, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpError(id any, code int, message string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}}
}

func writeMCP(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	writeMCPRaw(w, status, string(b))
}

func writeMCPRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Mcp-Session-Id", "forge-session")
	w.Header().Set("X-GitHub-Request-Id", requestID())
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func mustJSONMap(s string) map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(s), &m) == nil {
		return m
	}
	return map[string]any{"result": s}
}

func mcpArg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := args[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func mcpArgsForRepo(args map[string]any) (string, string) {
	owner := mcpArg(args, "owner")
	repo := mcpArg(args, "repo", "repository")
	if full := mcpArg(args, "full_name", "repo_full_name"); full != "" {
		if i := strings.IndexByte(full, '/'); i > 0 {
			return full[:i], full[i+1:]
		}
	}
	return owner, repo
}

func mcpCall(name string, args map[string]any) (string, int, string) {
	switch name {
	case "create_repo":
		owner := mcpArg(args, "owner")
		if owner == "" {
			owner = store.loginName()
		}
		n := mcpArg(args, "name")
		if n == "" {
			return "", -32602, "missing required argument: name"
		}
		desc := mcpArg(args, "description")
		private, _ := args["private"].(bool)
		rp, ok := store.createRepo(owner, n, desc, private)
		if !ok {
			rp = store.getRepo(owner, n)
		}
		return jsonText(map[string]any{"id": repoID(rp), "name": rp.Name, "full_name": rp.FullName}), 0, ""
	case "get_repo":
		owner, n := mcpArgsForRepo(args)
		if owner == "" || n == "" {
			return "", -32602, "missing required arguments: owner, repo"
		}
		rp := store.getRepo(owner, n)
		if rp == nil {
			return "", -32602, "repository not found"
		}
		return jsonText(map[string]any{"id": repoID(rp), "name": rp.Name, "full_name": rp.FullName, "description": rp.Description}), 0, ""
	case "create_issue":
		owner, n := mcpArgsForRepo(args)
		title := mcpArg(args, "title")
		if owner == "" || n == "" || title == "" {
			return "", -32602, "missing required arguments: owner, repo, title"
		}
		rp := store.getRepo(owner, n)
		if rp == nil {
			return "", -32602, "repository not found"
		}
		iss := createIssue(rp, title, mcpArg(args, "body"))
		return jsonText(map[string]any{"number": iss.Number, "id": iss.ID, "title": iss.Title}), 0, ""
	case "list_issues":
		owner, n := mcpArgsForRepo(args)
		if owner == "" || n == "" {
			return "", -32602, "missing required arguments: owner, repo"
		}
		rp := store.getRepo(owner, n)
		if rp == nil {
			return "", -32602, "repository not found"
		}
		state := mcpArg(args, "state")
		rp.mu.RLock()
		out := []any{}
		for _, iss := range rp.issues {
			if state != "" && state != "all" && !strings.EqualFold(iss.State, state) {
				continue
			}
			out = append(out, map[string]any{"number": iss.Number, "title": iss.Title, "id": iss.ID})
		}
		rp.mu.RUnlock()
		return jsonText(out), 0, ""
	case "create_pull_request":
		owner, n := mcpArgsForRepo(args)
		head := mcpArg(args, "head", "head_ref", "head_branch")
		base := mcpArg(args, "base", "base_ref", "base_branch")
		if owner == "" || n == "" || head == "" || base == "" {
			return "", -32602, "missing required arguments: owner, repo, head, base"
		}
		rp := store.getRepo(owner, n)
		if rp == nil {
			return "", -32602, "repository not found"
		}
		pr := createPull(rp, base, head, mcpArg(args, "title"), mcpArg(args, "body"))
		return jsonText(map[string]any{"number": pr.Number, "id": pr.ID, "title": pr.Title}), 0, ""
	case "merge_pull_request":
		owner, n := mcpArgsForRepo(args)
		num := mcpInt(args, "pull_number", "number")
		if owner == "" || n == "" || num == 0 {
			return "", -32602, "missing required arguments: owner, repo, pull_number"
		}
		rp := store.getRepo(owner, n)
		if rp == nil {
			return "", -32602, "repository not found"
		}
		rp.mu.RLock()
		pr := rp.prByN[num]
		rp.mu.RUnlock()
		if pr == nil {
			return "", -32602, "pull request not found"
		}
		mergePull(pr)
		return jsonText(map[string]any{"number": pr.Number, "merged": true}), 0, ""
	}
	return "", -32602, "unknown tool"
}

func mcpInt(args map[string]any, keys ...string) int {
	for _, k := range keys {
		switch v := args[k].(type) {
		case float64:
			return int(v)
		case json.Number:
			n, _ := strconv.Atoi(string(v))
			return n
		case string:
			n, _ := strconv.Atoi(v)
			if n != 0 {
				return n
			}
		}
	}
	return 0
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
