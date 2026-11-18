// The REST v3 API: /api/v3/...
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// userJSON is the user object embedded in repository responses.
type userJSON struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

func viewerJSON() userJSON { return userJSON{Login: viewerLogin, ID: viewerID, Type: "User"} }

// repoJSON is the repository object the REST API returns.
type repoJSON struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Owner         userJSON `json:"owner"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
}

func (r *Repo) toJSON() repoJSON {
	return repoJSON{
		ID:            r.ID,
		Name:          r.Name,
		FullName:      r.Owner + "/" + r.Name,
		Owner:         viewerJSON(),
		Private:       r.Private,
		Description:   r.Description,
		DefaultBranch: r.DefaultBranch,
	}
}

// repoCreateBody is the body of POST /user/repos.
type repoCreateBody struct {
	Name        string `json:"name"`
	Private     bool   `json:"private"`
	Description string `json:"description"`
}

// repoPatchBody is the body of PATCH /repos/{owner}/{repo}.
type repoPatchBody struct {
	Description *string `json:"description"`
	Private     *bool   `json:"private"`
}

// handleREST routes one REST v3 request. The caller has already checked the
// bearer token. Every reply is application/json with a non-empty
// X-GitHub-Request-Id; unknown routes answer 404 {"message": "Not Found"}.
func handleREST(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	// The API root: a 200 with the token's scopes, as api.github.com does.
	if r.Method == http.MethodGet && (path == "/api/v3" || path == "/api/v3/") {
		writeJSON(w, http.StatusOK,
			map[string]string{
				"current_user_url": "/api/v3/user",
				"rate_limit_url":   "/api/v3/rate_limit",
			},
			map[string]string{"x-oauth-scopes": "repo, read:org"})
		return
	}
	// /meta: the installed version string.
	if r.Method == http.MethodGet && path == "/api/v3/meta" {
		writeJSON(w, http.StatusOK, map[string]string{"installed_version": "gitforge 1.0.0"}, nil)
		return
	}
	// /user: the login the token names.
	if r.Method == http.MethodGet && path == "/api/v3/user" {
		writeJSON(w, http.StatusOK, viewerJSON(), nil)
		return
	}
	segs := segments(strings.TrimPrefix(path, "/api/v3"))
	if len(segs) == 0 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	// POST /user/repos and GET /user/repos.
	if segs[0] == "user" && len(segs) == 2 && segs[1] == "repos" {
		switch r.Method {
		case http.MethodPost:
			restCreateRepo(w, r)
		case http.MethodGet:
			restListRepos(w, r)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	// /repos/{owner}/{repo}[...].
	if segs[0] == "repos" {
		restRepo(w, r, segs[1:])
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// restCreateRepo handles POST /user/repos.
func restCreateRepo(w http.ResponseWriter, r *http.Request) {
	var body repoCreateBody
	if !readJSON(r, &body) {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	store.mu.Lock()
	repo, ok := store.createRepo(body.Name, body.Private, body.Description)
	if !ok {
		store.mu.Unlock()
		writeErr(w, http.StatusUnprocessableEntity, "Repository already exists")
		return
	}
	resp := repo.toJSON()
	store.mu.Unlock()
	writeJSON(w, http.StatusCreated, resp, nil)
}

// restListRepos handles GET /user/repos, paginated in creation order.
// The Link header carries an absolute same-origin next URL while another
// page follows, and no next link on the last page.
func restListRepos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perPage := queryInt(q, "per_page", 30)
	if perPage < 1 {
		perPage = 30
	}
	page := queryInt(q, "page", 1)
	if page < 1 {
		page = 1
	}
	store.mu.RLock()
	repos := store.listRepos()
	out := make([]repoJSON, 0, len(repos))
	for _, repo := range repos {
		out = append(out, repo.toJSON())
	}
	store.mu.RUnlock()
	total := len(out)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	pageOut := out[start:end]
	var extra map[string]string
	if end < total {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		next := scheme + "://" + r.Host + "/api/v3/user/repos?per_page=" +
			strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
		extra = map[string]string{"link": "<" + next + ">; rel=\"next\""}
	}
	writeJSON(w, http.StatusOK, pageOut, extra)
}

// restRepo handles /repos/{owner}/{repo} and its sub-routes.
func restRepo(w http.ResponseWriter, r *http.Request, segs []string) {
	if len(segs) < 2 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	owner, name := segs[0], segs[1]
	rest := segs[2:]
	// /repos/{owner}/{repo} itself: get, patch, delete.
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			store.mu.RLock()
			repo := store.getRepo(owner, name)
			var resp *repoJSON
			if repo != nil {
				j := repo.toJSON()
				resp = &j
			}
			store.mu.RUnlock()
			if resp == nil {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			writeJSON(w, http.StatusOK, *resp, nil)
		case http.MethodPatch:
			var body repoPatchBody
			if !readJSON(r, &body) {
				writeErr(w, http.StatusBadRequest, "Bad Request")
				return
			}
			store.mu.Lock()
			repo := store.getRepo(owner, name)
			var resp *repoJSON
			if repo != nil {
				if body.Description != nil {
					repo.Description = *body.Description
				}
				if body.Private != nil {
					repo.Private = *body.Private
				}
				j := repo.toJSON()
				resp = &j
			}
			store.mu.Unlock()
			if resp == nil {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			writeJSON(w, http.StatusOK, *resp, nil)
		case http.MethodDelete:
			// A foreign owner is a 404 and must not delete anything.
			if owner != viewerLogin {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			store.mu.Lock()
			ok := store.deleteRepo(name)
			store.mu.Unlock()
			if !ok {
				writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			w.Header().Set("x-github-request-id", requestID())
			w.WriteHeader(http.StatusNoContent)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	// Sub-routes need the repository.
	store.mu.RLock()
	repo := store.getRepo(owner, name)
	store.mu.RUnlock()
	if repo == nil {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	switch rest[0] {
	case "branches":
		restBranches(w, r, repo, rest[1:])
	case "commits":
		restCommits(w, r, repo)
	case "git":
		restGitRefs(w, r, repo, rest[1:])
	case "contents":
		restContents(w, r, repo, rest[1:])
	case "labels":
		restLabels(w, r, repo)
	case "issues":
		restIssues(w, r, repo, rest[1:])
	case "pulls":
		restPulls(w, r, repo, rest[1:])
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// queryInt reads an integer query parameter, defaulting to def.
func queryInt(q url.Values, key string, def int) int {
	s := q.Get(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// ---------------------------------------------------------------------------
// Branches, commits and git refs
// ---------------------------------------------------------------------------

// branchJSON is one entry of GET /branches.
type branchJSON struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

func (r *Repo) branchJSON(name string) branchJSON {
	var b branchJSON
	b.Name = name
	b.Commit.SHA = r.Refs["refs/heads/"+name]
	return b
}

// restBranches handles GET /branches and GET /branches/{branch}.
func restBranches(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if len(rest) == 0 {
		names := repo.branchNames()
		out := make([]branchJSON, 0, len(names))
		for _, name := range names {
			out = append(out, repo.branchJSON(name))
		}
		writeJSON(w, http.StatusOK, out, nil)
		return
	}
	branch := strings.Join(rest, "/")
	oid, ok := repo.Refs["refs/heads/"+branch]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	_ = oid
	writeJSON(w, http.StatusOK, repo.branchJSON(branch), nil)
}

// commitJSON is one entry of GET /commits.
type commitJSON struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
}

// restCommits handles GET /commits[?sha=<ref>]: the first-parent chain
// from the ref tip, newest first.
func restCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	ref := r.URL.Query().Get("sha")
	if ref == "" {
		ref = repo.DefaultBranch
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	tip, ok := repo.resolveRef(ref)
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	var out []commitJSON
	seen := map[string]bool{}
	cur := tip
	for cur != "" && !seen[cur] {
		seen[cur] = true
		obj, ok := repo.Objects[cur]
		if !ok || obj.Type != objCommit {
			break
		}
		var c commitJSON
		c.SHA = cur
		c.Commit.Message = commitMessage(obj.Data)
		out = append(out, c)
		parents := commitParents(obj.Data)
		if len(parents) == 0 {
			break
		}
		cur = parents[0]
	}
	if out == nil {
		out = []commitJSON{}
	}
	writeJSON(w, http.StatusOK, out, nil)
}

// commitMessage returns the message part of a commit object.
func commitMessage(data []byte) string {
	idx := bytes.Index(data, []byte("\n\n"))
	if idx < 0 {
		return ""
	}
	return string(data[idx+2:])
}

// refJSON is the object POST /git/refs and GET /git/refs/... return.
type refJSON struct {
	Ref    string `json:"ref"`
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

// gitRefCreateBody is the body of POST /git/refs.
type gitRefCreateBody struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// restGitRefs handles POST /git/refs and GET /git/refs/heads/{branch}.
func restGitRefs(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if r.Method == http.MethodPost && len(rest) == 1 && rest[0] == "refs" {
		var body gitRefCreateBody
		if !readJSON(r, &body) {
			writeErr(w, http.StatusBadRequest, "Bad Request")
			return
		}
		if body.Ref == "" || body.SHA == "" {
			writeErr(w, http.StatusUnprocessableEntity, "ref and sha are required")
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, exists := repo.Refs[body.Ref]; exists {
			writeErr(w, http.StatusUnprocessableEntity, "Reference already exists")
			return
		}
		if _, ok := repo.Objects[body.SHA]; !ok {
			writeErr(w, http.StatusUnprocessableEntity, "object not found")
			return
		}
		repo.Refs[body.Ref] = body.SHA
		var resp refJSON
		resp.Ref = body.Ref
		resp.Object.SHA = body.SHA
		writeJSON(w, http.StatusCreated, resp, nil)
		return
	}
	if r.Method == http.MethodGet && len(rest) >= 3 && rest[0] == "refs" && rest[1] == "heads" {
		branch := strings.Join(rest[2:], "/")
		store.mu.RLock()
		defer store.mu.RUnlock()
		oid, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			writeErr(w, http.StatusNotFound, "Not Found")
			return
		}
		var resp refJSON
		resp.Ref = "refs/heads/" + branch
		resp.Object.SHA = oid
		writeJSON(w, http.StatusOK, resp, nil)
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// ---------------------------------------------------------------------------
// Contents
// ---------------------------------------------------------------------------

// contentFileJSON is the file object GET/PUT /contents returns.
type contentFileJSON struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Size     int    `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	SHA      string `json:"sha"`
}

// contentEntryJSON is one entry of a directory listing.
type contentEntryJSON struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
}

// contentPutBody is the body of PUT /contents/{path} and DELETE /contents/{path}.
type contentPutBody struct {
	Message string `json:"message"`
	Content string `json:"content"`
	Branch  string `json:"branch"`
	SHA     string `json:"sha"`
}

// resolveBranchRef resolves the branch a contents call names (defaulting
// to the default branch) to its tip commit. It reports false when the
// branch does not exist.
func (r *Repo) resolveBranchRef(branch string) (string, bool) {
	if branch == "" {
		branch = r.DefaultBranch
	}
	oid, ok := r.Refs["refs/heads/"+branch]
	return oid, ok
}

// restContents handles PUT, GET and DELETE /contents/{path}.
func restContents(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	path := strings.Join(rest, "/")
	switch r.Method {
	case http.MethodPut:
		restContentsPut(w, r, repo, path)
	case http.MethodGet:
		restContentsGet(w, r, repo, path)
	case http.MethodDelete:
		restContentsDelete(w, r, repo, path)
	default:
		writeErr(w, http.StatusNotFound, "Not Found")
	}
}

// restContentsPut handles PUT /contents/{path}: create (201) or update
// (200) a file as a real git commit. A branch that does not exist is
// created from the default branch tip; on an unborn repository the
// default branch is created. A stale or missing sha on an existing file
// is a 409.
func restContentsPut(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	var body contentPutBody
	if !readJSON(r, &body) {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	if path == "" {
		writeErr(w, http.StatusUnprocessableEntity, "path is required")
		return
	}
	raw, err := base64.StdEncoding.DecodeString(body.Content)
	if err != nil {
		// Tolerate base64 with newlines (GitHub's own encoding wraps).
		raw, err = base64.StdEncoding.DecodeString(strings.ReplaceAll(body.Content, "\n", ""))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "Bad Request")
			return
		}
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	message := body.Message
	if message == "" {
		message = "Update " + path
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tip, exists := repo.Refs["refs/heads/"+branch]
	var files map[string]string
	var parents []string
	if !exists {
		// A branch that does not exist is created from the default
		// branch tip; on an unborn repository there is no parent.
		if defTip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			tip = defTip
			obj := repo.Objects[tip]
			files = repo.flattenTree(commitTree(obj.Data))
			parents = []string{tip}
		} else {
			files = map[string]string{}
		}
	} else {
		obj := repo.Objects[tip]
		files = repo.flattenTree(commitTree(obj.Data))
		parents = []string{tip}
	}
	blobID := hashObject(objBlob, raw)
	_, isUpdate := files[path]
	if isUpdate {
		if body.SHA == "" || body.SHA != files[path] {
			writeErr(w, http.StatusConflict, "sha does not match")
			return
		}
	}
	// Store the blob, rebuild the tree, commit, move the branch.
	repo.storeObject(objBlob, raw)
	files[path] = blobID
	treeID := repo.buildTree(files)
	commitID := repo.buildCommit(treeID, parents, message)
	repo.Refs["refs/heads/"+branch] = commitID
	status := http.StatusCreated
	if isUpdate {
		status = http.StatusOK
	}
	resp := map[string]any{
		"content": contentFileJSON{
			Type:     "file",
			Encoding: "base64",
			Size:     len(raw),
			Name:     baseName(path),
			Path:     path,
			Content:  base64.StdEncoding.EncodeToString(raw),
			SHA:      blobID,
		},
		"commit": map[string]string{"sha": commitID, "message": message},
	}
	writeJSON(w, status, resp, nil)
}

// restContentsGet handles GET /contents/{path}[?ref=]: a file, or a
// directory listing when the path names a directory.
func restContentsGet(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	ref := r.URL.Query().Get("ref")
	store.mu.RLock()
	defer store.mu.RUnlock()
	tip, ok := repo.resolveRef(ref)
	if ref == "" {
		tip, ok = repo.resolveBranchRef("")
	}
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	obj, ok := repo.Objects[tip]
	if !ok || obj.Type != objCommit {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	files := repo.flattenTree(commitTree(obj.Data))
	if path == "" {
		writeJSON(w, http.StatusOK, dirListing(repo, files, ""), nil)
		return
	}
	if blobID, ok := files[path]; ok {
		blob := repo.Objects[blobID]
		writeJSON(w, http.StatusOK, contentFileJSON{
			Type:     "file",
			Encoding: "base64",
			Size:     len(blob.Data),
			Name:     baseName(path),
			Path:     path,
			Content:  base64.StdEncoding.EncodeToString(blob.Data),
			SHA:      blobID,
		}, nil)
		return
	}
	// A directory: the entries directly under it.
	prefix := path + "/"
	found := false
	for p := range files {
		if strings.HasPrefix(p, prefix) {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, dirListing(repo, files, path), nil)
}

// dirListing returns the entries directly under dir ("" for the root).
// Files carry their blob sha; directories carry their tree sha.
func dirListing(repo *Repo, files map[string]string, dir string) []contentEntryJSON {
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	seen := map[string]contentEntryJSON{}
	for path, blobID := range files {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(path, prefix)
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 1 {
			seen[parts[0]] = contentEntryJSON{Type: "file", Name: parts[0], Path: prefix + parts[0], SHA: blobID}
		} else {
			seen[parts[0]] = contentEntryJSON{Type: "dir", Name: parts[0], Path: prefix + parts[0], SHA: dirTreeSHA(repo, files, prefix+parts[0])}
		}
	}
	out := make([]contentEntryJSON, 0, len(seen))
	for _, e := range seen {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// dirTreeSHA returns the tree oid of the directory at dir, by rebuilding
// the tree for the files under it.
func dirTreeSHA(repo *Repo, files map[string]string, dir string) string {
	sub := map[string]string{}
	prefix := dir + "/"
	for path, blob := range files {
		if strings.HasPrefix(path, prefix) {
			sub[strings.TrimPrefix(path, prefix)] = blob
		}
	}
	return repo.buildTree(sub)
}

// restContentsDelete handles DELETE /contents/{path}.
func restContentsDelete(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	var body contentPutBody
	if !readJSON(r, &body) {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	message := body.Message
	if message == "" {
		message = "Delete " + path
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	tip, exists := repo.Refs["refs/heads/"+branch]
	if !exists {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	obj := repo.Objects[tip]
	files := repo.flattenTree(commitTree(obj.Data))
	blobID, ok := files[path]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.SHA == "" || body.SHA != blobID {
		writeErr(w, http.StatusConflict, "sha does not match")
		return
	}
	delete(files, path)
	treeID := repo.buildTree(files)
	commitID := repo.buildCommit(treeID, []string{tip}, message)
	repo.Refs["refs/heads/"+branch] = commitID
	writeJSON(w, http.StatusOK, map[string]any{
		"commit": map[string]string{"sha": commitID, "message": message},
	}, nil)
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

// labelJSON is the label object the REST API returns.
type labelJSON struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// labelCreateBody is the body of POST /labels.
type labelCreateBody struct {
	Name  string `json:"name"`
	Color string `json:"color"`
}

// restLabels handles POST /labels.
func restLabels(w http.ResponseWriter, r *http.Request, repo *Repo) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	var body labelCreateBody
	if !readJSON(r, &body) {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	if body.Name == "" {
		writeErr(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := repo.Labels[body.Name]; exists {
		writeErr(w, http.StatusUnprocessableEntity, "Label already exists")
		return
	}
	repo.Labels[body.Name] = &Label{Name: body.Name, Color: body.Color}
	writeJSON(w, http.StatusCreated, labelJSON{Name: body.Name, Color: body.Color}, nil)
}

// ---------------------------------------------------------------------------
// Issues and comments
// ---------------------------------------------------------------------------

// issueJSON is the issue object the REST API returns.
type issueJSON struct {
	ID     int64       `json:"id"`
	Number int         `json:"number"`
	Title  string      `json:"title"`
	Body   string      `json:"body"`
	State  string      `json:"state"`
	Labels []labelJSON `json:"labels"`
}

func (r *Repo) issueToJSON(issue *Issue) issueJSON {
	labels := make([]labelJSON, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := ""
		if l, ok := r.Labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, labelJSON{Name: name, Color: color})
	}
	return issueJSON{
		ID:     issue.Seq,
		Number: issue.Number,
		Title:  issue.Title,
		Body:   issue.Body,
		State:  issue.State,
		Labels: labels,
	}
}

// issueCreateBody is the body of POST /issues.
type issueCreateBody struct {
	Title  string `json:"title"`
	Body   string `json:"body"`
	Labels []any  `json:"labels"`
}

// issuePatchBody is the body of PATCH /issues/{n}.
type issuePatchBody struct {
	Title *string `json:"title"`
	Body  *string `json:"body"`
	State *string `json:"state"`
}

// restIssues handles /issues, /issues/{n} and /issues/{n}/comments.
func restIssues(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodPost:
			restIssueCreate(w, r, repo)
		case http.MethodGet:
			restIssueList(w, r, repo)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil || n < 1 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case http.MethodGet:
			restIssueGet(w, r, repo, n)
		case http.MethodPatch:
			restIssuePatch(w, r, repo, n)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case http.MethodPost:
			restCommentCreate(w, r, repo, n)
		case http.MethodGet:
			restCommentList(w, r, repo, n)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// restIssueCreate handles POST /issues.
func restIssueCreate(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var body issueCreateBody
	if !readJSON(r, &body) {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	if body.Title == "" {
		writeErr(w, http.StatusUnprocessableEntity, "title is required")
		return
	}
	var labelNames []string
	for _, l := range body.Labels {
		switch v := l.(type) {
		case string:
			labelNames = append(labelNames, v)
		case map[string]any:
			if name, ok := v["name"].(string); ok {
				labelNames = append(labelNames, name)
			}
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, name := range labelNames {
		if _, ok := repo.Labels[name]; !ok {
			writeErr(w, http.StatusUnprocessableEntity, "unknown label "+name)
			return
		}
	}
	issue := &Issue{
		Number: repo.NextIssue,
		Title:  body.Title,
		Body:   body.Body,
		State:  "open",
		Labels: labelNames,
		Seq:    store.seq + 1,
	}
	store.seq++
	repo.NextIssue++
	repo.Issues[issue.Number] = issue
	writeJSON(w, http.StatusCreated, repo.issueToJSON(issue), nil)
}

// restIssueList handles GET /issues, filtered by state and sorted by
// creation sequence.
func restIssueList(w http.ResponseWriter, r *http.Request, repo *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortBy := q.Get("sort")
	if sortBy == "" {
		sortBy = "created"
	}
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage := queryInt(q, "per_page", 30)
	if perPage < 1 {
		perPage = 30
	}
	page := queryInt(q, "page", 1)
	if page < 1 {
		page = 1
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	var issues []*Issue
	for _, issue := range repo.Issues {
		if state != "all" && issue.State != state {
			continue
		}
		issues = append(issues, issue)
	}
	// sort=created orders by creation sequence.
	sort.Slice(issues, func(i, j int) bool {
		if direction == "asc" {
			return issues[i].Seq < issues[j].Seq
		}
		return issues[i].Seq > issues[j].Seq
	})
	total := len(issues)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	out := make([]issueJSON, 0, end-start)
	for _, issue := range issues[start:end] {
		out = append(out, repo.issueToJSON(issue))
	}
	writeJSON(w, http.StatusOK, out, nil)
}

// restIssueGet handles GET /issues/{n}.
func restIssueGet(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	issue, ok := repo.Issues[n]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, repo.issueToJSON(issue), nil)
}

// restIssuePatch handles PATCH /issues/{n}.
func restIssuePatch(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	var body issuePatchBody
	if !readJSON(r, &body) {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	issue, ok := repo.Issues[n]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.Title != nil {
		issue.Title = *body.Title
	}
	if body.Body != nil {
		issue.Body = *body.Body
	}
	if body.State != nil {
		issue.State = *body.State
	}
	writeJSON(w, http.StatusOK, repo.issueToJSON(issue), nil)
}

// commentJSON is the comment object the REST API returns.
type commentJSON struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
}

// commentCreateBody is the body of POST /issues/{n}/comments.
type commentCreateBody struct {
	Body string `json:"body"`
}

// restCommentCreate handles POST /issues/{n}/comments.
func restCommentCreate(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	var body commentCreateBody
	if !readJSON(r, &body) {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := repo.Issues[n]; !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	comment := &Comment{ID: repo.NextComment, IssueNumber: n, Body: body.Body}
	repo.NextComment++
	repo.Comments[comment.ID] = comment
	writeJSON(w, http.StatusCreated, commentJSON{ID: comment.ID, Body: comment.Body}, nil)
}

// restCommentList handles GET /issues/{n}/comments, in creation order.
func restCommentList(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if _, ok := repo.Issues[n]; !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	var out []commentJSON
	for id := 1; id < repo.NextComment; id++ {
		if c, ok := repo.Comments[id]; ok && c.IssueNumber == n {
			out = append(out, commentJSON{ID: c.ID, Body: c.Body})
		}
	}
	if out == nil {
		out = []commentJSON{}
	}
	writeJSON(w, http.StatusOK, out, nil)
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

// prJSON is the pull request object the REST API returns.
type prJSON struct {
	ID     int64  `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Head   struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Merged        bool   `json:"merged"`
	MergedAt      string `json:"merged_at,omitempty"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
}

// prToJSON renders a pull request, resolving head and base to their
// current branch tips.
func (r *Repo) prToJSON(pr *PR) prJSON {
	var out prJSON
	out.ID = pr.Seq
	out.Number = pr.Number
	out.Title = pr.Title
	out.Body = pr.Body
	out.State = pr.State
	out.Head.Ref = pr.HeadRef
	out.Base.Ref = pr.BaseRef
	if sha, ok := r.Refs["refs/heads/"+pr.HeadRef]; ok {
		out.Head.SHA = sha
	} else {
		out.Head.SHA = pr.HeadSHA
	}
	if sha, ok := r.Refs["refs/heads/"+pr.BaseRef]; ok {
		out.Base.SHA = sha
	} else {
		out.Base.SHA = pr.BaseSHA
	}
	out.Merged = pr.Merged
	out.MergedAt = pr.MergedAt
	out.MergeCommitSHA = pr.MergeSHA
	return out
}

// prCreateBody is the body of POST /pulls.
type prCreateBody struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Head  string `json:"head"`
	Base  string `json:"base"`
}

// prMergeBody is the body of PUT /pulls/{n}/merge.
type prMergeBody struct {
	MergeMethod string `json:"merge_method"`
}

// restPulls handles /pulls, /pulls/{n}, /pulls/{n}/files and /pulls/{n}/merge.
func restPulls(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodPost:
			restPRCreate(w, r, repo)
		case http.MethodGet:
			restPRList(w, r, repo)
		default:
			writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil || n < 1 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 1 {
		if r.Method == http.MethodGet {
			restPRGet(w, r, repo, n)
			return
		}
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == http.MethodGet {
		restPRFiles(w, r, repo, n)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && r.Method == http.MethodPut {
		restPRMerge(w, r, repo, n)
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}

// restPRCreate handles POST /pulls.
func restPRCreate(w http.ResponseWriter, r *http.Request, repo *Repo) {
	var body prCreateBody
	if !readJSON(r, &body) {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	if body.Head == "" || body.Base == "" {
		writeErr(w, http.StatusUnprocessableEntity, "head and base are required")
		return
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	headSHA, ok := repo.Refs["refs/heads/"+body.Head]
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, "head branch not found")
		return
	}
	baseSHA, ok := repo.Refs["refs/heads/"+body.Base]
	if !ok {
		writeErr(w, http.StatusUnprocessableEntity, "base branch not found")
		return
	}
	pr := &PR{
		Number:  repo.NextPR,
		Title:   body.Title,
		Body:    body.Body,
		State:   "open",
		HeadRef: body.Head,
		BaseRef: body.Base,
		HeadSHA: headSHA,
		BaseSHA: baseSHA,
		Seq:     store.seq + 1,
	}
	store.seq++
	repo.NextPR++
	repo.PRs[pr.Number] = pr
	writeJSON(w, http.StatusCreated, repo.prToJSON(pr), nil)
}

// restPRList handles GET /pulls?state=. No state means open only;
// state=all returns open, merged and closed.
func restPRList(w http.ResponseWriter, r *http.Request, repo *Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	var prs []*PR
	for _, pr := range repo.PRs {
		if state != "all" && pr.State != state {
			continue
		}
		prs = append(prs, pr)
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Seq < prs[j].Seq })
	out := make([]prJSON, 0, len(prs))
	for _, pr := range prs {
		out = append(out, repo.prToJSON(pr))
	}
	writeJSON(w, http.StatusOK, out, nil)
}

// restPRGet handles GET /pulls/{n}.
func restPRGet(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	pr, ok := repo.PRs[n]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	writeJSON(w, http.StatusOK, repo.prToJSON(pr), nil)
}

// mergeBase returns the newest commit reachable from both a and b.
func (r *Repo) mergeBase(a, b string) string {
	ancestors := r.reachableCommits([]string{a}, map[string]bool{})
	// Walk b's ancestry newest-first (breadth-first from the tip).
	seen := map[string]bool{}
	queue := []string{b}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if ancestors[cur] {
			return cur
		}
		obj, ok := r.Objects[cur]
		if !ok || obj.Type != objCommit {
			continue
		}
		queue = append(queue, commitParents(obj.Data)...)
	}
	return ""
}

// prFileJSON is one entry of GET /pulls/{n}/files.
type prFileJSON struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

// diffFiles diffs two flat file maps (merge-base vs head) into the file
// list. Line counts come from a simple line-multiset diff.
func diffFiles(repo *Repo, base, head map[string]string) []prFileJSON {
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var names []string
	for p := range paths {
		names = append(names, p)
	}
	sort.Strings(names)
	var out []prFileJSON
	for _, p := range names {
		baseBlob, inBase := base[p]
		headBlob, inHead := head[p]
		if inBase && inHead && baseBlob == headBlob {
			continue
		}
		var f prFileJSON
		f.Filename = p
		switch {
		case !inBase && inHead:
			f.Status = "added"
			f.Additions = countLines(repo.Objects[headBlob].Data)
		case inBase && !inHead:
			f.Status = "removed"
			f.Deletions = countLines(repo.Objects[baseBlob].Data)
		default:
			f.Status = "modified"
			add, del := diffLines(repo.Objects[baseBlob].Data, repo.Objects[headBlob].Data)
			f.Additions, f.Deletions = add, del
		}
		f.Changes = f.Additions + f.Deletions
		out = append(out, f)
	}
	if out == nil {
		out = []prFileJSON{}
	}
	return out
}

// countLines counts the lines in b (a trailing newline does not add one).
func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte("\n"))
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

// diffLines counts added and removed lines between two blobs, by
// multiset difference of their lines.
func diffLines(old, new []byte) (add, del int) {
	oldLines := map[string]int{}
	for _, l := range strings.Split(string(old), "\n") {
		oldLines[l]++
	}
	newLines := map[string]int{}
	for _, l := range strings.Split(string(new), "\n") {
		newLines[l]++
	}
	for l, n := range newLines {
		if d := n - oldLines[l]; d > 0 {
			add += d
		}
	}
	for l, n := range oldLines {
		if d := n - newLines[l]; d > 0 {
			del += d
		}
	}
	return add, del
}

// restPRFiles handles GET /pulls/{n}/files.
func restPRFiles(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	pr, ok := repo.PRs[n]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	headSHA, ok1 := repo.Refs["refs/heads/"+pr.HeadRef]
	baseSHA, ok2 := repo.Refs["refs/heads/"+pr.BaseRef]
	if !ok1 || !ok2 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	base := repo.mergeBase(baseSHA, headSHA)
	var baseTree map[string]string
	if base != "" {
		baseTree = repo.flattenTree(commitTree(repo.Objects[base].Data))
	} else {
		baseTree = map[string]string{}
	}
	headTree := repo.flattenTree(commitTree(repo.Objects[headSHA].Data))
	writeJSON(w, http.StatusOK, diffFiles(repo, baseTree, headTree), nil)
}

// mergeTree three-way merges the head's changes onto the base tip. It
// starts from the base tip's tree and applies the head's changes relative
// to the merge-base. It returns the merged file map and, when a file was
// changed differently on both sides, that file's path as the conflict
// ("" when the merge is clean).
func (r *Repo) mergeTree(baseSHA, headSHA string) (map[string]string, string) {
	base := r.mergeBase(baseSHA, headSHA)
	var baseTree map[string]string
	if base != "" {
		baseTree = r.flattenTree(commitTree(r.Objects[base].Data))
	} else {
		baseTree = map[string]string{}
	}
	headTree := r.flattenTree(commitTree(r.Objects[headSHA].Data))
	baseTipTree := r.flattenTree(commitTree(r.Objects[baseSHA].Data))
	result := map[string]string{}
	for p, b := range baseTipTree {
		result[p] = b
	}
	paths := map[string]bool{}
	for p := range baseTree {
		paths[p] = true
	}
	for p := range headTree {
		paths[p] = true
	}
	for p := range paths {
		baseBlob, inBase := baseTree[p]
		headBlob, inHead := headTree[p]
		if inBase && inHead && baseBlob == headBlob {
			continue // head did not change this path
		}
		if !inBase && !inHead {
			continue
		}
		tipBlob, inTip := baseTipTree[p]
		if inTip && tipBlob == baseBlob {
			// Base tip did not change it: take the head's version.
			if inHead {
				result[p] = headBlob
			} else {
				delete(result, p)
			}
			continue
		}
		if inTip && inHead && tipBlob == headBlob {
			continue // both sides changed it to the same content
		}
		if !inTip && !inBase && inHead {
			result[p] = headBlob
			continue
		}
		// Both sides changed the path, differently: a conflict.
		return nil, p
	}
	return result, ""
}
func restPRMerge(w http.ResponseWriter, r *http.Request, repo *Repo, n int) {
	var body prMergeBody
	// The merge method is optional; read the body leniently.
	data, _ := readBody(r, 1<<20)
	if len(data) > 0 {
		_ = json.Unmarshal(data, &body)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	pr, ok := repo.PRs[n]
	if !ok {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if pr.State != "open" {
		writeErr(w, http.StatusMethodNotAllowed, "pull request is not open")
		return
	}
	headSHA, ok1 := repo.Refs["refs/heads/"+pr.HeadRef]
	baseSHA, ok2 := repo.Refs["refs/heads/"+pr.BaseRef]
	if !ok1 || !ok2 {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	result, conflict := repo.mergeTree(baseSHA, headSHA)
	if conflict != "" {
		writeErr(w, http.StatusMethodNotAllowed, "merge conflict on "+conflict)
		return
	}
	treeID := repo.buildTree(result)
	message := "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.HeadRef
	if pr.Title != "" {
		message = pr.Title
	}
	mergeID := repo.buildCommit(treeID, []string{baseSHA}, message)
	repo.Refs["refs/heads/"+pr.BaseRef] = mergeID
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = fixedTime
	pr.MergeSHA = mergeID
	writeJSON(w, http.StatusOK, map[string]any{
		"merged":  true,
		"sha":     mergeID,
		"message": message,
	}, nil)
}
