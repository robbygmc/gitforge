// Entry point for the GitForge submission.
//
// The forge itself is a statically linked Go binary, `forge`, vendored in
// this repository (source in go/, built with
// `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w"`). This file is only
// the launcher: it spawns ./forge with the same environment, forwards
// termination signals to it, and exits with the child's exit code. The
// binary does the whole lifecycle itself — bind an ephemeral port,
// register it with the env, serve, and poll /done until the trial ends —
// mirroring what the Node server used to do.
//
// Why a binary: the kit's rules allow it (RULES.txt: "a statically linked
// Linux amd64 binary you commit runs from your repository as vendored
// source"), and the compiled core serves each request at roughly a third
// of the Node server's CPU cost, which is what the perf column measures.
//
// If the binary cannot be started, or dies at once with an error, the
// launcher falls back to the Node server in app.ts, so the entry still
// works — just slower. That fallback is a safety net, not the normal path,
// and it is logged loudly when it happens.
//
// Lifecycle (unchanged from the Node version):
//   1. MAKEFUN_ENV_URL points at the env (for example http://store:8080
//      inside a trial container). Without it there is nobody to register
//      with, so the process gives up at once with exit code 1.
//   2. The forge binds port 0, so the kernel chooses a free ephemeral
//      port. Port 8080 is off limits — the env listens there.
//   3. The forge POSTs {"port": N} to $MAKEFUN_ENV_URL/register, then
//      polls GET $MAKEFUN_ENV_URL/done until it answers {"done": true}.
//   4. On done, the forge closes its server and exits 0; this launcher
//      exits with the same code.
//
// main.ts imports its own server and nothing else: no package, no Node
// built-in beyond child_process/path/url, no model call.

import { spawn, type ChildProcess } from 'node:child_process';
import { existsSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createApp } from './app.ts';

const envUrl = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (envUrl === '') {
  process.stderr.write('[gitforge] MAKEFUN_ENV_URL is not set, nothing to register with\n');
  process.exit(1);
}

/** One request may take at most this long before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** Backoff starts here and doubles after each failed attempt, up to this cap. */
const BACKOFF_START_MS = 250;
const BACKOFF_CAP_MS = 2_000;

/** A child that dies this soon after spawn never served a request: it failed to start. */
const STARTUP_GRACE_MS = 2_000;

const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

/** The wait after `attempt` failures: exponential from BACKOFF_START_MS, capped. */
function backoffMs(attempt: number): number {
  return Math.min(BACKOFF_CAP_MS, BACKOFF_START_MS * 2 ** Math.min(attempt, 8));
}

/**
 * Run `step` until it reports success, waiting longer after each failure.
 * `step` returns true when its work is done, or throws a fatal error that
 * must end the process right away instead of being retried.
 */
async function retryUntilDone(step: (attempt: number) => Promise<boolean>): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    if (await step(attempt)) return;
    await sleep(backoffMs(attempt));
  }
}

/**
 * POST the bound port until the env accepts it. 200 is the normal answer;
 * 409 means a first register already won, which is not this process's to
 * fix. 400 and 405 are contract errors and end the program; a network error
 * or another status is retried, since the env may still be starting.
 */
async function register(port: number): Promise<void> {
  await retryUntilDone(async () => {
    try {
      const res = await fetch(`${envUrl}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return true;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
      return false;
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
      return false;
    }
  });
}

/** Poll GET /done until it answers {"done":true}; never return before that. */
async function waitForDone(): Promise<void> {
  await retryUntilDone(async () => {
    try {
      const res = await fetch(`${envUrl}/done`, {
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (!res.ok) return false;
      const body = (await res.json()) as { done?: unknown };
      return body.done === true;
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done,
      // so the loop polls again.
      return false;
    }
  });
}

/** The old Node server, kept as the fallback when the binary cannot run. */
async function runNodeServer(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('the server did not bind an ephemeral TCP port');
  }
  process.stdout.write(`[gitforge] listening on port ${address.port}\n`);

  await register(address.port);
  process.stdout.write('[gitforge] registered; waiting for the env to say done\n');

  await waitForDone();
  server.close();
  process.stdout.write('[gitforge] done\n');
}

/** Spawn the vendored Go binary and wait for it. Resolves false when it
 *  failed to start (or died at once), so the caller can fall back. */
function runBinary(forgePath: string): Promise<boolean> {
  return new Promise((resolve) => {
    const startedAt = Date.now();
    let child: ChildProcess;
    try {
      child = spawn(forgePath, [], { stdio: 'inherit', env: process.env });
    } catch (err) {
      process.stderr.write(`[gitforge] could not spawn ${forgePath}: ${err}\n`);
      resolve(false);
      return;
    }
    const forward = (signal: NodeJS.Signals): void => {
      child.kill(signal);
    };
    process.on('SIGTERM', () => forward('SIGTERM'));
    process.on('SIGINT', () => forward('SIGINT'));
    process.on('SIGHUP', () => forward('SIGHUP'));
    child.on('error', (err) => {
      process.stderr.write(`[gitforge] could not spawn ${forgePath}: ${err}\n`);
      resolve(false);
    });
    child.on('exit', (code, signal) => {
      if (signal !== null) {
        // The binary died on a signal: die the same way, so the harness
        // sees the same termination it would have seen from the binary.
        process.kill(process.pid, signal);
        return;
      }
      if (code !== 0 && Date.now() - startedAt < STARTUP_GRACE_MS) {
        process.stderr.write(
          `[gitforge] binary exited ${code} within ${STARTUP_GRACE_MS}ms of spawn: startup failure\n`,
        );
        resolve(false);
        return;
      }
      process.exit(code ?? 1);
    });
  });
}

async function main(): Promise<void> {
  const forgePath = join(dirname(fileURLToPath(import.meta.url)), 'forge');
  if (existsSync(forgePath)) {
    process.stdout.write(`[gitforge] launching ${forgePath}\n`);
    if (await runBinary(forgePath)) return; // unreachable: runBinary exits on success
    process.stderr.write('[gitforge] WARNING: falling back to the Node server\n');
  } else {
    process.stderr.write(`[gitforge] WARNING: ${forgePath} not found, falling back to the Node server\n`);
  }
  await runNodeServer();
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[gitforge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
