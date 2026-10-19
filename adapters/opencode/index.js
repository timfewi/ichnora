// Thin OpenCode V2 adapter for Ichnora, executed by OpenCode's own runtime.
//
// It forwards messages that are new since its last call to
// `ichnora hook opencode` and appends returned context to the system prompt.
// All parsing, scope decisions and storage happen in the Go service. The hook
// process runs in the session directory, because the service derives scope
// from the caller's working directory. Failures never block a session: the
// call is bounded and returns nothing on error.
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const TIMEOUT_MS = 3000;
const MAX_OUTPUT = 256 * 1024;
const MAX_MESSAGES = 200;
const MAX_MESSAGE_BYTES = 64 * 1024;

// The package installs this file at share/ichnora/opencode/index.js.
const binary =
  process.env.ICHNORA_BIN ||
  fileURLToPath(new URL('../../../bin/ichnora', import.meta.url));

function call(payload, cwd) {
  return new Promise((resolve) => {
    let out = '';
    let done = false;
    const finish = (value) => {
      if (!done) {
        done = true;
        resolve(value);
      }
    };
    let child;
    try {
      child = spawn(binary, ['hook', 'opencode'], { cwd, stdio: ['pipe', 'pipe', 'ignore'] });
    } catch {
      return finish(null);
    }
    const timer = setTimeout(() => {
      child.kill('SIGKILL');
      finish(null);
    }, TIMEOUT_MS);
    child.on('error', () => finish(null));
    child.stdout.on('data', (chunk) => {
      out += chunk;
      if (out.length > MAX_OUTPUT) child.kill('SIGKILL');
    });
    child.on('close', () => {
      clearTimeout(timer);
      try {
        const response = JSON.parse(out);
        finish(response.ok ? response.result : null);
      } catch {
        finish(null);
      }
    });
    child.stdin.on('error', () => {});
    child.stdin.end(JSON.stringify(payload));
  });
}

export default {
  id: 'ichnora',
  async setup(ctx) {
    const forwarded = new Map();
    const injected = new Set();
    const sessions = new Map();

    const info = async (sessionID) => {
      if (!sessions.has(sessionID)) {
        let value = {};
        try {
          value = (await ctx.session.get({ sessionID })) ?? {};
        } catch {}
        sessions.set(sessionID, {
          parentID: value.parentID ?? value.parent_id ?? '',
          directory: value.directory ?? value.location?.directory ?? process.cwd(),
          version: value.version ?? '',
        });
      }
      return sessions.get(sessionID);
    };

    const forward = async (event, request, wantContext) => {
      const sessionID = request.sessionID;
      if (typeof sessionID !== 'string') return null;
      const messages = Array.isArray(request.messages) ? request.messages : [];
      // Compaction can shorten the list; event identities keep a re-send idempotent.
      const start = (forwarded.get(sessionID) ?? 0) <= messages.length ? forwarded.get(sessionID) ?? 0 : 0;
      const fresh = messages
        .slice(start)
        .slice(-MAX_MESSAGES)
        .filter((m) => JSON.stringify(m ?? null).length <= MAX_MESSAGE_BYTES);
      const meta = await info(sessionID);
      const result = await call(
        {
          event,
          sessionID,
          parentID: meta.parentID,
          directory: meta.directory,
          version: meta.version,
          want_context: wantContext,
          messages: fresh,
        },
        meta.directory,
      );
      if (result) forwarded.set(sessionID, messages.length);
      return result;
    };

    const inject = (request, result) => {
      if (!result?.context || !Array.isArray(request.system)) return;
      try {
        request.system.push(result.context);
      } catch {}
    };

    await ctx.session.hook('context', async (request) => {
      const want = !injected.has(request.sessionID);
      const result = await forward('context', request, want);
      if (want && result) injected.add(request.sessionID);
      inject(request, result);
    });
    await ctx.session.hook('compaction', async (request) => {
      inject(request, await forward('compaction', request, true));
    });
  },
};
