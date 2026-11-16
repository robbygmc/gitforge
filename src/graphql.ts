// GraphQL over POST /api/graphql: a small hand-written parser and executor
// for the documents gh and github-mcp-server send (see checks-graphql.ts).
// Supports operations, variables, fragments, inline fragments, aliases,
// enums, input objects, lists, __type/__schema introspection and batching
// (a JSON array of operation documents in one request).

import { b64urlDecode, b64urlEncode } from './util.ts';
import { isAuthorized, parseJsonBody, sendError, sendJson, type Ctx } from './http.ts';
import type { Issue, Pull, Repo, Store } from './store.ts';
import { createPullRecord, mergePullRecord, pullFiles, pullMergeable } from './rest.ts';

// ---------------------------------------------------------------- global ids

export function repoGlobalId(repo: Repo): string {
  return `R_${b64urlEncode(Buffer.from(`repo:${repo.owner}/${repo.name}`, 'utf8'))}`;
}

export function issueGlobalId(repo: Repo, issue: Issue): string {
  return `I_${b64urlEncode(Buffer.from(`issue:${repo.owner}/${repo.name}:${issue.number}`, 'utf8'))}`;
}

export function pullGlobalId(repo: Repo, pull: Pull): string {
  return `P_${b64urlEncode(Buffer.from(`pull:${repo.owner}/${repo.name}:${pull.number}`, 'utf8'))}`;
}

export function commentGlobalId(repo: Repo, commentId: number): string {
  return `C_${b64urlEncode(Buffer.from(`comment:${repo.owner}/${repo.name}:${commentId}`, 'utf8'))}`;
}

interface DecodedId {
  kind: 'repo' | 'issue' | 'pull' | 'comment';
  owner: string;
  name: string;
  number: number;
}

export function decodeGlobalId(id: string): DecodedId | null {
  const match = /^([RIPC])_(.+)$/.exec(id);
  if (match === null) return null;
  const kind = match[1] === 'R' ? 'repo' : match[1] === 'I' ? 'issue' : match[1] === 'P' ? 'pull' : 'comment';
  const text = b64urlDecode(match[2] as string);
  if (text === null) return null;
  const parts = /^(\w+):([^/]+)\/([^:]+)(?::(\d+))?$/.exec(text);
  if (parts === null) return null;
  return { kind, owner: parts[2] as string, name: parts[3] as string, number: parts[4] === undefined ? 0 : Number(parts[4]) };
}

// ---------------------------------------------------------------- lexer

type Token =
  | { kind: 'punct'; value: string }
  | { kind: 'name'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'eof' };

function lex(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const push = (token: Token): void => {
    tokens.push(token);
  };
  while (i < source.length) {
    const char = source[i] as string;
    if (char === ' ' || char === '\t' || char === '\n' || char === '\r' || char === ',' || char === '﻿') {
      i += 1;
      continue;
    }
    if (char === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (char === '.' && source.slice(i, i + 3) === '...') {
      push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('{}()[]:!=$@&|'.includes(char)) {
      push({ kind: 'punct', value: char });
      i += 1;
      continue;
    }
    if (char === '"') {
      if (source.slice(i, i + 3) === '"""') {
        const end = source.indexOf('"""', i + 3);
        if (end < 0) throw new Error('Unterminated block string');
        push({ kind: 'string', value: source.slice(i + 3, end) });
        i = end + 3;
        continue;
      }
      let j = i + 1;
      let value = '';
      while (j < source.length && source[j] !== '"') {
        if (source[j] === '\\') {
          const escaped = source[j + 1] as string;
          const map: Record<string, string> = { n: '\n', t: '\t', r: '\r', '"': '"', '\\': '\\', '/': '/', b: '\b', f: '\f' };
          if (escaped === 'u') {
            value += String.fromCharCode(Number.parseInt(source.slice(j + 2, j + 6), 16));
            j += 6;
          } else {
            value += map[escaped] ?? escaped;
            j += 2;
          }
        } else {
          value += source[j];
          j += 1;
        }
      }
      if (j >= source.length) throw new Error('Unterminated string');
      push({ kind: 'string', value });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(char)) {
      const match = /^-?\d+(\.\d+)?([eE][+-]?\d+)?/.exec(source.slice(i));
      if (match === null) throw new Error(`Bad number at ${i}`);
      const text = match[0];
      push(text.includes('.') || /[eE]/.test(text) ? { kind: 'float', value: Number(text) } : { kind: 'int', value: Number(text) });
      i += text.length;
      continue;
    }
    if (/[_A-Za-z]/.test(char)) {
      const match = /^[_A-Za-z][_0-9A-Za-z]*/.exec(source.slice(i));
      if (match === null) throw new Error(`Bad name at ${i}`);
      push({ kind: 'name', value: match[0] });
      i += match[0].length;
      continue;
    }
    throw new Error(`Unexpected character ${JSON.stringify(char)} at ${i}`);
  }
  push({ kind: 'eof' });
  return tokens;
}

// ---------------------------------------------------------------- parser

type ValueNode =
  | { kind: 'variable'; name: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'boolean'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: ValueNode[] }
  | { kind: 'object'; fields: { name: string; value: ValueNode }[] };

type Selection =
  | { kind: 'field'; alias: string | null; name: string; args: { name: string; value: ValueNode }[]; selections: Selection[] }
  | { kind: 'fragmentSpread'; name: string }
  | { kind: 'inlineFragment'; typeCondition: string | null; selections: Selection[] };

interface Operation {
  type: 'query' | 'mutation' | 'subscription';
  name: string | null;
  variables: { name: string; defaultValue: ValueNode | null }[];
  selections: Selection[];
}

interface Fragment {
  name: string;
  typeCondition: string;
  selections: Selection[];
}

interface Document {
  operations: Operation[];
  fragments: Map<string, Fragment>;
}

class Parser {
  private pos = 0;
  private readonly tokens: Token[];

  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.pos] as Token;
  }

  private next(): Token {
    const token = this.tokens[this.pos] as Token;
    this.pos += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) throw new Error(`Expected ${value}, got ${token.kind} ${'value' in token ? token.value : ''}`);
  }

  private isPunct(value: string): boolean {
    const token = this.peek();
    return token.kind === 'punct' && token.value === value;
  }

  private expectName(): string {
    const token = this.next();
    if (token.kind !== 'name') throw new Error(`Expected name, got ${token.kind}`);
    return token.value;
  }

  parseDocument(): Document {
    const operations: Operation[] = [];
    const fragments = new Map<string, Fragment>();
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'punct' && token.value === '{') {
        operations.push({ type: 'query', name: null, variables: [], selections: this.parseSelectionSet() });
      } else if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation' || token.value === 'subscription')) {
        const typeToken = this.next();
        if (typeToken.kind !== 'name') throw new Error('Expected an operation type');
        const type = typeToken.value as 'query' | 'mutation' | 'subscription';
        let name: string | null = null;
        if (this.peek().kind === 'name') name = this.expectName();
        let variables: { name: string; defaultValue: ValueNode | null }[] = [];
        if (this.isPunct('(')) variables = this.parseVariableDefinitions();
        this.skipDirectives();
        operations.push({ type, name, variables, selections: this.parseSelectionSet() });
      } else if (token.kind === 'name' && token.value === 'fragment') {
        this.next();
        const name = this.expectName();
        const on = this.expectName();
        if (on !== 'on') throw new Error('Expected "on" in fragment');
        const typeCondition = this.expectName();
        this.skipDirectives();
        fragments.set(name, { name, typeCondition, selections: this.parseSelectionSet() });
      } else {
        throw new Error(`Unexpected token ${token.kind}`);
      }
    }
    return { operations, fragments };
  }

  private skipDirectives(): void {
    while (this.isPunct('@')) {
      this.next();
      this.expectName();
      if (this.isPunct('(')) this.parseArguments();
    }
  }

  private parseVariableDefinitions(): { name: string; defaultValue: ValueNode | null }[] {
    this.expectPunct('(');
    const definitions: { name: string; defaultValue: ValueNode | null }[] = [];
    while (!this.isPunct(')')) {
      this.expectPunct('$');
      const name = this.expectName();
      this.expectPunct(':');
      this.parseType();
      let defaultValue: ValueNode | null = null;
      if (this.isPunct('=')) {
        this.next();
        defaultValue = this.parseValue(true);
      }
      this.skipDirectives();
      definitions.push({ name, defaultValue });
    }
    this.expectPunct(')');
    return definitions;
  }

  private parseType(): string {
    if (this.isPunct('[')) {
      this.next();
      const inner = this.parseType();
      this.expectPunct(']');
      let text = `[${inner}]`;
      if (this.isPunct('!')) {
        this.next();
        text += '!';
      }
      return text;
    }
    const name = this.expectName();
    let text = name;
    if (this.isPunct('!')) {
      this.next();
      text += '!';
    }
    return text;
  }

  private parseSelectionSet(): Selection[] {
    this.expectPunct('{');
    const selections: Selection[] = [];
    while (!this.isPunct('}')) {
      if (this.peek().kind === 'eof') throw new Error('Unterminated selection set');
      selections.push(this.parseSelection());
    }
    this.expectPunct('}');
    return selections;
  }

  private parseSelection(): Selection {
    if (this.isPunct('...')) {
      this.next();
      const token = this.peek();
      if (token.kind === 'name' && token.value !== 'on') {
        const name = this.expectName();
        this.skipDirectives();
        return { kind: 'fragmentSpread', name };
      }
      let typeCondition: string | null = null;
      if (token.kind === 'name' && token.value === 'on') {
        this.next();
        typeCondition = this.expectName();
      }
      this.skipDirectives();
      return { kind: 'inlineFragment', typeCondition, selections: this.parseSelectionSet() };
    }
    const first = this.expectName();
    let alias: string | null = null;
    let name = first;
    if (this.isPunct(':')) {
      this.next();
      alias = first;
      name = this.expectName();
    }
    const args = this.isPunct('(') ? this.parseArguments() : [];
    this.skipDirectives();
    const selections = this.isPunct('{') ? this.parseSelectionSet() : [];
    return { kind: 'field', alias, name, args, selections };
  }

  private parseArguments(): { name: string; value: ValueNode }[] {
    this.expectPunct('(');
    const args: { name: string; value: ValueNode }[] = [];
    while (!this.isPunct(')')) {
      const name = this.expectName();
      this.expectPunct(':');
      args.push({ name, value: this.parseValue(false) });
    }
    this.expectPunct(')');
    return args;
  }

  private parseValue(isConst: boolean): ValueNode {
    const token = this.next();
    if (token.kind === 'punct' && token.value === '$') {
      if (isConst) throw new Error('Variable in constant value');
      return { kind: 'variable', name: this.expectName() };
    }
    if (token.kind === 'punct' && token.value === '[') {
      const values: ValueNode[] = [];
      while (!this.isPunct(']')) values.push(this.parseValue(isConst));
      this.expectPunct(']');
      return { kind: 'list', values };
    }
    if (token.kind === 'punct' && token.value === '{') {
      const fields: { name: string; value: ValueNode }[] = [];
      while (!this.isPunct('}')) {
        const name = this.expectName();
        this.expectPunct(':');
        fields.push({ name, value: this.parseValue(isConst) });
      }
      this.expectPunct('}');
      return { kind: 'object', fields };
    }
    if (token.kind === 'int') return { kind: 'int', value: token.value };
    if (token.kind === 'float') return { kind: 'float', value: token.value };
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'boolean', value: true };
      if (token.value === 'false') return { kind: 'boolean', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    throw new Error(`Unexpected value token ${token.kind}`);
  }
}

// ---------------------------------------------------------------- values

type Variables = Record<string, unknown>;

function resolveValue(node: ValueNode, variables: Variables): unknown {
  switch (node.kind) {
    case 'variable':
      return variables[node.name] ?? null;
    case 'int':
    case 'float':
    case 'string':
    case 'boolean':
      return node.value;
    case 'null':
      return null;
    case 'enum':
      return node.value;
    case 'list':
      return node.values.map((value) => resolveValue(value, variables));
    case 'object': {
      const out: Record<string, unknown> = {};
      for (const field of node.fields) out[field.name] = resolveValue(field.value, variables);
      return out;
    }
  }
}

function argsOf(selection: Extract<Selection, { kind: 'field' }>, variables: Variables): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const arg of selection.args) out[arg.name] = resolveValue(arg.value, variables);
  return out;
}

// ---------------------------------------------------------------- schema

interface SchemaField {
  name: string;
  type?: string;
}

interface SchemaType {
  kind: 'OBJECT' | 'ENUM' | 'INPUT_OBJECT' | 'SCALAR';
  fields?: SchemaField[];
  enumValues?: string[];
  inputFields?: SchemaField[];
}

const SCHEMA: Record<string, SchemaType> = {
  Query: {
    kind: 'OBJECT',
    fields: [
      { name: 'viewer' },
      { name: 'repository' },
      { name: 'node' },
      { name: '__type' },
      { name: '__schema' },
    ],
  },
  Mutation: {
    kind: 'OBJECT',
    fields: [
      { name: 'createIssue' },
      { name: 'closeIssue' },
      { name: 'createPullRequest' },
      { name: 'mergePullRequest' },
      { name: 'addComment' },
    ],
  },
  Repository: {
    kind: 'OBJECT',
    fields: [
      { name: 'id' },
      { name: 'databaseId' },
      { name: 'name' },
      { name: 'nameWithOwner' },
      { name: 'owner' },
      { name: 'defaultBranchRef' },
      { name: 'viewerPermission' },
      { name: 'hasIssuesEnabled' },
      { name: 'issue' },
      { name: 'issues' },
      { name: 'pullRequests' },
      { name: 'pullRequest' },
    ],
  },
  User: { kind: 'OBJECT', fields: [{ name: 'login' }, { name: 'id' }, { name: 'name' }, { name: 'email' }] },
  Ref: { kind: 'OBJECT', fields: [{ name: 'name' }] },
  Issue: {
    kind: 'OBJECT',
    fields: [
      { name: 'id' },
      { name: 'number' },
      { name: 'title' },
      { name: 'state' },
      { name: 'body' },
      { name: 'createdAt' },
      { name: 'labels' },
      { name: 'comments' },
    ],
  },
  IssueConnection: { kind: 'OBJECT', fields: [{ name: 'nodes' }, { name: 'edges' }, { name: 'pageInfo' }, { name: 'totalCount' }] },
  IssueEdge: { kind: 'OBJECT', fields: [{ name: 'node' }, { name: 'cursor' }] },
  PageInfo: { kind: 'OBJECT', fields: [{ name: 'hasNextPage' }, { name: 'hasPreviousPage' }, { name: 'startCursor' }, { name: 'endCursor' }] },
  PullRequest: {
    kind: 'OBJECT',
    fields: [
      { name: 'id' },
      { name: 'number' },
      { name: 'title' },
      { name: 'body' },
      { name: 'state' },
      { name: 'merged' },
      { name: 'headRefName' },
      { name: 'baseRefName' },
      { name: 'mergeable' },
      { name: 'files' },
      { name: 'createdAt' },
    ],
  },
  PullRequestConnection: { kind: 'OBJECT', fields: [{ name: 'nodes' }, { name: 'edges' }, { name: 'pageInfo' }, { name: 'totalCount' }] },
  PullRequestEdge: { kind: 'OBJECT', fields: [{ name: 'node' }, { name: 'cursor' }] },
  PullRequestChangedFile: { kind: 'OBJECT', fields: [{ name: 'path' }, { name: 'additions' }, { name: 'deletions' }, { name: 'changes' }] },
  PullRequestChangedFileConnection: { kind: 'OBJECT', fields: [{ name: 'nodes' }, { name: 'edges' }, { name: 'pageInfo' }, { name: 'totalCount' }] },
  Comment: { kind: 'OBJECT', fields: [{ name: 'id' }, { name: 'body' }, { name: 'createdAt' }] },
  CommentEdge: { kind: 'OBJECT', fields: [{ name: 'node' }, { name: 'cursor' }] },
  Label: { kind: 'OBJECT', fields: [{ name: 'name' }, { name: 'color' }, { name: 'description' }] },
  LabelConnection: { kind: 'OBJECT', fields: [{ name: 'nodes' }, { name: 'edges' }, { name: 'pageInfo' }, { name: 'totalCount' }] },
  IssueState: { kind: 'ENUM', enumValues: ['OPEN', 'CLOSED'] },
  PullRequestState: { kind: 'ENUM', enumValues: ['OPEN', 'CLOSED', 'MERGED'] },
  MergeableState: { kind: 'ENUM', enumValues: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
  RepositoryPermission: { kind: 'ENUM', enumValues: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'] },
  IssueOrderField: { kind: 'ENUM', enumValues: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'] },
  OrderDirection: { kind: 'ENUM', enumValues: ['ASC', 'DESC'] },
  SearchType: { kind: 'ENUM', enumValues: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT', 'MILESTONE'] },
  PullRequestMergeMethod: { kind: 'ENUM', enumValues: ['MERGE', 'SQUASH', 'REBASE'] },
  CreateIssueInput: { kind: 'INPUT_OBJECT', inputFields: [{ name: 'repositoryId' }, { name: 'title' }, { name: 'body' }] },
  CloseIssueInput: { kind: 'INPUT_OBJECT', inputFields: [{ name: 'issueId' }] },
  CreatePullRequestInput: { kind: 'INPUT_OBJECT', inputFields: [{ name: 'repositoryId' }, { name: 'baseRefName' }, { name: 'headRefName' }, { name: 'title' }, { name: 'body' }] },
  MergePullRequestInput: { kind: 'INPUT_OBJECT', inputFields: [{ name: 'pullRequestId' }, { name: 'mergeMethod' }] },
  AddCommentInput: { kind: 'INPUT_OBJECT', inputFields: [{ name: 'subjectId' }, { name: 'body' }] },
  IssueOrder: { kind: 'INPUT_OBJECT', inputFields: [{ name: 'field' }, { name: 'direction' }] },
};

// ---------------------------------------------------------------- execution

interface GqlContext {
  store: Store;
}

interface SourceObject {
  __typename: string;
  [key: string]: unknown;
}

interface FieldResult {
  value: unknown;
  /** GraphQL type of the value, for sub-selection execution. */
  typeName: string | null;
}

function cursorFor(index: number): string {
  return b64urlEncode(Buffer.from(`cursor:${index}`, 'utf8'));
}

function cursorIndex(cursor: unknown): number {
  if (typeof cursor !== 'string') return -1;
  const text = b64urlDecode(cursor);
  if (text === null) return -1;
  const match = /^cursor:(\d+)$/.exec(text);
  return match === null ? -1 : Number(match[1]);
}

function connectionOf<T>(items: readonly T[], args: Record<string, unknown>, itemType: string): SourceObject {
  const first = typeof args.first === 'number' ? args.first : items.length;
  const after = cursorIndex(args.after);
  const start = after + 1;
  const page = items.slice(start, start + first);
  const endCursor = page.length === 0 ? null : cursorFor(start + page.length - 1);
  return {
    __typename: `${itemType}Connection`,
    nodes: page,
    edges: page.map((node, i) => ({ __typename: `${itemType}Edge`, node, cursor: cursorFor(start + i) })),
    pageInfo: {
      __typename: 'PageInfo',
      hasNextPage: start + page.length < items.length,
      hasPreviousPage: start > 0,
      startCursor: page.length === 0 ? null : cursorFor(start),
      endCursor,
    },
    totalCount: items.length,
  };
}

function repoSource(repo: Repo): SourceObject {
  return { __typename: 'Repository', repo };
}

function issueSource(repo: Repo, issue: Issue): SourceObject {
  return { __typename: 'Issue', repo, issue };
}

function pullSource(repo: Repo, pull: Pull): SourceObject {
  return { __typename: 'PullRequest', repo, pull };
}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullState(pull: Pull): string {
  if (pull.merged) return 'MERGED';
  return pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function findByGlobalId(store: Store, id: string): { kind: string; repo: Repo; issue?: Issue; pull?: Pull; commentId?: number } | null {
  const decoded = decodeGlobalId(id);
  if (decoded === null) return null;
  const repo = store.getRepo(decoded.owner, decoded.name);
  if (repo === undefined) return null;
  if (decoded.kind === 'repo') return { kind: 'repo', repo };
  if (decoded.kind === 'issue') {
    const issue = repo.issues.find((entry) => entry.number === decoded.number);
    return issue === undefined ? null : { kind: 'issue', repo, issue };
  }
  if (decoded.kind === 'pull') {
    const pull = repo.pulls.find((entry) => entry.number === decoded.number);
    return pull === undefined ? null : { kind: 'pull', repo, pull };
  }
  return { kind: 'comment', repo, commentId: decoded.number };
}

function resolveField(ctx: GqlContext, typeName: string, field: string, source: SourceObject, args: Record<string, unknown>): FieldResult | null {
  const store = ctx.store;
  switch (typeName) {
    case 'Query': {
      if (field === 'viewer') return { value: { __typename: 'User', login: 'adaiversen', id: 1000, name: 'ada iversen', email: 'adaiversen@example.invalid' }, typeName: 'User' };
      if (field === 'repository') {
        const owner = String(args.owner ?? '');
        const name = String(args.name ?? '');
        if (owner.toLowerCase() !== 'adaiversen') return { value: null, typeName: 'Repository' };
        const repo = store.getRepo(owner, name);
        return { value: repo === undefined ? null : repoSource(repo), typeName: 'Repository' };
      }
      if (field === 'node') {
        const found = findByGlobalId(store, String(args.id ?? ''));
        if (found === null) return { value: null, typeName: null };
        if (found.kind === 'repo') return { value: repoSource(found.repo), typeName: 'Repository' };
        if (found.kind === 'issue' && found.issue !== undefined) return { value: issueSource(found.repo, found.issue), typeName: 'Issue' };
        if (found.kind === 'pull' && found.pull !== undefined) return { value: pullSource(found.repo, found.pull), typeName: 'PullRequest' };
        return { value: null, typeName: null };
      }
      if (field === '__type') {
        const type = SCHEMA[String(args.name ?? '')];
        if (type === undefined) return { value: null, typeName: '__Type' };
        return { value: introspectionType(String(args.name ?? ''), type), typeName: '__Type' };
      }
      if (field === '__schema') {
        return {
          value: {
            __typename: '__Schema',
            queryType: { __typename: '__Type', name: 'Query' },
            mutationType: { __typename: '__Type', name: 'Mutation' },
            types: Object.keys(SCHEMA).map((name) => introspectionType(name, SCHEMA[name] as SchemaType)),
          },
          typeName: '__Schema',
        };
      }
      return null;
    }
    case 'Mutation': {
      if (field === 'createIssue') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const found = findByGlobalId(store, String(input.repositoryId ?? ''));
        if (found === null || found.kind !== 'repo') return { value: { __typename: 'CreateIssuePayload', issue: null }, typeName: 'CreateIssuePayload' };
        const repo = found.repo;
        const title = typeof input.title === 'string' ? input.title : '';
        if (title === '') return { value: { __typename: 'CreateIssuePayload', issue: null }, typeName: 'CreateIssuePayload' };
        const seq = store.seq();
        const issue: Issue = {
          id: store.nextGlobalId(),
          number: repo.nextNumber,
          title,
          body: typeof input.body === 'string' ? input.body : '',
          state: 'open',
          labels: [],
          comments: [],
          createdSeq: seq,
          updatedSeq: seq,
        };
        repo.nextNumber += 1;
        repo.issues.push(issue);
        return { value: { __typename: 'CreateIssuePayload', issue: issueSource(repo, issue) }, typeName: 'CreateIssuePayload' };
      }
      if (field === 'closeIssue') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const found = findByGlobalId(store, String(input.issueId ?? ''));
        if (found === null || found.kind !== 'issue' || found.issue === undefined) {
          return { value: { __typename: 'CloseIssuePayload', issue: null }, typeName: 'CloseIssuePayload' };
        }
        found.issue.state = 'closed';
        found.issue.updatedSeq = store.seq();
        return { value: { __typename: 'CloseIssuePayload', issue: issueSource(found.repo, found.issue) }, typeName: 'CloseIssuePayload' };
      }
      if (field === 'createPullRequest') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const found = findByGlobalId(store, String(input.repositoryId ?? ''));
        if (found === null || found.kind !== 'repo') {
          return { value: { __typename: 'CreatePullRequestPayload', pullRequest: null }, typeName: 'CreatePullRequestPayload' };
        }
        const { pull } = createPullRecord(store, found.repo, {
          title: typeof input.title === 'string' ? input.title : '',
          body: typeof input.body === 'string' ? input.body : '',
          head: String(input.headRefName ?? ''),
          base: String(input.baseRefName ?? ''),
        });
        return {
          value: { __typename: 'CreatePullRequestPayload', pullRequest: pull === null ? null : pullSource(found.repo, pull) },
          typeName: 'CreatePullRequestPayload',
        };
      }
      if (field === 'mergePullRequest') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const found = findByGlobalId(store, String(input.pullRequestId ?? ''));
        if (found === null || found.kind !== 'pull' || found.pull === undefined) {
          return { value: { __typename: 'MergePullRequestPayload', pullRequest: null }, typeName: 'MergePullRequestPayload' };
        }
        const method = typeof input.mergeMethod === 'string' ? input.mergeMethod.toLowerCase() : 'squash';
        mergePullRecord(store, found.repo, found.pull, method);
        return { value: { __typename: 'MergePullRequestPayload', pullRequest: pullSource(found.repo, found.pull) }, typeName: 'MergePullRequestPayload' };
      }
      if (field === 'addComment') {
        const input = (args.input ?? {}) as Record<string, unknown>;
        const found = findByGlobalId(store, String(input.subjectId ?? ''));
        const body = typeof input.body === 'string' ? input.body : '';
        if (found === null || (found.kind !== 'issue' && found.kind !== 'pull') || body === '') {
          return { value: { __typename: 'AddCommentPayload', commentEdge: null }, typeName: 'AddCommentPayload' };
        }
        const repo = found.repo;
        const comment = { id: repo.nextCommentId, body, author: 'adaiversen', createdSeq: store.seq() };
        repo.nextCommentId += 1;
        if (found.kind === 'issue' && found.issue !== undefined) {
          found.issue.comments.push(comment);
          found.issue.updatedSeq = store.seq();
        } else if (found.kind === 'pull' && found.pull !== undefined) {
          found.pull.comments.push(comment);
          found.pull.updatedSeq = store.seq();
        }
        const node = { __typename: 'Comment', id: commentGlobalId(repo, comment.id), body, createdAt: new Date((1_735_689_600 + comment.createdSeq) * 1000).toISOString() };
        return {
          value: { __typename: 'AddCommentPayload', commentEdge: { __typename: 'CommentEdge', node, cursor: cursorFor(0) } },
          typeName: 'AddCommentPayload',
        };
      }
      return null;
    }
    case 'Repository': {
      const repo = source.repo as Repo;
      if (field === 'id') return { value: repoGlobalId(repo), typeName: null };
      if (field === 'databaseId') return { value: repo.id, typeName: null };
      if (field === 'name') return { value: repo.name, typeName: null };
      if (field === 'nameWithOwner') return { value: `${repo.owner}/${repo.name}`, typeName: null };
      if (field === 'owner') return { value: { __typename: 'User', login: repo.owner, id: 1000, name: 'ada iversen', email: 'adaiversen@example.invalid' }, typeName: 'User' };
      if (field === 'defaultBranchRef') {
        const tip = store.headTip(repo);
        return { value: tip === undefined ? null : { __typename: 'Ref', name: repo.defaultBranch }, typeName: 'Ref' };
      }
      if (field === 'viewerPermission') return { value: 'ADMIN', typeName: null };
      if (field === 'hasIssuesEnabled') return { value: true, typeName: null };
      if (field === 'issue') {
        const number = Number(args.number ?? 0);
        const issue = repo.issues.find((entry) => entry.number === number);
        return { value: issue === undefined ? null : issueSource(repo, issue), typeName: 'Issue' };
      }
      if (field === 'issues') {
        let issues = [...repo.issues];
        const states = Array.isArray(args.states) ? (args.states as string[]) : null;
        if (states !== null) {
          const wanted = new Set(states.map((state) => state.toLowerCase()));
          issues = issues.filter((issue) => wanted.has(issue.state));
        }
        const orderBy = (args.orderBy ?? {}) as Record<string, unknown>;
        const orderField = String(orderBy.field ?? 'CREATED_AT');
        const orderDirection = String(orderBy.direction ?? 'DESC');
        const key = (issue: Issue): number => (orderField === 'UPDATED_AT' ? issue.updatedSeq : orderField === 'COMMENTS' ? issue.comments.length : issue.createdSeq);
        issues.sort((a, b) => (orderDirection === 'ASC' ? key(a) - key(b) : key(b) - key(a)));
        const connection = connectionOf(
          issues.map((issue) => issueSource(repo, issue)),
          args,
          'Issue',
        );
        return { value: connection, typeName: 'IssueConnection' };
      }
      if (field === 'pullRequests') {
        const connection = connectionOf(
          repo.pulls.map((pull) => pullSource(repo, pull)),
          args,
          'PullRequest',
        );
        return { value: connection, typeName: 'PullRequestConnection' };
      }
      if (field === 'pullRequest') {
        const number = Number(args.number ?? 0);
        const pull = repo.pulls.find((entry) => entry.number === number);
        return { value: pull === undefined ? null : pullSource(repo, pull), typeName: 'PullRequest' };
      }
      return null;
    }
    case 'Issue': {
      const issue = source.issue as Issue;
      const repo = source.repo as Repo;
      if (field === 'id') return { value: issueGlobalId(repo, issue), typeName: null };
      if (field === 'number') return { value: issue.number, typeName: null };
      if (field === 'title') return { value: issue.title, typeName: null };
      if (field === 'state') return { value: issueState(issue), typeName: null };
      if (field === 'body') return { value: issue.body, typeName: null };
      if (field === 'createdAt') return { value: new Date((1_735_689_600 + issue.createdSeq) * 1000).toISOString(), typeName: null };
      if (field === 'labels') {
        const nodes = issue.labels.map((name) => ({ __typename: 'Label', name, color: 'ededed', description: '' }));
        return { value: connectionOf(nodes, args, 'Label'), typeName: 'LabelConnection' };
      }
      if (field === 'comments') {
        const nodes = issue.comments.map((comment) => ({
          __typename: 'Comment',
          id: commentGlobalId(repo, comment.id),
          body: comment.body,
          createdAt: new Date((1_735_689_600 + comment.createdSeq) * 1000).toISOString(),
        }));
        return { value: connectionOf(nodes, args, 'Comment'), typeName: 'CommentConnection' };
      }
      return null;
    }
    case 'PullRequest': {
      const pull = source.pull as Pull;
      const repo = source.repo as Repo;
      if (field === 'id') return { value: pullGlobalId(repo, pull), typeName: null };
      if (field === 'number') return { value: pull.number, typeName: null };
      if (field === 'title') return { value: pull.title, typeName: null };
      if (field === 'body') return { value: pull.body, typeName: null };
      if (field === 'state') return { value: pullState(pull), typeName: null };
      if (field === 'merged') return { value: pull.merged, typeName: null };
      if (field === 'headRefName') return { value: pull.headRef, typeName: null };
      if (field === 'baseRefName') return { value: pull.baseRef, typeName: null };
      if (field === 'mergeable') {
        const mergeable = pullMergeable(store, repo, pull);
        return { value: mergeable === null ? 'UNKNOWN' : mergeable ? 'MERGEABLE' : 'CONFLICTING', typeName: null };
      }
      if (field === 'createdAt') return { value: new Date((1_735_689_600 + pull.createdSeq) * 1000).toISOString(), typeName: null };
      if (field === 'files') {
        const files = pullFiles(store, repo, pull).map((file) => ({
          __typename: 'PullRequestChangedFile',
          path: file.filename,
          additions: file.additions,
          deletions: file.deletions,
          changes: file.changes,
        }));
        const connection = connectionOf(files, args, 'PullRequestChangedFile');
        return { value: connection, typeName: 'PullRequestChangedFileConnection' };
      }
      return null;
    }
    case 'IssueConnection':
    case 'PullRequestConnection':
    case 'PullRequestChangedFileConnection':
    case 'LabelConnection':
    case 'CommentConnection': {
      if (field === 'nodes') return { value: source.nodes as unknown[], typeName: singularOf(typeName) };
      if (field === 'edges') return { value: source.edges as unknown[], typeName: `${singularOf(typeName)}Edge` };
      if (field === 'pageInfo') return { value: source.pageInfo as SourceObject, typeName: 'PageInfo' };
      if (field === 'totalCount') return { value: source.totalCount as number, typeName: null };
      return null;
    }
    case 'IssueEdge':
    case 'PullRequestEdge':
    case 'CommentEdge':
    case 'PullRequestChangedFileEdge': {
      if (field === 'node') return { value: source.node as SourceObject, typeName: singularOf(typeName.replace('Edge', 'Connection')) };
      if (field === 'cursor') return { value: source.cursor as string, typeName: null };
      return null;
    }
    case 'PageInfo': {
      if (field === 'hasNextPage') return { value: source.hasNextPage as boolean, typeName: null };
      if (field === 'hasPreviousPage') return { value: source.hasPreviousPage as boolean, typeName: null };
      if (field === 'startCursor') return { value: source.startCursor as string | null, typeName: null };
      if (field === 'endCursor') return { value: source.endCursor as string | null, typeName: null };
      return null;
    }
    case 'User': {
      if (field === 'login') return { value: source.login as string, typeName: null };
      if (field === 'id') return { value: source.id as number, typeName: null };
      if (field === 'name') return { value: source.name as string, typeName: null };
      if (field === 'email') return { value: source.email as string, typeName: null };
      return null;
    }
    case 'Ref': {
      if (field === 'name') return { value: source.name as string, typeName: null };
      return null;
    }
    case 'Comment': {
      if (field === 'id') return { value: source.id as string, typeName: null };
      if (field === 'body') return { value: source.body as string, typeName: null };
      if (field === 'createdAt') return { value: source.createdAt as string, typeName: null };
      return null;
    }
    case 'PullRequestChangedFile': {
      if (field === 'path') return { value: source.path as string, typeName: null };
      if (field === 'additions') return { value: source.additions as number, typeName: null };
      if (field === 'deletions') return { value: source.deletions as number, typeName: null };
      if (field === 'changes') return { value: source.changes as number, typeName: null };
      return null;
    }
    case 'Label': {
      if (field === 'name') return { value: source.name as string, typeName: null };
      if (field === 'color') return { value: source.color as string, typeName: null };
      if (field === 'description') return { value: source.description as string, typeName: null };
      return null;
    }
    case '__Type': {
      if (field === 'name') return { value: source.name as string, typeName: null };
      if (field === 'kind') return { value: source.kind as string, typeName: null };
      if (field === 'description') return { value: null, typeName: null };
      if (field === 'fields') {
        const fields = (source.fields ?? null) as { name: string }[] | null;
        return { value: fields === null ? null : fields.map((entry) => ({ __typename: '__Field', name: entry.name, description: null, args: [], type: null })), typeName: '__Field' };
      }
      if (field === 'enumValues') {
        const values = (source.enumValues ?? null) as string[] | null;
        return { value: values === null ? null : values.map((name) => ({ __typename: '__EnumValue', name, description: null })), typeName: '__EnumValue' };
      }
      if (field === 'inputFields') {
        const fields = (source.inputFields ?? null) as { name: string }[] | null;
        return { value: fields === null ? null : fields.map((entry) => ({ __typename: '__InputValue', name: entry.name, description: null, type: null })), typeName: '__InputValue' };
      }
      return null;
    }
    case '__Schema': {
      if (field === 'queryType') return { value: source.queryType as SourceObject, typeName: '__Type' };
      if (field === 'mutationType') return { value: source.mutationType as SourceObject, typeName: '__Type' };
      if (field === 'types') return { value: source.types as SourceObject[], typeName: '__Type' };
      return null;
    }
    case '__Field':
    case '__EnumValue':
    case '__InputValue': {
      if (field === 'name') return { value: source.name as string, typeName: null };
      if (field === 'description') return { value: null, typeName: null };
      if (field === 'args') return { value: [], typeName: null };
      if (field === 'type') return { value: null, typeName: null };
      return null;
    }
    case 'CreateIssuePayload':
    case 'CloseIssuePayload': {
      if (field === 'issue') return { value: source.issue as SourceObject | null, typeName: 'Issue' };
      return null;
    }
    case 'CreatePullRequestPayload':
    case 'MergePullRequestPayload': {
      if (field === 'pullRequest') return { value: source.pullRequest as SourceObject | null, typeName: 'PullRequest' };
      return null;
    }
    case 'AddCommentPayload': {
      if (field === 'commentEdge') return { value: source.commentEdge as SourceObject | null, typeName: 'CommentEdge' };
      return null;
    }
    default:
      return null;
  }
}

function singularOf(connectionType: string): string {
  const map: Record<string, string> = {
    IssueConnection: 'Issue',
    PullRequestConnection: 'PullRequest',
    PullRequestChangedFileConnection: 'PullRequestChangedFile',
    LabelConnection: 'Label',
    CommentConnection: 'Comment',
  };
  return map[connectionType] ?? connectionType;
}

function introspectionType(name: string, type: SchemaType): SourceObject {
  return {
    __typename: '__Type',
    name,
    kind: type.kind,
    description: null,
    fields: type.fields ?? null,
    enumValues: type.enumValues ?? null,
    inputFields: type.inputFields ?? null,
  };
}

function executeSelections(
  selections: Selection[],
  typeName: string,
  source: SourceObject,
  ctx: GqlContext,
  fragments: Map<string, Fragment>,
  variables: Variables,
  errors: { message: string }[],
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const selection of selections) {
    if (selection.kind === 'field') {
      const key = selection.alias ?? selection.name;
      const args = argsOf(selection, variables);
      const resolved = resolveField(ctx, typeName, selection.name, source, args);
      if (resolved === null) {
        errors.push({ message: `Cannot query field "${selection.name}" on type "${typeName}".` });
        out[key] = null;
        continue;
      }
      const { value, typeName: valueType } = resolved;
      if (selection.selections.length === 0 || value === null || valueType === null) {
        out[key] = value;
      } else if (Array.isArray(value)) {
        out[key] = value.map((item) => {
          if (item === null || typeof item !== 'object') return item;
          return executeSelections(selection.selections, valueType, item as SourceObject, ctx, fragments, variables, errors);
        });
      } else if (typeof value === 'object') {
        out[key] = executeSelections(selection.selections, valueType, value as SourceObject, ctx, fragments, variables, errors);
      } else {
        out[key] = value;
      }
    } else if (selection.kind === 'fragmentSpread') {
      const fragment = fragments.get(selection.name);
      if (fragment !== undefined && (fragment.typeCondition === typeName || fragment.typeCondition === source.__typename)) {
        Object.assign(out, executeSelections(fragment.selections, typeName, source, ctx, fragments, variables, errors));
      }
    } else if (selection.kind === 'inlineFragment') {
      if (selection.typeCondition === null || selection.typeCondition === typeName || selection.typeCondition === source.__typename) {
        Object.assign(out, executeSelections(selection.selections, typeName, source, ctx, fragments, variables, errors));
      }
    }
  }
  return out;
}

function executeDocument(document: Document, operationName: string | null | undefined, variables: Variables, ctx: GqlContext): Record<string, unknown> {
  let operation = document.operations[0] as Operation | undefined;
  if (operationName !== null && operationName !== undefined) {
    operation = document.operations.find((entry) => entry.name === operationName) ?? operation;
  }
  if (operation === undefined) return { data: null, errors: [{ message: 'No operation provided' }] };
  const mergedVariables: Variables = { ...variables };
  for (const definition of operation.variables) {
    if (mergedVariables[definition.name] === undefined && definition.defaultValue !== null) {
      mergedVariables[definition.name] = resolveValue(definition.defaultValue, mergedVariables);
    }
  }
  const errors: { message: string }[] = [];
  const rootType = operation.type === 'mutation' ? 'Mutation' : 'Query';
  const data = executeSelections(operation.selections, rootType, { __typename: rootType }, ctx, document.fragments, mergedVariables, errors);
  if (errors.length > 0) return { data, errors };
  return { data };
}

// ---------------------------------------------------------------- HTTP

/** Handle POST /api/graphql. Returns true when the path matched. */
export function handleGraphQL(ctx: Ctx): boolean {
  if (ctx.path !== '/api/graphql') return false;
  if (ctx.method !== 'POST') {
    sendError(ctx, 404, 'Not Found');
    return true;
  }
  if (!isAuthorized(ctx)) {
    sendError(ctx, 401, 'Requires authentication');
    return true;
  }
  const parsed = parseJsonBody(ctx);
  if (!parsed.ok) {
    sendJson(ctx, 400, { errors: [{ message: 'Request body must be valid JSON' }] });
    return true;
  }
  const gqlCtx: GqlContext = { store: ctx.store };
  const runOne = (entry: unknown): Record<string, unknown> => {
    if (entry === null || typeof entry !== 'object' || Array.isArray(entry)) return { errors: [{ message: 'Operation must be an object' }] };
    const record = entry as Record<string, unknown>;
    const query = typeof record.query === 'string' ? record.query : '';
    const variables = (record.variables ?? {}) as Variables;
    const operationName = typeof record.operationName === 'string' ? record.operationName : null;
    let document: Document;
    try {
      document = new Parser(lex(query)).parseDocument();
    } catch (error) {
      return { errors: [{ message: error instanceof Error ? error.message : 'GraphQL syntax error' }] };
    }
    return executeDocument(document, operationName, variables, gqlCtx);
  };
  if (Array.isArray(parsed.value)) {
    sendJson(ctx, 200, parsed.value.map(runOne));
    return true;
  }
  sendJson(ctx, 200, runOne(parsed.value));
  return true;
}
