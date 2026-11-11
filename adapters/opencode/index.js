// Thin OpenCode V2 adapter for Ichnora, executed by OpenCode's own runtime.
// It sends ordered, acknowledged batches. Scope, parsing and storage remain
// service responsibilities; native message versions are historical sources.
import { spawn } from "node:child_process";
import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";

const TIMEOUT_MS = 3000;
const MAX_OUTPUT = 256 * 1024;
const MAX_MESSAGES = 200;
const MAX_MESSAGE_BYTES = 64 * 1024;
const MAX_PAYLOAD_BYTES = 512 * 1024;
const MAX_BATCHES = 4;

const binary =
  process.env.ICHNORA_BIN ||
  fileURLToPath(new URL("../../../bin/ichnora", import.meta.url));

// Match Go's JSON HTML escaping before measuring the bytes sent to both
// the hook process and the service's one-MiB request envelope.
const encode = (value) =>
  JSON.stringify(value).replace(
    /[<>&\u2028\u2029]/g,
    (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, "0")}`,
  );
const digest = (value) => createHash("sha256").update(value).digest("hex");

function call(payload, cwd, timeout) {
  return new Promise((resolve) => {
    let out = "";
    let done = false;
    const finish = (value) => {
      if (!done) {
        done = true;
        resolve(value);
      }
    };
    let child;
    try {
      child = spawn(binary, ["hook", "opencode"], {
        cwd,
        stdio: ["pipe", "pipe", "ignore"],
      });
    } catch {
      return finish(null);
    }
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      finish(null);
    }, timeout);
    child.on("error", () => finish(null));
    child.stdout.setEncoding("utf8");
    child.stdout.on("data", (chunk) => {
      out += chunk;
      if (Buffer.byteLength(out) > MAX_OUTPUT) {
        child.kill("SIGKILL");
        finish(null);
      }
    });
    child.on("close", () => {
      clearTimeout(timer);
      try {
        const response = JSON.parse(out);
        finish(response.ok ? response.result : null);
      } catch {
        finish(null);
      }
    });
    child.stdin.on("error", () => {});
    child.stdin.end(payload);
  });
}

export default {
  id: "ichnora",
  async setup(ctx) {
    const acknowledged = new Map();
    const pendingMessages = new Map();
    const injected = new Set();
    const sessions = new Map();
    const pendingCalls = new Map();

    const info = async (sessionID) => {
      if (!sessions.has(sessionID)) {
        sessions.set(
          sessionID,
          (async () => {
            let value = {};
            try {
              value = (await ctx.session.get({ sessionID })) ?? {};
            } catch {}
            return {
              parentID: value.parentID ?? value.parent_id ?? "",
              directory:
                value.directory ?? value.location?.directory ?? process.cwd(),
              version: value.version ?? "",
            };
          })(),
        );
      }
      return sessions.get(sessionID);
    };

    const forward = async (event, request, wantContext) => {
      const sessionID = request.sessionID;
      if (typeof sessionID !== "string") return null;
      // Serialize each session's callbacks so a late response cannot overwrite
      // acknowledgement of a newer version of the same native message.
      const previous = pendingCalls.get(sessionID) ?? Promise.resolve();
      const current = previous
        .catch(() => {})
        .then(async () => {
          const deadline = Date.now() + TIMEOUT_MS;
          let metadataTimer;
          const meta = await Promise.race([
            info(sessionID),
            new Promise((resolve) => {
              metadataTimer = setTimeout(() => resolve(null), TIMEOUT_MS);
            }),
          ]);
          clearTimeout(metadataTimer);
          if (!meta) return null;
          let seen = acknowledged.get(sessionID);
          if (!seen) {
            seen = new Map();
            acknowledged.set(sessionID, seen);
          }
          const messages = Array.isArray(request.messages)
            ? request.messages
            : [];
          let fresh = pendingMessages.get(sessionID);
          if (!fresh) {
            fresh = [];
            pendingMessages.set(sessionID, fresh);
          }
          const queued = new Set(
            fresh.map((item) => `${item.key}:${item.hash}`),
          );
          for (const message of messages) {
            const raw = encode(message ?? null);
            const hash = digest(raw);
            const key =
              typeof message?.id === "string"
                ? `id:${message.id}`
                : `digest:${hash}`;
            if (seen.get(key) === hash || queued.has(`${key}:${hash}`))
              continue;
            // Large records get an explicit durable gap marker, never a silent
            // filter. The next ordinary message can still be acknowledged.
            const value =
              Buffer.byteLength(raw) <= MAX_MESSAGE_BYTES
                ? JSON.parse(raw)
                : {
                    id: `omitted:${hash}`,
                    role: "ichnora_omission",
                    omitted_bytes: Buffer.byteLength(raw),
                  };
            fresh.push({ key, hash, value });
            queued.add(`${key}:${hash}`);
          }
          let offset = 0;
          let result = null;
          for (let batchNo = 0; batchNo < MAX_BATCHES; batchNo++) {
            const base = {
              event,
              sessionID,
              parentID: meta.parentID,
              directory: meta.directory,
              version: meta.version,
              want_context: wantContext && batchNo === 0,
              messages: [],
            };
            const batch = [];
            while (
              offset + batch.length < fresh.length &&
              batch.length < MAX_MESSAGES
            ) {
              const candidate = fresh[offset + batch.length];
              base.messages.push(candidate.value);
              if (Buffer.byteLength(encode(base)) > MAX_PAYLOAD_BYTES) {
                base.messages.pop();
                break;
              }
              batch.push(candidate);
            }
            const payload = encode(base);
            const remaining = deadline - Date.now();
            if (
              remaining <= 0 ||
              Buffer.byteLength(payload) > MAX_PAYLOAD_BYTES
            )
              break;
            if (batch.length === 0 && offset < fresh.length) break;
            const response = await call(payload, meta.directory, remaining);
            if (response?.context && !result?.context) result = response;
            else if (!result) result = response;
            if (
              !response ||
              response.ingestion_paused ||
              response.skipped ||
              response.accepted_messages !== batch.length
            )
              break;
            for (const item of batch) seen.set(item.key, item.hash);
            offset += batch.length;
            if (offset === fresh.length) break;
          }
          // Keep unacknowledged versions across compaction, pause and timeout.
          // The next callback may contain a shorter or entirely different list.
          fresh.splice(0, offset);
          return result;
        })
        .catch(() => null);
      pendingCalls.set(sessionID, current);
      try {
        return await current;
      } finally {
        if (pendingCalls.get(sessionID) === current)
          pendingCalls.delete(sessionID);
      }
    };

    const inject = (request, result) => {
      if (!result?.context || !Array.isArray(request.system)) return;
      try {
        request.system.push(result.context);
      } catch {}
    };

    await ctx.session.hook("context", async (request) => {
      const want = !injected.has(request.sessionID);
      const result = await forward("context", request, want);
      if (want && result?.context) injected.add(request.sessionID);
      inject(request, result);
    });
    await ctx.session.hook("compaction", async (request) => {
      inject(request, await forward("compaction", request, true));
    });
  },
};
