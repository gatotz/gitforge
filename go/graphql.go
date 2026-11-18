// GraphQL v4: POST /api/graphql.
//
// A small GraphQL parser and executor for the surface gh and
// github-mcp-server actually send: queries and mutations with variables,
// aliases, inline fragments, inline enum lists and inline input objects,
// plus __type introspection and operation batching. It reads and writes
// the same in-memory store as the REST API, so a mutation over GraphQL
// is visible over REST and the other way round.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Global ids: opaque, stable, never the plain database id or number.
// ---------------------------------------------------------------------------

func globalID(parts ...string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(parts, ":")))
}

func repoGlobalID(repo *Repo) string {
	return globalID("Repository", strconv.FormatInt(repo.ID, 10))
}

func issueGlobalID(repo *Repo, issue *Issue) string {
	return globalID("Issue", strconv.FormatInt(repo.ID, 10), strconv.Itoa(issue.Number))
}

func prGlobalID(repo *Repo, pr *PR) string {
	return globalID("PullRequest", strconv.FormatInt(repo.ID, 10), strconv.Itoa(pr.Number))
}

func commentGlobalID(repo *Repo, comment *Comment) string {
	return globalID("IssueComment", strconv.FormatInt(repo.ID, 10), strconv.Itoa(comment.IssueNumber), strconv.Itoa(comment.ID))
}

// decodeGlobalID splits an opaque global id into its type and parts.
// It reports false when the id is not a base64 global id we minted.
func decodeGlobalID(id string) (string, []string, bool) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", nil, false
	}
	parts := strings.Split(string(raw), ":")
	if len(parts) < 2 {
		return "", nil, false
	}
	switch parts[0] {
	case "Repository", "Issue", "PullRequest", "IssueComment":
		return parts[0], parts[1:], true
	}
	return "", nil, false
}

// repoByGlobalID resolves a Repository global id to its repository.
func repoByGlobalID(id string) *Repo {
	typ, parts, ok := decodeGlobalID(id)
	if !ok || typ != "Repository" || len(parts) != 1 {
		return nil
	}
	dbid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	for _, repo := range store.repos {
		if repo.ID == dbid {
			return repo
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Lexer
// ---------------------------------------------------------------------------

type gqlTokKind int

const (
	tokEOF gqlTokKind = iota
	tokPunct
	tokName
	tokInt
	tokFloat
	tokString
)

type gqlToken struct {
	kind gqlTokKind
	text string
}

func gqlLex(doc string) ([]gqlToken, error) {
	var toks []gqlToken
	i := 0
	for i < len(doc) {
		c := doc[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			// A comment runs to the end of the line.
			for i < len(doc) && doc[i] != '\n' {
				i++
			}
		case c == '{' || c == '}' || c == '(' || c == ')' || c == '[' || c == ']' || c == ':' || c == '=' || c == '!' || c == '$':
			toks = append(toks, gqlToken{tokPunct, string(c)})
			i++
		case c == '.' && i+2 < len(doc) && doc[i+1] == '.' && doc[i+2] == '.':
			toks = append(toks, gqlToken{tokPunct, "..."})
			i += 3
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(doc) && doc[j] != '"' {
				if doc[j] == '\\' && j+1 < len(doc) {
					j++
					switch doc[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					default:
						sb.WriteByte(doc[j])
					}
					j++
					continue
				}
				sb.WriteByte(doc[j])
				j++
			}
			if j >= len(doc) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlToken{tokString, sb.String()})
			i = j + 1
		case c >= '0' && c <= '9' || c == '-':
			j := i
			if doc[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(doc) && doc[j] >= '0' && doc[j] <= '9' {
				j++
			}
			if j < len(doc) && doc[j] == '.' {
				isFloat = true
				j++
				for j < len(doc) && doc[j] >= '0' && doc[j] <= '9' {
					j++
				}
			}
			if j < len(doc) && (doc[j] == 'e' || doc[j] == 'E') {
				isFloat = true
				j++
				if j < len(doc) && (doc[j] == '+' || doc[j] == '-') {
					j++
				}
				for j < len(doc) && doc[j] >= '0' && doc[j] <= '9' {
					j++
				}
			}
			if isFloat {
				toks = append(toks, gqlToken{tokFloat, doc[i:j]})
			} else {
				toks = append(toks, gqlToken{tokInt, doc[i:j]})
			}
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i
			for j < len(doc) && (doc[j] == '_' || doc[j] >= 'a' && doc[j] <= 'z' || doc[j] >= 'A' && doc[j] <= 'Z' || doc[j] >= '0' && doc[j] <= '9') {
				j++
			}
			toks = append(toks, gqlToken{tokName, doc[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	toks = append(toks, gqlToken{tokEOF, ""})
	return toks, nil
}

// ---------------------------------------------------------------------------
// Parser: the subset gh sends.
// ---------------------------------------------------------------------------

// gqlValue is a parsed GraphQL value literal.
type gqlValue struct {
	kind string // "null","bool","int","float","string","enum","var","list","object"
	b    bool
	n    float64
	s    string // string value, enum name, or variable name
	list []*gqlValue
	obj  map[string]*gqlValue
	keys []string
}

// gqlSelection is one field (or inline fragment) in a selection set.
type gqlSelection struct {
	alias string
	name  string
	args  map[string]*gqlValue
	sel   []*gqlSelection
	frag  string // inline fragment type condition, "" for a plain field
}

// gqlOperation is one query or mutation document.
type gqlOperation struct {
	kind string // "query" or "mutation"
	name string
	sel  []*gqlSelection
}

type gqlParser struct {
	toks []gqlToken
	pos  int
}

func (p *gqlParser) peek() gqlToken  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlToken  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) atPunct(s string) bool {
	t := p.peek()
	return t.kind == tokPunct && t.text == s
}
func (p *gqlParser) eatPunct(s string) bool {
	if p.atPunct(s) {
		p.pos++
		return true
	}
	return false
}
func (p *gqlParser) expectPunct(s string) error {
	if !p.eatPunct(s) {
		return fmt.Errorf("expected %q, got %q", s, p.peek().text)
	}
	return nil
}
func (p *gqlParser) expectName() (string, error) {
	t := p.next()
	if t.kind != tokName {
		return "", fmt.Errorf("expected a name, got %q", t.text)
	}
	return t.text, nil
}

func gqlParse(doc string) ([]*gqlOperation, error) {
	toks, err := gqlLex(doc)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	var ops []*gqlOperation
	for p.peek().kind != tokEOF {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (*gqlOperation, error) {
	op := &gqlOperation{kind: "query"}
	if t := p.peek(); t.kind == tokName && (t.text == "query" || t.text == "mutation") {
		op.kind = t.text
		p.next()
	}
	if t := p.peek(); t.kind == tokName {
		op.name = t.text
		p.next()
	}
	// Variable definitions: ($name: Type, ...) — the types are parsed
	// only far enough to skip them.
	if p.atPunct("(") {
		p.next()
		for !p.atPunct(")") {
			if !p.eatPunct("$") {
				return nil, fmt.Errorf("expected a variable")
			}
			if _, err := p.expectName(); err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			if err := p.parseType(); err != nil {
				return nil, err
			}
			if p.eatPunct("=") {
				if _, err := p.parseValue(); err != nil {
					return nil, err
				}
			}
		}
		if err := p.expectPunct(")"); err != nil {
			return nil, err
		}
	}
	sel, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.sel = sel
	return op, nil
}

// parseType skips a GraphQL type: Name, [Type], and trailing ! markers.
func (p *gqlParser) parseType() error {
	if p.atPunct("[") {
		p.next()
		if err := p.parseType(); err != nil {
			return err
		}
		if err := p.expectPunct("]"); err != nil {
			return err
		}
	} else {
		if _, err := p.expectName(); err != nil {
			return err
		}
	}
	p.eatPunct("!")
	return nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sel []*gqlSelection
	for !p.atPunct("}") {
		if p.peek().kind == tokEOF {
			return nil, fmt.Errorf("unterminated selection set")
		}
		s, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sel = append(sel, s)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return sel, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	// Inline fragment: ... on Type { ... } (or a fragment spread, which
	// the suite never sends — it is rejected).
	if p.atPunct("...") {
		p.next()
		if t := p.peek(); t.kind == tokName && t.text == "on" {
			p.next()
			typ, err := p.expectName()
			if err != nil {
				return nil, err
			}
			sel, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			return &gqlSelection{frag: typ, sel: sel}, nil
		}
		return nil, fmt.Errorf("named fragment spreads are not supported")
	}
	// A field, optionally aliased: alias: name(args) { ... }.
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	s := &gqlSelection{name: name, alias: name}
	if p.atPunct(":") {
		p.next()
		real, err := p.expectName()
		if err != nil {
			return nil, err
		}
		s.alias = name
		s.name = real
	}
	if p.atPunct("(") {
		args, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		s.args = args
	}
	if p.atPunct("{") {
		sel, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		s.sel = sel
	}
	return s, nil
}

func (p *gqlParser) parseArguments() (map[string]*gqlValue, error) {
	args := map[string]*gqlValue{}
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	for !p.atPunct(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = v
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue() (*gqlValue, error) {
	t := p.next()
	switch t.kind {
	case tokInt:
		n, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "int", n: n}, nil
	case tokFloat:
		n, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return nil, err
		}
		return &gqlValue{kind: "float", n: n}, nil
	case tokString:
		return &gqlValue{kind: "string", s: t.text}, nil
	case tokName:
		switch t.text {
		case "true":
			return &gqlValue{kind: "bool", b: true}, nil
		case "false":
			return &gqlValue{kind: "bool", b: false}, nil
		case "null":
			return &gqlValue{kind: "null"}, nil
		default:
			return &gqlValue{kind: "enum", s: t.text}, nil
		}
	case tokPunct:
		switch t.text {
		case "$":
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			return &gqlValue{kind: "var", s: name}, nil
		case "[":
			v := &gqlValue{kind: "list"}
			for !p.atPunct("]") {
				item, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.list = append(v.list, item)
			}
			if err := p.expectPunct("]"); err != nil {
				return nil, err
			}
			return v, nil
		case "{":
			v := &gqlValue{kind: "object", obj: map[string]*gqlValue{}}
			for !p.atPunct("}") {
				name, err := p.expectName()
				if err != nil {
					return nil, err
				}
				if err := p.expectPunct(":"); err != nil {
					return nil, err
				}
				val, err := p.parseValue()
				if err != nil {
					return nil, err
				}
				v.obj[name] = val
				v.keys = append(v.keys, name)
			}
			if err := p.expectPunct("}"); err != nil {
				return nil, err
			}
			return v, nil
		}
	}
	return nil, fmt.Errorf("unexpected value token %q", t.text)
}

// resolveValue turns a parsed value into a Go value, substituting
// variables from vars (the request's JSON variables).
func resolveValue(v *gqlValue, vars map[string]any) any {
	if v == nil {
		return nil
	}
	switch v.kind {
	case "null":
		return nil
	case "bool":
		return v.b
	case "int", "float":
		return v.n
	case "string", "enum":
		return v.s
	case "var":
		if vars == nil {
			return nil
		}
		return vars[v.s]
	case "list":
		out := make([]any, 0, len(v.list))
		for _, item := range v.list {
			out = append(out, resolveValue(item, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, k := range v.keys {
			out[k] = resolveValue(v.obj[k], vars)
		}
		return out
	}
	return nil
}

// arg returns the resolved value of a field argument, or nil.
func argOf(sel *gqlSelection, name string, vars map[string]any) any {
	if sel.args == nil {
		return nil
	}
	return resolveValue(sel.args[name], vars)
}

// argString returns an argument as a string, or "".
func argString(sel *gqlSelection, name string, vars map[string]any) string {
	if s, ok := argOf(sel, name, vars).(string); ok {
		return s
	}
	return ""
}

// argInt returns an argument as an int, or def.
func argInt(sel *gqlSelection, name string, vars map[string]any, def int) int {
	switch v := argOf(sel, name, vars).(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
}

// ---------------------------------------------------------------------------
// Schema (for __type introspection)
// ---------------------------------------------------------------------------

// gqlTypeInfo is what __type(name:) reports for one type.
type gqlTypeInfo struct {
	fields []string
	enums  []string
}

var gqlSchema = map[string]gqlTypeInfo{
	"Query": {
		fields: []string{"viewer", "repository", "node", "__type", "__schema"},
	},
	"Mutation": {
		fields: []string{"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	},
	"Repository": {
		fields: []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest"},
	},
	"Issue": {
		fields: []string{"id", "number", "title", "state", "body"},
	},
	"PullRequest": {
		fields: []string{"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files"},
	},
	"User": {
		fields: []string{"login", "id"},
	},
	"IssueConnection": {
		fields: []string{"nodes", "pageInfo", "totalCount"},
	},
	"PullRequestConnection": {
		fields: []string{"nodes", "pageInfo", "totalCount"},
	},
	"PullRequestChangedFileConnection": {
		fields: []string{"nodes", "pageInfo", "totalCount"},
	},
	"PullRequestChangedFile": {
		fields: []string{"path"},
	},
	"PageInfo": {
		fields: []string{"hasNextPage", "endCursor", "hasPreviousPage", "startCursor"},
	},
	"BranchRef": {
		fields: []string{"name"},
	},
	"IssueComment": {
		fields: []string{"id", "body"},
	},
	"IssueCommentEdge": {
		fields: []string{"node", "cursor"},
	},
	"__Type": {
		fields: []string{"name", "kind", "fields", "enumValues"},
	},
	"__Field": {
		fields: []string{"name"},
	},
	"__EnumValue": {
		fields: []string{"name"},
	},
	"CreateIssuePayload": {
		fields: []string{"issue", "clientMutationId"},
	},
	"CloseIssuePayload": {
		fields: []string{"issue", "clientMutationId"},
	},
	"CreatePullRequestPayload": {
		fields: []string{"pullRequest", "clientMutationId"},
	},
	"MergePullRequestPayload": {
		fields: []string{"pullRequest", "clientMutationId"},
	},
	"AddCommentPayload": {
		fields: []string{"commentEdge", "clientMutationId"},
	},
	"SearchType": {
		enums: []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"},
	},
	"IssueState": {
		enums: []string{"OPEN", "CLOSED"},
	},
	"PullRequestState": {
		enums: []string{"OPEN", "CLOSED", "MERGED"},
	},
	"MergeableState": {
		enums: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	},
}

// fieldNames returns the valid field names for a type, for validation.
func fieldNames(typ string) []string {
	if info, ok := gqlSchema[typ]; ok {
		return info.fields
	}
	return nil
}

func hasField(typ, name string) bool {
	for _, f := range fieldNames(typ) {
		if f == name {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Executor
// ---------------------------------------------------------------------------

type gqlError struct {
	Message string `json:"message"`
}

// gqlVal is one resolved value: a scalar, or an object of a known type.
type gqlVal struct {
	typ     string // object type, "" for a scalar
	scalar  any
	repo    *Repo
	issue   *Issue
	pr      *PR
	comment *Comment
	conn    *gqlConn
	typeDef string // for __Type values: the type being described
	branch  string // for BranchRef values: the branch name
}

// gqlConn is a connection value (nodes + pageInfo).
type gqlConn struct {
	kind    string // "issue", "pr", "file"
	nodes   []gqlVal
	total   int
	hasNext bool
	cursor  string
}

// gqlExec carries the per-operation state.
type gqlExec struct {
	vars map[string]any
	errs []gqlError
}

// errf records a GraphQL error.
func (e *gqlExec) errf(format string, args ...any) {
	e.errs = append(e.errs, gqlError{Message: fmt.Sprintf(format, args...)})
}

// execObject resolves a selection set against an object value and returns
// the result map. Unknown fields are reported as errors and read as null.
func (e *gqlExec) execObject(typ string, val gqlVal, sel []*gqlSelection) map[string]any {
	out := map[string]any{}
	for _, s := range sel {
		if s.frag != "" {
			// An inline fragment applies when its type matches.
			if s.frag == typ {
				for k, v := range e.execObject(typ, val, s.sel) {
					out[k] = v
				}
			}
			continue
		}
		if !hasField(typ, s.name) {
			e.errf("Cannot query field %q on type %q.", s.name, typ)
			out[s.alias] = nil
			continue
		}
		out[s.alias] = e.resolveField(typ, val, s)
	}
	return out
}

// resolveField resolves one field of an object.
func (e *gqlExec) resolveField(typ string, val gqlVal, s *gqlSelection) any {
	switch typ {
	case "Query":
		return e.resolveQueryField(s)
	case "Mutation":
		return e.resolveMutationField(s)
	case "Repository":
		return e.resolveRepoField(val.repo, s)
	case "Issue":
		return e.resolveIssueField(val.repo, val.issue, s)
	case "PullRequest":
		return e.resolvePRField(val.repo, val.pr, s)
	case "User":
		return e.resolveUserField(s)
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection":
		return e.resolveConnField(val.conn, s)
	case "PullRequestChangedFile":
		return e.resolveFileField(val, s)
	case "PageInfo":
		return e.resolvePageInfoField(val.conn, s)
	case "BranchRef":
		if s.name == "name" {
			return val.branch
		}
		return nil
	case "__Type":
		return e.resolveTypeField(val.typeDef, s)
	case "__Field", "__EnumValue":
		if s.name == "name" {
			return val.scalar
		}
		return nil
	case "IssueComment":
		return e.resolveCommentField(val.repo, val.comment, s)
	case "IssueCommentEdge":
		if s.name == "node" {
			if len(s.sel) == 0 {
				return nil
			}
			return e.execObject("IssueComment", val, s.sel)
		}
		if s.name == "cursor" {
			return ""
		}
		return nil
	case "CreateIssuePayload", "CloseIssuePayload", "CreatePullRequestPayload", "MergePullRequestPayload", "AddCommentPayload":
		return e.resolvePayloadField(typ, val, s)
	}
	return nil
}

// resolveQueryField resolves a root Query field.
func (e *gqlExec) resolveQueryField(s *gqlSelection) any {
	switch s.name {
	case "viewer":
		if len(s.sel) == 0 {
			return nil
		}
		return e.execObject("User", gqlVal{typ: "User"}, s.sel)
	case "repository":
		owner := argString(s, "owner", e.vars)
		name := argString(s, "name", e.vars)
		store.mu.RLock()
		repo := store.getRepo(owner, name)
		store.mu.RUnlock()
		if repo == nil {
			return nil
		}
		if len(s.sel) == 0 {
			return nil
		}
		return e.execObject("Repository", gqlVal{typ: "Repository", repo: repo}, s.sel)
	case "node":
		id := argString(s, "id", e.vars)
		typ, parts, ok := decodeGlobalID(id)
		if !ok {
			return nil
		}
		switch typ {
		case "Repository":
			if len(parts) != 1 {
				return nil
			}
			dbid, err := strconv.ParseInt(parts[0], 10, 64)
			if err != nil {
				return nil
			}
			store.mu.RLock()
			var repo *Repo
			for _, r := range store.repos {
				if r.ID == dbid {
					repo = r
					break
				}
			}
			store.mu.RUnlock()
			if repo == nil || len(s.sel) == 0 {
				return nil
			}
			return e.execObject("Repository", gqlVal{typ: "Repository", repo: repo}, s.sel)
		case "Issue":
			if len(parts) != 2 {
				return nil
			}
			dbid, err1 := strconv.ParseInt(parts[0], 10, 64)
			num, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil {
				return nil
			}
			store.mu.RLock()
			var repo *Repo
			for _, r := range store.repos {
				if r.ID == dbid {
					repo = r
					break
				}
			}
			var issue *Issue
			if repo != nil {
				issue = repo.Issues[num]
			}
			store.mu.RUnlock()
			if issue == nil || len(s.sel) == 0 {
				return nil
			}
			return e.execObject("Issue", gqlVal{typ: "Issue", repo: repo, issue: issue}, s.sel)
		}
		return nil
	case "__type":
		name := argString(s, "name", e.vars)
		if _, ok := gqlSchema[name]; !ok {
			return nil
		}
		if len(s.sel) == 0 {
			return nil
		}
		return e.execObject("__Type", gqlVal{typ: "__Type", typeDef: name}, s.sel)
	case "__schema":
		return nil
	}
	return nil
}

// resolveRepoField resolves one Repository field.
func (e *gqlExec) resolveRepoField(repo *Repo, s *gqlSelection) any {
	switch s.name {
	case "id":
		return repoGlobalID(repo)
	case "databaseId":
		return repo.ID
	case "name":
		return repo.Name
	case "nameWithOwner":
		return repo.Owner + "/" + repo.Name
	case "owner":
		if len(s.sel) == 0 {
			return nil
		}
		return e.execObject("User", gqlVal{typ: "User"}, s.sel)
	case "defaultBranchRef":
		store.mu.RLock()
		_, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		store.mu.RUnlock()
		if !ok || len(s.sel) == 0 {
			return nil
		}
		return e.execObject("BranchRef", gqlVal{typ: "BranchRef", branch: repo.DefaultBranch}, s.sel)
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "issue":
		num := argInt(s, "number", e.vars, 0)
		store.mu.RLock()
		issue := repo.Issues[num]
		store.mu.RUnlock()
		if issue == nil || len(s.sel) == 0 {
			return nil
		}
		return e.execObject("Issue", gqlVal{typ: "Issue", repo: repo, issue: issue}, s.sel)
	case "issues":
		return e.issuesConnection(repo, s)
	case "pullRequests":
		return e.prsConnection(repo, s)
	case "pullRequest":
		num := argInt(s, "number", e.vars, 0)
		store.mu.RLock()
		pr := repo.PRs[num]
		store.mu.RUnlock()
		if pr == nil || len(s.sel) == 0 {
			return nil
		}
		return e.execObject("PullRequest", gqlVal{typ: "PullRequest", repo: repo, pr: pr}, s.sel)
	}
	return nil
}

// issuesConnection builds the issues connection for a repository.
func (e *gqlExec) issuesConnection(repo *Repo, s *gqlSelection) any {
	if len(s.sel) == 0 {
		return nil
	}
	first := argInt(s, "first", e.vars, 0)
	after := argString(s, "after", e.vars)
	// states: an inline enum list, or a variable holding one.
	var states []string
	if raw := argOf(s, "states", e.vars); raw != nil {
		if list, ok := raw.([]any); ok {
			for _, item := range list {
				if str, ok := item.(string); ok {
					states = append(states, str)
				}
			}
		}
	}
	// orderBy: {field, direction} — default CREATED_AT DESC.
	direction := "DESC"
	if raw := argOf(s, "orderBy", e.vars); raw != nil {
		if obj, ok := raw.(map[string]any); ok {
			if d, ok := obj["direction"].(string); ok && d != "" {
				direction = d
			}
		}
	}
	store.mu.RLock()
	var issues []*Issue
	for _, issue := range repo.Issues {
		if len(states) > 0 {
			want := "OPEN"
			if issue.State == "closed" {
				want = "CLOSED"
			}
			found := false
			for _, st := range states {
				if st == want {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		issues = append(issues, issue)
	}
	store.mu.RUnlock()
	// CREATED_AT order is the creation sequence (issue number).
	sort.Slice(issues, func(i, j int) bool {
		if direction == "ASC" {
			return issues[i].Number < issues[j].Number
		}
		return issues[i].Number > issues[j].Number
	})
	// after: a cursor holding the last number of the previous page.
	if after != "" {
		if raw, err := base64.StdEncoding.DecodeString(after); err == nil {
			if n, err := strconv.Atoi(string(raw)); err == nil {
				var kept []*Issue
				for _, issue := range issues {
					if direction == "ASC" {
						if issue.Number > n {
							kept = append(kept, issue)
						}
					} else {
						if issue.Number < n {
							kept = append(kept, issue)
						}
					}
				}
				issues = kept
			}
		}
	}
	total := len(issues)
	hasNext := false
	if first > 0 && total > first {
		hasNext = true
		issues = issues[:first]
	}
	cursor := ""
	if len(issues) > 0 {
		cursor = base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(issues[len(issues)-1].Number)))
	}
	conn := &gqlConn{kind: "issue", total: total, hasNext: hasNext, cursor: cursor}
	for _, issue := range issues {
		conn.nodes = append(conn.nodes, gqlVal{typ: "Issue", repo: repo, issue: issue})
	}
	return e.execObject("IssueConnection", gqlVal{typ: "IssueConnection", conn: conn}, s.sel)
}

// prsConnection builds the pull-requests connection for a repository.
func (e *gqlExec) prsConnection(repo *Repo, s *gqlSelection) any {
	if len(s.sel) == 0 {
		return nil
	}
	first := argInt(s, "first", e.vars, 0)
	store.mu.RLock()
	var prs []*PR
	for _, pr := range repo.PRs {
		if pr.State != "open" {
			continue
		}
		prs = append(prs, pr)
	}
	store.mu.RUnlock()
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	total := len(prs)
	hasNext := false
	if first > 0 && total > first {
		hasNext = true
		prs = prs[:first]
	}
	cursor := ""
	if len(prs) > 0 {
		cursor = base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(prs[len(prs)-1].Number)))
	}
	conn := &gqlConn{kind: "pr", total: total, hasNext: hasNext, cursor: cursor}
	for _, pr := range prs {
		conn.nodes = append(conn.nodes, gqlVal{typ: "PullRequest", repo: repo, pr: pr})
	}
	return e.execObject("PullRequestConnection", gqlVal{typ: "PullRequestConnection", conn: conn}, s.sel)
}

// resolveIssueField resolves one Issue field.
func (e *gqlExec) resolveIssueField(repo *Repo, issue *Issue, s *gqlSelection) any {
	switch s.name {
	case "id":
		return issueGlobalID(repo, issue)
	case "number":
		return issue.Number
	case "title":
		return issue.Title
	case "state":
		if issue.State == "closed" {
			return "CLOSED"
		}
		return "OPEN"
	case "body":
		return issue.Body
	}
	return nil
}

// resolvePRField resolves one PullRequest field.
func (e *gqlExec) resolvePRField(repo *Repo, pr *PR, s *gqlSelection) any {
	switch s.name {
	case "id":
		return prGlobalID(repo, pr)
	case "number":
		return pr.Number
	case "headRefName":
		return pr.HeadRef
	case "baseRefName":
		return pr.BaseRef
	case "state":
		if pr.Merged {
			return "MERGED"
		}
		if pr.State == "closed" {
			return "CLOSED"
		}
		return "OPEN"
	case "merged":
		return pr.Merged
	case "mergeable":
		if pr.State != "open" {
			return "UNKNOWN"
		}
		store.mu.RLock()
		headSHA, ok1 := repo.Refs["refs/heads/"+pr.HeadRef]
		baseSHA, ok2 := repo.Refs["refs/heads/"+pr.BaseRef]
		var conflict string
		if ok1 && ok2 {
			_, conflict = repo.mergeTree(baseSHA, headSHA)
		}
		store.mu.RUnlock()
		if conflict != "" {
			return "CONFLICTING"
		}
		return "MERGEABLE"
	case "files":
		return e.prFilesConnection(repo, pr, s)
	}
	return nil
}

// prFilesConnection builds the changed-files connection for a pull request.
func (e *gqlExec) prFilesConnection(repo *Repo, pr *PR, s *gqlSelection) any {
	if len(s.sel) == 0 {
		return nil
	}
	first := argInt(s, "first", e.vars, 0)
	store.mu.RLock()
	headSHA, ok1 := repo.Refs["refs/heads/"+pr.HeadRef]
	baseSHA, ok2 := repo.Refs["refs/heads/"+pr.BaseRef]
	var files []prFileJSON
	if ok1 && ok2 {
		base := repo.mergeBase(baseSHA, headSHA)
		var baseTree map[string]string
		if base != "" {
			baseTree = repo.flattenTree(commitTree(repo.Objects[base].Data))
		} else {
			baseTree = map[string]string{}
		}
		headTree := repo.flattenTree(commitTree(repo.Objects[headSHA].Data))
		files = diffFiles(repo, baseTree, headTree)
	}
	store.mu.RUnlock()
	total := len(files)
	hasNext := false
	if first > 0 && total > first {
		hasNext = true
		files = files[:first]
	}
	cursor := ""
	if len(files) > 0 {
		cursor = base64.StdEncoding.EncodeToString([]byte(files[len(files)-1].Filename))
	}
	conn := &gqlConn{kind: "file", total: total, hasNext: hasNext, cursor: cursor}
	for _, f := range files {
		conn.nodes = append(conn.nodes, gqlVal{typ: "PullRequestChangedFile", scalar: f.Filename})
	}
	return e.execObject("PullRequestChangedFileConnection", gqlVal{typ: "PullRequestChangedFileConnection", conn: conn}, s.sel)
}

// resolveConnField resolves one connection field (nodes, pageInfo).
func (e *gqlExec) resolveConnField(conn *gqlConn, s *gqlSelection) any {
	switch s.name {
	case "nodes":
		out := make([]any, 0, len(conn.nodes))
		typ := ""
		switch conn.kind {
		case "issue":
			typ = "Issue"
		case "pr":
			typ = "PullRequest"
		case "file":
			typ = "PullRequestChangedFile"
		}
		for _, node := range conn.nodes {
			if len(s.sel) == 0 {
				out = append(out, nil)
				continue
			}
			out = append(out, e.execObject(typ, node, s.sel))
		}
		return out
	case "pageInfo":
		if len(s.sel) == 0 {
			return nil
		}
		return e.execObject("PageInfo", gqlVal{typ: "PageInfo", conn: conn}, s.sel)
	case "totalCount":
		return conn.total
	}
	return nil
}

// resolveFileField resolves one PullRequestChangedFile field.
func (e *gqlExec) resolveFileField(val gqlVal, s *gqlSelection) any {
	if s.name == "path" {
		if str, ok := val.scalar.(string); ok {
			return str
		}
	}
	return nil
}

// resolvePageInfoField resolves one PageInfo field.
func (e *gqlExec) resolvePageInfoField(conn *gqlConn, s *gqlSelection) any {
	switch s.name {
	case "hasNextPage":
		return conn.hasNext
	case "endCursor":
		if conn.cursor == "" {
			return nil
		}
		return conn.cursor
	case "hasPreviousPage":
		return false
	case "startCursor":
		return nil
	}
	return nil
}

// resolveUserField resolves one User field.
func (e *gqlExec) resolveUserField(s *gqlSelection) any {
	switch s.name {
	case "login":
		return viewerLogin
	case "id":
		return globalID("User", strconv.Itoa(viewerID))
	}
	return nil
}

// resolveTypeField resolves one __Type field (fields / enumValues).
func (e *gqlExec) resolveTypeField(typeName string, s *gqlSelection) any {
	info := gqlSchema[typeName]
	switch s.name {
	case "name":
		return typeName
	case "kind":
		if len(info.enums) > 0 {
			return "ENUM"
		}
		return "OBJECT"
	case "fields":
		var out []any
		for _, name := range info.fields {
			if len(s.sel) == 0 {
				out = append(out, map[string]any{"name": name})
				continue
			}
			out = append(out, e.execObject("__Field", gqlVal{typ: "__Field", scalar: name}, s.sel))
		}
		if out == nil {
			out = []any{}
		}
		return out
	case "enumValues":
		var out []any
		for _, name := range info.enums {
			if len(s.sel) == 0 {
				out = append(out, map[string]any{"name": name})
				continue
			}
			out = append(out, e.execObject("__EnumValue", gqlVal{typ: "__EnumValue", scalar: name}, s.sel))
		}
		if out == nil {
			out = []any{}
		}
		return out
	}
	return nil
}

// resolveCommentField resolves one IssueComment field.
func (e *gqlExec) resolveCommentField(repo *Repo, comment *Comment, s *gqlSelection) any {
	switch s.name {
	case "id":
		return commentGlobalID(repo, comment)
	case "body":
		return comment.Body
	}
	return nil
}

// resolvePayloadField resolves one mutation-payload field.
func (e *gqlExec) resolvePayloadField(typ string, val gqlVal, s *gqlSelection) any {
	// The payload value carries the object it wraps in val.
	switch s.name {
	case "issue":
		if val.issue == nil || len(s.sel) == 0 {
			return nil
		}
		return e.execObject("Issue", gqlVal{typ: "Issue", repo: val.repo, issue: val.issue}, s.sel)
	case "pullRequest":
		if val.pr == nil || len(s.sel) == 0 {
			return nil
		}
		return e.execObject("PullRequest", gqlVal{typ: "PullRequest", repo: val.repo, pr: val.pr}, s.sel)
	case "commentEdge":
		if val.comment == nil || len(s.sel) == 0 {
			return nil
		}
		return e.execObject("IssueCommentEdge", gqlVal{typ: "IssueCommentEdge", repo: val.repo, comment: val.comment}, s.sel)
	case "clientMutationId":
		return nil
	}
	return nil
}

// resolveMutationField resolves one Mutation field. The caller holds the
// store write lock.
func (e *gqlExec) resolveMutationField(s *gqlSelection) any {
	input, _ := argOf(s, "input", e.vars).(map[string]any)
	if input == nil {
		e.errf("mutation %q needs an input object", s.name)
		return nil
	}
	switch s.name {
	case "createIssue":
		repoID, _ := input["repositoryId"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		repo := repoByGlobalIDNoLock(repoID)
		if repo == nil {
			e.errf("createIssue: unknown repositoryId")
			return nil
		}
		if title == "" {
			e.errf("createIssue: title is required")
			return nil
		}
		issue := &Issue{Number: repo.NextIssue, Title: title, Body: body, State: "open", Seq: store.seq + 1}
		store.seq++
		repo.NextIssue++
		repo.Issues[issue.Number] = issue
		if len(s.sel) == 0 {
			return nil
		}
		return e.execObject("CreateIssuePayload", gqlVal{typ: "CreateIssuePayload", repo: repo, issue: issue}, s.sel)
	case "closeIssue":
		issueID, _ := input["issueId"].(string)
		typ, parts, ok := decodeGlobalID(issueID)
		if !ok || typ != "Issue" || len(parts) != 2 {
			e.errf("closeIssue: unknown issueId")
			return nil
		}
		dbid, err1 := strconv.ParseInt(parts[0], 10, 64)
		num, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			e.errf("closeIssue: unknown issueId")
			return nil
		}
		var repo *Repo
		for _, r := range store.repos {
			if r.ID == dbid {
				repo = r
				break
			}
		}
		if repo == nil {
			e.errf("closeIssue: unknown issueId")
			return nil
		}
		issue := repo.Issues[num]
		if issue == nil {
			e.errf("closeIssue: unknown issueId")
			return nil
		}
		issue.State = "closed"
		if len(s.sel) == 0 {
			return nil
		}
		return e.execObject("CloseIssuePayload", gqlVal{typ: "CloseIssuePayload", repo: repo, issue: issue}, s.sel)
	case "createPullRequest":
		repoID, _ := input["repositoryId"].(string)
		baseRef, _ := input["baseRefName"].(string)
		headRef, _ := input["headRefName"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		repo := repoByGlobalIDNoLock(repoID)
		if repo == nil {
			e.errf("createPullRequest: unknown repositoryId")
			return nil
		}
		headSHA, ok1 := repo.Refs["refs/heads/"+headRef]
		baseSHA, ok2 := repo.Refs["refs/heads/"+baseRef]
		if !ok1 || !ok2 {
			e.errf("createPullRequest: head or base branch not found")
			return nil
		}
		pr := &PR{Number: repo.NextPR, Title: title, Body: body, State: "open", HeadRef: headRef, BaseRef: baseRef, HeadSHA: headSHA, BaseSHA: baseSHA, Seq: store.seq + 1}
		store.seq++
		repo.NextPR++
		repo.PRs[pr.Number] = pr
		if len(s.sel) == 0 {
			return nil
		}
		return e.execObject("CreatePullRequestPayload", gqlVal{typ: "CreatePullRequestPayload", repo: repo, pr: pr}, s.sel)
	case "mergePullRequest":
		prID, _ := input["pullRequestId"].(string)
		typ, parts, ok := decodeGlobalID(prID)
		if !ok || typ != "PullRequest" || len(parts) != 2 {
			e.errf("mergePullRequest: unknown pullRequestId")
			return nil
		}
		dbid, err1 := strconv.ParseInt(parts[0], 10, 64)
		num, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			e.errf("mergePullRequest: unknown pullRequestId")
			return nil
		}
		var repo *Repo
		for _, r := range store.repos {
			if r.ID == dbid {
				repo = r
				break
			}
		}
		if repo == nil {
			e.errf("mergePullRequest: unknown pullRequestId")
			return nil
		}
		pr := repo.PRs[num]
		if pr == nil || pr.State != "open" {
			e.errf("mergePullRequest: pull request is not open")
			return nil
		}
		headSHA, ok1 := repo.Refs["refs/heads/"+pr.HeadRef]
		baseSHA, ok2 := repo.Refs["refs/heads/"+pr.BaseRef]
		if !ok1 || !ok2 {
			e.errf("mergePullRequest: head or base branch not found")
			return nil
		}
		result, conflict := repo.mergeTree(baseSHA, headSHA)
		if conflict != "" {
			e.errf("mergePullRequest: merge conflict on %s", conflict)
			return nil
		}
		treeID := repo.buildTree(result)
		message := pr.Title
		if message == "" {
			message = "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.HeadRef
		}
		mergeID := repo.buildCommit(treeID, []string{baseSHA}, message)
		repo.Refs["refs/heads/"+pr.BaseRef] = mergeID
		pr.State = "closed"
		pr.Merged = true
		pr.MergedAt = fixedTime
		pr.MergeSHA = mergeID
		if len(s.sel) == 0 {
			return nil
		}
		return e.execObject("MergePullRequestPayload", gqlVal{typ: "MergePullRequestPayload", repo: repo, pr: pr}, s.sel)
	case "addComment":
		subjectID, _ := input["subjectId"].(string)
		body, _ := input["body"].(string)
		typ, parts, ok := decodeGlobalID(subjectID)
		if !ok || typ != "Issue" || len(parts) != 2 {
			e.errf("addComment: unknown subjectId")
			return nil
		}
		dbid, err1 := strconv.ParseInt(parts[0], 10, 64)
		num, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			e.errf("addComment: unknown subjectId")
			return nil
		}
		var repo *Repo
		for _, r := range store.repos {
			if r.ID == dbid {
				repo = r
				break
			}
		}
		if repo == nil {
			e.errf("addComment: unknown subjectId")
			return nil
		}
		if _, ok := repo.Issues[num]; !ok {
			e.errf("addComment: unknown subjectId")
			return nil
		}
		comment := &Comment{ID: repo.NextComment, IssueNumber: num, Body: body}
		repo.NextComment++
		repo.Comments[comment.ID] = comment
		if len(s.sel) == 0 {
			return nil
		}
		return e.execObject("AddCommentPayload", gqlVal{typ: "AddCommentPayload", repo: repo, comment: comment}, s.sel)
	}
	return nil
}

// repoByGlobalIDNoLock resolves a Repository global id. The caller holds
// a store lock (read or write).
func repoByGlobalIDNoLock(id string) *Repo {
	typ, parts, ok := decodeGlobalID(id)
	if !ok || typ != "Repository" || len(parts) != 1 {
		return nil
	}
	dbid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil
	}
	for _, repo := range store.repos {
		if repo.ID == dbid {
			return repo
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

// execGraphQLRequest executes one GraphQL request body (a JSON object with
// query/variables) and returns the response object. The caller has already
// checked the bearer token.
func execGraphQLRequest(raw []byte) map[string]any {
	var req struct {
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
		OperationName string         `json:"operationName"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return map[string]any{"data": nil, "errors": []gqlError{{Message: "request body is not valid JSON"}}}
	}
	if req.Query == "" {
		return map[string]any{"data": nil, "errors": []gqlError{{Message: "no query in request"}}}
	}
	ops, err := gqlParse(req.Query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []gqlError{{Message: err.Error()}}}
	}
	op := ops[0]
	if req.OperationName != "" {
		for _, o := range ops {
			if o.name == req.OperationName {
				op = o
				break
			}
		}
	}
	exec := &gqlExec{vars: req.Variables}
	var data map[string]any
	if op.kind == "mutation" {
		store.mu.Lock()
		data = exec.execObject("Mutation", gqlVal{typ: "Mutation"}, op.sel)
		store.mu.Unlock()
	} else {
		store.mu.RLock()
		data = exec.execObject("Query", gqlVal{typ: "Query"}, op.sel)
		store.mu.RUnlock()
	}
	resp := map[string]any{"data": data}
	if len(exec.errs) > 0 {
		resp["errors"] = exec.errs
	}
	return resp
}

func handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	body, ok := readBody(r, 64<<20)
	if !ok {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		writeErr(w, http.StatusBadRequest, "Bad Request")
		return
	}
	if trimmed[0] == '[' {
		// Operation batching: a JSON array of requests, answered in order.
		var batch []json.RawMessage
		if err := json.Unmarshal(body, &batch); err != nil {
			writeErr(w, http.StatusBadRequest, "Bad Request")
			return
		}
		out := make([]any, 0, len(batch))
		for _, raw := range batch {
			out = append(out, execGraphQLRequest(raw))
		}
		writeJSON(w, http.StatusOK, out, nil)
		return
	}
	writeJSON(w, http.StatusOK, execGraphQLRequest(body), nil)
}
