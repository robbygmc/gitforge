// HTTP plumbing shared by every surface: the request context, body
// reading, JSON answers, the bearer-token check and the request-id header
// the protocol checks require on every reply. Mirrors src/http.ts.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

const jsonType = "application/json; charset=utf-8"

// maxBodyBytes is the largest request body accepted (packs are far below this).
const maxBodyBytes = 256 * 1024 * 1024

var requestCounter int64

// nextRequestID returns the next request id: deterministic, non-empty,
// unique per request.
func nextRequestID() string {
	n := atomic.AddInt64(&requestCounter, 1)
	return "gitforge-" + itoa64(n)
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

var shopTokenOnce sync.Once
var shopTokenValue string

// shopToken returns the bearer token the env set, read once.
func shopToken() string {
	shopTokenOnce.Do(func() {
		shopTokenValue = os.Getenv("MAKEFUN_SHOP_TOKEN")
	})
	return shopTokenValue
}

// Ctx is the per-request context.
type Ctx struct {
	Store     *Store
	Token     string
	Method    string
	Path      string
	Query     url.Values
	Body      []byte
	RequestID string
	Host      string
	AuthHdr   string
	ProtoHdr  string // git-protocol header
}

// isAuthorized reports whether the request carries the expected bearer token.
func (c *Ctx) isAuthorized() bool {
	header := c.AuthHdr
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	presented := strings.TrimSpace(header[len("Bearer "):])
	if presented == "" {
		return false
	}
	if c.Token != "" {
		return presented == c.Token
	}
	return true
}

// marshalJSON renders a value as JSON the way JSON.stringify does: no HTML
// escaping, no trailing newline (the caller adds it for sendJson).
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

// sendJSON answers with a JSON body, always carrying the request-id header.
func (c *Ctx) sendJSON(w http.ResponseWriter, status int, body any, extraHeaders map[string]string) {
	text := append(marshalJSON(body), '\n')
	h := w.Header()
	h.Set("content-type", jsonType)
	h.Set("content-length", itoa(len(text)))
	h.Set("x-github-request-id", c.RequestID)
	for k, v := range extraHeaders {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(text)
}

func itoa(n int) string { return itoa64(int64(n)) }

// sendJSONRaw answers with pre-rendered JSON bytes plus the trailing
// newline sendJson appends, always carrying the request-id header.
func (c *Ctx) sendJSONRaw(w http.ResponseWriter, status int, body []byte, extraHeaders map[string]string) {
	text := append(append([]byte{}, body...), '\n')
	h := w.Header()
	h.Set("content-type", jsonType)
	h.Set("content-length", itoa(len(text)))
	h.Set("x-github-request-id", c.RequestID)
	for k, v := range extraHeaders {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(text)
}

// sendBytes answers with a raw buffer and an explicit content type.
func (c *Ctx) sendBytes(w http.ResponseWriter, status int, body []byte, contentType string, extraHeaders map[string]string) {
	h := w.Header()
	h.Set("content-type", contentType)
	h.Set("content-length", itoa(len(body)))
	h.Set("x-github-request-id", c.RequestID)
	for k, v := range extraHeaders {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// sendError answers with a JSON error body: {"message": ...}.
func (c *Ctx) sendError(w http.ResponseWriter, status int, message string) {
	c.sendJSON(w, status, map[string]any{"message": message}, nil)
}

// sendNoContent answers with a 204 with no body (or a 202 acknowledgement).
func (c *Ctx) sendNoContent(w http.ResponseWriter, status int) {
	h := w.Header()
	h.Set("x-github-request-id", c.RequestID)
	w.WriteHeader(status)
}

// parseJSONBody parses the request body as JSON. Returns (value, ok, empty):
// ok=false, empty=true when the body is empty; ok=false, empty=false when malformed.
func (c *Ctx) parseJSONBody() (any, bool, bool) {
	if len(c.Body) == 0 {
		return nil, false, true
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(c.Body))
	if err := dec.Decode(&v); err != nil {
		return nil, false, false
	}
	return v, true, false
}

func isRecord(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// readBody reads a request body whole, or returns nil when it exceeds the cap.
func readBody(r *http.Request) []byte {
	if (r.Method == "GET" || r.Method == "HEAD") && r.Header.Get("Transfer-Encoding") == "" {
		if r.ContentLength == 0 {
			return []byte{}
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil {
		return nil
	}
	if len(body) > maxBodyBytes {
		return nil
	}
	return body
}

// splitRequestURL splits a request URL into pathname and query, following
// the TS parseRequestUrl rules: the pathname ends at the first '?' or '#',
// percent-escapes stay as written.
func splitRequestURL(raw string) (string, url.Values) {
	pathEnd := len(raw)
	q := strings.IndexByte(raw, '?')
	h := strings.IndexByte(raw, '#')
	if q >= 0 && q < pathEnd {
		pathEnd = q
	}
	if h >= 0 && h < pathEnd {
		pathEnd = h
	}
	rawPath := raw[:pathEnd]
	queryString := ""
	if q >= 0 && (h < 0 || q < h) {
		if h < 0 {
			queryString = raw[q+1:]
		} else {
			queryString = raw[q+1 : h]
		}
	}
	var path string
	if rawPath == "" || !strings.HasPrefix(rawPath, "/") || strings.HasPrefix(rawPath, "//") ||
		strings.Contains(rawPath, "/./") || strings.Contains(rawPath, "/../") ||
		strings.HasSuffix(rawPath, "/.") || strings.HasSuffix(rawPath, "/..") {
		u, err := url.ParseRequestURI(raw)
		if err != nil {
			path = "/"
		} else {
			path = u.Path
		}
	} else {
		path = rawPath
	}
	var query url.Values
	if queryString == "" {
		query = url.Values{}
	} else {
		query, _ = url.ParseQuery(queryString)
		if query == nil {
			query = url.Values{}
		}
	}
	return path, query
}

// decodeSegment decodes one path segment the way decodeURIComponent does.
func decodeSegment(s string) string {
	dec, err := url.PathUnescape(s)
	if err != nil {
		return s
	}
	return dec
}

// splitSegments splits a path into decoded segments, dropping empties.
func splitSegments(path string) []string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, decodeSegment(p))
		}
	}
	return out
}
