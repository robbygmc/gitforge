// The forge as a compiled static binary.
//
// The grading harness owns a small "env" service that drives the trial.
// This process has one job only: start the forge on a port the kernel
// picks, tell the env which port that is, and stay alive until the env
// says the trial is finished. Lifecycle (mirrors main.ts):
//   1. MAKEFUN_ENV_URL points at the env. Without it there is nobody to
//      register with, so the process gives up at once with exit code 1.
//   2. The server binds port 0, so the kernel chooses a free ephemeral
//      port. Port 8080 is off limits — the env listens there.
//   3. The process POSTs {"port": N} to $MAKEFUN_ENV_URL/register. A 200
//      means registered; a 409 means another entry registered first, which
//      is fine — this process just waits like any other. A 400 or 405 is a
//      contract violation and ends the process; anything else (including
//      the env still booting) is retried with backoff.
//   4. The process polls GET $MAKEFUN_ENV_URL/done until it answers
//      {"done": true}. It never exits early.
//   5. On done, the server is closed and the process exits 0.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const requestTimeout = 30 * time.Second
const backoffStartMs = 250
const backoffCapMs = 2000

func backoffMs(attempt int) time.Duration {
	n := attempt
	if n > 8 {
		n = 8
	}
	ms := backoffStartMs * (1 << uint(n))
	if ms > backoffCapMs {
		ms = backoffCapMs
	}
	return time.Duration(ms) * time.Millisecond
}

type app struct {
	store  *Store
	client *http.Client
	envURL string
}

func (a *app) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Read the body whole first, like createCtx does.
	body := readBody(r)
	if body == nil {
		// 413: no trailing newline, no request-id header (matches http.ts).
		w.Header().Set("content-type", jsonType)
		w.WriteHeader(413)
		_, _ = w.Write([]byte(`{"message":"Request body too large"}`))
		return
	}
	path, query := splitRequestURL(r.URL.RequestURI())
	ctx := &Ctx{
		Store:     a.store,
		Token:     shopToken(),
		Method:    r.Method,
		Path:      path,
		Query:     query,
		Body:      body,
		RequestID: nextRequestID(),
		Host:      r.Host,
		AuthHdr:   r.Header.Get("Authorization"),
		ProtoHdr:  r.Header.Get("Git-Protocol"),
	}
	// One store lock per request: the store is not built for concurrent
	// mutation, and the lock keeps every surface consistent. CPU-heavy
	// pack work happens inside the handlers, but the lock is only held
	// while computing a response, never during I/O.
	// Read-only requests (GET/HEAD, and git-upload-pack fetches, which are
	// POST but never mutate) take a read lock so concurrent clones/fetches
	// run in parallel. Everything else takes the write lock. The parsed-
	// object caches have their own mutex (store.cacheMu); isoOfSeq has its
	// own too, so read-locked handlers never write a shared map.
	readOnly := r.Method == "GET" || r.Method == "HEAD" ||
		(r.Method == "POST" && strings.HasSuffix(path, "/git-upload-pack"))
	if readOnly {
		a.store.mu.RLock()
		defer a.store.mu.RUnlock()
	} else {
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
	}
	defer func() {
		if err := recover(); err != nil {
			// Never let one request crash the server: JSON 500 instead.
			w.Header().Set("content-type", jsonType)
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"message":"Internal Server Error"}`))
			fmt.Fprintf(os.Stderr, "gitforge: request failed: %v\n", err)
		}
	}()
	// The env's boot check calls this twice: 200 {"ok":true}. No auth here.
	if ctx.Method == "GET" && ctx.Path == "/healthz" {
		ctx.sendJSON(w, 200, map[string]any{"ok": true}, nil)
		return
	}
	if handleGit(ctx, w) {
		return
	}
	if handleRest(ctx, w) {
		return
	}
	if handleGraphQL(ctx, w) {
		return
	}
	if handleMcp(ctx, w) {
		return
	}
	// Everything else, every method: GitHub's 404 body.
	ctx.sendError(w, 404, "Not Found")
}

// register posts the bound port until the env accepts it.
func (a *app) register(port int) error {
	for attempt := 0; ; attempt++ {
		done, fatal := a.tryRegister(port)
		if fatal != nil {
			return fatal
		}
		if done {
			return nil
		}
		time.Sleep(backoffMs(attempt))
	}
}

func (a *app) tryRegister(port int) (bool, error) {
	payload, _ := json.Marshal(map[string]any{"port": port})
	req, err := http.NewRequest("POST", a.envURL+"/register", bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("content-type", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return false, nil
	}
	defer res.Body.Close()
	bodyBytes, _ := io.ReadAll(res.Body)
	switch res.StatusCode {
	case 200, 409:
		return true, nil
	case 400, 405:
		return false, fmt.Errorf("register answered HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(bodyBytes)))
	default:
		return false, nil
	}
}

// waitForDone polls GET /done until it answers {"done":true}.
func (a *app) waitForDone() {
	for attempt := 0; ; attempt++ {
		if a.tryDone() {
			return
		}
		time.Sleep(backoffMs(attempt))
	}
}

func (a *app) tryDone() bool {
	req, err := http.NewRequest("GET", a.envURL+"/done", nil)
	if err != nil {
		return false
	}
	res, err := a.client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, res.Body)
		return false
	}
	var body struct {
		Done bool `json:"done"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return false
	}
	return body.Done
}

func main() {
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "[gitforge] MAKEFUN_ENV_URL is not set, nothing to register with")
		os.Exit(1)
	}
	a := &app{
		store:  NewStore(),
		client: &http.Client{Timeout: requestTimeout},
		envURL: envURL,
	}
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[gitforge] %v\n", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	fmt.Printf("[gitforge] listening on port %d\n", port)
	server := &http.Server{Handler: a}
	go func() {
		_ = server.Serve(listener)
	}()
	if err := a.register(port); err != nil {
		fmt.Fprintf(os.Stderr, "[gitforge] %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "[gitforge] registered; waiting for the env to say done")
	a.waitForDone()
	_ = server.Close()
	fmt.Fprintln(os.Stdout, "[gitforge] done")
	os.Exit(0)
}
