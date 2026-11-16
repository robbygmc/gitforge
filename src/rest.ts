// GitHub REST v3: repositories, branches, commits, contents, labels,
// issues, comments and pull requests, in the shapes the suite's
// checks-rest.ts asserts (see its header comment for the endpoint contract).

import { b64decode, b64encode, isOid, VIEWER_ID, VIEWER_LOGIN, VIEWER_TYPE } from './util.ts';
import { isAuthorized, isRecord, parseJsonBody, sendError, sendJson, sendNoContent, type Ctx } from './http.ts';
import { parseCommit, parseTree } from './gitcore.ts';
import type { Issue, Label, Pull, Repo, Store } from './store.ts';

// ---------------------------------------------------------------- rendering

export function userJson(): Record<string, unknown> {
  return { login: VIEWER_LOGIN, id: VIEWER_ID, type: VIEWER_TYPE, avatar_url: '', html_url: '' };
}

export function repoJson(store: Store, repo: Repo): Record<string, unknown> {
  const created = new Date((1_735_689_600 + repo.createdSeq) * 1000).toISOString();
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: userJson(),
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}`,
    created_at: created,
    updated_at: created,
    pushed_at: created,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
    fork: false,
    archived: false,
    disabled: false,
  };
}

export function labelJson(label: Label): Record<string, unknown> {
  return { id: label.name, name: label.name, color: label.color, description: label.description ?? '', default: false };
}

export function issueJson(store: Store, repo: Repo, issue: Issue): Record<string, unknown> {
  const created = new Date((1_735_689_600 + issue.createdSeq) * 1000).toISOString();
  const updated = new Date((1_735_689_600 + issue.updatedSeq) * 1000).toISOString();
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const label = repo.labels.find((entry) => entry.name === name);
      return label !== undefined ? labelJson(label) : { name, color: 'ededed', description: '' };
    }),
    user: userJson(),
    assignees: [],
    created_at: created,
    updated_at: updated,
    closed_at: issue.state === 'closed' ? updated : null,
    comments: issue.comments.length,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

export function pullJson(store: Store, repo: Repo, pull: Pull): Record<string, unknown> {
  const created = new Date((1_735_689_600 + pull.createdSeq) * 1000).toISOString();
  const updated = new Date((1_735_689_600 + pull.updatedSeq) * 1000).toISOString();
  const headTip = store.getRef(repo, store.refName(pull.headRef)) ?? pull.headSha;
  const baseTip = store.getRef(repo, store.refName(pull.baseRef)) ?? pull.baseSha;
  return {
    id: pull.id,
    number: pull.number,
    state: pull.state,
    title: pull.title,
    body: pull.body,
    head: { ref: pull.headRef, sha: headTip, label: `${repo.owner}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: baseTip, label: `${repo.owner}:${pull.baseRef}` },
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    mergeable: pullMergeable(store, repo, pull),
    user: userJson(),
    created_at: created,
    updated_at: updated,
    closed_at: pull.state === 'closed' ? updated : null,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/pull/${pull.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pull.number}`,
  };
}

/** Whether a pull request merges cleanly right now (null when it cannot be told). */
export function pullMergeable(store: Store, repo: Repo, pull: Pull): boolean | null {
  if (pull.state !== 'open') return false;
  const baseTip = store.getRef(repo, store.refName(pull.baseRef));
  const headTip = store.getRef(repo, store.refName(pull.headRef));
  if (baseTip === undefined || headTip === undefined) return null;
  if (baseTip === headTip) return true;
  return store.mergeTrees(repo, baseTip, headTip) !== null;
}

export function commentJson(comment: { id: number; body: string; author: string; createdSeq: number }): Record<string, unknown> {
  const created = new Date((1_735_689_600 + comment.createdSeq) * 1000).toISOString();
  return {
    id: comment.id,
    body: comment.body,
    user: { login: comment.author, id: comment.author === VIEWER_LOGIN ? VIEWER_ID : 0, type: 'User' },
    created_at: created,
    updated_at: created,
  };
}

// ---------------------------------------------------------------- pagination

interface PageParams {
  page: number;
  perPage: number;
}

function pageParams(ctx: Ctx): PageParams {
  const page = Math.max(1, Number(ctx.query.get('page') ?? '1') || 1);
  const perPageRaw = Number(ctx.query.get('per_page') ?? '30') || 30;
  const perPage = Math.min(100, Math.max(1, perPageRaw));
  return { page, perPage };
}

/** The Link header for a paginated list (absolute URLs on this origin). */
function linkHeader(ctx: Ctx, page: number, perPage: number, total: number): string | null {
  const pages = Math.max(1, Math.ceil(total / perPage));
  if (pages <= 1) return null;
  const urlFor = (pageNumber: number): string => {
    const url = new URL(ctx.req.url ?? '/', `http://${ctx.host}`);
    url.searchParams.set('page', String(pageNumber));
    url.searchParams.set('per_page', String(perPage));
    return url.toString();
  };
  const parts: string[] = [];
  if (page < pages) parts.push(`<${urlFor(page + 1)}>; rel="next"`);
  if (page > 1) parts.push(`<${urlFor(page - 1)}>; rel="prev"`);
  parts.push(`<${urlFor(1)}>; rel="first"`);
  parts.push(`<${urlFor(pages)}>; rel="last"`);
  return parts.join(', ');
}

function pageSlice<T>(items: readonly T[], params: PageParams): T[] {
  const start = (params.page - 1) * params.perPage;
  return items.slice(start, start + params.perPage);
}

// ---------------------------------------------------------------- line diff

/** Added/removed line counts between two text blobs (multiset line diff). */
export function lineDiff(oldText: string | null, newText: string | null): { additions: number; deletions: number } {
  const linesOf = (text: string | null): string[] => {
    if (text === null) return [];
    const lines = text.split('\n');
    if (lines.length > 0 && lines[lines.length - 1] === '') lines.pop();
    return lines;
  };
  const oldLines = linesOf(oldText);
  const newLines = linesOf(newText);
  const oldCount = new Map<string, number>();
  for (const line of oldLines) oldCount.set(line, (oldCount.get(line) ?? 0) + 1);
  let additions = 0;
  for (const line of newLines) {
    const left = oldCount.get(line) ?? 0;
    if (left > 0) oldCount.set(line, left - 1);
    else additions += 1;
  }
  let deletions = 0;
  for (const left of oldCount.values()) deletions += left;
  return { additions, deletions };
}

// ---------------------------------------------------------------- routing

/** Handle a REST v3 request. Returns true when the path was under /api/v3. */
export function handleRest(ctx: Ctx): boolean {
  if (ctx.path !== '/api/v3' && !ctx.path.startsWith('/api/v3/')) return false;
  if (!isAuthorized(ctx)) {
    sendError(ctx, 401, 'Requires authentication');
    return true;
  }
  const segments = ctx.path.split('/').filter((piece) => piece !== '').slice(2).map((piece) => {
    try {
      return decodeURIComponent(piece);
    } catch {
      return piece;
    }
  });
  void dispatch(ctx, segments);
  return true;
}

function dispatch(ctx: Ctx, segments: string[]): void {
  const store = ctx.store;
  const method = ctx.method;
  // The API root.
  if (segments.length === 0) {
    if (method === 'GET') {
      sendJson(ctx, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
    } else {
      sendError(ctx, 404, 'Not Found');
    }
    return;
  }
  if (segments[0] === 'meta' && segments.length === 1) {
    if (method === 'GET') sendJson(ctx, 200, { installed_version: '1.0.0' });
    else sendError(ctx, 404, 'Not Found');
    return;
  }
  if (segments[0] === 'user' && segments.length === 1) {
    if (method === 'GET') sendJson(ctx, 200, { ...userJson(), name: 'ada iversen', email: 'adaiversen@example.invalid' });
    else sendError(ctx, 404, 'Not Found');
    return;
  }
  if (segments[0] === 'user' && segments[1] === 'repos') {
    if (method === 'POST') return createRepo(ctx);
    if (method === 'GET') return listUserRepos(ctx);
    sendError(ctx, 404, 'Not Found');
    return;
  }
  if (segments[0] === 'repos' && segments.length >= 3) {
    return repoRoutes(ctx, segments.slice(1));
  }
  sendError(ctx, 404, 'Not Found');
}

function readBodyJson(ctx: Ctx): { ok: true; value: Record<string, unknown> } | { ok: false; status: number; message: string } {
  const parsed = parseJsonBody(ctx);
  if (!parsed.ok) {
    if (parsed.empty) return { ok: true, value: {} };
    return { ok: false, status: 400, message: 'Invalid JSON in request body' };
  }
  if (!isRecord(parsed.value)) return { ok: false, status: 400, message: 'Request body must be a JSON object' };
  return { ok: true, value: parsed.value };
}

function createRepo(ctx: Ctx): void {
  const body = readBodyJson(ctx);
  if (!body.ok) {
    sendError(ctx, body.status, body.message);
    return;
  }
  const name = typeof body.value.name === 'string' ? body.value.name.trim() : '';
  if (name === '' || /[/\s]/.test(name)) {
    sendError(ctx, 422, 'Validation Failed: name is required');
    return;
  }
  const repo = ctx.store.createRepo(VIEWER_LOGIN, name, {
    private: body.value.private === true,
    description: typeof body.value.description === 'string' ? body.value.description : '',
  });
  if (repo === null) {
    sendError(ctx, 422, `Validation Failed: Repository ${name} already exists`);
    return;
  }
  sendJson(ctx, 201, repoJson(ctx.store, repo));
}

function listUserRepos(ctx: Ctx): void {
  const params = pageParams(ctx);
  const repos = ctx.store.listRepos(VIEWER_LOGIN);
  const link = linkHeader(ctx, params.page, params.perPage, repos.length);
  sendJson(ctx, 200, pageSlice(repos, params).map((repo) => repoJson(ctx.store, repo)), link === null ? {} : { link });
}

/** Resolve owner/name to a repository the token owns; 404 otherwise (never 403). */
function ownedRepo(ctx: Ctx, owner: string, name: string): Repo | null {
  if (owner.toLowerCase() !== VIEWER_LOGIN.toLowerCase()) return null;
  return ctx.store.getRepo(owner, name) ?? null;
}

function repoRoutes(ctx: Ctx, segments: string[]): void {
  const [owner, name, ...rest] = segments as [string, string, ...string[]];
  const repo = ownedRepo(ctx, owner, name);
  if (repo === null) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  const store = ctx.store;
  const method = ctx.method;
  if (rest.length === 0) {
    if (method === 'GET') {
      sendJson(ctx, 200, repoJson(store, repo));
    } else if (method === 'PATCH') {
      const body = readBodyJson(ctx);
      if (!body.ok) {
        sendError(ctx, body.status, body.message);
        return;
      }
      if (typeof body.value.description === 'string') repo.description = body.value.description;
      if (typeof body.value.private === 'boolean') repo.private = body.value.private;
      sendJson(ctx, 200, repoJson(store, repo));
    } else if (method === 'DELETE') {
      store.deleteRepo(owner, name);
      sendNoContent(ctx);
    } else {
      sendError(ctx, 404, 'Not Found');
    }
    return;
  }
  const [head, ...tail] = rest as [string, ...string[]];
  if (head === 'branches') {
    if (tail.length === 0 && method === 'GET') {
      const branches = store.listRefs(repo).filter((ref) => ref.name.startsWith('refs/heads/'));
      sendJson(ctx, 200, branches.map((ref) => ({ name: ref.name.slice('refs/heads/'.length), commit: { sha: ref.oid } })));
      return;
    }
    if (tail.length === 1 && method === 'GET') {
      const oid = store.getRef(repo, store.refName(tail[0] as string));
      if (oid === undefined) {
        sendError(ctx, 404, 'Not Found');
        return;
      }
      sendJson(ctx, 200, { name: tail[0], commit: { sha: oid } });
      return;
    }
    sendError(ctx, 404, 'Not Found');
    return;
  }
  if (head === 'commits') {
    if (method === 'GET' && tail.length === 0) return listCommits(ctx, repo);
    sendError(ctx, 404, 'Not Found');
    return;
  }
  if (head === 'git' && tail[0] === 'refs') {
    if (method === 'POST' && tail.length === 1) return createRef(ctx, repo);
    if (method === 'GET') return getRef(ctx, repo, tail.slice(1));
    sendError(ctx, 404, 'Not Found');
    return;
  }
  if (head === 'contents') {
    if (method === 'PUT') return putContents(ctx, repo, tail);
    if (method === 'GET') return getContents(ctx, repo, tail);
    if (method === 'DELETE') return deleteContents(ctx, repo, tail);
    sendError(ctx, 404, 'Not Found');
    return;
  }
  if (head === 'labels') {
    if (method === 'POST') return createLabel(ctx, repo);
    if (method === 'GET') {
      sendJson(ctx, 200, repo.labels.map(labelJson));
      return;
    }
    sendError(ctx, 404, 'Not Found');
    return;
  }
  if (head === 'issues') {
    if (tail.length === 0) {
      if (method === 'POST') return createIssue(ctx, repo);
      if (method === 'GET') return listIssues(ctx, repo);
      sendError(ctx, 404, 'Not Found');
      return;
    }
    const number = Number(tail[0]);
    if (!Number.isInteger(number) || number < 1) {
      sendError(ctx, 404, 'Not Found');
      return;
    }
    if (tail.length === 1) {
      if (method === 'GET') return getIssue(ctx, repo, number);
      if (method === 'PATCH') return patchIssue(ctx, repo, number);
      sendError(ctx, 404, 'Not Found');
      return;
    }
    if (tail[1] === 'comments') {
      if (method === 'POST') return createComment(ctx, repo, number);
      if (method === 'GET') return listComments(ctx, repo, number);
      sendError(ctx, 404, 'Not Found');
      return;
    }
    sendError(ctx, 404, 'Not Found');
    return;
  }
  if (head === 'pulls') {
    if (tail.length === 0) {
      if (method === 'POST') return createPull(ctx, repo);
      if (method === 'GET') return listPulls(ctx, repo);
      sendError(ctx, 404, 'Not Found');
      return;
    }
    const number = Number(tail[0]);
    if (!Number.isInteger(number) || number < 1) {
      sendError(ctx, 404, 'Not Found');
      return;
    }
    if (tail.length === 1) {
      if (method === 'GET') return getPull(ctx, repo, number);
      sendError(ctx, 404, 'Not Found');
      return;
    }
    if (tail[1] === 'files' && method === 'GET') return listPullFiles(ctx, repo, number);
    if (tail[1] === 'merge' && method === 'PUT') return mergePull(ctx, repo, number);
    sendError(ctx, 404, 'Not Found');
    return;
  }
  sendError(ctx, 404, 'Not Found');
}

// ---------------------------------------------------------------- commits / refs

function resolveRefTip(store: Store, repo: Repo, ref: string | null): string | undefined {
  if (ref === null || ref === undefined || ref === '') return store.headTip(repo);
  if (/^[0-9a-f]{40}$/.test(ref)) return store.hasObject(repo, ref) ? ref : undefined;
  if (ref.startsWith('refs/')) return store.getRef(repo, ref);
  return store.getRef(repo, store.refName(ref));
}

function listCommits(ctx: Ctx, repo: Repo): void {
  const store = ctx.store;
  const sha = ctx.query.get('sha') ?? null;
  const tip = resolveRefTip(store, repo, sha);
  if (tip === undefined) {
    sendJson(ctx, 200, []);
    return;
  }
  const commits = store.walkCommits(repo, [tip]);
  sendJson(
    ctx,
    200,
    commits.map((oid) => {
      const parsed = store.readCommit(repo, oid);
      const message = parsed?.message ?? '';
      const author = parsed?.author ?? '';
      const committer = parsed?.committer ?? '';
      return {
        sha: oid,
        commit: {
          message,
          author: identityJson(author),
          committer: identityJson(committer),
        },
        author: { login: VIEWER_LOGIN },
        committer: { login: VIEWER_LOGIN },
      };
    }),
  );
}

function identityJson(line: string): { name: string; email: string; date: string } {
  const match = /^(.*) <([^>]*)> (\d+) [+-]\d{4}$/.exec(line.trim());
  if (match === null) return { name: line, email: '', date: '' };
  return { name: match[1] as string, email: match[2] as string, date: new Date(Number(match[3]) * 1000).toISOString() };
}

function createRef(ctx: Ctx, repo: Repo): void {
  const body = readBodyJson(ctx);
  if (!body.ok) {
    sendError(ctx, body.status, body.message);
    return;
  }
  const ref = typeof body.value.ref === 'string' ? body.value.ref : '';
  const sha = typeof body.value.sha === 'string' ? body.value.sha : '';
  if (!ref.startsWith('refs/') || !isOid(sha) || !ctx.store.hasObject(repo, sha)) {
    sendError(ctx, 422, 'Validation Failed: ref and sha are required');
    return;
  }
  if (ctx.store.getRef(repo, ref) !== undefined) {
    sendError(ctx, 422, `Validation Failed: Reference ${ref} already exists`);
    return;
  }
  ctx.store.setRef(repo, ref, sha);
  sendJson(ctx, 201, { ref, object: { sha, type: 'commit' } });
}

function getRef(ctx: Ctx, repo: Repo, tail: string[]): void {
  if (tail.length === 0) {
    sendJson(
      ctx,
      200,
      ctx.store.listRefs(repo).map((ref) => ({ ref: ref.name, object: { sha: ref.oid, type: 'commit' } })),
    );
    return;
  }
  const ref = `refs/${tail.join('/')}`;
  const oid = ctx.store.getRef(repo, ref);
  if (oid === undefined) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  sendJson(ctx, 200, { ref, object: { sha: oid, type: 'commit' } });
}

// ---------------------------------------------------------------- contents

function contentsCommit(store: Store, repo: Repo, branch: string, changes: ReadonlyMap<string, string | null>, message: string): string {
  const currentTip = store.getRef(repo, store.refName(branch));
  const baseTree = currentTip === undefined ? null : (store.readCommit(repo, currentTip)?.tree ?? null);
  const treeOid = store.updateTree(repo, baseTree, changes);
  const parents = currentTip === undefined ? [] : [currentTip];
  const commitOid = store.createCommit(repo, { tree: treeOid, parents, message });
  store.setRef(repo, store.refName(branch), commitOid);
  return commitOid;
}

function putContents(ctx: Ctx, repo: Repo, pathParts: string[]): void {
  const store = ctx.store;
  const path = pathParts.join('/');
  if (path === '') {
    sendError(ctx, 422, 'Validation Failed: path is required');
    return;
  }
  const body = readBodyJson(ctx);
  if (!body.ok) {
    sendError(ctx, body.status, body.message);
    return;
  }
  const message = typeof body.value.message === 'string' && body.value.message !== '' ? body.value.message : 'Update file';
  const contentB64 = typeof body.value.content === 'string' ? body.value.content : '';
  const branch = typeof body.value.branch === 'string' && body.value.branch !== '' ? body.value.branch : repo.defaultBranch;
  const sha = typeof body.value.sha === 'string' ? body.value.sha : null;
  let content: Buffer;
  try {
    content = b64decode(contentB64);
  } catch {
    sendError(ctx, 422, 'Validation Failed: content is not valid base64');
    return;
  }
  const branchTip = store.getRef(repo, store.refName(branch));
  const existing = branchTip === undefined ? null : store.getBlobAt(repo, branchTip, path);
  if (sha !== null) {
    if (existing === null || existing.oid !== sha) {
      sendError(ctx, 409, 'Conflict: the file has changed since the given sha');
      return;
    }
  } else if (existing !== null) {
    sendError(ctx, 422, `Validation Failed: ${path} already exists`);
    return;
  }
  // A branch that does not exist yet is created from the default branch tip
  // (or as the repository's first commit when the repository is unborn).
  if (branchTip === undefined && branch !== repo.defaultBranch && store.headTip(repo) !== undefined) {
    store.setRef(repo, store.refName(branch), store.headTip(repo) as string);
  }
  const blobOid = store.putObject(repo, 'blob', content);
  const commitOid = contentsCommit(store, repo, branch, new Map([[path, blobOid]]), message);
  const isUpdate = existing !== null;
  sendJson(ctx, isUpdate ? 200 : 201, {
    content: {
      name: path.split('/').pop() ?? path,
      path,
      sha: blobOid,
      size: content.length,
      type: 'file',
      encoding: 'base64',
      content: b64encode(content),
    },
    commit: { sha: commitOid, message },
  });
}

function getContents(ctx: Ctx, repo: Repo, pathParts: string[]): void {
  const store = ctx.store;
  const path = pathParts.join('/');
  const ref = ctx.query.get('ref') ?? null;
  const tip = resolveRefTip(store, repo, ref);
  if (tip === undefined) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  const commit = store.readCommit(repo, tip);
  if (commit === null) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  if (path === '') {
    sendJson(ctx, 200, directoryEntries(store, repo, commit.tree, ''));
    return;
  }
  const flat = store.flattenTree(repo, commit.tree);
  const file = flat.get(path);
  if (file !== undefined) {
    const blob = store.getObject(repo, file.oid);
    if (blob === undefined) {
      sendError(ctx, 404, 'Not Found');
      return;
    }
    sendJson(ctx, 200, {
      name: path.split('/').pop() ?? path,
      path,
      sha: file.oid,
      size: blob.data.length,
      type: 'file',
      encoding: 'base64',
      content: b64encode(blob.data),
    });
    return;
  }
  // A directory: list its immediate children.
  const prefix = `${path}/`;
  const children = new Map<string, { oid: string; type: 'file' | 'dir'; mode: string }>();
  for (const [filePath, entry] of flat) {
    if (!filePath.startsWith(prefix)) continue;
    const rest = filePath.slice(prefix.length);
    const slash = rest.indexOf('/');
    if (slash < 0) children.set(rest, { oid: entry.oid, type: 'file', mode: entry.mode });
    else children.set(rest.slice(0, slash), { oid: '', type: 'dir', mode: '40000' });
  }
  if (children.size === 0) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  // Directory entries need their tree shas: resolve them from the tree walk.
  const entries = directoryEntries(store, repo, commit.tree, path);
  sendJson(ctx, 200, entries);
}

function directoryEntries(store: Store, repo: Repo, treeOid: string, dirPath: string): Record<string, unknown>[] {
  const segments = dirPath === '' ? [] : dirPath.split('/');
  let current = treeOid;
  for (const segment of segments) {
    const object = store.getObject(repo, current);
    if (object === undefined || object.type !== 'tree') return [];
    const entry = parseTree(object.data).find((item) => item.name === segment && item.mode === '40000');
    if (entry === undefined) return [];
    current = entry.oid;
  }
  const object = store.getObject(repo, current);
  if (object === undefined || object.type !== 'tree') return [];
  const out: Record<string, unknown>[] = [];
  for (const entry of parseTree(object.data)) {
    const entryPath = dirPath === '' ? entry.name : `${dirPath}/${entry.name}`;
    if (entry.mode === '40000') {
      out.push({ type: 'dir', name: entry.name, path: entryPath, sha: entry.oid });
    } else {
      const blob = store.getObject(repo, entry.oid);
      out.push({ type: 'file', name: entry.name, path: entryPath, sha: entry.oid, size: blob?.data.length ?? 0 });
    }
  }
  out.sort((a, b) => String(a.name).localeCompare(String(b.name)));
  return out;
}

function deleteContents(ctx: Ctx, repo: Repo, pathParts: string[]): void {
  const store = ctx.store;
  const path = pathParts.join('/');
  const body = readBodyJson(ctx);
  if (!body.ok) {
    sendError(ctx, body.status, body.message);
    return;
  }
  const message = typeof body.value.message === 'string' && body.value.message !== '' ? body.value.message : 'Delete file';
  const sha = typeof body.value.sha === 'string' ? body.value.sha : null;
  const branch = typeof body.value.branch === 'string' && body.value.branch !== '' ? body.value.branch : repo.defaultBranch;
  const branchTip = store.getRef(repo, store.refName(branch));
  const existing = branchTip === undefined ? null : store.getBlobAt(repo, branchTip, path);
  if (existing === null) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  if (sha !== null && existing.oid !== sha) {
    sendError(ctx, 409, 'Conflict: the file has changed since the given sha');
    return;
  }
  const commitOid = contentsCommit(store, repo, branch, new Map([[path, null]]), message);
  sendJson(ctx, 200, { content: null, commit: { sha: commitOid, message } });
}

// ---------------------------------------------------------------- labels

function createLabel(ctx: Ctx, repo: Repo): void {
  const body = readBodyJson(ctx);
  if (!body.ok) {
    sendError(ctx, body.status, body.message);
    return;
  }
  const name = typeof body.value.name === 'string' ? body.value.name.trim() : '';
  const color = typeof body.value.color === 'string' ? body.value.color.replace(/^#/, '') : 'ededed';
  if (name === '') {
    sendError(ctx, 422, 'Validation Failed: name is required');
    return;
  }
  if (repo.labels.some((label) => label.name === name)) {
    sendError(ctx, 422, `Validation Failed: label ${name} already exists`);
    return;
  }
  const label: Label = { name, color, description: typeof body.value.description === 'string' ? body.value.description : '' };
  repo.labels.push(label);
  sendJson(ctx, 201, labelJson(label));
}

// ---------------------------------------------------------------- issues

function findIssue(repo: Repo, number: number): Issue | undefined {
  return repo.issues.find((issue) => issue.number === number);
}

function createIssue(ctx: Ctx, repo: Repo): void {
  const body = readBodyJson(ctx);
  if (!body.ok) {
    sendError(ctx, body.status, body.message);
    return;
  }
  const title = typeof body.value.title === 'string' ? body.value.title : '';
  if (title === '') {
    sendError(ctx, 422, 'Validation Failed: title is required');
    return;
  }
  const labels: string[] = Array.isArray(body.value.labels) ? body.value.labels.filter((entry): entry is string => typeof entry === 'string') : [];
  for (const name of labels) {
    if (!repo.labels.some((label) => label.name === name)) {
      sendError(ctx, 422, `Validation Failed: label ${name} does not exist`);
      return;
    }
  }
  const seq = ctx.store.seq();
  const issue: Issue = {
    id: ctx.store.nextGlobalId(),
    number: repo.nextNumber,
    title,
    body: typeof body.value.body === 'string' ? body.value.body : '',
    state: 'open',
    labels,
    comments: [],
    createdSeq: seq,
    updatedSeq: seq,
  };
  repo.nextNumber += 1;
  repo.issues.push(issue);
  sendJson(ctx, 201, issueJson(ctx.store, repo, issue));
}

function listIssues(ctx: Ctx, repo: Repo): void {
  const state = (ctx.query.get('state') ?? 'open').toLowerCase();
  const sort = (ctx.query.get('sort') ?? 'created').toLowerCase();
  const direction = (ctx.query.get('direction') ?? 'desc').toLowerCase();
  let issues = repo.issues;
  if (state === 'open') issues = issues.filter((issue) => issue.state === 'open');
  else if (state === 'closed') issues = issues.filter((issue) => issue.state === 'closed');
  else if (state !== 'all') issues = issues.filter((issue) => issue.state === 'open');
  const sorted = [...issues];
  if (sort === 'created') sorted.sort((a, b) => (direction === 'asc' ? a.createdSeq - b.createdSeq : b.createdSeq - a.createdSeq));
  else if (sort === 'updated') sorted.sort((a, b) => (direction === 'asc' ? a.updatedSeq - b.updatedSeq : b.updatedSeq - a.updatedSeq));
  else if (sort === 'comments') sorted.sort((a, b) => (direction === 'asc' ? a.comments.length - b.comments.length : b.comments.length - a.comments.length));
  const params = pageParams(ctx);
  const link = linkHeader(ctx, params.page, params.perPage, sorted.length);
  sendJson(ctx, 200, pageSlice(sorted, params).map((issue) => issueJson(ctx.store, repo, issue)), link === null ? {} : { link });
}

function getIssue(ctx: Ctx, repo: Repo, number: number): void {
  const issue = findIssue(repo, number);
  if (issue === undefined) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  sendJson(ctx, 200, issueJson(ctx.store, repo, issue));
}

function patchIssue(ctx: Ctx, repo: Repo, number: number): void {
  const issue = findIssue(repo, number);
  if (issue === undefined) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  const body = readBodyJson(ctx);
  if (!body.ok) {
    sendError(ctx, body.status, body.message);
    return;
  }
  if (typeof body.value.title === 'string') issue.title = body.value.title;
  if (typeof body.value.body === 'string') issue.body = body.value.body;
  if (body.value.state === 'open' || body.value.state === 'closed') issue.state = body.value.state;
  issue.updatedSeq = ctx.store.seq();
  sendJson(ctx, 200, issueJson(ctx.store, repo, issue));
}

function createComment(ctx: Ctx, repo: Repo, number: number): void {
  const issue = findIssue(repo, number);
  if (issue === undefined) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  const body = readBodyJson(ctx);
  if (!body.ok) {
    sendError(ctx, body.status, body.message);
    return;
  }
  const text = typeof body.value.body === 'string' ? body.value.body : '';
  const comment = { id: repo.nextCommentId, body: text, author: VIEWER_LOGIN, createdSeq: ctx.store.seq() };
  repo.nextCommentId += 1;
  issue.comments.push(comment);
  issue.updatedSeq = ctx.store.seq();
  sendJson(ctx, 201, commentJson(comment));
}

function listComments(ctx: Ctx, repo: Repo, number: number): void {
  const issue = findIssue(repo, number);
  if (issue === undefined) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  sendJson(ctx, 200, issue.comments.map(commentJson));
}

// ---------------------------------------------------------------- pulls

function findPull(repo: Repo, number: number): Pull | undefined {
  return repo.pulls.find((pull) => pull.number === number);
}

export function createPullRecord(store: Store, repo: Repo, input: { title: string; body: string; head: string; base: string }): { pull: Pull | null; error: string | null } {
  const headTip = store.getRef(repo, store.refName(input.head));
  const baseTip = store.getRef(repo, store.refName(input.base));
  if (headTip === undefined || baseTip === undefined) return { pull: null, error: 'head and base branches must exist' };
  const existing = repo.pulls.find((pull) => pull.headRef === input.head && pull.baseRef === input.base && pull.state === 'open');
  if (existing !== undefined) return { pull: null, error: `a pull request from ${input.head} is already open` };
  const seq = store.seq();
  const pull: Pull = {
    id: store.nextGlobalId(),
    number: repo.nextNumber,
    title: input.title,
    body: input.body,
    state: 'open',
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    headRef: input.head,
    baseRef: input.base,
    headSha: headTip,
    baseSha: baseTip,
    createdSeq: seq,
    updatedSeq: seq,
    comments: [],
  };
  repo.nextNumber += 1;
  repo.pulls.push(pull);
  return { pull, error: null };
}

function createPull(ctx: Ctx, repo: Repo): void {
  const body = readBodyJson(ctx);
  if (!body.ok) {
    sendError(ctx, body.status, body.message);
    return;
  }
  const title = typeof body.value.title === 'string' ? body.value.title : '';
  const head = typeof body.value.head === 'string' ? body.value.head : '';
  const base = typeof body.value.base === 'string' ? body.value.base : '';
  if (title === '' || head === '' || base === '') {
    sendError(ctx, 422, 'Validation Failed: title, head and base are required');
    return;
  }
  const { pull, error } = createPullRecord(ctx.store, repo, { title, body: typeof body.value.body === 'string' ? body.value.body : '', head, base });
  if (pull === null) {
    sendError(ctx, 422, `Validation Failed: ${error ?? 'cannot create pull request'}`);
    return;
  }
  sendJson(ctx, 201, pullJson(ctx.store, repo, pull));
}

function listPulls(ctx: Ctx, repo: Repo): void {
  const state = (ctx.query.get('state') ?? 'open').toLowerCase();
  let pulls = repo.pulls;
  if (state === 'open') pulls = pulls.filter((pull) => pull.state === 'open');
  else if (state === 'closed') pulls = pulls.filter((pull) => pull.state === 'closed');
  else if (state !== 'all') pulls = pulls.filter((pull) => pull.state === 'open');
  const sorted = [...pulls].sort((a, b) => b.createdSeq - a.createdSeq);
  const params = pageParams(ctx);
  const link = linkHeader(ctx, params.page, params.perPage, sorted.length);
  sendJson(ctx, 200, pageSlice(sorted, params).map((pull) => pullJson(ctx.store, repo, pull)), link === null ? {} : { link });
}

function getPull(ctx: Ctx, repo: Repo, number: number): void {
  const pull = findPull(repo, number);
  if (pull === undefined) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  sendJson(ctx, 200, pullJson(ctx.store, repo, pull));
}

/** The changed files of a pull request, with line counts. */
export function pullFiles(store: Store, repo: Repo, pull: Pull): { filename: string; status: string; additions: number; deletions: number; changes: number }[] {
  const baseTip = store.getRef(repo, store.refName(pull.baseRef)) ?? pull.baseSha;
  const headTip = store.getRef(repo, store.refName(pull.headRef)) ?? pull.headSha;
  const baseCommit = store.readCommit(repo, baseTip);
  const headCommit = store.readCommit(repo, headTip);
  if (baseCommit === null || headCommit === null) return [];
  const diffs = store.diffTrees(repo, baseCommit.tree, headCommit.tree);
  const out: { filename: string; status: string; additions: number; deletions: number; changes: number }[] = [];
  for (const diff of diffs) {
    const oldText = diff.oldOid === null ? null : (store.getObject(repo, diff.oldOid)?.data.toString('utf8') ?? null);
    const newText = diff.newOid === null ? null : (store.getObject(repo, diff.newOid)?.data.toString('utf8') ?? null);
    const counts = lineDiff(oldText, newText);
    out.push({
      filename: diff.path,
      status: diff.status === 'added' ? 'added' : diff.status === 'deleted' ? 'removed' : 'modified',
      additions: counts.additions,
      deletions: counts.deletions,
      changes: counts.additions + counts.deletions,
    });
  }
  return out;
}

function listPullFiles(ctx: Ctx, repo: Repo, number: number): void {
  const pull = findPull(repo, number);
  if (pull === undefined) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  sendJson(ctx, 200, pullFiles(ctx.store, repo, pull));
}

/**
 * Merge a pull request. Returns the merge commit sha, or null with a status
 * (404 unknown, 405 conflicted or already merged).
 */
export function mergePullRecord(store: Store, repo: Repo, pull: Pull, method: string): { sha: string | null; status: number; message: string } {
  if (pull.state !== 'open') return { sha: null, status: 405, message: 'Pull request is already closed' };
  const baseTip = store.getRef(repo, store.refName(pull.baseRef));
  const headTip = store.getRef(repo, store.refName(pull.headRef));
  if (baseTip === undefined || headTip === undefined) return { sha: null, status: 405, message: 'Head or base branch is missing' };
  const merged = store.mergeTrees(repo, baseTip, headTip);
  if (merged === null) return { sha: null, status: 405, message: 'Merge conflict' };
  let mergeSha: string;
  if (method === 'merge') {
    mergeSha = store.createCommit(repo, {
      tree: merged.treeOid,
      parents: [baseTip, headTip],
      message: `Merge pull request #${pull.number} from ${repo.owner}/${pull.headRef}`,
    });
  } else if (method === 'rebase') {
    // Replay the head's commits (those not reachable from the base) onto the base tip.
    const headOnly = store.walkCommits(repo, [headTip]).filter((oid) => !store.isAncestor(repo, oid, baseTip));
    headOnly.reverse();
    let tip = baseTip;
    let last = baseTip;
    for (const oid of headOnly) {
      const commit = store.readCommit(repo, oid);
      if (commit === null) return { sha: null, status: 405, message: 'Merge conflict' };
      const parentTree = commit.parents.length > 0 ? (store.readCommit(repo, commit.parents[0] as string)?.tree ?? null) : null;
      const currentTree = store.readCommit(repo, tip)?.tree ?? null;
      const rebased = rebaseTree(store, repo, parentTree, currentTree, commit.tree);
      if (rebased === null) return { sha: null, status: 405, message: 'Merge conflict' };
      last = store.createCommit(repo, { tree: rebased, parents: [tip], message: commit.message });
      tip = last;
    }
    mergeSha = last;
    if (headOnly.length === 0) mergeSha = baseTip;
  } else {
    // squash (the default)
    mergeSha = store.createCommit(repo, { tree: merged.treeOid, parents: [baseTip], message: pull.title });
  }
  store.setRef(repo, store.refName(pull.baseRef), mergeSha);
  pull.state = 'closed';
  pull.merged = true;
  pull.mergedAt = store.clock.nextIso();
  pull.mergeCommitSha = mergeSha;
  pull.updatedSeq = store.seq();
  return { sha: mergeSha, status: 200, message: 'Pull request successfully merged' };
}

/** Apply one commit's tree change onto another tree (rebase step). */
function rebaseTree(store: Store, repo: Repo, parentTree: string | null, currentTree: string | null, commitTree: string): string | null {
  const baseFlat = new Map([...store.flattenTree(repo, parentTree)].map(([path, file]) => [path, file.oid] as [string, string | null]));
  const oursFlat = new Map([...store.flattenTree(repo, currentTree)].map(([path, file]) => [path, file.oid] as [string, string | null]));
  const theirsFlat = new Map([...store.flattenTree(repo, commitTree)].map(([path, file]) => [path, file.oid] as [string, string | null]));
  const { merged, conflicts } = store.mergeFlatTrees(baseFlat, oursFlat, theirsFlat);
  if (conflicts.length > 0) return null;
  const flat = new Map<string, { oid: string; mode: string }>();
  const theirsModes = store.flattenTree(repo, commitTree);
  const oursModes = store.flattenTree(repo, currentTree);
  for (const [path, oid] of merged) {
    if (oid === null) continue;
    flat.set(path, { oid, mode: theirsModes.get(path)?.mode ?? oursModes.get(path)?.mode ?? '100644' });
  }
  return store.treeFromFlat(repo, flat);
}

function mergePull(ctx: Ctx, repo: Repo, number: number): void {
  const pull = findPull(repo, number);
  if (pull === undefined) {
    sendError(ctx, 404, 'Not Found');
    return;
  }
  const body = readBodyJson(ctx);
  if (!body.ok) {
    sendError(ctx, body.status, body.message);
    return;
  }
  const method = typeof body.value.merge_method === 'string' ? body.value.merge_method.toLowerCase() : 'squash';
  const result = mergePullRecord(ctx.store, repo, pull, method);
  if (result.sha === null) {
    sendError(ctx, result.status, result.message);
    return;
  }
  sendJson(ctx, 200, { sha: result.sha, merged: true, message: result.message });
}
