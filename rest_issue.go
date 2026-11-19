package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func cachedIssueJSON(i *Issue) []byte {
	if p := i.cache.Load(); p != nil {
		return *p
	}
	b, _ := json.Marshal(issueJSON(i))
	b = append(b, '\n')
	i.cache.Store(&b)
	return b
}

func parseIssueLabels(raw json.RawMessage, rp *Repo) ([]Label, bool) {
	out := []Label{}
	if len(raw) == 0 || string(raw) == "null" {
		return out, true
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err == nil {
		for _, name := range names {
			if name == "" {
				continue
			}
			if _, ok := rp.labels[name]; !ok {
				return nil, false
			}
			out = append(out, Label{Name: name})
		}
		return out, true
	}
	var objs []map[string]any
	if err := json.Unmarshal(raw, &objs); err == nil {
		for _, obj := range objs {
			name, _ := obj["name"].(string)
			if name == "" {
				continue
			}
			if _, ok := rp.labels[name]; !ok {
				return nil, false
			}
			out = append(out, Label{Name: name})
		}
		return out, true
	}
	return nil, false
}

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request, rp *Repo, p []string) {
	if len(p) == 0 {
		switch r.Method {
		case "GET":
			state := r.URL.Query().Get("state")
			rp.mu.RLock()
			out := make([]any, 0, len(rp.issues))
			for _, iss := range rp.issues {
				if state != "" && state != "all" {
					if state == "open" && iss.State != "open" {
						continue
					}
					if state == "closed" && iss.State != "closed" {
						continue
					}
				}
				var m map[string]any
				_ = json.Unmarshal(cachedIssueJSON(iss), &m)
				out = append(out, m)
			}
			rp.mu.RUnlock()
			desc := strings.EqualFold(r.URL.Query().Get("direction"), "desc")
			sort.SliceStable(out, func(i, j int) bool {
				a, _ := out[i].(map[string]any)["number"].(float64)
				b, _ := out[j].(map[string]any)["number"].(float64)
				if desc {
					return a > b
				}
				return a < b
			})
			writeJSON(w, 200, out)
		case "POST":
			var body struct {
				Title  string          `json:"title"`
				Body   string          `json:"body"`
				Labels json.RawMessage `json:"labels"`
			}
			if !decodeBody(r, &body) {
				writeErr(w, 400, "Bad Request")
				return
			}
			rp.mu.Lock()
			labels, ok := parseIssueLabels(body.Labels, rp)
			if !ok {
				rp.mu.Unlock()
				writeErr(w, 422, "label does not exist")
				return
			}
			rp.seq++
			iss := &Issue{
				Number: int(rp.seq),
				Title:  body.Title,
				Body:   body.Body,
				State:  "open",
				Labels: labels,
				ID:     issueID(rp, int(rp.seq)),
			}
			rp.issues = append(rp.issues, iss)
			rp.issueByN[iss.Number] = iss
			rp.mu.Unlock()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-GitHub-Request-Id", requestID())
			w.WriteHeader(201)
			_, _ = w.Write(cachedIssueJSON(iss))
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	n, err := strconv.Atoi(p[0])
	if err != nil {
		writeErr(w, 404, "Not Found")
		return
	}
	rp.mu.RLock()
	iss := rp.issueByN[n]
	rp.mu.RUnlock()
	if iss == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	if len(p) == 1 {
		switch r.Method {
		case "GET":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("X-GitHub-Request-Id", requestID())
			w.WriteHeader(200)
			_, _ = w.Write(cachedIssueJSON(iss))
		case "PATCH":
			var body struct {
				State string `json:"state"`
			}
			if !decodeBody(r, &body) {
				writeErr(w, 400, "Bad Request")
				return
			}
			rp.mu.Lock()
			if body.State != "" {
				iss.State = body.State
			}
			iss.cache.Store(nil)
			rp.mu.Unlock()
			writeJSON(w, 200, map[string]any{"number": iss.Number, "state": iss.State})
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	if len(p) == 2 && p[1] == "comments" {
		switch r.Method {
		case "GET":
			rp.mu.RLock()
			cs := append([]*Comment(nil), rp.comments[n]...)
			rp.mu.RUnlock()
			out := make([]any, 0, len(cs))
			for _, c := range cs {
				out = append(out, map[string]any{"id": c.ID, "body": c.Body})
			}
			writeJSON(w, 200, out)
		case "POST":
			var body struct {
				Body string `json:"body"`
			}
			if !decodeBody(r, &body) {
				writeErr(w, 400, "Bad Request")
				return
			}
			rp.mu.Lock()
			rp.commentSeq++
			c := &Comment{ID: int(rp.commentSeq), Body: body.Body}
			rp.comments[n] = append(rp.comments[n], c)
			rp.mu.Unlock()
			writeJSON(w, 201, map[string]any{"id": c.ID, "body": c.Body})
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	writeErr(w, 404, "Not Found")
}

func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request, rp *Repo, p []string) {
	if len(p) == 0 {
		switch r.Method {
		case "GET":
			state := r.URL.Query().Get("state")
			if state == "" {
				state = "open"
			}
			rp.mu.RLock()
			out := []any{}
			for _, pr := range rp.prs {
				if state != "all" && pr.State != state {
					continue
				}
				out = append(out, pullJSON(pr))
			}
			rp.mu.RUnlock()
			writeJSON(w, 200, out)
		case "POST":
			var body struct {
				Head  string `json:"head"`
				Base  string `json:"base"`
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			if !decodeBody(r, &body) {
				writeErr(w, 400, "Bad Request")
				return
			}
			if body.Base == "" {
				body.Base = rp.DefaultBranch
			}
			rp.mu.Lock()
			rp.seq++
			headSHA := rp.branches[body.Head]
			baseSHA := rp.branches[body.Base]
			pr := &Pull{
				Number:    int(rp.seq),
				State:     "open",
				Title:     body.Title,
				Body:      body.Body,
				Head:      RefSpec{Ref: body.Head, SHA: headSHA, Label: body.Head},
				Base:      RefSpec{Ref: body.Base, SHA: baseSHA, Label: body.Base},
				ID:        pullID(rp, int(rp.seq)),
				Mergeable: "MERGEABLE",
			}
			pr.Files = diffFilesLocked(rp, body.Base, body.Head)
			rp.prs = append(rp.prs, pr)
			rp.prByN[pr.Number] = pr
			rp.mu.Unlock()
			writeJSON(w, 201, pullJSON(pr))
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	n, err := strconv.Atoi(p[0])
	if err != nil {
		writeErr(w, 404, "Not Found")
		return
	}
	rp.mu.RLock()
	pr := rp.prByN[n]
	rp.mu.RUnlock()
	if pr == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	if len(p) == 1 && r.Method == "GET" {
		writeJSON(w, 200, pullJSON(pr))
		return
	}
	if len(p) == 2 && p[1] == "files" && r.Method == "GET" {
		rp.mu.RLock()
		fs := append([]PRFile(nil), pr.Files...)
		rp.mu.RUnlock()
		out := make([]any, 0, len(fs))
		for _, f := range fs {
			out = append(out, map[string]any{
				"filename": f.Filename, "status": f.Status,
				"additions": f.Additions, "deletions": f.Deletions, "changes": f.Changes,
			})
		}
		writeJSON(w, 200, out)
		return
	}
	if len(p) == 2 && p[1] == "merge" && r.Method == "PUT" {
		var body struct {
			MergeMethod string `json:"merge_method"`
		}
		if !decodeBody(r, &body) {
			writeErr(w, 400, "Bad Request")
			return
		}
		rp.mu.Lock()
		if pr.Merged || pr.State == "closed" {
			rp.mu.Unlock()
			writeErr(w, 405, "Pull request is closed")
			return
		}
		for _, other := range rp.prs {
			if other.Number == pr.Number || !other.Merged {
				continue
			}
			for _, f := range pr.Files {
				for _, g := range other.Files {
					if f.Filename == g.Filename {
						rp.mu.Unlock()
						writeErr(w, 405, "Merge conflict")
						return
					}
				}
			}
		}
		base := pr.Base.Ref
		files := rp.treeForLocked(base)
		for path, oid := range rp.treeForLocked(pr.Head.Ref) {
			files[path] = oid
		}
		tree := rp.buildTreeLocked(files)
		parent := rp.branches[base]
		sha := rp.putObjectLocked("commit", makeCommit(tree, parent, fmt.Sprintf("Merge pull request #%d", pr.Number)))
		rp.branches[base] = sha
		rp.files[base] = files
		pr.Merged = true
		pr.State = "closed"
		pr.MergedAt = "2025-02-10T08:19:57Z"
		pr.MergeCommit = sha
		rp.mu.Unlock()
		writeJSON(w, 200, map[string]any{"sha": sha, "merged": true, "message": "Merged"})
		return
	}
	writeErr(w, 404, "Not Found")
}

// diffFilesLocked compares two branch tips. Caller holds rp.mu.
func diffFilesLocked(rp *Repo, base, head string) []PRFile {
	bf := rp.treeForLocked(base)
	hf := rp.treeForLocked(head)
	out := []PRFile{}
	for path, oid := range hf {
		if bf[path] == oid {
			continue
		}
		additions := 1
		if obj := rp.objectLocked(oid); obj != nil && len(obj.Data) > 0 {
			additions = len(obj.Data)
		}
		out = append(out, PRFile{
			Filename: path, Path: path, Status: "modified",
			Additions: additions, Deletions: 0, Changes: additions,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}
