// Git smart HTTP: info/refs advertisements (protocol v0 and v2),
// git-upload-pack (fetch and ls-refs, v0 and v2) and git-receive-pack
// (push with report-status over sideband). Mirrors src/git-http.ts.
package main

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const uploadAd = "application/x-git-upload-pack-advertisement"
const receiveAd = "application/x-git-receive-pack-advertisement"
const uploadResult = "application/x-git-upload-pack-result"
const receiveResult = "application/x-git-receive-pack-result"

const agent = "gitforge/1.0"
const uploadV0Caps = "symref=HEAD:refs/heads/main side-band-64k thin-pack ofs-delta multi_ack_detailed shallow agent=gitforge/1.0"
const receiveV0Caps = "report-status side-band-64k delete-refs ofs-delta agent=gitforge/1.0"

// parseCaps parses a space-separated capability list into key -> value ("" for bare keys).
func parseCaps(text string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(text, " ") {
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			out[part] = ""
		} else {
			out[part[:eq]] = part[eq+1:]
		}
	}
	return out
}

type gitPath struct {
	owner string
	repo  string
	rest  string
}

// parseGitPath extracts the route's owner/repo from /:owner/:repo.git/... or /:owner/:repo/...
func parseGitPath(path string) *gitPath {
	parts := splitSegments(path)
	if len(parts) < 3 {
		return nil
	}
	owner := parts[0]
	repo := parts[1]
	if strings.HasSuffix(repo, ".git") {
		repo = repo[:len(repo)-4]
	}
	rest := strings.Join(parts[2:], "/")
	if owner == "" || repo == "" || rest == "" {
		return nil
	}
	return &gitPath{owner: owner, repo: repo, rest: rest}
}

// peelTag returns the target of an annotated tag object, or "".
func peelTag(data []byte) string {
	text := string(data)
	// /^object ([0-9a-f]{40})$/m
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "object ") && len(line) == 7+40 {
			cand := line[7:]
			if isOID(cand) {
				return cand
			}
		}
	}
	return ""
}

// advertiseV0 builds the v0 advertisement: service preamble, flush, ref lines, flush.
func advertiseV0(s *Store, repo *Repo, service string) []byte {
	caps := uploadV0Caps
	if service == "git-receive-pack" {
		caps = receiveV0Caps
	}
	var parts [][]byte
	parts = append(parts, pktLineStr(fmt.Sprintf("# service=%s\n", service)))
	parts = append(parts, pktFlush)
	refs := s.ListRefs(repo)
	var lines []string
	if len(refs) == 0 {
		lines = append(lines, fmt.Sprintf("%s capabilities^{}\x00%s\n", ZeroOID, caps))
	} else {
		headTip := s.HeadTip(repo)
		first := true
		if headTip != "" {
			lines = append(lines, fmt.Sprintf("%s HEAD\x00%s\n", headTip, caps))
			first = false
		}
		for _, ref := range refs {
			prefix := ""
			if first {
				prefix = "\x00" + caps
			}
			first = false
			lines = append(lines, fmt.Sprintf("%s %s%s\n", ref.OID, ref.Name, prefix))
			if strings.HasPrefix(ref.Name, "refs/tags/") {
				if object := s.GetObject(repo, ref.OID); object != nil && object.Type == ObjTag {
					if target := peelTag(object.Data); target != "" {
						lines = append(lines, fmt.Sprintf("%s %s^{}\n", target, ref.Name))
					}
				}
			}
		}
	}
	for _, line := range lines {
		parts = append(parts, pktLineStr(line))
	}
	parts = append(parts, pktFlush)
	return concatBytes(parts)
}

func concatBytes(parts [][]byte) []byte {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// advertiseV2 builds the v2 advertisement: `version 2` plus capability lines, then flush.
func advertiseV2() []byte {
	lines := []string{
		"version 2\n",
		"agent=" + agent + "\n",
		"ls-refs=unborn\n",
		"fetch=shallow thin-pack\n",
		"object-format=sha1\n",
	}
	var parts [][]byte
	for _, line := range lines {
		parts = append(parts, pktLineStr(line))
	}
	parts = append(parts, pktFlush)
	return concatBytes(parts)
}

// ---------------------------------------------------------------- ls-refs (v2)

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

func lsRefsV2(s *Store, repo *Repo, args lsRefsArgs) []byte {
	var parts [][]byte
	refs := s.ListRefs(repo)
	matches := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		for _, prefix := range args.prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	defaultRef := "refs/heads/" + repo.DefaultBranch
	if s.HeadTip(repo) != "" && matches("HEAD") {
		headTip := s.HeadTip(repo)
		attrs := ""
		if args.symrefs {
			attrs = " symref-target:" + defaultRef
		}
		parts = append(parts, pktLineStr(fmt.Sprintf("%s HEAD%s\n", headTip, attrs)))
	}
	for _, ref := range refs {
		if !matches(ref.Name) {
			continue
		}
		line := fmt.Sprintf("%s %s", ref.OID, ref.Name)
		if args.peel && strings.HasPrefix(ref.Name, "refs/tags/") {
			if object := s.GetObject(repo, ref.OID); object != nil && object.Type == ObjTag {
				if target := peelTag(object.Data); target != "" {
					line += " peeled:" + target
				}
			}
		}
		parts = append(parts, pktLineStr(line+"\n"))
	}
	parts = append(parts, pktFlush)
	return concatBytes(parts)
}

// ---------------------------------------------------------------- fetch

type fetchArgs struct {
	wants    []string
	haves    []string
	shallow  []string
	depth    int // -1 when absent
	thinPack bool
	features []string
	done     bool
}

func parseFetchArgs(lines []PktLine) fetchArgs {
	args := fetchArgs{depth: -1}
	for _, line := range lines {
		if line.Kind != PktData {
			continue
		}
		text := strings.TrimSpace(string(line.Data))
		switch {
		case strings.HasPrefix(text, "want "):
			rest := strings.TrimSpace(text[5:])
			space := strings.IndexByte(rest, ' ')
			if space < 0 {
				args.wants = append(args.wants, rest)
			} else {
				args.wants = append(args.wants, rest[:space])
				for _, feature := range strings.Fields(rest[space+1:]) {
					args.features = append(args.features, feature)
				}
			}
		case strings.HasPrefix(text, "have "):
			args.haves = append(args.haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			n, err := strconv.Atoi(strings.TrimSpace(text[7:]))
			if err == nil && n > 0 {
				args.depth = n
			}
		case text == "thin-pack":
			args.thinPack = true
		case text == "done":
			args.done = true
		}
	}
	return args
}

type fetchPlan struct {
	objects   []GitObject
	shallow   []string
	unshallow []string
}

// planFetch computes the objects a fetch sends: the wants' closure (limited
// to depth commit generations when given), minus what the client already
// holds (the haves' closure, stopping at the client's shallow boundary).
func planFetch(s *Store, repo *Repo, args fetchArgs) fetchPlan {
	extra := map[string]bool{}
	var roots []string
	for _, want := range args.wants {
		object := s.GetObject(repo, want)
		if object == nil {
			continue
		}
		if object.Type == ObjTag {
			extra[want] = true
			if target := peelTag(object.Data); target != "" {
				roots = append(roots, target)
			}
		} else if object.Type == ObjCommit {
			roots = append(roots, want)
		} else {
			extra[want] = true
		}
	}
	genOf := map[string]int{}
	type queueItem struct {
		oid string
		gen int
	}
	queue := []queueItem{}
	for _, oid := range roots {
		queue = append(queue, queueItem{oid: oid, gen: 1})
	}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		if _, ok := genOf[item.oid]; ok {
			continue
		}
		if args.depth >= 0 && item.gen > args.depth {
			continue
		}
		commit := s.ReadCommit(repo, item.oid)
		if commit == nil {
			continue
		}
		genOf[item.oid] = item.gen
		if args.depth < 0 || item.gen < args.depth {
			for _, parent := range commit.Parents {
				queue = append(queue, queueItem{oid: parent, gen: item.gen + 1})
			}
		}
	}
	included := map[string]bool{}
	for oid := range genOf {
		included[oid] = true
	}
	shallowSet := map[string]bool{}
	for _, oid := range args.shallow {
		shallowSet[oid] = true
	}
	held := s.ReachableObjects(repo, args.haves, shallowSet)
	send := map[string]bool{}
	for oid := range extra {
		send[oid] = true
	}
	for oid := range included {
		send[oid] = true
		commit := s.ReadCommit(repo, oid)
		if commit == nil {
			continue
		}
		stack := []string{commit.Tree}
		for len(stack) > 0 {
			at := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if send[at] {
				continue
			}
			object := s.GetObject(repo, at)
			if object == nil {
				continue
			}
			send[at] = true
			if object.Type == ObjTree {
				if entries := s.ReadTree(repo, at); entries != nil {
					for _, entry := range entries {
						stack = append(stack, entry.OID)
					}
				}
			}
		}
	}
	for oid := range held {
		delete(send, oid)
	}
	sortedOIDs := make([]string, 0, len(send))
	for oid := range send {
		sortedOIDs = append(sortedOIDs, oid)
	}
	sort.Strings(sortedOIDs)
	var objects []GitObject
	for _, oid := range sortedOIDs {
		if object := s.GetObject(repo, oid); object != nil {
			objects = append(objects, *object)
		}
	}
	var shallow, unshallow []string
	if args.depth >= 0 {
		for oid, gen := range genOf {
			if gen == args.depth {
				shallow = append(shallow, oid)
			}
		}
		sort.Strings(shallow)
		shallowSet2 := map[string]bool{}
		for _, oid := range shallow {
			shallowSet2[oid] = true
		}
		old := map[string]bool{}
		for _, oid := range args.shallow {
			old[oid] = true
		}
		for oid := range old {
			if included[oid] && !shallowSet2[oid] {
				unshallow = append(unshallow, oid)
			}
		}
		sort.Strings(unshallow)
	}
	return fetchPlan{objects: objects, shallow: shallow, unshallow: unshallow}
}

func fetchV2(s *Store, repo *Repo, args fetchArgs) []byte {
	plan := planFetch(s, repo, args)
	var parts [][]byte
	if len(args.haves) > 0 {
		parts = append(parts, pktLineStr("acknowledgments\n"))
		var common []string
		for _, have := range args.haves {
			if s.HasObject(repo, have) {
				common = append(common, have)
			}
		}
		if len(common) == 0 {
			parts = append(parts, pktLineStr("NAK\n"))
		} else {
			for _, oid := range common {
				parts = append(parts, pktLineStr("ACK "+oid+"\n"))
			}
		}
		if args.done {
			parts = append(parts, pktLineStr("ready\n"))
		}
		parts = append(parts, pktDelim)
	}
	if args.depth >= 0 {
		parts = append(parts, pktLineStr("shallow-info\n"))
		for _, oid := range plan.shallow {
			parts = append(parts, pktLineStr("shallow "+oid+"\n"))
		}
		for _, oid := range plan.unshallow {
			parts = append(parts, pktLineStr("unshallow "+oid+"\n"))
		}
		parts = append(parts, pktDelim)
	}
	parts = append(parts, pktLineStr("packfile\n"))
	parts = append(parts, sidebandPack(writePack(plan.objects)))
	parts = append(parts, pktFlush)
	return concatBytes(parts)
}

func fetchV0(s *Store, repo *Repo, args fetchArgs) []byte {
	plan := planFetch(s, repo, args)
	var parts [][]byte
	hasMultiAck := false
	for _, f := range args.features {
		if f == "multi_ack" || f == "multi_ack_detailed" {
			hasMultiAck = true
		}
	}
	if hasMultiAck {
		var common []string
		for _, have := range args.haves {
			if s.HasObject(repo, have) {
				common = append(common, have)
			}
		}
		if len(common) == 0 {
			parts = append(parts, pktLineStr("NAK\n"))
		} else {
			for _, oid := range common {
				parts = append(parts, pktLineStr("ACK "+oid+"\n"))
			}
		}
	}
	if args.depth >= 0 {
		for _, oid := range plan.shallow {
			parts = append(parts, pktLineStr("shallow "+oid+"\n"))
		}
		for _, oid := range plan.unshallow {
			parts = append(parts, pktLineStr("unshallow "+oid+"\n"))
		}
	}
	parts = append(parts, sidebandPack(writePack(plan.objects)))
	parts = append(parts, pktFlush)
	return concatBytes(parts)
}

// ---------------------------------------------------------------- receive-pack

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

type pushRefResult struct {
	ref    string
	ok     bool
	reason string
}

type pushReport struct {
	unpackOk     bool
	unpackDetail string
	refs         []pushRefResult
}

func applyPush(s *Store, repo *Repo, commands []pushCommand) pushReport {
	report := pushReport{unpackOk: true}
	for _, command := range commands {
		current, exists := s.GetRef(repo, command.ref)
		if command.oldOID == ZeroOID && command.newOID == ZeroOID {
			report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: false, reason: "invalid command"})
			continue
		}
		if command.newOID == ZeroOID {
			// Delete.
			if !exists {
				report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: false, reason: "remote ref does not exist"})
			} else if current != command.oldOID {
				report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: false, reason: "stale info"})
			} else {
				s.DeleteRef(repo, command.ref)
				report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: true})
			}
			continue
		}
		if command.oldOID == ZeroOID {
			// Create.
			if exists {
				report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: false, reason: "already exists"})
				continue
			}
			if !closureComplete(s, repo, command.newOID) {
				report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: false, reason: "missing objects"})
				continue
			}
			s.SetRef(repo, command.ref, command.newOID)
			report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: true})
			continue
		}
		// Update.
		if !exists {
			report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: false, reason: "remote ref does not exist"})
		} else if current != command.oldOID {
			report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: false, reason: "stale info"})
		} else if !closureComplete(s, repo, command.newOID) {
			report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: false, reason: "missing objects"})
		} else if !s.IsAncestor(repo, command.oldOID, command.newOID) {
			report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
		} else {
			s.SetRef(repo, command.ref, command.newOID)
			report.refs = append(report.refs, pushRefResult{ref: command.ref, ok: true})
		}
	}
	return report
}

// closureComplete reports whether the tip's whole closure (commits, trees,
// blobs) is in the store.
func closureComplete(s *Store, repo *Repo, tip string) bool {
	seen := map[string]bool{}
	var visitedCommits []string
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object := s.GetObject(repo, oid)
		if object == nil {
			return false
		}
		switch object.Type {
		case ObjCommit:
			if repo.VerifiedTips[oid] {
				continue
			}
			commit := s.ReadCommit(repo, oid)
			if commit == nil || !s.HasObject(repo, commit.Tree) {
				return false
			}
			visitedCommits = append(visitedCommits, oid)
			stack = append(stack, commit.Parents...)
			stack = append(stack, commit.Tree)
		case ObjTree:
			entries := s.ReadTree(repo, oid)
			if entries == nil {
				return false
			}
			for _, entry := range entries {
				stack = append(stack, entry.OID)
			}
		case ObjTag:
			if target := peelTag(object.Data); target != "" {
				stack = append(stack, target)
			}
		}
	}
	for _, oid := range visitedCommits {
		repo.VerifiedTips[oid] = true
	}
	return true
}

// reportStream renders the report-status stream, pkt-lined, optionally
// wrapped in sideband band 1.
func reportStream(report pushReport, sideband bool) []byte {
	var inner [][]byte
	if report.unpackOk {
		inner = append(inner, pktLineStr("unpack ok\n"))
	} else {
		inner = append(inner, pktLineStr("unpack "+report.unpackDetail+"\n"))
	}
	for _, ref := range report.refs {
		if ref.ok {
			inner = append(inner, pktLineStr("ok "+ref.ref+"\n"))
		} else {
			inner = append(inner, pktLineStr("ng "+ref.ref+" "+ref.reason+"\n"))
		}
	}
	inner = append(inner, pktFlush)
	body := concatBytes(inner)
	if !sideband {
		return body
	}
	payload := append([]byte{1}, body...)
	return concatBytes([][]byte{pktLine(payload), pktFlush})
}

// ---------------------------------------------------------------- dispatch

// handleGit handles a git smart-HTTP request. Returns true when it was a git route.
func handleGit(ctx *Ctx, w http.ResponseWriter) bool {
	parsed := parseGitPath(ctx.Path)
	if parsed == nil {
		return false
	}
	if parsed.rest != "info/refs" && parsed.rest != "git-upload-pack" && parsed.rest != "git-receive-pack" {
		return false
	}
	if !ctx.isAuthorized() {
		ctx.sendError(w, 401, "Requires authentication")
		return true
	}
	repo := ctx.Store.GetRepo(parsed.owner, parsed.repo)
	if repo == nil {
		ctx.sendError(w, 404, "Not Found")
		return true
	}
	if ctx.Method == "GET" && parsed.rest == "info/refs" {
		service := ctx.Query.Get("service")
		if service == "git-upload-pack" {
			v2 := ctx.ProtoHdr == "version=2"
			if v2 {
				ctx.sendBytes(w, 200, advertiseV2(), uploadAd, nil)
			} else {
				ctx.sendBytes(w, 200, advertiseV0(ctx.Store, repo, "git-upload-pack"), uploadAd, nil)
			}
			return true
		}
		if service == "git-receive-pack" {
			ctx.sendBytes(w, 200, advertiseV0(ctx.Store, repo, "git-receive-pack"), receiveAd, nil)
			return true
		}
		ctx.sendError(w, 400, "Unknown service")
		return true
	}
	if ctx.Method == "POST" && parsed.rest == "git-upload-pack" {
		lines, err := parsePktLines(ctx.Body)
		if err != nil {
			ctx.sendError(w, 400, "Malformed upload-pack request: "+err.Error())
			return true
		}
		v2 := ctx.ProtoHdr == "version=2"
		if v2 {
			command := ""
			for _, line := range lines {
				if line.Kind == PktData {
					command = strings.TrimSpace(string(line.Data))
					break
				}
			}
			if command == "command=ls-refs" {
				args := parseLsRefsArgs(lines)
				ctx.sendBytes(w, 200, lsRefsV2(ctx.Store, repo, args), uploadResult, nil)
				return true
			}
			if command == "command=fetch" {
				args := parseFetchArgs(lines)
				ctx.sendBytes(w, 200, fetchV2(ctx.Store, repo, args), uploadResult, nil)
				return true
			}
			ctx.sendError(w, 400, "Unknown upload-pack command "+jsonString(command))
			return true
		}
		args := parseFetchArgs(lines)
		ctx.sendBytes(w, 200, fetchV0(ctx.Store, repo, args), uploadResult, nil)
		return true
	}
	if ctx.Method == "POST" && parsed.rest == "git-receive-pack" {
		flushAt := firstFlushOffset(ctx.Body)
		if flushAt < 0 {
			ctx.sendError(w, 400, "Malformed receive-pack request: no command flush")
			return true
		}
		commandLines, err := parsePktLines(ctx.Body[:flushAt])
		if err != nil {
			ctx.sendError(w, 400, "Malformed receive-pack request: "+err.Error())
			return true
		}
		var commands []pushCommand
		sideband := false
		for _, line := range commandLines {
			if line.Kind != PktData {
				continue
			}
			text := string(line.Data)
			nul := strings.IndexByte(text, 0)
			head := text
			if nul >= 0 {
				head = text[:nul]
				caps := parseCaps(text[nul+1:])
				if _, ok := caps["side-band-64k"]; ok {
					sideband = true
				}
			}
			m := pushCommandRe.FindStringSubmatch(strings.TrimSpace(head))
			if m == nil {
				continue
			}
			commands = append(commands, pushCommand{oldOID: m[1], newOID: m[2], ref: m[3]})
		}
		hasPack := false
		for _, c := range commands {
			if c.newOID != ZeroOID {
				hasPack = true
				break
			}
		}
		if hasPack {
			packBytes := ctx.Body[flushAt:]
			pack, err := readPack(packBytes, func(oid string) *GitObject {
				return ctx.Store.GetObject(repo, oid)
			})
			if err != nil {
				report := pushReport{unpackOk: false, unpackDetail: err.Error()}
				for _, c := range commands {
					report.refs = append(report.refs, pushRefResult{ref: c.ref, ok: false, reason: "unpack failed"})
				}
				ctx.sendBytes(w, 200, reportStream(report, sideband), receiveResult, nil)
				return true
			}
			for oid, object := range pack.ByOID {
				ctx.Store.PutObject(repo, object.Type, object.Data, oid)
			}
		}
		applied := applyPush(ctx.Store, repo, commands)
		ctx.sendBytes(w, 200, reportStream(applied, sideband), receiveResult, nil)
		return true
	}
	ctx.sendError(w, 404, "Not Found")
	return true
}

func parseLsRefsArgs(lines []PktLine) lsRefsArgs {
	args := lsRefsArgs{}
	afterDelim := false
	for _, line := range lines {
		if line.Kind == PktDelim {
			afterDelim = true
			continue
		}
		if line.Kind != PktData || !afterDelim {
			continue
		}
		text := strings.TrimSpace(string(line.Data))
		switch {
		case text == "peel":
			args.peel = true
		case text == "symrefs":
			args.symrefs = true
		case text == "unborn":
			args.unborn = true
		case strings.HasPrefix(text, "ref-prefix "):
			args.prefixes = append(args.prefixes, strings.TrimSpace(text[len("ref-prefix "):]))
		}
	}
	return args
}
