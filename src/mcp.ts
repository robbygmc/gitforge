// MCP over POST /mcp: Streamable HTTP, legacy 2025-11-25 era.
// `server/discover` answers method-not-found (pre-discover server), then
// `initialize` + `notifications/initialized` handshakes, then tools/list
// and tools/call. Plain JSON answers (the suite's client also reads SSE,
// but plain JSON is accepted). Authentication is decided before any tool
// dispatch: no valid Bearer token -> HTTP 401.

import { isAuthorized, isRecord, parseJsonBody, sendJson, sendNoContent, type Ctx } from './http.ts';
import { VIEWER_LOGIN } from './util.ts';
import type { Issue, Pull, Repo, Store } from './store.ts';
import { createPullRecord, issueJson, mergePullRecord, pullJson, repoJson } from './rest.ts';

const SERVER_NAME = 'gitforge';
const SERVER_VERSION = '1.0.0';
const LEGACY_REVISIONS = ['2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

// ---------------------------------------------------------------- tools

interface McpToolDef {
  name: string;
  description: string;
  inputSchema: { type: 'object'; properties: Record<string, { type: string; description?: string }>; required: string[] };
}

const TOOLS: McpToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'The repository name.' },
        description: { type: 'string', description: 'A short description of the repository.' },
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The issue title.' },
        body: { type: 'string', description: 'The issue body.' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        state: { type: 'string', description: 'Filter by state: open, closed or all.' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        title: { type: 'string', description: 'The pull request title.' },
        head: { type: 'string', description: 'The head branch name.' },
        base: { type: 'string', description: 'The base branch name.' },
        body: { type: 'string', description: 'The pull request body.' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'The repository owner login.' },
        repo: { type: 'string', description: 'The repository name.' },
        pull_number: { type: 'integer', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

// ---------------------------------------------------------------- JSON-RPC

type JsonRpcId = string | number | null;

function rpcResult(id: JsonRpcId, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id, result };
}

function rpcError(id: JsonRpcId, code: number, message: string, data?: unknown): Record<string, unknown> {
  const error: Record<string, unknown> = { code, message };
  if (data !== undefined) error.data = data;
  return { jsonrpc: '2.0', id, error };
}

function toolText(payload: unknown): Record<string, unknown> {
  return { content: [{ type: 'text', text: typeof payload === 'string' ? payload : JSON.stringify(payload) }] };
}

function toolError(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

// ---------------------------------------------------------------- dispatch

function ownedRepo(store: Store, args: Record<string, unknown>): Repo | null {
  const owner = typeof args.owner === 'string' ? args.owner : '';
  const name = typeof args.repo === 'string' ? args.repo : '';
  if (owner.toLowerCase() !== VIEWER_LOGIN.toLowerCase()) return null;
  return store.getRepo(owner, name) ?? null;
}

function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  switch (name) {
    case 'create_repo': {
      const repoName = typeof args.name === 'string' ? args.name.trim() : '';
      if (repoName === '') return toolError('name is required');
      const repo = store.createRepo(VIEWER_LOGIN, repoName, {
        private: args.private === true,
        description: typeof args.description === 'string' ? args.description : '',
      });
      if (repo === null) return toolError(`Repository ${repoName} already exists`);
      return toolText(repoJson(store, repo));
    }
    case 'get_repo': {
      const repo = ownedRepo(store, args);
      if (repo === null) return toolError('Repository not found');
      return toolText(repoJson(store, repo));
    }
    case 'create_issue': {
      const repo = ownedRepo(store, args);
      if (repo === null) return toolError('Repository not found');
      const title = typeof args.title === 'string' ? args.title : '';
      if (title === '') return toolError('title is required');
      const seq = store.seq();
      const issue: Issue = {
        id: store.nextGlobalId(),
        number: repo.nextNumber,
        title,
        body: typeof args.body === 'string' ? args.body : '',
        state: 'open',
        labels: [],
        comments: [],
        createdSeq: seq,
        updatedSeq: seq,
      };
      repo.nextNumber += 1;
      repo.issues.push(issue);
      return toolText(issueJson(store, repo, issue));
    }
    case 'list_issues': {
      const repo = ownedRepo(store, args);
      if (repo === null) return toolError('Repository not found');
      const state = typeof args.state === 'string' ? args.state.toLowerCase() : 'open';
      let issues = repo.issues;
      if (state === 'open') issues = issues.filter((issue) => issue.state === 'open');
      else if (state === 'closed') issues = issues.filter((issue) => issue.state === 'closed');
      return toolText(issues.map((issue) => issueJson(store, repo, issue)));
    }
    case 'create_pull_request': {
      const repo = ownedRepo(store, args);
      if (repo === null) return toolError('Repository not found');
      const title = typeof args.title === 'string' ? args.title : '';
      const head = typeof args.head === 'string' ? args.head : '';
      const base = typeof args.base === 'string' ? args.base : '';
      if (title === '' || head === '' || base === '') return toolError('title, head and base are required');
      const { pull, error } = createPullRecord(store, repo, { title, body: typeof args.body === 'string' ? args.body : '', head, base });
      if (pull === null) return toolError(error ?? 'cannot create pull request');
      return toolText(pullJson(store, repo, pull));
    }
    case 'merge_pull_request': {
      const repo = ownedRepo(store, args);
      if (repo === null) return toolError('Repository not found');
      const number = Number(args.pull_number ?? 0);
      const pull: Pull | undefined = repo.pulls.find((entry) => entry.number === number);
      if (pull === undefined) return toolError('Pull request not found');
      const result = mergePullRecord(store, repo, pull, 'squash');
      if (result.sha === null) return toolError(result.message);
      return toolText({ sha: result.sha, merged: true, message: result.message, number: pull.number, state: pull.state });
    }
    default:
      return toolError(`Unknown tool: ${name}`);
  }
}

/** Handle POST /mcp. Returns true when the path matched. */
export function handleMcp(ctx: Ctx): boolean {
  if (ctx.path !== '/mcp') return false;
  if (ctx.method !== 'POST') {
    sendJson(ctx, 405, rpcError(null, -32600, 'Method Not Allowed'));
    return true;
  }
  if (!isAuthorized(ctx)) {
    sendJson(ctx, 401, rpcError(null, -32000, 'Requires authentication'));
    return true;
  }
  const parsed = parseJsonBody(ctx);
  if (!parsed.ok) {
    sendJson(ctx, 400, rpcError(null, -32700, 'Parse error'));
    return true;
  }
  const value = parsed.value;
  if (!isRecord(value)) {
    sendJson(ctx, 400, rpcError(null, -32600, 'Invalid Request'));
    return true;
  }
  if (typeof value.method !== 'string') {
    sendJson(ctx, 400, rpcError(null, -32600, 'Invalid Request'));
    return true;
  }
  const id: JsonRpcId = typeof value.id === 'string' || typeof value.id === 'number' ? value.id : null;
  const method = value.method;
  const params = isRecord(value.params) ? value.params : {};
  // A notification (no id) is acknowledged with 202 and no body.
  if (method === 'notifications/initialized' || method.startsWith('notifications/')) {
    sendNoContent(ctx, 202);
    return true;
  }
  if (method === 'server/discover') {
    // Pre-discover server: the suite's client falls back to the legacy handshake.
    sendJson(ctx, 200, rpcError(id, -32601, 'Method not found'));
    return true;
  }
  if (method === 'initialize') {
    const requested = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
    const protocolVersion = LEGACY_REVISIONS.includes(requested) ? requested : '2025-11-25';
    sendJson(
      ctx,
      200,
      rpcResult(id, {
        protocolVersion,
        capabilities: { tools: {} },
        serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      }),
    );
    return true;
  }
  if (method === 'ping') {
    sendJson(ctx, 200, rpcResult(id, {}));
    return true;
  }
  if (method === 'tools/list') {
    sendJson(ctx, 200, rpcResult(id, { tools: TOOLS }));
    return true;
  }
  if (method === 'tools/call') {
    const name = typeof params.name === 'string' ? params.name : '';
    const args = isRecord(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
    const tool = TOOLS.find((entry) => entry.name === name);
    if (tool === undefined) {
      sendJson(ctx, 200, rpcError(id, -32602, `Unknown tool: ${name}`));
      return true;
    }
    const missing = tool.inputSchema.required.filter((key) => args[key] === undefined || args[key] === null || args[key] === '');
    if (missing.length > 0) {
      sendJson(ctx, 200, rpcResult(id, toolError(`Missing required arguments: ${missing.join(', ')}`)));
      return true;
    }
    const result = callTool(ctx.store, name, args);
    sendJson(ctx, 200, rpcResult(id, result));
    return true;
  }
  if (method === 'resources/list') {
    sendJson(ctx, 200, rpcResult(id, { resources: [] }));
    return true;
  }
  if (method === 'prompts/list') {
    sendJson(ctx, 200, rpcResult(id, { prompts: [] }));
    return true;
  }
  sendJson(ctx, 200, rpcError(id, -32601, `Method not found: ${method}`));
  return true;
}
