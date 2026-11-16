// HTTP plumbing shared by every surface: the request context, body
// reading, JSON answers, the bearer-token check and the request-id header
// the protocol checks require on every reply.

import type { IncomingMessage, ServerResponse } from 'node:http';
import type { Store } from './store.ts';

export interface Ctx {
  store: Store;
  /** The bearer token the env set (MAKEFUN_SHOP_TOKEN); '' accepts any non-empty token. */
  token: string;
  req: IncomingMessage;
  res: ServerResponse;
  method: string;
  /** The decoded pathname. */
  path: string;
  query: URLSearchParams;
  /** The raw request body, already read. */
  body: Buffer;
  requestId: string;
  /** The request's host, for absolute pagination URLs. */
  host: string;
}

const JSON_TYPE = 'application/json; charset=utf-8';
/** The largest request body accepted (packs are far below this). */
export const MAX_BODY_BYTES = 256 * 1024 * 1024;

let requestCounter = 0;

/** The next request id: deterministic, non-empty, unique per request. */
export function nextRequestId(): string {
  requestCounter += 1;
  return `gitforge-${requestCounter}`;
}

/** Read a request body whole, or null when it exceeds the cap. */
export function readBody(req: IncomingMessage): Promise<Buffer | null> {
  // A GET or HEAD with no declared body has nothing to read: resolve at
  // once instead of waiting a tick for the stream's 'end' event.
  if ((req.method === 'GET' || req.method === 'HEAD') && req.headers['transfer-encoding'] === undefined) {
    const length = Number(req.headers['content-length'] ?? 0);
    if (length === 0) return Promise.resolve(Buffer.alloc(0));
  }
  return new Promise((resolve) => {
    const chunks: Buffer[] = [];
    let size = 0;
    let tooBig = false;
    req.on('data', (chunk: Buffer) => {
      if (tooBig) return;
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        tooBig = true;
        resolve(null);
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (tooBig) return;
      resolve(Buffer.concat(chunks));
    });
    req.on('error', () => resolve(null));
  });
}

/**
 * Split a request URL into its pathname and query in one pass, without
 * constructing a URL object (two of those per request was the old cost).
 * The split follows the URL rules for the inputs this server sees: the
 * pathname ends at the first '?' or '#', the query is the '?'-part up to
 * '#', and percent-escapes stay as written (the handlers decode the
 * segments they need themselves). Anything unusual — a relative target,
 * a dot segment (which URL resolution would collapse), or a parse failure
 * — falls back to `new URL`, which is always correct.
 */
export function parseRequestUrl(req: IncomingMessage): { path: string; query: URLSearchParams } {
  const raw = req.url ?? '/';
  let pathEnd = raw.length;
  const q = raw.indexOf('?');
  const h = raw.indexOf('#');
  if (q >= 0 && q < pathEnd) pathEnd = q;
  if (h >= 0 && h < pathEnd) pathEnd = h;
  const rawPath = raw.slice(0, pathEnd);
  const queryString = q >= 0 && (h < 0 || q < h) ? raw.slice(q + 1, h < 0 ? raw.length : h) : '';
  let path: string;
  if (
    rawPath === '' ||
    !rawPath.startsWith('/') ||
    rawPath.startsWith('//') ||
    rawPath.includes('/./') ||
    rawPath.includes('/../') ||
    rawPath.endsWith('/.') ||
    rawPath.endsWith('/..')
  ) {
    try {
      path = new URL(raw, 'http://entry').pathname;
    } catch {
      path = '/';
    }
  } else {
    path = rawPath;
  }
  return { path, query: queryString === '' ? new URLSearchParams() : new URLSearchParams(queryString) };
}

/** The pathname of a request URL, or '/' when it does not parse. */
export function pathOf(req: IncomingMessage): string {
  return parseRequestUrl(req).path;
}

/** The query string of a request URL. */
export function queryOf(req: IncomingMessage): URLSearchParams {
  return parseRequestUrl(req).query;
}

/** Whether the request carries the expected bearer token. */
export function isAuthorized(ctx: Ctx): boolean {
  const header = ctx.req.headers.authorization ?? '';
  if (!header.startsWith('Bearer ')) return false;
  const presented = header.slice('Bearer '.length).trim();
  if (presented === '') return false;
  if (ctx.token !== '') return presented === ctx.token;
  return true;
}

/** Answer with a JSON body, always carrying the request-id header. */
export function sendJson(ctx: Ctx, status: number, body: unknown, headers: Readonly<Record<string, string>> = {}): void {
  const text = `${JSON.stringify(body)}\n`;
  ctx.res.writeHead(status, {
    'content-type': JSON_TYPE,
    'content-length': Buffer.byteLength(text),
    'x-github-request-id': ctx.requestId,
    ...headers,
  });
  ctx.res.end(text);
}

/** Answer with a raw buffer and an explicit content type. */
export function sendBytes(ctx: Ctx, status: number, body: Buffer, contentType: string, headers: Readonly<Record<string, string>> = {}): void {
  ctx.res.writeHead(status, {
    'content-type': contentType,
    'content-length': body.length,
    'x-github-request-id': ctx.requestId,
    ...headers,
  });
  ctx.res.end(body);
}

/** Answer with a JSON error body: {"message": ...}. */
export function sendError(ctx: Ctx, status: number, message: string): void {
  sendJson(ctx, status, { message });
}

/** A 204 with no body (DELETE a repository), or a 202 acknowledgement. */
export function sendNoContent(ctx: Ctx, status = 204): void {
  ctx.res.writeHead(status, { 'x-github-request-id': ctx.requestId });
  ctx.res.end();
}

/** Parse the request body as JSON, or null when it is empty or malformed. */
export function parseJsonBody(ctx: Ctx): { ok: true; value: unknown } | { ok: false; empty: boolean } {
  if (ctx.body.length === 0) return { ok: false, empty: true };
  try {
    return { ok: true, value: JSON.parse(ctx.body.toString('utf8')) as unknown };
  } catch {
    return { ok: false, empty: false };
  }
}

/** Whether a value is a plain JSON object. */
export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** The bearer token the env set, read once: it cannot change during the process. */
let cachedToken: string | null = null;
function shopToken(): string {
  if (cachedToken === null) cachedToken = process.env.MAKEFUN_SHOP_TOKEN ?? '';
  return cachedToken;
}

/** Build the request context: read the body whole, then fill in the fields the handlers need. Returns null when the body exceeds the cap. */
export async function createCtx(req: IncomingMessage, res: ServerResponse, store: Store): Promise<Ctx | null> {
  const body = await readBody(req);
  if (body === null) return null;
  const { path, query } = parseRequestUrl(req);
  return {
    store,
    token: shopToken(),
    req,
    res,
    method: req.method ?? 'GET',
    path,
    query,
    body,
    requestId: nextRequestId(),
    host: req.headers.host ?? '127.0.0.1',
  };
}
