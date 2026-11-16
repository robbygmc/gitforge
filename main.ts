// Entry point for the GitForge submission.
//
// The grading harness owns a small "env" service that drives the trial. This
// process has one job only: start the forge on a port the kernel picks, tell
// the env which port that is, and stay alive until the env says the trial is
// finished. The forge itself lives in app.ts; this file is just the glue
// between the harness and the server.
//
// Lifecycle:
//   1. MAKEFUN_ENV_URL points at the env (for example http://store:8080
//      inside a trial container). Without it there is nobody to register
//      with, so the process gives up at once with exit code 1.
//   2. The server binds port 0, so the kernel chooses a free ephemeral
//      port. Port 8080 is off limits — the env listens there.
//   3. The process POSTs {"port": N} to $MAKEFUN_ENV_URL/register. The env
//      remembers the caller's address plus N as the forge's base URL. A 200
//      means registered; a 409 means another entry registered first, which
//      is fine — this process just waits like any other. A 400 or 405 is a
//      contract violation and ends the process; anything else (including the
//      env still booting) is retried with backoff.
//   4. The process polls GET $MAKEFUN_ENV_URL/done until it answers
//      {"done": true}. The env answers at once either way, so this is a
//      poll, not a long-held request. It never exits early.
//   5. On done, the server is closed and the process exits 0. The harness
//      then stops the env, which writes the state export the scorer reads.
//
// main.ts imports its own server and nothing else: no package, no Node
// built-in, no model call.

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

/** Bind the port, register it, then wait for the env: the whole life of the process. */
async function main(): Promise<void> {
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

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[gitforge] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
