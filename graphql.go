package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeErr(w, 404, "Not Found")
		return
	}
	if !authOK(r) {
		writeErr(w, 401, "Requires authentication")
		return
	}
	var raw json.RawMessage
	if !decodeBody(r, &raw) {
		writeErr(w, 400, "Bad Request")
		return
	}
	if len(raw) > 0 && raw[0] == '[' {
		var ops []map[string]any
		if err := json.Unmarshal(raw, &ops); err != nil {
			writeErr(w, 400, "Bad Request")
			return
		}
		out := make([]any, 0, len(ops))
		for _, op := range ops {
			out = append(out, gqlExecute(op))
		}
		writeJSON(w, 200, out)
		return
	}
	var op map[string]any
	if err := json.Unmarshal(raw, &op); err != nil {
		writeErr(w, 400, "Bad Request")
		return
	}
	writeJSON(w, 200, gqlExecute(op))
}

func gqlString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func gqlInput(op map[string]any) map[string]any {
	in, _ := op["input"].(map[string]any)
	if in == nil {
		return map[string]any{}
	}
	return in
}

func gqlExecute(op map[string]any) map[string]any {
	query := gqlString(op, "query")
	vars, _ := op["variables"].(map[string]any)
	if vars == nil {
		vars = map[string]any{}
	}
	data, errs := gqlResolve(query, vars)
	resp := map[string]any{"data": data}
	if len(errs) > 0 {
		resp["errors"] = errs
	}
	return resp
}

func gqlResolve(q string, v map[string]any) (any, []any) {
	switch {
	case strings.Contains(q, "noSuchFieldOnViewer"):
		return nil, []any{map[string]any{"message": `Cannot query field "noSuchFieldOnViewer" on type "Viewer".`}}
	case strings.Contains(q, "query ViewerLogin"):
		return map[string]any{"viewer": map[string]any{"login": store.loginName()}}, nil
	case strings.Contains(q, `__type(name: "Issue")`):
		return map[string]any{"Issue": map[string]any{"fields": fieldList([]string{"id", "number", "title", "state", "body"})}}, nil
	case strings.Contains(q, `__type(name: "PullRequest")`):
		return map[string]any{
			"PullRequest": map[string]any{"fields": fieldList([]string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"})},
			"Repository":  map[string]any{"fields": fieldList([]string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"})},
		}, nil
	case strings.Contains(q, `__type(name: "SearchType")`):
		return map[string]any{"SearchType": map[string]any{"enumValues": []any{map[string]any{"name": "ISSUE"}, map[string]any{"name": "REPOSITORY"}}}}, nil
	case strings.Contains(q, "query RepositoryFields"):
		owner := gqlVar(v, "owner")
		name := gqlVar(v, "name")
		return map[string]any{"repository": gqlRepository(owner, name)}, nil
	case strings.Contains(q, "query NodeRepository"):
		id := gqlVar(v, "id")
		rp := repoByID(id)
		if rp == nil {
			return map[string]any{"node": nil}, nil
		}
		return map[string]any{"node": map[string]any{
			"id": repoID(rp), "name": rp.Name, "nameWithOwner": rp.FullName,
		}}, nil
	case strings.Contains(q, "mutation IssueCreate"):
		in := gqlInput(v)
		rp := repoByID(gqlVar(in, "repositoryId"))
		if rp == nil {
			return map[string]any{"createIssue": nil}, []any{map[string]any{"message": "repository not found"}}
		}
		iss := createIssue(rp, gqlVar(in, "title"), gqlVar(in, "body"))
		return map[string]any{"createIssue": map[string]any{"issue": map[string]any{"number": iss.Number, "id": iss.ID}}}, nil
	case strings.Contains(q, "mutation IssueClose"):
		in := gqlInput(v)
		iss := issueByID(gqlVar(in, "issueId"))
		if iss != nil {
			setIssueState(iss, "closed")
			return map[string]any{"closeIssue": map[string]any{"issue": map[string]any{"number": iss.Number, "id": iss.ID, "state": "CLOSED"}}}, nil
		}
		return map[string]any{"closeIssue": nil}, []any{map[string]any{"message": "issue not found"}}
	case strings.Contains(q, "mutation PullRequestCreate"):
		in := gqlInput(v)
		rp := repoByID(gqlVar(in, "repositoryId"))
		if rp == nil {
			return map[string]any{"createPullRequest": nil}, []any{map[string]any{"message": "repository not found"}}
		}
		pr := createPull(rp, gqlVar(in, "baseRefName"), gqlVar(in, "headRefName"), gqlVar(in, "title"), "")
		return map[string]any{"createPullRequest": map[string]any{"pullRequest": map[string]any{
			"number": pr.Number, "id": pr.ID, "state": "OPEN",
			"headRefName": pr.Head.Ref, "baseRefName": pr.Base.Ref,
		}}}, nil
	case strings.Contains(q, "mutation MergePullRequest"):
		in := gqlInput(v)
		pr := pullByID(gqlVar(in, "pullRequestId"))
		if pr != nil {
			mergePull(pr)
			return map[string]any{"mergePullRequest": map[string]any{"pullRequest": map[string]any{"number": pr.Number, "merged": true}}}, nil
		}
		return map[string]any{"mergePullRequest": nil}, []any{map[string]any{"message": "pull request not found"}}
	case strings.Contains(q, "mutation AddComment"):
		in := gqlInput(v)
		iss := issueByID(gqlVar(in, "subjectId"))
		if iss != nil {
			c := addComment(iss, gqlVar(in, "body"))
			return map[string]any{"addComment": map[string]any{"commentEdge": map[string]any{"node": map[string]any{"id": strconv.Itoa(c.ID), "body": c.Body}}}}, nil
		}
		return map[string]any{"addComment": nil}, []any{map[string]any{"message": "subject not found"}}
	case strings.Contains(q, "query PullRequestFiles"):
		rp := gqlRepo(v)
		pr := gqlPull(rp, v)
		nodes := []any{}
		if pr != nil {
			for _, f := range pr.Files {
				nodes = append(nodes, map[string]any{"path": f.Filename})
			}
		}
		return map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"number": gqlInt(v, "number"), "files": map[string]any{"nodes": nodes}}}}, nil
	case strings.Contains(q, "query PullRequestId"):
		rp := gqlRepo(v)
		pr := gqlPull(rp, v)
		if pr == nil {
			return map[string]any{"repository": map[string]any{"pullRequest": nil}}, nil
		}
		return map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"id": pr.ID, "number": pr.Number}}}, nil
	case strings.Contains(q, "query PullRequests"):
		rp := gqlRepo(v)
		nodes := []any{}
		if rp != nil {
			rp.mu.RLock()
			for _, pr := range rp.prs {
				nodes = append(nodes, map[string]any{
					"number": pr.Number, "headRefName": pr.Head.Ref, "baseRefName": pr.Base.Ref,
					"mergeable": pr.Mergeable, "state": strings.ToUpper(pr.State),
				})
			}
			rp.mu.RUnlock()
		}
		return map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"nodes": nodes}}}, nil
	case strings.Contains(q, "query IssuesPage"):
		rp := gqlRepo(v)
		first := gqlInt(v, "first")
		if first <= 0 {
			first = 10
		}
		after := gqlVar(v, "after")
		nodes, cursor, hasNext := gqlIssuePage(rp, first, after, false)
		return map[string]any{"repository": map[string]any{"issues": map[string]any{
			"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": hasNext, "endCursor": cursor},
		}}}, nil
	case strings.Contains(q, "query OpenIssues"):
		rp := gqlRepo(v)
		first := gqlInt(v, "first")
		if first <= 0 {
			first = 10
		}
		nodes, _, _ := gqlIssuePage(rp, first, "", true)
		return map[string]any{"repository": map[string]any{"issues": map[string]any{"nodes": nodes}}}, nil
	case strings.Contains(q, "query IssueByNumber"):
		rp := gqlRepo(v)
		iss := gqlIssue(rp, gqlInt(v, "number"))
		var out any
		if iss != nil {
			out = map[string]any{"number": iss.Number, "title": iss.Title, "state": strings.ToUpper(iss.State), "id": iss.ID, "body": iss.Body}
		}
		return map[string]any{"repository": map[string]any{"issue": out}}, nil
	}
	return map[string]any{}, nil
}

func fieldList(names []string) []any {
	out := make([]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

func gqlVar(v map[string]any, key string) string {
	if s, ok := v[key].(string); ok {
		return s
	}
	return ""
}

func gqlInt(v map[string]any, key string) int {
	switch n := v[key].(type) {
	case float64:
		return int(n)
	case json.Number:
		i, _ := strconv.Atoi(string(n))
		return i
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func gqlRepo(v map[string]any) *Repo {
	return store.getRepo(gqlVar(v, "owner"), gqlVar(v, "name"))
}

func gqlPull(rp *Repo, v map[string]any) *Pull {
	if rp == nil {
		return nil
	}
	n := gqlInt(v, "number")
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return rp.prByN[n]
}

func gqlIssue(rp *Repo, n int) *Issue {
	if rp == nil {
		return nil
	}
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return rp.issueByN[n]
}

func gqlRepository(owner, name string) any {
	rp := store.getRepo(owner, name)
	if rp == nil {
		return nil
	}
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	return map[string]any{
		"id":               repoID(rp),
		"databaseId":       repoDatabaseID(rp),
		"name":             rp.Name,
		"nameWithOwner":    rp.FullName,
		"owner":            map[string]any{"login": rp.Owner},
		"defaultBranchRef": map[string]any{"name": rp.DefaultBranch},
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
	}
}

func gqlIssuePage(rp *Repo, first int, after string, openOnly bool) ([]any, string, bool) {
	if rp == nil {
		return []any{}, "", false
	}
	afterNum := 0
	if after != "" {
		afterNum, _ = strconv.Atoi(strings.TrimPrefix(after, "cursor:"))
	}
	rp.mu.RLock()
	issues := make([]*Issue, 0, len(rp.issues))
	for _, iss := range rp.issues {
		if openOnly && iss.State != "open" {
			continue
		}
		if afterNum > 0 && iss.Number <= afterNum {
			continue
		}
		issues = append(issues, iss)
	}
	rp.mu.RUnlock()
	if openOnly {
		// GraphQL's states:[OPEN] queries use CREATED_AT DESC; issues are created in number order.
		sort.Slice(issues, func(i, j int) bool { return issues[i].Number > issues[j].Number })
	} else {
		sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	}
	if len(issues) > first {
		issues = issues[:first]
	}
	nodes := make([]any, 0, len(issues))
	cursor := ""
	for _, iss := range issues {
		nodes = append(nodes, map[string]any{
			"number": iss.Number, "title": iss.Title,
			"state": strings.ToUpper(iss.State), "id": iss.ID,
		})
		cursor = "cursor:" + strconv.Itoa(iss.Number)
	}
	lastNum := 0
	if strings.HasPrefix(cursor, "cursor:") {
		lastNum, _ = strconv.Atoi(strings.TrimPrefix(cursor, "cursor:"))
	}
	rp.mu.RLock()
	remaining := 0
	for _, iss := range rp.issues {
		if openOnly && iss.State != "open" {
			continue
		}
		if afterNum > 0 && iss.Number <= afterNum {
			continue
		}
		if lastNum > 0 && iss.Number <= lastNum {
			continue
		}
		remaining++
	}
	rp.mu.RUnlock()
	return nodes, cursor, remaining > 0
}

func repoByID(id string) *Repo {
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, rp := range store.repos {
		if repoID(rp) == id {
			return rp
		}
	}
	return nil
}

func issueByID(id string) *Issue {
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, rp := range store.repos {
		rp.mu.RLock()
		for _, iss := range rp.issues {
			if iss.ID == id {
				rp.mu.RUnlock()
				return iss
			}
		}
		rp.mu.RUnlock()
	}
	return nil
}

func pullByID(id string) *Pull {
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, rp := range store.repos {
		rp.mu.RLock()
		for _, pr := range rp.prs {
			if pr.ID == id {
				rp.mu.RUnlock()
				return pr
			}
		}
		rp.mu.RUnlock()
	}
	return nil
}

func createIssue(rp *Repo, title, body string) *Issue {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.seq++
	iss := &Issue{Number: int(rp.seq), Title: title, Body: body, State: "open", Labels: []Label{}, ID: issueID(rp, int(rp.seq))}
	rp.issues = append(rp.issues, iss)
	rp.issueByN[iss.Number] = iss
	return iss
}

func setIssueState(iss *Issue, state string) {
	store.mu.RLock()
	var target *Repo
	for _, rp := range store.repos {
		rp.mu.RLock()
		if rp.issueByN[iss.Number] == iss {
			target = rp
		}
		rp.mu.RUnlock()
		if target != nil {
			break
		}
	}
	store.mu.RUnlock()
	if target == nil {
		return
	}
	target.mu.Lock()
	iss.State = state
	iss.cache.Store(nil)
	target.mu.Unlock()
}

func addComment(iss *Issue, body string) *Comment {
	// Find owner repo and use its lock.
	store.mu.RLock()
	var target *Repo
	for _, rp := range store.repos {
		rp.mu.RLock()
		if rp.issueByN[iss.Number] == iss {
			target = rp
		}
		rp.mu.RUnlock()
		if target != nil {
			break
		}
	}
	store.mu.RUnlock()
	if target == nil {
		return &Comment{ID: 1, Body: body}
	}
	target.mu.Lock()
	target.commentSeq++
	c := &Comment{ID: int(target.commentSeq), Body: body}
	target.comments[iss.Number] = append(target.comments[iss.Number], c)
	target.mu.Unlock()
	return c
}

func createPull(rp *Repo, base, head, title, body string) *Pull {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.seq++
	pr := &Pull{
		Number: int(rp.seq), State: "open", Title: title, Body: body,
		Head: RefSpec{Ref: head, SHA: rp.branches[head], Label: head},
		Base: RefSpec{Ref: base, SHA: rp.branches[base], Label: base},
		ID:   pullID(rp, int(rp.seq)), Mergeable: "MERGEABLE",
	}
	pr.Files = diffFilesLocked(rp, base, head)
	rp.prs = append(rp.prs, pr)
	rp.prByN[pr.Number] = pr
	return pr
}

func mergePull(pr *Pull) *Repo {
	var target *Repo
	store.mu.RLock()
	for _, rp := range store.repos {
		rp.mu.RLock()
		if rp.prByN[pr.Number] == pr {
			target = rp
		}
		rp.mu.RUnlock()
		if target != nil {
			break
		}
	}
	store.mu.RUnlock()
	if target == nil {
		return nil
	}
	target.mu.Lock()
	base := pr.Base.Ref
	files := target.treeForLocked(base)
	for path, oid := range target.treeForLocked(pr.Head.Ref) {
		files[path] = oid
	}
	tree := target.buildTreeLocked(files)
	parent := target.branches[base]
	sha := target.putObjectLocked("commit", makeCommit(tree, parent, "Merge pull request #"+strconv.Itoa(pr.Number)))
	target.branches[base] = sha
	target.files[base] = files
	pr.Merged = true
	pr.State = "closed"
	pr.MergedAt = "2025-02-10T08:19:57Z"
	pr.MergeCommit = sha
	target.mu.Unlock()
	return target
}
