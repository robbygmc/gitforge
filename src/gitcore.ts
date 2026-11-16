// The git object and packfile layer: SHA-1 object ids, commit/tree/blob/tag
// encoding, packfile read (plain + OFS/REF deltas, checksum verified) and write
// (plain objects, version 2), and the pkt-line framing the smart-HTTP protocol
// is built from. Standard library only (node:crypto, node:zlib).

import { deflateSync, inflateSync } from 'node:zlib';
import { sha1, ZERO_OID } from './util.ts';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';

export interface GitObject {
  type: ObjType;
  data: Buffer;
}

/** The object id of `type` + `data`: SHA-1 over `<type> <size>\0<data>`. */
export function hashObject(type: ObjType, data: Buffer | Uint8Array): string {
  const header = Buffer.from(`${type} ${data.length}\0`, 'ascii');
  return sha1(Buffer.concat([header, Buffer.from(data)]));
}

/** The canonical header bytes of an object, for hashing and storage. */
export function objectHeader(type: ObjType, size: number): Buffer {
  return Buffer.from(`${type} ${size}\0`, 'ascii');
}

// ---------------------------------------------------------------- commits

export interface ParsedCommit {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

/** Parse a commit object's payload. */
export function parseCommit(data: Buffer): ParsedCommit {
  const text = data.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd >= 0 ? text.slice(0, headEnd) : text;
  const message = headEnd >= 0 ? text.slice(headEnd + 2) : '';
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
  }
  return { tree, parents, author, committer, message };
}

/** The identity line git writes: `Name <email> <epoch> +0000`. */
export function identityLine(name: string, email: string, epochSec: number): string {
  return `${name} <${email}> ${epochSec} +0000`;
}

export interface CommitInput {
  tree: string;
  parents: readonly string[];
  author: string;
  committer: string;
  message: string;
}

/** Serialize a commit object. */
export function buildCommit(input: CommitInput): Buffer {
  let out = `tree ${input.tree}\n`;
  for (const parent of input.parents) out += `parent ${parent}\n`;
  out += `author ${input.author}\n`;
  out += `committer ${input.committer}\n`;
  out += `\n${input.message}`;
  return Buffer.from(out, 'utf8');
}

// ---------------------------------------------------------------- trees

export interface TreeEntry {
  /** '100644', '100755', '120000', '40000' (directories are stored without the leading zero). */
  mode: string;
  name: string;
  oid: string;
}

/** Parse a tree object's payload. */
export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space < 0) break;
    const nul = data.indexOf(0x00, space + 1);
    if (nul < 0 || nul + 21 > data.length) break;
    const mode = data.subarray(offset, space).toString('ascii');
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

/** Git's tree sort: entries by name bytes, directories compared as if ending in '/'. */
export function treeCompare(a: TreeEntry, b: TreeEntry): number {
  const aName = a.mode === '40000' ? `${a.name}/` : a.name;
  const bName = b.mode === '40000' ? `${b.name}/` : b.name;
  if (aName < bName) return -1;
  if (aName > bName) return 1;
  return 0;
}

/** Serialize a tree object (entries sorted by git's rule). */
export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort(treeCompare);
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

// ---------------------------------------------------------------- tags

export interface ParsedTag {
  object: string;
  type: string;
  tag: string;
  tagger: string;
  message: string;
}

/** Parse an annotated tag object's payload. */
export function parseTag(data: Buffer): ParsedTag {
  const text = data.toString('utf8');
  const headEnd = text.indexOf('\n\n');
  const head = headEnd >= 0 ? text.slice(0, headEnd) : text;
  const message = headEnd >= 0 ? text.slice(headEnd + 2) : '';
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) tagger = line.slice(7);
  }
  return { object, type, tag, tagger, message };
}

// ---------------------------------------------------------------- packfiles

const PACK_TYPE_CODES: Record<number, ObjType | 'ofs-delta' | 'ref-delta'> = {
  1: 'commit',
  2: 'tree',
  3: 'blob',
  4: 'tag',
  6: 'ofs-delta',
  7: 'ref-delta',
};
const PACK_TYPE_IDS: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };

export class PackError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'PackError';
  }
}

/** Inflate one zlib stream at `offset`; returns its bytes and the offset just past it. */
export function inflateAt(buffer: Uint8Array, offset: number): { data: Buffer; next: number } {
  const view = buffer.subarray(offset);
  let result: Buffer;
  let written = 0;
  try {
    const out = inflateSync(view, { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    result = Buffer.from(out.buffer);
    written = out.engine?.bytesWritten ?? 0;
  } catch (err) {
    throw new PackError(`zlib stream at offset ${offset} did not inflate: ${(err as Error).message}`);
  }
  if (written <= 0) throw new PackError(`zlib stream at offset ${offset} consumed no bytes`);
  return { data: result, next: offset + written };
}

/** Apply a git delta to `base`, verifying the declared sizes. */
export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readVarint = (): number => {
    let value = 0;
    let shift = 0;
    for (;;) {
      if (pos >= delta.length) throw new PackError('delta ends inside a varint');
      const byte = delta[pos++] ?? 0;
      value |= (byte & 0x7f) << shift;
      shift += 7;
      if ((byte & 0x80) === 0) break;
    }
    return value;
  };
  const sourceSize = readVarint();
  const targetSize = readVarint();
  if (sourceSize !== base.length) throw new PackError(`delta source size ${sourceSize} does not match base ${base.length}`);
  const parts: Buffer[] = [];
  let size = 0;
  while (pos < delta.length) {
    const op = delta[pos++] ?? 0;
    if ((op & 0x80) !== 0) {
      let offset = 0;
      let copySize = 0;
      for (let i = 0; i < 4; i += 1) {
        if ((op & (1 << i)) !== 0) {
          if (pos >= delta.length) throw new PackError('delta ends inside a copy offset');
          offset |= (delta[pos++] ?? 0) << (i * 8);
        }
      }
      for (let i = 0; i < 3; i += 1) {
        if ((op & (0x10 << i)) !== 0) {
          if (pos >= delta.length) throw new PackError('delta ends inside a copy size');
          copySize |= (delta[pos++] ?? 0) << (i * 8);
        }
      }
      if (copySize === 0) copySize = 0x10000;
      if (offset + copySize > base.length) throw new PackError('delta copy runs past the base');
      parts.push(base.subarray(offset, offset + copySize));
      size += copySize;
    } else if (op !== 0) {
      if (pos + op > delta.length) throw new PackError('delta ends inside an insert');
      parts.push(delta.subarray(pos, pos + op));
      pos += op;
      size += op;
    } else {
      throw new PackError('delta holds a zero opcode');
    }
  }
  if (size !== targetSize) throw new PackError(`delta target size ${targetSize} does not match ${size}`);
  return Buffer.concat(parts);
}

export interface PackReadResult {
  objects: GitObject[];
  /** oid -> object, for quick lookup of what the pack carried. */
  byOid: Map<string, GitObject>;
}

/**
 * Parse a packfile, resolving deltas. `resolveBase` answers bases a thin pack
 * refers to but does not carry (the repository's objects); null when the base
 * is unknown. Verifies the trailer SHA-1 and every entry's inflated size.
 */
export function readPack(pack: Uint8Array, resolveBase?: (oid: string) => GitObject | null): PackReadResult {
  const buf = Buffer.from(pack);
  if (buf.length < 12 + 20) throw new PackError('packfile is too short for a header and trailer');
  if (buf.subarray(0, 4).toString('ascii') !== 'PACK') throw new PackError('packfile does not start with PACK');
  const version = buf.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new PackError(`packfile version ${version} is not 2 or 3`);
  const count = buf.readUInt32BE(8);
  const trailerStart = buf.length - 20;
  const trailer = buf.subarray(trailerStart).toString('hex');
  if (sha1(buf.subarray(0, trailerStart)) !== trailer) throw new PackError('packfile trailer checksum does not match');
  const objects: GitObject[] = [];
  const byOid = new Map<string, GitObject>();
  const byOffset = new Map<number, GitObject>();
  let offset = 12;
  for (let index = 0; index < count; index += 1) {
    const entryOffset = offset;
    if (offset >= trailerStart) throw new PackError('packfile ends before its declared entries');
    let byte = pack[offset++] ?? 0;
    const typeCode = (byte >> 4) & 0x7;
    let size = byte & 0x0f;
    let shift = 4;
    while ((byte & 0x80) !== 0) {
      if (offset >= trailerStart) throw new PackError('packfile ends inside an entry header');
      byte = pack[offset++] ?? 0;
      size |= (byte & 0x7f) << shift;
      shift += 7;
    }
    const kind = PACK_TYPE_CODES[typeCode];
    if (kind === undefined) throw new PackError(`packfile entry type ${typeCode} is unknown`);
    let base: GitObject | null = null;
    if (kind === 'ofs-delta') {
      // Git's negative-offset encoding.
      if (offset >= trailerStart) throw new PackError('packfile ends inside an ofs-delta offset');
      byte = pack[offset++] ?? 0;
      let baseOffset = byte & 0x7f;
      while ((byte & 0x80) !== 0) {
        if (offset >= trailerStart) throw new PackError('packfile ends inside an ofs-delta offset');
        byte = pack[offset++] ?? 0;
        baseOffset = ((baseOffset + 1) << 7) | (byte & 0x7f);
      }
      const at = entryOffset - baseOffset;
      base = byOffset.get(at) ?? null;
      if (base === null) throw new PackError(`ofs-delta base at offset ${at} is not in the pack`);
    } else if (kind === 'ref-delta') {
      if (offset + 20 > trailerStart) throw new PackError('packfile ends inside a ref-delta base');
      const baseOid = Buffer.from(pack.subarray(offset, offset + 20)).toString('hex');
      offset += 20;
      base = byOid.get(baseOid) ?? (resolveBase?.(baseOid) ?? null);
      if (base === null) throw new PackError(`ref-delta base ${baseOid} is not in the pack or the store`);
    }
    const inflated = inflateAt(pack.subarray(0, trailerStart), offset);
    if (inflated.data.length !== size) {
      throw new PackError(`pack entry at ${entryOffset} declares ${size} bytes but inflated to ${inflated.data.length}`);
    }
    offset = inflated.next;
    let object: GitObject;
    if (kind === 'ofs-delta' || kind === 'ref-delta') {
      if (base === null) throw new PackError('delta entry has no base');
      object = { type: base.type, data: applyDelta(base.data, inflated.data) };
    } else {
      object = { type: kind, data: inflated.data };
    }
    const oid = hashObject(object.type, object.data);
    objects.push(object);
    byOid.set(oid, object);
    byOffset.set(entryOffset, object);
  }
  if (offset !== trailerStart) throw new PackError('packfile has trailing bytes before the trailer');
  return { objects, byOid };
}

/** Write a packfile of plain objects (no deltas), version 2, with its SHA-1 trailer. */
export function writePack(objects: readonly GitObject[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'ascii');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const object of objects) {
    const typeCode = PACK_TYPE_IDS[object.type];
    const size = object.data.length;
    const headerBytes: number[] = [];
    let first = (typeCode << 4) | (size & 0x0f);
    let rest = size >>> 4;
    if (rest !== 0) first |= 0x80;
    headerBytes.push(first);
    while (rest !== 0) {
      let byte = rest & 0x7f;
      rest >>>= 7;
      if (rest !== 0) byte |= 0x80;
      headerBytes.push(byte);
    }
    parts.push(Buffer.from(headerBytes));
    parts.push(deflateSync(object.data));
  }
  const body = Buffer.concat(parts);
  return Buffer.concat([body, Buffer.from(sha1(body), 'hex')]);
}

// ---------------------------------------------------------------- pkt-lines

export type PktKind = 'data' | 'flush' | 'delim' | 'response-end';

export interface PktLine {
  kind: PktKind;
  data: Buffer;
}

/** Encode one pkt-line (a string is UTF-8, a Buffer is raw). */
export function pktLine(data: string | Buffer): Buffer {
  const payload = typeof data === 'string' ? Buffer.from(data, 'utf8') : data;
  const length = payload.length + 4;
  if (length > 65520) throw new Error(`pkt-line of ${length} bytes exceeds 65520`);
  const header = Buffer.from(length.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([header, payload]);
}

export const PKT_FLUSH = Buffer.from('0000', 'ascii');
export const PKT_DELIM = Buffer.from('0001', 'ascii');
export const PKT_RESPONSE_END = Buffer.from('0002', 'ascii');

/**
 * Decode a whole pkt-line stream into lines. Throws on a truncated or
 * over-long line; a trailing partial line is a protocol error.
 */
export function parsePktLines(buffer: Buffer): PktLine[] {
  const lines: PktLine[] = [];
  let offset = 0;
  while (offset < buffer.length) {
    if (offset + 4 > buffer.length) throw new PackError('pkt-line stream ends inside a length prefix');
    const text = buffer.subarray(offset, offset + 4).toString('ascii');
    if (text === '0000') {
      lines.push({ kind: 'flush', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (text === '0001') {
      lines.push({ kind: 'delim', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    if (text === '0002') {
      lines.push({ kind: 'response-end', data: Buffer.alloc(0) });
      offset += 4;
      continue;
    }
    const length = parseInt(text, 16);
    if (!Number.isFinite(length) || length < 4 || length > 65520) throw new PackError(`bad pkt-line length ${JSON.stringify(text)}`);
    if (offset + length > buffer.length) throw new PackError('pkt-line stream ends inside a line');
    lines.push({ kind: 'data', data: Buffer.from(buffer.subarray(offset + 4, offset + length)) });
    offset += length;
  }
  return lines;
}

/** The offset just past the first flush pkt-line in `buffer`, or -1 when none. */
export function firstFlushOffset(buffer: Buffer): number {
  let offset = 0;
  while (offset + 4 <= buffer.length) {
    const text = buffer.subarray(offset, offset + 4).toString('ascii');
    if (text === '0000') return offset + 4;
    if (text === '0001' || text === '0002') {
      offset += 4;
      continue;
    }
    const length = parseInt(text, 16);
    if (!Number.isFinite(length) || length < 4 || length > 65520) return -1;
    offset += length;
  }
  return -1;
}

/** Wrap pack bytes in sideband band-1 packets (≤65515 payload bytes each). */
export function sidebandPack(pack: Buffer): Buffer {
  const parts: Buffer[] = [];
  const CHUNK = 65515;
  for (let offset = 0; offset < pack.length; offset += CHUNK) {
    const chunk = pack.subarray(offset, Math.min(pack.length, offset + CHUNK));
    const payload = Buffer.concat([Buffer.from([1]), chunk]);
    parts.push(pktLine(payload));
  }
  return Buffer.concat(parts);
}

/** The zero object id, for unborn refs. */
export { ZERO_OID };
