// GraphQL over POST /api/graphql: a small hand-written parser and executor
// for the documents gh and github-mcp-server send. Supports operations,
// variables, fragments, inline fragments, aliases, enums, input objects,
// lists, __type/__schema introspection and batching. Mirrors src/graphql.ts.
package main

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- global ids

func repoGlobalID(repo *Repo) string {
	return "R_" + b64urlEncode([]byte("repo:" + repo.Owner + "/" + repo.Name))
}

func issueGlobalID(repo *Repo, issue *Issue) string {
	return "I_" + b64urlEncode([]byte(fmt.Sprintf("issue:%s/%s:%d", repo.Owner, repo.Name, issue.Number)))
}

func pullGlobalID(repo *Repo, pull *Pull) string {
	return "P_" + b64urlEncode([]byte(fmt.Sprintf("pull:%s/%s:%d", repo.Owner, repo.Name, pull.Number)))
}

func commentGlobalID(repo *Repo, commentID int) string {
	return "C_" + b64urlEncode([]byte(fmt.Sprintf("comment:%s/%s:%d", repo.Owner, repo.Name, commentID)))
}

type decodedID struct {
	kind   string // "repo" | "issue" | "pull" | "comment"
	owner  string
	name   string
	number int
}

var globalIDRe = regexp.MustCompile(`^([RIPC])_(.+)$`)
var globalIDBodyRe = regexp.MustCompile(`^(\w+):([^/]+)\/([^:]+)(?::(\d+))?$`)

func decodeGlobalID(id string) *decodedID {
	m := globalIDRe.FindStringSubmatch(id)
	if m == nil {
		return nil
	}
	kind := "comment"
	switch m[1] {
	case "R":
		kind = "repo"
	case "I":
		kind = "issue"
	case "P":
		kind = "pull"
	}
	text, ok := b64urlDecodeStr(m[2])
	if !ok {
		return nil
	}
	parts := globalIDBodyRe.FindStringSubmatch(text)
	if parts == nil {
		return nil
	}
	number := 0
	if parts[4] != "" {
		number, _ = strconv.Atoi(parts[4])
	}
	return &decodedID{kind: kind, owner: parts[2], name: parts[3], number: number}
}

// ---------------------------------------------------------------- lexer

type tokenKind int

const (
	tokPunct tokenKind = iota
	tokName
	tokInt
	tokFloat
	tokString
	tokEOF
)

type gqlToken struct {
	kind  tokenKind
	value string
	num   float64
}

var gqlNumberRe = regexp.MustCompile(`^-?\d+(\.\d+)?([eE][+-]?\d+)?`)
var gqlNameRe = regexp.MustCompile(`^[_A-Za-z][_0-9A-Za-z]*`)

func lex(source string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	for i < len(source) {
		char := source[i]
		if char == ' ' || char == '\t' || char == '\n' || char == '\r' || char == ',' || (i+2 < len(source) && source[i] == 0xEF && source[i+1] == 0xBB && source[i+2] == 0xBF) {
			if source[i] == 0xEF {
				i += 3
			} else {
				i++
			}
			continue
		}
		if char == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if char == '.' && i+2 < len(source) && source[i+1] == '.' && source[i+2] == '.' {
			tokens = append(tokens, gqlToken{kind: tokPunct, value: "..."})
			i += 3
			continue
		}
		if strings.ContainsRune("{}()[]:!=$@&|", rune(char)) {
			tokens = append(tokens, gqlToken{kind: tokPunct, value: string(char)})
			i++
			continue
		}
		if char == '"' {
			if i+2 < len(source) && source[i+1] == '"' && source[i+2] == '"' {
				end := strings.Index(source[i+3:], `"""`)
				if end < 0 {
					return nil, fmt.Errorf("Unterminated block string")
				}
				tokens = append(tokens, gqlToken{kind: tokString, value: source[i+3 : i+3+end]})
				i = i + 3 + end + 3
				continue
			}
			j := i + 1
			var sb strings.Builder
			for j < len(source) && source[j] != '"' {
				if source[j] == '\\' {
					escaped := byte(0)
					if j+1 < len(source) {
						escaped = source[j+1]
					}
					switch escaped {
					case 'u':
						hexStr := ""
						if j+5 < len(source) {
							hexStr = source[j+2 : j+6]
						}
						n, _ := strconv.ParseInt(hexStr, 16, 32)
						sb.WriteRune(rune(n))
						j += 6
					case 'n':
						sb.WriteByte('\n')
						j += 2
					case 't':
						sb.WriteByte('\t')
						j += 2
					case 'r':
						sb.WriteByte('\r')
						j += 2
					case '"':
						sb.WriteByte('"')
						j += 2
					case '\\':
						sb.WriteByte('\\')
						j += 2
					case '/':
						sb.WriteByte('/')
						j += 2
					case 'b':
						sb.WriteByte('\b')
						j += 2
					case 'f':
						sb.WriteByte('\f')
						j += 2
					default:
						sb.WriteByte(escaped)
						j += 2
					}
				} else {
					sb.WriteByte(source[j])
					j++
				}
			}
			if j >= len(source) {
				return nil, fmt.Errorf("Unterminated string")
			}
			tokens = append(tokens, gqlToken{kind: tokString, value: sb.String()})
			i = j + 1
			continue
		}
		if (char >= '0' && char <= '9') || char == '-' {
			m := gqlNumberRe.FindString(source[i:])
			if m == "" {
				return nil, fmt.Errorf("Bad number at %d", i)
			}
			n, _ := strconv.ParseFloat(m, 64)
			if strings.ContainsAny(m, ".eE") {
				tokens = append(tokens, gqlToken{kind: tokFloat, num: n})
			} else {
				tokens = append(tokens, gqlToken{kind: tokInt, num: n})
			}
			i += len(m)
			continue
		}
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || char == '_' {
			m := gqlNameRe.FindString(source[i:])
			if m == "" {
				return nil, fmt.Errorf("Bad name at %d", i)
			}
			tokens = append(tokens, gqlToken{kind: tokName, value: m})
			i += len(m)
			continue
		}
		return nil, fmt.Errorf("Unexpected character %s at %d", jsonString(string(char)), i)
	}
	tokens = append(tokens, gqlToken{kind: tokEOF})
	return tokens, nil
}

// ---------------------------------------------------------------- parser

type valueNode struct {
	kind   string // variable|int|float|string|boolean|null|enum|list|object
	name   string
	num    float64
	str    string
	b      bool
	values []*valueNode
	fields []valueField
}

type valueField struct {
	name  string
	value *valueNode
}

type gqlSelection struct {
	kind          string // field|fragmentSpread|inlineFragment
	alias         string
	name          string
	args          []gqlArg
	selections    []*gqlSelection
	typeCondition string
}

type gqlArg struct {
	name  string
	value *valueNode
}

type gqlOperation struct {
	typ       string // query|mutation|subscription
	name      string
	variables []gqlVarDef
	selections []*gqlSelection
}

type gqlVarDef struct {
	name         string
	defaultValue *valueNode
}

type gqlFragment struct {
	name          string
	typeCondition string
	selections    []*gqlSelection
}

type gqlDocument struct {
	operations []*gqlOperation
	fragments  map[string]*gqlFragment
}

type gqlParser struct {
	tokens []gqlToken
	pos    int
}

func (p *gqlParser) peek() gqlToken  { return p.tokens[p.pos] }
func (p *gqlParser) next() gqlToken  { t := p.tokens[p.pos]; p.pos++; return t }

func (p *gqlParser) expectPunct(value string) error {
	token := p.next()
	if token.kind != tokPunct || token.value != value {
		return fmt.Errorf("Expected %s, got %s %s", value, tokenKindName(token.kind), tokenDisplayValue(token))
	}
	return nil
}

func tokenKindName(k tokenKind) string {
	switch k {
	case tokPunct:
		return "punct"
	case tokName:
		return "name"
	case tokInt:
		return "int"
	case tokFloat:
		return "float"
	case tokString:
		return "string"
	default:
		return "eof"
	}
}

func tokenDisplayValue(t gqlToken) string {
	switch t.kind {
	case tokPunct, tokName, tokString:
		return t.value
	case tokInt, tokFloat:
		return strconv.FormatFloat(t.num, 'g', -1, 64)
	default:
		return ""
	}
}

func (p *gqlParser) isPunct(value string) bool {
	t := p.peek()
	return t.kind == tokPunct && t.value == value
}

func (p *gqlParser) expectName() (string, error) {
	token := p.next()
	if token.kind != tokName {
		return "", fmt.Errorf("Expected name, got %s", tokenKindName(token.kind))
	}
	return token.value, nil
}

func (p *gqlParser) parseDocument() (*gqlDocument, error) {
	doc := &gqlDocument{fragments: map[string]*gqlFragment{}}
	for p.peek().kind != tokEOF {
		token := p.peek()
		if token.kind == tokPunct && token.value == "{" {
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, &gqlOperation{typ: "query", selections: sels})
		} else if token.kind == tokName && (token.value == "query" || token.value == "mutation" || token.value == "subscription") {
			typeToken := p.next()
			if typeToken.kind != tokName {
				return nil, fmt.Errorf("Expected an operation type")
			}
			typ := typeToken.value
			var name string
			if p.peek().kind == tokName {
				n, err := p.expectName()
				if err != nil {
					return nil, err
				}
				name = n
			}
			var variables []gqlVarDef
			if p.isPunct("(") {
				v, err := p.parseVariableDefinitions()
				if err != nil {
					return nil, err
				}
				variables = v
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, &gqlOperation{typ: typ, name: name, variables: variables, selections: sels})
		} else if token.kind == tokName && token.value == "fragment" {
			p.next()
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			on, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if on != "on" {
				return nil, fmt.Errorf("Expected \"on\" in fragment")
			}
			typeCondition, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			doc.fragments[name] = &gqlFragment{name: name, typeCondition: typeCondition, selections: sels}
		} else {
			return nil, fmt.Errorf("Unexpected token %s", tokenKindName(token.kind))
		}
	}
	return doc, nil
}

func (p *gqlParser) skipDirectives() error {
	for p.isPunct("@") {
		p.next()
		if _, err := p.expectName(); err != nil {
			return err
		}
		if p.isPunct("(") {
			if _, err := p.parseArguments(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *gqlParser) parseVariableDefinitions() ([]gqlVarDef, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var defs []gqlVarDef
	for !p.isPunct(")") {
		if err := p.expectPunct("$"); err != nil {
			return nil, err
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		if _, err := p.parseType(); err != nil {
			return nil, err
		}
		var def *valueNode
		if p.isPunct("=") {
			p.next()
			v, err := p.parseValue(true)
			if err != nil {
				return nil, err
			}
			def = v
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		defs = append(defs, gqlVarDef{name: name, defaultValue: def})
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return defs, nil
}

func (p *gqlParser) parseType() (string, error) {
	if p.isPunct("[") {
		p.next()
		inner, err := p.parseType()
		if err != nil {
			return "", err
		}
		if err := p.expectPunct("]"); err != nil {
			return "", err
		}
		text := "[" + inner + "]"
		if p.isPunct("!") {
			p.next()
			text += "!"
		}
		return text, nil
	}
	name, err := p.expectName()
	if err != nil {
		return "", err
	}
	text := name
	if p.isPunct("!") {
		p.next()
		text += "!"
	}
	return text, nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlSelection, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	for !p.isPunct("}") {
		if p.peek().kind == tokEOF {
			return nil, fmt.Errorf("Unterminated selection set")
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	if err := p.expectPunct("}"); err != nil {
		return nil, err
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (*gqlSelection, error) {
	if p.isPunct("...") {
		p.next()
		token := p.peek()
		if token.kind == tokName && token.value != "on" {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.skipDirectives(); err != nil {
				return nil, err
			}
			return &gqlSelection{kind: "fragmentSpread", name: name}, nil
		}
		var typeCondition string
		if token.kind == tokName && token.value == "on" {
			p.next()
			tc, err := p.expectName()
			if err != nil {
				return nil, err
			}
			typeCondition = tc
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &gqlSelection{kind: "inlineFragment", typeCondition: typeCondition, selections: sels}, nil
	}
	first, err := p.expectName()
	if err != nil {
		return nil, err
	}
	alias := ""
	name := first
	if p.isPunct(":") {
		p.next()
		alias = first
		n, err := p.expectName()
		if err != nil {
			return nil, err
		}
		name = n
	}
	var args []gqlArg
	if p.isPunct("(") {
		a, err := p.parseArguments()
		if err != nil {
			return nil, err
		}
		args = a
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	var sels []*gqlSelection
	if p.isPunct("{") {
		s, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		sels = s
	}
	return &gqlSelection{kind: "field", alias: alias, name: name, args: args, selections: sels}, nil
}

func (p *gqlParser) parseArguments() ([]gqlArg, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	var args []gqlArg
	for !p.isPunct(")") {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue(false)
		if err != nil {
			return nil, err
		}
		args = append(args, gqlArg{name: name, value: v})
	}
	if err := p.expectPunct(")"); err != nil {
		return nil, err
	}
	return args, nil
}

func (p *gqlParser) parseValue(isConst bool) (*valueNode, error) {
	token := p.next()
	if token.kind == tokPunct && token.value == "$" {
		if isConst {
			return nil, fmt.Errorf("Variable in constant value")
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return &valueNode{kind: "variable", name: name}, nil
	}
	if token.kind == tokPunct && token.value == "[" {
		var values []*valueNode
		for !p.isPunct("]") {
			v, err := p.parseValue(isConst)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
		if err := p.expectPunct("]"); err != nil {
			return nil, err
		}
		return &valueNode{kind: "list", values: values}, nil
	}
	if token.kind == tokPunct && token.value == "{" {
		var fields []valueField
		for !p.isPunct("}") {
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue(isConst)
			if err != nil {
				return nil, err
			}
			fields = append(fields, valueField{name: name, value: v})
		}
		if err := p.expectPunct("}"); err != nil {
			return nil, err
		}
		return &valueNode{kind: "object", fields: fields}, nil
	}
	switch token.kind {
	case tokInt:
		return &valueNode{kind: "int", num: token.num}, nil
	case tokFloat:
		return &valueNode{kind: "float", num: token.num}, nil
	case tokString:
		return &valueNode{kind: "string", str: token.value}, nil
	case tokName:
		switch token.value {
		case "true":
			return &valueNode{kind: "boolean", b: true}, nil
		case "false":
			return &valueNode{kind: "boolean", b: false}, nil
		case "null":
			return &valueNode{kind: "null"}, nil
		default:
			return &valueNode{kind: "enum", str: token.value}, nil
		}
	}
	return nil, fmt.Errorf("Unexpected value token %s", tokenKindName(token.kind))
}

// ---------------------------------------------------------------- values

type gqlVariables map[string]any

func resolveValue(node *valueNode, variables gqlVariables) any {
	switch node.kind {
	case "variable":
		if v, ok := variables[node.name]; ok {
			return v
		}
		return nil
	case "int", "float":
		return node.num
	case "string":
		return node.str
	case "boolean":
		return node.b
	case "null":
		return nil
	case "enum":
		return node.str
	case "list":
		out := make([]any, 0, len(node.values))
		for _, v := range node.values {
			out = append(out, resolveValue(v, variables))
		}
		return out
	case "object":
		out := map[string]any{}
		for _, f := range node.fields {
			out[f.name] = resolveValue(f.value, variables)
		}
		return out
	}
	return nil
}

func argsOf(sel *gqlSelection, variables gqlVariables) map[string]any {
	out := map[string]any{}
	for _, arg := range sel.args {
		out[arg.name] = resolveValue(arg.value, variables)
	}
	return out
}

// ---------------------------------------------------------------- schema

type schemaField struct {
	name string
}

type schemaType struct {
	kind       string // OBJECT | ENUM | INPUT_OBJECT | SCALAR
	fields     []schemaField
	enumValues []string
	inputFields []schemaField
}

var gqlSchemaOrder = []string{
	"Query", "Mutation", "Repository", "User", "Ref", "Issue", "IssueConnection", "IssueEdge",
	"PageInfo", "PullRequest", "PullRequestConnection", "PullRequestEdge", "PullRequestChangedFile",
	"PullRequestChangedFileConnection", "Comment", "CommentEdge", "Label", "LabelConnection",
	"IssueState", "PullRequestState", "MergeableState", "RepositoryPermission", "IssueOrderField",
	"OrderDirection", "SearchType", "PullRequestMergeMethod", "CreateIssueInput", "CloseIssueInput",
	"CreatePullRequestInput", "MergePullRequestInput", "AddCommentInput", "IssueOrder",
}

var gqlSchema = map[string]*schemaType{
	"Query": {kind: "OBJECT", fields: []schemaField{{"viewer"}, {"repository"}, {"node"}, {"__type"}, {"__schema"}}},
	"Mutation": {kind: "OBJECT", fields: []schemaField{{"createIssue"}, {"closeIssue"}, {"createPullRequest"}, {"mergePullRequest"}, {"addComment"}}},
	"Repository": {kind: "OBJECT", fields: []schemaField{{"id"}, {"databaseId"}, {"name"}, {"nameWithOwner"}, {"owner"}, {"defaultBranchRef"}, {"viewerPermission"}, {"hasIssuesEnabled"}, {"issue"}, {"issues"}, {"pullRequests"}, {"pullRequest"}}},
	"User": {kind: "OBJECT", fields: []schemaField{{"login"}, {"id"}, {"name"}, {"email"}}},
	"Ref":  {kind: "OBJECT", fields: []schemaField{{"name"}}},
	"Issue": {kind: "OBJECT", fields: []schemaField{{"id"}, {"number"}, {"title"}, {"state"}, {"body"}, {"createdAt"}, {"labels"}, {"comments"}}},
	"IssueConnection": {kind: "OBJECT", fields: []schemaField{{"nodes"}, {"edges"}, {"pageInfo"}, {"totalCount"}}},
	"IssueEdge": {kind: "OBJECT", fields: []schemaField{{"node"}, {"cursor"}}},
	"PageInfo": {kind: "OBJECT", fields: []schemaField{{"hasNextPage"}, {"hasPreviousPage"}, {"startCursor"}, {"endCursor"}}},
	"PullRequest": {kind: "OBJECT", fields: []schemaField{{"id"}, {"number"}, {"title"}, {"body"}, {"state"}, {"merged"}, {"headRefName"}, {"baseRefName"}, {"mergeable"}, {"files"}, {"createdAt"}}},
	"PullRequestConnection": {kind: "OBJECT", fields: []schemaField{{"nodes"}, {"edges"}, {"pageInfo"}, {"totalCount"}}},
	"PullRequestEdge": {kind: "OBJECT", fields: []schemaField{{"node"}, {"cursor"}}},
	"PullRequestChangedFile": {kind: "OBJECT", fields: []schemaField{{"path"}, {"additions"}, {"deletions"}, {"changes"}}},
	"PullRequestChangedFileConnection": {kind: "OBJECT", fields: []schemaField{{"nodes"}, {"edges"}, {"pageInfo"}, {"totalCount"}}},
	"Comment": {kind: "OBJECT", fields: []schemaField{{"id"}, {"body"}, {"createdAt"}}},
	"CommentEdge": {kind: "OBJECT", fields: []schemaField{{"node"}, {"cursor"}}},
	"Label": {kind: "OBJECT", fields: []schemaField{{"name"}, {"color"}, {"description"}}},
	"LabelConnection": {kind: "OBJECT", fields: []schemaField{{"nodes"}, {"edges"}, {"pageInfo"}, {"totalCount"}}},
	"IssueState": {kind: "ENUM", enumValues: []string{"OPEN", "CLOSED"}},
	"PullRequestState": {kind: "ENUM", enumValues: []string{"OPEN", "CLOSED", "MERGED"}},
	"MergeableState": {kind: "ENUM", enumValues: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	"RepositoryPermission": {kind: "ENUM", enumValues: []string{"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}},
	"IssueOrderField": {kind: "ENUM", enumValues: []string{"CREATED_AT", "UPDATED_AT", "COMMENTS"}},
	"OrderDirection": {kind: "ENUM", enumValues: []string{"ASC", "DESC"}},
	"SearchType": {kind: "ENUM", enumValues: []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "MILESTONE"}},
	"PullRequestMergeMethod": {kind: "ENUM", enumValues: []string{"MERGE", "SQUASH", "REBASE"}},
	"CreateIssueInput": {kind: "INPUT_OBJECT", inputFields: []schemaField{{"repositoryId"}, {"title"}, {"body"}}},
	"CloseIssueInput": {kind: "INPUT_OBJECT", inputFields: []schemaField{{"issueId"}}},
	"CreatePullRequestInput": {kind: "INPUT_OBJECT", inputFields: []schemaField{{"repositoryId"}, {"baseRefName"}, {"headRefName"}, {"title"}, {"body"}}},
	"MergePullRequestInput": {kind: "INPUT_OBJECT", inputFields: []schemaField{{"pullRequestId"}, {"mergeMethod"}}},
	"AddCommentInput": {kind: "INPUT_OBJECT", inputFields: []schemaField{{"subjectId"}, {"body"}}},
	"IssueOrder": {kind: "INPUT_OBJECT", inputFields: []schemaField{{"field"}, {"direction"}}},
}

// ---------------------------------------------------------------- execution

// gqlSource is one GraphQL object value: its type plus its fields.
type gqlSource struct {
	typename string
	data     map[string]any
}

func gqlSrc(typename string) *gqlSource {
	return &gqlSource{typename: typename, data: map[string]any{}}
}

func (s *gqlSource) set(key string, value any) *gqlSource {
	s.data[key] = value
	return s
}

func cursorFor(index int) string {
	return b64urlEncode([]byte("cursor:" + itoa(index)))
}

func cursorIndex(cursor any) int {
	s, ok := cursor.(string)
	if !ok {
		return -1
	}
	text, ok := b64urlDecodeStr(s)
	if !ok {
		return -1
	}
	m := regexp.MustCompile(`^cursor:(\d+)$`).FindStringSubmatch(text)
	if m == nil {
		return -1
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func connectionOf(items []any, args map[string]any, itemType string) *gqlSource {
	first := len(items)
	if f, ok := args["first"].(float64); ok {
		first = int(f)
	}
	after := cursorIndex(args["after"])
	start := after + 1
	if start < 0 {
		start = 0
	}
	end := start + first
	if end > len(items) {
		end = len(items)
	}
	if end < start {
		end = start
	}
	page := items[start:end]
	var endCursor any
	if len(page) == 0 {
		endCursor = nil
	} else {
		endCursor = cursorFor(start + len(page) - 1)
	}
	edges := make([]any, 0, len(page))
	for i, node := range page {
		edges = append(edges, gqlSrc(itemType+"Edge").set("node", node).set("cursor", cursorFor(start+i)))
	}
	var startCursor any
	if len(page) == 0 {
		startCursor = nil
	} else {
		startCursor = cursorFor(start)
	}
	pageInfo := gqlSrc("PageInfo").
		set("hasNextPage", start+len(page) < len(items)).
		set("hasPreviousPage", start > 0).
		set("startCursor", startCursor).
		set("endCursor", endCursor)
	return gqlSrc(itemType + "Connection").
		set("nodes", page).
		set("edges", edges).
		set("pageInfo", pageInfo).
		set("totalCount", len(items))
}

func repoSource(repo *Repo) *gqlSource {
	return gqlSrc("Repository").set("repo", repo)
}

func issueSource(repo *Repo, issue *Issue) *gqlSource {
	return gqlSrc("Issue").set("repo", repo).set("issue", issue)
}

func pullSource(repo *Repo, pull *Pull) *gqlSource {
	return gqlSrc("PullRequest").set("repo", repo).set("pull", pull)
}

func issueState(issue *Issue) string {
	if issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func pullState(pull *Pull) string {
	if pull.Merged {
		return "MERGED"
	}
	if pull.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

type foundByID struct {
	kind      string
	repo      *Repo
	issue     *Issue
	pull      *Pull
	commentID int
}

func findByGlobalID(s *Store, id string) *foundByID {
	decoded := decodeGlobalID(id)
	if decoded == nil {
		return nil
	}
	repo := s.GetRepo(decoded.owner, decoded.name)
	if repo == nil {
		return nil
	}
	switch decoded.kind {
	case "repo":
		return &foundByID{kind: "repo", repo: repo}
	case "issue":
		issue := FindIssue(repo, decoded.number)
		if issue == nil {
			return nil
		}
		return &foundByID{kind: "issue", repo: repo, issue: issue}
	case "pull":
		pull := FindPull(repo, decoded.number)
		if pull == nil {
			return nil
		}
		return &foundByID{kind: "pull", repo: repo, pull: pull}
	default:
		return &foundByID{kind: "comment", repo: repo, commentID: decoded.number}
	}
}

type gqlFieldResult struct {
	value    any
	typeName string
	ok       bool
}

func fieldResult(value any, typeName string) gqlFieldResult {
	return gqlFieldResult{value: value, typeName: typeName, ok: true}
}

func resolveField(s *Store, typeName, field string, source *gqlSource, args map[string]any) gqlFieldResult {
	switch typeName {
	case "Query":
		switch field {
		case "viewer":
			return fieldResult(gqlSrc("User").set("login", "adaiversen").set("id", 1000).set("name", "ada iversen").set("email", "adaiversen@example.invalid"), "User")
		case "repository":
			owner, _ := args["owner"].(string)
			name, _ := args["name"].(string)
			if !strings.EqualFold(owner, "adaiversen") {
				return fieldResult(nil, "Repository")
			}
			repo := s.GetRepo(owner, name)
			if repo == nil {
				return fieldResult(nil, "Repository")
			}
			return fieldResult(repoSource(repo), "Repository")
		case "node":
			id := ""
			if v, ok := args["id"].(string); ok {
				id = v
			}
			found := findByGlobalID(s, id)
			if found == nil {
				return fieldResult(nil, "")
			}
			switch found.kind {
			case "repo":
				return fieldResult(repoSource(found.repo), "Repository")
			case "issue":
				return fieldResult(issueSource(found.repo, found.issue), "Issue")
			case "pull":
				return fieldResult(pullSource(found.repo, found.pull), "PullRequest")
			default:
				return fieldResult(nil, "")
			}
		case "__type":
			name := ""
			if v, ok := args["name"].(string); ok {
				name = v
			}
			typ, ok := gqlSchema[name]
			if !ok {
				return fieldResult(nil, "__Type")
			}
			return fieldResult(introspectionType(name, typ), "__Type")
		case "__schema":
			types := make([]any, 0, len(gqlSchemaOrder))
			for _, name := range gqlSchemaOrder {
				types = append(types, introspectionType(name, gqlSchema[name]))
			}
			return fieldResult(gqlSrc("__Schema").
				set("queryType", gqlSrc("__Type").set("name", "Query")).
				set("mutationType", gqlSrc("__Type").set("name", "Mutation")).
				set("types", types), "__Schema")
		}
		return gqlFieldResult{}
	case "Mutation":
		switch field {
		case "createIssue":
			input, _ := args["input"].(map[string]any)
			repoID := ""
			if v, ok := input["repositoryId"].(string); ok {
				repoID = v
			}
			found := findByGlobalID(s, repoID)
			if found == nil || found.kind != "repo" {
				return fieldResult(gqlSrc("CreateIssuePayload").set("issue", nil), "CreateIssuePayload")
			}
			repo := found.repo
			title := ""
			if v, ok := input["title"].(string); ok {
				title = v
			}
			if title == "" {
				return fieldResult(gqlSrc("CreateIssuePayload").set("issue", nil), "CreateIssuePayload")
			}
			seq := s.Seq()
			body := ""
			if v, ok := input["body"].(string); ok {
				body = v
			}
			issue := &Issue{
				ID: s.NextGlobalID(), Number: repo.NextNumber, Title: title, Body: body,
				State: "open", Labels: []string{}, Comments: []*Comment{},
				CreatedSeq: seq, UpdatedSeq: seq,
			}
			repo.NextNumber++
			repo.Issues = append(repo.Issues, issue)
			return fieldResult(gqlSrc("CreateIssuePayload").set("issue", issueSource(repo, issue)), "CreateIssuePayload")
		case "closeIssue":
			input, _ := args["input"].(map[string]any)
			issueID := ""
			if v, ok := input["issueId"].(string); ok {
				issueID = v
			}
			found := findByGlobalID(s, issueID)
			if found == nil || found.kind != "issue" || found.issue == nil {
				return fieldResult(gqlSrc("CloseIssuePayload").set("issue", nil), "CloseIssuePayload")
			}
			found.issue.State = "closed"
			found.issue.UpdatedSeq = s.Seq()
			invalidateIssue(found.issue)
			return fieldResult(gqlSrc("CloseIssuePayload").set("issue", issueSource(found.repo, found.issue)), "CloseIssuePayload")
		case "createPullRequest":
			input, _ := args["input"].(map[string]any)
			repoID := ""
			if v, ok := input["repositoryId"].(string); ok {
				repoID = v
			}
			found := findByGlobalID(s, repoID)
			if found == nil || found.kind != "repo" {
				return fieldResult(gqlSrc("CreatePullRequestPayload").set("pullRequest", nil), "CreatePullRequestPayload")
			}
			title, _ := input["title"].(string)
			body, _ := input["body"].(string)
			head, _ := input["headRefName"].(string)
			base, _ := input["baseRefName"].(string)
			pull, _ := createPullRecord(s, found.repo, pullInput{title: title, body: body, head: head, base: base})
			var pr any
			if pull != nil {
				pr = pullSource(found.repo, pull)
			}
			return fieldResult(gqlSrc("CreatePullRequestPayload").set("pullRequest", pr), "CreatePullRequestPayload")
		case "mergePullRequest":
			input, _ := args["input"].(map[string]any)
			prID := ""
			if v, ok := input["pullRequestId"].(string); ok {
				prID = v
			}
			found := findByGlobalID(s, prID)
			if found == nil || found.kind != "pull" || found.pull == nil {
				return fieldResult(gqlSrc("MergePullRequestPayload").set("pullRequest", nil), "MergePullRequestPayload")
			}
			method := "squash"
			if v, ok := input["mergeMethod"].(string); ok {
				method = strings.ToLower(v)
			}
			mergePullRecord(s, found.repo, found.pull, method)
			return fieldResult(gqlSrc("MergePullRequestPayload").set("pullRequest", pullSource(found.repo, found.pull)), "MergePullRequestPayload")
		case "addComment":
			input, _ := args["input"].(map[string]any)
			subjectID := ""
			if v, ok := input["subjectId"].(string); ok {
				subjectID = v
			}
			body, _ := input["body"].(string)
			found := findByGlobalID(s, subjectID)
			if found == nil || (found.kind != "issue" && found.kind != "pull") || body == "" {
				return fieldResult(gqlSrc("AddCommentPayload").set("commentEdge", nil), "AddCommentPayload")
			}
			repo := found.repo
			comment := &Comment{ID: repo.NextCommentID, Body: body, Author: "adaiversen", CreatedSeq: s.Seq()}
			repo.NextCommentID++
			if found.kind == "issue" && found.issue != nil {
				found.issue.Comments = append(found.issue.Comments, comment)
				found.issue.UpdatedSeq = s.Seq()
				invalidateIssue(found.issue)
			} else if found.kind == "pull" && found.pull != nil {
				found.pull.Comments = append(found.pull.Comments, comment)
				found.pull.UpdatedSeq = s.Seq()
				invalidatePull(found.pull)
			}
			node := gqlSrc("Comment").set("id", commentGlobalID(repo, comment.ID)).set("body", body).set("createdAt", isoOfSeq(comment.CreatedSeq))
			return fieldResult(gqlSrc("AddCommentPayload").set("commentEdge", gqlSrc("CommentEdge").set("node", node).set("cursor", cursorFor(0))), "AddCommentPayload")
		}
		return gqlFieldResult{}
	case "Repository":
		repo := source.data["repo"].(*Repo)
		switch field {
		case "id":
			return fieldResult(repoGlobalID(repo), "")
		case "databaseId":
			return fieldResult(float64(repo.ID), "")
		case "name":
			return fieldResult(repo.Name, "")
		case "nameWithOwner":
			return fieldResult(repo.Owner+"/"+repo.Name, "")
		case "owner":
			return fieldResult(gqlSrc("User").set("login", repo.Owner).set("id", 1000).set("name", "ada iversen").set("email", "adaiversen@example.invalid"), "User")
		case "defaultBranchRef":
			tip := s.HeadTip(repo)
			if tip == "" {
				return fieldResult(nil, "Ref")
			}
			return fieldResult(gqlSrc("Ref").set("name", repo.DefaultBranch), "Ref")
		case "viewerPermission":
			return fieldResult("ADMIN", "")
		case "hasIssuesEnabled":
			return fieldResult(true, "")
		case "issue":
			number := 0
			if n, ok := args["number"].(float64); ok {
				number = int(n)
			}
			issue := FindIssue(repo, number)
			if issue == nil {
				return fieldResult(nil, "Issue")
			}
			return fieldResult(issueSource(repo, issue), "Issue")
		case "issues":
			issues := append([]*Issue{}, repo.Issues...)
			var states []string
			if arr, ok := args["states"].([]any); ok {
				for _, v := range arr {
					if str, ok := v.(string); ok {
						states = append(states, str)
					}
				}
			}
			if states != nil {
				wanted := map[string]bool{}
				for _, st := range states {
					wanted[strings.ToLower(st)] = true
				}
				filtered := issues[:0]
				for _, issue := range issues {
					if wanted[issue.State] {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			}
			orderBy, _ := args["orderBy"].(map[string]any)
			orderField := "CREATED_AT"
			if v, ok := orderBy["field"].(string); ok {
				orderField = v
			}
			orderDirection := "DESC"
			if v, ok := orderBy["direction"].(string); ok {
				orderDirection = v
			}
			key := func(issue *Issue) int {
				switch orderField {
				case "UPDATED_AT":
					return issue.UpdatedSeq
				case "COMMENTS":
					return len(issue.Comments)
				default:
					return issue.CreatedSeq
				}
			}
			sortIssues(issues, key, orderDirection == "ASC")
			items := make([]any, 0, len(issues))
			for _, issue := range issues {
				items = append(items, issueSource(repo, issue))
			}
			return fieldResult(connectionOf(items, args, "Issue"), "IssueConnection")
		case "pullRequests":
			items := make([]any, 0, len(repo.Pulls))
			for _, pull := range repo.Pulls {
				items = append(items, pullSource(repo, pull))
			}
			return fieldResult(connectionOf(items, args, "PullRequest"), "PullRequestConnection")
		case "pullRequest":
			number := 0
			if n, ok := args["number"].(float64); ok {
				number = int(n)
			}
			pull := FindPull(repo, number)
			if pull == nil {
				return fieldResult(nil, "PullRequest")
			}
			return fieldResult(pullSource(repo, pull), "PullRequest")
		}
		return gqlFieldResult{}
	case "Issue":
		issue := source.data["issue"].(*Issue)
		repo := source.data["repo"].(*Repo)
		switch field {
		case "id":
			return fieldResult(issueGlobalID(repo, issue), "")
		case "number":
			return fieldResult(float64(issue.Number), "")
		case "title":
			return fieldResult(issue.Title, "")
		case "state":
			return fieldResult(issueState(issue), "")
		case "body":
			return fieldResult(issue.Body, "")
		case "createdAt":
			return fieldResult(isoOfSeq(issue.CreatedSeq), "")
		case "labels":
			nodes := make([]any, 0, len(issue.Labels))
			for _, name := range issue.Labels {
				nodes = append(nodes, gqlSrc("Label").set("name", name).set("color", "ededed").set("description", ""))
			}
			return fieldResult(connectionOf(nodes, args, "Label"), "LabelConnection")
		case "comments":
			nodes := make([]any, 0, len(issue.Comments))
			for _, comment := range issue.Comments {
				nodes = append(nodes, gqlSrc("Comment").set("id", commentGlobalID(repo, comment.ID)).set("body", comment.Body).set("createdAt", isoOfSeq(comment.CreatedSeq)))
			}
			return fieldResult(connectionOf(nodes, args, "Comment"), "CommentConnection")
		}
		return gqlFieldResult{}
	case "PullRequest":
		pull := source.data["pull"].(*Pull)
		repo := source.data["repo"].(*Repo)
		switch field {
		case "id":
			return fieldResult(pullGlobalID(repo, pull), "")
		case "number":
			return fieldResult(float64(pull.Number), "")
		case "title":
			return fieldResult(pull.Title, "")
		case "body":
			return fieldResult(pull.Body, "")
		case "state":
			return fieldResult(pullState(pull), "")
		case "merged":
			return fieldResult(pull.Merged, "")
		case "headRefName":
			return fieldResult(pull.HeadRef, "")
		case "baseRefName":
			return fieldResult(pull.BaseRef, "")
		case "mergeable":
			m := pullMergeable(s, repo, pull)
			if m == nil {
				return fieldResult("UNKNOWN", "")
			}
			if m.(bool) {
				return fieldResult("MERGEABLE", "")
			}
			return fieldResult("CONFLICTING", "")
		case "createdAt":
			return fieldResult(isoOfSeq(pull.CreatedSeq), "")
		case "files":
			files := pullFiles(s, repo, pull)
			items := make([]any, 0, len(files))
			for _, f := range files {
				items = append(items, gqlSrc("PullRequestChangedFile").set("path", f.Filename).set("additions", float64(f.Additions)).set("deletions", float64(f.Deletions)).set("changes", float64(f.Changes)))
			}
			return fieldResult(connectionOf(items, args, "PullRequestChangedFile"), "PullRequestChangedFileConnection")
		}
		return gqlFieldResult{}
	case "IssueConnection", "PullRequestConnection", "PullRequestChangedFileConnection", "LabelConnection", "CommentConnection":
		switch field {
		case "nodes":
			return fieldResult(source.data["nodes"], singularOf(typeName))
		case "edges":
			return fieldResult(source.data["edges"], singularOf(typeName)+"Edge")
		case "pageInfo":
			return fieldResult(source.data["pageInfo"], "PageInfo")
		case "totalCount":
			return fieldResult(source.data["totalCount"], "")
		}
		return gqlFieldResult{}
	case "IssueEdge", "PullRequestEdge", "CommentEdge", "PullRequestChangedFileEdge":
		switch field {
		case "node":
			return fieldResult(source.data["node"], singularOf(strings.Replace(typeName, "Edge", "Connection", 1)))
		case "cursor":
			return fieldResult(source.data["cursor"], "")
		}
		return gqlFieldResult{}
	case "PageInfo":
		switch field {
		case "hasNextPage":
			return fieldResult(source.data["hasNextPage"], "")
		case "hasPreviousPage":
			return fieldResult(source.data["hasPreviousPage"], "")
		case "startCursor":
			return fieldResult(source.data["startCursor"], "")
		case "endCursor":
			return fieldResult(source.data["endCursor"], "")
		}
		return gqlFieldResult{}
	case "User":
		switch field {
		case "login":
			return fieldResult(source.data["login"], "")
		case "id":
			return fieldResult(source.data["id"], "")
		case "name":
			return fieldResult(source.data["name"], "")
		case "email":
			return fieldResult(source.data["email"], "")
		}
		return gqlFieldResult{}
	case "Ref":
		if field == "name" {
			return fieldResult(source.data["name"], "")
		}
		return gqlFieldResult{}
	case "Comment":
		switch field {
		case "id":
			return fieldResult(source.data["id"], "")
		case "body":
			return fieldResult(source.data["body"], "")
		case "createdAt":
			return fieldResult(source.data["createdAt"], "")
		}
		return gqlFieldResult{}
	case "PullRequestChangedFile":
		switch field {
		case "path":
			return fieldResult(source.data["path"], "")
		case "additions":
			return fieldResult(source.data["additions"], "")
		case "deletions":
			return fieldResult(source.data["deletions"], "")
		case "changes":
			return fieldResult(source.data["changes"], "")
		}
		return gqlFieldResult{}
	case "Label":
		switch field {
		case "name":
			return fieldResult(source.data["name"], "")
		case "color":
			return fieldResult(source.data["color"], "")
		case "description":
			return fieldResult(source.data["description"], "")
		}
		return gqlFieldResult{}
	case "__Type":
		switch field {
		case "name":
			return fieldResult(source.data["name"], "")
		case "kind":
			return fieldResult(source.data["kind"], "")
		case "description":
			return fieldResult(nil, "")
		case "fields":
			fields, ok := source.data["fields"].([]schemaField)
			if !ok || fields == nil {
				return fieldResult(nil, "__Field")
			}
			out := make([]any, 0, len(fields))
			for _, f := range fields {
				out = append(out, gqlSrc("__Field").set("name", f.name).set("description", nil).set("args", []any{}).set("type", nil))
			}
			return fieldResult(out, "__Field")
		case "enumValues":
			values, ok := source.data["enumValues"].([]string)
			if !ok || values == nil {
				return fieldResult(nil, "__EnumValue")
			}
			out := make([]any, 0, len(values))
			for _, name := range values {
				out = append(out, gqlSrc("__EnumValue").set("name", name).set("description", nil))
			}
			return fieldResult(out, "__EnumValue")
		case "inputFields":
			fields, ok := source.data["inputFields"].([]schemaField)
			if !ok || fields == nil {
				return fieldResult(nil, "__InputValue")
			}
			out := make([]any, 0, len(fields))
			for _, f := range fields {
				out = append(out, gqlSrc("__InputValue").set("name", f.name).set("description", nil).set("type", nil))
			}
			return fieldResult(out, "__InputValue")
		}
		return gqlFieldResult{}
	case "__Schema":
		switch field {
		case "queryType":
			return fieldResult(source.data["queryType"], "__Type")
		case "mutationType":
			return fieldResult(source.data["mutationType"], "__Type")
		case "types":
			return fieldResult(source.data["types"], "__Type")
		}
		return gqlFieldResult{}
	case "__Field", "__EnumValue", "__InputValue":
		switch field {
		case "name":
			return fieldResult(source.data["name"], "")
		case "description":
			return fieldResult(nil, "")
		case "args":
			return fieldResult([]any{}, "")
		case "type":
			return fieldResult(nil, "")
		}
		return gqlFieldResult{}
	case "CreateIssuePayload", "CloseIssuePayload":
		if field == "issue" {
			return fieldResult(source.data["issue"], "Issue")
		}
		return gqlFieldResult{}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field == "pullRequest" {
			return fieldResult(source.data["pullRequest"], "PullRequest")
		}
		return gqlFieldResult{}
	case "AddCommentPayload":
		if field == "commentEdge" {
			return fieldResult(source.data["commentEdge"], "CommentEdge")
		}
		return gqlFieldResult{}
	}
	return gqlFieldResult{}
}

func sortIssues(issues []*Issue, key func(*Issue) int, asc bool) {
	sort.SliceStable(issues, func(i, j int) bool {
		if asc {
			return key(issues[i]) < key(issues[j])
		}
		return key(issues[i]) > key(issues[j])
	})
}

func singularOf(connectionType string) string {
	switch connectionType {
	case "IssueConnection":
		return "Issue"
	case "PullRequestConnection":
		return "PullRequest"
	case "PullRequestChangedFileConnection":
		return "PullRequestChangedFile"
	case "LabelConnection":
		return "Label"
	case "CommentConnection":
		return "Comment"
	default:
		return connectionType
	}
}

func introspectionType(name string, typ *schemaType) *gqlSource {
	src := gqlSrc("__Type").set("name", name).set("kind", typ.kind).set("description", nil)
	if typ.fields != nil {
		src.set("fields", typ.fields)
	} else {
		src.set("fields", nil)
	}
	if typ.enumValues != nil {
		src.set("enumValues", typ.enumValues)
	} else {
		src.set("enumValues", nil)
	}
	if typ.inputFields != nil {
		src.set("inputFields", typ.inputFields)
	} else {
		src.set("inputFields", nil)
	}
	return src
}

func executeSelections(selections []*gqlSelection, typeName string, source *gqlSource, s *Store, fragments map[string]*gqlFragment, variables gqlVariables, errors *[]map[string]any) map[string]any {
	out := map[string]any{}
	for _, sel := range selections {
		switch sel.kind {
		case "field":
			key := sel.name
			if sel.alias != "" {
				key = sel.alias
			}
			args := argsOf(sel, variables)
			resolved := resolveField(s, typeName, sel.name, source, args)
			if !resolved.ok {
				*errors = append(*errors, map[string]any{"message": fmt.Sprintf("Cannot query field %q on type %q.", sel.name, typeName)})
				out[key] = nil
				continue
			}
			value, valueType := resolved.value, resolved.typeName
			if len(sel.selections) == 0 || value == nil || valueType == "" {
				out[key] = marshalValue(value)
			} else if arr, ok := value.([]any); ok {
				mapped := make([]any, 0, len(arr))
				for _, item := range arr {
					if src, ok := item.(*gqlSource); ok {
						mapped = append(mapped, executeSelections(sel.selections, valueType, src, s, fragments, variables, errors))
					} else {
						mapped = append(mapped, item)
					}
				}
				out[key] = mapped
			} else if src, ok := value.(*gqlSource); ok {
				out[key] = executeSelections(sel.selections, valueType, src, s, fragments, variables, errors)
			} else {
				out[key] = marshalValue(value)
			}
		case "fragmentSpread":
			if frag, ok := fragments[sel.name]; ok && (frag.typeCondition == typeName || frag.typeCondition == source.typename) {
				for k, v := range executeSelections(frag.selections, typeName, source, s, fragments, variables, errors) {
					out[k] = v
				}
			}
		case "inlineFragment":
			if sel.typeCondition == "" || sel.typeCondition == typeName || sel.typeCondition == source.typename {
				for k, v := range executeSelections(sel.selections, typeName, source, s, fragments, variables, errors) {
					out[k] = v
				}
			}
		}
	}
	return out
}

// marshalValue converts an internal value to its JSON form. gqlSource values
// without sub-selections render as their __typename only (an invalid query
// shape the suite never sends).
func marshalValue(v any) any {
	if src, ok := v.(*gqlSource); ok {
		return map[string]any{"__typename": src.typename}
	}
	return v
}

func executeDocument(doc *gqlDocument, operationName *string, variables gqlVariables, s *Store) map[string]any {
	var operation *gqlOperation
	if len(doc.operations) > 0 {
		operation = doc.operations[0]
	}
	if operationName != nil {
		for _, op := range doc.operations {
			if op.name == *operationName {
				operation = op
				break
			}
		}
	}
	if operation == nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "No operation provided"}}}
	}
	merged := gqlVariables{}
	for k, v := range variables {
		merged[k] = v
	}
	for _, def := range operation.variables {
		if _, ok := merged[def.name]; !ok && def.defaultValue != nil {
			merged[def.name] = resolveValue(def.defaultValue, merged)
		}
	}
	var errors []map[string]any
	rootType := "Query"
	if operation.typ == "mutation" {
		rootType = "Mutation"
	}
	root := gqlSrc(rootType)
	data := executeSelections(operation.selections, rootType, root, s, doc.fragments, merged, &errors)
	if len(errors) > 0 {
		errList := make([]any, 0, len(errors))
		for _, e := range errors {
			errList = append(errList, e)
		}
		return map[string]any{"data": data, "errors": errList}
	}
	return map[string]any{"data": data}
}

// ---------------------------------------------------------------- HTTP

// handleGraphQL handles POST /api/graphql. Returns true when the path matched.
func handleGraphQL(ctx *Ctx, w http.ResponseWriter) bool {
	if ctx.Path != "/api/graphql" {
		return false
	}
	if ctx.Method != "POST" {
		ctx.sendError(w, 404, "Not Found")
		return true
	}
	if !ctx.isAuthorized() {
		ctx.sendError(w, 401, "Requires authentication")
		return true
	}
	parsed, ok, _ := ctx.parseJSONBody()
	if !ok {
		ctx.sendJSON(w, 400, map[string]any{"errors": []any{map[string]any{"message": "Request body must be valid JSON"}}}, nil)
		return true
	}
	runOne := func(entry any) map[string]any {
		if entry == nil || !isRecord(entry) {
			return map[string]any{"errors": []any{map[string]any{"message": "Operation must be an object"}}}
		}
		record := entry.(map[string]any)
		query, _ := record["query"].(string)
		variables := gqlVariables{}
		if v, ok := record["variables"].(map[string]any); ok {
			variables = v
		}
		var operationName *string
		if v, ok := record["operationName"].(string); ok {
			operationName = &v
		}
		tokens, err := lex(query)
		if err != nil {
			return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
		}
		parser := &gqlParser{tokens: tokens}
		doc, err := parser.parseDocument()
		if err != nil {
			return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
		}
		return executeDocument(doc, operationName, variables, ctx.Store)
	}
	if arr, ok := parsed.([]any); ok {
		out := make([]any, 0, len(arr))
		for _, entry := range arr {
			out = append(out, runOne(entry))
		}
		ctx.sendJSON(w, 200, out, nil)
		return true
	}
	ctx.sendJSON(w, 200, runOne(parsed), nil)
	return true
}
