// MCP over POST /mcp: Streamable HTTP, legacy 2025-11-25 era.
// `server/discover` answers method-not-found (pre-discover server), then
// `initialize` + `notifications/initialized` handshakes, then tools/list
// and tools/call. Plain JSON answers. Authentication is decided before any
// tool dispatch: no valid Bearer token -> HTTP 401. Mirrors src/mcp.ts.
package main

import (
	"net/http"
	"strings"
)

const mcpServerName = "gitforge"
const mcpServerVersion = "1.0.0"

var mcpLegacyRevisions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpToolProp struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

type mcpToolDef struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description"`
	InputSchema mcpToolInputSchema      `json:"inputSchema"`
}

type mcpToolInputSchema struct {
	Type       string                  `json:"type"`
	Properties map[string]mcpToolProp  `json:"properties"`
	Required   []string                `json:"required"`
}

func prop(t, desc string) mcpToolProp { return mcpToolProp{Type: t, Description: desc} }

var mcpTools = []mcpToolDef{
	{
		Name: "create_repo", Description: "Create a repository owned by the authenticated user.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"name": prop("string", "The repository name."),
			"description": prop("string", "A short description of the repository."),
			"private": prop("boolean", "Whether the repository is private."),
		}, Required: []string{"name"}},
	},
	{
		Name: "get_repo", Description: "Get a repository by owner and name.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"owner": prop("string", "The repository owner login."),
			"repo":  prop("string", "The repository name."),
		}, Required: []string{"owner", "repo"}},
	},
	{
		Name: "create_issue", Description: "Create an issue in a repository.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"owner": prop("string", "The repository owner login."),
			"repo":  prop("string", "The repository name."),
			"title": prop("string", "The issue title."),
			"body":  prop("string", "The issue body."),
		}, Required: []string{"owner", "repo", "title"}},
	},
	{
		Name: "list_issues", Description: "List issues in a repository, optionally filtered by state.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"owner": prop("string", "The repository owner login."),
			"repo":  prop("string", "The repository name."),
			"state": prop("string", "Filter by state: open, closed or all."),
		}, Required: []string{"owner", "repo"}},
	},
	{
		Name: "create_pull_request", Description: "Create a pull request in a repository.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"owner": prop("string", "The repository owner login."),
			"repo":  prop("string", "The repository name."),
			"title": prop("string", "The pull request title."),
			"head":  prop("string", "The head branch name."),
			"base":  prop("string", "The base branch name."),
			"body":  prop("string", "The pull request body."),
		}, Required: []string{"owner", "repo", "title", "head", "base"}},
	},
	{
		Name: "merge_pull_request", Description: "Merge a pull request in a repository.",
		InputSchema: mcpToolInputSchema{Type: "object", Properties: map[string]mcpToolProp{
			"owner":       prop("string", "The repository owner login."),
			"repo":        prop("string", "The repository name."),
			"pull_number": prop("integer", "The pull request number."),
		}, Required: []string{"owner", "repo", "pull_number"}},
	},
}

type jsonRPCID any

func rpcResult(id jsonRPCID, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func rpcError(id jsonRPCID, code int, message string, data ...any) map[string]any {
	err := map[string]any{"code": code, "message": message}
	if len(data) > 0 && data[0] != nil {
		err["data"] = data[0]
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": err}
}

func toolText(payload any) map[string]any {
	var text string
	if s, ok := payload.(string); ok {
		text = s
	} else {
		text = string(marshalJSON(payload))
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}
}

func toolError(message string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": message}}, "isError": true}
}

func mcpOwnedRepo(s *Store, args map[string]any) *Repo {
	owner, _ := args["owner"].(string)
	name, _ := args["repo"].(string)
	if !strings.EqualFold(owner, ViewerLogin) {
		return nil
	}
	return s.GetRepo(owner, name)
}

func mcpCallTool(s *Store, name string, args map[string]any) map[string]any {
	switch name {
	case "create_repo":
		repoName := ""
		if v, ok := args["name"].(string); ok {
			repoName = strings.TrimSpace(v)
		}
		if repoName == "" {
			return toolError("name is required")
		}
		description, _ := args["description"].(string)
		repo := s.CreateRepo(ViewerLogin, repoName, args["private"] == true, description)
		if repo == nil {
			return toolError("Repository " + repoName + " already exists")
		}
		return toolText(repoJSON(s, repo))
	case "get_repo":
		repo := mcpOwnedRepo(s, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		return toolText(repoJSON(s, repo))
	case "create_issue":
		repo := mcpOwnedRepo(s, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		title, _ := args["title"].(string)
		if title == "" {
			return toolError("title is required")
		}
		body, _ := args["body"].(string)
		seq := s.Seq()
		issue := &Issue{
			ID: s.NextGlobalID(), Number: repo.NextNumber, Title: title, Body: body,
			State: "open", Labels: []string{}, Comments: []*Comment{},
			CreatedSeq: seq, UpdatedSeq: seq,
		}
		repo.NextNumber++
		repo.Issues = append(repo.Issues, issue)
		return toolText(issueJSON(s, repo, issue))
	case "list_issues":
		repo := mcpOwnedRepo(s, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		state := "open"
		if v, ok := args["state"].(string); ok {
			state = strings.ToLower(v)
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
			default:
				issues = append(issues, issue)
			}
		}
		out := make([]any, 0, len(issues))
		for _, issue := range issues {
			out = append(out, issueJSON(s, repo, issue))
		}
		return toolText(out)
	case "create_pull_request":
		repo := mcpOwnedRepo(s, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		title, _ := args["title"].(string)
		head, _ := args["head"].(string)
		base, _ := args["base"].(string)
		if title == "" || head == "" || base == "" {
			return toolError("title, head and base are required")
		}
		body, _ := args["body"].(string)
		pull, errMsg := createPullRecord(s, repo, pullInput{title: title, body: body, head: head, base: base})
		if pull == nil {
			if errMsg == "" {
				errMsg = "cannot create pull request"
			}
			return toolError(errMsg)
		}
		return toolText(pullJSON(s, repo, pull))
	case "merge_pull_request":
		repo := mcpOwnedRepo(s, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		number := 0
		if n, ok := args["pull_number"].(float64); ok {
			number = int(n)
		}
		pull := FindPull(repo, number)
		if pull == nil {
			return toolError("Pull request not found")
		}
		result := mergePullRecord(s, repo, pull, "squash")
		if result.sha == "" {
			return toolError(result.message)
		}
		return toolText(map[string]any{"sha": result.sha, "merged": true, "message": result.message, "number": pull.Number, "state": pull.State})
	default:
		return toolError("Unknown tool: " + name)
	}
}

// handleMcp handles POST /mcp. Returns true when the path matched.
func handleMcp(ctx *Ctx, w http.ResponseWriter) bool {
	if ctx.Path != "/mcp" {
		return false
	}
	if ctx.Method != "POST" {
		ctx.sendJSON(w, 405, rpcError(nil, -32600, "Method Not Allowed"), nil)
		return true
	}
	if !ctx.isAuthorized() {
		ctx.sendJSON(w, 401, rpcError(nil, -32000, "Requires authentication"), nil)
		return true
	}
	parsed, ok, _ := ctx.parseJSONBody()
	if !ok {
		ctx.sendJSON(w, 400, rpcError(nil, -32700, "Parse error"), nil)
		return true
	}
	if !isRecord(parsed) {
		ctx.sendJSON(w, 400, rpcError(nil, -32600, "Invalid Request"), nil)
		return true
	}
	value := parsed.(map[string]any)
	method, ok := value["method"].(string)
	if !ok {
		ctx.sendJSON(w, 400, rpcError(nil, -32600, "Invalid Request"), nil)
		return true
	}
	var id jsonRPCID
	if v, ok := value["id"].(string); ok {
		id = v
	} else if v, ok := value["id"].(float64); ok {
		id = v
	} else {
		id = nil
	}
	params := map[string]any{}
	if v, ok := value["params"].(map[string]any); ok {
		params = v
	}
	// A notification (no id) is acknowledged with 202 and no body.
	if method == "notifications/initialized" || strings.HasPrefix(method, "notifications/") {
		ctx.sendNoContent(w, 202)
		return true
	}
	switch method {
	case "server/discover":
		// Pre-discover server: the suite's client falls back to the legacy handshake.
		ctx.sendJSON(w, 200, rpcError(id, -32601, "Method not found"), nil)
		return true
	case "initialize":
		requested, _ := params["protocolVersion"].(string)
		protocolVersion := "2025-11-25"
		for _, rev := range mcpLegacyRevisions {
			if rev == requested {
				protocolVersion = requested
				break
			}
		}
		ctx.sendJSON(w, 200, rpcResult(id, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		}), nil)
		return true
	case "ping":
		ctx.sendJSON(w, 200, rpcResult(id, map[string]any{}), nil)
		return true
	case "tools/list":
		ctx.sendJSON(w, 200, rpcResult(id, map[string]any{"tools": mcpTools}), nil)
		return true
	case "tools/call":
		name, _ := params["name"].(string)
		args := map[string]any{}
		if v, ok := params["arguments"].(map[string]any); ok {
			args = v
		}
		var tool *mcpToolDef
		for i := range mcpTools {
			if mcpTools[i].Name == name {
				tool = &mcpTools[i]
				break
			}
		}
		if tool == nil {
			ctx.sendJSON(w, 200, rpcError(id, -32602, "Unknown tool: "+name), nil)
			return true
		}
		var missing []string
		for _, key := range tool.InputSchema.Required {
			v, ok := args[key]
			if !ok || v == nil || v == "" {
				missing = append(missing, key)
			}
		}
		if len(missing) > 0 {
			ctx.sendJSON(w, 200, rpcResult(id, toolError("Missing required arguments: "+strings.Join(missing, ", "))), nil)
			return true
		}
		result := mcpCallTool(ctx.Store, name, args)
		ctx.sendJSON(w, 200, rpcResult(id, result), nil)
		return true
	case "resources/list":
		ctx.sendJSON(w, 200, rpcResult(id, map[string]any{"resources": []any{}}), nil)
		return true
	case "prompts/list":
		ctx.sendJSON(w, 200, rpcResult(id, map[string]any{"prompts": []any{}}), nil)
		return true
	default:
		ctx.sendJSON(w, 200, rpcError(id, -32601, "Method not found: "+method), nil)
		return true
	}
}
