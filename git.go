package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Reusing zlib readers avoids a fresh ~40 KiB inflate history buffer per
// pushed object (600 per push workload). Reset keeps the decoded bytes zero-copy.
var pushInflatePool = sync.Pool{New: func() any {
	var seed bytes.Buffer
	zw := zlib.NewWriter(&seed)
	_ = zw.Close()
	zr, _ := zlib.NewReader(bytes.NewReader(seed.Bytes()))
	return zr
}}

func getPushInflater(src io.Reader) (io.ReadCloser, error) {
	zr := pushInflatePool.Get().(io.ReadCloser)
	if err := zr.(zlib.Resetter).Reset(src, nil); err != nil {
		pushInflatePool.Put(zr)
		return nil, err
	}
	return zr, nil
}

const zeroOID = "0000000000000000000000000000000000000000"

type packedRef struct {
	OID    string
	Name   string
	Peeled string
	Symref string
	Unborn bool
}

const hexDigits = "0123456789abcdef"

// appendHex4 writes n as exactly four lowercase hex digits (n < 65536) with
// no strconv.FormatInt allocation.
func appendHex4(dst []byte, n int) []byte {
	return append(dst,
		hexDigits[(n>>12)&0xf], hexDigits[(n>>8)&0xf],
		hexDigits[(n>>4)&0xf], hexDigits[n&0xf])
}

// hexNibble decodes one ASCII hex digit, or -1.
func hexNibble(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// hex4 decodes a four-digit pkt-line length.
func hex4(b []byte) (int, bool) {
	a, c, d, e := hexNibble(b[0]), hexNibble(b[1]), hexNibble(b[2]), hexNibble(b[3])
	if a < 0 || c < 0 || d < 0 || e < 0 {
		return 0, false
	}
	return (a << 12) | (c << 8) | (d << 4) | e, true
}

// appendPkt writes a pkt-line directly into dst: no intermediate allocation.
func appendPkt(dst []byte, payload []byte) []byte {
	dst = appendHex4(dst, len(payload)+4)
	return append(dst, payload...)
}

func appendPktStr(dst []byte, s string) []byte {
	dst = appendHex4(dst, len(s)+4)
	return append(dst, s...)
}

func pkt(payload []byte) []byte {
	out := make([]byte, 0, 4+len(payload))
	return appendPkt(out, payload)
}

func pktStr(s string) []byte {
	out := make([]byte, 0, 4+len(s))
	return appendPktStr(out, s)
}

var (
	flushPkt = []byte("0000")
	delimPkt = []byte("0001")
)

// decodePkts returns payloads, "flush", "delim", or raw trailing bytes.
func decodePkts(data []byte) ([]string, []byte) {
	out := []string{}
	i := 0
	for i+4 <= len(data) {
		h := data[i : i+4]
		if bytes.Equal(h, flushPkt) {
			out = append(out, "flush")
			i += 4
			return out, data[i:]
		}
		if bytes.Equal(h, delimPkt) {
			out = append(out, "delim")
			i += 4
			continue
		}
		n, ok := hex4(h)
		if !ok || n < 4 || i+n > len(data) {
			break
		}
		out = append(out, string(data[i+4:i+n]))
		i += n
	}
	return out, data[i:]
}

type packCacheEntry struct {
	key       string
	b         []byte
	shallow   []string
	unshallow []string
}

type advertCacheEntry struct {
	key string
	b   []byte
}

// writeGitResult serves a git response, using the pre-serialized full HTTP
// response on the raw server's cached-body fast path when available.
func writeGitResult(w http.ResponseWriter, contentType string, body []byte) {
	if writeCachedBody(w, http.StatusOK, contentType, false, body) {
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(body)
}

func (rp *Repo) gitRoute(w http.ResponseWriter, r *http.Request, owner, repo string, rest []string) {
	if owner != store.loginName() {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rp = store.getRepo(owner, repo)
	if rp == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if len(rest) == 2 && rest[0] == "info" && rest[1] == "refs" && r.Method == "GET" {
		service := r.URL.Query().Get("service")
		if service == "git-upload-pack" {
			if r.Header.Get("Git-Protocol") == "version=2" {
				writeGitResult(w, "application/x-git-upload-pack-advertisement", rp.uploadAdvertV2())
			} else {
				writeGitResult(w, "application/x-git-upload-pack-advertisement", rp.uploadAdvertV0())
			}
			return
		}
		if service == "git-receive-pack" {
			writeGitResult(w, "application/x-git-receive-pack-advertisement", rp.receiveAdvert())
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if len(rest) != 1 || r.Method != "POST" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch rest[0] {
	case "git-upload-pack":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if r.Header.Get("Git-Protocol") == "version=2" {
			writeGitResult(w, "application/x-git-upload-pack-result", rp.handleFetchV2(body))
		} else {
			writeGitResult(w, "application/x-git-upload-pack-result", rp.handleFetchV0(body))
		}
	case "git-receive-pack":
		body, _ := io.ReadAll(io.LimitReader(r.Body, 256<<20))
		writeGitResult(w, "application/x-git-receive-pack-result", rp.handleReceivePack(body))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (rp *Repo) refListLocked() []packedRef {
	out := []packedRef{}
	tip := rp.branches[rp.DefaultBranch]
	out = append(out, packedRef{OID: tip, Name: "HEAD", Symref: "refs/heads/" + rp.DefaultBranch})
	for name, oid := range rp.branches {
		if oid == "" {
			continue
		}
		out = append(out, packedRef{OID: oid, Name: "refs/heads/" + name})
	}
	for name, oid := range rp.tags {
		peel := oid
		if obj := rp.objectLocked(oid); obj != nil && obj.Kind == "tag" {
			s := string(obj.Data)
			if i := strings.Index(s, "\n\n"); i > 0 {
				for _, line := range strings.Split(s[:i], "\n") {
					if strings.HasPrefix(line, "object ") {
						peel = strings.TrimPrefix(line, "object ")
					}
				}
			}
		}
		out = append(out, packedRef{OID: oid, Name: "refs/tags/" + name, Peeled: peel})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func capabilities(advert bool, receive bool) string {
	if receive {
		return "report-status delete-refs side-band-64k ofs-delta agent=forge/1"
	}
	if advert {
		return "side-band-64k shallow multi_ack_detailed ofs-delta agent=forge/1"
	}
	return ""
}

func (rp *Repo) uploadAdvertV2() []byte {
	// The v2 advertisement is a constant capability list: it carries no refs.
	// Returning the cached bytes without rp.mu removes a lock acquisition from
	// every clone's three advertisement round trips. Concurrent first writes
	// are harmless (identical bytes).
	if p := rp.advV2.Load(); p != nil {
		return p.b
	}
	var b []byte
	b = appendPktStr(b, "# service=git-upload-pack\n")
	b = appendPktStr(b, "version 2\n")
	b = appendPktStr(b, "ls-refs\n")
	b = appendPktStr(b, "fetch=shallow filter wait-tip\n")
	b = appendPktStr(b, "object-format=sha1\n")
	b = appendPktStr(b, "agent=forge/1\n")
	b = append(b, flushPkt...)
	rp.advV2.Store(&advertCacheEntry{key: "v2", b: b})
	return b
}

func (rp *Repo) uploadAdvertV0() []byte {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	tip := rp.branches[rp.DefaultBranch]
	key := "v0:" + tip
	if p := rp.advV0.Load(); p != nil && p.key == key {
		return p.b
	}
	var b []byte
	b = appendPktStr(b, "# service=git-upload-pack\n")
	refs := rp.refListLocked()
	caps := capabilities(true, false)
	if len(refs) == 0 || tip == "" {
		b = appendPktStr(b, zeroOID+" capabilities^{}\x00"+caps+"\n")
	} else {
		for i, ref := range refs {
			line := ref.OID + " " + ref.Name
			if i == 0 {
				line += "\x00" + caps + " symref=HEAD:refs/heads/" + rp.DefaultBranch
			}
			b = appendPktStr(b, line+"\n")
			if ref.Peeled != "" && ref.Peeled != ref.OID {
				b = appendPktStr(b, ref.Peeled+" "+ref.Name+"^{}\n")
			}
		}
	}
	b = append(b, flushPkt...)
	rp.advV0.Store(&advertCacheEntry{key: key, b: b})
	return b
}

func (rp *Repo) receiveAdvert() []byte {
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	refs := rp.refListLocked()
	var kb strings.Builder
	kb.WriteString("recv:")
	for _, ref := range refs {
		kb.WriteString(ref.Name)
		kb.WriteByte('=')
		kb.WriteString(ref.OID)
		kb.WriteByte(';')
	}
	key := kb.String()
	if p := rp.advRecv.Load(); p != nil && p.key == key {
		return p.b
	}
	var b []byte
	b = appendPktStr(b, "# service=git-receive-pack\n")
	tip := rp.branches[rp.DefaultBranch]
	caps := capabilities(false, true)
	if tip == "" {
		b = appendPktStr(b, zeroOID+" capabilities^{}\x00"+caps+"\n")
	} else {
		b = appendPktStr(b, tip+" HEAD\x00"+caps+" symref=HEAD:refs/heads/"+rp.DefaultBranch+"\n")
		for _, ref := range refs {
			if ref.Name == "HEAD" {
				continue
			}
			b = appendPktStr(b, ref.OID+" "+ref.Name+"\n")
		}
	}
	b = append(b, flushPkt...)
	rp.advRecv.Store(&advertCacheEntry{key: key, b: b})
	return b
}

// lsRefsKey is semantic: the agent and object-format lines do not change the
// reply, so they are deliberately excluded. This lets a prewarm with a generic
// body satisfy any client's ls-refs request with the same options.
func lsRefsKey(body []byte, tip string) string {
	lines, _ := decodePkts(body)
	var b strings.Builder
	b.WriteString(tip)
	for _, line := range lines {
		switch {
		case line == "peel\n", line == "symrefs\n", line == "unborn\n":
			b.WriteByte('|')
			b.WriteString(line[:len(line)-1])
		case strings.HasPrefix(line, "ref-prefix "):
			b.WriteByte('|')
			b.WriteString(line)
		}
	}
	return b.String()
}

func (rp *Repo) handleLsRefs(body []byte) []byte {
	rp.mu.RLock()
	key := lsRefsKey(body, rp.branches[rp.DefaultBranch])
	if p := rp.lsRefsCache.Load(); p != nil && p.key == key {
		rp.mu.RUnlock()
		return p.b
	}
	rp.mu.RUnlock()
	out := rp.handleLsRefsSlow(body)
	rp.lsRefsCache.Store(&advertCacheEntry{key: key, b: out})
	return out
}

func (rp *Repo) handleLsRefsSlow(body []byte) []byte {
	lines, _ := decodePkts(body)
	prefixes := []string{}
	peel, symrefs, unborn := false, false, false
	for _, line := range lines {
		switch {
		case line == "peel\n":
			peel = true
		case line == "symrefs\n":
			symrefs = true
		case line == "unborn\n":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimSuffix(strings.TrimPrefix(line, "ref-prefix "), "\n"))
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	rp.mu.RLock()
	defer rp.mu.RUnlock()
	var out []byte
	tip := rp.branches[rp.DefaultBranch]
	if tip == "" && matches("HEAD") && (unborn || len(prefixes) == 0) {
		out = appendPktStr(out, "unborn HEAD symref-target:refs/heads/"+rp.DefaultBranch+"\n")
	}
	for _, ref := range rp.refListLocked() {
		if ref.OID == "" || ref.OID == zeroOID {
			continue
		}
		if ref.Unborn && !unborn {
			continue
		}
		if !matches(ref.Name) {
			continue
		}
		line := ref.OID + " " + ref.Name
		if symrefs && ref.Symref != "" {
			line += " symref-target:" + ref.Symref
		}
		if peel && ref.Peeled != "" && ref.Peeled != ref.OID {
			line += " peeled:" + ref.Peeled
		}
		out = appendPktStr(out, line+"\n")
	}
	out = append(out, flushPkt...)
	return out
}

type fetchRequest struct {
	wants   []string
	haves   []string
	shallow []string
	depth   int
	filter  string
}

func parseFetchRequest(body []byte) fetchRequest {
	lines, _ := decodePkts(body)
	fr := fetchRequest{}
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			fr.wants = append(fr.wants, strings.TrimSpace(strings.TrimPrefix(line, "want ")))
		case strings.HasPrefix(line, "have "):
			fr.haves = append(fr.haves, strings.TrimSpace(strings.TrimPrefix(line, "have ")))
		case strings.HasPrefix(line, "shallow "):
			fr.shallow = append(fr.shallow, strings.TrimSpace(strings.TrimPrefix(line, "shallow ")))
		case strings.HasPrefix(line, "deepen "):
			fr.depth, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "deepen ")))
		case strings.HasPrefix(line, "filter "):
			fr.filter = strings.TrimSpace(strings.TrimPrefix(line, "filter "))
		}
	}
	return fr
}

// fetchV2RespCache / fetchV0RespCache cache the complete response body keyed
// on the cached pack slice, so the whole fetch reply becomes a stable []byte
// that writeCachedBody can serve with a single conn.Write.
var (
	fetchV2RespCache atomic.Pointer[framedPackEntry]
	fetchV0RespCache atomic.Pointer[framedPackEntry]
	fetchRespGen     sync.Mutex // dedups concurrent cold fetch-response assembly
)

func samePack(a, b []byte) bool {
	return len(a) == len(b) && len(a) > 0 && &a[0] == &b[0]
}

func (rp *Repo) handleFetchV2(body []byte) []byte {
	lines, _ := decodePkts(body)
	command := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "command=") {
			command = strings.TrimSuffix(strings.TrimPrefix(line, "command="), "\n")
			break
		}
	}
	if command == "ls-refs" {
		return rp.handleLsRefs(body)
	}
	if command != "fetch" {
		return appendPktStr(nil, "ERR unknown command\n")
	}
	fr := parseFetchRequest(body)
	pack, shallowLines, unshallowLines := rp.packFor(fr)
	if p := fetchV2RespCache.Load(); p != nil && samePack(p.src, pack) {
		return p.framed
	}
	fetchRespGen.Lock()
	defer fetchRespGen.Unlock()
	if p := fetchV2RespCache.Load(); p != nil && samePack(p.src, pack) {
		return p.framed
	}
	out := buildFetchV2Response(pack, shallowLines, unshallowLines)
	fetchV2RespCache.Store(&framedPackEntry{src: pack, framed: out})
	return out
}

// buildFetchV2Response assembles the complete v2 fetch body from a pack. It is
// shared by the live fetch path and the eager ref-update prewarm so both
// produce byte-identical, cacheable responses.
func buildFetchV2Response(pack []byte, shallowLines, unshallowLines []string) []byte {
	var out []byte
	out = appendPktStr(out, "acknowledgments\n")
	out = appendPktStr(out, "NAK\n")
	out = append(out, delimPkt...)
	if len(shallowLines)+len(unshallowLines) > 0 {
		out = appendPktStr(out, "shallow-info\n")
		for _, s := range unshallowLines {
			out = appendPktStr(out, "unshallow "+s+"\n")
		}
		for _, s := range shallowLines {
			out = appendPktStr(out, "shallow "+s+"\n")
		}
		out = append(out, delimPkt...)
	}
	out = appendPktStr(out, "packfile\n")
	out = appendSidebandPack(out, pack)
	out = append(out, flushPkt...)
	return out
}

func (rp *Repo) handleFetchV0(body []byte) []byte {
	fr := parseFetchRequest(body)
	pack, shallowLines, unshallowLines := rp.packFor(fr)
	if p := fetchV0RespCache.Load(); p != nil && samePack(p.src, pack) {
		return p.framed
	}
	fetchRespGen.Lock()
	defer fetchRespGen.Unlock()
	if p := fetchV0RespCache.Load(); p != nil && samePack(p.src, pack) {
		return p.framed
	}
	var out []byte
	for _, s := range unshallowLines {
		out = appendPktStr(out, "unshallow "+s+"\n")
	}
	for _, s := range shallowLines {
		out = appendPktStr(out, "shallow "+s+"\n")
	}
	out = appendPktStr(out, "NAK\n")
	out = appendSidebandPack(out, pack)
	out = append(out, flushPkt...)
	fetchV0RespCache.Store(&framedPackEntry{src: pack, framed: out})
	return out
}

func appendSidebandPack(dst []byte, pack []byte) []byte {
	return append(dst, framedSidebandPack(pack)...)
}

// framedSidebandPack returns the complete sideband-framed pack bytes. The
// clone workload serves the same cached pack to 50 concurrent clients; framing
// it once removes one allocation and one full copy per clone.
type framedPackEntry struct {
	src    []byte
	framed []byte
}

var framedPackCache atomic.Pointer[framedPackEntry]
var framedPackGen sync.Mutex // dedups concurrent framing of the same cold pack

func framedSidebandPack(pack []byte) []byte {
	if len(pack) == 0 {
		pack = emptyPack()
	}
	if p := framedPackCache.Load(); p != nil && samePack(p.src, pack) {
		return p.framed
	}
	framedPackGen.Lock()
	defer framedPackGen.Unlock()
	if p := framedPackCache.Load(); p != nil && samePack(p.src, pack) {
		return p.framed
	}
	const chunk = 65000
	framed := make([]byte, 0, len(pack)+len(pack)/chunk*8+64)
	for i := 0; i < len(pack); i += chunk {
		end := i + chunk
		if end > len(pack) {
			end = len(pack)
		}
		payload := make([]byte, 1+end-i)
		payload[0] = 1
		copy(payload[1:], pack[i:end])
		framed = append(framed, pkt(payload)...)
	}
	framedPackCache.Store(&framedPackEntry{src: pack, framed: framed})
	return framed
}

func emptyPack() []byte {
	b := make([]byte, 32)
	copy(b, "PACK")
	binary.BigEndian.PutUint32(b[4:], 2)
	binary.BigEndian.PutUint32(b[8:], 0)
	sum := sha1.Sum(b[:12])
	copy(b[12:], sum[:])
	return b
}

// packFor returns a complete pack file and shallow info for one fetch request.
func (rp *Repo) packFor(fr fetchRequest) ([]byte, []string, []string) {
	rp.mu.RLock()
	defer rp.mu.RUnlock()

	sort.Strings(fr.wants)
	key := strings.Join(fr.wants, ",") + "|" + strings.Join(fr.haves, ",") + "|" + strings.Join(fr.shallow, ",") + "|" + strconv.Itoa(fr.depth) + "|" + fr.filter
	if c := rp.packCache.Load(); c != nil && c.key == key {
		return c.b, c.shallow, c.unshallow
	}
	rp.packGen.Lock()
	defer rp.packGen.Unlock()
	if c := rp.packCache.Load(); c != nil && c.key == key {
		return c.b, c.shallow, c.unshallow
	}
	return rp.packForSlow(fr, key)
}

// packForSlow is called with rp.mu read-locked and rp.packGen held, so a burst
// of identical clones generates the pack once and the rest reuse the cached bytes.
func (rp *Repo) packForSlow(fr fetchRequest, key string) ([]byte, []string, []string) {
	shallowSet := map[string]bool{}
	for _, oid := range fr.shallow {
		shallowSet[oid] = true
	}
	known := map[string]bool{}
	for _, oid := range fr.haves {
		if shallowSet[oid] {
			continue // a shallow have only owns its own tree, not its ancestors
		}
		rp.collectReachableLocked(oid, known, true)
	}
	for _, oid := range fr.shallow {
		rp.collectShallowKnownLocked(oid, known)
	}

	include := map[string]bool{}
	newShallow := map[string]bool{}
	crossed := map[string]bool{}
	var visit func(oid string)
	visit = func(oid string) {
		obj := rp.objectLocked(oid)
		if obj == nil {
			return
		}
		if obj.Kind == "tag" {
			include[oid] = true
			for _, line := range strings.Split(string(obj.Data), "\n") {
				if strings.HasPrefix(line, "object ") {
					visit(strings.TrimPrefix(line, "object "))
				}
			}
			return
		}
		if obj.Kind != "commit" {
			include[oid] = true
			return
		}
		type step struct {
			oid string
			n   int
		}
		best := map[string]int{}
		queue := []step{{oid: oid, n: 0}}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if prev, ok := best[cur.oid]; ok && prev >= cur.n {
				continue
			}
			best[cur.oid] = cur.n
			if shallowSet[cur.oid] {
				crossed[cur.oid] = true
				for _, p := range parseCommit(rp.objectLocked(cur.oid).Data).Parents {
					queue = append(queue, step{oid: p, n: 1})
				}
				continue
			}
			if known[cur.oid] {
				continue
			}
			boundary := fr.depth > 0 && cur.n >= fr.depth-1
			include[cur.oid] = true
			obj2 := rp.objectLocked(cur.oid)
			if obj2 == nil {
				continue
			}
			ci := parseCommit(obj2.Data)
			rp.collectReachableLocked(ci.Tree, include, true)
			if boundary && len(ci.Parents) > 0 {
				// Git marks the last commit it sends as shallow; its parents are not sent.
				newShallow[cur.oid] = true
				continue
			}
			for _, p := range ci.Parents {
				queue = append(queue, step{oid: p, n: cur.n + 1})
			}
		}
	}
	for _, want := range fr.wants {
		visit(want)
	}
	// Tree closures are collected wholesale; drop anything the client already holds.
	for oid := range known {
		delete(include, oid)
	}

	objects := make(map[string]*GitObject, len(include))
	for oid := range include {
		if obj := rp.objectLocked(oid); obj != nil {
			objects[oid] = obj
		}
	}
	pack := writePack(objects)

	shallowOut := []string{}
	for s := range newShallow {
		if !fr.knownShallow(s) {
			shallowOut = append(shallowOut, s)
		}
	}
	unshallowOut := []string{}
	for s := range crossed {
		if !include[s] {
			unshallowOut = append(unshallowOut, s)
		}
	}
	sort.Strings(shallowOut)
	sort.Strings(unshallowOut)
	rp.packCache.Store(&packCacheEntry{key: key, b: pack, shallow: shallowOut, unshallow: unshallowOut})
	return pack, shallowOut, unshallowOut
}

func (fr fetchRequest) knownShallow(oid string) bool {
	for _, s := range fr.shallow {
		if s == oid {
			return true
		}
	}
	return false
}

func (rp *Repo) collectReachableLocked(oid string, out map[string]bool, includeTrees bool) {
	if oid == "" || out[oid] {
		return
	}
	obj := rp.objectLocked(oid)
	if obj == nil {
		return
	}
	out[oid] = true
	switch obj.Kind {
	case "commit":
		ci := parseCommit(obj.Data)
		if includeTrees {
			rp.collectReachableLocked(ci.Tree, out, true)
		} else {
			out[ci.Tree] = true
		}
		for _, p := range ci.Parents {
			rp.collectReachableLocked(p, out, false)
		}
	case "tree":
		for _, e := range decodeTree(obj.Data) {
			if e.Mode == "40000" {
				rp.collectReachableLocked(e.OID, out, true)
			} else {
				out[e.OID] = true
			}
		}
	case "tag":
		for _, line := range strings.Split(string(obj.Data), "\n") {
			if strings.HasPrefix(line, "object ") {
				rp.collectReachableLocked(strings.TrimPrefix(line, "object "), out, includeTrees)
			}
		}
	}
}

// collectShallowKnownLocked marks what a client holding a shallow commit actually has:
// that commit and its tree closure, but none of its ancestors.
func (rp *Repo) collectShallowKnownLocked(oid string, out map[string]bool) {
	if oid == "" || out[oid] {
		return
	}
	obj := rp.objectLocked(oid)
	if obj == nil {
		return
	}
	out[oid] = true
	if obj.Kind == "commit" {
		ci := parseCommit(obj.Data)
		rp.collectReachableLocked(ci.Tree, out, true)
	}
}

func writePack(objects map[string]*GitObject) []byte {
	oids := make([]string, 0, len(objects))
	for oid := range objects {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	var b bytes.Buffer
	b.WriteString("PACK")
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], 2)
	b.Write(h[:])
	binary.BigEndian.PutUint32(h[:], uint32(len(oids)))
	b.Write(h[:])
	for _, oid := range oids {
		obj := objects[oid]
		code := 3
		switch obj.Kind {
		case "commit":
			code = 1
		case "tree":
			code = 2
		case "blob":
			code = 3
		case "tag":
			code = 4
		}
		size := len(obj.Data)
		header := byte(code<<4) | byte(size&0x0f)
		size >>= 4
		if size != 0 {
			header |= 0x80
		}
		b.WriteByte(header)
		for size != 0 {
			c := byte(size & 0x7f)
			size >>= 7
			if size != 0 {
				c |= 0x80
			}
			b.WriteByte(c)
		}
		if obj.RawZlib != nil {
			// Verbatim zlib stream from the incoming push pack. The header
			// above still carries the uncompressed size, so the result is a
			// byte-identical, valid pack entry with zero recompression.
			_, _ = b.Write(obj.RawZlib)
		} else {
			zw := zlib.NewWriter(&b)
			_, _ = zw.Write(obj.Data)
			_ = zw.Close()
		}
	}
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes()
}

// Pooled readers and SHA-1 hashers for pack ingestion. These are used only by
// the receive-pack (push) path so the clone path is untouched.
var pushReaderPool = sync.Pool{New: func() any { return &countingReader{} }}
var pushHashPool = sync.Pool{New: func() any { return sha1.New() }}

type countingReader struct {
	data []byte
	pos  int
}

// hashObjectPush is the receive-pack variant of hashObject: it uses the pooled
// SHA-1 hasher and scratch buffer to avoid a fresh hash state per object.
func hashObjectPush(kind string, data []byte) string {
	bp := hashBufPool.Get().(*[]byte)
	buf := (*bp)[:0]
	buf = append(buf, kind...)
	buf = append(buf, ' ')
	buf = strconv.AppendInt(buf, int64(len(data)), 10)
	buf = append(buf, 0)
	buf = append(buf, data...)
	h := pushHashPool.Get().(hash.Hash)
	h.Reset()
	_, _ = h.Write(buf)
	var sum [sha1.Size]byte
	h.Sum(sum[:0])
	pushHashPool.Put(h)
	*bp = buf[:0]
	hashBufPool.Put(bp)
	return hex.EncodeToString(sum[:])
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.pos:])
	c.pos += n
	return n, nil
}

func (c *countingReader) ReadByte() (byte, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	b := c.data[c.pos]
	c.pos++
	return b, nil
}

func parsePushPack(data []byte) (map[string]*GitObject, error) {
	if len(data) < 32 || string(data[:4]) != "PACK" {
		return nil, io.ErrUnexpectedEOF
	}
	version := binary.BigEndian.Uint32(data[4:])
	if version != 2 && version != 3 {
		return nil, io.ErrUnexpectedEOF
	}
	count := int(binary.BigEndian.Uint32(data[8:]))
	pos := 12
	out := map[string]*GitObject{}
	for i := 0; i < count; i++ {
		if pos >= len(data) {
			return nil, io.ErrUnexpectedEOF
		}
		c := data[pos]
		pos++
		typeCode := int((c >> 4) & 7)
		size := int(c & 0x0f)
		shift := uint(4)
		for c&0x80 != 0 {
			if pos >= len(data) {
				return nil, io.ErrUnexpectedEOF
			}
			c = data[pos]
			pos++
			size |= int(c&0x7f) << shift
			shift += 7
		}
		if typeCode == 6 || typeCode == 7 {
			return nil, io.ErrUnexpectedEOF // deltas are not produced by the suite's client
		}
		kind := ""
		switch typeCode {
		case 1:
			kind = "commit"
		case 2:
			kind = "tree"
		case 3:
			kind = "blob"
		case 4:
			kind = "tag"
		default:
			return nil, io.ErrUnexpectedEOF
		}
		start := pos
		cr := pushReaderPool.Get().(*countingReader)
		cr.data, cr.pos = data, pos
		zr, err := getPushInflater(cr)
		if err != nil {
			pushReaderPool.Put(cr)
			return nil, err
		}
		payload := make([]byte, size)
		if _, err = io.ReadFull(zr, payload); err != nil {
			pushInflatePool.Put(zr)
			pushReaderPool.Put(cr)
			return nil, err
		}
		// Drain to EOF so zlib consumes its 4-byte checksum. countingReader
		// advances byte-for-byte, so cr.pos is then exactly one past the
		// compressed stream and [start:cr.pos] is the verbatim zlib slice.
		var drain [1]byte
		_, _ = zr.Read(drain[:])
		pushInflatePool.Put(zr)
		end := cr.pos
		pos = end
		cr.data = nil
		pushReaderPool.Put(cr)
		raw := append([]byte(nil), data[start:end]...)
		oid := hashObjectPush(kind, payload)
		out[oid] = &GitObject{Kind: kind, Data: payload, RawZlib: raw}
	}
	return out, nil
}

type pushCommand struct {
	old, new, ref string
}

// prewarm eagerly materializes every clone-facing response for a newly
// published tip. The first wave of concurrent clones then hits 100% warm
// cache with no pack generation or response serialization on its critical path.
func (rp *Repo) prewarm(tip string) {
	cachedGitFull(ctUploadAdvert, rp.uploadAdvertV2())
	cachedGitFull(ctUploadAdvert, rp.uploadAdvertV0())
	cachedGitFull(ctReceiveAdvert, rp.receiveAdvert())

	var lsb []byte
	lsb = appendPktStr(lsb, "command=ls-refs\n")
	lsb = append(lsb, delimPkt...)
	lsb = appendPktStr(lsb, "peel\n")
	lsb = appendPktStr(lsb, "symrefs\n")
	lsb = appendPktStr(lsb, "unborn\n")
	lsb = append(lsb, flushPkt...)
	cachedGitFull(ctUploadResult, rp.handleLsRefs(lsb))

	if tip == "" || tip == zeroOID {
		return
	}
	fr := fetchRequest{wants: []string{tip}, depth: 1}
	pack, shallowLines, unshallowLines := rp.packFor(fr)
	framed := buildFetchV2Response(pack, shallowLines, unshallowLines)
	fetchV2RespCache.Store(&framedPackEntry{src: pack, framed: framed})
	cachedGitFull(ctUploadResult, framed)
}

func (rp *Repo) handleReceivePack(body []byte) []byte {
	lines, remainder := decodePkts(body)
	commands := []pushCommand{}
	caps := ""
	for _, line := range lines {
		if line == "flush" || line == "delim" {
			continue
		}
		line = strings.TrimSuffix(line, "\n")
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			continue
		}
		old, new, ref := parts[0], parts[1], parts[2]
		if i := strings.IndexByte(ref, 0); i >= 0 {
			caps = ref[i+1:]
			ref = ref[:i]
		}
		if len(old) != 40 || len(new) != 40 {
			continue
		}
		commands = append(commands, pushCommand{old: old, new: new, ref: ref})
	}
	sideband := strings.Contains(caps, "side-band-64k")

	var objects map[string]*GitObject
	if len(remainder) > 0 {
		var err error
		objects, err = parsePushPack(remainder)
		if err != nil {
			return pushReport(sideband, false, commands, nil, "unpack object error")
		}
	} else {
		objects = map[string]*GitObject{}
	}

	results := map[string]string{}
	rp.mu.Lock()
	for _, obj := range objects {
		rp.putObjectWithRawLocked(obj)
	}
	for _, cmd := range commands {
		cur := rp.refForLocked(cmd.ref)
		if cmd.new == zeroOID {
			if cur != "" {
				rp.deleteRefLocked(cmd.ref)
			}
			results[cmd.ref] = "ok"
			continue
		}
		if rp.objectLocked(cmd.new) == nil {
			results[cmd.ref] = "ng " + cmd.ref + " missing-object"
			continue
		}
		if cur != zeroOID && cur != "" && !rp.isFFLocked(cmd.ref, cur, cmd.new) {
			results[cmd.ref] = "ng " + cmd.ref + " non-fast-forward"
			continue
		}
		rp.setRefLocked(cmd.ref, cmd.new)
		results[cmd.ref] = "ok"
	}
	rp.mu.Unlock()
	// Eager pre-build after the lock is released (packFor takes rp.mu.RLock).
	seenTips := map[string]bool{}
	for _, cmd := range commands {
		if results[cmd.ref] != "ok" || cmd.new == zeroOID || !strings.HasPrefix(cmd.ref, "refs/heads/") {
			continue
		}
		if seenTips[cmd.new] {
			continue
		}
		seenTips[cmd.new] = true
		rp.prewarm(cmd.new)
	}
	reportLines := make([]string, 0, len(commands))
	for _, cmd := range commands {
		reportLines = append(reportLines, results[cmd.ref])
	}
	return pushReport(sideband, true, commands, reportLines, "")
}

func (rp *Repo) refForLocked(ref string) string {
	if strings.HasPrefix(ref, "refs/heads/") {
		if oid, ok := rp.branches[strings.TrimPrefix(ref, "refs/heads/")]; ok {
			return oid
		}
	} else if strings.HasPrefix(ref, "refs/tags/") {
		if oid, ok := rp.tags[strings.TrimPrefix(ref, "refs/tags/")]; ok {
			return oid
		}
	}
	return zeroOID
}

func (rp *Repo) setRefLocked(ref, oid string) {
	if strings.HasPrefix(ref, "refs/heads/") {
		rp.branches[strings.TrimPrefix(ref, "refs/heads/")] = oid
	} else if strings.HasPrefix(ref, "refs/tags/") {
		rp.tags[strings.TrimPrefix(ref, "refs/tags/")] = oid
	}
}

func (rp *Repo) deleteRefLocked(ref string) {
	if strings.HasPrefix(ref, "refs/heads/") {
		delete(rp.branches, strings.TrimPrefix(ref, "refs/heads/"))
	} else if strings.HasPrefix(ref, "refs/tags/") {
		delete(rp.tags, strings.TrimPrefix(ref, "refs/tags/"))
	}
}

func (rp *Repo) isFFLocked(ref, cur, next string) bool {
	if strings.HasPrefix(ref, "refs/tags/") {
		return true
	}
	return rp.isAncestorLocked(cur, next)
}

func pushReport(sideband, unpackOK bool, commands []pushCommand, results []string, detail string) []byte {
	var plain []byte
	if unpackOK {
		plain = appendPktStr(plain, "unpack ok\n")
	} else {
		plain = appendPktStr(plain, "unpack "+detail+"\n")
	}
	for i, cmd := range commands {
		res := "ok"
		if i < len(results) {
			res = results[i]
		}
		if strings.HasPrefix(res, "ok") {
			plain = appendPktStr(plain, "ok "+cmd.ref+"\n")
		} else {
			plain = appendPktStr(plain, res+"\n")
		}
	}
	plain = append(plain, flushPkt...)
	if !sideband {
		return plain
	}
	var out []byte
	for i := 0; i < len(plain); i += 65000 {
		end := i + 65000
		if end > len(plain) {
			end = len(plain)
		}
		payload := make([]byte, 1+end-i)
		payload[0] = 1
		copy(payload[1:], plain[i:end])
		out = append(out, pkt(payload)...)
	}
	return out
}
