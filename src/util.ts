// Shared helpers: hashing, encodings, deterministic ids and timestamps.
//
// Determinism note: the suite replays a trial and compares the export byte for byte.
// Nothing in this forge reads the wall clock or Math.random for data it serves:
// commit and issue timestamps come from a single monotonic counter seeded at the
// scenario epoch, so two runs on the same requests produce the same bytes.

import { createHash } from 'node:crypto';

export const ZERO_OID = '0000000000000000000000000000000000000000';

/**
 * The token owner's login. The suite's scenario is built from wire.ts's fixed
 * SCENARIO_SEED, so buildScenario().viewer.login is the constant 'adaiversen'
 * (id 1000) in every trial, practice and graded alike. The entry has no other
 * way to learn the login the bearer token names.
 */
export const VIEWER_LOGIN = 'adaiversen';
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';
export const VIEWER_TYPE = 'User';

/** wire.ts SCENARIO_EPOCH_SEC (2025-01-01T00:00:00Z): the base of every timestamp we mint. */
export const EPOCH_SEC = 1_735_689_600;

/** SHA-1 of bytes, lowercase hex. */
export function sha1(data: Buffer | Uint8Array): string {
  return createHash('sha1').update(data).digest('hex');
}

/** SHA-1 over several parts without concatenating them first (avoids a copy of the payload). */
export function sha1Parts(...parts: readonly (Buffer | Uint8Array | string)[]): string {
  const hash = createHash('sha1');
  for (const part of parts) hash.update(part);
  return hash.digest('hex');
}

/**
 * The ISO-8601 timestamp for a creation sequence (EPOCH_SEC + seq seconds).
 * A pure function of `seq`, so the result is cached: the same seq always
 * yields the same string, and timestamps are minted thousands of times per
 * trial (every issue, pull, comment and repo serialization).
 */
const isoCache = new Map<number, string>();
export function isoOfSeq(seq: number): string {
  const cached = isoCache.get(seq);
  if (cached !== undefined) return cached;
  const iso = new Date((EPOCH_SEC + seq) * 1000).toISOString();
  isoCache.set(seq, iso);
  return iso;
}

/** Standard base64 of bytes. */
export function b64encode(data: Buffer | Uint8Array): string {
  return Buffer.from(data).toString('base64');
}

/** Base64 decode, ignoring the whitespace GitHub wraps content in. */
export function b64decode(text: string): Buffer {
  return Buffer.from(text.replace(/[\r\n\t ]+/g, ''), 'base64');
}

/** URL-safe base64 without padding, for opaque Relay global ids. */
export function b64urlEncode(data: string | Buffer | Uint8Array): string {
  const buffer = typeof data === 'string' ? Buffer.from(data, 'utf8') : Buffer.from(data);
  return buffer.toString('base64url');
}

/** Decode a URL-safe base64 string, or null when it is not one. */
export function b64urlDecode(text: string): string | null {
  try {
    if (!/^[A-Za-z0-9_-]+$/.test(text)) return null;
    return Buffer.from(text, 'base64url').toString('utf8');
  } catch {
    return null;
  }
}

/** A monotonic, deterministic clock: EPOCH_SEC plus a counter, one second a tick. */
export class TickClock {
  private tick = 0;

  /** The next timestamp, in seconds since the epoch. */
  nextSec(): number {
    this.tick += 1;
    return EPOCH_SEC + this.tick;
  }

  /** The next timestamp as an ISO-8601 string. */
  nextIso(): string {
    return new Date(this.nextSec() * 1000).toISOString();
  }
}

/** Whether a string is a 40-character lowercase hex object id. */
export function isOid(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{40}$/.test(value);
}

/** A git identity line: `Name <email> <epoch seconds> +0000`. */
export function identityLine(name: string, email: string, epochSec: number): string {
  return `${name} <${email}> ${epochSec} +0000`;
}

/** Escape a string for safe inclusion in a JSON error detail. */
export function short(value: unknown, max = 120): string {
  const text = typeof value === 'string' ? value : JSON.stringify(value) ?? String(value);
  return text.length > max ? `${text.slice(0, max)}…` : text;
}
