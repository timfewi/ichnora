// Drives the real OpenCode adapter with a stand-in plugin context. Run by
// OpenCode's embedded runtime (BUN_BE_BUN=1), never by Node.js.
import { readFileSync } from 'node:fs';

const plugin = (await import(process.env.PLUGIN_PATH)).default;
const hooks = {};
const ctx = {
  session: {
    hook: async (name, fn) => {
      hooks[name] = fn;
    },
    get: async () => ({ parentID: '', directory: process.env.SESSION_DIR, version: '2.0.32' }),
  },
};
await plugin.setup(ctx);
const messages = JSON.parse(readFileSync(process.env.MESSAGES, 'utf8'));
const first = { sessionID: 'oc-live', messages, system: [] };
await hooks.context(first);
const second = { sessionID: 'oc-live', messages, system: [] };
await hooks.context(second);
const compacted = { sessionID: 'oc-live', messages: messages.slice(-1), system: [] };
await hooks.compaction(compacted);
console.log(JSON.stringify({ first: first.system, second: second.system, compaction: compacted.system }));
