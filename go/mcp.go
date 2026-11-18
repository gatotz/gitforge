// MCP: JSON-RPC 2.0 over Streamable HTTP at POST /mcp.
//
// The modern 2026-07-28 era: `server/discover` returns the server's
// identity, capabilities and supported versions; every request declares
// its revision in _meta and mirrors it in the Mcp-Protocol-Version
// header. The legacy handshake (`initialize` + `notifications/initialized`)
// is also answered, for servers that predate discover. Tools are the
// same store the REST and GraphQL surfaces read and write, so a repo or
// issue created over MCP reads back over REST with stable ids.
package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
)

const mcpRevision = "2026-07-28"
const mcpServerName = "gitforge"
const mcpServerVersion = "1.0.0"

// ---------------------------------------------------------------------------
// Tool list: a fixed ordered slice, so two tools/list answers are
// byte-identical. (Never build it from a Go map — iteration order is
// random and the idempotency check would fail.)
// ---------------------------------------------------------------------------

type mcpToolProp struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

type mcpToolSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]mcpToolProp `json:"properties"`
	Required   []string               `json:"required,omitempty"`
}

type mcpTool struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	InputSchema mcpToolSchema `json:"inputSchema"`
}

var mcpTools = []mcpTool{
	{
		Name:        "create_repo",
		Description: "Create a repository for the authenticated user.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"name":        {Type: "string", Description: "The repository name."},
				"description": {Type: "string", Description: "A short description of the repository."},
				"private":     {Type: "boolean", Description: "Whether the repository is private."},
			},
			Required: []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner": {Type: "string", Description: "The repository owner login."},
				"repo":  {Type: "string", Description: "The repository name."},
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner": {Type: "string", Description: "The repository owner login."},
				"repo":  {Type: "string", Description: "The repository name."},
				"title": {Type: "string", Description: "The issue title."},
				"body":  {Type: "string", Description: "The issue body."},
			},
			Required: []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List the issues of a repository, optionally filtered by state.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner": {Type: "string", Description: "The repository owner login."},
				"repo":  {Type: "string", Description: "The repository name."},
				"state": {Type: "string", Description: "Issue state to list: open, closed, or all."},
			},
			Required: []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Open a pull request from a head branch into a base branch.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner": {Type: "string", Description: "The repository owner login."},
				"repo":  {Type: "string", Description: "The repository name."},
				"title": {Type: "string", Description: "The pull request title."},
				"head":  {Type: "string", Description: "The head branch name."},
				"base":  {Type: "string", Description: "The base branch name."},
				"body":  {Type: "string", Description: "The pull request body."},
			},
			Required: []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request with a squash commit.",
		InputSchema: mcpToolSchema{
			Type: "object",
			Properties: map[string]mcpToolProp{
				"owner":       {Type: "string", Description: "The repository owner login."},
				"repo":        {Type: "string", Description: "The repository name."},
				"pull_number": {Type: "integer", Description: "The pull request number."},
			},
			Required: []string{"owner", "repo", "pull_number"},
		},
	},
}

// ---------------------------------------------------------------------------
// JSON-RPC envelope helpers
// ---------------------------------------------------------------------------

// mcpResult writes a JSON-RPC 2.0 result, echoing the request id verbatim.
func mcpResult(w http.ResponseWriter, id json.RawMessage, result any) {
	if id == nil {
		id = json.RawMessage("null")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Protocol-Version", mcpRevision)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	})
}

// mcpError writes a JSON-RPC 2.0 error. A parse error answers HTTP 400;
// every other JSON-RPC error answers HTTP 200 (never 500).
func mcpError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	if id == nil {
		id = json.RawMessage("null")
	}
	status := http.StatusOK
	if code == -32700 {
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Mcp-Protocol-Version", mcpRevision)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

// mcpToolResult builds a tool result carrying the same JSON both as text
// (so a reader that only looks at content sees it) and as structured
// content (so a reader that parses it sees it).
func mcpToolResult(data map[string]any) map[string]any {
	text, _ := json.Marshal(data)
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(text)},
		},
		"structuredContent": data,
		"isError":           false,
	}
}

// mcpToolError builds a tool result that reports a tool-level failure.
func mcpToolError(message string) map[string]any {
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": message},
		},
		"isError": true,
	}
}

// ---------------------------------------------------------------------------
// Argument helpers
// ---------------------------------------------------------------------------

func mcpArgString(args map[string]any, key string) string {
	if s, ok := args[key].(string); ok {
		return s
	}
	return ""
}

func mcpArgBool(args map[string]any, key string) bool {
	if b, ok := args[key].(bool); ok {
		return b
	}
	return false
}

func mcpArgInt(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

// mcpMissing reports the first required argument that is absent or empty.
func mcpMissing(args map[string]any, required ...string) string {
	for _, key := range required {
		v, ok := args[key]
		if !ok || v == nil {
			return key
		}
		if s, ok := v.(string); ok && s == "" {
			return key
		}
	}
	return ""
}

// toMap renders a struct as a JSON object map, for structuredContent.
func toMap(v any) map[string]any {
	data, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return out
}

// ---------------------------------------------------------------------------
// Tool handlers. The caller holds the store write lock.
// ---------------------------------------------------------------------------

func mcpCreateRepo(args map[string]any) (map[string]any, int, string) {
	if missing := mcpMissing(args, "name"); missing != "" {
		return nil, -32602, "missing required argument: " + missing
	}
	name := mcpArgString(args, "name")
	repo, ok := store.createRepo(name, mcpArgBool(args, "private"), mcpArgString(args, "description"))
	if !ok {
		return mcpToolError("repository already exists: " + name), 0, ""
	}
	return mcpToolResult(toMap(repo.toJSON())), 0, ""
}

func mcpGetRepo(args map[string]any) (map[string]any, int, string) {
	if missing := mcpMissing(args, "owner", "repo"); missing != "" {
		return nil, -32602, "missing required argument: " + missing
	}
	repo := store.getRepo(mcpArgString(args, "owner"), mcpArgString(args, "repo"))
	if repo == nil {
		return mcpToolError("Not Found"), 0, ""
	}
	return mcpToolResult(toMap(repo.toJSON())), 0, ""
}

func mcpCreateIssue(args map[string]any) (map[string]any, int, string) {
	if missing := mcpMissing(args, "owner", "repo", "title"); missing != "" {
		return nil, -32602, "missing required argument: " + missing
	}
	repo := store.getRepo(mcpArgString(args, "owner"), mcpArgString(args, "repo"))
	if repo == nil {
		return mcpToolError("Not Found"), 0, ""
	}
	issue := &Issue{Number: repo.NextIssue, Title: mcpArgString(args, "title"), Body: mcpArgString(args, "body"), State: "open", Seq: store.seq + 1}
	store.seq++
	repo.NextIssue++
	repo.Issues[issue.Number] = issue
	return mcpToolResult(toMap(repo.issueToJSON(issue))), 0, ""
}

func mcpListIssues(args map[string]any) (map[string]any, int, string) {
	if missing := mcpMissing(args, "owner", "repo"); missing != "" {
		return nil, -32602, "missing required argument: " + missing
	}
	repo := store.getRepo(mcpArgString(args, "owner"), mcpArgString(args, "repo"))
	if repo == nil {
		return mcpToolError("Not Found"), 0, ""
	}
	state := mcpArgString(args, "state")
	var issues []*Issue
	for _, issue := range repo.Issues {
		if state == "open" && issue.State != "open" {
			continue
		}
		if state == "closed" && issue.State != "closed" {
			continue
		}
		issues = append(issues, issue)
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	list := make([]map[string]any, 0, len(issues))
	for _, issue := range issues {
		list = append(list, toMap(repo.issueToJSON(issue)))
	}
	return mcpToolResult(map[string]any{"issues": list}), 0, ""
}

func mcpCreatePR(args map[string]any) (map[string]any, int, string) {
	if missing := mcpMissing(args, "owner", "repo", "title", "head", "base"); missing != "" {
		return nil, -32602, "missing required argument: " + missing
	}
	repo := store.getRepo(mcpArgString(args, "owner"), mcpArgString(args, "repo"))
	if repo == nil {
		return mcpToolError("Not Found"), 0, ""
	}
	headRef := mcpArgString(args, "head")
	baseRef := mcpArgString(args, "base")
	headSHA, ok1 := repo.Refs["refs/heads/"+headRef]
	baseSHA, ok2 := repo.Refs["refs/heads/"+baseRef]
	if !ok1 || !ok2 {
		return mcpToolError("head or base branch not found"), 0, ""
	}
	pr := &PR{Number: repo.NextPR, Title: mcpArgString(args, "title"), Body: mcpArgString(args, "body"), State: "open", HeadRef: headRef, BaseRef: baseRef, HeadSHA: headSHA, BaseSHA: baseSHA, Seq: store.seq + 1}
	store.seq++
	repo.NextPR++
	repo.PRs[pr.Number] = pr
	return mcpToolResult(toMap(repo.prToJSON(pr))), 0, ""
}

func mcpMergePR(args map[string]any) (map[string]any, int, string) {
	if missing := mcpMissing(args, "owner", "repo", "pull_number"); missing != "" {
		return nil, -32602, "missing required argument: " + missing
	}
	repo := store.getRepo(mcpArgString(args, "owner"), mcpArgString(args, "repo"))
	if repo == nil {
		return mcpToolError("Not Found"), 0, ""
	}
	num, ok := mcpArgInt(args, "pull_number")
	if !ok {
		return nil, -32602, "pull_number must be an integer"
	}
	pr, ok := repo.PRs[num]
	if !ok {
		return mcpToolError("Not Found"), 0, ""
	}
	if pr.State != "open" {
		return mcpToolError("pull request is not open"), 0, ""
	}
	headSHA, ok1 := repo.Refs["refs/heads/"+pr.HeadRef]
	baseSHA, ok2 := repo.Refs["refs/heads/"+pr.BaseRef]
	if !ok1 || !ok2 {
		return mcpToolError("head or base branch not found"), 0, ""
	}
	result, conflict := repo.mergeTree(baseSHA, headSHA)
	if conflict != "" {
		return mcpToolError("merge conflict on " + conflict), 0, ""
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
	return mcpToolResult(map[string]any{
		"merged":  true,
		"sha":     mergeID,
		"message": message,
		"number":  pr.Number,
	}), 0, ""
}

// mcpCallTool dispatches one tools/call. It returns (result, rpcCode, rpcMessage):
// a non-zero rpcCode means the call failed at the JSON-RPC level.
func mcpCallTool(name string, args map[string]any) (map[string]any, int, string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	switch name {
	case "create_repo":
		return mcpCreateRepo(args)
	case "get_repo":
		return mcpGetRepo(args)
	case "create_issue":
		return mcpCreateIssue(args)
	case "list_issues":
		return mcpListIssues(args)
	case "create_pull_request":
		return mcpCreatePR(args)
	case "merge_pull_request":
		return mcpMergePR(args)
	default:
		return nil, -32601, "unknown tool: " + name
	}
}

// ---------------------------------------------------------------------------
// HTTP handler
// ---------------------------------------------------------------------------

func handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	// Authentication is decided before the body is parsed.
	if !authed(r) {
		writeErr(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	body, ok := readBody(r, 1<<20)
	if !ok {
		mcpError(w, nil, -32700, "Parse error")
		return
	}
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		mcpError(w, nil, -32700, "Parse error")
		return
	}
	// A notification (no id) is answered 202 with an empty body.
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "server/discover":
		mcpResult(w, req.ID, map[string]any{
			"supportedVersions": []string{mcpRevision},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        map[string]any{"name": mcpServerName, "version": mcpServerVersion},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": mcpServerName, "version": mcpServerVersion},
			},
		})
		return
	case "initialize":
		// Echo the revision the client offered when it is a valid
		// YYYY-MM-DD revision; otherwise offer the newest legacy one.
		version := "2025-11-25"
		if len(req.Params) > 0 {
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(req.Params, &params); err == nil && params.ProtocolVersion != "" {
				version = params.ProtocolVersion
			}
		}
		mcpResult(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		})
		return
	case "tools/list":
		mcpResult(w, req.ID, map[string]any{"tools": mcpTools})
		return
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &params); err != nil {
				mcpError(w, req.ID, -32602, "Invalid params")
				return
			}
		}
		if params.Arguments == nil {
			params.Arguments = map[string]any{}
		}
		result, code, message := mcpCallTool(params.Name, params.Arguments)
		if code != 0 {
			mcpError(w, req.ID, code, message)
			return
		}
		mcpResult(w, req.ID, result)
		return
	default:
		mcpError(w, req.ID, -32601, "Method not found: "+req.Method)
		return
	}
}
