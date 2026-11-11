// Transport regressions run in OpenCode's embedded Bun runtime. This harness
// supplies the documented callback shape; it is not a live plugin-loader test.
import { readFileSync } from "node:fs";

const plugin = (await import(process.env.PLUGIN_PATH)).default;
const hooks = {};
await plugin.setup({
  session: {
    hook: async (name, fn) => {
      hooks[name] = fn;
    },
    get: async () => ({ directory: process.env.SESSION_DIR }),
  },
});
for (const entry of JSON.parse(readFileSync(process.env.SCENARIO, "utf8"))) {
  await hooks[entry.event ?? "context"]({
    sessionID: "batch-fixture",
    messages: entry.messages,
    system: [],
  });
}
