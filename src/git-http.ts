// Git smart HTTP: info/refs advertisements (protocol v0 and v2),
// git-upload-pack (fetch and ls-refs, v0 and v2) and git-receive-pack
// (push with report-status over sideband). The wire shapes follow the
// suite's git-client (program/env/src/git-client.ts) exactly: it is the
// only client that ever speaks to this endpoint.

import { firstFlushOffset, parsePktLines, PKT_DELIM, PKT_FLUSH, pktLine, readPack, sidebandPack, writePack, type GitObject, type PktLine } from './gitcore.ts';
import { isAuthorized, sendBytes, sendError, type Ctx } from './http.ts';
import type { Repo, Store } from './store.ts';
import { ZERO_OID } from './util.ts';

const UPLOAD_AD = 'application/x-git-upload-pack-advertisement';
const RECEIVE_AD = 'application/x-git-receive-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';

const AGENT = 'gitforge/1.0';
const UPLOAD_V0_CAPS = `symref=HEAD:refs/heads/main side-band-64k thin-pack ofs-delta multi_ack_detailed shallow agent=${AGENT}`;
const RECEIVE_V0_CAPS = `report-status side-band-64k delete-refs ofs-delta agent=${AGENT}`;

/** Parse a space-separated capability list into key -> value (null for bare keys). */
function parseCaps(text: string): Map<string, string | null> {
  const out = new Map<string, string | null>();
  for (const part of text.split(' ').filter((piece) => piece !== '')) {
    const eq = part.indexOf('=');
    if (eq < 0) out.set(part, null);
    else out.set(part.slice(0, eq), part.slice(eq + 1));
  }
  return out;
}

/** The route's owner/repo, from `/:owner/:repo.git/...` or `/:owner/:repo/...`. */
function parseGitPath(path: string): { owner: string; repo: string; rest: string } | null {
  const parts = path.split('/').filter((piece) => piece !== '');
  if (parts.length < 3) return null;
  const owner = decodeURIComponent(parts[0] ?? '');
  let repo = decodeURIComponent(parts[1] ?? '');
  if (repo.endsWith('.git')) repo = repo.slice(0, -4);
  const rest = parts.slice(2).join('/');
  if (owner === '' || repo === '' || rest === '') return null;
  return { owner, repo, rest };
}

/** The v0 advertisement: service preamble, flush, ref lines, flush. */
function advertiseV0(store: Store, repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const caps = service === 'git-upload-pack' ? UPLOAD_V0_CAPS : RECEIVE_V0_CAPS;
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), PKT_FLUSH];
  const refs = store.listRefs(repo);
  const lines: string[] = [];
  if (refs.length === 0) {
    lines.push(`${ZERO_OID} capabilities^{}\0${caps}\n`);
  } else {
    const headTip = store.headTip(repo);
    let first = true;
    if (headTip !== undefined) {
      lines.push(`${headTip} HEAD\0${caps}\n`);
      first = false;
    }
    for (const ref of refs) {
      const prefix = first ? `\0${caps}` : '';
      first = false;
      lines.push(`${ref.oid} ${ref.name}${prefix}\n`);
      // Annotated tags peel to their commit.
      if (ref.name.startsWith('refs/tags/')) {
        const object = store.getObject(repo, ref.oid);
        if (object !== undefined && object.type === 'tag') {
          const target = peelTag(object.data);
          if (target !== null) lines.push(`${target} ${ref.name}^{}\n`);
        }
      }
    }
  }
  for (const line of lines) parts.push(pktLine(line));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

/** The target of an annotated tag object, or null. */
function peelTag(data: Buffer): string | null {
  const match = /^object ([0-9a-f]{40})$/m.exec(data.toString('utf8'));
  return match !== null ? (match[1] as string) : null;
}

/** The v2 advertisement: `version 2` plus capability lines, then flush. */
function advertiseV2(): Buffer {
  const lines = [
    'version 2\n',
    `agent=${AGENT}\n`,
    'ls-refs=unborn\n',
    'fetch=shallow thin-pack\n',
    'object-format=sha1\n',
  ];
  const parts = lines.map((line) => pktLine(line));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- ls-refs (v2)

interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

function lsRefsV2(store: Store, repo: Repo, args: LsRefsArgs): Buffer {
  const parts: Buffer[] = [];
  const refs = store.listRefs(repo);
  const matches = (name: string): boolean => args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  const defaultRef = `refs/heads/${repo.defaultBranch}`;
  // HEAD is a ref like any other for prefix filtering: it is listed when
  // 'HEAD' starts with one of the requested prefixes (or none were given).
  // An unborn repository has no refs at all, so nothing is listed: the
  // client learns the repository is empty and never fetches.
  if (store.headTip(repo) !== undefined && matches('HEAD')) {
    const headTip = store.headTip(repo) as string;
    const attrs = args.symrefs ? ` symref-target:${defaultRef}` : '';
    parts.push(pktLine(`${headTip} HEAD${attrs}\n`));
  }
  for (const ref of refs) {
    if (!matches(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (args.peel && ref.name.startsWith('refs/tags/')) {
      const object = store.getObject(repo, ref.oid);
      if (object !== undefined && object.type === 'tag') {
        const target = peelTag(object.data);
        if (target !== null) line += ` peeled:${target}`;
      }
    }
    parts.push(pktLine(`${line}\n`));
  }
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- fetch

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  thinPack: boolean;
  /** Capability names the client put on its want lines (v0 multi_ack etc.). */
  features: string[];
  /** The client sent `done`: negotiation is over and the pack may follow. */
  done: boolean;
}

function parseFetchArgs(lines: readonly PktLine[]): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null, thinPack: false, features: [], done: false };
  for (const line of lines) {
    if (line.kind !== 'data') continue;
    const text = line.data.toString('utf8').trim();
    if (text.startsWith('want ')) {
      const rest = text.slice(5).trim();
      const space = rest.indexOf(' ');
      args.wants.push(space < 0 ? rest : rest.slice(0, space));
      if (space >= 0) for (const feature of rest.slice(space + 1).split(/\s+/)) if (feature !== '') args.features.push(feature);
    } else if (text.startsWith('have ')) args.haves.push(text.slice(5).trim());
    else if (text.startsWith('shallow ')) args.shallow.push(text.slice(8).trim());
    else if (text.startsWith('deepen ')) {
      const n = Number(text.slice(7).trim());
      if (Number.isFinite(n) && n > 0) args.depth = Math.trunc(n);
    } else if (text === 'thin-pack') args.thinPack = true;
    else if (text === 'done') args.done = true;
    // no-progress, ofs-delta, filter, deepen-since, deepen-not: accepted, no effect.
  }
  return args;
}

interface FetchPlan {
  objects: GitObject[];
  shallow: string[];
  unshallow: string[];
}

/**
 * The objects a fetch sends: the wants' closure (limited to `depth`
 * commit generations when given), minus what the client already holds
 * (the haves' closure, stopping at the client's shallow boundary).
 * Also the new shallow boundary and the boundaries that are no longer
 * shallow (a deepen).
 */
function planFetch(store: Store, repo: Repo, args: FetchArgs): FetchPlan {
  // Peel tag wants to their target commit; keep the tag objects themselves.
  const extra = new Set<string>();
  const roots: string[] = [];
  for (const want of args.wants) {
    const object = store.getObject(repo, want);
    if (object === undefined) continue;
    if (object.type === 'tag') {
      extra.add(want);
      const target = peelTag(object.data);
      if (target !== null) roots.push(target);
    } else if (object.type === 'commit') {
      roots.push(want);
    } else {
      extra.add(want);
    }
  }
  // Walk commit generations from the roots (BFS, generation 1 = the roots).
  const genOf = new Map<string, number>();
  const queue: { oid: string; gen: number }[] = roots.map((oid) => ({ oid, gen: 1 }));
  while (queue.length > 0) {
    const { oid, gen } = queue.shift() as { oid: string; gen: number };
    if (genOf.has(oid)) continue;
    if (args.depth !== null && gen > args.depth) continue;
    const commit = store.readCommit(repo, oid);
    if (commit === null) continue;
    genOf.set(oid, gen);
    if (args.depth === null || gen < args.depth) {
      for (const parent of commit.parents) queue.push({ oid: parent, gen: gen + 1 });
    }
  }
  const included = new Set(genOf.keys());
  // The objects the client already holds.
  const held = store.reachableObjects(repo, args.haves, new Set(args.shallow));
  // The send set: each included commit plus its tree closure, minus held.
  const send = new Set<string>(extra);
  for (const oid of included) {
    send.add(oid);
    const commit = store.readCommit(repo, oid);
    if (commit === null) continue;
    const stack = [commit.tree];
    while (stack.length > 0) {
      const at = stack.pop() as string;
      if (send.has(at)) continue;
      const object = store.getObject(repo, at);
      if (object === undefined) continue;
      send.add(at);
      if (object.type === 'tree') {
        const entries = store.readTree(repo, at);
        if (entries !== null) for (const entry of entries) stack.push(entry.oid);
      }
    }
  }
  for (const oid of held) send.delete(oid);
  const objects: GitObject[] = [];
  for (const oid of [...send].sort()) {
    const object = store.getObject(repo, oid);
    if (object !== undefined) objects.push(object);
  }
  let shallow: string[] = [];
  let unshallow: string[] = [];
  if (args.depth !== null) {
    for (const [oid, gen] of genOf) if (gen === args.depth) shallow.push(oid);
    shallow.sort();
    const old = new Set(args.shallow);
    for (const oid of old) {
      if (included.has(oid) && !shallow.includes(oid)) unshallow.push(oid);
    }
    unshallow.sort();
  }
  return { objects, shallow, unshallow };
}

/** A v2 fetch response: acknowledgments, shallow-info, then the packfile section. */
function fetchV2(store: Store, repo: Repo, args: FetchArgs): Buffer {
  const plan = planFetch(store, repo, args);
  const parts: Buffer[] = [];
  if (args.haves.length > 0) {
    parts.push(pktLine('acknowledgments\n'));
    const common = args.haves.filter((have) => store.hasObject(repo, have));
    if (common.length === 0) parts.push(pktLine('NAK\n'));
    else for (const oid of common) parts.push(pktLine(`ACK ${oid}\n`));
    if (args.done) parts.push(pktLine('ready\n'));
    parts.push(PKT_DELIM);
  }
  if (args.depth !== null) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(PKT_DELIM);
  }
  parts.push(pktLine('packfile\n'));
  // Always send a pack, even an empty one: a client that asked for the zero
  // oid (an empty repository's HEAD) must still receive a well-formed pack
  // to index, rather than an empty packfile section.
  parts.push(sidebandPack(writePack(plan.objects)));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

/** A v0 fetch response: ACK/NAK (when multi_ack was negotiated), shallow lines, then the sideband pack. */
function fetchV0(store: Store, repo: Repo, args: FetchArgs): Buffer {
  const plan = planFetch(store, repo, args);
  const parts: Buffer[] = [];
  if (args.features.includes('multi_ack') || args.features.includes('multi_ack_detailed')) {
    const common = args.haves.filter((have) => store.hasObject(repo, have));
    if (common.length === 0) parts.push(pktLine('NAK\n'));
    else for (const oid of common) parts.push(pktLine(`ACK ${oid}\n`));
  }
  if (args.depth !== null) {
    for (const oid of plan.shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
  }
  parts.push(sidebandPack(writePack(plan.objects)));
  parts.push(PKT_FLUSH);
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- receive-pack

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

interface PushReport {
  unpackOk: boolean;
  unpackDetail: string;
  refs: { ref: string; ok: boolean; reason: string }[];
}

/** Apply a push's commands to the refs, after the pack has been stored. */
function applyPush(store: Store, repo: Repo, commands: readonly PushCommand[]): PushReport {
  const report: PushReport = { unpackOk: true, unpackDetail: '', refs: [] };
  for (const command of commands) {
    const current = store.getRef(repo, command.ref);
    if (command.oldOid === ZERO_OID && command.newOid === ZERO_OID) {
      report.refs.push({ ref: command.ref, ok: false, reason: 'invalid command' });
      continue;
    }
    if (command.newOid === ZERO_OID) {
      // Delete.
      if (current === undefined) {
        report.refs.push({ ref: command.ref, ok: false, reason: 'remote ref does not exist' });
      } else if (current !== command.oldOid) {
        report.refs.push({ ref: command.ref, ok: false, reason: 'stale info' });
      } else {
        store.deleteRef(repo, command.ref);
        report.refs.push({ ref: command.ref, ok: true, reason: '' });
      }
      continue;
    }
    if (command.oldOid === ZERO_OID) {
      // Create.
      if (current !== undefined) {
        report.refs.push({ ref: command.ref, ok: false, reason: 'already exists' });
        continue;
      }
      if (!closureComplete(store, repo, command.newOid)) {
        report.refs.push({ ref: command.ref, ok: false, reason: 'missing objects' });
        continue;
      }
      store.setRef(repo, command.ref, command.newOid);
      report.refs.push({ ref: command.ref, ok: true, reason: '' });
      continue;
    }
    // Update.
    if (current === undefined) {
      report.refs.push({ ref: command.ref, ok: false, reason: 'remote ref does not exist' });
    } else if (current !== command.oldOid) {
      report.refs.push({ ref: command.ref, ok: false, reason: 'stale info' });
    } else if (!closureComplete(store, repo, command.newOid)) {
      report.refs.push({ ref: command.ref, ok: false, reason: 'missing objects' });
    } else if (!store.isAncestor(repo, command.oldOid, command.newOid)) {
      report.refs.push({ ref: command.ref, ok: false, reason: 'non-fast-forward' });
    } else {
      store.setRef(repo, command.ref, command.newOid);
      report.refs.push({ ref: command.ref, ok: true, reason: '' });
    }
  }
  return report;
}

/**
 * Whether the tip's whole closure (commits, trees, blobs) is in the store.
 *
 * Two caches keep this cheap on the push path: commits and trees are parsed
 * once per repository (Store.readCommit/readTree), and commits whose closure
 * was already verified are remembered on the repo (verifiedTips) — a push
 * only ever adds objects, so a complete closure stays complete and the
 * walk stops at the previous tip instead of re-walking the whole history.
 * Only commits reached by a walk that completed are remembered; a failed
 * walk remembers nothing.
 */
function closureComplete(store: Store, repo: Repo, tip: string): boolean {
  const seen = new Set<string>();
  const visitedCommits: string[] = [];
  const stack = [tip];
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = store.getObject(repo, oid);
    if (object === undefined) return false;
    if (object.type === 'commit') {
      if (repo.verifiedTips.has(oid)) continue;
      const commit = store.readCommit(repo, oid);
      if (commit === null || !store.hasObject(repo, commit.tree)) return false;
      visitedCommits.push(oid);
      for (const parent of commit.parents) stack.push(parent);
      stack.push(commit.tree);
    } else if (object.type === 'tree') {
      const entries = store.readTree(repo, oid);
      if (entries === null) return false;
      for (const entry of entries) stack.push(entry.oid);
    } else if (object.type === 'tag') {
      const target = peelTag(object.data);
      if (target !== null) stack.push(target);
    }
  }
  for (const oid of visitedCommits) repo.verifiedTips.add(oid);
  return true;
}

/** The report-status stream, pkt-lined, optionally wrapped in sideband band 1. */
function reportStream(report: PushReport, sideband: boolean): Buffer {
  const inner: Buffer[] = [];
  inner.push(pktLine(report.unpackOk ? 'unpack ok\n' : `unpack ${report.unpackDetail}\n`));
  for (const ref of report.refs) {
    inner.push(pktLine(ref.ok ? `ok ${ref.ref}\n` : `ng ${ref.ref} ${ref.reason}\n`));
  }
  inner.push(PKT_FLUSH);
  const body = Buffer.concat(inner);
  if (!sideband) return body;
  const payload = Buffer.concat([Buffer.from([1]), body]);
  return Buffer.concat([pktLine(payload), PKT_FLUSH]);
}

// ---------------------------------------------------------------- dispatch

/** Handle a git smart-HTTP request. Returns true when it was a git route. */
export function handleGit(ctx: Ctx): boolean {
  const parsed = parseGitPath(ctx.path);
  if (parsed === null) return false;
  const { owner, repo: repoName, rest } = parsed;
  if (rest !== 'info/refs' && rest !== 'git-upload-pack' && rest !== 'git-receive-pack') return false;
  if (!isAuthorized(ctx)) {
    sendError(ctx, 401, 'Requires authentication');
    return true;
  }
  const repo = ctx.store.getRepo(owner, repoName);
  if (repo === undefined) {
    sendError(ctx, 404, 'Not Found');
    return true;
  }
  if (ctx.method === 'GET' && rest === 'info/refs') {
    const service = ctx.query.get('service') ?? '';
    if (service === 'git-upload-pack') {
      const v2 = (ctx.req.headers['git-protocol'] ?? '') === 'version=2';
      sendBytes(ctx, 200, v2 ? advertiseV2() : advertiseV0(ctx.store, repo, 'git-upload-pack'), UPLOAD_AD);
      return true;
    }
    if (service === 'git-receive-pack') {
      sendBytes(ctx, 200, advertiseV0(ctx.store, repo, 'git-receive-pack'), RECEIVE_AD);
      return true;
    }
    sendError(ctx, 400, 'Unknown service');
    return true;
  }
  if (ctx.method === 'POST' && rest === 'git-upload-pack') {
    try {
      const lines = parsePktLines(ctx.body);
      const v2 = (ctx.req.headers['git-protocol'] ?? '') === 'version=2';
      if (v2) {
        const command = lines.find((line) => line.kind === 'data')?.data.toString('utf8').trim() ?? '';
        if (command === 'command=ls-refs') {
          const args = parseLsRefsArgs(lines);
          sendBytes(ctx, 200, lsRefsV2(ctx.store, repo, args), UPLOAD_RESULT);
          return true;
        }
        if (command === 'command=fetch') {
          const args = parseFetchArgs(lines);
          sendBytes(ctx, 200, fetchV2(ctx.store, repo, args), UPLOAD_RESULT);
          return true;
        }
        sendError(ctx, 400, `Unknown upload-pack command ${JSON.stringify(command)}`);
        return true;
      }
      const args = parseFetchArgs(lines);
      sendBytes(ctx, 200, fetchV0(ctx.store, repo, args), UPLOAD_RESULT);
      return true;
    } catch (err) {
      sendError(ctx, 400, `Malformed upload-pack request: ${(err as Error).message}`);
      return true;
    }
  }
  if (ctx.method === 'POST' && rest === 'git-receive-pack') {
    try {
      const flushAt = firstFlushOffset(ctx.body);
      if (flushAt < 0) {
        sendError(ctx, 400, 'Malformed receive-pack request: no command flush');
        return true;
      }
      const commandLines = parsePktLines(ctx.body.subarray(0, flushAt));
      const commands: PushCommand[] = [];
      let sideband = false;
      for (const line of commandLines) {
        if (line.kind !== 'data') continue;
        const text = line.data.toString('utf8');
        const nul = text.indexOf('\0');
        const head = nul >= 0 ? text.slice(0, nul) : text;
        if (nul >= 0) {
          const caps = parseCaps(text.slice(nul + 1));
          sideband = caps.has('side-band-64k');
        }
        const match = /^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)\s*$/.exec(head.trim());
        if (match === null) continue;
        commands.push({ oldOid: match[1] as string, newOid: match[2] as string, ref: match[3] as string });
      }
      const report: PushReport = { unpackOk: true, unpackDetail: '', refs: [] };
      const hasPack = commands.some((command) => command.newOid !== ZERO_OID);
      if (hasPack) {
        const packBytes = ctx.body.subarray(flushAt);
        try {
          // packBytes is already a Buffer: readPack uses it as-is (no copy).
          const pack = readPack(packBytes, (oid) => ctx.store.getObject(repo, oid) ?? null);
          // readPack already hashed every entry: store by that oid instead of re-hashing.
          for (const [oid, object] of pack.byOid) ctx.store.putObject(repo, object.type, object.data, oid);
        } catch (err) {
          report.unpackOk = false;
          report.unpackDetail = (err as Error).message;
          report.refs = commands.map((command) => ({ ref: command.ref, ok: false, reason: 'unpack failed' }));
          sendBytes(ctx, 200, reportStream(report, sideband), RECEIVE_RESULT);
          return true;
        }
      }
      const applied = applyPush(ctx.store, repo, commands);
      sendBytes(ctx, 200, reportStream(applied, sideband), RECEIVE_RESULT);
      return true;
    } catch (err) {
      sendError(ctx, 400, `Malformed receive-pack request: ${(err as Error).message}`);
      return true;
    }
  }
  sendError(ctx, 404, 'Not Found');
  return true;
}

function parseLsRefsArgs(lines: readonly PktLine[]): LsRefsArgs {
  const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
  let afterDelim = false;
  for (const line of lines) {
    if (line.kind === 'delim') {
      afterDelim = true;
      continue;
    }
    if (line.kind !== 'data' || !afterDelim) continue;
    const text = line.data.toString('utf8').trim();
    if (text === 'peel') args.peel = true;
    else if (text === 'symrefs') args.symrefs = true;
    else if (text === 'unborn') args.unborn = true;
    else if (text.startsWith('ref-prefix ')) args.prefixes.push(text.slice('ref-prefix '.length).trim());
  }
  return args;
}
