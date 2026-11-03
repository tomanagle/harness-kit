// The small API the dashboard (app.js) uses, built on @harness-kit/kit.
// Electron exposes it from the main process over IPC; the Wails example
// builds it in the webview. Pure JavaScript: no Node.js or DOM APIs.

/**
 * @param {import("@harness-kit/kit").HarnessKit} kit
 * @param {{ cwd?: string, nativeMcpRuntimes?: string[] }} [opts]
 */
export function makeApi(kit, opts = {}) {
  const handle = async (id) => {
    // Restore is idempotent for live sessions. Do this at prompt time so
    // viewing a saved transcript never needs an installed/signed-in runtime.
    const session = await kit.sessions.restore(id);
    return session;
  };
  return {
    async overview() {
      const [runtimes, tools, mcp] = await Promise.all([
        kit.runtimes.discover(),
        kit.tools.list(),
        kit.mcp.list({ cwd: opts.cwd, runtimes: opts.nativeMcpRuntimes ?? ["codex"] }).catch((e) => ({ error: String(e?.message ?? e) })),
      ]);
      return { runtimes, tools, mcp };
    },
    listSessions: () => kit.sessions.list(),
    async createSession(runtime) {
      const s = await kit.sessions.create({ runtime: String(runtime), cwd: opts.cwd });
      return s.info;
    },
    renameSession: (id, title) => kit.sessions.rename(String(id), String(title)),
    async openSession(id) {
      return kit.sessions.get(String(id));
    },
    async history(id) {
      return kit.sessions.events(String(id));
    },
    async prompt(id, text) {
      const turn = (await handle(String(id))).prompt(String(text));
      turn.done.catch(() => {}); // failures also arrive as events
      return turn.id;
    },
    async cancel(id) {
      await kit.request("session.cancel", { sessionId: String(id) });
    },
    async approve(approvalId, outcome) {
      if (!["allow", "allow_session", "deny", "abort"].includes(outcome)) throw new Error("bad outcome");
      await kit.approvals.respond(String(approvalId), outcome);
    },
  };
}
