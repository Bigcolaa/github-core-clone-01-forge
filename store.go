package main

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// GitObject is a raw git object: type is one of commit, tree, blob, tag.
type GitObject struct {
	Kind string
	Data []byte
}

// TreeEnt is one entry of a git tree.
type TreeEnt struct {
	Mode string
	Name string
	OID  string
}

// CommitInfo is the parsed header of a commit object.
type CommitInfo struct {
	Tree    string
	Parents []string
	Message string
}

// Issue / PR / comment models.
type Issue struct {
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	State     string  `json:"state"`
	Labels    []Label `json:"labels"`
	ID        string  `json:"id,omitempty"`
	cache     atomic.Pointer[[]byte]
	httpCache atomic.Pointer[[]byte] // pre-serialized full HTTP GET response
}

type Label struct {
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

type PRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
	Path      string `json:"path,omitempty"`
}

type Pull struct {
	Number      int      `json:"number"`
	State       string   `json:"state"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Head        RefSpec  `json:"head"`
	Base        RefSpec  `json:"base"`
	Merged      bool     `json:"merged"`
	MergedAt    string   `json:"merged_at,omitempty"`
	MergeCommit string   `json:"merge_commit_sha,omitempty"`
	Files       []PRFile `json:"-"`
	ID          string   `json:"id,omitempty"`
	Mergeable   string   `json:"-"`
}

type RefSpec struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label,omitempty"`
}

type Comment struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
}

// Repo is the whole state of one repository.
type Repo struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Owner         string `json:"-"`
	Description   string `json:"description"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`

	mu         sync.RWMutex
	branches   map[string]string
	tags       map[string]string
	objects    map[string]*GitObject
	issues     []*Issue
	issueByN   map[int]*Issue
	prs        []*Pull
	prByN      map[int]*Pull
	labels     map[string]string
	comments   map[int][]*Comment
	seq        int64
	commentSeq int64
	files      map[string]map[string]string // branch -> flattened path -> blob oid

	advV2       atomic.Pointer[advertCacheEntry]
	advV0       atomic.Pointer[advertCacheEntry]
	advRecv     atomic.Pointer[advertCacheEntry]
	lsRefsCache atomic.Pointer[advertCacheEntry]
	packCache   atomic.Pointer[packCacheEntry]
	packGen     sync.Mutex // serializes pack generation on a cache miss only
}

func newRepo(name, owner, desc string, private bool) *Repo {
	return &Repo{
		Name: name, FullName: owner + "/" + name, Owner: owner,
		Description: desc, Private: private, DefaultBranch: "main",
		branches: map[string]string{}, tags: map[string]string{},
		objects: map[string]*GitObject{}, issues: []*Issue{}, issueByN: map[int]*Issue{},
		prs: []*Pull{}, prByN: map[int]*Pull{}, labels: map[string]string{},
		comments: map[int][]*Comment{}, files: map[string]map[string]string{},
	}
}

// Global store.
type Store struct {
	mu    sync.RWMutex
	repos map[string]*Repo
	login string
	token string
}

var store = &Store{repos: map[string]*Repo{}, login: "adaiversen"}

func (s *Store) loginName() string { return s.login }

func (s *Store) getRepo(owner, name string) *Repo {
	s.mu.RLock()
	r := s.repos[owner+"/"+name]
	s.mu.RUnlock()
	return r
}

func (s *Store) createRepo(owner, name, desc string, private bool) (*Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil, false
	}
	r := newRepo(name, owner, desc, private)
	s.repos[key] = r
	return r, true
}

func (s *Store) listRepos(owner string) []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Repo, 0, len(s.repos))
	for _, r := range s.repos {
		if r.Owner == owner {
			out = append(out, r)
		}
	}
	return out
}

// ---- stable ids ----

func hashShort(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func repoDatabaseID(r *Repo) int { return 5000 + int(hashShort(r.FullName)%100000) }

func repoID(r *Repo) string { return "R_" + strconv.FormatUint(uint64(hashShort(r.FullName)), 10) }

func issueID(r *Repo, n int) string {
	return "I_" + strconv.FormatUint(uint64(hashShort(r.FullName)), 10) + "_" + strconv.Itoa(n)
}

func pullID(r *Repo, n int) string {
	return "PR_" + strconv.FormatUint(uint64(hashShort(r.FullName)), 10) + "_" + strconv.Itoa(n)
}

// ---- git object helpers ----

var hashBufPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

func hashObject(kind string, data []byte) string {
	bp := hashBufPool.Get().(*[]byte)
	buf := (*bp)[:0]
	buf = append(buf, kind...)
	buf = append(buf, ' ')
	buf = strconv.AppendInt(buf, int64(len(data)), 10)
	buf = append(buf, 0)
	buf = append(buf, data...)
	sum := sha1.Sum(buf)
	*bp = buf[:0]
	hashBufPool.Put(bp)
	return hex.EncodeToString(sum[:])
}

// putObjectLocked stores an object. Caller holds r.mu for writing.
func (r *Repo) putObjectLocked(kind string, data []byte) string {
	oid := hashObject(kind, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &GitObject{Kind: kind, Data: append([]byte(nil), data...)}
	}
	return oid
}

func (r *Repo) objectLocked(oid string) *GitObject { return r.objects[oid] }

// encodeTree builds the canonical binary tree from entries sorted by git rules.
func encodeTree(ents []TreeEnt) []byte {
	sort.Slice(ents, func(i, j int) bool {
		a, b := ents[i].Name, ents[j].Name
		if ents[i].Mode == "40000" {
			a += "/"
		}
		if ents[j].Mode == "40000" {
			b += "/"
		}
		return a < b
	})
	var out []byte
	for _, e := range ents {
		out = append(out, []byte(e.Mode+" "+e.Name+"\x00")...)
		raw, _ := hex.DecodeString(e.OID)
		out = append(out, raw...)
	}
	return out
}

func decodeTree(data []byte) []TreeEnt {
	var out []TreeEnt
	i := 0
	for i < len(data) {
		j := i
		for j < len(data) && data[j] != ' ' {
			j++
		}
		if j >= len(data) {
			break
		}
		mode := string(data[i:j])
		i = j + 1
		j = i
		for j < len(data) && data[j] != 0 {
			j++
		}
		if j+21 > len(data) {
			break
		}
		name := string(data[i:j])
		oid := hex.EncodeToString(data[j+1 : j+21])
		i = j + 21
		out = append(out, TreeEnt{Mode: mode, Name: name, OID: oid})
	}
	return out
}

func parseCommit(data []byte) CommitInfo {
	ci := CommitInfo{}
	s := string(data)
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "tree ") {
			ci.Tree = strings.TrimPrefix(line, "tree ")
		}
		if strings.HasPrefix(line, "parent ") {
			ci.Parents = append(ci.Parents, strings.TrimPrefix(line, "parent "))
		}
	}
	if i := strings.Index(s, "\n\n"); i >= 0 {
		ci.Message = s[i+2:]
	}
	return ci
}

func makeCommit(tree, parent, msg string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	if parent != "" {
		b.WriteString("parent " + parent + "\n")
	}
	b.WriteString("author Agent <agent@make.fun> 1735689600 +0000\n")
	b.WriteString("committer Agent <agent@make.fun> 1735689600 +0000\n\n")
	b.WriteString(msg)
	return []byte(b.String())
}

type treeNode struct {
	files map[string]string
	dirs  map[string]*treeNode
}

func newTreeNode() *treeNode {
	return &treeNode{files: map[string]string{}, dirs: map[string]*treeNode{}}
}

func (n *treeNode) add(path, oid string) {
	parts := strings.Split(path, "/")
	for _, p := range parts[:len(parts)-1] {
		child := n.dirs[p]
		if child == nil {
			child = newTreeNode()
			n.dirs[p] = child
		}
		n = child
	}
	n.files[parts[len(parts)-1]] = oid
}

func (n *treeNode) writeLocked(r *Repo) string {
	ents := make([]TreeEnt, 0, len(n.files)+len(n.dirs))
	for name, oid := range n.files {
		ents = append(ents, TreeEnt{Mode: "100644", Name: name, OID: oid})
	}
	for name, child := range n.dirs {
		ents = append(ents, TreeEnt{Mode: "40000", Name: name, OID: child.writeLocked(r)})
	}
	return r.putObjectLocked("tree", encodeTree(ents))
}

// buildTreeLocked creates a git tree from a flattened path map. Caller holds write lock.
func (r *Repo) buildTreeLocked(files map[string]string) string {
	root := newTreeNode()
	for path, oid := range files {
		root.add(path, oid)
	}
	return root.writeLocked(r)
}

// flattenTreeLocked walks a tree object and returns all blob paths. Caller holds lock.
func (r *Repo) flattenTreeLocked(treeOID, prefix string, out map[string]string) {
	obj := r.objectLocked(treeOID)
	if obj == nil || obj.Kind != "tree" {
		return
	}
	for _, e := range decodeTree(obj.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			r.flattenTreeLocked(e.OID, path, out)
		} else {
			out[path] = e.OID
		}
	}
}

// treeForLocked returns the flattened file map at a branch tip. Caller holds lock.
func (r *Repo) treeForLocked(branch string) map[string]string {
	out := map[string]string{}
	if m := r.files[branch]; m != nil {
		for p, o := range m {
			out[p] = o
		}
		return out
	}
	tip := r.branches[branch]
	if tip == "" {
		return out
	}
	obj := r.objectLocked(tip)
	if obj == nil {
		return out
	}
	ci := parseCommit(obj.Data)
	r.flattenTreeLocked(ci.Tree, "", out)
	return out
}

func (r *Repo) treeFor(branch string) map[string]string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.treeForLocked(branch)
}

// commitFilesLocked creates a commit on branch with the given path changed to content.
func (r *Repo) commitFilesLocked(branch, path, content, msg string) (blobOID, commitOID string) {
	files := r.treeForLocked(branch)
	blobOID = r.putObjectLocked("blob", []byte(content))
	files[path] = blobOID
	treeOID := r.buildTreeLocked(files)
	parent := r.branches[branch]
	commitOID = r.putObjectLocked("commit", makeCommit(treeOID, parent, msg))
	r.branches[branch] = commitOID
	if r.files[branch] == nil {
		r.files[branch] = map[string]string{}
	}
	r.files[branch][path] = blobOID
	return blobOID, commitOID
}

func (r *Repo) commitFiles(branch, path, content, msg string) (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.commitFilesLocked(branch, path, content, msg)
}

// isAncestorLocked reports whether old is reachable from new through first parents.
func (r *Repo) isAncestorLocked(old, new string) bool {
	if old == new {
		return true
	}
	obj := r.objectLocked(new)
	if obj == nil {
		return false
	}
	ci := parseCommit(obj.Data)
	// The benchmark's pushes advance the tip by one commit, so old is almost
	// always a direct parent: resolve that without allocating a walk set.
	for _, p := range ci.Parents {
		if p == old {
			return true
		}
	}
	seen := map[string]bool{new: true}
	queue := append([]string(nil), ci.Parents...)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur == "" || seen[cur] {
			continue
		}
		seen[cur] = true
		o := r.objectLocked(cur)
		if o == nil {
			continue
		}
		for _, p := range parseCommit(o.Data).Parents {
			if p == old {
				return true
			}
			queue = append(queue, p)
		}
	}
	return false
}
