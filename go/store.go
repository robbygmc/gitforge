// The forge's store: every repository, its objects, refs, issues, pull
// requests, labels and comments, held in memory in insertion order. All git
// operations the surfaces need — commit walks, merge-base, three-way tree
// merge, tree updates and diffs — live here so REST, GraphQL, MCP and the
// smart-HTTP handlers all see one consistent state. Mirrors src/store.ts.
package main

import (
	"regexp"
	"sort"
	"strings"
	"sync"
)

const EmptyTreeOID = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

type Comment struct {
	ID         int
	Body       string
	Author     string
	CreatedSeq int
}

type Label struct {
	Name        string
	Color       string
	Description string
}

type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	Comments   []*Comment
	CreatedSeq int
	UpdatedSeq int
	Version    int    // bumps on any mutation; invalidates the cached JSON
	cachedJSON []byte // rendered issue JSON, valid while Version unchanged
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       *string
	MergeCommitSHA *string
	HeadRef        string
	BaseRef        string
	HeadSHA        string
	BaseSHA        string
	CreatedSeq     int
	UpdatedSeq     int
	Comments       []*Comment
	Version        int
	cachedJSON     []byte
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedSeq    int
	Objects       map[string]GitObject
	Refs          map[string]string
	Issues        []*Issue
	Pulls         []*Pull
	Labels        []Label
	NextNumber    int
	NextCommentID int
	// Parsed commits and trees, cached by oid. Objects are immutable and
	// append-only, so a parsed form never goes stale.
	CommitCache  map[string]ParsedCommit
	TreeCache    map[string][]TreeEntry
	VerifiedTips map[string]bool
}

// FlatTree: path -> blob oid (empty string means deleted in a merge result).
type FlatTree map[string]string

type Store struct {
	// mu guards the whole store. Read-only requests take RLock (clones and
	// fetches then run in parallel); mutating requests take Lock.
	mu      sync.RWMutex
	// cacheMu guards the per-repo parsed-object caches, which read-only
	// requests also populate. It is always taken after mu, never before.
	cacheMu  sync.Mutex
	Clock    TickClock
	repos    map[string]*Repo
	nextRepoID int
	nextSeq    int
	nextGlobal int
}

func NewStore() *Store {
	return &Store{
		repos:      map[string]*Repo{},
		nextRepoID: 1,
		nextGlobal: 1,
	}
}

// Seq is the next global creation sequence (ordering for lists and sorts).
func (s *Store) Seq() int {
	s.nextSeq++
	return s.nextSeq
}

// NextGlobalID is a store-wide numeric id for issues and pull requests.
func (s *Store) NextGlobalID() int {
	id := s.nextGlobal
	s.nextGlobal++
	return id
}

func repoKey(owner, name string) string {
	return strings.ToLower(owner) + "\x00" + strings.ToLower(name)
}

func (s *Store) CreateRepo(owner, name string, private bool, description string) *Repo {
	key := repoKey(owner, name)
	if _, exists := s.repos[key]; exists {
		return nil
	}
	repo := &Repo{
		ID:            s.nextRepoID,
		Owner:         owner,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		CreatedSeq:    s.Seq(),
		Objects:       map[string]GitObject{},
		Refs:          map[string]string{},
		Issues:        []*Issue{},
		Pulls:         []*Pull{},
		Labels:        []Label{},
		NextNumber:    1,
		NextCommentID: 1,
		CommitCache:   map[string]ParsedCommit{},
		TreeCache:     map[string][]TreeEntry{},
		VerifiedTips:  map[string]bool{},
	}
	s.nextRepoID++
	s.repos[key] = repo
	return repo
}

func (s *Store) GetRepo(owner, name string) *Repo {
	return s.repos[repoKey(owner, name)]
}

func (s *Store) ListRepos(owner string) []*Repo {
	wanted := strings.ToLower(owner)
	var out []*Repo
	for _, repo := range s.repos {
		if strings.ToLower(repo.Owner) == wanted {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedSeq < out[j].CreatedSeq })
	return out
}

func (s *Store) DeleteRepo(owner, name string) bool {
	key := repoKey(owner, name)
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	return true
}

// ---------------------------------------------------------------- objects

// PutObject stores an object, returning its oid. oid may be supplied when
// the caller already hashed the payload; it must be the object's true hash.
func (s *Store) PutObject(repo *Repo, typ ObjType, data []byte, oid ...string) string {
	id := ""
	if len(oid) > 0 {
		id = oid[0]
	} else {
		id = hashObject(typ, data)
	}
	if _, ok := repo.Objects[id]; !ok {
		repo.Objects[id] = GitObject{Type: typ, Data: data}
	}
	return id
}

func (s *Store) GetObject(repo *Repo, oid string) *GitObject {
	if o, ok := repo.Objects[oid]; ok {
		oc := o
		return &oc
	}
	return nil
}

func (s *Store) HasObject(repo *Repo, oid string) bool {
	_, ok := repo.Objects[oid]
	return ok
}

// ---------------------------------------------------------------- refs

func (s *Store) RefName(branch string) string { return "refs/heads/" + branch }

func (s *Store) GetRef(repo *Repo, ref string) (string, bool) {
	oid, ok := repo.Refs[ref]
	return oid, ok
}

func (s *Store) SetRef(repo *Repo, ref, oid string) { repo.Refs[ref] = oid }

func (s *Store) DeleteRef(repo *Repo, ref string) bool {
	if _, ok := repo.Refs[ref]; !ok {
		return false
	}
	delete(repo.Refs, ref)
	return true
}

// ListRefs returns every ref, sorted by name (the advertisement order).
func (s *Store) ListRefs(repo *Repo) []RefEntry {
	out := make([]RefEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		out = append(out, RefEntry{Name: name, OID: oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type RefEntry struct {
	Name string
	OID  string
}

// HeadTip is the default branch's tip, or "" when the repository is unborn.
func (s *Store) HeadTip(repo *Repo) string {
	return repo.Refs[s.RefName(repo.DefaultBranch)]
}

// ---------------------------------------------------------------- commits

// CreateCommit creates a commit object with the viewer's identity and a
// deterministic timestamp.
func (s *Store) CreateCommit(repo *Repo, tree string, parents []string, message string) string {
	epoch := s.Clock.NextSec()
	author := identityLine(ViewerName, ViewerEmail, epoch)
	data := buildCommit(CommitInput{Tree: tree, Parents: parents, Author: author, Committer: author, Message: message})
	return s.PutObject(repo, ObjCommit, data)
}

// ReadCommit returns the parsed commit of an oid, or nil when absent/not a commit.
func (s *Store) ReadCommit(repo *Repo, oid string) *ParsedCommit {
	s.cacheMu.Lock()
	cached, ok := repo.CommitCache[oid]
	s.cacheMu.Unlock()
	if ok {
		c := cached
		return &c
	}
	object, ok := repo.Objects[oid]
	if !ok || object.Type != ObjCommit {
		return nil
	}
	parsed := parseCommit(object.Data)
	s.cacheMu.Lock()
	repo.CommitCache[oid] = parsed
	s.cacheMu.Unlock()
	return &parsed
}

// ReadTree returns the parsed tree entries of an oid, or nil when absent/not a tree.
func (s *Store) ReadTree(repo *Repo, oid string) []TreeEntry {
	s.cacheMu.Lock()
	cached, ok := repo.TreeCache[oid]
	s.cacheMu.Unlock()
	if ok {
		return cached
	}
	object, ok := repo.Objects[oid]
	if !ok || object.Type != ObjTree {
		return nil
	}
	parsed := parseTree(object.Data)
	s.cacheMu.Lock()
	repo.TreeCache[oid] = parsed
	s.cacheMu.Unlock()
	return parsed
}

// WalkCommits returns every commit reachable from tips, newest first (by
// deterministic timestamp, then by discovery order).
func (s *Store) WalkCommits(repo *Repo, tips []string) []string {
	seen := map[string]bool{}
	stack := append([]string{}, tips...)
	type foundEntry struct {
		oid   string
		epoch int
		order int
	}
	var found []foundEntry
	order := 0
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		commit := s.ReadCommit(repo, oid)
		if commit == nil {
			continue
		}
		epoch := commitEpoch(commit.Committer)
		found = append(found, foundEntry{oid: oid, epoch: epoch, order: order})
		order++
		for _, parent := range commit.Parents {
			stack = append(stack, parent)
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if found[i].epoch != found[j].epoch {
			return found[i].epoch > found[j].epoch
		}
		return found[i].order > found[j].order
	})
	out := make([]string, len(found))
	for i, e := range found {
		out[i] = e.oid
	}
	return out
}

// IsAncestor reports whether ancestor is reachable from descendant (or is the same commit).
func (s *Store) IsAncestor(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		commit := s.ReadCommit(repo, oid)
		if commit == nil {
			continue
		}
		for _, parent := range commit.Parents {
			if parent == ancestor {
				return true
			}
			stack = append(stack, parent)
		}
	}
	return false
}

// MergeBase returns the newest common ancestor of two tips, or "" when their
// histories never meet.
func (s *Store) MergeBase(repo *Repo, a, b string) string {
	ancestorsA := map[string]int{}
	stack := []string{a}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if _, ok := ancestorsA[oid]; ok {
			continue
		}
		commit := s.ReadCommit(repo, oid)
		if commit == nil {
			continue
		}
		ancestorsA[oid] = commitEpoch(commit.Committer)
		stack = append(stack, commit.Parents...)
	}
	best := ""
	bestEpoch := -1
	seenB := map[string]bool{}
	stackB := []string{b}
	for len(stackB) > 0 {
		oid := stackB[len(stackB)-1]
		stackB = stackB[:len(stackB)-1]
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		commit := s.ReadCommit(repo, oid)
		if commit == nil {
			continue
		}
		if epochA, ok := ancestorsA[oid]; ok && epochA > bestEpoch {
			best = oid
			bestEpoch = epochA
		}
		stackB = append(stackB, commit.Parents...)
	}
	return best
}

// ReachableObjects returns every object reachable from tips (commits, their
// trees and blobs, and annotated tags peeled to their targets). stopAt names
// commits whose parents are not walked (a shallow boundary): their own tree
// and blobs are still included.
func (s *Store) ReachableObjects(repo *Repo, tips []string, stopAt map[string]bool) map[string]bool {
	out := map[string]bool{}
	queue := append([]string{}, tips...)
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if out[oid] {
			continue
		}
		object, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		out[oid] = true
		switch object.Type {
		case ObjCommit:
			commit := s.ReadCommit(repo, oid)
			if commit == nil {
				continue
			}
			queue = append(queue, commit.Tree)
			if stopAt == nil || !stopAt[oid] {
				queue = append(queue, commit.Parents...)
			}
		case ObjTree:
			entries := s.ReadTree(repo, oid)
			if entries != nil {
				for _, e := range entries {
					queue = append(queue, e.OID)
				}
			}
		case ObjTag:
			if target := peelTag(object.Data); target != "" {
				queue = append(queue, target)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------- trees

// FlattenTree returns the flattened file map of a tree: path -> {oid, mode}.
// A nil tree returns an empty map.
func (s *Store) FlattenTree(repo *Repo, treeOID string) map[string]TreeEntry {
	out := map[string]TreeEntry{}
	if treeOID == "" {
		return out
	}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		entries := s.ReadTree(repo, oid)
		if entries == nil {
			return
		}
		for _, entry := range entries {
			path := entry.Name
			if prefix != "" {
				path = prefix + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.OID, path)
			} else {
				out[path] = entry
			}
		}
	}
	walk(treeOID, "")
	return out
}

// GetBlobAt returns the blob at path in the tree of commitOID, or nil.
func (s *Store) GetBlobAt(repo *Repo, commitOID, path string) *GitObject {
	commit := s.ReadCommit(repo, commitOID)
	if commit == nil {
		return nil
	}
	segments := []string{}
	for _, part := range splitPath(path) {
		if part != "" {
			segments = append(segments, part)
		}
	}
	treeOID := commit.Tree
	for i, segment := range segments {
		entries := s.ReadTree(repo, treeOID)
		if entries == nil {
			return nil
		}
		var found *TreeEntry
		for j := range entries {
			if entries[j].Name == segment {
				found = &entries[j]
				break
			}
		}
		if found == nil {
			return nil
		}
		if i == len(segments)-1 {
			if found.Mode == "40000" {
				return nil
			}
			blob, ok := repo.Objects[found.OID]
			if !ok || blob.Type != ObjBlob {
				return nil
			}
			b := blob
			return &b
		}
		if found.Mode != "40000" {
			return nil
		}
		treeOID = found.OID
	}
	return nil
}

func splitPath(p string) []string {
	var out []string
	start := 0
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			out = append(out, p[start:i])
			start = i + 1
		}
	}
	out = append(out, p[start:])
	return out
}

// UpdateTree returns a new tree like baseTreeOID with changes applied
// (path -> blob oid, or "" to delete).
func (s *Store) UpdateTree(repo *Repo, baseTreeOID string, changes map[string]string) string {
	flat := s.FlattenTree(repo, baseTreeOID)
	for path, oid := range changes {
		if oid == "" {
			delete(flat, path)
		} else {
			flat[path] = TreeEntry{Mode: "100644", OID: oid, Name: path}
		}
	}
	return s.TreeFromFlat(repo, flat)
}

// TreeFromFlat builds (and stores) the nested trees for a flattened file map.
func (s *Store) TreeFromFlat(repo *Repo, flat map[string]TreeEntry) string {
	var build func(prefix string) string
	build = func(prefix string) string {
		var entries []TreeEntry
		dirs := map[string]bool{}
		for path, file := range flat {
			if prefix != "" && !hasPrefix(path, prefix+"/") {
				continue
			}
			rest := path
			if prefix != "" {
				rest = path[len(prefix)+1:]
			}
			slash := indexByte(rest, '/')
			if slash < 0 {
				entries = append(entries, TreeEntry{Mode: file.Mode, Name: rest, OID: file.OID})
			} else {
				dirs[rest[:slash]] = true
			}
		}
		for dir := range dirs {
			sub := dir
			if prefix != "" {
				sub = prefix + "/" + dir
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: dir, OID: build(sub)})
		}
		return s.PutObject(repo, ObjTree, buildTree(entries))
	}
	return build("")
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------- diff & merge

type TreeDiffEntry struct {
	Path   string
	OldOID string
	NewOID string
	Status string // "added" | "deleted" | "modified"
}

// DiffTrees returns the path-level difference between two trees.
func (s *Store) DiffTrees(repo *Repo, oldTree, newTree string) []TreeDiffEntry {
	oldFlat := s.FlattenTree(repo, oldTree)
	newFlat := s.FlattenTree(repo, newTree)
	paths := map[string]bool{}
	for p := range oldFlat {
		paths[p] = true
	}
	for p := range newFlat {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var out []TreeDiffEntry
	for _, path := range sorted {
		oldEntry, oldOk := oldFlat[path]
		newEntry, newOk := newFlat[path]
		switch {
		case !oldOk && newOk:
			out = append(out, TreeDiffEntry{Path: path, OldOID: "", NewOID: newEntry.OID, Status: "added"})
		case oldOk && !newOk:
			out = append(out, TreeDiffEntry{Path: path, OldOID: oldEntry.OID, NewOID: "", Status: "deleted"})
		case oldOk && newOk && oldEntry.OID != newEntry.OID:
			out = append(out, TreeDiffEntry{Path: path, OldOID: oldEntry.OID, NewOID: newEntry.OID, Status: "modified"})
		}
	}
	return out
}

type MergeFlatResult struct {
	Merged    FlatTree
	Conflicts []string
}

// MergeFlatTrees is a three-way merge of flattened trees. ours and theirs
// are the two tips' trees, base their merge-base's tree. Returns the merged
// file map and the conflicting paths.
func (s *Store) MergeFlatTrees(base, ours, theirs FlatTree) MergeFlatResult {
	merged := FlatTree{}
	var conflicts []string
	paths := map[string]bool{}
	for p := range base {
		paths[p] = true
	}
	for p := range ours {
		paths[p] = true
	}
	for p := range theirs {
		paths[p] = true
	}
	for path := range paths {
		b, bOk := base[path]
		o, oOk := ours[path]
		t, tOk := theirs[path]
		// normalize "absent" to "" (nil in TS)
		bv, ov, tv := "", "", ""
		if bOk {
			bv = b
		}
		if oOk {
			ov = o
		}
		if tOk {
			tv = t
		}
		// TS compares with === on string|null; absent === "" here.
		switch {
		case ov == tv:
			if ov != "" {
				merged[path] = ov
			}
		case ov == bv:
			if tv != "" {
				merged[path] = tv
			}
		case tv == bv:
			if ov != "" {
				merged[path] = ov
			}
		default:
			conflicts = append(conflicts, path)
		}
	}
	sort.Strings(conflicts)
	return MergeFlatResult{Merged: merged, Conflicts: conflicts}
}

type MergeTreesResult struct {
	TreeOID string
	Base    string
}

// MergeTrees returns the merged tree of headTip into baseTip, or nil when
// they conflict. A fast-forward (the base tip already an ancestor of the
// head) merges to the head's tree.
func (s *Store) MergeTrees(repo *Repo, baseTip, headTip string) *MergeTreesResult {
	baseCommit := s.ReadCommit(repo, baseTip)
	headCommit := s.ReadCommit(repo, headTip)
	if baseCommit == nil || headCommit == nil {
		return nil
	}
	if s.IsAncestor(repo, baseTip, headTip) {
		return &MergeTreesResult{TreeOID: headCommit.Tree, Base: baseTip}
	}
	base := s.MergeBase(repo, baseTip, headTip)
	if base == "" {
		return nil
	}
	baseCommit2 := s.ReadCommit(repo, base)
	if baseCommit2 == nil {
		return nil
	}
	toOidMap := func(flat map[string]TreeEntry) FlatTree {
		out := FlatTree{}
		for path, file := range flat {
			out[path] = file.OID
		}
		return out
	}
	baseFlat := s.FlattenTree(repo, baseCommit2.Tree)
	oursFlat := s.FlattenTree(repo, baseCommit.Tree)
	theirsFlat := s.FlattenTree(repo, headCommit.Tree)
	res := s.MergeFlatTrees(toOidMap(baseFlat), toOidMap(oursFlat), toOidMap(theirsFlat))
	if len(res.Conflicts) > 0 {
		return nil
	}
	flat := map[string]TreeEntry{}
	for path, oid := range res.Merged {
		if oid == "" {
			continue
		}
		mode := "100644"
		if e, ok := oursFlat[path]; ok {
			mode = e.Mode
		} else if e, ok := theirsFlat[path]; ok {
			mode = e.Mode
		} else if e, ok := baseFlat[path]; ok {
			mode = e.Mode
		}
		flat[path] = TreeEntry{Mode: mode, OID: oid, Name: path}
	}
	return &MergeTreesResult{TreeOID: s.TreeFromFlat(repo, flat), Base: base}
}

// CommitEpoch extracts the epoch seconds of a "Name <email> <epoch> +0000" line.
var commitEpochRe = regexp.MustCompile(` (\d+) [+-]\d{4}$`)

func commitEpoch(identity string) int {
	m := commitEpochRe.FindStringSubmatch(strings.TrimSpace(identity))
	if m == nil {
		return 0
	}
	n, _ := atoiSafe(m[1])
	return n
}

// FindIssue returns the issue with the given number, or nil.
func FindIssue(repo *Repo, number int) *Issue {
	for _, issue := range repo.Issues {
		if issue.Number == number {
			return issue
		}
	}
	return nil
}

// FindPull returns the pull request with the given number, or nil.
func FindPull(repo *Repo, number int) *Pull {
	for _, pull := range repo.Pulls {
		if pull.Number == number {
			return pull
		}
	}
	return nil
}
