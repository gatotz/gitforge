// The forge's in-memory store. One store holds every repository, its git
// objects and refs, and its issues, pull requests, labels and comments.
//
// Concurrency: a single RWMutex guards the whole store. Handlers take the
// read lock for reads and the write lock for anything that mutates, and hold
// it for the whole request, so a push's fast-forward check and ref update
// are atomic and a fetch sees a consistent snapshot. The helper methods
// below assume the caller already holds the appropriate lock.
package main

import (
	"crypto/sha1"
	"encoding/hex"
	"sort"
	"strconv"
	"sync"
)

// Git object type numbers.
const (
	objCommit = 1
	objTree   = 2
	objBlob   = 3
	objTag    = 4
)

var objTypeName = [...]string{"", "commit", "tree", "blob", "tag"}

// zeroOID is the all-zero object id: the old id of a created ref, the new
// id of a deleted one.
const zeroOID = "0000000000000000000000000000000000000000"

// Object is one git object, stored by its SHA-1.
type Object struct {
	Type int
	Data []byte // raw content, without the "<type> <size>\0" header
}

// hashObject returns the SHA-1 id of an object with the given type and data.
func hashObject(typ int, data []byte) string {
	h := sha1.New()
	h.Write([]byte(objTypeName[typ] + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// storeObject stores obj in repo and returns its id.
func (r *Repo) storeObject(typ int, data []byte) string {
	id := hashObject(typ, data)
	if _, ok := r.Objects[id]; !ok {
		r.Objects[id] = &Object{Type: typ, Data: data}
	}
	return id
}

// Label is one repository label.
type Label struct {
	Name  string
	Color string
}

// Issue is one repository issue.
type Issue struct {
	Number int
	Title  string
	Body   string
	State  string // "open" or "closed"
	Labels []string
	Seq    int64 // creation sequence; orders lists newest-first when needed
}

// Comment is one issue comment. IDs increment per repository.
type Comment struct {
	ID          int
	IssueNumber int
	Body        string
}

// PR is one pull request.
type PR struct {
	Number   int
	Title    string
	Body     string
	State    string // "open" or "closed"; a merged PR is closed with Merged set
	HeadRef  string // head branch name (bare, no refs/heads/ prefix)
	BaseRef  string // base branch name
	HeadSHA  string // head branch tip at creation (refreshed on read)
	BaseSHA  string // base branch tip at creation
	MergeSHA string // merge commit, once merged
	Merged   bool
	MergedAt string
	Seq      int64
}

// Repo is one repository: its git objects and refs plus its REST resources.
type Repo struct {
	ID            int64
	Name          string
	Owner         string
	Description   string
	Private       bool
	DefaultBranch string
	Seq           int64 // creation sequence; orders the repository list
	Refs          map[string]string // ref name -> oid, e.g. refs/heads/main
	Objects       map[string]*Object
	Issues        map[int]*Issue
	NextIssue     int
	Labels        map[string]*Label
	PRs           map[int]*PR
	NextPR        int
	Comments      map[int]*Comment // comment id -> comment
	NextComment   int
}

// Store is the forge's whole state.
type Store struct {
	mu         sync.RWMutex
	repos      map[string]*Repo // key: owner + "/" + name
	nextRepoID int64
	seq        int64
}

func newStore() *Store {
	return &Store{repos: make(map[string]*Repo)}
}

// createRepo makes a repository owned by the viewer. It reports false when
// a repository of that name already exists for the viewer.
func (s *Store) createRepo(name string, private bool, description string) (*Repo, bool) {
	key := viewerLogin + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil, false
	}
	s.nextRepoID++
	s.seq++
	r := &Repo{
		ID:            s.nextRepoID,
		Name:          name,
		Owner:         viewerLogin,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		Seq:           s.seq,
		Refs:          make(map[string]string),
		Objects:       make(map[string]*Object),
		Issues:        make(map[int]*Issue),
		NextIssue:     1,
		Labels:        make(map[string]*Label),
		PRs:           make(map[int]*PR),
		NextPR:        1,
		Comments:      make(map[int]*Comment),
		NextComment:   1,
	}
	s.repos[key] = r
	return r, true
}

// getRepo returns the repository owned by owner with the given name, or nil.
// Any owner other than the viewer is a 404: repositories belong to the
// login the token names, never to anyone else.
func (s *Store) getRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[owner+"/"+name]
}

// listRepos returns every repository the viewer owns, in creation order.
func (s *Store) listRepos() []*Repo {
	out := make([]*Repo, 0, len(s.repos))
	for _, r := range s.repos {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// deleteRepo removes the viewer's repository of the given name.
// It reports false when there is nothing to delete.
func (s *Store) deleteRepo(name string) bool {
	key := viewerLogin + "/" + name
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	return true
}

// refNames returns the repository's ref names in sorted order, so
// advertisements and listings are deterministic.
func (r *Repo) refNames() []string {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// branchNames returns the repository's branch names (refs/heads/*) sorted.
func (r *Repo) branchNames() []string {
	var out []string
	for name := range r.Refs {
		if len(name) > 11 && name[:11] == "refs/heads/" {
			out = append(out, name[11:])
		}
	}
	sort.Strings(out)
	return out
}
