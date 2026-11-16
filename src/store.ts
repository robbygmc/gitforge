// The forge's store: every repository, its objects, refs, issues, pull
// requests, labels and comments, held in memory in insertion order. All git
// operations the surfaces need — commit walks, merge-base, three-way tree
// merge, tree updates and diffs — live here so REST, GraphQL, MCP and the
// smart-HTTP handlers all see one consistent state.

import { buildCommit, buildTree, hashObject, parseCommit, parseTree, type GitObject, type ObjType, type TreeEntry } from './gitcore.ts';
import { identityLine, TickClock, VIEWER_EMAIL, VIEWER_LOGIN, VIEWER_NAME } from './util.ts';

export interface Comment {
  id: number;
  body: string;
  author: string;
  createdSeq: number;
}

export interface Label {
  name: string;
  color: string;
  description: string;
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  comments: Comment[];
  createdSeq: number;
  updatedSeq: number;
}

export interface Pull {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  headRef: string;
  baseRef: string;
  headSha: string;
  baseSha: string;
  createdSeq: number;
  updatedSeq: number;
  comments: Comment[];
}

export interface Repo {
  id: number;
  owner: string;
  name: string;
  description: string;
  private: boolean;
  defaultBranch: string;
  createdSeq: number;
  objects: Map<string, GitObject>;
  refs: Map<string, string>;
  issues: Issue[];
  pulls: Pull[];
  labels: Label[];
  /** Issues and pull requests share one number sequence, as on GitHub. */
  nextNumber: number;
  nextCommentId: number;
}

/** A flattened tree: path -> blob oid (null means deleted in a merge result). */
export type FlatTree = Map<string, string | null>;

export class Store {
  readonly clock = new TickClock();
  private repos = new Map<string, Repo>();
  private nextRepoId = 1;
  private nextSeq = 0;
  private nextGlobal = 1;

  /** The next global creation sequence (ordering for lists and sorts). */
  seq(): number {
    this.nextSeq += 1;
    return this.nextSeq;
  }

  /** A store-wide numeric id for issues and pull requests (stable across reads). */
  nextGlobalId(): number {
    const id = this.nextGlobal;
    this.nextGlobal += 1;
    return id;
  }

  private key(owner: string, name: string): string {
    return `${owner.toLowerCase()}\0${name.toLowerCase()}`;
  }

  createRepo(owner: string, name: string, options: { private: boolean; description: string }): Repo | null {
    const key = this.key(owner, name);
    if (this.repos.has(key)) return null;
    const repo: Repo = {
      id: this.nextRepoId,
      owner,
      name,
      description: options.description,
      private: options.private,
      defaultBranch: 'main',
      createdSeq: this.seq(),
      objects: new Map(),
      refs: new Map(),
      issues: [],
      pulls: [],
      labels: [],
      nextNumber: 1,
      nextCommentId: 1,
    };
    this.nextRepoId += 1;
    this.repos.set(key, repo);
    return repo;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(this.key(owner, name));
  }

  listRepos(owner: string): Repo[] {
    const wanted = owner.toLowerCase();
    const out: Repo[] = [];
    for (const repo of this.repos.values()) if (repo.owner.toLowerCase() === wanted) out.push(repo);
    out.sort((a, b) => a.createdSeq - b.createdSeq);
    return out;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(this.key(owner, name));
  }

  // ---------------------------------------------------------------- objects

  putObject(repo: Repo, type: ObjType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  getObject(repo: Repo, oid: string): GitObject | undefined {
    return repo.objects.get(oid);
  }

  hasObject(repo: Repo, oid: string): boolean {
    return repo.objects.has(oid);
  }

  // ---------------------------------------------------------------- refs

  refName(branch: string): string {
    return `refs/heads/${branch}`;
  }

  getRef(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  setRef(repo: Repo, ref: string, oid: string): void {
    repo.refs.set(ref, oid);
  }

  deleteRef(repo: Repo, ref: string): boolean {
    return repo.refs.delete(ref);
  }

  /** Every ref, sorted by name (the advertisement order). */
  listRefs(repo: Repo): { name: string; oid: string }[] {
    const out: { name: string; oid: string }[] = [];
    for (const [name, oid] of repo.refs) out.push({ name, oid });
    out.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    return out;
  }

  /** The default branch's tip, or undefined when the repository is unborn. */
  headTip(repo: Repo): string | undefined {
    return repo.refs.get(this.refName(repo.defaultBranch));
  }

  // ---------------------------------------------------------------- commits

  /** Create a commit object with the viewer's identity and a deterministic timestamp. */
  createCommit(repo: Repo, input: { tree: string; parents: readonly string[]; message: string }): string {
    const epoch = this.clock.nextSec();
    const author = identityLine(VIEWER_NAME, VIEWER_EMAIL, epoch);
    const data = buildCommit({ tree: input.tree, parents: input.parents, author, committer: author, message: input.message });
    return this.putObject(repo, 'commit', data);
  }

  /** The parsed commit of an oid, or null when it is absent or not a commit. */
  readCommit(repo: Repo, oid: string): ReturnType<typeof parseCommit> | null {
    const object = repo.objects.get(oid);
    if (object === undefined || object.type !== 'commit') return null;
    return parseCommit(object.data);
  }

  /**
   * Every commit reachable from `tips`, newest first (by deterministic
   * timestamp, then by discovery order). A bounded walk: histories here are
   * small, and the suite never builds one past a few hundred commits.
   */
  walkCommits(repo: Repo, tips: readonly string[]): string[] {
    const seen = new Set<string>();
    const stack: string[] = [...tips];
    const found: { oid: string; epoch: number; order: number }[] = [];
    let order = 0;
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const commit = this.readCommit(repo, oid);
      if (commit === null) continue;
      const epoch = commitEpoch(commit.committer);
      found.push({ oid, epoch, order: order });
      order += 1;
      for (const parent of commit.parents) stack.push(parent);
    }
    found.sort((a, b) => b.epoch - a.epoch || b.order - a.order);
    return found.map((entry) => entry.oid);
  }

  /** Whether `ancestor` is reachable from `descendant` (or is the same commit). */
  isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
    if (ancestor === descendant) return true;
    const seen = new Set<string>();
    const stack = [descendant];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const commit = this.readCommit(repo, oid);
      if (commit === null) continue;
      for (const parent of commit.parents) {
        if (parent === ancestor) return true;
        stack.push(parent);
      }
    }
    return false;
  }

  /** The newest common ancestor of two tips, or null when their histories never meet. */
  mergeBase(repo: Repo, a: string, b: string): string | null {
    const ancestorsA = new Map<string, number>();
    const stack: string[] = [a];
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (ancestorsA.has(oid)) continue;
      const commit = this.readCommit(repo, oid);
      if (commit === null) continue;
      ancestorsA.set(oid, commitEpoch(commit.committer));
      for (const parent of commit.parents) stack.push(parent);
    }
    let best: string | null = null;
    let bestEpoch = -1;
    const seenB = new Set<string>();
    const stackB = [b];
    while (stackB.length > 0) {
      const oid = stackB.pop() as string;
      if (seenB.has(oid)) continue;
      seenB.add(oid);
      const commit = this.readCommit(repo, oid);
      if (commit === null) continue;
      const epochA = ancestorsA.get(oid);
      if (epochA !== undefined && epochA > bestEpoch) {
        best = oid;
        bestEpoch = epochA;
      }
      for (const parent of commit.parents) stackB.push(parent);
    }
    return best;
  }

  /**
   * Every object reachable from `tips` (commits, their trees and blobs, and
   * annotated tags peeled to their targets). `stopAt` names commits whose
   * parents are not walked (a shallow boundary): their own tree and blobs are
   * still included.
   */
  reachableObjects(repo: Repo, tips: readonly string[], stopAt?: ReadonlySet<string>): Set<string> {
    const out = new Set<string>();
    const queue: string[] = [...tips];
    while (queue.length > 0) {
      const oid = queue.pop() as string;
      if (out.has(oid)) continue;
      const object = repo.objects.get(oid);
      if (object === undefined) continue;
      out.add(oid);
      if (object.type === 'commit') {
        const commit = parseCommit(object.data);
        queue.push(commit.tree);
        if (stopAt === undefined || !stopAt.has(oid)) {
          for (const parent of commit.parents) queue.push(parent);
        }
      } else if (object.type === 'tree') {
        for (const entry of parseTree(object.data)) queue.push(entry.oid);
      } else if (object.type === 'tag') {
        // Tags in this forge are only ever created by pushes; peel them.
        const text = object.data.toString('utf8');
        const match = /^object ([0-9a-f]{40})$/m.exec(text);
        if (match !== null) queue.push(match[1] as string);
      }
    }
    return out;
  }

  // ---------------------------------------------------------------- trees

  /** The flattened file map of a tree: path -> {oid, mode}. */
  flattenTree(repo: Repo, treeOid: string | null): Map<string, { oid: string; mode: string }> {
    const out = new Map<string, { oid: string; mode: string }>();
    if (treeOid === null) return out;
    const walk = (oid: string, prefix: string): void => {
      const object = repo.objects.get(oid);
      if (object === undefined || object.type !== 'tree') return;
      for (const entry of parseTree(object.data)) {
        const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
        if (entry.mode === '40000') walk(entry.oid, path);
        else out.set(path, { oid: entry.oid, mode: entry.mode });
      }
    };
    walk(treeOid, '');
    return out;
  }

  /** The blob at `path` in the tree of `commitOid`, or null. */
  getBlobAt(repo: Repo, commitOid: string, path: string): { oid: string; data: Buffer } | null {
    const commit = this.readCommit(repo, commitOid);
    if (commit === null) return null;
    const segments = path.split('/').filter((part) => part !== '');
    let treeOid = commit.tree;
    for (let i = 0; i < segments.length; i += 1) {
      const object = repo.objects.get(treeOid);
      if (object === undefined || object.type !== 'tree') return null;
      const entry = parseTree(object.data).find((item) => item.name === segments[i]);
      if (entry === undefined) return null;
      if (i === segments.length - 1) {
        if (entry.mode === '40000') return null;
        const blob = repo.objects.get(entry.oid);
        if (blob === undefined || blob.type !== 'blob') return null;
        return { oid: entry.oid, data: blob.data };
      }
      if (entry.mode !== '40000') return null;
      treeOid = entry.oid;
    }
    return null;
  }

  /**
   * A new tree like `baseTreeOid` with `changes` applied (path -> blob oid,
   * or null to delete). Intermediate directories are created or pruned as
   * needed; an empty tree is the well-known empty-tree oid.
   */
  updateTree(repo: Repo, baseTreeOid: string | null, changes: ReadonlyMap<string, string | null>): string {
    const flat = this.flattenTree(repo, baseTreeOid);
    for (const [path, oid] of changes) {
      if (oid === null) flat.delete(path);
      else flat.set(path, { oid, mode: '100644' });
    }
    return this.treeFromFlat(repo, flat);
  }

  /** Build (and store) the nested trees for a flattened file map. */
  treeFromFlat(repo: Repo, flat: ReadonlyMap<string, { oid: string; mode: string }>): string {
    const build = (prefix: string): string => {
      const entries: TreeEntry[] = [];
      const dirs = new Set<string>();
      for (const [path, file] of flat) {
        if (prefix !== '' && !path.startsWith(`${prefix}/`)) continue;
        const rest = prefix === '' ? path : path.slice(prefix.length + 1);
        const slash = rest.indexOf('/');
        if (slash < 0) {
          entries.push({ mode: file.mode, name: rest, oid: file.oid });
        } else {
          dirs.add(rest.slice(0, slash));
        }
      }
      for (const dir of dirs) entries.push({ mode: '40000', name: dir, oid: build(prefix === '' ? dir : `${prefix}/${dir}`) });
      return this.putObject(repo, 'tree', buildTree(entries));
    };
    return build('');
  }

  // ---------------------------------------------------------------- diff & merge

  /** The path-level difference between two trees. */
  diffTrees(repo: Repo, oldTree: string | null, newTree: string | null): { path: string; oldOid: string | null; newOid: string | null; status: 'added' | 'deleted' | 'modified' }[] {
    const oldFlat = this.flattenTree(repo, oldTree);
    const newFlat = this.flattenTree(repo, newTree);
    const paths = new Set([...oldFlat.keys(), ...newFlat.keys()]);
    const out: { path: string; oldOid: string | null; newOid: string | null; status: 'added' | 'deleted' | 'modified' }[] = [];
    for (const path of [...paths].sort()) {
      const oldEntry = oldFlat.get(path);
      const newEntry = newFlat.get(path);
      if (oldEntry === undefined && newEntry !== undefined) out.push({ path, oldOid: null, newOid: newEntry.oid, status: 'added' });
      else if (oldEntry !== undefined && newEntry === undefined) out.push({ path, oldOid: oldEntry.oid, newOid: null, status: 'deleted' });
      else if (oldEntry !== undefined && newEntry !== undefined && oldEntry.oid !== newEntry.oid) out.push({ path, oldOid: oldEntry.oid, newOid: newEntry.oid, status: 'modified' });
    }
    return out;
  }

  /**
   * Three-way merge of flattened trees. `ours` and `theirs` are the two tips'
   * trees, `base` their merge-base's tree. Returns the merged file map and
   * the conflicting paths (both sides changed the same path differently, or
   * one deleted what the other changed).
   */
  mergeFlatTrees(base: FlatTree, ours: FlatTree, theirs: FlatTree): { merged: FlatTree; conflicts: string[] } {
    const merged: FlatTree = new Map();
    const conflicts: string[] = [];
    const paths = new Set([...base.keys(), ...ours.keys(), ...theirs.keys()]);
    for (const path of paths) {
      const b = base.get(path) ?? null;
      const o = ours.get(path) ?? null;
      const t = theirs.get(path) ?? null;
      if (o === t) {
        if (o !== null) merged.set(path, o);
      } else if (o === b) {
        if (t !== null) merged.set(path, t);
      } else if (t === b) {
        if (o !== null) merged.set(path, o);
      } else {
        conflicts.push(path);
      }
    }
    conflicts.sort();
    return { merged, conflicts };
  }

  /**
   * The merged tree of `headTip` into `baseTip`, or null when they conflict.
   * The merge-base is computed from the two tips; a fast-forward (the base
   * tip already an ancestor of the head) merges to the head's tree.
   */
  mergeTrees(repo: Repo, baseTip: string, headTip: string): { treeOid: string; base: string } | null {
    const baseCommit = this.readCommit(repo, baseTip);
    const headCommit = this.readCommit(repo, headTip);
    if (baseCommit === null || headCommit === null) return null;
    if (this.isAncestor(repo, baseTip, headTip)) return { treeOid: headCommit.tree, base: baseTip };
    const base = this.mergeBase(repo, baseTip, headTip);
    if (base === null) return null;
    const baseCommit2 = this.readCommit(repo, base);
    if (baseCommit2 === null) return null;
    const toOidMap = (flat: Map<string, { oid: string; mode: string }>): FlatTree => {
      const out: FlatTree = new Map();
      for (const [path, file] of flat) out.set(path, file.oid);
      return out;
    };
    const baseFlat = this.flattenTree(repo, baseCommit2.tree);
    const oursFlat = this.flattenTree(repo, baseCommit.tree);
    const theirsFlat = this.flattenTree(repo, headCommit.tree);
    const { merged, conflicts } = this.mergeFlatTrees(toOidMap(baseFlat), toOidMap(oursFlat), toOidMap(theirsFlat));
    if (conflicts.length > 0) return null;
    const flat = new Map<string, { oid: string; mode: string }>();
    for (const [path, oid] of merged) {
      if (oid === null) continue;
      const mode = oursFlat.get(path)?.mode ?? theirsFlat.get(path)?.mode ?? baseFlat.get(path)?.mode ?? '100644';
      flat.set(path, { oid, mode });
    }
    return { treeOid: this.treeFromFlat(repo, flat), base };
  }
}

/** The epoch seconds of a `Name <email> <epoch> +0000` identity line. */
export function commitEpoch(identity: string): number {
  const match = / (\d+) [+-]\d{4}$/.exec(identity.trim());
  return match !== null ? Number(match[1]) : 0;
}

/** The empty tree's oid (git's well-known constant). */
export const EMPTY_TREE_OID = '4b825dc642cb6eb9a060e54bf8d69288fbee4904';
