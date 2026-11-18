// Command gitforge is a single-process GitHub-compatible git forge.
//
// It serves git smart HTTP, the REST v3 API, GraphQL v4 and MCP from an
// in-memory store, and speaks the make.fun entry <-> env lifecycle: bind an
// ephemeral port, register it with the env, poll /done, exit 0.
//
// Run it with MAKEFUN_ENV_URL set for a trial. Without it, it serves
// standalone (FORGE_PORT, default ephemeral) until killed — the local
// practice mode the kit's sdk/run.ts drives.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// viewerLogin is the login the bearer token names. It is fixed by the
// competition's scenario seed (SCENARIO_SEED 0x6a09e667f3bcc909 in the kit's
// wire.ts, verified by running the kit's own buildScenario): the suite's
// checks address every repository under this login, and the same name under
// any other login is a 404, never a 403.
const viewerLogin = "adaiversen"

// viewerID is the viewer's database id (scenario viewer id 1000).
const viewerID = 1000

// fixedCommitTime is the Unix timestamp every commit the forge itself
// creates carries, so a live run and its replay mint byte-identical objects.
const fixedCommitTime = 1700000000

// forgeAgent is the agent string the forge advertises on receive-pack.
const forgeAgent = "gitforge/1.0"

// commitAuthor is the ident line for forge-created commits.
const commitAuthor = "adaiversen <adaiversen@example.invalid>"

// fixedTime is the ISO timestamp forge-created resources carry (merged_at
// and friends), so a live run and its replay agree byte for byte.
const fixedTime = "2023-11-14T22:13:20Z"

var (
	// expectedToken is the bearer token the forge accepts (MAKEFUN_SHOP_TOKEN).
	expectedToken = os.Getenv("MAKEFUN_SHOP_TOKEN")
	// envURL is the env origin to register with (MAKEFUN_ENV_URL).
	envURL = strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	// reqSeq numbers X-GitHub-Request-Id values; a counter is deterministic
	// for a fixed request sequence, so live and replay agree.
	reqSeq atomic.Uint64
	// store is the forge's whole state.
	store = newStore()
)

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func sleep(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }

// retryDelay is the wait after `attempt` failed register/done requests:
// exponential from 250ms, capped at 2s.
func retryDelay(attempt int) int {
	d := 250 << uint(attempt)
	if d > 2000 {
		d = 2000
	}
	return d
}

// register POSTs the bound port to the env until it accepts it. 200 is the
// normal answer; 409 means a first register already won, which is not this
// process's to fix. 400/405 are contract errors and end the program.
func register(port int) error {
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		body := bytes.NewReader([]byte(`{"port":` + strconv.Itoa(port) + `}`))
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, envURL+"/register", body)
		if err != nil {
			return err
		}
		req.Header.Set("content-type", "application/json")
		res, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 409 {
				return nil
			}
			if res.StatusCode == 400 || res.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", res.StatusCode)
			}
		}
		sleep(retryDelay(attempt))
	}
}

// waitForDone polls GET /done until it answers {"done":true}; it never
// returns before that.
func waitForDone() {
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		res, err := client.Get(envURL + "/done")
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			if json.NewDecoder(res.Body).Decode(&body) == nil && body.Done {
				res.Body.Close()
				return
			}
			res.Body.Close()
		}
		sleep(retryDelay(attempt))
	}
}

func listenAddr() string {
	if p := os.Getenv("FORGE_PORT"); p != "" {
		return ":" + p
	}
	return ":0"
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "[gitforge] %v\n", err)
	os.Exit(1)
}

func main() {
	ln, err := net.Listen("tcp", listenAddr())
	if err != nil {
		fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	fmt.Fprintf(os.Stdout, "[gitforge] listening on port %d\n", port)
	srv := &http.Server{Handler: http.HandlerFunc(dispatch)}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[gitforge] serve: %v\n", err)
		}
	}()
	if envURL == "" {
		// Standalone (local practice) mode: serve until killed.
		fmt.Fprintln(os.Stdout, "[gitforge] standalone mode (no MAKEFUN_ENV_URL); serving until killed")
		waitForSignal()
		srv.Close()
		return
	}
	if err := register(port); err != nil {
		fatal(err)
	}
	fmt.Fprintln(os.Stdout, "[gitforge] registered; waiting for the env to say done")
	waitForDone()
	srv.Close()
	fmt.Fprintln(os.Stdout, "[gitforge] done")
}

// ---------------------------------------------------------------------------
// HTTP helpers
// ---------------------------------------------------------------------------

// requestID returns the next X-GitHub-Request-Id value.
func requestID() string {
	return fmt.Sprintf("%016x", reqSeq.Add(1))
}

// writeJSON answers with a JSON body, always carrying a non-empty
// X-GitHub-Request-Id. Extra headers go beside the JSON ones.
func writeJSON(w http.ResponseWriter, status int, v any, extra map[string]string) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		w.Header().Set("content-type", "application/json")
		w.Header().Set("x-github-request-id", requestID())
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"Internal Server Error"}`))
		return
	}
	data := bytes.TrimRight(buf.Bytes(), "\n")
	h := w.Header()
	h.Set("content-type", "application/json")
	h.Set("x-github-request-id", requestID())
	for k, v := range extra {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(data)
}

// writeErr answers with GitHub's {"message": ...} error body.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"message": msg}, nil)
}

// readBody reads the whole request body, capped at limit bytes.
// It reports false when the read fails or the body exceeds the cap.
func readBody(r *http.Request, limit int64) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, false
	}
	return data, true
}

// readJSON decodes a JSON request body into v. It reports false (and the
// caller answers 400) when the body is not valid JSON for v.
func readJSON(r *http.Request, v any) bool {
	data, ok := readBody(r, 64<<20)
	if !ok {
		return false
	}
	if len(data) == 0 {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

// authed reports whether the request carries the bearer token. When the
// forge was started with MAKEFUN_SHOP_TOKEN set, the token must match it;
// otherwise any non-empty bearer token is accepted. No token is never
// accepted.
func authed(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	tok := strings.TrimPrefix(h, "Bearer ")
	if tok == "" {
		return false
	}
	if expectedToken != "" {
		return tok == expectedToken
	}
	return true
}

// segments splits a URL path into its non-empty segments.
func segments(path string) []string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isGitPath reports whether path is one of the three git smart HTTP routes.
func isGitPath(path string) bool {
	return strings.HasSuffix(path, ".git/info/refs") ||
		strings.HasSuffix(path, ".git/git-upload-pack") ||
		strings.HasSuffix(path, ".git/git-receive-pack")
}

// dispatch routes one request. /healthz answers without a token (the boot
// probe); every other route needs the bearer token and answers 401 without
// it; unknown routes answer GitHub's 404 JSON body.
func dispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodGet && path == "/healthz" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true}, nil)
		return
	}
	if !authed(r) {
		writeErr(w, http.StatusUnauthorized, "Bad credentials")
		return
	}
	if isGitPath(path) {
		handleGit(w, r)
		return
	}
	if path == "/api/graphql" {
		handleGraphQL(w, r)
		return
	}
	if path == "/mcp" {
		handleMCP(w, r)
		return
	}
	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		handleREST(w, r)
		return
	}
	writeErr(w, http.StatusNotFound, "Not Found")
}
