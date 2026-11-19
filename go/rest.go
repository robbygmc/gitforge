// GitHub REST v3: repositories, branches, commits, contents, labels,
// issues, comments and pull requests, in the shapes the suite's
// checks-rest.ts asserts. Mirrors src/rest.ts.
package main

import (
	"encoding/base64"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- rendering

func userJSON() map[string]any {
	return map[string]any{"login": ViewerLogin, "id": ViewerID, "type": ViewerType, "avatar_url": "", "html_url": ""}
}

func repoJSON(s *Store, repo *Repo) map[string]any {
	created := isoOfSeq(repo.CreatedSeq)
	return map[string]any{
		"id":           repo.ID,
		"name":         repo.Name,
		"full_name":    repo.Owner + "/" + repo.Name,
		"owner":        userJSON(),
		"private":      repo.Private,
		"description":  repo.Description,
		"default_branch": repo.DefaultBranch,
		"html_url":     "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		"url":          "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
		"created_at":   created,
		"updated_at":   created,
		"pushed_at":    created,
		"has_issues":   true,
		"has_projects": true,
		"has_wiki":     true,
		"fork":         false,
		"archived":     false,
		"disabled":     false,
	}
}

func labelJSON(label Label) map[string]any {
	return map[string]any{"id": label.Name, "name": label.Name, "color": label.Color, "description": label.Description, "default": false}
}

func issueJSON(s *Store, repo *Repo, issue *Issue) map[string]any {
	created := isoOfSeq(issue.CreatedSeq)
	updated := isoOfSeq(issue.UpdatedSeq)
	labels := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		found := false
		for _, entry := range repo.Labels {
			if entry.Name == name {
				labels = append(labels, labelJSON(entry))
				found = true
				break
			}
		}
		if !found {
			labels = append(labels, map[string]any{"name": name, "color": "ededed", "description": ""})
		}
	}
	closedAt := any(nil)
	if issue.State == "closed" {
		closedAt = updated
	}
	out := map[string]any{
		"id":         issue.ID,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       userJSON(),
		"assignees":  []any{},
		"created_at": created,
		"updated_at": updated,
		"closed_at":  closedAt,
		"comments":   len(issue.Comments),
		"html_url":   "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
		"url":        "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number),
	}
	return out
}

// issueJSONBytes renders an issue as JSON bytes, cached on the issue until
// it mutates (the read workload asks for the same issue thousands of times).
func issueJSONBytes(s *Store, repo *Repo, issue *Issue) []byte {
	if issue.cachedJSON != nil {
		return issue.cachedJSON
	}
	b := marshalJSON(issueJSON(s, repo, issue))
	issue.cachedJSON = b
	return b
}

func invalidateIssue(issue *Issue) {
	issue.Version++
	issue.cachedJSON = nil
}

func invalidateRepoIssues(repo *Repo) {
	for _, issue := range repo.Issues {
		issue.cachedJSON = nil
	}
}

func pullJSON(s *Store, repo *Repo, pull *Pull) map[string]any {
	created := isoOfSeq(pull.CreatedSeq)
	updated := isoOfSeq(pull.UpdatedSeq)
	headTip, ok := s.GetRef(repo, s.RefName(pull.HeadRef))
	if !ok {
		headTip = pull.HeadSHA
	}
	baseTip, ok := s.GetRef(repo, s.RefName(pull.BaseRef))
	if !ok {
		baseTip = pull.BaseSHA
	}
	closedAt := any(nil)
	if pull.State == "closed" {
		closedAt = updated
	}
	return map[string]any{
		"id":     pull.ID,
		"number": pull.Number,
		"state":  pull.State,
		"title":  pull.Title,
		"body":   pull.Body,
		"head":   map[string]any{"ref": pull.HeadRef, "sha": headTip, "label": repo.Owner + ":" + pull.HeadRef},
		"base":   map[string]any{"ref": pull.BaseRef, "sha": baseTip, "label": repo.Owner + ":" + pull.BaseRef},
		"merged":          pull.Merged,
		"merged_at":       nullableString(pull.MergedAt),
		"merge_commit_sha": nullableString(pull.MergeCommitSHA),
		"mergeable":       pullMergeable(s, repo, pull),
		"user":            userJSON(),
		"created_at":      created,
		"updated_at":      updated,
		"closed_at":       closedAt,
		"html_url":        "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + itoa(pull.Number),
		"url":             "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + itoa(pull.Number),
	}
}

func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// pullMergeable reports whether a pull request merges cleanly right now
// (nil when it cannot be told).
func pullMergeable(s *Store, repo *Repo, pull *Pull) any {
	if pull.State != "open" {
		return false
	}
	baseTip, baseOk := s.GetRef(repo, s.RefName(pull.BaseRef))
	headTip, headOk := s.GetRef(repo, s.RefName(pull.HeadRef))
	if !baseOk || !headOk {
		return nil
	}
	if baseTip == headTip {
		return true
	}
	return s.MergeTrees(repo, baseTip, headTip) != nil
}

func commentJSON(comment *Comment) map[string]any {
	created := isoOfSeq(comment.CreatedSeq)
	id := 0
	if comment.Author == ViewerLogin {
		id = ViewerID
	}
	return map[string]any{
		"id":   comment.ID,
		"body": comment.Body,
		"user": map[string]any{"login": comment.Author, "id": id, "type": "User"},
		"created_at": created,
		"updated_at": created,
	}
}

// ---------------------------------------------------------------- pagination

type pageParams struct {
	page    float64
	perPage float64
}

func getPageParams(ctx *Ctx) pageParams {
	page := math.Max(1, jsNumberOr(ctx.Query.Get("page"), 1))
	perPageRaw := jsNumberOr(ctx.Query.Get("per_page"), 30)
	perPage := math.Min(100, math.Max(1, perPageRaw))
	return pageParams{page: page, perPage: perPage}
}

// jsNumberOr mimics JS Number(x) || fallback for a query value that may be absent.
func jsNumberOr(raw string, fallback float64) float64 {
	if raw == "" {
		return fallback
	}
	n, ok := jsNumber(raw)
	if !ok || n == 0 || math.IsNaN(n) {
		return fallback
	}
	return n
}

// jsNumber mimics JS Number(s): "" -> 0, otherwise ParseFloat semantics.
func jsNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return math.NaN(), false
	}
	return n, true
}

// linkHeader renders the Link header for a paginated list (absolute URLs on this origin).
func linkHeader(ctx *Ctx, page, perPage float64, total int) string {
	pages := math.Max(1, math.Ceil(float64(total)/perPage))
	if pages <= 1 {
		return ""
	}
	urlFor := func(pageNumber float64) string {
		// Rebuild the request URL with page/per_page set (URLSearchParams
		// semantics: existing params kept, page/per_page overwritten).
		q := url.Values{}
		for k, vs := range ctx.Query {
			q[k] = append([]string{}, vs...)
		}
		q.Set("page", formatFloat(pageNumber))
		q.Set("per_page", formatFloat(perPage))
		return "http://" + ctx.Host + ctx.Path + "?" + q.Encode()
	}
	var parts []string
	if page < pages {
		parts = append(parts, "<"+urlFor(page+1)+">; rel=\"next\"")
	}
	if page > 1 {
		parts = append(parts, "<"+urlFor(page-1)+">; rel=\"prev\"")
	}
	parts = append(parts, "<"+urlFor(1)+">; rel=\"first\"")
	parts = append(parts, "<"+urlFor(pages)+">; rel=\"last\"")
	return strings.Join(parts, ", ")
}

func formatFloat(f float64) string {
	if f == math.Trunc(f) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func pageSlice[T any](items []T, params pageParams) []T {
	start := int((params.page - 1) * params.perPage)
	if start < 0 {
		start = 0
	}
	if start > len(items) {
		start = len(items)
	}
	end := start + int(params.perPage)
	if end > len(items) {
		end = len(items)
	}
	return items[start:end]
}

// ---------------------------------------------------------------- line diff

// lineDiff returns added/removed line counts between two text blobs (multiset line diff).
func lineDiff(oldText, newText *string) (additions, deletions int) {
	linesOf := func(text *string) []string {
		if text == nil {
			return []string{}
		}
		lines := strings.Split(*text, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	oldLines := linesOf(oldText)
	newLines := linesOf(newText)
	oldCount := map[string]int{}
	for _, line := range oldLines {
		oldCount[line]++
	}
	for _, line := range newLines {
		if left := oldCount[line]; left > 0 {
			oldCount[line] = left - 1
		} else {
			additions++
		}
	}
	for _, left := range oldCount {
		deletions += left
	}
	return additions, deletions
}

// ---------------------------------------------------------------- routing

// handleRest handles a REST v3 request. Returns true when the path was under /api/v3.
func handleRest(ctx *Ctx, w http.ResponseWriter) bool {
	if ctx.Path != "/api/v3" && !strings.HasPrefix(ctx.Path, "/api/v3/") {
		return false
	}
	if !ctx.isAuthorized() {
		ctx.sendError(w, 401, "Requires authentication")
		return true
	}
	segments := splitSegments(ctx.Path)[2:]
	dispatchRest(ctx, w, segments)
	return true
}

func dispatchRest(ctx *Ctx, w http.ResponseWriter, segments []string) {
	method := ctx.Method
	if len(segments) == 0 {
		if method == "GET" {
			ctx.sendJSON(w, 200, map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"}, map[string]string{"x-oauth-scopes": "repo, read:org"})
		} else {
			ctx.sendError(w, 404, "Not Found")
		}
		return
	}
	if segments[0] == "meta" && len(segments) == 1 {
		if method == "GET" {
			ctx.sendJSON(w, 200, map[string]any{"installed_version": "1.0.0"}, nil)
		} else {
			ctx.sendError(w, 404, "Not Found")
		}
		return
	}
	if segments[0] == "user" && len(segments) == 1 {
		if method == "GET" {
			u := userJSON()
			u["name"] = "ada iversen"
			u["email"] = "adaiversen@example.invalid"
			ctx.sendJSON(w, 200, u, nil)
		} else {
			ctx.sendError(w, 404, "Not Found")
		}
		return
	}
	if segments[0] == "user" && len(segments) >= 2 && segments[1] == "repos" {
		if method == "POST" {
			createRepoHandler(ctx, w)
			return
		}
		if method == "GET" {
			listUserRepos(ctx, w)
			return
		}
		ctx.sendError(w, 404, "Not Found")
		return
	}
	if segments[0] == "repos" && len(segments) >= 3 {
		repoRoutes(ctx, w, segments[1:])
		return
	}
	ctx.sendError(w, 404, "Not Found")
}

type bodyResult struct {
	ok      bool
	value   map[string]any
	status  int
	message string
}

func readBodyJSON(ctx *Ctx) bodyResult {
	parsed, ok, empty := ctx.parseJSONBody()
	if !ok {
		if empty {
			return bodyResult{ok: true, value: map[string]any{}}
		}
		return bodyResult{ok: false, status: 400, message: "Invalid JSON in request body"}
	}
	if !isRecord(parsed) {
		return bodyResult{ok: false, status: 400, message: "Request body must be a JSON object"}
	}
	return bodyResult{ok: true, value: parsed.(map[string]any)}
}

func createRepoHandler(ctx *Ctx, w http.ResponseWriter) {
	body := readBodyJSON(ctx)
	if !body.ok {
		ctx.sendError(w, body.status, body.message)
		return
	}
	name := ""
	if v, ok := body.value["name"].(string); ok {
		name = strings.TrimSpace(v)
	}
	if name == "" || nameHasSpaceOrSlash(name) {
		ctx.sendError(w, 422, "Validation Failed: name is required")
		return
	}
	private := body.value["private"] == true
	description := ""
	if v, ok := body.value["description"].(string); ok {
		description = v
	}
	repo := ctx.Store.CreateRepo(ViewerLogin, name, private, description)
	if repo == nil {
		ctx.sendError(w, 422, "Validation Failed: Repository "+name+" already exists")
		return
	}
	ctx.sendJSON(w, 201, repoJSON(ctx.Store, repo), nil)
}

func listUserRepos(ctx *Ctx, w http.ResponseWriter) {
	params := getPageParams(ctx)
	repos := ctx.Store.ListRepos(ViewerLogin)
	link := linkHeader(ctx, params.page, params.perPage, len(repos))
	var headers map[string]string
	if link != "" {
		headers = map[string]string{"link": link}
	}
	out := make([]any, 0, len(pageSlice(repos, params)))
	for _, repo := range pageSlice(repos, params) {
		out = append(out, repoJSON(ctx.Store, repo))
	}
	ctx.sendJSON(w, 200, out, headers)
}

// ownedRepo resolves owner/name to a repository the token owns; nil otherwise (never 403).
func ownedRepo(ctx *Ctx, owner, name string) *Repo {
	if !strings.EqualFold(owner, ViewerLogin) {
		return nil
	}
	return ctx.Store.GetRepo(owner, name)
}

func repoRoutes(ctx *Ctx, w http.ResponseWriter, segments []string) {
	owner, name := segments[0], segments[1]
	rest := segments[2:]
	repo := ownedRepo(ctx, owner, name)
	if repo == nil {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	store := ctx.Store
	method := ctx.Method
	if len(rest) == 0 {
		if method == "GET" {
			ctx.sendJSON(w, 200, repoJSON(store, repo), nil)
		} else if method == "PATCH" {
			body := readBodyJSON(ctx)
			if !body.ok {
				ctx.sendError(w, body.status, body.message)
				return
			}
			if v, ok := body.value["description"].(string); ok {
				repo.Description = v
			}
			if v, ok := body.value["private"].(bool); ok {
				repo.Private = v
			}
			ctx.sendJSON(w, 200, repoJSON(store, repo), nil)
		} else if method == "DELETE" {
			store.DeleteRepo(owner, name)
			ctx.sendNoContent(w, 204)
		} else {
			ctx.sendError(w, 404, "Not Found")
		}
		return
	}
	head := rest[0]
	tail := rest[1:]
	switch head {
	case "branches":
		if len(tail) == 0 && method == "GET" {
			var out []any
			for _, ref := range store.ListRefs(repo) {
				if strings.HasPrefix(ref.Name, "refs/heads/") {
					out = append(out, map[string]any{"name": ref.Name[len("refs/heads/"):], "commit": map[string]any{"sha": ref.OID}})
				}
			}
			if out == nil {
				out = []any{}
			}
			ctx.sendJSON(w, 200, out, nil)
			return
		}
		if len(tail) == 1 && method == "GET" {
			oid, ok := store.GetRef(repo, store.RefName(tail[0]))
			if !ok {
				ctx.sendError(w, 404, "Not Found")
				return
			}
			ctx.sendJSON(w, 200, map[string]any{"name": tail[0], "commit": map[string]any{"sha": oid}}, nil)
			return
		}
		ctx.sendError(w, 404, "Not Found")
		return
	case "commits":
		if method == "GET" && len(tail) == 0 {
			listCommits(ctx, w, repo)
			return
		}
		ctx.sendError(w, 404, "Not Found")
		return
	case "git":
		if len(tail) > 0 && tail[0] == "refs" {
			if method == "POST" && len(tail) == 1 {
				createRef(ctx, w, repo)
				return
			}
			if method == "GET" {
				getRef(ctx, w, repo, tail[1:])
				return
			}
		}
	case "contents":
		if method == "PUT" {
			putContents(ctx, w, repo, tail)
			return
		}
		if method == "GET" {
			getContents(ctx, w, repo, tail)
			return
		}
		if method == "DELETE" {
			deleteContents(ctx, w, repo, tail)
			return
		}
		ctx.sendError(w, 404, "Not Found")
		return
	case "labels":
		if method == "POST" {
			createLabel(ctx, w, repo)
			return
		}
		if method == "GET" {
			out := make([]any, 0, len(repo.Labels))
			for _, label := range repo.Labels {
				out = append(out, labelJSON(label))
			}
			ctx.sendJSON(w, 200, out, nil)
			return
		}
		ctx.sendError(w, 404, "Not Found")
		return
	case "issues":
		issuesRoutes(ctx, w, repo, tail)
		return
	case "pulls":
		pullsRoutes(ctx, w, repo, tail)
		return
	}
	ctx.sendError(w, 404, "Not Found")
}

func issuesRoutes(ctx *Ctx, w http.ResponseWriter, repo *Repo, tail []string) {
	method := ctx.Method
	if len(tail) == 0 {
		if method == "POST" {
			createIssue(ctx, w, repo)
			return
		}
		if method == "GET" {
			listIssues(ctx, w, repo)
			return
		}
		ctx.sendError(w, 404, "Not Found")
		return
	}
	number, ok := jsIntNumber(tail[0])
	if !ok || number < 1 {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	if len(tail) == 1 {
		if method == "GET" {
			getIssue(ctx, w, repo, number)
			return
		}
		if method == "PATCH" {
			patchIssue(ctx, w, repo, number)
			return
		}
		ctx.sendError(w, 404, "Not Found")
		return
	}
	if tail[1] == "comments" {
		if method == "POST" {
			createComment(ctx, w, repo, number)
			return
		}
		if method == "GET" {
			listComments(ctx, w, repo, number)
			return
		}
		ctx.sendError(w, 404, "Not Found")
		return
	}
	ctx.sendError(w, 404, "Not Found")
}

func pullsRoutes(ctx *Ctx, w http.ResponseWriter, repo *Repo, tail []string) {
	method := ctx.Method
	if len(tail) == 0 {
		if method == "POST" {
			createPull(ctx, w, repo)
			return
		}
		if method == "GET" {
			listPulls(ctx, w, repo)
			return
		}
		ctx.sendError(w, 404, "Not Found")
		return
	}
	number, ok := jsIntNumber(tail[0])
	if !ok || number < 1 {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	if len(tail) == 1 {
		if method == "GET" {
			getPull(ctx, w, repo, number)
			return
		}
		ctx.sendError(w, 404, "Not Found")
		return
	}
	if tail[1] == "files" && method == "GET" {
		listPullFiles(ctx, w, repo, number)
		return
	}
	if tail[1] == "merge" && method == "PUT" {
		mergePull(ctx, w, repo, number)
		return
	}
	ctx.sendError(w, 404, "Not Found")
}

// jsIntNumber parses a segment the way JS Number() does and reports whether
// it is an integer.
func jsIntNumber(s string) (int, bool) {
	n, ok := jsNumber(s)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n {
		return 0, false
	}
	return int(n), true
}

// ---------------------------------------------------------------- commits / refs

var nameSpaceSlashRe = regexp.MustCompile(`[/\s]`)

func nameHasSpaceOrSlash(name string) bool {
	return nameSpaceSlashRe.MatchString(name)
}

func resolveRefTip(s *Store, repo *Repo, ref *string) (string, bool) {
	if ref == nil || *ref == "" {
		tip := s.HeadTip(repo)
		return tip, tip != ""
	}
	if isOID(*ref) {
		if s.HasObject(repo, *ref) {
			return *ref, true
		}
		return "", false
	}
	if strings.HasPrefix(*ref, "refs/") {
		return s.GetRef(repo, *ref)
	}
	return s.GetRef(repo, s.RefName(*ref))
}

func listCommits(ctx *Ctx, w http.ResponseWriter, repo *Repo) {
	store := ctx.Store
	var sha *string
	if v := ctx.Query.Get("sha"); v != "" {
		sha = &v
	}
	tip, ok := resolveRefTip(store, repo, sha)
	if !ok {
		ctx.sendJSON(w, 200, []any{}, nil)
		return
	}
	commits := store.WalkCommits(repo, []string{tip})
	out := make([]any, 0, len(commits))
	for _, oid := range commits {
		parsed := store.ReadCommit(repo, oid)
		message := ""
		author := ""
		committer := ""
		if parsed != nil {
			message = parsed.Message
			author = parsed.Author
			committer = parsed.Committer
		}
		out = append(out, map[string]any{
			"sha": oid,
			"commit": map[string]any{
				"message":   message,
				"author":    identityJSON(author),
				"committer": identityJSON(committer),
			},
			"author":    map[string]any{"login": ViewerLogin},
			"committer": map[string]any{"login": ViewerLogin},
		})
	}
	ctx.sendJSON(w, 200, out, nil)
}

var identityRe = regexp.MustCompile(`^(.*) <([^>]*)> (\d+) [+-]\d{4}$`)

func identityJSON(line string) map[string]any {
	m := identityRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return map[string]any{"name": line, "email": "", "date": ""}
	}
	epoch, _ := strconv.ParseInt(m[3], 10, 64)
	return map[string]any{"name": m[1], "email": m[2], "date": isoOfEpoch(epoch)}
}

// isoOfEpoch renders an absolute epoch as an ISO-8601 string (toISOString).
func isoOfEpoch(epoch int64) string {
	return timeUnix(epoch).Format("2006-01-02T15:04:05.000Z")
}

func createRef(ctx *Ctx, w http.ResponseWriter, repo *Repo) {
	body := readBodyJSON(ctx)
	if !body.ok {
		ctx.sendError(w, body.status, body.message)
		return
	}
	ref, _ := body.value["ref"].(string)
	sha, _ := body.value["sha"].(string)
	if !strings.HasPrefix(ref, "refs/") || !isOID(sha) || !ctx.Store.HasObject(repo, sha) {
		ctx.sendError(w, 422, "Validation Failed: ref and sha are required")
		return
	}
	if _, ok := ctx.Store.GetRef(repo, ref); ok {
		ctx.sendError(w, 422, "Validation Failed: Reference "+ref+" already exists")
		return
	}
	ctx.Store.SetRef(repo, ref, sha)
	ctx.sendJSON(w, 201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"}}, nil)
}

func getRef(ctx *Ctx, w http.ResponseWriter, repo *Repo, tail []string) {
	if len(tail) == 0 {
		out := make([]any, 0)
		for _, ref := range ctx.Store.ListRefs(repo) {
			out = append(out, map[string]any{"ref": ref.Name, "object": map[string]any{"sha": ref.OID, "type": "commit"}})
		}
		ctx.sendJSON(w, 200, out, nil)
		return
	}
	ref := "refs/" + strings.Join(tail, "/")
	oid, ok := ctx.Store.GetRef(repo, ref)
	if !ok {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	ctx.sendJSON(w, 200, map[string]any{"ref": ref, "object": map[string]any{"sha": oid, "type": "commit"}}, nil)
}

// ---------------------------------------------------------------- contents

func contentsCommit(s *Store, repo *Repo, branch string, changes map[string]string, message string) string {
	currentTip, hasTip := s.GetRef(repo, s.RefName(branch))
	baseTree := ""
	if hasTip {
		if commit := s.ReadCommit(repo, currentTip); commit != nil {
			baseTree = commit.Tree
		}
	}
	treeOID := s.UpdateTree(repo, baseTree, changes)
	var parents []string
	if hasTip {
		parents = []string{currentTip}
	}
	commitOID := s.CreateCommit(repo, treeOID, parents, message)
	s.SetRef(repo, s.RefName(branch), commitOID)
	return commitOID
}

// b64decodeLenient decodes base64 the way Node's Buffer.from(str, 'base64')
// does: whitespace stripped, never throws, invalid characters dropped.
func b64decodeLenient(text string) []byte {
	clean := wsRe.ReplaceAllString(text, "")
	// Keep only alphabet + padding; Node stops/drops the rest.
	var kept []byte
	for i := 0; i < len(clean); i++ {
		c := clean[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			kept = append(kept, c)
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(string(kept))
	if err == nil {
		return decoded
	}
	// Trim to the longest decodable prefix (multiples of 4, padding intact).
	for n := len(kept); n > 0; n-- {
		if d, err := base64.StdEncoding.DecodeString(string(kept[:n])); err == nil {
			return d
		}
	}
	return []byte{}
}

func putContents(ctx *Ctx, w http.ResponseWriter, repo *Repo, pathParts []string) {
	store := ctx.Store
	path := strings.Join(pathParts, "/")
	if path == "" {
		ctx.sendError(w, 422, "Validation Failed: path is required")
		return
	}
	body := readBodyJSON(ctx)
	if !body.ok {
		ctx.sendError(w, body.status, body.message)
		return
	}
	message := "Update file"
	if v, ok := body.value["message"].(string); ok && v != "" {
		message = v
	}
	contentB64, _ := body.value["content"].(string)
	branch := repo.DefaultBranch
	if v, ok := body.value["branch"].(string); ok && v != "" {
		branch = v
	}
	var sha *string
	if v, ok := body.value["sha"].(string); ok {
		sha = &v
	}
	content := b64decodeLenient(contentB64)
	branchTip, hasTip := store.GetRef(repo, store.RefName(branch))
	var existing *GitObject
	if hasTip {
		existing = store.GetBlobAt(repo, branchTip, path)
	}
	if sha != nil {
		if existing == nil || hashObject(ObjBlob, existing.Data) != *sha {
			ctx.sendError(w, 409, "Conflict: the file has changed since the given sha")
			return
		}
	} else if existing != nil {
		ctx.sendError(w, 422, "Validation Failed: "+path+" already exists")
		return
	}
	// A branch that does not exist yet is created from the default branch tip
	// (or as the repository's first commit when the repository is unborn).
	if !hasTip && branch != repo.DefaultBranch {
		if headTip := store.HeadTip(repo); headTip != "" {
			store.SetRef(repo, store.RefName(branch), headTip)
		}
	}
	blobOID := store.PutObject(repo, ObjBlob, content)
	commitOID := contentsCommit(store, repo, branch, map[string]string{path: blobOID}, message)
	isUpdate := existing != nil
	status := 201
	if isUpdate {
		status = 200
	}
	ctx.sendJSON(w, status, map[string]any{
		"content": map[string]any{
			"name":     lastPathSegment(path),
			"path":     path,
			"sha":      blobOID,
			"size":     len(content),
			"type":     "file",
			"encoding": "base64",
			"content":  b64encode(content),
		},
		"commit": map[string]any{"sha": commitOID, "message": message},
	}, nil)
}

func lastPathSegment(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func getContents(ctx *Ctx, w http.ResponseWriter, repo *Repo, pathParts []string) {
	store := ctx.Store
	path := strings.Join(pathParts, "/")
	var ref *string
	if v := ctx.Query.Get("ref"); v != "" {
		ref = &v
	}
	tip, ok := resolveRefTip(store, repo, ref)
	if !ok {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	commit := store.ReadCommit(repo, tip)
	if commit == nil {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	if path == "" {
		ctx.sendJSON(w, 200, directoryEntries(store, repo, commit.Tree, ""), nil)
		return
	}
	flat := store.FlattenTree(repo, commit.Tree)
	if file, ok := flat[path]; ok {
		blob := store.GetObject(repo, file.OID)
		if blob == nil {
			ctx.sendError(w, 404, "Not Found")
			return
		}
		ctx.sendJSON(w, 200, map[string]any{
			"name":     lastPathSegment(path),
			"path":     path,
			"sha":      file.OID,
			"size":     len(blob.Data),
			"type":     "file",
			"encoding": "base64",
			"content":  b64encode(blob.Data),
		}, nil)
		return
	}
	// A directory: list its immediate children.
	prefix := path + "/"
	hasChild := false
	for filePath := range flat {
		if strings.HasPrefix(filePath, prefix) {
			hasChild = true
			break
		}
	}
	if !hasChild {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	ctx.sendJSON(w, 200, directoryEntries(store, repo, commit.Tree, path), nil)
}

func directoryEntries(s *Store, repo *Repo, treeOID, dirPath string) []any {
	var segments []string
	if dirPath != "" {
		segments = strings.Split(dirPath, "/")
	}
	current := treeOID
	for _, segment := range segments {
		entries := s.ReadTree(repo, current)
		if entries == nil {
			return []any{}
		}
		var found *TreeEntry
		for i := range entries {
			if entries[i].Name == segment && entries[i].Mode == "40000" {
				found = &entries[i]
				break
			}
		}
		if found == nil {
			return []any{}
		}
		current = found.OID
	}
	entries := s.ReadTree(repo, current)
	if entries == nil {
		return []any{}
	}
	out := make([]any, 0, len(entries))
	for _, entry := range entries {
		entryPath := entry.Name
		if dirPath != "" {
			entryPath = dirPath + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			out = append(out, map[string]any{"type": "dir", "name": entry.Name, "path": entryPath, "sha": entry.OID})
		} else {
			size := 0
			if blob := s.GetObject(repo, entry.OID); blob != nil {
				size = len(blob.Data)
			}
			out = append(out, map[string]any{"type": "file", "name": entry.Name, "path": entryPath, "sha": entry.OID, "size": size})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].(map[string]any)["name"].(string) < out[j].(map[string]any)["name"].(string)
	})
	return out
}

func deleteContents(ctx *Ctx, w http.ResponseWriter, repo *Repo, pathParts []string) {
	store := ctx.Store
	path := strings.Join(pathParts, "/")
	body := readBodyJSON(ctx)
	if !body.ok {
		ctx.sendError(w, body.status, body.message)
		return
	}
	message := "Delete file"
	if v, ok := body.value["message"].(string); ok && v != "" {
		message = v
	}
	var sha *string
	if v, ok := body.value["sha"].(string); ok {
		sha = &v
	}
	branch := repo.DefaultBranch
	if v, ok := body.value["branch"].(string); ok && v != "" {
		branch = v
	}
	branchTip, hasTip := store.GetRef(repo, store.RefName(branch))
	var existing *GitObject
	if hasTip {
		existing = store.GetBlobAt(repo, branchTip, path)
	}
	if existing == nil {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	if sha != nil && hashObject(ObjBlob, existing.Data) != *sha {
		ctx.sendError(w, 409, "Conflict: the file has changed since the given sha")
		return
	}
	commitOID := contentsCommit(store, repo, branch, map[string]string{path: ""}, message)
	ctx.sendJSON(w, 200, map[string]any{"content": nil, "commit": map[string]any{"sha": commitOID, "message": message}}, nil)
}

// ---------------------------------------------------------------- labels

func createLabel(ctx *Ctx, w http.ResponseWriter, repo *Repo) {
	body := readBodyJSON(ctx)
	if !body.ok {
		ctx.sendError(w, body.status, body.message)
		return
	}
	name := ""
	if v, ok := body.value["name"].(string); ok {
		name = strings.TrimSpace(v)
	}
	color := "ededed"
	if v, ok := body.value["color"].(string); ok {
		color = strings.TrimPrefix(v, "#")
	}
	if name == "" {
		ctx.sendError(w, 422, "Validation Failed: name is required")
		return
	}
	for _, label := range repo.Labels {
		if label.Name == name {
			ctx.sendError(w, 422, "Validation Failed: label "+name+" already exists")
			return
		}
	}
	description := ""
	if v, ok := body.value["description"].(string); ok {
		description = v
	}
	label := Label{Name: name, Color: color, Description: description}
	repo.Labels = append(repo.Labels, label)
	invalidateRepoIssues(repo)
	ctx.sendJSON(w, 201, labelJSON(label), nil)
}

// ---------------------------------------------------------------- issues

func createIssue(ctx *Ctx, w http.ResponseWriter, repo *Repo) {
	body := readBodyJSON(ctx)
	if !body.ok {
		ctx.sendError(w, body.status, body.message)
		return
	}
	title, _ := body.value["title"].(string)
	if title == "" {
		ctx.sendError(w, 422, "Validation Failed: title is required")
		return
	}
	var labels []string
	if arr, ok := body.value["labels"].([]any); ok {
		for _, entry := range arr {
			if s, ok := entry.(string); ok {
				labels = append(labels, s)
			}
		}
	}
	for _, name := range labels {
		found := false
		for _, label := range repo.Labels {
			if label.Name == name {
				found = true
				break
			}
		}
		if !found {
			ctx.sendError(w, 422, "Validation Failed: label "+name+" does not exist")
			return
		}
	}
	seq := ctx.Store.Seq()
	bodyText, _ := body.value["body"].(string)
	issue := &Issue{
		ID:         ctx.Store.NextGlobalID(),
		Number:     repo.NextNumber,
		Title:      title,
		Body:       bodyText,
		State:      "open",
		Labels:     labels,
		Comments:   []*Comment{},
		CreatedSeq: seq,
		UpdatedSeq: seq,
	}
	repo.NextNumber++
	repo.Issues = append(repo.Issues, issue)
	ctx.sendJSONRaw(w, 201, issueJSONBytes(ctx.Store, repo, issue), nil)
}

func listIssues(ctx *Ctx, w http.ResponseWriter, repo *Repo) {
	state := strings.ToLower(ctx.Query.Get("state"))
	if state == "" {
		state = "open"
	}
	sortBy := strings.ToLower(ctx.Query.Get("sort"))
	if sortBy == "" {
		sortBy = "created"
	}
	direction := strings.ToLower(ctx.Query.Get("direction"))
	if direction == "" {
		direction = "desc"
	}
	var issues []*Issue
	for _, issue := range repo.Issues {
		switch state {
		case "open":
			if issue.State == "open" {
				issues = append(issues, issue)
			}
		case "closed":
			if issue.State == "closed" {
				issues = append(issues, issue)
			}
		case "all":
			issues = append(issues, issue)
		default:
			if issue.State == "open" {
				issues = append(issues, issue)
			}
		}
	}
	sorted := append([]*Issue{}, issues...)
	switch sortBy {
	case "created":
		sort.Slice(sorted, func(i, j int) bool {
			if direction == "asc" {
				return sorted[i].CreatedSeq < sorted[j].CreatedSeq
			}
			return sorted[i].CreatedSeq > sorted[j].CreatedSeq
		})
	case "updated":
		sort.Slice(sorted, func(i, j int) bool {
			if direction == "asc" {
				return sorted[i].UpdatedSeq < sorted[j].UpdatedSeq
			}
			return sorted[i].UpdatedSeq > sorted[j].UpdatedSeq
		})
	case "comments":
		sort.Slice(sorted, func(i, j int) bool {
			if direction == "asc" {
				return len(sorted[i].Comments) < len(sorted[j].Comments)
			}
			return len(sorted[i].Comments) > len(sorted[j].Comments)
		})
	}
	params := getPageParams(ctx)
	link := linkHeader(ctx, params.page, params.perPage, len(sorted))
	var headers map[string]string
	if link != "" {
		headers = map[string]string{"link": link}
	}
	page := pageSlice(sorted, params)
	out := make([]any, 0, len(page))
	for _, issue := range page {
		out = append(out, issueJSON(ctx.Store, repo, issue))
	}
	ctx.sendJSON(w, 200, out, headers)
}

func getIssue(ctx *Ctx, w http.ResponseWriter, repo *Repo, number int) {
	issue := FindIssue(repo, number)
	if issue == nil {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	ctx.sendJSONRaw(w, 200, issueJSONBytes(ctx.Store, repo, issue), nil)
}

func patchIssue(ctx *Ctx, w http.ResponseWriter, repo *Repo, number int) {
	issue := FindIssue(repo, number)
	if issue == nil {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	body := readBodyJSON(ctx)
	if !body.ok {
		ctx.sendError(w, body.status, body.message)
		return
	}
	if v, ok := body.value["title"].(string); ok {
		issue.Title = v
	}
	if v, ok := body.value["body"].(string); ok {
		issue.Body = v
	}
	if v, ok := body.value["state"].(string); ok && (v == "open" || v == "closed") {
		issue.State = v
	}
	issue.UpdatedSeq = ctx.Store.Seq()
	invalidateIssue(issue)
	ctx.sendJSONRaw(w, 200, issueJSONBytes(ctx.Store, repo, issue), nil)
}

func createComment(ctx *Ctx, w http.ResponseWriter, repo *Repo, number int) {
	issue := FindIssue(repo, number)
	if issue == nil {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	body := readBodyJSON(ctx)
	if !body.ok {
		ctx.sendError(w, body.status, body.message)
		return
	}
	text, _ := body.value["body"].(string)
	comment := &Comment{
		ID:         repo.NextCommentID,
		Body:       text,
		Author:     ViewerLogin,
		CreatedSeq: ctx.Store.Seq(),
	}
	repo.NextCommentID++
	issue.Comments = append(issue.Comments, comment)
	issue.UpdatedSeq = ctx.Store.Seq()
	invalidateIssue(issue)
	ctx.sendJSON(w, 201, commentJSON(comment), nil)
}

func listComments(ctx *Ctx, w http.ResponseWriter, repo *Repo, number int) {
	issue := FindIssue(repo, number)
	if issue == nil {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	out := make([]any, 0, len(issue.Comments))
	for _, comment := range issue.Comments {
		out = append(out, commentJSON(comment))
	}
	ctx.sendJSON(w, 200, out, nil)
}

// ---------------------------------------------------------------- pulls

type pullInput struct {
	title string
	body  string
	head  string
	base  string
}

func createPullRecord(s *Store, repo *Repo, input pullInput) (*Pull, string) {
	headTip, headOk := s.GetRef(repo, s.RefName(input.head))
	baseTip, baseOk := s.GetRef(repo, s.RefName(input.base))
	if !headOk || !baseOk {
		return nil, "head and base branches must exist"
	}
	for _, pull := range repo.Pulls {
		if pull.HeadRef == input.head && pull.BaseRef == input.base && pull.State == "open" {
			return nil, "a pull request from " + input.head + " is already open"
		}
	}
	seq := s.Seq()
	pull := &Pull{
		ID:         s.NextGlobalID(),
		Number:     repo.NextNumber,
		Title:      input.title,
		Body:       input.body,
		State:      "open",
		Merged:     false,
		MergedAt:   nil,
		HeadRef:    input.head,
		BaseRef:    input.base,
		HeadSHA:    headTip,
		BaseSHA:    baseTip,
		CreatedSeq: seq,
		UpdatedSeq: seq,
		Comments:   []*Comment{},
	}
	repo.NextNumber++
	repo.Pulls = append(repo.Pulls, pull)
	return pull, ""
}

func createPull(ctx *Ctx, w http.ResponseWriter, repo *Repo) {
	body := readBodyJSON(ctx)
	if !body.ok {
		ctx.sendError(w, body.status, body.message)
		return
	}
	title, _ := body.value["title"].(string)
	head, _ := body.value["head"].(string)
	base, _ := body.value["base"].(string)
	if title == "" || head == "" || base == "" {
		ctx.sendError(w, 422, "Validation Failed: title, head and base are required")
		return
	}
	bodyText, _ := body.value["body"].(string)
	pull, errMsg := createPullRecord(ctx.Store, repo, pullInput{title: title, body: bodyText, head: head, base: base})
	if pull == nil {
		if errMsg == "" {
			errMsg = "cannot create pull request"
		}
		ctx.sendError(w, 422, "Validation Failed: "+errMsg)
		return
	}
	ctx.sendJSON(w, 201, pullJSON(ctx.Store, repo, pull), nil)
}

func listPulls(ctx *Ctx, w http.ResponseWriter, repo *Repo) {
	state := strings.ToLower(ctx.Query.Get("state"))
	if state == "" {
		state = "open"
	}
	var pulls []*Pull
	for _, pull := range repo.Pulls {
		switch state {
		case "open":
			if pull.State == "open" {
				pulls = append(pulls, pull)
			}
		case "closed":
			if pull.State == "closed" {
				pulls = append(pulls, pull)
			}
		case "all":
			pulls = append(pulls, pull)
		default:
			if pull.State == "open" {
				pulls = append(pulls, pull)
			}
		}
	}
	sorted := append([]*Pull{}, pulls...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedSeq > sorted[j].CreatedSeq })
	params := getPageParams(ctx)
	link := linkHeader(ctx, params.page, params.perPage, len(sorted))
	var headers map[string]string
	if link != "" {
		headers = map[string]string{"link": link}
	}
	page := pageSlice(sorted, params)
	out := make([]any, 0, len(page))
	for _, pull := range page {
		out = append(out, pullJSON(ctx.Store, repo, pull))
	}
	ctx.sendJSON(w, 200, out, headers)
}

func getPull(ctx *Ctx, w http.ResponseWriter, repo *Repo, number int) {
	pull := FindPull(repo, number)
	if pull == nil {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	ctx.sendJSON(w, 200, pullJSON(ctx.Store, repo, pull), nil)
}

type pullFileEntry struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

// pullFiles returns the changed files of a pull request, with line counts.
func pullFiles(s *Store, repo *Repo, pull *Pull) []pullFileEntry {
	baseTip, ok := s.GetRef(repo, s.RefName(pull.BaseRef))
	if !ok {
		baseTip = pull.BaseSHA
	}
	headTip, ok := s.GetRef(repo, s.RefName(pull.HeadRef))
	if !ok {
		headTip = pull.HeadSHA
	}
	baseCommit := s.ReadCommit(repo, baseTip)
	headCommit := s.ReadCommit(repo, headTip)
	if baseCommit == nil || headCommit == nil {
		return []pullFileEntry{}
	}
	diffs := s.DiffTrees(repo, baseCommit.Tree, headCommit.Tree)
	out := make([]pullFileEntry, 0, len(diffs))
	for _, diff := range diffs {
		var oldText, newText *string
		if diff.OldOID != "" {
			if obj := s.GetObject(repo, diff.OldOID); obj != nil {
				t := string(obj.Data)
				oldText = &t
			}
		}
		if diff.NewOID != "" {
			if obj := s.GetObject(repo, diff.NewOID); obj != nil {
				t := string(obj.Data)
				newText = &t
			}
		}
		additions, deletions := lineDiff(oldText, newText)
		status := "modified"
		if diff.Status == "added" {
			status = "added"
		} else if diff.Status == "deleted" {
			status = "removed"
		}
		out = append(out, pullFileEntry{Filename: diff.Path, Status: status, Additions: additions, Deletions: deletions, Changes: additions + deletions})
	}
	return out
}

func listPullFiles(ctx *Ctx, w http.ResponseWriter, repo *Repo, number int) {
	pull := FindPull(repo, number)
	if pull == nil {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	ctx.sendJSON(w, 200, pullFiles(ctx.Store, repo, pull), nil)
}

type mergeResult struct {
	sha     string
	status  int
	message string
}

// mergePullRecord merges a pull request. Returns the merge commit sha, or ""
// with a status (404 unknown, 405 conflicted or already merged).
func mergePullRecord(s *Store, repo *Repo, pull *Pull, method string) mergeResult {
	if pull.State != "open" {
		return mergeResult{status: 405, message: "Pull request is already closed"}
	}
	baseTip, baseOk := s.GetRef(repo, s.RefName(pull.BaseRef))
	headTip, headOk := s.GetRef(repo, s.RefName(pull.HeadRef))
	if !baseOk || !headOk {
		return mergeResult{status: 405, message: "Head or base branch is missing"}
	}
	merged := s.MergeTrees(repo, baseTip, headTip)
	if merged == nil {
		return mergeResult{status: 405, message: "Merge conflict"}
	}
	var mergeSHA string
	if method == "merge" {
		mergeSHA = s.CreateCommit(repo, merged.TreeOID, []string{baseTip, headTip},
			"Merge pull request #"+itoa(pull.Number)+" from "+repo.Owner+"/"+pull.HeadRef)
	} else if method == "rebase" {
		// Replay the head's commits (those not reachable from the base) onto the base tip.
		headOnly := []string{}
		for _, oid := range s.WalkCommits(repo, []string{headTip}) {
			if !s.IsAncestor(repo, oid, baseTip) {
				headOnly = append(headOnly, oid)
			}
		}
		// reverse: oldest first
		for i, j := 0, len(headOnly)-1; i < j; i, j = i+1, j-1 {
			headOnly[i], headOnly[j] = headOnly[j], headOnly[i]
		}
		tip := baseTip
		last := baseTip
		for _, oid := range headOnly {
			commit := s.ReadCommit(repo, oid)
			if commit == nil {
				return mergeResult{status: 405, message: "Merge conflict"}
			}
			var parentTree *string
			if len(commit.Parents) > 0 {
				if pc := s.ReadCommit(repo, commit.Parents[0]); pc != nil {
					parentTree = &pc.Tree
				}
			}
			var currentTree *string
			if tc := s.ReadCommit(repo, tip); tc != nil {
				currentTree = &tc.Tree
			}
			rebased := rebaseTree(s, repo, parentTree, currentTree, commit.Tree)
			if rebased == "" {
				return mergeResult{status: 405, message: "Merge conflict"}
			}
			last = s.CreateCommit(repo, rebased, []string{tip}, commit.Message)
			tip = last
		}
		mergeSHA = last
		if len(headOnly) == 0 {
			mergeSHA = baseTip
		}
	} else {
		// squash (the default)
		mergeSHA = s.CreateCommit(repo, merged.TreeOID, []string{baseTip}, pull.Title)
	}
	s.SetRef(repo, s.RefName(pull.BaseRef), mergeSHA)
	pull.State = "closed"
	pull.Merged = true
	mergedAt := s.Clock.NextISO()
	pull.MergedAt = &mergedAt
	pull.MergeCommitSHA = &mergeSHA
	pull.UpdatedSeq = s.Seq()
	invalidatePull(pull)
	return mergeResult{sha: mergeSHA, status: 200, message: "Pull request successfully merged"}
}

func invalidatePull(pull *Pull) {
	pull.Version++
	pull.cachedJSON = nil
}

// rebaseTree applies one commit's tree change onto another tree (rebase step).
func rebaseTree(s *Store, repo *Repo, parentTree, currentTree *string, commitTree string) string {
	toFlat := func(tree *string) FlatTree {
		flat := FlatTree{}
		if tree == nil {
			return flat
		}
		for path, file := range s.FlattenTree(repo, *tree) {
			flat[path] = file.OID
		}
		return flat
	}
	baseFlat := toFlat(parentTree)
	oursFlat := toFlat(currentTree)
	theirsFlat := toFlat(&commitTree)
	res := s.MergeFlatTrees(baseFlat, oursFlat, theirsFlat)
	if len(res.Conflicts) > 0 {
		return ""
	}
	flat := map[string]TreeEntry{}
	theirsModes := s.FlattenTree(repo, commitTree)
	var oursModes map[string]TreeEntry
	if currentTree != nil {
		oursModes = s.FlattenTree(repo, *currentTree)
	} else {
		oursModes = map[string]TreeEntry{}
	}
	for path, oid := range res.Merged {
		if oid == "" {
			continue
		}
		mode := "100644"
		if e, ok := theirsModes[path]; ok {
			mode = e.Mode
		} else if e, ok := oursModes[path]; ok {
			mode = e.Mode
		}
		flat[path] = TreeEntry{Mode: mode, OID: oid, Name: path}
	}
	return s.TreeFromFlat(repo, flat)
}

func mergePull(ctx *Ctx, w http.ResponseWriter, repo *Repo, number int) {
	pull := FindPull(repo, number)
	if pull == nil {
		ctx.sendError(w, 404, "Not Found")
		return
	}
	body := readBodyJSON(ctx)
	if !body.ok {
		ctx.sendError(w, body.status, body.message)
		return
	}
	method := "squash"
	if v, ok := body.value["merge_method"].(string); ok {
		method = strings.ToLower(v)
	}
	result := mergePullRecord(ctx.Store, repo, pull, method)
	if result.sha == "" {
		ctx.sendError(w, result.status, result.message)
		return
	}
	ctx.sendJSON(w, 200, map[string]any{"sha": result.sha, "merged": true, "message": result.message}, nil)
}
