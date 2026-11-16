// The forge server: one process, one store, four surfaces.
// main.ts (the kit lifecycle) imports createApp() from here and listens on an
// ephemeral port. Routing order: health probe, git smart HTTP, REST v3,
// GraphQL, MCP, then a JSON 404 for everything else. A handler that throws
// never takes the server down: the request answers JSON 500 instead.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { Store } from './src/store.ts';
import { createCtx, sendError, sendJson } from './src/http.ts';
import { handleGit } from './src/git-http.ts';
import { handleRest } from './src/rest.ts';
import { handleGraphQL } from './src/graphql.ts';
import { handleMcp } from './src/mcp.ts';

/** Build the server. It binds nothing until the caller listens. */
export function createApp(): Server {
  const store = new Store();
  return createServer((req: IncomingMessage, res: ServerResponse) => {
    void handle(req, res, store).catch(() => {
      // The handler already answered (or the socket is gone); nothing more to do.
    });
  });
}

async function handle(req: IncomingMessage, res: ServerResponse, store: Store): Promise<void> {
  const ctx = await createCtx(req, res, store);
  if (ctx === null) {
    const body = JSON.stringify({ message: 'Request body too large' });
    res.writeHead(413, { 'content-type': 'application/json; charset=utf-8', 'content-length': Buffer.byteLength(body) });
    res.end(body);
    return;
  }
  try {
    // The env's boot check calls this twice: 200 {"ok":true}. No auth here.
    if (ctx.method === 'GET' && ctx.path === '/healthz') {
      sendJson(ctx, 200, { ok: true });
      return;
    }
    if (handleGit(ctx)) return;
    if (handleRest(ctx)) return;
    if (handleGraphQL(ctx)) return;
    if (handleMcp(ctx)) return;
    // Everything else, every method: GitHub's 404 body.
    sendError(ctx, 404, 'Not Found');
  } catch (error) {
    // Never let one request crash the server or hang the suite.
    try {
      const body = JSON.stringify({ message: 'Internal Server Error' });
      res.writeHead(500, { 'content-type': 'application/json; charset=utf-8', 'content-length': Buffer.byteLength(body) });
      res.end(body);
    } catch {
      // The socket is gone; nothing more to do.
    }
    // Keep the failure visible in the server's own stderr for debugging.
    console.error('gitforge: request failed', error instanceof Error ? error.message : error);
  }
}
