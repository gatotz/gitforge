// Git smart HTTP: /{owner}/{repo}.git/info/refs, git-upload-pack and
// git-receive-pack.
//
// The forge speaks git protocol v2 for upload-pack (advertisement,
// ls-refs, fetch with shallow/deepen) and v0 for receive-pack (push with
// report-status and side-band-64k). Packs carry plain objects only, never
// deltas, on either side: the suite's client sends plain objects, and the
// forge answers with plain objects, which the suite verifies by hash.
package main

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// pkt-line codec
// ---------------------------------------------------------------------------

// pktLine frames data as one pkt-line: a 4-hex length (itself included)
// followed by the data.
func pktLine(data []byte) []byte {
	out := make([]byte, 0, len(data)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(data)+4))...)
	return append(out, data...)
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

func pktFlush() []byte { return []byte("0000") }

func pktDelim() []byte { return []byte("0001") }

// pktCursor reads pkt-lines from a byte slice, one at a time. It reports
// the kind ("data", "flush", "delim", "end") and, for data packets, the
// payload. After the cursor passes a flush, the remaining bytes (c.rest)
// are the raw pack that follows a receive-pack command list.
type pktCursor struct {
	b   []byte
	pos int
}

func (c *pktCursor) next() (string, []byte, error) {
	if c.pos >= len(c.b) {
		return "", nil, io.EOF
	}
	if len(c.b)-c.pos < 4 {
		return "", nil, fmt.Errorf("truncated pkt-line")
	}
	hdr := string(c.b[c.pos : c.pos+4])
	switch hdr {
	case "0000":
		c.pos += 4
		return "flush", nil, nil
	case "0001":
		c.pos += 4
		return "delim", nil, nil
	case "0002":
		c.pos += 4
		return "end", nil, nil
	}
	n, err := strconv.ParseUint(hdr, 16, 32)
	if err != nil || n < 4 || n > 65520 {
		return "", nil, fmt.Errorf("bad pkt-line length %q", hdr)
	}
	if c.pos+int(n) > len(c.b) {
		return "", nil, fmt.Errorf("truncated pkt-line data")
	}
	data := c.b[c.pos+4 : c.pos+int(n)]
	c.pos += int(n)
	return "data", data, nil
}

// rest returns the bytes after the cursor's current position.
func (c *pktCursor) rest() []byte { return c.b[c.pos:] }

// decodePktLines parses a pkt-line stream and returns the data payloads
// as strings, without their trailing "\n". It stops at the first flush,
// end or error; a delim packet is skipped (arguments run on both sides of
// it in a v2 request).
func decodePktLines(b []byte) ([]string, error) {
	var out []string
	cur := &pktCursor{b: b}
	for {
		kind, data, err := cur.next()
		if err != nil {
			return nil, err
		}
		switch kind {
		case "data":
			out = append(out, strings.TrimSuffix(string(data), "\n"))
		case "delim":
			continue
		default: // flush or end
			return out, nil
		}
	}
}

// ---------------------------------------------------------------------------
// Packfiles (plain objects only, no deltas)
// ---------------------------------------------------------------------------

// parsePack parses a packfile, verifies its trailer and every object's
// SHA-1, and returns the objects keyed by id. A delta entry is an error:
// the suite's client only ever sends plain objects.
func parsePack(data []byte) (map[string]*Object, error) {
	if len(data) < 12+20 {
		return nil, fmt.Errorf("pack too short")
	}
	if string(data[0:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack magic")
	}
	if binary.BigEndian.Uint32(data[4:8]) != 2 {
		return nil, fmt.Errorf("unsupported pack version %d", binary.BigEndian.Uint32(data[4:8]))
	}
	count := binary.BigEndian.Uint32(data[8:12])
	sum := sha1.Sum(data[:len(data)-20])
	if !bytes.Equal(sum[:], data[len(data)-20:]) {
		return nil, fmt.Errorf("pack trailer mismatch")
	}
	// The body is data[12:len-20]. A single bufio reader walks it: each
	// zlib stream ends exactly where the next object starts, and any
	// read-ahead stays in the buffer for the next object.
	body := data[12 : len(data)-20]
	br := bytes.NewReader(body)
	buf := bufio.NewReader(br)
	objs := make(map[string]*Object, count)
	for i := uint32(0); i < count; i++ {
		b, err := buf.ReadByte()
		if err != nil {
			return nil, fmt.Errorf("pack truncated at object %d", i)
		}
		typ := (b >> 4) & 0x7
		size := uint64(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b, err = buf.ReadByte()
			if err != nil {
				return nil, fmt.Errorf("pack truncated at object %d", i)
			}
			size |= uint64(b&0x7f) << shift
			shift += 7
			if shift > 63 {
				return nil, fmt.Errorf("bad object size in pack")
			}
		}
		if typ == 6 || typ == 7 {
			return nil, fmt.Errorf("delta object (type %d) not supported", typ)
		}
		if typ < 1 || typ > 4 {
			return nil, fmt.Errorf("bad object type %d", typ)
		}
		zr, err := zlib.NewReader(buf)
		if err != nil {
			return nil, fmt.Errorf("bad zlib stream: %v", err)
		}
		raw, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, fmt.Errorf("bad zlib data: %v", err)
		}
		if uint64(len(raw)) != size {
			return nil, fmt.Errorf("object size mismatch: want %d, got %d", size, len(raw))
		}
		id := hashObject(int(typ), raw)
		if _, dup := objs[id]; dup {
			return nil, fmt.Errorf("duplicate object %s in pack", id)
		}
		objs[id] = &Object{Type: int(typ), Data: raw}
	}
	return objs, nil
}

// writePack encodes objects as a packfile (version 2, no deltas), in oid
// order so the bytes are deterministic. It returns the whole pack,
// trailer included.
// compCache caches the zlib-compressed form of an object, keyed by its
// oid. Objects are immutable and content-addressed, so the compressed
// bytes are deterministic for a given oid and never go stale — no
// invalidation is ever needed. A shallow clone of one tip rebuilds the
// same pack fifty times over; this cache makes the second and later
// builds reuse the first one's compression work.
var compCache sync.Map // string -> []byte

// compressedObject returns the zlib-compressed form of an object,
// reusing the cache when another fetch already compressed it.
func compressedObject(id string, obj *Object) []byte {
	if v, ok := compCache.Load(id); ok {
		return v.([]byte)
	}
	var comp bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&comp, zlib.BestSpeed)
	zw.Write(obj.Data)
	zw.Close()
	b := comp.Bytes()
	actual, _ := compCache.LoadOrStore(id, b)
	return actual.([]byte)
}

func writePack(objs map[string]*Object) []byte {
	ids := make([]string, 0, len(objs))
	for id := range objs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var body bytes.Buffer
	h := sha1.New()
	write := func(p []byte) {
		body.Write(p)
		h.Write(p)
	}
	hdr := make([]byte, 12)
	copy(hdr[0:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(ids)))
	write(hdr)
	for _, id := range ids {
		obj := objs[id]
		// Object header: type in the top 3 bits, size in base-128.
		size := uint64(len(obj.Data))
		var first byte
		first = byte(obj.Type<<4) | byte(size&0x0f)
		size >>= 4
		if size != 0 {
			first |= 0x80
		}
		write([]byte{first})
		for size != 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size != 0 {
				b |= 0x80
			}
			write([]byte{b})
		}
		// The object data, zlib-compressed (cached per object; see
		// compressedObject). BestSpeed: the pack is sent over
		// localhost and the client only checksums it, so CPU matters
		// far more than a few extra bytes.
		write(compressedObject(id, obj))
	}
	// Trailer: SHA-1 over everything before it.
	write(h.Sum(nil))
	return body.Bytes()
}

// ---------------------------------------------------------------------------
// Object helpers
// ---------------------------------------------------------------------------

// commitParents parses a commit object and returns its parent oids.
func commitParents(data []byte) []string {
	var parents []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "parent ") {
			parents = append(parents, strings.TrimSpace(line[7:]))
		}
	}
	return parents
}

// commitTree parses a commit object and returns its tree oid.
func commitTree(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "tree ") {
			return strings.TrimSpace(line[5:])
		}
	}
	return ""
}

// commitTime parses a commit object and returns its committer timestamp.
func commitTime(data []byte) int64 {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "committer ") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				if ts, err := strconv.ParseInt(fields[len(fields)-2], 10, 64); err == nil {
					return ts
				}
			}
		}
	}
	return 0
}

// tagTarget parses a tag object and returns the oid it points at.
func tagTarget(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
	}
	return ""
}

// treeEntry is one entry of a tree object.
type treeEntry struct {
	mode string
	name string
	oid  string
}

// parseTree parses a tree object into its entries.
func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	pos := 0
	for pos < len(data) {
		space := bytes.IndexByte(data[pos:], ' ')
		if space < 0 {
			break
		}
		mode := string(data[pos : pos+space])
		pos += space + 1
		nul := bytes.IndexByte(data[pos:], 0)
		if nul < 0 || pos+nul+20 > len(data) {
			break
		}
		name := string(data[pos : pos+nul])
		pos += nul + 1
		oid := hex.EncodeToString(data[pos : pos+20])
		pos += 20
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
	}
	return out
}

// treeKey orders tree entries the way git does: by name bytes, with a
// directory compared as if its name ended in '/'.
func treeKey(e treeEntry) string {
	if e.mode == "40000" {
		return e.name + "/"
	}
	return e.name
}

// buildTree builds trees bottom-up from a flat path -> blob oid map and
// returns the root tree oid, storing every tree object it creates.
func (r *Repo) buildTree(files map[string]string) string {
	// Group the files by their top directory.
	type node struct {
		blobs map[string]string // name -> blob oid (files in this dir)
		dirs  map[string]*node  // name -> subdir
	}
	root := &node{blobs: map[string]string{}, dirs: map[string]*node{}}
	for path, blob := range files {
		parts := strings.Split(path, "/")
		cur := root
		for _, part := range parts[:len(parts)-1] {
			next, ok := cur.dirs[part]
			if !ok {
				next = &node{blobs: map[string]string{}, dirs: map[string]*node{}}
				cur.dirs[part] = next
			}
			cur = next
		}
		cur.blobs[parts[len(parts)-1]] = blob
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []treeEntry
		for name, blob := range n.blobs {
			entries = append(entries, treeEntry{mode: "100644", name: name, oid: blob})
		}
		for name, sub := range n.dirs {
			entries = append(entries, treeEntry{mode: "40000", name: name, oid: build(sub)})
		}
		sort.Slice(entries, func(i, j int) bool { return treeKey(entries[i]) < treeKey(entries[j]) })
		var buf bytes.Buffer
		for _, e := range entries {
			buf.WriteString(e.mode)
			buf.WriteByte(' ')
			buf.WriteString(e.name)
			buf.WriteByte(0)
			oid, _ := hex.DecodeString(e.oid)
			buf.Write(oid)
		}
		return r.storeObject(objTree, buf.Bytes())
	}
	return build(root)
}

// flattenTree walks a tree recursively and returns path -> blob oid.
func (r *Repo) flattenTree(treeOID string) map[string]string {
	out := map[string]string{}
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		obj, ok := r.Objects[treeOID]
		if !ok || obj.Type != objTree {
			return
		}
		for _, e := range parseTree(obj.Data) {
			path := prefix + e.name
			if e.mode == "40000" {
				walk(e.oid, path+"/")
			} else {
				out[path] = e.oid
			}
		}
	}
	walk(treeOID, "")
	return out
}

// buildCommit creates a commit object and returns its oid. The author and
// committer are the forge's fixed ident at the fixed timestamp, so the
// commit is identical on a live run and its replay.
func (r *Repo) buildCommit(treeOID string, parents []string, message string) string {
	var b strings.Builder
	b.WriteString("tree " + treeOID + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + commitAuthor + " " + strconv.FormatInt(fixedCommitTime, 10) + " +0000\n")
	b.WriteString("committer " + commitAuthor + " " + strconv.FormatInt(fixedCommitTime, 10) + " +0000\n")
	b.WriteString("\n")
	b.WriteString(message)
	return r.storeObject(objCommit, []byte(b.String()))
}

// isAncestor reports whether ancestor is reachable from descendant by
// walking the descendant's parent chain.
func (r *Repo) isAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{descendant: true}
	stack := []string{descendant}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != objCommit {
			continue
		}
		for _, p := range commitParents(obj.Data) {
			if p == ancestor {
				return true
			}
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return false
}

// resolveWant turns a wanted oid into the commit it names, following a tag
// object to its target. It reports false when the oid is not a commit
// (or a tag pointing at one) the repository holds.
func (r *Repo) resolveWant(oid string) (string, bool) {
	for i := 0; i < 8; i++ {
		obj, ok := r.Objects[oid]
		if !ok {
			return "", false
		}
		if obj.Type == objCommit {
			return oid, true
		}
		if obj.Type == objTag {
			oid = tagTarget(obj.Data)
			continue
		}
		return "", false
	}
	return "", false
}

// reachableCommits returns every commit reachable from roots, stopping at
// shallow boundaries: a boundary commit is included, its parents are not.
func (r *Repo) reachableCommits(roots []string, shallow map[string]bool) map[string]bool {
	out := map[string]bool{}
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if out[cur] {
			continue
		}
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != objCommit {
			continue
		}
		out[cur] = true
		if shallow[cur] {
			continue
		}
		for _, p := range commitParents(obj.Data) {
			if !out[p] {
				stack = append(stack, p)
			}
		}
	}
	return out
}

// depthCommits returns the commits within `depth` generations of roots
// (generation 1 is the roots themselves). It deliberately does NOT stop at
// the client's old shallow boundaries: a deepen asks to go past them, and
// the depth limit itself bounds the walk. (reachableCommits, used for the
// have set and for a depth-less fetch, does stop there, so a shallow clone
// stays shallow unless it deepens.)
func (r *Repo) depthCommits(roots []string, depth int, shallow map[string]bool) map[string]bool {
	out := map[string]bool{}
	type item struct {
		oid string
		gen int
	}
	queue := []item{}
	for _, root := range roots {
		queue = append(queue, item{root, 1})
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if out[cur.oid] || cur.gen > depth {
			continue
		}
		obj, ok := r.Objects[cur.oid]
		if !ok || obj.Type != objCommit {
			continue
		}
		out[cur.oid] = true
		for _, p := range commitParents(obj.Data) {
			queue = append(queue, item{p, cur.gen + 1})
		}
	}
	return out
}

// commitObjects returns every object a set of commits needs: the commits
// themselves plus their trees and blobs, recursively.
func (r *Repo) commitObjects(commits map[string]bool) map[string]bool {
	out := map[string]bool{}
	var addTree func(treeOID string)
	addTree = func(treeOID string) {
		if out[treeOID] {
			return
		}
		obj, ok := r.Objects[treeOID]
		if !ok || obj.Type != objTree {
			return
		}
		out[treeOID] = true
		for _, e := range parseTree(obj.Data) {
			if e.mode == "40000" {
				addTree(e.oid)
			} else {
				out[e.oid] = true
			}
		}
	}
	for oid := range commits {
		out[oid] = true
		obj, ok := r.Objects[oid]
		if !ok || obj.Type != objCommit {
			continue
		}
		addTree(commitTree(obj.Data))
	}
	return out
}

// resolveRef resolves a ref name, branch name or oid to a commit oid.
// It reports false when nothing matches.
func (r *Repo) resolveRef(ref string) (string, bool) {
	if oid, ok := r.Refs[ref]; ok {
		return oid, true
	}
	if oid, ok := r.Refs["refs/heads/"+ref]; ok {
		return oid, true
	}
	if ref == "HEAD" {
		if oid, ok := r.Refs["refs/heads/"+r.DefaultBranch]; ok {
			return oid, true
		}
	}
	if _, ok := r.Objects[ref]; ok {
		return ref, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// handleGit
// ---------------------------------------------------------------------------

// gitRepo parses /{owner}/{repo}.git/... and returns the repository, or
// nil when the owner is not the viewer or the repository does not exist.
func gitRepo(path string) (*Repo, string, bool) {
	segs := segments(path)
	if len(segs) < 3 {
		return nil, "", false
	}
	owner := segs[0]
	repoName := strings.TrimSuffix(segs[1], ".git")
	if !strings.HasSuffix(segs[1], ".git") {
		return nil, "", false
	}
	repo := store.getRepo(owner, repoName)
	if repo == nil {
		return nil, "", false
	}
	// The tail is everything after /{owner}/{repo}.git: "info/refs",
	// "git-upload-pack" or "git-receive-pack".
	return repo, strings.Join(segs[2:], "/"), true
}

func handleGit(w http.ResponseWriter, r *http.Request) {
	repo, tail, ok := gitRepo(r.URL.Path)
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	switch {
	case r.Method == http.MethodGet && tail == "info/refs":
		service := r.URL.Query().Get("service")
		if service == "git-upload-pack" {
			handleUploadAdvertise(w, r, repo)
		} else if service == "git-receive-pack" {
			handleReceiveAdvertise(w, r, repo)
		} else {
			writeErr(w, http.StatusBadRequest, "Bad Request")
		}
	case r.Method == http.MethodPost && tail == "git-upload-pack":
		handleUploadPack(w, r, repo)
	case r.Method == http.MethodPost && tail == "git-receive-pack":
		handleReceivePack(w, r, repo)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// ---------------------------------------------------------------------------
// upload-pack: advertisement (v2, with a v0 fallback)
// ---------------------------------------------------------------------------

// uploadV2Caps are the capabilities the forge advertises for upload-pack
// v2. ls-refs is bare on purpose: with no =unborn the client never sends
// `unborn`, so an ls-refs on a fresh repository returns no ref at all,
// which the clone-empty check requires.
var uploadV2Caps = []string{
	"agent=" + forgeAgent,
	"ls-refs",
	"fetch=shallow wait-for-done",
	"object-format=sha1",
}

func handleUploadAdvertise(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	// A v0 answer only when the client asked for v0 outright.
	if r.Header.Get("Git-Protocol") == "version=0" {
		handleUploadAdvertiseV0(w, repo)
		return
	}
	var buf bytes.Buffer
	buf.Write(pktLineStr("# service=git-upload-pack\n"))
	buf.Write(pktFlush())
	buf.Write(pktLineStr("version 2\n"))
	for _, cap := range uploadV2Caps {
		buf.Write(pktLineStr(cap + "\n"))
	}
	buf.Write(pktFlush())
	w.Header().Set("content-type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("x-github-request-id", requestID())
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

func handleUploadAdvertiseV0(w http.ResponseWriter, repo *Repo) {
	var buf bytes.Buffer
	buf.Write(pktLineStr("# service=git-upload-pack\n"))
	buf.Write(pktFlush())
	caps := "side-band-64k shallow agent=" + forgeAgent + " symref=HEAD:refs/heads/" + repo.DefaultBranch
	refs := repo.refNames()
	if len(refs) == 0 {
		buf.Write(pktLineStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		// HEAD first, then the refs in sorted order.
		if head, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			buf.Write(pktLineStr(head + " HEAD\x00" + caps + "\n"))
		} else {
			buf.Write(pktLineStr(repo.Refs[refs[0]] + " " + refs[0] + "\x00" + caps + "\n"))
			refs = refs[1:]
		}
		for _, name := range refs {
			buf.Write(pktLineStr(repo.Refs[name] + " " + name + "\n"))
			if peeled := peeledOf(repo, name); peeled != "" {
				buf.Write(pktLineStr(peeled + " " + name + "^{}\n"))
			}
		}
	}
	buf.Write(pktFlush())
	w.Header().Set("content-type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("x-github-request-id", requestID())
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

// peeledOf returns the target a tag ref peels to, or "".
func peeledOf(repo *Repo, ref string) string {
	oid, ok := repo.Refs[ref]
	if !ok {
		return ""
	}
	obj, ok := repo.Objects[oid]
	if !ok || obj.Type != objTag {
		return ""
	}
	return tagTarget(obj.Data)
}

// ---------------------------------------------------------------------------
// upload-pack: ls-refs and fetch (v2)
// ---------------------------------------------------------------------------

func handleUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, ok := readBody(r, 64<<20)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	lines, err := decodePktLines(body)
	if err != nil || len(lines) == 0 {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	if !strings.HasPrefix(lines[0], "command=") {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	cmd := strings.TrimPrefix(lines[0], "command=")
	args := lines[1:]
	switch cmd {
	case "ls-refs":
		handleLsRefs(w, repo, args)
	case "fetch":
		handleFetch(w, repo, args)
	default:
		writeErr(w, http.StatusBadRequest, "Bad Request")
	}
}

// handleLsRefs answers a v2 ls-refs request. The response lists HEAD (with
// its symref target) and every ref matching a ref-prefix, with peeled tag
// targets when peel was asked for.
func handleLsRefs(w http.ResponseWriter, repo *Repo, args []string) {
	peel, symrefs := false, false
	var prefixes []string
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(a, "ref-prefix "))
		}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	keep := func(name string) bool {
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
	var buf bytes.Buffer
	if head, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok && keep("HEAD") {
		line := head + " HEAD"
		if symrefs {
			line += " symref-target:refs/heads/" + repo.DefaultBranch
		}
		buf.Write(pktLineStr(line + "\n"))
	}
	for _, name := range repo.refNames() {
		if !keep(name) {
			continue
		}
		oid := repo.Refs[name]
		line := oid + " " + name
		if peel {
			if peeled := peeledOf(repo, name); peeled != "" {
				line += " peeled:" + peeled
			}
		}
		buf.Write(pktLineStr(line + "\n"))
	}
	buf.Write(pktFlush())
	w.Header().Set("content-type", "application/x-git-upload-pack-result")
	w.Header().Set("x-github-request-id", requestID())
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

// handleFetch answers a v2 fetch request: acknowledgments, shallow-info
// when the shallow boundary moves, and a packfile section when there is
// anything to send. An empty fetch sends acknowledgments only.
func handleFetch(w http.ResponseWriter, repo *Repo, args []string) {
	var wants, haves []string
	oldShallow := map[string]bool{}
	depth := 0
	var deepenSince int64
	var deepenNot []string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			wants = append(wants, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "have "):
			haves = append(haves, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "shallow "):
			oldShallow[strings.TrimSpace(a[8:])] = true
		case strings.HasPrefix(a, "deepen "):
			depth, _ = strconv.Atoi(strings.TrimSpace(a[7:]))
		case strings.HasPrefix(a, "deepen-since "):
			deepenSince, _ = strconv.ParseInt(strings.TrimSpace(a[13:]), 10, 64)
		case strings.HasPrefix(a, "deepen-not "):
			deepenNot = append(deepenNot, strings.TrimSpace(a[11:]))
		}
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	var buf bytes.Buffer
	// --- acknowledgments ---
	buf.Write(pktLineStr("acknowledgments\n"))
	ack := ""
	for _, have := range haves {
		if _, ok := repo.Objects[have]; ok {
			ack = have
			break
		}
	}
	if ack != "" {
		buf.Write(pktLineStr("ACK " + ack + "\n"))
		buf.Write(pktLineStr("ready\n"))
	} else {
		buf.Write(pktLineStr("NAK\n"))
	}
	buf.Write(pktFlush())
	// --- object selection ---
	var wantCommits []string
	for _, want := range wants {
		if commit, ok := repo.resolveWant(want); ok {
			wantCommits = append(wantCommits, commit)
		}
	}
	if len(wantCommits) == 0 {
		// Nothing wanted (or nothing the repository holds): no pack.
		w.Header().Set("content-type", "application/x-git-upload-pack-result")
		w.Header().Set("x-github-request-id", requestID())
		w.WriteHeader(http.StatusOK)
		w.Write(buf.Bytes())
		return
	}
	haveCommits := repo.reachableCommits(haves, oldShallow)
	var wantSet map[string]bool
	if depth > 0 {
		wantSet = repo.depthCommits(wantCommits, depth, oldShallow)
	} else {
		wantSet = repo.reachableCommits(wantCommits, oldShallow)
	}
	// deepen-not: drop every commit reachable from the excluded ref.
	for _, ref := range deepenNot {
		if oid, ok := repo.resolveRef(ref); ok {
			for c := range repo.reachableCommits([]string{oid}, map[string]bool{}) {
				delete(wantSet, c)
			}
		}
	}
	// deepen-since: keep only commits newer than the timestamp.
	if deepenSince > 0 {
		for c := range wantSet {
			if obj, ok := repo.Objects[c]; !ok || commitTime(obj.Data) < deepenSince {
				delete(wantSet, c)
			}
		}
	}
	// Objects to send: what the wants need, minus what the haves cover.
	// A wanted tag object itself is sent too: resolving a tag to its
	// commit must not drop the tag from the clone.
	wantObjs := repo.commitObjects(wantSet)
	haveObjs := repo.commitObjects(haveCommits)
	send := map[string]bool{}
	for oid := range wantObjs {
		if !haveObjs[oid] {
			send[oid] = true
		}
	}
	for _, want := range wants {
		oid := want
		for i := 0; i < 8; i++ {
			obj, ok := repo.Objects[oid]
			if !ok || obj.Type != objTag {
				break
			}
			send[oid] = true
			oid = tagTarget(obj.Data)
		}
	}
	// --- shallow-info ---
	var newShallow, unshallow []string
	for c := range wantSet {
		obj, ok := repo.Objects[c]
		if !ok || obj.Type != objCommit {
			continue
		}
		for _, p := range commitParents(obj.Data) {
			if !wantSet[p] && !haveCommits[p] {
				newShallow = append(newShallow, c)
				break
			}
		}
	}
	for c := range oldShallow {
		if !wantSet[c] && !haveCommits[c] {
			continue
		}
		obj, ok := repo.Objects[c]
		if !ok || obj.Type != objCommit {
			continue
		}
		filled := true
		for _, p := range commitParents(obj.Data) {
			if !wantSet[p] && !haveCommits[p] {
				filled = false
				break
			}
		}
		if filled {
			unshallow = append(unshallow, c)
		}
	}
	sort.Strings(newShallow)
	sort.Strings(unshallow)
	if len(newShallow) > 0 || len(unshallow) > 0 {
		buf.Write(pktLineStr("shallow-info\n"))
		for _, c := range newShallow {
			buf.Write(pktLineStr("shallow " + c + "\n"))
		}
		for _, c := range unshallow {
			buf.Write(pktLineStr("unshallow " + c + "\n"))
		}
		buf.Write(pktFlush())
	}
	// --- packfile ---
	if len(send) > 0 {
		packObjs := make(map[string]*Object, len(send))
		for oid := range send {
			packObjs[oid] = repo.Objects[oid]
		}
		pack := writePack(packObjs)
		buf.Write(pktLineStr("packfile\n"))
		// Sideband band 1, chunked so no pkt-line tops 65520 bytes.
		for len(pack) > 0 {
			chunk := 65515
			if len(pack) < chunk {
				chunk = len(pack)
			}
			payload := make([]byte, 0, chunk+1)
			payload = append(payload, 1)
			payload = append(payload, pack[:chunk]...)
			buf.Write(pktLine(payload))
			pack = pack[chunk:]
		}
		buf.Write(pktFlush())
	}
	w.Header().Set("content-type", "application/x-git-upload-pack-result")
	w.Header().Set("x-github-request-id", requestID())
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

// ---------------------------------------------------------------------------
// receive-pack: advertisement (v0) and push
// ---------------------------------------------------------------------------

func handleReceiveAdvertise(w http.ResponseWriter, r *http.Request, repo *Repo) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var buf bytes.Buffer
	buf.Write(pktLineStr("# service=git-receive-pack\n"))
	buf.Write(pktFlush())
	caps := "report-status side-band-64k delete-refs ofs-delta agent=" + forgeAgent +
		" symref=HEAD:refs/heads/" + repo.DefaultBranch
	refs := repo.refNames()
	if len(refs) == 0 {
		buf.Write(pktLineStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
	} else {
		// HEAD first, then the refs in sorted order.
		if head, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			buf.Write(pktLineStr(head + " HEAD\x00" + caps + "\n"))
		} else {
			buf.Write(pktLineStr(repo.Refs[refs[0]] + " " + refs[0] + "\x00" + caps + "\n"))
			refs = refs[1:]
		}
		for _, name := range refs {
			buf.Write(pktLineStr(repo.Refs[name] + " " + name + "\n"))
			if peeled := peeledOf(repo, name); peeled != "" {
				buf.Write(pktLineStr(peeled + " " + name + "^{}\n"))
			}
		}
	}
	buf.Write(pktFlush())
	w.Header().Set("content-type", "application/x-git-receive-pack-advertisement")
	w.Header().Set("x-github-request-id", requestID())
	w.WriteHeader(http.StatusOK)
	w.Write(buf.Bytes())
}

// pushCommand is one receive-pack ref update.
type pushCommand struct {
	old string
	new string
	ref string
}

// handleReceivePack answers a v0 push: it unpacks the pack, applies the
// ref updates with fast-forward enforcement, and reports over side-band-64k.
// A malformed request body is a 400 and the server stays healthy.
func handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, ok := readBody(r, 64<<20)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	cur := &pktCursor{b: body}
	var commands []pushCommand
	sideband := false
	reportStatus := false
	for {
		kind, data, err := cur.next()
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Bad Request")
			return
		}
		if kind != "data" {
			break
		}
		line := strings.TrimSuffix(string(data), "\n")
		// Capabilities ride after NUL on the first command line.
		if i := strings.Index(line, "\x00"); i >= 0 {
			for _, cap := range strings.Fields(line[i+1:]) {
				if cap == "side-band-64k" {
					sideband = true
				}
				if cap == "report-status" || cap == "report-status-v2" {
					reportStatus = true
				}
			}
			line = line[:i]
		}
		parts := strings.Fields(line)
		if len(parts) != 3 {
			writeErr(w, http.StatusBadRequest, "Bad Request")
			return
		}
		commands = append(commands, pushCommand{old: parts[0], new: parts[1], ref: parts[2]})
	}
	if len(commands) == 0 {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	// The rest of the body after the flush is the pack.
	packBytes := cur.rest()
	store.mu.Lock()
	defer store.mu.Unlock()
	// Unpack first: objects land even when a ref update is refused.
	unpackErr := ""
	if len(packBytes) > 0 {
		objs, err := parsePack(packBytes)
		if err != nil {
			unpackErr = err.Error()
		} else {
			for id, obj := range objs {
				repo.Objects[id] = obj
			}
		}
	}
	// Apply the ref updates, one at a time, so a refused ref does not
	// stop the others (a mixed report).
	type refResult struct {
		ref    string
		ok     bool
		reason string
	}
	results := make([]refResult, 0, len(commands))
	for _, cmd := range commands {
		if unpackErr != "" {
			results = append(results, refResult{cmd.ref, false, "unpack failed"})
			continue
		}
		current, exists := repo.Refs[cmd.ref]
		switch {
		case cmd.new == zeroOID: // delete
			if !exists {
				results = append(results, refResult{cmd.ref, false, "reference does not exist"})
			} else if cmd.old != zeroOID && cmd.old != current {
				results = append(results, refResult{cmd.ref, false, "non-fast-forward"})
			} else {
				delete(repo.Refs, cmd.ref)
				results = append(results, refResult{cmd.ref, true, ""})
			}
		case cmd.old == zeroOID: // create
			if exists {
				results = append(results, refResult{cmd.ref, false, "already exists"})
			} else if _, ok := repo.Objects[cmd.new]; !ok {
				results = append(results, refResult{cmd.ref, false, "object not found"})
			} else {
				repo.Refs[cmd.ref] = cmd.new
				results = append(results, refResult{cmd.ref, true, ""})
			}
		default: // update
			if !exists {
				results = append(results, refResult{cmd.ref, false, "reference does not exist"})
			} else if cmd.old != current {
				results = append(results, refResult{cmd.ref, false, "non-fast-forward"})
			} else if _, ok := repo.Objects[cmd.new]; !ok {
				results = append(results, refResult{cmd.ref, false, "object not found"})
			} else if !repo.isAncestor(cmd.old, cmd.new) {
				results = append(results, refResult{cmd.ref, false, "non-fast-forward"})
			} else {
				repo.Refs[cmd.ref] = cmd.new
				results = append(results, refResult{cmd.ref, true, ""})
			}
		}
	}
	// Report. With side-band-64k the report-status stream is itself
	// pkt-lined inside band 1; the client decodes it either way.
	var report bytes.Buffer
	if unpackErr != "" {
		report.Write(pktLineStr("unpack " + unpackErr + "\n"))
	} else {
		report.Write(pktLineStr("unpack ok\n"))
	}
	for _, res := range results {
		if res.ok {
			report.Write(pktLineStr("ok " + res.ref + "\n"))
		} else {
			report.Write(pktLineStr("ng " + res.ref + " " + res.reason + "\n"))
		}
	}
	var out bytes.Buffer
	if sideband && reportStatus {
		// One sideband packet per report line.
		cur2 := &pktCursor{b: report.Bytes()}
		for {
			kind, data, err := cur2.next()
			if err != nil || kind != "data" {
				break
			}
			payload := make([]byte, 0, len(data)+1)
			payload = append(payload, 1)
			payload = append(payload, data...)
			out.Write(pktLine(payload))
		}
		out.Write(pktFlush())
	} else {
		out.Write(report.Bytes())
		out.Write(pktFlush())
	}
	w.Header().Set("content-type", "application/x-git-receive-pack-result")
	w.Header().Set("x-github-request-id", requestID())
	w.WriteHeader(http.StatusOK)
	w.Write(out.Bytes())
}
